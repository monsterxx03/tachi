package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Mock Provider ----

// mockStreamProvider implements llm.Provider and lets tests control the
// exact stream events produced, including multi-turn sequences.
type mockStreamProvider struct {
	name       string
	sequences  [][]llm.StreamEvent // each entry is one API call's full stream
	callIdx    int
}

func (p *mockStreamProvider) Name() string { return p.name }

func (p *mockStreamProvider) CreateChat(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (*llm.Response, error) {
	return nil, fmt.Errorf("not implemented")
}

func (p *mockStreamProvider) CreateChatStream(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	if p.callIdx >= len(p.sequences) {
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.StreamEventError, Error: fmt.Errorf("no more sequences")}
		close(ch)
		return ch, nil
	}

	events := p.sequences[p.callIdx]
	p.callIdx++

	ch := make(chan llm.StreamEvent, len(events)+4)
	go func() {
		defer close(ch)
		for _, e := range events {
			ch <- e
		}
	}()
	return ch, nil
}

// ---- Helpers ----

// textSeq builds a single sequence that emits a text response then stops.
func textSeq(text string) []llm.StreamEvent {
	var events []llm.StreamEvent
	for _, r := range text {
		events = append(events, llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: string(r)})
	}
	events = append(events, llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}})
	return events
}

// toolCallSeq builds a single sequence that emits a tool call then tool_calls finish.
func toolCallSeq(name, id, args string) []llm.StreamEvent {
	var events []llm.StreamEvent
	events = append(events, llm.StreamEvent{
		Type:      llm.StreamEventToolUseStart,
		ToolIndex: 0,
		ToolCall:  &llm.ToolCall{ID: id, Type: "function", Function: llm.ToolCallFunction{Name: name}},
	})
	events = append(events, llm.StreamEvent{Type: llm.StreamEventInputJSONDelta, ToolIndex: 0, InputDelta: args})
	events = append(events, llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls", Usage: &llm.Usage{InputTokens: 20, OutputTokens: 10}})
	return events
}

// toolCallSeqWithText is like toolCallSeq but also emits text before the tool call.
func toolCallSeqWithText(text, name, id, args string) []llm.StreamEvent {
	var events []llm.StreamEvent
	for _, r := range text {
		events = append(events, llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: string(r)})
	}
	events = append(events, llm.StreamEvent{
		Type:      llm.StreamEventToolUseStart,
		ToolIndex: 0,
		ToolCall:  &llm.ToolCall{ID: id, Type: "function", Function: llm.ToolCallFunction{Name: name}},
	})
	events = append(events, llm.StreamEvent{Type: llm.StreamEventInputJSONDelta, ToolIndex: 0, InputDelta: args})
	events = append(events, llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls", Usage: &llm.Usage{InputTokens: 30, OutputTokens: 15}})
	return events
}

// newTestAgent creates an AIAgent preconfigured for agent-loop testing.
func newTestAgent(provider llm.Provider) *AIAgent {
	a := NewAIAgent(provider, "test-model", 10)
	a.SetSkipEditConfirm(true)
	a.SetReminderCollector(systemreminder.NewCollector()) // no reminders — clean
	a.SetContextWindow(128_000)
	return a
}

// drainAgentEvents collects all events from the channel until it closes and
// returns the final RunResult (if any).
func drainAgentEvents(ch <-chan AgentEvent) (*RunResult, []AgentEvent) {
	var events []AgentEvent
	var result *RunResult
	for e := range ch {
		events = append(events, e)
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}
	return result, events
}

// ---- Tests: RunConversationStream / runAgentLoop ----

func TestAgentLoop_SimpleTextResponse(t *testing.T) {
	mp := &mockStreamProvider{
		name:      "mock",
		sequences: [][]llm.StreamEvent{textSeq("Hello World")},
	}

	a := newTestAgent(mp)
	ch := a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, events := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "Hello World", result.Response)
	assert.Equal(t, "stop", result.ExitReason)
	assert.Equal(t, 1, result.IterationsUsed)
	assert.Nil(t, result.Error)
	assert.NotEmpty(t, events)
}

func TestAgentLoop_ToolCallThenText(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"echo hi"}`),
			textSeq("Command executed successfully"),
		},
	}

	a := newTestAgent(mp)
	// Register a simple Bash-like tool that returns fixed output
	a.RegisterTool(echoStub())

	ch := a.RunConversationStream(t.Context(), nil, "run echo", "", llm.ChatOptions{MaxTokens: 4096})

	result, events := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "Command executed successfully", result.Response)
	assert.Equal(t, "stop", result.ExitReason)

	// Verify tool result event was emitted
	var toolResults []AgentEvent
	for _, e := range events {
		if e.Type == AgentEventToolResult {
			toolResults = append(toolResults, e)
		}
	}
	assert.Len(t, toolResults, 1)
	assert.Equal(t, "Bash", toolResults[0].ToolName)
}

func TestAgentLoop_MultipleTurns(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"cmd1"}`),
			toolCallSeq("Bash", "call-2", `{"command":"cmd2"}`),
			textSeq("All done"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(echoStub())

	ch := a.RunConversationStream(t.Context(), nil, "do work", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "All done", result.Response)
	assert.Equal(t, "stop", result.ExitReason)
}

// ---- Helper stubs ----

// echoStub returns a Bash tool that parses JSON args and echoes back the command.
func echoStub() *stubTool {
	return &stubTool{
		name:     "Bash",
		desc:     "Run a command",
		parallel: true,
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(args), &m); err != nil {
				return "", err
			}
			if cmd, ok := m["command"]; ok {
				return fmt.Sprintf("executed: %s", cmd), nil
			}
			return "executed", nil
		},
	}
}

// ---- Tests: IterationBudget ----

func TestIterationBudget_Consume(t *testing.T) {
	b := &IterationBudget{Remaining: 3}
	assert.True(t, b.consume())
	assert.Equal(t, 2, b.Remaining)

	assert.True(t, b.consume())
	assert.Equal(t, 1, b.Remaining)

	assert.True(t, b.consume())
	assert.Equal(t, 0, b.Remaining)

	assert.False(t, b.consume())
}

func TestIterationBudget_Unlimited(t *testing.T) {
	b := &IterationBudget{Unlimited: true}
	for i := 0; i < 1000; i++ {
		assert.True(t, b.consume())
	}
}

func TestAgentLoop_IterationBudgetExhausted(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"cmd"}`),
			toolCallSeq("Bash", "call-2", `{"command":"cmd"}`),
			toolCallSeq("Bash", "call-3", `{"command":"cmd"}`),
		},
	}

	a := newTestAgent(mp)
	a.maxIterations = 2 // only 2 iterations allowed
	a.RegisterTool(&stubTool{
		name:     "Bash",
		desc:     "Run a command",
		parallel: true,
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(args), &m); err != nil {
				return "", err
			}
			if cmd, ok := m["command"]; ok {
				return fmt.Sprintf("executed: %s", cmd), nil
			}
			return "executed", nil
		},
	})

	ch := a.RunConversationStream(t.Context(), nil, "do a lot", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "budget_exhausted", result.ExitReason)
	assert.Contains(t, result.Error.Error(), "iteration budget exhausted")
}

func TestAgentLoop_ContextCancellation(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			textSeq("Hello"),
		},
	}

	a := newTestAgent(mp)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	ch := a.RunConversationStream(ctx, nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "interrupted", result.ExitReason)
}

func TestAgentLoop_APICallFailed(t *testing.T) {
	// Provider that fails at the CreateChatStream level (not via stream event).
	failingProvider := &failingStreamProvider{name: "fail"}
	a := newTestAgent(failingProvider)
	a.maxIterations = 1

	ch := a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "error", result.ExitReason)
	assert.Contains(t, result.Error.Error(), "API call failed")
	assert.Contains(t, result.Error.Error(), "connection refused")
}

// failingStreamProvider fails at the CreateChatStream level.
type failingStreamProvider struct {
	name string
}

func (p *failingStreamProvider) Name() string { return p.name }
func (p *failingStreamProvider) CreateChat(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (*llm.Response, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *failingStreamProvider) CreateChatStream(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	return nil, fmt.Errorf("connection refused")
}

func TestAgentLoop_StreamErrorMidway(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			{
				{Type: llm.StreamEventTextDelta, TextDelta: "partial"},
				{Type: llm.StreamEventError, Error: fmt.Errorf("connection reset")},
			},
		},
	}

	a := newTestAgent(mp)

	ch := a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Contains(t, result.Error.Error(), "connection reset")
}
