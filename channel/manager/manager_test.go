package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	sesspkg "github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- helpers ----

// newTempSessionStore creates a FileStore backed by a test-temporary directory
// that is automatically cleaned up when the test finishes.
func newTempSessionStore(t *testing.T) *sesspkg.FileStore {
	t.Helper()
	store, err := sesspkg.NewFileStore(t.TempDir())
	require.NoError(t, err)
	return store
}

// ---- Mock Channel ----

type mockChannel struct {
	name        string
	runFunc     func(ctx context.Context, handler channel.MessageHandler) error
	mu          sync.Mutex
	lastHandler channel.MessageHandler
	running     bool
}

func (m *mockChannel) Name() string { return m.name }

func (m *mockChannel) OnStart(ctx context.Context) error {
	return nil // mock: no pre-start setup needed
}

func (m *mockChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	m.mu.Lock()
	m.lastHandler = handler
	m.running = true
	m.mu.Unlock()

	if m.runFunc != nil {
		return m.runFunc(ctx, handler)
	}

	// Default: block until context cancelled (simulates long-polling loop).
	<-ctx.Done()

	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
	return nil
}

func (m *mockChannel) getHandler() channel.MessageHandler {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastHandler
}

func (m *mockChannel) isRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// ---- Mock LLM Provider ----

type mockProvider struct {
	name       string
	responses  []string
	respIdx    int
	streamFunc func(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error)
}

func (p *mockProvider) Name() string { return p.name }

func (p *mockProvider) CreateChat(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}

func (p *mockProvider) CreateChatStream(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	if p.streamFunc != nil {
		return p.streamFunc(ctx, messages, tools, opts)
	}

	ch := make(chan llm.StreamEvent, 16)
	go func() {
		defer close(ch)

		if p.respIdx < len(p.responses) {
			text := p.responses[p.respIdx]
			p.respIdx++

			// Simulate streaming deltas — one rune at a time.
			for _, r := range text {
				ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: string(r)}
			}
		}
		ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
	}()
	return ch, nil
}

// ---- Tests ----

func TestChannelInterface(t *testing.T) {
	var ch channel.Channel = &mockChannel{name: "test"}
	assert.Equal(t, "test", ch.Name())
}

func TestIncomingOutgoingMessage(t *testing.T) {
	in := channel.IncomingMessage{
		ThreadID:  "user-42",
		MessageID: "msg-1",
		Content:   "hello",
		ChannelID: "group-chat",
	}

	out := channel.OutgoingMessage{
		ThreadID: in.ThreadID,
		Content:  "hi there",
		ReplyTo:  in.MessageID,
	}

	assert.Equal(t, "user-42", out.ThreadID)
	assert.Equal(t, "msg-1", out.ReplyTo)
	assert.Equal(t, "hi there", out.Content)
}

func TestNewManager(t *testing.T) {
	cfg := config.DefaultConfig()

	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
	})

	require.NotNil(t, mgr)
	assert.Equal(t, "test prompt", mgr.systemPrompt)
}

func TestNewManagerDefaults(t *testing.T) {
	cfg := config.DefaultConfig()

	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
	})

	require.NotNil(t, mgr)
	assert.Equal(t, "test prompt", mgr.systemPrompt)
}

func TestManagerAddChannel(t *testing.T) {
	mgr := New(Config{
		Cfg:          config.DefaultConfig(),
		SystemPrompt: "test",
	})

	mgr.Add(&mockChannel{name: "chan-a"})
	mgr.Add(&mockChannel{name: "chan-b"})

	assert.Len(t, mgr.channels, 2)
	assert.Equal(t, "chan-a", mgr.channels[0].Name())
	assert.Equal(t, "chan-b", mgr.channels[1].Name())
}

func TestChannelStopsOnContextCancel(t *testing.T) {
	ch := &mockChannel{name: "test-chan"}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		err := ch.Run(ctx, nil)
		assert.NoError(t, err)
		close(done)
	}()

	cancel()
	<-done

	assert.False(t, ch.isRunning())
}

func TestMessageHandlerReturnsErrorWithoutProvider(t *testing.T) {
	mgr := New(Config{
		Cfg:          config.DefaultConfig(),
		SystemPrompt: "test prompt",
	})

	handler := mgr.buildHandler()

	msg := channel.IncomingMessage{
		ThreadID:  "thread-1",
		MessageID: "msg-1",
		Content:   "hello",
	}

	ctx := t.Context()
	result := handler(ctx, msg)

	// Expect error: initProvider was never called, so provider is nil.
	assert.Error(t, result.Err)
	// Handler still returns a structured OutgoingMessage with error text.
	assert.Equal(t, "thread-1", result.Reply.ThreadID)
	assert.Equal(t, "msg-1", result.Reply.ReplyTo)
	assert.Contains(t, result.Reply.Content, "❌")
	assert.False(t, result.Steered)
}

// TestDrainEvents_BasicResponse verifies drainEvents collects a clean
// text response from the agent event channel.
func TestDrainEvents_BasicResponse(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
	})

	mp := &mockProvider{
		name:      "mock",
		responses: []string{"Hello, I'm Tachi!"},
	}

	aiAgent := agent.NewAIAgent(mp, "mock-model", 10)
	aiAgent.SetSkipEditConfirm(true)

	eventCh := aiAgent.RunConversationStream(
		t.Context(),
		nil,
		"test message",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(eventCh, aiAgent, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello, I'm Tachi!", result)
}

// TestDrainEvents_ConfirmationDoesNotDeadlock verifies that if a tool
// confirmation event fires (should not happen with skip_edit_confirm, but
// we handle it), drainEvents auto-approves and continues.
func TestDrainEvents_ConfirmationDoesNotDeadlock(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test",
	})

	mp := &mockProvider{}
	mp.streamFunc = func(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
		ch := make(chan llm.StreamEvent, 16)
		go func() {
			defer close(ch)
			ch <- llm.StreamEvent{
				Type: llm.StreamEventToolUseStart,
				ToolCall: &llm.ToolCall{
					ID: "tc-1",
					Function: llm.ToolCallFunction{
						Name:      "EditFile",
						Arguments: `{"path":"/tmp/test.txt","old_string":"foo","new_string":"bar"}`,
					},
				},
			}
			ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls"}
		}()
		return ch, nil
	}

	aiAgent := agent.NewAIAgent(mp, "mock-model", 10)
	aiAgent.SetSkipEditConfirm(true)
	// Register EditFile so the tool call can be dispatched (it will error on
	// file read, but the key assertion is: it doesn't deadlock).
	aiAgent.RegisterTool(agenttools.EditTool{})

	eventCh := aiAgent.RunConversationStream(
		t.Context(),
		nil,
		"edit something",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(eventCh, aiAgent, false, nil)
	t.Logf("result=%q err=%v", result, err)
	// Either result is set (tool executed) or err (file not found) — neither
	// case is a deadlock. The function must return.
}

// TestDrainEvents_AskUserDoesNotDeadlock verifies that drainEvents
// auto-rejects AskUser events without blocking.
func TestDrainEvents_AskUserDoesNotDeadlock(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test",
	})

	mp := &mockProvider{}
	mp.streamFunc = func(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
		ch := make(chan llm.StreamEvent, 16)
		go func() {
			defer close(ch)
			ch <- llm.StreamEvent{
				Type: llm.StreamEventToolUseStart,
				ToolCall: &llm.ToolCall{
					ID: "tc-ask-1",
					Function: llm.ToolCallFunction{
						Name:      agenttools.ToolNameAskUser,
						Arguments: `{"questions":[{"question":"test?","header":"Test","options":[{"label":"A","description":"Option A"}],"multiSelect":false}]}`,
					},
				},
			}
			ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls"}
		}()
		return ch, nil
	}

	aiAgent := agent.NewAIAgent(mp, "mock-model", 10)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.RegisterTool(agenttools.AskUserTool{})

	eventCh := aiAgent.RunConversationStream(
		t.Context(),
		nil,
		"ask me something",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(eventCh, aiAgent, false, nil)
	t.Logf("result=%q err=%v", result, err)
	// Must not deadlock — either completes with an error or empty response.
}

// TestLoadThreadSession_CreatesNewSession verifies that for a brand-new
// ThreadID, loadThreadSession creates a fresh session manager and session.
func TestLoadThreadSession_CreatesNewSession(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
		SessionStore: newTempSessionStore(t),
	})
	// Inject resolved config so loadThreadSession can call sm.New().
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "test-model",
			ContextWindow: 128_000,
		},
		MaxTokens: 4096,
	}
	mgr.provider = &mockProvider{name: "mock"}

	// Unique per invocation to avoid interference from prior test runs.
	threadID := fmt.Sprintf("new-%s-%d", t.Name(), time.Now().UnixNano())
	sm, history, err := mgr.loadThreadSession(threadID)
	require.NoError(t, err)
	require.NotNil(t, sm)
	assert.True(t, sm.HasCurrent(), "session should be auto-created")
	assert.Nil(t, history, "no prior history for a new thread")

	sess := sm.Current()
	require.NotNil(t, sess)
	assert.Equal(t, threadID, sess.ThreadID)
}

// TestLoadThreadSession_LoadsExistingSession verifies that for a ThreadID
// with a prior session, loadThreadSession returns the converted history.
func TestLoadThreadSession_LoadsExistingSession(t *testing.T) {
	cfg := config.DefaultConfig()
	store := newTempSessionStore(t)
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
		SessionStore: store,
	})
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "test-model",
			ContextWindow: 128_000,
		},
		MaxTokens: 4096,
	}
	mgr.provider = &mockProvider{name: "mock"}

	threadID := fmt.Sprintf("hist-%s-%d", t.Name(), time.Now().UnixNano())

	// First call: creates session + records a user message.
	sm1, _, err := mgr.loadThreadSession(threadID)
	require.NoError(t, err)

	msg := &sesspkg.Message{
		Type:    sesspkg.MessageTypeUser,
		Content: "hello world",
	}
	err = sm1.AppendMessage(msg)
	require.NoError(t, err)

	// Second call: should find the existing session and return history.
	sm2, history, err := mgr.loadThreadSession(threadID)
	require.NoError(t, err)
	require.NotNil(t, history, "should return history from existing session")
	assert.Len(t, history, 1)
	assert.Equal(t, "user", history[0].Role)
	assert.Equal(t, "hello world", history[0].Content)

	// Both handles point to the same session ID.
	assert.Equal(t, sm1.Current().ID, sm2.Current().ID)
}

// TestDrainEvents_VerboseMode verifies that drainEvents sends intermediate
// tool call results via sendProgress when verbose is true, and does not
// bundle them into the final result.
func TestDrainEvents_VerboseMode(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
	})

	// Create a provider that returns a tool_calls response followed by text.
	mp := &mockProvider{}
	callCount := 0
	mp.streamFunc = func(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
		ch := make(chan llm.StreamEvent, 16)
		go func() {
			defer close(ch)
			callCount++
			if callCount == 1 {
				// First turn: tool calls with streaming args.
				ch <- llm.StreamEvent{
					Type:      llm.StreamEventToolUseStart,
					ToolIndex: 0,
					ToolCall: &llm.ToolCall{
						ID: "tc-1",
						Function: llm.ToolCallFunction{
							Name: "Bash",
						},
					},
				}
				ch <- llm.StreamEvent{
					Type:       llm.StreamEventInputJSONDelta,
					ToolIndex:  0,
					InputDelta: `{"command":"echo ok"}`,
				}
				ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "tool_calls"}
			} else {
				// Second turn: final text
				for _, r := range "Build ok" {
					ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: string(r)}
				}
				ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
			}
		}()
		return ch, nil
	}

	aiAgent := agent.NewAIAgent(mp, "mock-model", 10)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.RegisterTool(agenttools.BashTool{})

	eventCh := aiAgent.RunConversationStream(
		t.Context(),
		nil,
		"build the project",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	// Capture progress messages sent by drainEvents.
	var progressMsgs []string
	sendProgress := func(text string) {
		progressMsgs = append(progressMsgs, text)
	}

	result, err := mgr.drainEvents(eventCh, aiAgent, true, sendProgress)
	require.NoError(t, err)
	// Final result should NOT contain the tool call prefix (it's streamed).
	assert.NotContains(t, result, "🔍 工具调用过程:")
	assert.Equal(t, "Build ok", result)

	// Tool call progress should have been sent.
	require.Len(t, progressMsgs, 1)
	assert.Contains(t, progressMsgs[0], "🔧 Bash(echo ok)")
	assert.Contains(t, progressMsgs[0], "✅")
}

// TestHandleVerboseCommand verifies that /v toggles verbose state correctly.
func TestHandleVerboseCommand(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test",
	})

	threadID := "thread-v-test"

	// First toggle: off → on
	resp, err := mgr.handleVerboseCommand(threadID)
	require.NoError(t, err)
	assert.Contains(t, resp, "ON")

	mgr.verboseMu.RLock()
	assert.True(t, mgr.verboseState[threadID])
	mgr.verboseMu.RUnlock()

	// Second toggle: on → off
	resp, err = mgr.handleVerboseCommand(threadID)
	require.NoError(t, err)
	assert.Contains(t, resp, "OFF")

	mgr.verboseMu.RLock()
	assert.False(t, mgr.verboseState[threadID])
	mgr.verboseMu.RUnlock()
}

// TestHandleVerboseCommand_ResetByNew verifies that /new resets verbose state.
func TestHandleVerboseCommand_ResetByNew(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test",
		SessionStore: newTempSessionStore(t),
	})
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "test-model",
			ContextWindow: 128_000,
		},
		MaxTokens: 4096,
	}
	mgr.provider = &mockProvider{name: "mock"}

	threadID := fmt.Sprintf("vnew-%s-%d", t.Name(), time.Now().UnixNano())

	// Enable verbose.
	_, err := mgr.handleVerboseCommand(threadID)
	require.NoError(t, err)

	mgr.verboseMu.RLock()
	assert.True(t, mgr.verboseState[threadID])
	mgr.verboseMu.RUnlock()

	// /new should reset it.
	_, err = mgr.handleNewCommand(threadID)
	require.NoError(t, err)

	mgr.verboseMu.RLock()
	assert.False(t, mgr.verboseState[threadID])
	mgr.verboseMu.RUnlock()
}

// TestCommandHandler_BuildAndDispatch verifies that buildCommandHandler
// returns a working CommandHandler that dispatches to slash command methods.
func TestCommandHandler_BuildAndDispatch(t *testing.T) {
	cfg := config.DefaultConfig()
	store := newTempSessionStore(t)
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test",
		SessionStore: store,
	})
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "test-model",
			ContextWindow: 128_000,
		},
		MaxTokens: 4096,
	}
	mgr.provider = &mockProvider{name: "mock"}

	handler := mgr.buildCommandHandler()
	require.NotNil(t, handler)

	threadID := fmt.Sprintf("cmd-%s-%d", t.Name(), time.Now().UnixNano())

	// /mcp (global, no ThreadID)
	resp, err := handler(t.Context(), channel.SlashCommand{Name: "mcp"})
	require.NoError(t, err)
	assert.Contains(t, resp, "No MCP servers configured")

	// /cron (global, scheduler nil → "not enabled")
	resp, err = handler(t.Context(), channel.SlashCommand{Name: "cron"})
	require.NoError(t, err)
	assert.Contains(t, resp, "not enabled")

	// /new (thread-scoped) — no session needed
	resp, err = handler(t.Context(), channel.SlashCommand{Name: "new", ThreadID: threadID})
	require.NoError(t, err)
	assert.Contains(t, resp, "Started a new conversation")

	// /v toggles (no session needed)
	resp, err = handler(t.Context(), channel.SlashCommand{Name: "v", ThreadID: threadID})
	require.NoError(t, err)
	assert.Contains(t, resp, "ON")

	resp, err = handler(t.Context(), channel.SlashCommand{Name: "v", ThreadID: threadID})
	require.NoError(t, err)
	assert.Contains(t, resp, "OFF")

	// /usage requires a pre-existing session — load one first.
	sm, _, err := mgr.loadThreadSession(threadID)
	require.NoError(t, err)
	require.NotNil(t, sm)
	require.True(t, sm.HasCurrent())

	msg := &sesspkg.Message{
		Type:    sesspkg.MessageTypeUser,
		Content: "hello",
	}
	_ = sm.AppendMessage(msg)

	resp, err = handler(t.Context(), channel.SlashCommand{Name: "usage", ThreadID: threadID})
	require.NoError(t, err)
	assert.Contains(t, resp, "📊 Session Usage")

	// unknown command
	resp, err = handler(t.Context(), channel.SlashCommand{Name: "nonexistent"})
	require.NoError(t, err)
	assert.Contains(t, resp, "Unknown command")
}

// mockCommandChannel is a Channel that also implements CommandChannel,
// capturing the handler for verification in tests.
type mockCommandChannel struct {
	mockChannel
	cmdHandler channel.CommandHandler
}

func (m *mockCommandChannel) SetCommandHandler(handler channel.CommandHandler) {
	m.cmdHandler = handler
}

// TestCommandChannel_Injection verifies that Manager injects a CommandHandler
// into channels that implement the CommandChannel optional interface.
// We test the injection logic directly without going through Start()
// (which requires a real config provider).
func TestCommandChannel_Injection(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
		SessionStore: newTempSessionStore(t),
	})
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "test-model",
			ContextWindow: 128_000,
		},
		MaxTokens: 4096,
	}
	mgr.provider = &mockProvider{name: "mock"}

	// Build the handler and simulate the Start() injection logic.
	cmdHandler := mgr.buildCommandHandler()

	cmdCh := &mockCommandChannel{
		mockChannel: mockChannel{name: "cmdchan"},
	}

	// Simulate the type assertion + injection that Start() would do.
	if cc, ok := channel.Channel(cmdCh).(channel.CommandChannel); ok {
		cc.SetCommandHandler(cmdHandler)
	}
	require.NotNil(t, cmdCh.cmdHandler, "CommandHandler should be injected")

	// The injected handler should be functional.
	resp, err := cmdCh.cmdHandler(t.Context(), channel.SlashCommand{Name: "mcp"})
	require.NoError(t, err)
	assert.Contains(t, resp, "No MCP servers configured")
}

// TestCommandChannel_NotInjectedToPlainChannel verifies that plain channels
// (not implementing CommandChannel) don't receive anything and type assertion
// succeeds without panicking.
func TestCommandChannel_NotInjectedToPlainChannel(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SystemPrompt: "test prompt",
		SessionStore: newTempSessionStore(t),
	})
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "test-model",
			ContextWindow: 128_000,
		},
		MaxTokens: 4096,
	}
	mgr.provider = &mockProvider{name: "mock"}

	cmdHandler := mgr.buildCommandHandler()

	plainCh := &mockChannel{name: "plainchan"}

	// Simulate the type assertion — should be false (no panic).
	if cc, ok := channel.Channel(plainCh).(channel.CommandChannel); ok {
		cc.SetCommandHandler(cmdHandler)
		t.Error("plain channel should not implement CommandChannel")
	}
	// Pass: type assertion returned false, plain channel untouched.
}

// TestSlashCommand_StringRepresentation verifies SlashCommand struct field semantics.
func TestSlashCommand_StringRepresentation(t *testing.T) {
	cmd := channel.SlashCommand{Name: "new", ThreadID: "thread-1"}
	assert.Equal(t, "new", cmd.Name)
	assert.Equal(t, "thread-1", cmd.ThreadID)
}

func newInt64Ptr(v int64) *int64 { return &v }
