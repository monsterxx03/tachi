package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// testConfig provides a minimal provider config for stream tests that need
// ProviderType() to resolve from provider name.
var testConfig = &config.Config{
	Providers: []config.ProviderConfig{
		{Name: "openai", Type: "openai", Model: "gpt-4o-mini"},
	},
}

func TestMapToolKind(t *testing.T) {
	assert.Equal(t, acp.ToolKindRead, mapToolKind("ReadFile"))
	assert.Equal(t, acp.ToolKindEdit, mapToolKind("WriteFile"))
	assert.Equal(t, acp.ToolKindEdit, mapToolKind("EditFile"))
	assert.Equal(t, acp.ToolKindExecute, mapToolKind("Bash"))
	assert.Equal(t, acp.ToolKindSearch, mapToolKind("Glob"))
	assert.Equal(t, acp.ToolKindSearch, mapToolKind("Grep"))
	assert.Equal(t, acp.ToolKindFetch, mapToolKind("WebSearch"))
	assert.Equal(t, acp.ToolKindFetch, mapToolKind("WebFetch"))
	assert.Equal(t, acp.ToolKind(""), mapToolKind("UnknownTool"))
}

func TestMapStopReason(t *testing.T) {
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("stop"))
	assert.Equal(t, acp.StopReasonCancelled, mapStopReason("cancelled"))
	assert.Equal(t, acp.StopReasonCancelled, mapStopReason("interrupted"))
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("error"))
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("budget_exhausted"))
}

func TestParseRawInput(t *testing.T) {
	// Valid JSON returns parsed map.
	result := parseRawInput(`{"pattern": "**/*.go", "path": "/tmp"}`)
	require.NotNil(t, result)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "**/*.go", m["pattern"])
	assert.Equal(t, "/tmp", m["path"])

	// Invalid JSON returns nil.
	assert.Nil(t, parseRawInput(`{bad json`))

	// Empty JSON object returns nil (nothing useful to send).
	assert.Nil(t, parseRawInput(`{}`))

	// Empty string returns nil.
	assert.Nil(t, parseRawInput(``))
}

// ── replaySessionHistory tests ──────────────────────────────────────────────

// mockACPConn sets up an acp.AgentSideConnection backed by pipes for
// capturing session/update notifications. Returns the connection, the write-end
// that must be closed after replay, and a channel that delivers all captured
// JSON-RPC notifications.
func mockACPConn(t *testing.T) (*acp.AgentSideConnection, *io.PipeWriter, <-chan []map[string]any) {
	t.Helper()

	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()
	t.Cleanup(func() {
		agentToClientR.Close()
		agentToClientW.Close()
		clientToAgentR.Close()
		clientToAgentW.Close()
	})

	conn := acp.NewAgentSideConnection(&mockACPAgent{}, agentToClientW, clientToAgentR)

	results := make(chan []map[string]any, 1)
	go func() {
		var notifications []map[string]any
		decoder := json.NewDecoder(agentToClientR)
		for {
			var msg map[string]any
			if err := decoder.Decode(&msg); err != nil {
				break
			}
			notifications = append(notifications, msg)
		}
		results <- notifications
	}()

	return conn, agentToClientW, results
}

// drainNotifications closes the writer and waits for all captured notifications.
func drainNotifications(w *io.PipeWriter, ch <-chan []map[string]any) []map[string]any {
	w.Close()
	return <-ch
}

// setupSessionWithMessages creates a session.Manager backed by a temp dir,
// creates a session, appends the given messages, and returns the manager
// along with an ACPSession wired to it.
func setupSessionWithMessages(t *testing.T, cwd string, msgs []session.Message) (*session.Manager, *ACPSession) {
	t.Helper()

	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	sess, err := sm.New("openai", cwd)
	require.NoError(t, err)

	for i := range msgs {
		require.NoError(t, sm.AppendMessage(&msgs[i]))
	}

	// LoadMessages uses sm.current — already set by New
	acpSess := &ACPSession{
		ID:      sess.ID,
		cwd:     cwd,
		cfg:     testConfig,
		sessMgr: sm,
	}

	return sm, acpSess
}

func TestReplaySessionHistory_NilConn(t *testing.T) {
	// Must not panic
	replaySessionHistory(t.Context(), nil, &ACPSession{})
}

func TestReplaySessionHistory_NilSessMgr(t *testing.T) {
	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, &ACPSession{sessMgr: nil})
	notifications := drainNotifications(w, ch)
	assert.Empty(t, notifications, "no notifications when sessMgr is nil")
}

func TestReplaySessionHistory_EmptyMessages(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	sess, err := sm.New("openai", "/empty")
	require.NoError(t, err)

	conn, w, ch := mockACPConn(t)
	acpSess := &ACPSession{
		ID:      sess.ID,
		cwd:     "/empty",
		cfg:     testConfig,
		sessMgr: sm,
	}

	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)
	assert.Empty(t, notifications, "no messages → no notifications")
}

func TestReplaySessionHistory_ReplaysUserMessage(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{Type: session.MessageTypeUser, Content: "Hello, world!"},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 1)
	verifyNotification(t, notifications[0], acpSess.ID, "user_message_chunk")
}

func TestReplaySessionHistory_ReplaysAssistantMessage(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{Type: session.MessageTypeAssistant, Content: "I can help with that."},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 1)
	verifyNotification(t, notifications[0], acpSess.ID, "agent_message_chunk")
	assert.Contains(t, extractUpdateContent(notifications[0], "content", "text"), "I can help with that.")
}

func TestReplaySessionHistory_ReplaysThinkingMessage(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{Type: session.MessageTypeThinking, Content: "Let me think about this..."},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 1)
	verifyNotification(t, notifications[0], acpSess.ID, "agent_thought_chunk")
	assert.Contains(t, extractUpdateContent(notifications[0], "content", "text"), "Let me think about this...")
}

func TestReplaySessionHistory_ReplaysToolCall(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{
			Type:       session.MessageTypeToolCall,
			Name:       tools.ToolNameRead,
			ToolCallID: "call_001",
		},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 1)
	verifyNotification(t, notifications[0], acpSess.ID, "tool_call")

	update := notifications[0]["params"].(map[string]any)["update"].(map[string]any)
	assert.Equal(t, "call_001", update["toolCallId"])
	assert.Equal(t, string(acp.ToolKindRead), update["kind"])
	assert.Equal(t, string(acp.ToolCallStatusInProgress), update["status"])
	assert.Equal(t, tools.ToolNameRead, update["title"])
}

func TestReplaySessionHistory_ReplaysToolCall_WithArgs(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{
			Type:       session.MessageTypeToolCall,
			Name:       tools.ToolNameGlob,
			ToolCallID: "call_args",
			Args:       map[string]any{"pattern": "**/*.go"},
		},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 1)
	verifyNotification(t, notifications[0], acpSess.ID, "tool_call")

	update := notifications[0]["params"].(map[string]any)["update"].(map[string]any)
	assert.Equal(t, "call_args", update["toolCallId"])
	assert.Equal(t, "Find `**/*.go`", update["title"])

	// Verify rawInput is present with the tool arguments.
	rawInput, ok := update["rawInput"].(map[string]any)
	require.True(t, ok, "rawInput should be present")
	assert.Equal(t, "**/*.go", rawInput["pattern"])
}

func TestBuildToolTitle(t *testing.T) {
	tests := []struct {
		name     string
		argsJSON string
		want     string
	}{
		{tools.ToolNameRead, `{"path": "/tmp/foo.go"}`, "Read foo.go"},
		{tools.ToolNameRead, `{"path": "/tmp/foo.go", "offset": 10}`, "Read foo.go"},
		{tools.ToolNameWrite, `{"path": "/tmp/bar.go"}`, "Write bar.go"},
		{tools.ToolNameEdit, `{"path": "/tmp/baz.go"}`, "Edit baz.go"},
		{tools.ToolNameBash, `{"command": "ls -la"}`, "Run `ls -la`"},
		{tools.ToolNameGlob, `{"pattern": "**/*.go"}`, "Find `**/*.go`"},
		{tools.ToolNameGrep, `{"pattern": "TODO"}`, "Search `TODO`"},
		{tools.ToolNameWebSearch, `{"query": "golang generics"}`, "Search golang generics"},
		{tools.ToolNameWebFetch, `{"url": "https://example.com"}`, "Fetch https://example.com"},
		{tools.ToolNameLSP, `{"operation": "goToDefinition", "path": "/tmp/x.go"}`, "LSP goToDefinition x.go"},
		{tools.ToolNameSubAgent, `{"prompt": "analyze this code"}`, "SubAgent: analyze this code"},
		// No args: returns tool name.
		{tools.ToolNameBash, ``, tools.ToolNameBash},
		{tools.ToolNameBash, `{}`, tools.ToolNameBash},
		{tools.ToolNameBash, `{invalid`, tools.ToolNameBash},
		// Unknown tool: returns tool name.
		{"UnknownTool", `{"x": 1}`, "UnknownTool"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"_"+tt.argsJSON, func(t *testing.T) {
			got := buildToolTitle(tt.name, tt.argsJSON)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReplaySessionHistory_ReplaysToolCall_Kinds(t *testing.T) {
	tests := []struct {
		toolName string
		kind     string
	}{
		{tools.ToolNameRead, string(acp.ToolKindRead)},
		{tools.ToolNameWrite, string(acp.ToolKindEdit)},
		{tools.ToolNameEdit, string(acp.ToolKindEdit)},
		{tools.ToolNameBash, string(acp.ToolKindExecute)},
		{tools.ToolNameGlob, string(acp.ToolKindSearch)},
		{tools.ToolNameGrep, string(acp.ToolKindSearch)},
		{tools.ToolNameWebSearch, string(acp.ToolKindFetch)},
		{tools.ToolNameWebFetch, string(acp.ToolKindFetch)},
		{"SomeUnknownTool", ""},
	}

	for _, tt := range tests {
		t.Run(tt.toolName, func(t *testing.T) {
			_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
				{
					Type:       session.MessageTypeToolCall,
					Name:       tt.toolName,
					ToolCallID: "call_kind",
				},
			})

			conn, w, ch := mockACPConn(t)
			replaySessionHistory(t.Context(), conn, acpSess)
			notifications := drainNotifications(w, ch)

			require.Len(t, notifications, 1)
			update := notifications[0]["params"].(map[string]any)["update"].(map[string]any)
			// When kind is empty string, JSON omits the field (omitempty)
			if tt.kind == "" {
				assert.Nil(t, update["kind"], "unknown tool should omit kind field")
			} else {
				assert.Equal(t, tt.kind, update["kind"])
			}
		})
	}
}

func TestReplaySessionHistory_ReplaysToolResult_Success(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{
			Type:       session.MessageTypeToolResult,
			ToolCallID: "call_002",
			Result:     "output here",
			IsError:    false,
		},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 1)
	verifyNotification(t, notifications[0], acpSess.ID, "tool_call_update")

	update := notifications[0]["params"].(map[string]any)["update"].(map[string]any)
	assert.Equal(t, "call_002", update["toolCallId"])
	assert.Equal(t, string(acp.ToolCallStatusCompleted), update["status"])
}

func TestReplaySessionHistory_ReplaysToolResult_Error(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{
			Type:       session.MessageTypeToolResult,
			ToolCallID: "call_err",
			Result:     "command failed",
			IsError:    true,
		},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 1)
	verifyNotification(t, notifications[0], acpSess.ID, "tool_call_update")

	update := notifications[0]["params"].(map[string]any)["update"].(map[string]any)
	assert.Equal(t, "call_err", update["toolCallId"])
	assert.Equal(t, string(acp.ToolCallStatusFailed), update["status"])
}

func TestReplaySessionHistory_SkipsConfirm(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{Type: session.MessageTypeUser, Content: "please edit"},
		{Type: session.MessageTypeConfirm, Content: "diff approved"},
		{Type: session.MessageTypeAssistant, Content: "done"},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	// Should have 2 notifications (user + assistant), confirm is skipped
	require.Len(t, notifications, 2)
	verifyNotification(t, notifications[0], acpSess.ID, "user_message_chunk")
	verifyNotification(t, notifications[1], acpSess.ID, "agent_message_chunk")
}

func TestReplaySessionHistory_FullSequence(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{Type: session.MessageTypeUser, Content: "read file x.go"},
		{Type: session.MessageTypeThinking, Content: "I should use ReadFile..."},
		{
			Type:       session.MessageTypeToolCall,
			Name:       tools.ToolNameRead,
			ToolCallID: "call_a",
		},
		{
			Type:       session.MessageTypeToolResult,
			ToolCallID: "call_a",
			Result:     "package main",
			IsError:    false,
		},
		{Type: session.MessageTypeAssistant, Content: "Here's what's in x.go:\n\npackage main"},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 5,
		"expected 5 notifications: user, thinking, tool_call, tool_result, assistant")

	// Verify sequence exactly
	expected := []string{
		"user_message_chunk",
		"agent_thought_chunk",
		"tool_call",
		"tool_call_update",
		"agent_message_chunk",
	}
	for i, exp := range expected {
		verifyNotification(t, notifications[i], acpSess.ID, exp)
	}

	// Verify tool call in position 2
	tcUpdate := notifications[2]["params"].(map[string]any)["update"].(map[string]any)
	assert.Equal(t, "call_a", tcUpdate["toolCallId"])

	// Verify tool result in position 3
	trUpdate := notifications[3]["params"].(map[string]any)["update"].(map[string]any)
	assert.Equal(t, "call_a", trUpdate["toolCallId"])
	assert.Equal(t, string(acp.ToolCallStatusCompleted), trUpdate["status"])
}

func TestReplaySessionHistory_CachesHistory(t *testing.T) {
	// Replaying should populate acpSess.history with converted LLM messages
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	sess, err := sm.New("openai", "/cache-test")
	require.NoError(t, err)

	require.NoError(t, sm.AppendMessage(&session.Message{
		Type:    session.MessageTypeUser,
		Content: "hi",
	}))
	require.NoError(t, sm.AppendMessage(&session.Message{
		Type:    session.MessageTypeAssistant,
		Content: "hello there",
	}))

	conn, w, ch := mockACPConn(t)
	acpSess := &ACPSession{
		ID:      sess.ID,
		cwd:     "/cache-test",
		cfg:     testConfig,
		sessMgr: sm,
	}

	assert.Nil(t, acpSess.history, "history should start nil")

	replaySessionHistory(t.Context(), conn, acpSess)
	drainNotifications(w, ch)

	require.NotNil(t, acpSess.history, "history should be populated after replay")
	require.Len(t, acpSess.history, 2, "should have 2 messages: user + assistant")

	assert.Equal(t, "user", acpSess.history[0].Role)
	assert.Equal(t, "hi", acpSess.history[0].Content)

	assert.Equal(t, "assistant", acpSess.history[1].Role)
	assert.Equal(t, "hello there", acpSess.history[1].Content)
}

func TestReplaySessionHistory_CachesHistory_WithTools(t *testing.T) {
	// Verify history is cached for sequences with tool calls
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	sess, err := sm.New("openai", "/cache-tools")
	require.NoError(t, err)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "list files"},
		{Type: session.MessageTypeThinking, Content: "I'll use Glob..."},
		{
			Type:       session.MessageTypeToolCall,
			Name:       tools.ToolNameGlob,
			ToolCallID: "call_gl",
			Args:       map[string]any{"pattern": "**/*.go"},
		},
		{
			Type:       session.MessageTypeToolResult,
			ToolCallID: "call_gl",
			Result:     "main.go\npkg/x.go",
			IsError:    false,
		},
		{Type: session.MessageTypeAssistant, Content: "Found 2 files: main.go and pkg/x.go"},
	}
	for i := range msgs {
		require.NoError(t, sm.AppendMessage(&msgs[i]))
	}

	conn, w, ch := mockACPConn(t)
	acpSess := &ACPSession{
		ID:      sess.ID,
		cwd:     "/cache-tools",
		cfg:     testConfig,
		sessMgr: sm,
	}

	replaySessionHistory(t.Context(), conn, acpSess)
	drainNotifications(w, ch)

	require.NotNil(t, acpSess.history, "history should be cached")
	// For OpenAI, ConvertSessionToLLMMessages produces:
	// [0] user, [1] assistant (with thinking prepended + tool_calls),
	// [2] tool (result), [3] assistant (text response)
	require.Len(t, acpSess.history, 4, "user + assistant(with thinking/tool_calls) + tool + assistant(text)")

	// Verify the assistant message contains the tool call
	assert.Equal(t, "assistant", acpSess.history[1].Role)
	require.Len(t, acpSess.history[1].ToolCalls, 1)
	assert.Equal(t, "call_gl", acpSess.history[1].ToolCalls[0].ID)
	assert.Equal(t, tools.ToolNameGlob, acpSess.history[1].ToolCalls[0].Function.Name)

	// Verify the tool result message
	assert.Equal(t, "tool", acpSess.history[2].Role)
	assert.Equal(t, "main.go\npkg/x.go", acpSess.history[2].Content)

	// Verify the final text response
	assert.Equal(t, "assistant", acpSess.history[3].Role)
	assert.Equal(t, "Found 2 files: main.go and pkg/x.go", acpSess.history[3].Content)
}

func TestReplaySessionHistory_CachesHistory_ConversionFailure(t *testing.T) {
	// Even if ConvertSessionToLLMMessages fails, replay should not crash
	// and history should remain nil
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	sess, err := sm.New("openai", "/cache-fail")
	require.NoError(t, err)

	// A tool_call without a corresponding tool_result can cause conversion issues
	// depending on the provider, but for openai it should still work.
	// This test verifies the replay doesn't crash on conversion failure.
	require.NoError(t, sm.AppendMessage(&session.Message{
		Type:    session.MessageTypeUser,
		Content: "hello",
	}))

	conn, w, ch := mockACPConn(t)
	acpSess := &ACPSession{
		ID:      sess.ID,
		cwd:     "/cache-fail",
		cfg:     testConfig,
		sessMgr: sm,
	}

	// Should not panic
	replaySessionHistory(t.Context(), conn, acpSess)
	drainNotifications(w, ch)

	// History should be populated (simple case succeeds)
	assert.NotNil(t, acpSess.history)
}

func TestReplaySessionHistory_ConcurrentSafety(t *testing.T) {
	// Ensure replaySessionHistory doesn't race when called from different goroutines.
	// This is a stress test for the SessionUpdate notification path.
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	sess, err := sm.New("openai", "/concurrent")
	require.NoError(t, err)

	require.NoError(t, sm.AppendMessage(&session.Message{
		Type:    session.MessageTypeUser,
		Content: "concurrent test",
	}))

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			conn, w, ch := mockACPConn(t)

			acpSess := &ACPSession{
				ID:      sess.ID,
				cwd:     "/concurrent",
				cfg:     testConfig,
				sessMgr: sm,
			}

			// Should not panic or race
			replaySessionHistory(t.Context(), conn, acpSess)
			notifications := drainNotifications(w, ch)
			assert.Len(t, notifications, 1)
		})
	}
	wg.Wait()
}

func TestReplaySessionHistory_LoadMessagesError(t *testing.T) {
	// When sessMgr.current is nil, LoadMessages returns an error.
	// replaySessionHistory should handle this gracefully.
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	// Create session but then end it (makes current nil)
	sess, err := sm.New("openai", "/load-err")
	require.NoError(t, err)
	require.NoError(t, sm.AppendMessage(&session.Message{
		Type:    session.MessageTypeUser,
		Content: "test",
	}))
	sm.EndCurrent()

	conn, w, ch := mockACPConn(t)
	acpSess := &ACPSession{
		ID:      sess.ID,
		cwd:     "/load-err",
		cfg:     testConfig,
		sessMgr: sm,
	}

	// Should not panic — LoadMessages will error because current is nil
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)
	assert.Empty(t, notifications, "no notifications when LoadMessages fails")
}

func TestReplaySessionHistory_DoesNotCacheOnConversionFailure(t *testing.T) {
	// Verify that history stays nil if ConvertSessionToLLMMessages fails.
	// This can happen with malformed message sequences.
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	sess, err := sm.New("openai", "/conv-fail")
	require.NoError(t, err)

	// A tool_result without preceding tool_call — this is a valid session
	// message but may cause conversion warnings (should still succeed for OpenAI).
	// We're testing that a regular sequence works.
	require.NoError(t, sm.AppendMessage(&session.Message{
		Type:    session.MessageTypeUser,
		Content: "hi",
	}))
	require.NoError(t, sm.AppendMessage(&session.Message{
		Type:    session.MessageTypeAssistant,
		Content: "hello",
	}))

	conn, w, ch := mockACPConn(t)
	acpSess := &ACPSession{
		ID:      sess.ID,
		cwd:     "/conv-fail",
		cfg:     testConfig,
		sessMgr: sm,
	}

	replaySessionHistory(t.Context(), conn, acpSess)
	drainNotifications(w, ch)

	assert.NotNil(t, acpSess.history, "simple sequence should cache successfully")
}

// ── helpers ─────────────────────────────────────────────────────────────────

// verifyNotification checks that a JSON-RPC notification has the expected
// sessionId and sessionUpdate discriminator.
func verifyNotification(t *testing.T, msg map[string]any, sessionID, updateType string) {
	t.Helper()

	assert.Equal(t, "2.0", msg["jsonrpc"])
	assert.Equal(t, "session/update", msg["method"])

	params, ok := msg["params"].(map[string]any)
	require.True(t, ok, "params should be a map")
	assert.Equal(t, sessionID, params["sessionId"])

	update, ok := params["update"].(map[string]any)
	require.True(t, ok, "update should be a map")
	assert.Equal(t, updateType, update["sessionUpdate"])
}

// extractUpdateContent extracts a nested value from the update using a path
// of map keys. Returns the value as a string via %v formatting.
func extractUpdateContent(msg map[string]any, keys ...string) string {
	params := msg["params"].(map[string]any)
	update := params["update"].(map[string]any)
	cur := update
	for i, key := range keys {
		if i == len(keys)-1 {
			return assertString(cur[key])
		}
		cur = cur[key].(map[string]any)
	}
	return ""
}

func assertString(v any) string {
	s, _ := v.(string)
	return s
}

// TestStreamToACP_ReturnsRunError pins the failure-signal contract: an
// AgentEventError (API error / budget exhaustion) is returned as an error so
// callers like the adversarial review chain can terminate, while the StopReason
// keeps the legacy EndTurn mapping (pinned by TestMapStopReason) — the two
// signals are intentionally independent.
func TestStreamToACP_ReturnsRunError(t *testing.T) {
	sess, conn := newACPReviewSession(t, testConfig, &acpReviewMockProvider{name: "p", model: "m"})

	ch := make(chan agent.AgentEvent, 2)
	ch <- agent.AgentEvent{
		Type: agent.AgentEventError,
		Result: &agent.RunResult{
			Error:      errors.New("rate limit exceeded"),
			ExitReason: agent.ExitReasonError,
		},
	}
	close(ch)

	stopReason, _, err := streamToACP(context.Background(), sess, conn, ch)
	if err == nil {
		t.Fatal("AgentEventError with a payload error must be returned as an error")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("err = %v, want the underlying error", err)
	}
	// Legacy stop-reason mapping preserved — the error is the failure signal.
	if stopReason != acp.StopReasonEndTurn {
		t.Errorf("stopReason = %v, want EndTurn (legacy mapping)", stopReason)
	}
}

// TestStreamToACP_InterruptedHasNoError verifies user cancellation does NOT
// surface as an error — only genuine failures do (so the chain's cancellation
// path stays a clean stop, distinguishable from a broken round).
func TestStreamToACP_InterruptedHasNoError(t *testing.T) {
	sess, conn := newACPReviewSession(t, testConfig, &acpReviewMockProvider{name: "p", model: "m"})

	ch := make(chan agent.AgentEvent, 2)
	ch <- agent.AgentEvent{
		Type: agent.AgentEventError,
		Result: &agent.RunResult{
			ExitReason: agent.ExitReasonInterrupted,
		},
	}
	close(ch)

	stopReason, _, err := streamToACP(context.Background(), sess, conn, ch)
	if err != nil {
		t.Errorf("interruption must not surface as an error, got %v", err)
	}
	if stopReason != acp.StopReasonCancelled {
		t.Errorf("stopReason = %v, want Cancelled (matches mapStopReason)", stopReason)
	}
}

// TestStreamToACP_InterruptedWithCtxErr pins the REAL event shape the agent
// loop produces: terminalError attaches ctx.Err() to an Interrupted exit
// (agent_loop.go), so the event carries a non-nil Error. It must still map to
// a clean cancellation — not to a request-level error (the legacy behavior
// let the buffered-event race decide between clean cancel and hard error).
func TestStreamToACP_InterruptedWithCtxErr(t *testing.T) {
	sess, conn := newACPReviewSession(t, testConfig, &acpReviewMockProvider{name: "p", model: "m"})

	ch := make(chan agent.AgentEvent, 2)
	ch <- agent.AgentEvent{
		Type: agent.AgentEventError,
		Result: &agent.RunResult{
			Error:      context.Canceled, // what terminalError attaches on Interrupted
			ExitReason: agent.ExitReasonInterrupted,
		},
	}
	close(ch)

	stopReason, _, err := streamToACP(context.Background(), sess, conn, ch)
	if err != nil {
		t.Errorf("interruption with ctx.Err() must not surface as an error, got %v", err)
	}
	if stopReason != acp.StopReasonCancelled {
		t.Errorf("stopReason = %v, want Cancelled", stopReason)
	}
}

// ── messageId tests ─────────────────────────────────────────────────────────

// extractUpdateMessageID pulls the messageId field from a session/update
// notification, returning "" when absent (user chunks, tool calls, etc).
func extractUpdateMessageID(msg map[string]any) string {
	params, _ := msg["params"].(map[string]any)
	update, _ := params["update"].(map[string]any)
	id, _ := update["messageId"].(string)
	return id
}

// updateType returns the sessionUpdate discriminator of a notification.
func updateType(msg map[string]any) string {
	params, _ := msg["params"].(map[string]any)
	update, _ := params["update"].(map[string]any)
	t, _ := update["sessionUpdate"].(string)
	return t
}

// messageChunkIDs collects messageIds of all agent_message_chunk /
// agent_thought_chunk notifications in order.
func messageChunkIDs(notifications []map[string]any, chunkType string) []string {
	var ids []string
	for _, n := range notifications {
		if updateType(n) == chunkType {
			ids = append(ids, extractUpdateMessageID(n))
		}
	}
	return ids
}

// TestStreamToACP_MessageID_SharedAcrossTextDeltas pins that streamed text
// deltas of ONE logical message share a single messageId — the client groups
// them into one message.
func TestStreamToACP_MessageID_SharedAcrossTextDeltas(t *testing.T) {
	sess, _ := newACPReviewSession(t, testConfig, &acpReviewMockProvider{name: "p", model: "m"})
	conn, w, ch := mockACPConn(t)

	evCh := make(chan agent.AgentEvent, 4)
	for _, d := range []string{"Hel", "lo, ", "world"} {
		evCh <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: d}
	}
	close(evCh)

	stopReason, _, _ := streamToACP(context.Background(), sess, conn, evCh)
	assert.Equal(t, acp.StopReasonEndTurn, stopReason)
	notifications := drainNotifications(w, ch)

	require.Len(t, notifications, 3)
	ids := messageChunkIDs(notifications, "agent_message_chunk")
	require.Len(t, ids, 3)
	require.NotEmpty(t, ids[0], "text chunks must carry a messageId")
	assert.Equal(t, ids[0], ids[1], "deltas of one message share an ID")
	assert.Equal(t, ids[0], ids[2], "deltas of one message share an ID")
}

// TestStreamToACP_MessageID_RotatesAfterToolResult pins that text emitted
// AFTER a tool round (the model's next response) starts a NEW message ID —
// otherwise the client would merge two distinct messages around the tool.
func TestStreamToACP_MessageID_RotatesAfterToolResult(t *testing.T) {
	sess, _ := newACPReviewSession(t, testConfig, &acpReviewMockProvider{name: "p", model: "m"})
	conn, w, ch := mockACPConn(t)

	evCh := make(chan agent.AgentEvent, 8)
	evCh <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "before tool"}
	evCh <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "bash", ToolID: "call_1"}
	evCh <- agent.AgentEvent{Type: agent.AgentEventToolCallArgs, ToolID: "call_1", ToolArgs: `{"command":"ls"}`}
	evCh <- agent.AgentEvent{Type: agent.AgentEventToolResult, ToolID: "call_1", ToolName: "bash"}
	evCh <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "after tool"}
	close(evCh)

	streamToACP(context.Background(), sess, conn, evCh)
	notifications := drainNotifications(w, ch)

	ids := messageChunkIDs(notifications, "agent_message_chunk")
	require.Len(t, ids, 2, "two text messages expected")
	require.NotEmpty(t, ids[0])
	require.NotEmpty(t, ids[1])
	assert.NotEqual(t, ids[0], ids[1], "post-tool text must start a new message")
}

// TestStreamToACP_MessageID_NotificationIsolated pins that one-shot
// notifications (auto-compact) are their own message AND terminate the
// in-flight text message — text after them starts a fresh ID.
func TestStreamToACP_MessageID_NotificationIsolated(t *testing.T) {
	sess, _ := newACPReviewSession(t, testConfig, &acpReviewMockProvider{name: "p", model: "m"})
	conn, w, ch := mockACPConn(t)

	evCh := make(chan agent.AgentEvent, 4)
	evCh <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "before compact"}
	evCh <- agent.AgentEvent{Type: agent.AgentEventAutoCompactStart}
	evCh <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "after compact"}
	close(evCh)

	streamToACP(context.Background(), sess, conn, evCh)
	notifications := drainNotifications(w, ch)

	ids := messageChunkIDs(notifications, "agent_message_chunk")
	require.Len(t, ids, 3, "text + compact notice + text")
	for _, id := range ids {
		require.NotEmpty(t, id)
	}
	assert.NotEqual(t, ids[0], ids[1], "notification is its own message")
	assert.NotEqual(t, ids[1], ids[2], "post-notification text is a new message")
	assert.NotEqual(t, ids[0], ids[2], "post-notification text is a new message")
}

// TestStreamToACP_MessageID_ThoughtSeparateFromText pins that a thinking
// stream and a text stream of the same response get distinct IDs — clients
// render reasoning and answer as separate blocks.
func TestStreamToACP_MessageID_ThoughtSeparateFromText(t *testing.T) {
	sess, _ := newACPReviewSession(t, testConfig, &acpReviewMockProvider{name: "p", model: "m"})
	conn, w, ch := mockACPConn(t)

	evCh := make(chan agent.AgentEvent, 4)
	evCh <- agent.AgentEvent{Type: agent.AgentEventThinkingDelta, ThinkingDelta: "hmm"}
	evCh <- agent.AgentEvent{Type: agent.AgentEventThinkingDelta, ThinkingDelta: " let me see"}
	evCh <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "answer"}
	close(evCh)

	streamToACP(context.Background(), sess, conn, evCh)
	notifications := drainNotifications(w, ch)

	thoughtIDs := messageChunkIDs(notifications, "agent_thought_chunk")
	require.Len(t, thoughtIDs, 2)
	require.NotEmpty(t, thoughtIDs[0])
	assert.Equal(t, thoughtIDs[0], thoughtIDs[1], "one thinking message spans deltas")

	textIDs := messageChunkIDs(notifications, "agent_message_chunk")
	require.Len(t, textIDs, 1)
	require.NotEmpty(t, textIDs[0])
	assert.NotEqual(t, thoughtIDs[0], textIDs[0], "thinking and text are separate messages")
}

// TestStreamToACP_MessageID_ReplayUnique pins that replayed history assigns a
// fresh ID per assistant/thinking message.
func TestStreamToACP_MessageID_ReplayUnique(t *testing.T) {
	_, acpSess := setupSessionWithMessages(t, "/proj", []session.Message{
		{Type: session.MessageTypeAssistant, Content: "first"},
		{Type: session.MessageTypeThinking, Content: "thinking"},
		{Type: session.MessageTypeAssistant, Content: "second"},
	})

	conn, w, ch := mockACPConn(t)
	replaySessionHistory(t.Context(), conn, acpSess)
	notifications := drainNotifications(w, ch)

	textIDs := messageChunkIDs(notifications, "agent_message_chunk")
	require.Len(t, textIDs, 2, "two assistant messages")
	require.NotEmpty(t, textIDs[0])
	assert.NotEqual(t, textIDs[0], textIDs[1], "each replayed message gets its own ID")
}
