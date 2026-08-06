package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Mock Provider ----

// mockStreamProvider implements llm.Provider and lets tests control the
// exact stream events produced, including multi-turn sequences.
type mockStreamProvider struct {
	name         string
	providerName string              // config provider name (Provider.ProviderName); "" = unknown
	sequences    [][]llm.StreamEvent // each entry is one API call's full stream
	callIdx      int
}

func (p *mockStreamProvider) Name() string         { return p.name }
func (p *mockStreamProvider) Model() string        { return "mock-model" }
func (p *mockStreamProvider) ProviderName() string { return p.providerName }

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

// failingStreamProvider fails at the CreateChatStream level.
type failingStreamProvider struct {
	name string
}

func (p *failingStreamProvider) Name() string         { return p.name }
func (p *failingStreamProvider) ProviderName() string { return "" }
func (p *failingStreamProvider) Model() string        { return "mock-model" }
func (p *failingStreamProvider) CreateChat(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (*llm.Response, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *failingStreamProvider) CreateChatStream(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	return nil, fmt.Errorf("connection refused")
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

// newTestAgent creates an AIAgent preconfigured for agent-loop testing.
// Optional testAgentOpt values fine-tune the agent's behaviour (tools,
// permissions, iteration budget, session).
//
// The agent's Close() is registered via t.Cleanup so background processes
// are cleaned up when the test completes.
func newTestAgent(t *testing.T, provider llm.Provider, opts ...testAgentOpt) *AIAgent {
	t.Helper()
	a := NewAIAgent(provider, 10)
	a.SetPermissionMode(PermissionModeSkip)
	a.SetReminderCollector(systemreminder.NewCollector()) // no reminders — clean
	a.SetContextWindow(128_000)
	for _, opt := range opts {
		opt(a)
	}
	t.Cleanup(a.Close)
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

// ---- Helper stubs ----

// echoStub returns a Bash tool that parses JSON args and echoes back the command.
func echoStub() *stubTool {
	return &stubTool{
		name:     "Bash",
		desc:     "Run a command",
		parallel: true,
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]any
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

func confirmStub() *stubTool {
	return &stubTool{
		name:         "EditFile",
		desc:         "Edit a file",
		parallel:     false,
		needsConfirm: true,
		diffFn:       func(ctx context.Context, args string) (string, error) { return "diff preview", nil },
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]any
			json.Unmarshal([]byte(args), &m)
			return fmt.Sprintf("edited %v", m["path"]), nil
		},
	}
}

func askUserStub() *stubTool {
	return &stubTool{
		name: agenttools.ToolNameAskUser,
		desc: "Ask user",
		props: map[string]agenttools.PropertySchema{
			"questions": {Type: "array", Description: "Questions to ask"},
		},
		required: []string{"questions"},
		parallel: false,
		executeFn: func(ctx context.Context, args string) (string, error) {
			return "", &agenttools.AskUserQuestionError{
				ToolName: agenttools.ToolNameAskUser,
				Args:     args,
				Questions: []agenttools.Question{
					{Question: "What?", Header: "Q", Options: []agenttools.QuestionOption{{Label: "A", Description: "Desc"}}, MultiSelect: false},
				},
			}
		},
	}
}

// ---- Tests: RunConversationStream / runAgentLoop ----

func TestAgentLoop_SimpleTextResponse(t *testing.T) {
	mp := &mockStreamProvider{
		name:      "mock",
		sequences: [][]llm.StreamEvent{textSeq("Hello World")},
	}

	a := newTestAgent(t, mp)
	ch := a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, events := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "Hello World", result.Response)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
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

	a := newTestAgent(t, mp)
	// Register a simple Bash-like tool that returns fixed output
	a.RegisterTool(echoStub())

	ch := a.RunConversationStream(t.Context(), nil, "run echo", "", llm.ChatOptions{MaxTokens: 4096})

	result, events := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "Command executed successfully", result.Response)
	assert.Equal(t, ExitReasonStop, result.ExitReason)

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

	a := newTestAgent(t, mp)
	a.RegisterTool(echoStub())

	ch := a.RunConversationStream(t.Context(), nil, "do work", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "All done", result.Response)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
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
	for range 1000 {
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

	a := newTestAgent(t, mp)
	a.Config.MaxIterations = 2 // only 2 iterations allowed
	a.RegisterTool(&stubTool{
		name:     "Bash",
		desc:     "Run a command",
		parallel: true,
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]any
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
	assert.Equal(t, ExitReasonBudgetExhausted, result.ExitReason)
	assert.Contains(t, result.Error.Error(), "iteration budget exhausted")
}

func TestAgentLoop_ContextCancellation(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			textSeq("Hello"),
		},
	}

	a := newTestAgent(t, mp)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	ch := a.RunConversationStream(ctx, nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonInterrupted, result.ExitReason)
}

func TestAgentLoop_APICallFailed(t *testing.T) {
	// Provider that fails at the CreateChatStream level (not via stream event).
	failingProvider := &failingStreamProvider{name: "fail"}
	a := newTestAgent(t, failingProvider)
	a.Config.MaxIterations = 1

	ch := a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonError, result.ExitReason)
	assert.Contains(t, result.Error.Error(), "API call failed")
	assert.Contains(t, result.Error.Error(), "connection refused")
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

	a := newTestAgent(t, mp)

	ch := a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Contains(t, result.Error.Error(), "connection reset")
}

// ---- Tests: historyHasReminder ----

func TestHistoryHasReminder_True(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "<system-reminder>\nCurrent date: Friday, May 8, 2026\n</system-reminder>\n\nhello"},
	}
	assert.True(t, historyHasReminder(history))
}

func TestHistoryHasReminder_False(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	assert.False(t, historyHasReminder(history))
}

func TestHistoryHasReminder_Empty(t *testing.T) {
	assert.False(t, historyHasReminder(nil))
}

// TestHistoryHasReminder_StandaloneArtifactReminder guards J1/#3: a
// standalone artifact reminder spliced as the history's first message
// (thread's first message was /research or /review) must NOT count as
// evidence of an injected first-message reminder — otherwise project
// context / date / git reminders would never be injected on the first real
// user turn.
func TestHistoryHasReminder_StandaloneArtifactReminder(t *testing.T) {
	artifactReminder := session.FormatArtifactReminder([]session.ArtifactRef{
		{Kind: session.ArtifactKindResearch, Title: "主题", Path: "/tmp/r.html"},
	})
	history := []llm.Message{
		{Role: "user", Content: artifactReminder},
	}
	assert.False(t, historyHasReminder(history), "standalone artifact reminder must not count as a first-message reminder")

	// Once a REAL user message carries a reminder prefix, it counts.
	history = append(history, llm.Message{
		Role:    "user",
		Content: "<system-reminder>\nProject Context\n</system-reminder>\n\n真实用户消息",
	})
	assert.True(t, historyHasReminder(history))
}

// ---- Tests: buildLLMTools ----

func TestBuildLLMTools(t *testing.T) {
	schemas := []agenttools.Schema{
		{
			Name:        "Read",
			Description: "Read a file",
			Parameters: agenttools.ParametersSchema{
				Type: "object",
				Properties: map[string]agenttools.PropertySchema{
					"path": {Type: "string", Description: "File path"},
				},
				Required: []string{"path"},
			},
		},
	}

	llmTools := buildLLMTools(schemas)
	require.Len(t, llmTools, 1)
	assert.Equal(t, "Read", llmTools[0].Name)
	assert.Equal(t, "Read a file", llmTools[0].Description)
	assert.Equal(t, "object", llmTools[0].Parameters.Type)
	assert.Equal(t, "string", llmTools[0].Parameters.Properties["path"].Type)
	assert.Equal(t, []string{"path"}, llmTools[0].Parameters.Required)
}

func TestBuildLLMTools_ConstraintPassthrough(t *testing.T) {
	maxResults := 200.0
	schemas := []agenttools.Schema{
		{
			Name:        "Grep",
			Description: "Search",
			Parameters: agenttools.ParametersSchema{
				Type: "object",
				Properties: map[string]agenttools.PropertySchema{
					"output_mode": {
						Type:    "string",
						Enum:    []string{"files_with_matches", "content", "count"},
						Default: "files_with_matches",
					},
					"max_results": {
						Type:    "integer",
						Maximum: &maxResults,
					},
					"tags": {
						Type:  "array",
						Items: map[string]any{"type": "string", "enum": []string{"a", "b"}},
					},
				},
				Required: []string{"output_mode"},
			},
		},
	}

	llmTools := buildLLMTools(schemas)
	require.Len(t, llmTools, 1)
	props := llmTools[0].Parameters.Properties

	om := props["output_mode"]
	assert.Equal(t, []string{"files_with_matches", "content", "count"}, om.Enum)
	assert.Equal(t, "files_with_matches", om.Default)

	mr := props["max_results"]
	require.NotNil(t, mr.Maximum)
	assert.Equal(t, 200.0, *mr.Maximum)

	tags := props["tags"]
	require.NotNil(t, tags.Items)
	assert.Equal(t, map[string]any{"type": "string", "enum": []string{"a", "b"}}, tags.Items)
}

func TestBuildLLMTools_Empty(t *testing.T) {
	llmTools := buildLLMTools(nil)
	assert.Empty(t, llmTools)
}

// ---- Tests: RunConversation (non-streaming) ----

func TestRunConversation(t *testing.T) {
	mp := &mockStreamProvider{
		name:      "mock",
		sequences: [][]llm.StreamEvent{textSeq("Response")},
	}

	a := newTestAgent(t, mp)
	result := a.RunConversation(t.Context(), "hello", "", llm.ChatOptions{MaxTokens: 4096})

	require.NotNil(t, result)
	assert.Equal(t, "Response", result.Response)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
}

func TestRunConversation_AutoConfirmsTool(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"cmd"}`),
			textSeq("done"),
		},
	}

	a := newTestAgent(t, mp)
	a.RegisterTool(echoStub())
	result := a.RunConversation(t.Context(), "run a command", "", llm.ChatOptions{MaxTokens: 4096})

	require.NotNil(t, result)
	assert.Equal(t, "done", result.Response)
}

// ---- Tests: executeToolCalls integration ----

func TestExecuteToolCalls_UnknownTool(t *testing.T) {
	a := newTestAgent(t, nil)

	toolCalls := []llm.ToolCall{
		{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "NonExistent", Arguments: "{}"}},
	}

	ch := make(chan AgentEvent, 8)
	msgs, err := a.executeToolCalls(t.Context(), &RunState{}, toolCalls, ch)
	require.NoError(t, err) // unknown tool does not stop the loop, it returns an error result
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].IsError)
	assert.Contains(t, msgs[0].Content, "unknown tool")
	close(ch)
}

func TestExecuteToolCalls_ToolError(t *testing.T) {
	a := newTestAgent(t, nil)
	a.RegisterTool(&stubTool{
		name:     "ErrorTool",
		desc:     "Always errors",
		parallel: true,
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]any
			json.Unmarshal([]byte(args), &m)
			if msg, ok := m["msg"]; ok {
				return "", fmt.Errorf("%s", msg)
			}
			return "", fmt.Errorf("error")
		},
	})

	toolCalls := []llm.ToolCall{
		{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "ErrorTool", Arguments: `{"msg":"fail"}`}},
	}

	ch := make(chan AgentEvent, 8)
	msgs, err := a.executeToolCalls(t.Context(), &RunState{}, toolCalls, ch)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].IsError)
	assert.Contains(t, msgs[0].Content, "fail")
	close(ch)

	// Verify error event emitted
	var toolResults []AgentEvent
	for e := range ch {
		if e.Type == AgentEventToolResult {
			toolResults = append(toolResults, e)
		}
	}
	require.Len(t, toolResults, 1)
	assert.True(t, toolResults[0].ToolIsError)
}

// ---- Tests: handleFinishReason via the agent loop ----

// TestAgentLoop_StopWithToolCallsExecutesTools verifies that a provider
// reporting "stop" on a message that still carries tool_use deltas does NOT
// end the turn: the tools execute, then the loop continues to a normal stop.
// Without this, the turn would silently end (integrations like Herdr flip to
// idle) while the tool work was skipped.
func TestAgentLoop_StopWithToolCallsExecutesTools(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			{ // iteration 1: text + tool call, but finish_reason=stop
				{Type: llm.StreamEventTextDelta, TextDelta: "let me check"},
				{Type: llm.StreamEventToolUseStart, ToolIndex: 0, ToolCall: &llm.ToolCall{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "Bash"}}},
				{Type: llm.StreamEventInputJSONDelta, ToolIndex: 0, InputDelta: `{"command":"echo hi"}`},
				{Type: llm.StreamEventDone, FinishReason: "stop"},
			},
			textSeq("done"),
		},
	}

	a := newTestAgent(t, mp)
	a.RegisterTool(bashStub())

	ch := a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})
	result, events := drainAgentEvents(ch)

	require.NotNil(t, result)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
	// The tool must have run even though the finish reason said "stop".
	if !toolResultContains(events, false, "ran: echo hi") {
		t.Errorf("expected Bash tool result in events, got %d events", len(events))
	}
	// The turn must not have ended at the tool iteration — the final
	// response comes from the second (stop) iteration.
	assert.Equal(t, "done", result.Response)
}

func TestAgentLoop_MaxTokensContinueAndStop(t *testing.T) {
	// After 3 max_tokens continuations, the loop should stop with length_exhausted.
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			{ // iteration 1: tool call
				{Type: llm.StreamEventTextDelta, TextDelta: "partial"},
				{Type: llm.StreamEventDone, FinishReason: "max_tokens", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}},
			},
			{ // iteration 2: max_tokens again
				{Type: llm.StreamEventTextDelta, TextDelta: "more"},
				{Type: llm.StreamEventDone, FinishReason: "max_tokens", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}},
			},
			{ // iteration 3: max_tokens again
				{Type: llm.StreamEventTextDelta, TextDelta: "final"},
				{Type: llm.StreamEventDone, FinishReason: "max_tokens", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}},
			},
			// 4th call would be the 4th max_tokens → stopped at max 3
		},
	}

	a := newTestAgent(t, mp)
	ch := a.RunConversationStream(t.Context(), nil, "long response", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonLengthExhausted, result.ExitReason)
	assert.Contains(t, result.Error.Error(), "truncated after 3 continuation")
	// Partial response should be preserved — the last iteration's text
	// should be delivered to the caller rather than lost.
	assert.Equal(t, "final", result.Response)
}

func TestAgentLoop_MaxTokensThenStop(t *testing.T) {
	// max_tokens followed by stop — should complete normally.
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			{
				{Type: llm.StreamEventTextDelta, TextDelta: "part "},
				{Type: llm.StreamEventDone, FinishReason: "max_tokens", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}},
			},
			textSeq("two"),
		},
	}

	a := newTestAgent(t, mp)
	ch := a.RunConversationStream(t.Context(), nil, "long", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
	// The final text should be from the stop turn
	assert.Equal(t, "two", result.Response)
}

// ---- Tests: handleLengthFinish continuation prompts ----

// TestHandleLengthFinish_ContinuationPrompt covers the three continuation
// prompt branches: interrupted tool call, thinking-only output, and plain
// text. The prompt must match what was truncated so the model knows how to
// recover.
func TestHandleLengthFinish_ContinuationPrompt(t *testing.T) {
	cases := []struct {
		name         string
		buildAcc     func() *streamAccumulator
		wantContains string
	}{
		{
			name: "interrupted tool call asks for retry",
			buildAcc: func() *streamAccumulator {
				acc := &streamAccumulator{finishReason: "max_tokens"}
				acc.toolCalls = []llm.ToolCall{
					{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command":"ls"}`}},
				}
				return acc
			},
			wantContains: "retry the tool call",
		},
		{
			name: "thinking-only output asks to continue response",
			buildAcc: func() *streamAccumulator {
				acc := &streamAccumulator{finishReason: "max_tokens"}
				acc.thinkBlocks = []llm.ThinkingBlock{{Type: "thinking", Thinking: "deep thought"}}
				return acc
			},
			wantContains: "Please continue with your response",
		},
		{
			name: "plain text asks to continue where left off",
			buildAcc: func() *streamAccumulator {
				acc := &streamAccumulator{finishReason: "max_tokens"}
				acc.text.WriteString("partial answer")
				return acc
			},
			wantContains: "Please continue where you left off",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAgent(t, nil)
			rs := &RunState{
				Messages: []llm.Message{},
				Budget:   NewIterationBudget(0),
			}
			ch := make(chan AgentEvent, 8)
			defer close(ch)

			acc := tc.buildAcc()
			outcome := a.handleLengthFinish(t.Context(), acc, rs, ch)

			assert.Equal(t, outcomeContinue, outcome)
			assert.Equal(t, 1, rs.LengthRetries)

			// Last message must be the continuation prompt.
			require.NotEmpty(t, rs.Messages)
			last := rs.Messages[len(rs.Messages)-1]
			assert.Equal(t, "user", last.Role)
			assert.Contains(t, last.Content, tc.wantContains)
		})
	}
}

// TestHandleLengthFinish_DropsTruncatedToolCalls verifies that tool calls
// truncated by the output limit are stripped from the assistant message —
// the API protocol requires every tool_use to pair with a tool_result,
// which un-executed calls cannot satisfy.
func TestHandleLengthFinish_DropsTruncatedToolCalls(t *testing.T) {
	a := newTestAgent(t, nil)
	rs := &RunState{
		Messages: []llm.Message{},
		Budget:   NewIterationBudget(0),
	}
	ch := make(chan AgentEvent, 8)
	defer close(ch)

	acc := &streamAccumulator{finishReason: "max_tokens"}
	acc.toolCalls = []llm.ToolCall{
		{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command":"ls"}`}},
	}

	outcome := a.handleLengthFinish(t.Context(), acc, rs, ch)
	assert.Equal(t, outcomeContinue, outcome)

	// First appended message is the assistant turn — tool calls stripped.
	require.GreaterOrEqual(t, len(rs.Messages), 2)
	assistantMsg := rs.Messages[0]
	assert.Equal(t, "assistant", assistantMsg.Role)
	assert.Nil(t, assistantMsg.ToolCalls)
}

// TestHandleLengthFinish_Exhausted verifies the loop stops after
// maxLengthContinueRetries and delivers the partial output.
func TestHandleLengthFinish_Exhausted(t *testing.T) {
	a := newTestAgent(t, nil)
	rs := &RunState{
		Messages:      []llm.Message{},
		Budget:        NewIterationBudget(0),
		LengthRetries: maxLengthContinueRetries - 1, // next one exhausts
	}
	ch := make(chan AgentEvent, 8)
	defer close(ch)

	acc := &streamAccumulator{finishReason: "max_tokens"}
	acc.text.WriteString("final chunk")

	outcome := a.handleLengthFinish(t.Context(), acc, rs, ch)
	assert.Equal(t, outcomeStop, outcome)

	// A TurnComplete event with length_exhausted must have been emitted.
	var result *RunResult
	for len(ch) > 0 {
		e := <-ch
		if e.Type == AgentEventTurnComplete {
			result = e.Result
		}
	}
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonLengthExhausted, result.ExitReason)
	assert.Equal(t, "final chunk", result.Response)
}

// ---- Tests: ConfirmationTool integration via agent loop ----

func TestAgentLoop_ConfirmationToolApproved(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("EditFile", "call-1", `{"path":"/tmp/f","old_string":"a","new_string":"b"}`),
			textSeq("File edited"),
		},
	}

	a := newTestAgent(t, mp)
	a.SetPermissionMode(PermissionModeTUI) // require confirmation
	a.RegisterTool(confirmStub())

	ch := a.RunConversationStream(t.Context(), nil, "edit file", "", llm.ChatOptions{MaxTokens: 4096})

	// Consume on a single goroutine — respond inline
	var result *RunResult
	for e := range ch {
		if e.Type == AgentEventToolConfirmation {
			a.ConfirmTool(ConfirmAllowOnce)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}
	require.NotNil(t, result)
	assert.Equal(t, "File edited", result.Response)
}

func TestAgentLoop_ConfirmationToolDenied(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("EditFile", "call-1", `{"path":"/tmp/f","old_string":"a","new_string":"b"}`),
		},
	}

	a := newTestAgent(t, mp)
	a.SetPermissionMode(PermissionModeTUI)
	a.RegisterTool(confirmStub())

	ch := a.RunConversationStream(t.Context(), nil, "edit file", "", llm.ChatOptions{MaxTokens: 4096})

	var result *RunResult
	for e := range ch {
		if e.Type == AgentEventToolConfirmation {
			a.ConfirmTool(ConfirmDeny)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}
	require.NotNil(t, result)
	assert.Equal(t, ExitReasonCancelled, result.ExitReason)
	assert.ErrorIs(t, result.Error, errCancelled)
}

// ---- Tests: Session recording ----

func TestAgentLoop_SessionRecording(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)

	mp := &mockStreamProvider{
		name:      "mock",
		sequences: [][]llm.StreamEvent{textSeq("Hello!")},
	}

	a := newTestAgent(t, mp)
	sm := session.NewManagerWithStore(store, nil)
	a.SetSessionManager(sm)

	ch := a.RunConversationStream(t.Context(), nil, "my message", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "Hello!", result.Response)

	// Verify session was created and messages recorded
	require.True(t, sm.HasCurrent())
	sess := sm.Current()
	require.NotNil(t, sess)
	assert.NotEmpty(t, sess.ID)

	msgs, err := sm.LoadMessages()
	require.NoError(t, err)
	assert.Greater(t, len(msgs), 0)

	// First message should be the user message
	assert.Equal(t, session.MessageTypeUser, msgs[0].Type)
	assert.Equal(t, "my message", msgs[0].Content)
}

func TestAgentLoop_PendingSessionThinkingInherited(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)

	mp := &mockStreamProvider{
		name:      "mock",
		sequences: [][]llm.StreamEvent{textSeq("Hello!")},
	}

	a := newTestAgent(t, mp)
	sm := session.NewManagerWithStore(store, nil)
	a.SetSessionManager(sm)

	// Record a pending per-session thinking override before any session
	// exists — the state a TUI /thinking sets right after startup.
	a.SetPendingSessionThinking("high")

	ch := a.RunConversationStream(t.Context(), nil, "first message", "", llm.ChatOptions{MaxTokens: 4096})
	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)

	// The auto-created session inherits the override and clears the pending.
	require.True(t, sm.HasCurrent())
	sess := sm.Current()
	require.NotNil(t, sess)
	assert.Equal(t, "high", sess.ThinkingLevel)
	assert.Equal(t, "", a.PendingSessionThinking())
}

func TestAgentLoop_NoPendingLeavesSessionDefault(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)

	mp := &mockStreamProvider{
		name:      "mock",
		sequences: [][]llm.StreamEvent{textSeq("Hello!")},
	}

	a := newTestAgent(t, mp)
	sm := session.NewManagerWithStore(store, nil)
	a.SetSessionManager(sm)

	// No pending override set.
	ch := a.RunConversationStream(t.Context(), nil, "first message", "", llm.ChatOptions{MaxTokens: 4096})
	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)

	require.True(t, sm.HasCurrent())
	assert.Equal(t, "", sm.Current().ThinkingLevel)
}

// ---- Tests: AskUserQuestion via agent loop ----

func TestAgentLoop_AskUserQuestionResponded(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq(agenttools.ToolNameAskUser, "call-1", `{"questions":[{"question":"q?","header":"Q","options":[{"label":"A","description":"D"}],"multiSelect":false}]}`),
			textSeq("Got your answer"),
		},
	}

	a := newTestAgent(t, mp)
	a.RegisterTool(askUserStub())

	ch := a.RunConversationStream(t.Context(), nil, "ask me", "", llm.ChatOptions{MaxTokens: 4096})

	var result *RunResult
	for e := range ch {
		if e.Type == AgentEventAskUser {
			// Respond immediately on same goroutine
			a.RespondToAskUser(map[string]string{"questions": "A"}, nil)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}
	require.NotNil(t, result)
	assert.Equal(t, "Got your answer", result.Response)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
}

// EditFile never requires confirmation (design decision) — the confirmation
// flow remains for other confirmation-gated tools (e.g. bash policy asks).
// Bash policy asks must still prompt in TUI mode.
func TestAgentLoop_BashAskConfirmsInTUI(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"git push origin main"}`),
			textSeq("done"),
		},
	}

	a := newTestAgent(t, mp)
	a.SetPermissionMode(PermissionModeTUI)
	a.RegisterTool(bashStub())
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))

	ch := a.RunConversationStream(t.Context(), nil, "push", "", llm.ChatOptions{MaxTokens: 4096})
	var result *RunResult
	confirms := 0
	for e := range ch {
		if e.Type == AgentEventToolConfirmation {
			confirms++
			a.ConfirmTool(ConfirmAllowOnce)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}

	require.NotNil(t, result)
	assert.Equal(t, "done", result.Response)
	assert.Equal(t, 1, confirms, "bash ask must still prompt in TUI mode")
}

// ---- Tests: Context cancellation at various agent-loop phases ----

// cancelAfterStreamProvider sends a stream event immediately, then blocks
// until context cancellation where it sends a StreamEventError. This lets
// the test verify that the AgentEventError from consumeStream's error path
// carries the accumulated ls.messages.
type cancelAfterStreamProvider struct {
	name string
}

func (p *cancelAfterStreamProvider) Name() string         { return p.name }
func (p *cancelAfterStreamProvider) ProviderName() string { return "" }
func (p *cancelAfterStreamProvider) Model() string        { return "mock-model" }
func (p *cancelAfterStreamProvider) CreateChat(ctx context.Context, _ []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (*llm.Response, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *cancelAfterStreamProvider) CreateChatStream(ctx context.Context, _ []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 3)
	go func() {
		defer close(ch)
		// Send an initial text event so the agent starts consuming.
		ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "partial "}
		// Block until context cancellation, then signal stream error.
		<-ctx.Done()
		ch <- llm.StreamEvent{Type: llm.StreamEventError, Error: ctx.Err()}
	}()
	return ch, nil
}

func TestAgentLoop_StreamCancelledDuringConsume_MessagesPreserved(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	a := newTestAgent(t, &cancelAfterStreamProvider{name: "slow"})

	// Cancel context after the agent has started consuming the stream.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	ch := a.RunConversationStream(ctx, nil, "hi", "", llm.ChatOptions{MaxTokens: 4096})
	result, events := drainAgentEvents(ch)

	require.NotNil(t, result)
	assert.Equal(t, ExitReasonInterrupted, result.ExitReason)
	assert.ErrorIs(t, result.Error, context.Canceled)

	// Verify the AgentEventError carries the conversation history (system + user).
	var found bool
	for _, e := range events {
		if e.Type == AgentEventError {
			found = true
			assert.NotEmpty(t, e.Messages, "AgentEventError from consumeStream cancel must carry ls.messages")
			assert.GreaterOrEqual(t, len(e.Messages), 1, "should have at least the user message")
			break
		}
	}
	assert.True(t, found, "AgentEventError must be emitted")
}

func TestAgentLoop_SteerCancelled_MessagesPreserved(t *testing.T) {
	// Provider returns a single tool call so the agent reaches the steer point.
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"echo hi"}`),
		},
	}

	ctx, cancel := context.WithCancel(t.Context())

	a := newTestAgent(t, mp)
	a.RegisterTool(echoStub())
	// Enable steer via RunOption.
	steerCh := make(chan SteerInput)

	// Cancel context after the agent reaches the steer point (tool executes
	// synchronously, so this happens almost immediately after the API call).
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	ch := a.RunConversationStream(ctx, nil, "run command", "", llm.ChatOptions{MaxTokens: 4096}, WithSteerChannel(steerCh))
	result, events := drainAgentEvents(ch)

	require.NotNil(t, result)
	assert.Equal(t, ExitReasonInterrupted, result.ExitReason)

	// Verify an AgentEventError was emitted with Messages (our fix for the
	// applySteer ctx.Done() path — message context is carried in rs.Messages).
	var found bool
	for _, e := range events {
		if e.Type == AgentEventError {
			found = true
			assert.NotEmpty(t, e.Messages, "AgentEventError from steer cancel must carry rs.Messages")
			// Messages must include at least the user message + tool result.
			// Previously loopState carried them; now RunState.Messages does.
			break
		}
	}
	assert.True(t, found, "AgentEventError must be emitted at steer point cancel (not silent exit)")
}

// TestAgentLoop_SteerTimeoutContinues verifies that when the frontend never
// answers a SteerCheck (e.g. a steer-channel mismatch after a concurrent turn
// rebuilt the channel), the loop does not hang forever: after the steer
// timeout it continues without steer and completes normally.
func TestAgentLoop_SteerTimeoutContinues(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"echo hi"}`),
			textSeq("done after steer timeout"),
		},
	}

	a := newTestAgent(t, mp)
	a.RegisterTool(echoStub())

	// Steer is enabled, but nobody ever writes to the channel — this simulates
	// a desynced/stuck frontend that leaves the loop waiting at the steer point.
	steerCh := make(chan SteerInput)

	start := time.Now()
	ch := a.RunConversationStream(t.Context(), nil, "run command", "", llm.ChatOptions{MaxTokens: 4096},
		WithSteerChannel(steerCh), WithSteerTimeout(100*time.Millisecond))
	result, events := drainAgentEvents(ch)

	require.NotNil(t, result)
	assert.Equal(t, ExitReasonStop, result.ExitReason)
	// Must not block for the full 5s default timeout — the short configured
	// timeout must have kicked in at the steer point.
	assert.Less(t, time.Since(start), 5*time.Second, "loop must continue past the steer point without a response")

	// The frontend still got its SteerCheck chance before the timeout.
	var sawCheck bool
	for _, e := range events {
		if e.Type == AgentEventSteerCheck {
			sawCheck = true
			break
		}
	}
	assert.True(t, sawCheck, "SteerCheck event must be emitted before timing out")
}

// ---- maybeAutoCompact tests (via CompactStrategy interface) ----

func TestMaybeAutoCompact_Success(t *testing.T) {
	trueVal := true
	a := newTestAgent(t, nil,
		withFakeSession(),
		withCompactStrategy(&fakeCompactStrategy{summary: "compacted summary"}),
	)
	a.Config.FullConfig = &config.Config{
		Compact: config.CompactConfig{
			Auto:      &trueVal,
			Threshold: 0.5,
			MaxTokens: 1024,
			Timeout:   time.Minute,
		},
	}
	a.Config.ContextWindow = 1000
	a.conv.setEstimate(600, tokenbreakdown.Breakdown{}) // 60% > 50% threshold

	rs := &RunState{
		Messages: []llm.Message{
			{Role: "system", Content: "You are Tachi."},
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
		},
		Budget: NewIterationBudget(0),
	}

	_, err := a.Config.SessionManager.New("test", "/tmp")
	require.NoError(t, err)

	ch := make(chan AgentEvent, 10)
	ctx := context.Background()

	_, compacted := a.maybeAutoCompact(ctx, rs, &llm.ChatOptions{}, ch)
	close(ch)

	assert.True(t, compacted, "should compact when estimate exceeds threshold")

	// Messages should be replaced with compacted history (3 messages: system, summary, continue)
	require.Len(t, rs.Messages, 3)
	assert.Equal(t, "system", rs.Messages[0].Role)
	assert.Contains(t, rs.Messages[1].Content, "compacted summary")
	assert.Equal(t, "user", rs.Messages[2].Role)

	// Verify auto-compact event emitted
	var compactDone bool
	for e := range ch {
		if e.Type == AgentEventAutoCompactDone {
			compactDone = true
			assert.Equal(t, "compacted summary", e.CompactSummary)
		}
	}
	assert.True(t, compactDone, "AutoCompactDone event must be emitted")
}

func TestMaybeAutoCompact_StrategyError(t *testing.T) {
	trueVal := true
	a := newTestAgent(t, nil,
		withFakeSession(),
		withCompactStrategy(&fakeCompactStrategy{err: fmt.Errorf("LLM unavailable")}),
	)
	a.Config.FullConfig = &config.Config{
		Compact: config.CompactConfig{
			Auto:      &trueVal,
			Threshold: 0.5,
			MaxTokens: 1024,
			Timeout:   time.Minute,
		},
	}
	a.Config.ContextWindow = 1000
	a.conv.setEstimate(600, tokenbreakdown.Breakdown{})

	rs := &RunState{
		Messages: []llm.Message{
			{Role: "system", Content: "You are Tachi."},
			{Role: "user", Content: "hello"},
		},
		Budget: NewIterationBudget(0),
	}

	_, err := a.Config.SessionManager.New("test", "/tmp")
	require.NoError(t, err)

	ch := make(chan AgentEvent, 10)
	ctx := context.Background()

	_, compacted := a.maybeAutoCompact(ctx, rs, &llm.ChatOptions{}, ch)
	close(ch)

	// Error does NOT stop the loop — compacted=true means the iteration was
	// consumed (not proceeding to LLM) and the loop continues.
	assert.True(t, compacted, "should return true even on error")

	// Original messages preserved
	assert.Len(t, rs.Messages, 2)

	// Error event emitted
	var compactErr bool
	for e := range ch {
		if e.Type == AgentEventAutoCompactDone {
			compactErr = e.Result != nil && e.Result.Error != nil
		}
	}
	assert.True(t, compactErr, "error should be reported in AutoCompactDone event")
}

func TestMaybeAutoCompact_BelowThreshold(t *testing.T) {
	trueVal := true
	a := newTestAgent(t, nil, withFakeSession())
	a.Config.FullConfig = &config.Config{
		Compact: config.CompactConfig{
			Auto:      &trueVal,
			Threshold: 0.8,
		},
	}
	a.Config.ContextWindow = 1000
	a.conv.setEstimate(100, tokenbreakdown.Breakdown{}) // 10% < 80% threshold

	rs := &RunState{
		Messages: []llm.Message{{Role: "user", Content: "hello"}},
		Budget:   NewIterationBudget(0),
	}

	ch := make(chan AgentEvent, 10)
	ctx := context.Background()

	_, compacted := a.maybeAutoCompact(ctx, rs, &llm.ChatOptions{}, ch)

	assert.False(t, compacted, "should not compact when estimate is below threshold")
	assert.Len(t, rs.Messages, 1, "messages must not be replaced")
}

// captureStreamHooks installs a hook dispatcher recording stream_start and
// tool_call in arrival order, mirroring how Herdr consumes the events.
func captureStreamHooks(a *AIAgent) *[]string {
	var mu sync.Mutex
	got := make([]string, 0, 8)
	d := hooks.NewDispatcher(nil)
	d.RegisterCallback(hooks.EventStreamStart, "test", func(_ context.Context, _ string, _ []byte) {
		mu.Lock()
		got = append(got, "stream_start")
		mu.Unlock()
	})
	d.RegisterCallback(hooks.EventToolCall, "test", func(_ context.Context, _ string, _ []byte) {
		mu.Lock()
		got = append(got, "tool_call")
		mu.Unlock()
	})
	a.Config.HookDispatcher = d
	return &got
}

func TestAgentLoop_StreamStartFiresOnThinking(t *testing.T) {
	// The exact complaint: herdr stays idle while thinking streams, only
	// flipping to working on the later tool_call. stream_start must fire as
	// soon as the thinking delta arrives.
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			{
				{Type: llm.StreamEventThinkingDelta, ThinkingDelta: "let me think"},
				{Type: llm.StreamEventTextDelta, TextDelta: "hello"},
				{Type: llm.StreamEventDone, FinishReason: "stop"},
			},
		},
	}

	a := newTestAgent(t, mp)
	got := captureStreamHooks(a)

	_, events := drainAgentEvents(
		a.RunConversationStream(t.Context(), nil, "hi", "", llm.ChatOptions{MaxTokens: 4096}))

	require.NotEmpty(t, events, "turn should complete normally")
	assert.Equal(t, []string{"stream_start"}, *got,
		"stream_start should fire exactly once, on the first thinking delta")
}

func TestAgentLoop_StreamStartPrecedesToolCall(t *testing.T) {
	// Thinking → tool_call → final text: the working state must be reported
	// during thinking, before the tool executes.
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			{
				{Type: llm.StreamEventThinkingDelta, ThinkingDelta: "hmm"},
				{Type: llm.StreamEventToolUseStart, ToolIndex: 0, ToolCall: &llm.ToolCall{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "Bash"}}},
				{Type: llm.StreamEventInputJSONDelta, ToolIndex: 0, InputDelta: `{"command":"echo hi"}`},
				{Type: llm.StreamEventDone, FinishReason: "tool_calls"},
			},
			textSeq("done"),
		},
	}

	a := newTestAgent(t, mp)
	a.RegisterTool(bashStub())
	got := captureStreamHooks(a)

	_, events := drainAgentEvents(
		a.RunConversationStream(t.Context(), nil, "run it", "", llm.ChatOptions{MaxTokens: 4096}))

	require.NotEmpty(t, events)
	assert.Equal(t, []string{"stream_start", "tool_call", "stream_start"}, *got,
		"stream_start fires during thinking (before tool_call), and again on the second API call")
}
