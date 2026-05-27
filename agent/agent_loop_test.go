package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Mock Provider ----

// mockStreamProvider implements llm.Provider and lets tests control the
// exact stream events produced, including multi-turn sequences.
type mockStreamProvider struct {
	name      string
	sequences [][]llm.StreamEvent // each entry is one API call's full stream
	callIdx   int
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

	a := newTestAgent(mp)
	a.maxIterations = 2 // only 2 iterations allowed
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

	a := newTestAgent(mp)
	result := a.RunConversation(t.Context(), "hello", "", llm.ChatOptions{MaxTokens: 4096})

	require.NotNil(t, result)
	assert.Equal(t, "Response", result.Response)
	assert.Equal(t, "stop", result.ExitReason)
}

func TestRunConversation_AutoConfirmsTool(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"cmd"}`),
			textSeq("done"),
		},
	}

	a := newTestAgent(mp)
	a.RegisterTool(echoStub())
	result := a.RunConversation(t.Context(), "run a command", "", llm.ChatOptions{MaxTokens: 4096})

	require.NotNil(t, result)
	assert.Equal(t, "done", result.Response)
}

// ---- Tests: executeToolCalls integration ----

func TestExecuteToolCalls_UnknownTool(t *testing.T) {
	a := newTestAgent(nil)

	toolCalls := []llm.ToolCall{
		{ID: "call-1", Type: "function", Function: llm.ToolCallFunction{Name: "NonExistent", Arguments: "{}"}},
	}

	ch := make(chan AgentEvent, 8)
	msgs, err := a.executeToolCalls(t.Context(), toolCalls, ch)
	require.NoError(t, err) // unknown tool does not stop the loop, it returns an error result
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].IsError)
	assert.Contains(t, msgs[0].Content, "unknown tool")
	close(ch)
}

func TestExecuteToolCalls_ToolError(t *testing.T) {
	a := newTestAgent(nil)
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
	msgs, err := a.executeToolCalls(t.Context(), toolCalls, ch)
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

	a := newTestAgent(mp)
	ch := a.RunConversationStream(t.Context(), nil, "long response", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "length_exhausted", result.ExitReason)
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

	a := newTestAgent(mp)
	ch := a.RunConversationStream(t.Context(), nil, "long", "", llm.ChatOptions{MaxTokens: 4096})

	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	assert.Equal(t, "stop", result.ExitReason)
	// The final text should be from the stop turn
	assert.Equal(t, "two", result.Response)
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

	a := newTestAgent(mp)
	a.SetSkipEditConfirm(false) // require confirmation
	a.RegisterTool(confirmStub())

	ch := a.RunConversationStream(t.Context(), nil, "edit file", "", llm.ChatOptions{MaxTokens: 4096})

	// Consume on a single goroutine — respond inline
	var result *RunResult
	for e := range ch {
		if e.Type == AgentEventToolConfirmation {
			a.ConfirmTool(true)
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

	a := newTestAgent(mp)
	a.SetSkipEditConfirm(false)
	a.RegisterTool(confirmStub())

	ch := a.RunConversationStream(t.Context(), nil, "edit file", "", llm.ChatOptions{MaxTokens: 4096})

	var result *RunResult
	for e := range ch {
		if e.Type == AgentEventToolConfirmation {
			a.ConfirmTool(false)
		}
		if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
			result = e.Result
		}
	}
	require.NotNil(t, result)
	assert.Equal(t, "cancelled", result.ExitReason)
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

	a := newTestAgent(mp)
	sm := session.NewManagerWithStore(store)
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

// ---- Tests: AskUserQuestion via agent loop ----

func TestAgentLoop_AskUserQuestionResponded(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq(agenttools.ToolNameAskUser, "call-1", `{"questions":[{"question":"q?","header":"Q","options":[{"label":"A","description":"D"}],"multiSelect":false}]}`),
			textSeq("Got your answer"),
		},
	}

	a := newTestAgent(mp)
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
	assert.Equal(t, "stop", result.ExitReason)
}
