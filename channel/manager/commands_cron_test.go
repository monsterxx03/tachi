package manager

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
)

// cronTestPrompt is long enough that a summary would truncate it — the detail
// view is asserted against the whole string.
const cronTestPrompt = "summarize the auth failures of the last day and send them here, grouped by service"

// newCronTestManager returns a Manager with a scheduler backed by a temp store,
// so /cron can be exercised without touching the real crons.json.
func newCronTestManager(t *testing.T) *Manager {
	t.Helper()
	mgr := mustNewManager(t, config.DefaultConfig())
	mgr.scheduler = cron.NewScheduler(cron.SchedulerConfig{
		Store:   cron.NewStore(filepath.Join(t.TempDir(), "crons.json")),
		Handler: func(context.Context, *cron.Job) error { return nil },
		Logger:  mgr.logger,
	})
	return mgr
}

func createCronTestJob(t *testing.T, mgr *Manager, threadID string) *cron.Job {
	t.Helper()
	job, err := mgr.scheduler.Create(&cron.Job{
		Name:           "daily report",
		Schedule:       "0 9 * * *",
		Prompt:         cronTestPrompt,
		TargetType:     cron.TargetTypeChannel,
		TargetThreadID: threadID,
		MaxRetries:     2,
	})
	require.NoError(t, err)
	return job
}

func TestCronCommandShowsDetailOfOneJob(t *testing.T) {
	mgr := newCronTestManager(t)
	job := createCronTestJob(t, mgr, "thread-a")

	// A bare id is the shortest thing to type, and what the list prints.
	out, err := mgr.handleCronCommand("thread-a", job.ID)
	require.NoError(t, err)

	assert.Contains(t, out, job.ID)
	assert.Contains(t, out, "- Status: Active")
	assert.Contains(t, out, "- Schedule: `0 9 * * *`")
	assert.Contains(t, out, "- Prompt: "+job.Prompt, "detail shows the prompt whole, never a preview")
	assert.Contains(t, out, "- Target: channel thread thread-a (this conversation)")
	assert.Contains(t, out, "- Max retries: 2")
	assert.Contains(t, out, "- Next run: ")
	assert.NotContains(t, out, "...")
}

func TestCronCommandListKeepsNextRunAndDetailHint(t *testing.T) {
	mgr := newCronTestManager(t)
	job := createCronTestJob(t, mgr, "thread-a")
	// A job of another thread must not leak into this thread's list.
	createCronTestJob(t, mgr, "thread-b")

	out, err := mgr.handleCronCommand("thread-a", "")
	require.NoError(t, err)

	assert.Contains(t, out, "📋 Cron Jobs (1)")
	assert.Contains(t, out, job.ID)
	assert.NotContains(t, out, "thread-b")
	assert.Contains(t, out, "  Next: ", "the list answers the first question an operator asks")
	assert.Contains(t, out, "/cron <id>", "the list must advertise the detail command")
}

func TestCronCommandPauseResumeAndDelete(t *testing.T) {
	mgr := newCronTestManager(t)
	job := createCronTestJob(t, mgr, "thread-a")

	out, err := mgr.handleCronCommand("thread-a", "pause "+job.ID)
	require.NoError(t, err)
	assert.Contains(t, out, "Paused")
	got, err := mgr.scheduler.Get(job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cron.JobStatusPaused, got.Status)
	assert.Contains(t, out, "- Status: Paused")

	out, err = mgr.handleCronCommand("thread-a", "resume "+job.ID)
	require.NoError(t, err)
	assert.Contains(t, out, "Resumed")
	got, err = mgr.scheduler.Get(job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cron.JobStatusActive, got.Status)

	// Delete echoes the whole job: the prompt is the part that cannot be
	// recovered after the row is gone.
	out, err = mgr.handleCronCommand("thread-a", "delete "+job.ID)
	require.NoError(t, err)
	assert.Contains(t, out, "Deleted")
	assert.Contains(t, out, "- Prompt: "+cronTestPrompt)
	got, err = mgr.scheduler.Get(job.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestCronCommandRefusesJobsOfAnotherThread(t *testing.T) {
	mgr := newCronTestManager(t)
	job := createCronTestJob(t, mgr, "thread-a")

	for _, args := range []string{job.ID, "pause " + job.ID, "delete " + job.ID} {
		_, err := mgr.handleCronCommand("thread-b", args)
		require.Error(t, err, "args=%q", args)
		assert.Contains(t, err.Error(), "not found", "an invisible job reads as missing, never as a permission error")
	}

	// Nothing was touched.
	got, err := mgr.scheduler.Get(job.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cron.JobStatusActive, got.Status)
}

func TestCronCommandMissingIDPrintsUsage(t *testing.T) {
	mgr := newCronTestManager(t)

	out, err := mgr.handleCronCommand("thread-a", "pause")
	require.NoError(t, err)
	assert.Contains(t, out, "/cron pause <id>")
	assert.True(t, strings.HasPrefix(out, "Usage:"))
}

func TestCronCommandUnknownVerbsAreTreatedAsIDs(t *testing.T) {
	mgr := newCronTestManager(t)

	_, err := mgr.handleCronCommand("thread-a", "cr_nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"cr_nope" not found`)
}

func TestParseCronCommand(t *testing.T) {
	cases := []struct {
		args     string
		wantVerb string
		wantID   string
	}{
		{"", "list", ""},
		{"list", "list", ""},
		{"list cr_a1", "list", ""},
		{"cr_a1", "get", "cr_a1"},
		{"show cr_a1", "get", "cr_a1"},
		{"get cr_a1", "get", "cr_a1"},
		{"pause cr_a1", "pause", "cr_a1"},
		{"resume cr_a1", "resume", "cr_a1"},
		{"run cr_a1", "run", "cr_a1"},
		{"delete cr_a1", "delete", "cr_a1"},
		{"pause", "pause", ""},
	}
	for _, tc := range cases {
		verb, id := parseCronCommand(tc.args)
		assert.Equal(t, tc.wantVerb, verb, "args=%q", tc.args)
		assert.Equal(t, tc.wantID, id, "args=%q", tc.args)
	}
}

// nestingChannel is a channel whose threads NEST (a topic inside a chat): it
// implements channel.CronThreadChannel, which is the only hook these tests need.
type nestingChannel struct {
	mockChannel
	home func(string) string
}

func (c *nestingChannel) CronThread(threadID string) string { return c.home(threadID) }

// A job created inside a topic belongs to the CHAT: it must survive the topic and
// stay manageable from the main area. The thread it was created in is still
// recorded, for audit.
func TestCronBelongsToTheChatNotTheTopic(t *testing.T) {
	mgr := newCronTestManager(t)
	const main, topic = "wave:group:oc_x", "wave:group:oc_x/topic:otc_y"
	ch := &nestingChannel{home: func(string) string { return main }}
	mgr.setThreadChannel(topic, ch)
	mgr.setThreadChannel(main, ch)

	// The model creates a job while the conversation sits in a topic.
	tool := mgr.newThreadCronTool(topic)
	_, err := tool.ExecuteContext(t.Context(),
		`{"action":"create","name":"daily","schedule":"0 9 * * *","prompt":"p"}`)
	require.NoError(t, err)

	jobs, err := mgr.scheduler.List()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, main, job.TargetThreadID, "the job fires into the group, not the topic")
	assert.Equal(t, topic, job.CreatedBy, "the topic it was created in is still recorded")

	// Both conversations of that chat can list and operate it.
	for _, thread := range []string{main, topic} {
		out, err := mgr.handleCronCommand(thread, "")
		require.NoError(t, err)
		assert.Contains(t, out, job.ID, "thread=%s", thread)

		out, err = mgr.handleCronCommand(thread, job.ID)
		require.NoError(t, err)
		assert.Contains(t, out, "- Prompt: ", "thread=%s", thread)
	}

	// Another chat cannot see or touch it.
	_, err = mgr.handleCronCommand("wave:group:oc_other", job.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// And the model, listing from the topic, sees the job it just created.
	listOut, err := tool.ExecuteContext(t.Context(), `{"action":"list"}`)
	require.NoError(t, err)
	assert.Contains(t, listOut, job.ID)
}

// A job written before a channel lifted topics stays reachable from the topic it
// was bound to — lifting must not strand existing work.
func TestLegacyTopicJobStaysManageableFromItsTopic(t *testing.T) {
	mgr := newCronTestManager(t)
	const main, topic = "wave:group:oc_x", "wave:group:oc_x/topic:otc_y"
	mgr.setThreadChannel(topic, &nestingChannel{home: func(string) string { return main }})

	legacy, err := mgr.scheduler.Create(&cron.Job{
		Name: "old", Schedule: "0 9 * * *", Prompt: "p",
		TargetType: cron.TargetTypeChannel, TargetThreadID: topic,
	})
	require.NoError(t, err)

	out, err := mgr.handleCronCommand(topic, "")
	require.NoError(t, err)
	assert.Contains(t, out, legacy.ID, "its own topic still sees it")

	out, err = mgr.handleCronCommand(main, "")
	require.NoError(t, err)
	assert.NotContains(t, out, legacy.ID, "but the main area never did")
}

// Without the hook — a plain channel, or a thread the manager has not seen — the
// thread is its own home, i.e. exactly the behaviour before topics existed.
func TestCronHomeFallsBackToTheThread(t *testing.T) {
	mgr := newCronTestManager(t)
	mgr.setThreadChannel("plain", &mockChannel{name: "plain"})

	assert.Equal(t, "plain", mgr.cronThread("plain"))
	assert.Equal(t, "never-seen", mgr.cronThread("never-seen"))
}
