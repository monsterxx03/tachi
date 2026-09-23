package manager

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInteractiveChannel embeds mockChannel (satisfying channel.Channel) and
// adds the InteractiveChannel methods, simulating an interactive channel
// (e.g. Discord with native AskUserQuestion UI).
type mockInteractiveChannel struct {
	mockChannel
}

func (m *mockInteractiveChannel) Interactive() bool { return true }

func (m *mockInteractiveChannel) PresentQuestions(ctx context.Context, threadID, replyID string, questions []channel.Question) error {
	return nil
}

// TestBuildAgent_InteractiveChannelRegistersAskUser guards against the
// regression where channel-mode agents had AskUserQuestion unregistered at
// construction (PermissionModeSkip) and the interactive branch only logged
// "thread is interactive" without re-registering the tool — leaving the LLM
// unable to call it even on interactive channels.
func TestBuildAgent_InteractiveChannelRegistersAskUser(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	mgr.setThreadChannel(threadID, &mockInteractiveChannel{})

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	assert.Contains(t, a.ToolNames(), agenttools.ToolNameAskUser,
		"interactive channel should keep AskUserQuestion registered")
}

// TestBuildAgent_NonInteractiveChannelUnregistersAskUser verifies the
// default behavior for plain channels (weixin/github): AskUserQuestion is
// unregistered so the LLM never tries to use it.
func TestBuildAgent_NonInteractiveChannelUnregistersAskUser(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	mgr.setThreadChannel(threadID, &mockChannel{name: "plain"})

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	assert.NotContains(t, a.ToolNames(), agenttools.ToolNameAskUser)
}

// TestBuildAgent_UnknownThreadUnregistersAskUser covers the safety default:
// a thread with no registered channel must not accidentally keep AskUser
// registered (it would render no UI and hang the turn).
func TestBuildAgent_UnknownThreadUnregistersAskUser(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)
	// No setThreadChannel on purpose.

	a, err := mgr.buildAgent(context.Background(), threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	defer a.Close()

	assert.NotContains(t, a.ToolNames(), agenttools.ToolNameAskUser)
}

// TestHandler_AskUserAnswerRouting pins the three ways a message can answer an
// AskUser question that drainEvents is parked on — the structured answers a
// channel delivers from a UI callback, raw text as a fallback answer to the
// first question, and an explicit cancel. All three must reach the parked wait
// instead of becoming a new turn or a queued steer, and the answer is never
// also queued as steer text.
func TestHandler_AskUserAnswerRouting(t *testing.T) {
	const threadID = "test:askuser-routing"

	cases := []struct {
		name string
		msg  channel.IncomingMessage
		want map[string]string // nil = the cancel path (nil answers)
	}{
		{
			name: "structured answers from a UI callback",
			msg:  channel.IncomingMessage{AskUserAnswers: map[string]string{"q0": "A", "q1": "B"}},
			want: map[string]string{"q0": "A", "q1": "B"},
		},
		{
			name: "raw text falls back to the first question",
			msg:  channel.IncomingMessage{Content: "直接回答"},
			want: map[string]string{"q0": "直接回答"},
		},
		{
			name: "explicit cancel",
			msg:  channel.IncomingMessage{CancelAskUser: true, Content: "算了"},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newTestManagerWithProvider(t)

			ta := &threadActivation{
				steerRespCh:   make(chan agent.SteerInput, 1),
				resultCh:      make(chan handlerResult, 1),
				askUserRespCh: make(chan agenttools.AskUserResult, 1),
			}
			ta.ctx, ta.cancel = t.Context(), func() {}
			mgr.threads.beginTurn(threadID, func() *threadActivation { return ta })

			tc.msg.ThreadID = threadID
			tc.msg.MessageID = "msg-answer"
			tc.msg.Directed = true

			result := mgr.buildHandler()(t.Context(), tc.msg)
			assert.True(t, result.Steered, "an answer is not a new turn")

			select {
			case got := <-ta.askUserRespCh:
				assert.Equal(t, tc.want, got.Answers)
			default:
				t.Fatal("the answer never reached the parked AskUser wait")
			}

			ta.mu.Lock()
			defer ta.mu.Unlock()
			assert.Empty(t, ta.pending, "the answer must not also be queued as steer text")
		})
	}
}
