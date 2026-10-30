package tools

import (
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/cron"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tool's by-id actions must be scoped to the conversation it belongs to.
// `list` filtering alone would let a model that saw an id elsewhere update or
// delete another conversation's (or another entry point's) task.
func TestCronToolScopesByIDActionsToItsConversation(t *testing.T) {
	sched := cron.NewScheduler(cron.SchedulerConfig{
		Store: cron.NewStore(filepath.Join(t.TempDir(), "crons.json")),
	})
	mk := func(thread string) *cron.Job {
		job, err := sched.Create(&cron.Job{
			Name: thread, Schedule: "@every 1h", Prompt: "original",
			TargetType: cron.TargetTypeChannel, TargetThreadID: thread,
		})
		require.NoError(t, err)
		return job
	}
	mine, theirs := mk("t1"), mk("t2")

	tool := NewCronTool(sched, CronTarget{
		Type: cron.TargetTypeChannel,
		ID:   func() string { return "t1" },
		Matches: func(j *cron.Job) bool {
			return j.TargetThreadID == "t1"
		},
	})

	msg, err := tool.handleUpdate(cronArgs{ID: theirs.ID, Prompt: "hijacked"})
	require.NoError(t, err)
	assert.Contains(t, msg, "not found")

	msg, err = tool.handleGet(cronArgs{ID: theirs.ID})
	require.NoError(t, err)
	assert.Contains(t, msg, "not found")

	msg, err = tool.handleDelete(cronArgs{ID: theirs.ID})
	require.NoError(t, err)
	assert.Contains(t, msg, "not found")

	msg, err = tool.handlePause(cronArgs{ID: theirs.ID})
	require.NoError(t, err)
	assert.Contains(t, msg, "not found")

	still, err := sched.Get(theirs.ID)
	require.NoError(t, err)
	require.NotNil(t, still, "another conversation's job must survive")
	assert.Equal(t, "original", still.Prompt)
	assert.Equal(t, cron.JobStatusActive, still.Status)

	// The tool's own job is reachable.
	msg, err = tool.handleUpdate(cronArgs{ID: mine.ID, Prompt: "updated"})
	require.NoError(t, err)
	assert.NotContains(t, msg, "not found")
	updated, err := sched.Get(mine.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated", updated.Prompt)
}

// A job that writes into the calling conversation must SAY so. On the desktop
// its target id is the logical conversation id, which stops equalling the
// session id the moment that conversation is compacted — without the label the
// model reads a foreign id and may conclude the job went elsewhere.
func TestDescribeTargetMarksTheCurrentConversation(t *testing.T) {
	tool := NewCronTool(nil, CronTarget{
		Type:   cron.TargetTypeDesktop,
		ID:     func() string { return "logical-1" },
		SelfID: func() string { return "logical-1" },
	})

	self := &cron.Job{TargetType: cron.TargetTypeDesktop,
		Desktop: &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: "logical-1"}}
	assert.Equal(t, "desktop conversation logical-1 (this conversation)", cron.DescribeTarget(self, tool.selfID()))

	other := &cron.Job{TargetType: cron.TargetTypeDesktop,
		Desktop: &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: "logical-2"}}
	assert.Equal(t, "desktop conversation logical-2", cron.DescribeTarget(other, tool.selfID()))

	pending := &cron.Job{TargetType: cron.TargetTypeDesktop,
		Desktop: &cron.DesktopTarget{Mode: cron.DesktopModeNew}}
	assert.Equal(t, "a new desktop conversation (created on the first run)", cron.DescribeTarget(pending, tool.selfID()))

	thread := &cron.Job{TargetType: cron.TargetTypeChannel, TargetThreadID: "t1"}
	assert.Equal(t, "channel thread t1", cron.DescribeTarget(thread, tool.selfID()), "a desktop tool must not claim a thread")

	ch := NewCronTool(nil, CronTarget{
		Type:   cron.TargetTypeChannel,
		ID:     func() string { return "t1" },
		SelfID: func() string { return "t1" },
	})
	assert.Equal(t, "channel thread t1 (this conversation)", cron.DescribeTarget(thread, ch.selfID()))
}
