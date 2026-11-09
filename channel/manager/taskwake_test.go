package manager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tasks"
)

// One shared ProcessManager serves every thread, so a finished process's event
// must find the queue of the thread whose conversation it belongs to — not the
// first agent that happened to bind the manager's sink.
func TestTaskEventsRouteToTheOwningThreadsQueue(t *testing.T) {
	m := newTestManagerWithProvider(t)
	defer m.Close()

	a := m.taskQueueFor("thread-a")
	b := m.taskQueueFor("thread-b")
	// Held so the watchers do not start real turns while the test inspects routing.
	a.Pause()
	b.Pause()
	m.bindTaskConversation("conv-a", "thread-a")
	m.bindTaskConversation("conv-b", "thread-b")

	m.postTaskEvent(tasks.Event{Conversation: "conv-a", Name: "one"})
	m.postTaskEvent(tasks.Event{Conversation: "conv-b", Name: "two"})
	m.postTaskEvent(tasks.Event{Conversation: "nobody's", Name: "dropped"})

	require.Len(t, a.Pending(), 1)
	require.Len(t, b.Pending(), 1)
	require.Equal(t, 0, m.taskQueueFor("thread-c").Len())
}

// The host's sink asks the channel first: a conversation a channel owns must be
// continued by the channel, and an unknown one is left to the caller.
func TestPostTaskClaimsOnlyItsOwnConversations(t *testing.T) {
	m := newTestManagerWithProvider(t)
	defer m.Close()

	q := m.taskQueueFor("thread-a")
	q.Pause()
	m.bindTaskConversation("conv-a", "thread-a")

	assert.False(t, m.PostTask("conv-unknown", tasks.Event{Name: "x"}))
	assert.True(t, m.PostTask("conv-a", tasks.Event{Conversation: "conv-a", Name: "x"}))
	assert.Len(t, q.Pending(), 1)
}

// /stop holds the wake until the next inbound message; the pause is per thread.
func TestStopHoldsTheThreadsWake(t *testing.T) {
	m := newTestManagerWithProvider(t)
	defer m.Close()

	q := m.taskQueueFor("thread-a")
	require.False(t, q.Paused())

	_, err := m.handleStopCommand("thread-a")
	require.NoError(t, err)
	require.True(t, q.Paused(), "a stop must hold the thread's automatic wake")

	m.resumeTaskQueue("thread-a")
	require.False(t, q.Paused(), "an inbound message releases it")
}

// A finished task really runs a turn on its thread: the reply path is the cron
// trigger's (acquire the agent, load the thread session, run, deliver).
func TestWakeThreadRunsATurn(t *testing.T) {
	m := newTestManagerWithProvider(t)
	defer m.Close()

	const thread = "thread-wake"
	m.taskQueueFor(thread).Pause()
	m.bindTaskConversation("conv-wake", thread)
	p := m.defaultResolvedProvider.Provider.(*mockProvider)
	p.responses = []string{"继续完成"}

	m.wakeThread(context.Background(), thread, []tasks.Event{
		{Kind: tasks.KindBackground, Name: "build", Conversation: "conv-wake"},
	})
	require.Equal(t, 1, p.respIdx, "the wake must have asked the model to continue")
}
