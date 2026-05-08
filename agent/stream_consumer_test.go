package agent

import (
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamAccumulator_TextOnly(t *testing.T) {
	streamCh := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(streamCh)
		streamCh <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "Hello "}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "World"}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}}
	}()

	eventCh := make(chan AgentEvent, 16)
	acc, err := consumeStream(streamCh, eventCh, 1)
	close(eventCh)
	require.NoError(t, err)

	assert.Equal(t, "Hello World", acc.text.String())
	assert.Equal(t, "stop", acc.finishReason)
	assert.Equal(t, int64(10), acc.usage.InputTokens)
	assert.Equal(t, int64(5), acc.usage.OutputTokens)
	assert.Empty(t, acc.toolCalls)

	// Collect events
	var events []AgentEvent
	for e := range eventCh {
		events = append(events, e)
	}
	assert.Len(t, events, 2)
	assert.Equal(t, AgentEventTextDelta, events[0].Type)
	assert.Equal(t, "Hello ", events[0].TextDelta)
}

func TestStreamAccumulator_ThinkingBlocks(t *testing.T) {
	streamCh := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(streamCh)
		streamCh <- llm.StreamEvent{Type: llm.StreamEventThinkingDelta, ThinkingDelta: "Let me think..."}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventSignatureDelta, SignatureDelta: "sig-xyz"}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "Answer"}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
	}()

	eventCh := make(chan AgentEvent, 16)
	acc, err := consumeStream(streamCh, eventCh, 1)
	close(eventCh)
	require.NoError(t, err)

	assert.Equal(t, "Answer", acc.text.String())
	require.Len(t, acc.thinkBlocks, 1)
	assert.Equal(t, "Let me think...", acc.thinkBlocks[0].Thinking)
	assert.Equal(t, "sig-xyz", acc.thinkBlocks[0].Signature)
}

func TestStreamAccumulator_SingleToolCall(t *testing.T) {
	streamCh := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(streamCh)
		streamCh <- llm.StreamEvent{
			Type:      llm.StreamEventToolUseStart,
			ToolIndex: 0,
			ToolCall: &llm.ToolCall{
				ID:   "call-1",
				Type: "function",
				Function: llm.ToolCallFunction{
					Name: "Bash",
				},
			},
		}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventInputJSONDelta, ToolIndex: 0, InputDelta: `{"c`}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventInputJSONDelta, ToolIndex: 0, InputDelta: `ommand": "ls"}`}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls"}
	}()

	eventCh := make(chan AgentEvent, 16)
	acc, err := consumeStream(streamCh, eventCh, 1)
	close(eventCh)
	require.NoError(t, err)

	require.Len(t, acc.toolCalls, 1)
	assert.Equal(t, "call-1", acc.toolCalls[0].ID)
	assert.Equal(t, "Bash", acc.toolCalls[0].Function.Name)
	assert.Equal(t, `{"command": "ls"}`, acc.toolCalls[0].Function.Arguments)

	msg := acc.assistantMessage()
	assert.Equal(t, "assistant", msg.Role)
	assert.Len(t, msg.ToolCalls, 1)
}

func TestStreamAccumulator_MultipleToolCalls(t *testing.T) {
	streamCh := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(streamCh)
		// Tool call A
		streamCh <- llm.StreamEvent{
			Type:      llm.StreamEventToolUseStart,
			ToolIndex: 0,
			ToolCall:  &llm.ToolCall{ID: "call-a", Type: "function", Function: llm.ToolCallFunction{Name: "Read"}},
		}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventInputJSONDelta, ToolIndex: 0, InputDelta: `{"path":"/a"}`}
		// Tool call B
		streamCh <- llm.StreamEvent{
			Type:      llm.StreamEventToolUseStart,
			ToolIndex: 1,
			ToolCall:  &llm.ToolCall{ID: "call-b", Type: "function", Function: llm.ToolCallFunction{Name: "Glob"}},
		}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventInputJSONDelta, ToolIndex: 1, InputDelta: `{"pattern":"*.go"}`}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls"}
	}()

	eventCh := make(chan AgentEvent, 16)
	acc, err := consumeStream(streamCh, eventCh, 1)
	close(eventCh)
	require.NoError(t, err)

	require.Len(t, acc.toolCalls, 2)
	assert.Equal(t, "call-a", acc.toolCalls[0].ID)
	assert.Equal(t, "Read", acc.toolCalls[0].Function.Name)
	assert.Equal(t, `{"path":"/a"}`, acc.toolCalls[0].Function.Arguments)

	assert.Equal(t, "call-b", acc.toolCalls[1].ID)
	assert.Equal(t, "Glob", acc.toolCalls[1].Function.Name)
	assert.Equal(t, `{"pattern":"*.go"}`, acc.toolCalls[1].Function.Arguments)
}

func TestStreamAccumulator_StreamError(t *testing.T) {
	streamCh := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(streamCh)
		streamCh <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "partial"}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventError, Error: assert.AnError}
	}()

	eventCh := make(chan AgentEvent, 16)
	_, err := consumeStream(streamCh, eventCh, 3)
	close(eventCh)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iteration 3")
}

func TestStreamAccumulator_InputJSONFallback(t *testing.T) {
	// When toolIndex is not tracked (e.g., OpenAI sends InputJSONDelta
	// without a preceding ToolUseStart that we correctly mapped),
	// the delta should fall back to the last tool args.
	streamCh := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(streamCh)
		streamCh <- llm.StreamEvent{
			Type:      llm.StreamEventToolUseStart,
			ToolIndex: 0,
			ToolCall:  &llm.ToolCall{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "Bash"}},
		}
		// No index mapping for this one
		streamCh <- llm.StreamEvent{Type: llm.StreamEventInputJSONDelta, ToolIndex: 99, InputDelta: `{"cmd":"ls"}`}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls"}
	}()

	eventCh := make(chan AgentEvent, 16)
	acc, err := consumeStream(streamCh, eventCh, 1)
	close(eventCh)
	require.NoError(t, err)

	require.Len(t, acc.toolCalls, 1)
	assert.Equal(t, `{"cmd":"ls"}`, acc.toolCalls[0].Function.Arguments)
}

func TestStreamAccumulator_ThinkingOnly(t *testing.T) {
	// Thinking blocks without any text content (e.g., pure tool_use after thinking)
	streamCh := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(streamCh)
		streamCh <- llm.StreamEvent{Type: llm.StreamEventThinkingDelta, ThinkingDelta: "Analysis"}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventSignatureDelta, SignatureDelta: "s"}
		streamCh <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
	}()

	eventCh := make(chan AgentEvent, 16)
	acc, err := consumeStream(streamCh, eventCh, 1)
	close(eventCh)
	require.NoError(t, err)

	assert.Empty(t, acc.text.String())
	require.Len(t, acc.thinkBlocks, 1)
	assert.Equal(t, "Analysis", acc.thinkBlocks[0].Thinking)

	msg := acc.assistantMessage()
	assert.Equal(t, "", msg.Content)
	assert.Len(t, msg.ThinkingBlocks, 1)
}
