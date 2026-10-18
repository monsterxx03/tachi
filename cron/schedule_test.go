package cron

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextRunUsesTheJobsTimezone(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	shanghai, err := NextRun("0 9 * * *", "Asia/Shanghai", now)
	require.NoError(t, err)
	// 12:00 UTC is 20:00 in Shanghai; the next 09:00 is the 19th, 01:00 UTC.
	assert.Equal(t, time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC), shanghai.UTC())

	utc, err := NextRun("0 9 * * *", "UTC", now)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC), utc.UTC())

	_, err = NextRun("0 9 * * *", "Mars/Olympus", now)
	assert.Error(t, err)
}

func TestNextRunsReturnsTheRequestedWindow(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	next, err := NextRuns("0 9 * * *", "UTC", now, 3)
	require.NoError(t, err)
	require.Len(t, next, 3)
	assert.Equal(t, 24*time.Hour, next[1].Sub(next[0]))
	assert.Equal(t, 24*time.Hour, next[2].Sub(next[1]))
}

func TestLocalTimezoneNameIsUseful(t *testing.T) {
	name := LocalTimezoneName()
	assert.NotEmpty(t, name)
	if name == "Local" {
		// A container can have /etc/localtime as a copy and no TZ; the
		// documented fallback is then correct. The desktop's placeholder still
		// reads 「本机」, so nothing is broken there.
		t.Skip("this machine does not expose its IANA zone")
	}
}

func TestDescribeSchedule(t *testing.T) {
	cases := map[string]string{
		"0 9 * * *":    "每天 09:00",
		"30 17 * * 5":  "每周五 17:30",
		"0 9 * * 1-5":  "每周一至周五 09:00",
		"0 9 * * 1,3":  "每周一、周三 09:00",
		"*/30 * * * *": "每 30 分钟",
		"0 */2 * * *":  "每 2 小时",
		"0 * * * *":    "每小时",
		"@hourly":      "每小时",
		"@every 2h":    "每 2 小时",
		"15 2 * * *":   "每天 02:15",
		"0 9 1 * *":    "0 9 1 * *",
	}
	for expr, want := range cases {
		assert.Equal(t, want, DescribeSchedule(expr), expr)
	}
}

func TestSchedulerOwnsOnlyItsTargets(t *testing.T) {
	scheduler, _ := newTestScheduler(t, func(context.Context, *Job) error { return nil })
	scheduler.owns = func(j *Job) bool { return j.TargetType == TargetTypeDesktop }

	channelJob, err := scheduler.Create(&Job{
		Name: "channel", Schedule: "@every 1h", Prompt: "p",
		TargetType: TargetTypeChannel, TargetThreadID: "t1",
	})
	require.NoError(t, err)
	desktopJob, err := scheduler.Create(&Job{
		Name: "desktop", Schedule: "@every 1h", Prompt: "p",
		TargetType: TargetTypeDesktop,
		Desktop:    &DesktopTarget{Mode: DesktopModeSession, SessionID: "conv-1"},
	})
	require.NoError(t, err)

	scheduler.mu.Lock()
	_, channelScheduled := scheduler.entryMap[channelJob.ID]
	_, desktopScheduled := scheduler.entryMap[desktopJob.ID]
	scheduler.mu.Unlock()
	assert.False(t, channelScheduled, "another entry point's job must not be scheduled here")
	assert.True(t, desktopScheduled)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, scheduler.Start(ctx))
	defer scheduler.Stop()

	assert.Error(t, scheduler.RunNow(channelJob.ID), "another entry point's job must not be run here")
}

func TestBindDesktopSessionFillsTheTarget(t *testing.T) {
	scheduler, _ := newTestScheduler(t, func(context.Context, *Job) error { return nil })
	job, err := scheduler.Create(&Job{
		Name: "first run", Schedule: "@every 1h", Prompt: "p",
		TargetType: TargetTypeDesktop,
		Desktop: &DesktopTarget{
			Mode: DesktopModeNew, WorkingDir: "/tmp/x", ProviderName: "p1", SkipApproval: true,
		},
	})
	require.NoError(t, err)

	updated, err := scheduler.BindDesktopSession(job.ID, "conv-42")
	require.NoError(t, err)
	require.NotNil(t, updated.Desktop)
	assert.Equal(t, DesktopModeSession, updated.Desktop.Mode)
	assert.Equal(t, "conv-42", updated.Desktop.SessionID)
	assert.Empty(t, updated.Desktop.WorkingDir)
	assert.Empty(t, updated.Desktop.ProviderName)
	assert.False(t, updated.Desktop.SkipApproval)

	// The stored copy is the bound one, not just the returned value.
	stored, err := scheduler.Get(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "conv-42", stored.Desktop.SessionID)
}

func TestStoreWritesFromTwoHandlesDoNotLoseEachOther(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")
	a, b := NewStore(path), NewStore(path)

	mk := func(id string) *Job {
		return &Job{
			ID: id, Name: id, Schedule: "@every 1h", Prompt: "start",
			TargetType: TargetTypeDesktop,
			Desktop:    &DesktopTarget{Mode: DesktopModeSession, SessionID: "conv"},
		}
	}
	require.NoError(t, a.Create(mk("j1")))
	require.NoError(t, a.Create(mk("j2")))

	const rounds = 20
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, err := a.UpdateFunc("j1", func(j *Job) error {
				j.Prompt = fmt.Sprintf("a-%d", i)
				return nil
			}); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, err := b.UpdateFunc("j2", func(j *Job) error {
				j.Prompt = fmt.Sprintf("b-%d", i)
				return nil
			}); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	got1, err := a.Get("j1")
	require.NoError(t, err)
	got2, err := a.Get("j2")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("a-%d", rounds-1), got1.Prompt)
	assert.Equal(t, fmt.Sprintf("b-%d", rounds-1), got2.Prompt, "the other handle's job was clobbered")
}

func TestListReturnsDeepCopies(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "crons.json"))
	require.NoError(t, store.Create(&Job{
		ID: "j1", Name: "j1", Schedule: "@every 1h", Prompt: "p",
		TargetType: TargetTypeDesktop,
		Desktop:    &DesktopTarget{Mode: DesktopModeNew, AdditionalDirs: []string{"/a"}},
	}))

	jobs, err := store.List()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	jobs[0].Desktop.AdditionalDirs[0] = "/mutated"
	jobs[0].Desktop.SessionID = "mutated"

	again, err := store.Get("j1")
	require.NoError(t, err)
	assert.Equal(t, "/a", again.Desktop.AdditionalDirs[0])
	assert.Empty(t, again.Desktop.SessionID)
}
