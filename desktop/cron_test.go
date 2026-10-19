package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cronTestApp is a desktopApp with its session store pointed at a temp dir and
// nothing else running (no Wails app, no config). Every scheduler test starts
// here, so no test can touch the user's ~/.tachi.
func cronTestApp(t *testing.T) (*desktopApp, *session.FileStore) {
	t.Helper()
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	d := newDesktopApp()
	d.store = store
	d.sm = session.NewManagerWithStore(store, nil)
	return d, store
}

func addSession(t *testing.T, store *session.FileStore, s *session.Session) *session.Session {
	t.Helper()
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	require.NoError(t, store.CreateSession(s))
	return s
}

func TestLeafSessionFollowsCompaction(t *testing.T) {
	d, store := cronTestApp(t)

	parent := addSession(t, store, &session.Session{ID: "p1", Title: "parent"})
	child := addSession(t, store, &session.Session{
		ID: "c1", Title: "child", LogicalID: "p1", CompactedParentID: "p1",
	})
	parent.CompactedChildID = "c1"
	require.NoError(t, store.UpdateMeta(parent))

	leaf, err := d.leafSession("p1")
	require.NoError(t, err)
	assert.Equal(t, "c1", leaf.ID, "the conversation lives in the child now")

	// A second compaction moves it again; the logical id never changes.
	grand := addSession(t, store, &session.Session{
		ID: "g1", Title: "grandchild", LogicalID: "p1", CompactedParentID: "c1",
	})
	child.CompactedChildID = "g1"
	require.NoError(t, store.UpdateMeta(child))

	leaf, err = d.leafSession("p1")
	require.NoError(t, err)
	assert.Equal(t, grand.ID, leaf.ID)

	_, err = d.leafSession("gone")
	assert.Error(t, err)
}

func TestCreateCronSessionUsesTheTemplateAndBindsIt(t *testing.T) {
	d, _ := cronTestApp(t)
	dir := t.TempDir()
	sched := cron.NewScheduler(cron.SchedulerConfig{
		Store:   cron.NewStore(filepath.Join(t.TempDir(), "crons.json")),
		Handler: func(context.Context, *cron.Job) error { return nil },
	})
	job, err := sched.Create(&cron.Job{
		Name: "每日巡检", Schedule: "@every 1h", Prompt: "p",
		TargetType: cron.TargetTypeDesktop,
		Desktop: &cron.DesktopTarget{
			Mode: cron.DesktopModeNew, WorkingDir: dir,
			ProviderName: "alpha", SkipApproval: true,
		},
	})
	require.NoError(t, err)

	built, err := d.createCronSession(job)
	require.NoError(t, err)
	assert.Equal(t, "每日巡检", built.Title)
	assert.Equal(t, "alpha", built.ProviderName)
	assert.Equal(t, dir, built.WorkingDir)
	assert.True(t, built.SkipApproval, "the template seeds the conversation's approval mode")
	assert.Equal(t, built.LogicalConversationID(), built.ID)

	updated, err := sched.BindDesktopSession(job.ID, built.LogicalConversationID())
	require.NoError(t, err)
	assert.Equal(t, cron.DesktopModeSession, updated.Desktop.Mode)
	assert.Equal(t, built.LogicalConversationID(), updated.Desktop.SessionID)
	assert.Empty(t, updated.Desktop.WorkingDir)
}

func TestCronTurnContextFollowsTheSessionApprovalMode(t *testing.T) {
	ctx := context.Background()
	assert.False(t, askable(cronTurnContext(ctx, &session.Session{SkipApproval: false})),
		"a session that asks must refuse an unattended ask")
	assert.True(t, askable(cronTurnContext(ctx, &session.Session{SkipApproval: true})),
		"a session with 无需审批 must not add the no-ask marker")
	assert.False(t, askable(cronTurnContext(ctx, nil)))
}

func TestCronPromptIsStableAndCarriesThePolicy(t *testing.T) {
	job := &cron.Job{Name: "x", Prompt: "整理站会要点", Notify: cron.NotifyAlways}
	a := cronModelPrompt(job)
	b := cronModelPrompt(job)
	assert.Equal(t, a, b, "the model-facing text must not change between runs (prompt cache)")
	assert.Contains(t, a, "整理站会要点")
	assert.Contains(t, a, "Do not ask the user questions")
	assert.NotContains(t, a, cron.SilentMarker)

	job.Notify = cron.NotifyWhenRelevant
	assert.Contains(t, cronModelPrompt(job), cron.SilentMarker)

	assert.Contains(t, cronDisplayText(job), "⏰ x")
	assert.Contains(t, cronDisplayText(job), "整理站会要点")
}

func TestCronToolTargetBindsTheCurrentConversation(t *testing.T) {
	d, store := cronTestApp(t)
	sess := addSession(t, store, &session.Session{
		ID: "s1", Title: "工作", WorkingDir: t.TempDir(),
		ProviderName: "alpha", SkipApproval: true,
	})
	// sessionRecord resolves through the per-session run; give it one.
	sm := session.NewManagerWithStore(store, nil)
	require.NoError(t, func() error { _, err := sm.Load("s1"); return err }())
	d.mu.Lock()
	d.runs["s1"] = &sessionRun{sm: sm}
	d.mu.Unlock()

	target := d.cronToolTarget("s1")
	assert.Equal(t, cron.TargetTypeDesktop, target.Type)
	assert.Equal(t, "s1", target.ID())
	assert.True(t, target.SupportsNewSession)

	job := &cron.Job{}
	require.NoError(t, target.Prepare(job, "session"))
	require.NotNil(t, job.Desktop)
	assert.Equal(t, cron.DesktopModeSession, job.Desktop.Mode)
	assert.Equal(t, "s1", job.Desktop.SessionID)
	assert.Equal(t, "s1", job.CreatedBy)

	next := &cron.Job{}
	require.NoError(t, target.Prepare(next, "new"))
	require.NotNil(t, next.Desktop)
	assert.Equal(t, cron.DesktopModeNew, next.Desktop.Mode)
	assert.Empty(t, next.Desktop.SessionID)
	assert.Equal(t, sess.WorkingDir, next.Desktop.WorkingDir)
	assert.Equal(t, "alpha", next.Desktop.ProviderName)
	assert.True(t, next.Desktop.SkipApproval)

	assert.True(t, target.Matches(&cron.Job{
		TargetType: cron.TargetTypeDesktop,
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: "s1"},
	}))
	assert.True(t, target.Matches(&cron.Job{
		TargetType: cron.TargetTypeDesktop,
		CreatedBy:  "s1",
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: "other"},
	}))
	assert.False(t, target.Matches(&cron.Job{
		TargetType: cron.TargetTypeDesktop,
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: "other"},
	}))
}

// A job belongs to the CONVERSATION, whose id survives compaction; the tool
// must also recognize that id as the conversation it is in, or the model reads
// the target id, compares it with its (newer) session id and concludes the job
// went somewhere else.
func TestCronToolTargetKeepsTheConversationIDAcrossCompaction(t *testing.T) {
	d, store := cronTestApp(t)
	parent := addSession(t, store, &session.Session{ID: "p1", Title: "对话"})
	addSession(t, store, &session.Session{
		ID: "c1", Title: "对话", LogicalID: "p1", CompactedParentID: "p1",
	})
	parent.CompactedChildID = "c1"
	require.NoError(t, store.UpdateMeta(parent))

	target := d.cronToolTarget("c1")
	assert.Equal(t, "p1", target.ID(), "a job belongs to the conversation, not one page of it")
	require.NotNil(t, target.SelfID)
	assert.Equal(t, "p1", target.SelfID(), "the running page must still name the conversation as its own")

	job := &cron.Job{TargetType: cron.TargetTypeDesktop}
	require.NoError(t, target.Prepare(job, "session"))
	assert.Equal(t, "p1", job.Desktop.SessionID)
}

func TestCronHandlerReloadsTheJobItWasGiven(t *testing.T) {
	d, _ := cronTestApp(t)
	sched := cron.NewScheduler(cron.SchedulerConfig{
		Store:   cron.NewStore(filepath.Join(t.TempDir(), "crons.json")),
		Handler: func(context.Context, *cron.Job) error { return nil },
	})
	d.cron = sched

	job, err := sched.Create(&cron.Job{
		Name: "first run", Schedule: "@every 1h", Prompt: "p",
		TargetType: cron.TargetTypeDesktop,
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeNew, WorkingDir: t.TempDir()},
	})
	require.NoError(t, err)
	// The scheduler's engine closure holds THIS copy. The first run binds the
	// conversation by rewriting the store, not by rescheduling — so the copy
	// must be refreshed before the next fire, or it would create another one.
	stale := job.Clone()
	_, err = sched.BindDesktopSession(job.ID, "conv-1")
	require.NoError(t, err)

	fresh := d.freshCronJob(stale)
	require.NotNil(t, fresh.Desktop)
	assert.Equal(t, cron.DesktopModeSession, fresh.Desktop.Mode)
	assert.Equal(t, "conv-1", fresh.Desktop.SessionID,
		"the handler must act on the bound record, not the engine's captured copy")
}

func TestSyncCronProtectionFollowsJobsAndLeaves(t *testing.T) {
	d, store := cronTestApp(t)
	parent := addSession(t, store, &session.Session{ID: "p1", Title: "parent"})
	child := addSession(t, store, &session.Session{
		ID: "c1", Title: "child", LogicalID: "p1", CompactedParentID: "p1",
	})
	parent.CompactedChildID = "c1"
	require.NoError(t, store.UpdateMeta(parent))
	other := addSession(t, store, &session.Session{ID: "s2", Title: "other"})

	sched := cron.NewScheduler(cron.SchedulerConfig{
		Store:   cron.NewStore(filepath.Join(t.TempDir(), "crons.json")),
		Handler: func(context.Context, *cron.Job) error { return nil },
	})
	d.cron = sched
	job, err := sched.Create(&cron.Job{
		Name: "bound", Schedule: "0 9 * * *", Prompt: "p",
		TargetType: cron.TargetTypeDesktop,
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: "p1"},
	})
	require.NoError(t, err)

	d.syncCronProtection()

	gotParent, err := store.LoadMeta(parent.ID)
	require.NoError(t, err)
	assert.False(t, gotParent.CronBound, "the parent is history; only the leaf may be protected")
	gotChild, err := store.LoadMeta(child.ID)
	require.NoError(t, err)
	assert.True(t, gotChild.CronBound, "the leaf the job resolves to must be protected")
	gotOther, err := store.LoadMeta(other.ID)
	require.NoError(t, err)
	assert.False(t, gotOther.CronBound)

	require.NoError(t, sched.Delete(job.ID))
	d.syncCronProtection()
	gotChild, err = store.LoadMeta(child.ID)
	require.NoError(t, err)
	assert.False(t, gotChild.CronBound, "deleting the job must release the session")
}

func TestPreviewCron(t *testing.T) {
	svc := &AgentService{desk: newDesktopApp()}
	p := svc.PreviewCron("0 9 * * *", "")
	assert.Empty(t, p.Error)
	assert.Len(t, p.Next, cronPreviewCount)
	assert.NotEmpty(t, p.LocalTimezone)

	p = svc.PreviewCron("nonsense", "")
	assert.NotEmpty(t, p.Error)
	assert.Empty(t, p.Next)
}

func TestListCronJobsResolvesItsTarget(t *testing.T) {
	d, store := cronTestApp(t)
	sess := addSession(t, store, &session.Session{ID: "s1", Title: "重构调度器"})
	sched := cron.NewScheduler(cron.SchedulerConfig{
		Store:   cron.NewStore(filepath.Join(t.TempDir(), "crons.json")),
		Handler: func(context.Context, *cron.Job) error { return nil },
	})
	d.cron = sched
	d.activeID = "s1"

	_, err := sched.Create(&cron.Job{
		Name: "每日站会", Schedule: "0 9 * * *", Prompt: "p",
		TargetType: cron.TargetTypeDesktop,
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: sess.LogicalConversationID()},
	})
	require.NoError(t, err)
	_, err = sched.Create(&cron.Job{
		Name: "待创建的", Schedule: "0 10 * * *", Prompt: "p",
		TargetType: cron.TargetTypeDesktop,
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeNew, WorkingDir: t.TempDir()},
	})
	require.NoError(t, err)

	jobs := (&AgentService{desk: d}).ListCronJobs()
	require.Len(t, jobs, 2)

	byName := map[string]CronJobVO{}
	for _, j := range jobs {
		byName[j.Name] = j
	}
	bound := byName["每日站会"]
	assert.Equal(t, "session", bound.TargetKind)
	assert.Equal(t, "重构调度器", bound.TargetTitle)
	assert.True(t, bound.TargetIsCurrent)
	assert.False(t, bound.NextRun.IsZero())
	assert.NotEmpty(t, bound.ScheduleText)

	pending := byName["待创建的"]
	assert.Equal(t, "pending", pending.TargetKind)
	assert.Empty(t, pending.TargetSessionID)
	assert.False(t, pending.TargetSkipApproval)
}

func TestCronJobRecordsReadsTheStoreWhenTheSchedulerIsOff(t *testing.T) {
	d, _ := cronTestApp(t)
	path := filepath.Join(t.TempDir(), "crons.json")
	store := cron.NewStore(path)
	require.NoError(t, store.Create(&cron.Job{
		ID: "j1", Name: "off", Schedule: "@every 1h", Prompt: "p",
		TargetType: cron.TargetTypeDesktop,
		Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeNew},
	}))
	d.cfg = &config.Config{Cron: config.CronConfig{StorePath: path}}

	jobs, err := d.cronJobRecords()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "off", jobs[0].Name)
}
