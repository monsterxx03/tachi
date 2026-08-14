package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// TestAgentLoop_CancelPersistsPartialTurnToDisk verifies the disk fallback
// that ACP's mid-turn cancel path relies on: the agent loop records messages
// incrementally (user → assistant with tool calls → each tool result), so an
// interrupted turn's executed steps are already persisted and can be restored
// for the next Prompt even when the in-memory history cache is invalidated.
func TestAgentLoop_CancelPersistsPartialTurnToDisk(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"cmd1"}`),
			toolCallSeq("Bash", "call-2", `{"command":"cmd2"}`),
			// nil = hang stream: the third LLM call never completes on its
			// own — it stays "in flight" until the test cancels, then aborts
			// with a stream error (exactly like a real provider whose network
			// stream is cut mid-turn). This makes the mid-turn cancel
			// deterministic regardless of scheduler timing: the loop cannot
			// finish this turn before cancel() lands.
			nil,
		},
	}

	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	a := newTestAgent(t, mp, func(ag *AIAgent) { ag.SetSessionManager(sm) })
	a.RegisterTool(echoStub())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch := a.RunConversationStream(ctx, nil, "interrupted turn", "", llm.ChatOptions{MaxTokens: 4096})

	// Wait for two completed tool calls — their results are written to disk
	// BEFORE their ToolResult events are emitted — then cancel mid-turn.
	toolResults := 0
	timeout := time.After(5 * time.Second)
	for toolResults < 2 {
		select {
		case ev, ok := <-ch:
			require.True(t, ok, "event channel closed before two tool results arrived")
			if ev.Type == AgentEventToolResult {
				toolResults++
			}
		case <-timeout:
			t.Fatal("timed out waiting for two tool results")
		}
	}
	cancel()

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonInterrupted, result.ExitReason)

	// The partial turn must be fully on disk: user, two assistant turns with
	// their tool calls, and two tool results.
	msgs, err := sm.LoadMessages()
	require.NoError(t, err)
	types := make([]session.MessageType, 0, len(msgs))
	for _, m := range msgs {
		types = append(types, m.Type)
	}
	assert.Equal(t, []session.MessageType{
		session.MessageTypeUser,
		session.MessageTypeAssistant,
		session.MessageTypeToolCall,
		session.MessageTypeToolResult,
		session.MessageTypeAssistant,
		session.MessageTypeToolCall,
		session.MessageTypeToolResult,
	}, types, "interrupted turn's steps must be persisted incrementally")

	// ... and must be convertible back to a usable LLM conversation (the ACP
	// disk fallback path used after a mid-turn cancel).
	llmMsgs, err := ConvertSessionToLLMMessages(msgs, "mock")
	require.NoError(t, err)
	require.Len(t, llmMsgs, 5, "expected user + 2×(assistant-with-tool-call + tool result)")
	assert.Equal(t, "user", llmMsgs[0].Role)
	assert.Equal(t, "assistant", llmMsgs[1].Role)
	require.Len(t, llmMsgs[1].ToolCalls, 1)
	assert.Equal(t, "call-1", llmMsgs[1].ToolCalls[0].ID)
	assert.Equal(t, "tool", llmMsgs[2].Role)
	assert.Equal(t, "assistant", llmMsgs[3].Role)
	require.Len(t, llmMsgs[3].ToolCalls, 1)
	assert.Equal(t, "call-2", llmMsgs[3].ToolCalls[0].ID)
	assert.Equal(t, "tool", llmMsgs[4].Role)
}
