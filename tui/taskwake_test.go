package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// wakeTestModel builds an idle terminal with an agent and one current session.
func wakeTestModel(t *testing.T) (*Model, *agent.AIAgent, string) {
	t.Helper()
	a := newTestAIAgent(t, &reviewMockProvider{name: "mock", model: "m"}, 10)
	t.Cleanup(a.Close)

	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, logger.New("tui-test"))
	_, err = sm.New("mock", t.TempDir())
	require.NoError(t, err)
	a.SetSessionManager(sm)

	m := testModel()
	m.agent = a
	t.Cleanup(func() {
		// Cancel and WAIT for the run to end before the temp session dir is
		// removed: a turn still writing its records would race the cleanup.
		if m.cancelFunc != nil {
			m.cancelFunc()
		}
		if m.eventCh != nil {
			done := make(chan struct{})
			go func() {
				for range m.eventCh {
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	})
	return m, a, sm.Current().LogicalConversationID()
}

// The TUI keeps ONE agent across sessions, so its queue can hold several
// conversations' events. A wake must take only the conversation on screen.
func TestTryWakeTakesOnlyTheCurrentConversation(t *testing.T) {
	m, a, conv := wakeTestModel(t)
	q := a.Config.TaskQueue

	q.Post(tasks.Event{Conversation: "another-conversation", Name: "theirs"})
	q.Post(tasks.Event{Conversation: conv, Name: "mine"})

	cmd := m.tryWake()
	require.NotNil(t, cmd, "the current conversation's task must start a turn")
	require.Equal(t, stateWaiting, m.state)

	require.Equal(t, 1, q.Len(), "the other conversation's event must stay queued")
	require.Equal(t, "another-conversation", q.Pending()[0].Conversation)
}

// Stop holds the wake until the user speaks again; a running turn leaves the
// events for the next reminder.
func TestTryWakeStaysQuietWhenPausedOrBusy(t *testing.T) {
	m, a, conv := wakeTestModel(t)
	q := a.Config.TaskQueue
	q.Post(tasks.Event{Conversation: conv, Name: "job"})

	m.wakePaused = true
	require.Nil(t, m.tryWake(), "a stop must hold the wake")
	require.Equal(t, 1, q.Len())

	m.wakePaused = false
	m.setState(stateStreaming)
	require.Nil(t, m.tryWake(), "a running turn must not be interrupted")

	m.setState(stateIdle)
	require.NotNil(t, m.tryWake())
}
