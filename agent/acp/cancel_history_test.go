package acp

import (
	"context"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

// TestStreamToACP_CancelInvalidatesStaleHistory reproduces the mid-turn
// cancellation race: the user cancels while the agent loop is still unwinding
// (e.g. a tool call in flight or the LLM still streaming). streamToACP's
// ctx.Done() branch fires BEFORE the loop's AgentEventError (which carries
// rs.Messages with the partial turn) is produced. The stale history cache from
// the previous completed turn must be invalidated so the next Prompt reloads
// the partial turn from disk — otherwise the LLM sees none of the interrupted
// turn's tool steps.
func TestStreamToACP_CancelInvalidatesStaleHistory(t *testing.T) {
	sess := &ACPSession{
		ID: "test-session",
		// Stale cache from a PREVIOUS completed turn.
		history: []llm.Message{
			{Role: "user", Content: "previous turn"},
			{Role: "assistant", Content: "previous answer"},
		},
	}

	// No events: the agent loop is still unwinding — its error event has not
	// arrived yet. Only ctx cancellation can fire.
	events := make(chan agent.AgentEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var stopReason acp.StopReason
	go func() {
		stopReason, _, _ = streamToACP(ctx, sess, nil, events)
		close(done)
	}()

	// Let the goroutine reach the select, then cancel (user pressed stop).
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamToACP did not return after ctx cancellation")
	}

	assert.Equal(t, acp.StopReasonCancelled, stopReason)
	// The stale cache must not leak into the next Prompt: the partial turn
	// has to be reloaded from disk instead.
	assert.Nil(t, sess.history, "stale history cache must be invalidated on mid-turn cancel")
}

// TestStreamToACP_ErrorEventSavesPartialHistory pins the normal (non-raced)
// path: when the AgentEventError IS consumed, the partial messages are cached
// (with dangling tool calls stripped) so the next Prompt resumes from them.
func TestStreamToACP_ErrorEventSavesPartialHistory(t *testing.T) {
	sess := &ACPSession{ID: "test-session"}

	events := make(chan agent.AgentEvent, 1)
	events <- agent.AgentEvent{
		Type: agent.AgentEventError,
		Messages: []llm.Message{
			{Role: "user", Content: "do the thing"},
			{Role: "assistant", Content: "let me read first", ToolCalls: []llm.ToolCall{
				{ID: "call_1", Type: "function", Function: llm.ToolCallFunction{Name: "Read"}},
			}},
			// no tool result — dangling tool call must be stripped
		},
		Result: &agent.RunResult{ExitReason: agent.ExitReasonInterrupted, Error: context.Canceled},
	}
	close(events)

	stopReason, _, err := streamToACP(context.Background(), sess, nil, events)
	require.NoError(t, err)
	assert.Equal(t, acp.StopReasonCancelled, stopReason)

	require.NotNil(t, sess.history, "consumed error event must cache partial messages")
	require.Len(t, sess.history, 2)
	assert.Empty(t, sess.history[1].ToolCalls, "dangling tool calls must be stripped")
}
