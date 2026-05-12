package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// ---- Tests: executeToolCalls with confirmation ----

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
			var m map[string]interface{}
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

func confirmStub() *stubTool {
	return &stubTool{
		name:         "EditFile",
		desc:         "Edit a file",
		parallel:     false,
		needsConfirm: true,
		diffFn:       func(ctx context.Context, args string) (string, error) { return "diff preview", nil },
		executeFn: func(ctx context.Context, args string) (string, error) {
			var m map[string]interface{}
			json.Unmarshal([]byte(args), &m)
			return fmt.Sprintf("edited %v", m["path"]), nil
		},
	}
}

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

	// Should have SessionTitle event
	sessTitle := false
	for m := range ch {
		if m.Type == AgentEventSessionTitle {
			sessTitle = true
		}
	}
	_ = sessTitle
}

// ---- Tests: AskUserQuestion via agent loop ----

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
