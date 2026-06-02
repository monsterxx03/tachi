package manager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/skill"
	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
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
	})

	require.NotNil(t, mgr)
}

func TestNewManagerDefaults(t *testing.T) {
	cfg := config.DefaultConfig()

	mgr := New(Config{
		Cfg:          cfg,
	})

	require.NotNil(t, mgr)
}

func TestManagerAddChannel(t *testing.T) {
	mgr := New(Config{
		Cfg:          config.DefaultConfig(),
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

	result, err := mgr.drainEvents(eventCh, aiAgent, func() bool { return false }, nil, nil)
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
	aiAgent.RegisterTool(agenttools.NewEditTool())

	eventCh := aiAgent.RunConversationStream(
		t.Context(),
		nil,
		"edit something",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(eventCh, aiAgent, func() bool { return false }, nil, nil)
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

	result, err := mgr.drainEvents(eventCh, aiAgent, func() bool { return false }, nil, nil)
	t.Logf("result=%q err=%v", result, err)
	// Must not deadlock — either completes with an error or empty response.
}

// TestLoadThreadSession_CreatesNewSession verifies that for a brand-new
// ThreadID, loadThreadSession creates a fresh session manager and session.
func TestLoadThreadSession_CreatesNewSession(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
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

	result, err := mgr.drainEvents(eventCh, aiAgent, func() bool { return true }, sendProgress, nil)
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
	assert.Contains(t, resp, "📊 **Session Usage**")

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

//go:fix inline
func newInt64Ptr(v int64) *int64 { return new(v) }

func TestBuildUserMessageWithAttachments_NoAttachments(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "hello world",
	}
	result, _ := buildUserMessageWithAttachments(msg)
	if result != "hello world" {
		t.Errorf("expected 'hello world', got %q", result)
	}
}

func TestBuildUserMessageWithAttachments_TextFile(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "请帮我 review 这段代码",
		Attachments: []channel.Attachment{
			{
				Type:        channel.AttachmentTypeFile,
				FileName:    "main.go",
				Size:        42,
				TextContent: "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}",
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	if !contains(result, "[文件: main.go]") {
		t.Errorf("expected file header, got %q", result)
	}
	if !contains(result, "package main") {
		t.Errorf("expected file content, got %q", result)
	}
	if !contains(result, "请帮我 review 这段代码") {
		t.Errorf("expected original text, got %q", result)
	}
}

func TestBuildUserMessageWithAttachments_Image(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "这是什么图片？",
		Attachments: []channel.Attachment{
			{
				Type:     channel.AttachmentTypeImage,
				FileName: "image.jpg",
				Size:     65536,
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	if !contains(result, "[图片: image.jpg (64.0KB)]") {
		t.Errorf("expected image summary, got %q", result)
	}
	if !contains(result, "这是什么图片？") {
		t.Errorf("expected original text, got %q", result)
	}
}

func TestBuildUserMessageWithAttachments_Error(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "看下这个文件",
		Attachments: []channel.Attachment{
			{
				Type:     channel.AttachmentTypeFile,
				FileName: "secret.pdf",
				Error:    "download failed: connection reset",
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	if !contains(result, "下载失败") || !contains(result, "secret.pdf") {
		t.Errorf("expected error info, got %q", result)
	}
	if !contains(result, "看下这个文件") {
		t.Errorf("expected original text, got %q", result)
	}
}

func TestBuildUserMessageWithAttachments_MultipleAttachments(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "",
		Attachments: []channel.Attachment{
			{
				Type:        channel.AttachmentTypeFile,
				FileName:    "a.txt",
				TextContent: "file A",
			},
			{
				Type:        channel.AttachmentTypeFile,
				FileName:    "b.txt",
				TextContent: "file B",
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	if !contains(result, "file A") || !contains(result, "file B") {
		t.Errorf("expected both files, got %q", result)
	}
}

func TestBuildUserMessageWithAttachments_BinaryFile(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "解压这个文件",
		Attachments: []channel.Attachment{
			{
				Type:     channel.AttachmentTypeFile,
				FileName: "archive.zip",
				MimeType: "application/zip",
				Size:     1048576,
				Content:  []byte{0x50, 0x4b, 0x03, 0x04}, // zip header
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	if !contains(result, "archive.zip") || !contains(result, "1.0MB") {
		t.Errorf("expected binary file info, got %q", result)
	}
	if !contains(result, "解压这个文件") {
		t.Errorf("expected original text, got %q", result)
	}
}

func TestBuildUserMessageWithAttachments_TextFileWithSavedPath(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "帮我看下这个文件",
		Attachments: []channel.Attachment{
			{
				Type:        channel.AttachmentTypeFile,
				FileName:    "main.go",
				TextContent: "package main\nfunc main() {}",
				SavedPath:   "/home/user/.tachi/weixin_files/bot/u/main.go-12345",
				Size:        42,
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	assert.True(t, contains(result, "已保存到"), "should mention saved path: %s", result)
	assert.True(t, contains(result, "main.go-12345"), "should include full path: %s", result)
	assert.True(t, contains(result, "package main"), "should include inline content: %s", result)
	assert.True(t, contains(result, "帮我看下这个文件"), "should include original text: %s", result)
}

func TestBuildUserMessageWithAttachments_BinaryWithSavedPath(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "解析这个 PDF",
		Attachments: []channel.Attachment{
			{
				Type:      channel.AttachmentTypeFile,
				FileName:  "report.pdf",
				MimeType:  "application/pdf",
				Size:      204800,
				SavedPath: "/home/user/.tachi/weixin_files/bot/u/report.pdf-abc",
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	assert.True(t, contains(result, "已保存到本地"), "should mention local save: %s", result)
	assert.True(t, contains(result, "Bash"), "should mention Bash tool: %s", result)
	assert.True(t, contains(result, "解析这个 PDF"), "should include original text: %s", result)
}

func TestBuildUserMessageWithAttachments_ImageWithSavedPath(t *testing.T) {
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "这是什么图片？",
		Attachments: []channel.Attachment{
			{
				Type:      channel.AttachmentTypeImage,
				FileName:  "photo.jpg",
				Size:      524288,
				SavedPath: "/home/user/.tachi/weixin_files/bot/u/photo.jpg-xyz",
			},
		},
	}
	result, _ := buildUserMessageWithAttachments(msg)
	assert.True(t, contains(result, "已保存到"), "should mention saved path: %s", result)
	assert.True(t, contains(result, "photo.jpg-xyz"), "should include file path: %s", result)
	assert.True(t, contains(result, "这是什么图片？"), "should include original text: %s", result)
}

func TestBuildUserMessageWithAttachments_ImageWithContent(t *testing.T) {
	imageBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0} // fake JPEG header
	msg := channel.IncomingMessage{
		ThreadID: "test",
		Content:  "describe this",
		Attachments: []channel.Attachment{
			{
				Type:     channel.AttachmentTypeImage,
				FileName: "photo.jpg",
				MimeType: "image/jpeg",
				Content:  imageBytes,
				Size:     int64(len(imageBytes)),
			},
		},
	}
	result, images := buildUserMessageWithAttachments(msg)
	assert.True(t, contains(result, "describe this"), "should include user text")
	assert.True(t, contains(result, "[图片: photo.jpg"), "should include image marker")
	assert.Len(t, images, 1, "should return one image part")
	assert.Equal(t, "image/jpeg", images[0].MediaType)
	assert.NotEmpty(t, images[0].Data, "should have base64 data")
}

// TestHandleModelCommand_List verifies that /model (no args) lists all
// configured providers, marking the active one with a star.
func TestHandleModelCommand_List(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gpt-5.2", Type: "openai", Model: "gpt-5.2"},
			{Name: "claude-haiku", Type: "anthropic", Model: "claude-3-5-haiku-20241022"},
			{Name: "deepseek", Type: "openai", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1"},
		},
	}
	mgr := New(Config{
		Cfg:          cfg,
	})
	// Set up the provider manually (simulating what initProvider would do).
	mgr.provider = &mockProvider{name: "openai"}
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:  "openai",
			Model: "gpt-5.2",
			Name:  "gpt-5.2",
		},
		MaxTokens:     4096,
		MaxIterations: 50,
	}
	mgr.currentProviderName = "gpt-5.2"

	resp, err := mgr.handleModelCommand("")
	require.NoError(t, err)

	assert.Contains(t, resp, "Configured models (3)")
	assert.Contains(t, resp, "* gpt-5.2")     // active
	assert.Contains(t, resp, " claude-haiku") // not active, no star
	assert.Contains(t, resp, " deepseek")     // not active, no star
	assert.Contains(t, resp, "Type: openai  Model: gpt-5.2")
	assert.Contains(t, resp, "Type: anthropic  Model: claude-3-5-haiku-20241022")
	assert.Contains(t, resp, "/model <name> to switch")
}

// TestHandleModelCommand_Switch verifies that /model <name> successfully
// switches the active provider.
func TestHandleModelCommand_Switch(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gpt-5.2", Type: "openai", Model: "gpt-5.2"},
			{Name: "claude-haiku", Type: "anthropic", Model: "claude-3-5-haiku-20241022", APIKey: "sk-ant-test"},
		},
	}
	mgr := New(Config{
		Cfg:          cfg,
	})
	mgr.provider = &mockProvider{name: "openai"}
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "gpt-5.2",
			Name:          "gpt-5.2",
			ContextWindow: 128_000,
		},
		MaxTokens:     4096,
		MaxIterations: 50,
	}
	mgr.currentProviderName = "gpt-5.2"

	resp, err := mgr.handleModelCommand("claude-haiku")
	require.NoError(t, err)
	assert.Contains(t, resp, "Switched to **claude-haiku**")
	assert.Contains(t, resp, "anthropic")
	assert.Contains(t, resp, "claude-3-5-haiku-20241022")

	// Verify internal state was updated.
	mgr.providerMu.RLock()
	defer mgr.providerMu.RUnlock()

	assert.Equal(t, "claude-haiku", mgr.currentProviderName)
	assert.NotNil(t, mgr.provider)
	assert.NotNil(t, mgr.resolvedConfig)
	assert.Equal(t, "anthropic", mgr.resolvedConfig.Provider.Type)
	assert.Equal(t, "claude-3-5-haiku-20241022", mgr.resolvedConfig.Provider.Model)
}

// TestHandleModelCommand_Unknown verifies that /model <unknown> returns
// a helpful error message.
func TestHandleModelCommand_Unknown(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gpt-5.2", Type: "openai", Model: "gpt-5.2"},
		},
	}
	mgr := New(Config{
		Cfg:          cfg,
	})
	mgr.provider = &mockProvider{name: "openai"}
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:  "openai",
			Model: "gpt-5.2",
			Name:  "gpt-5.2",
		},
		MaxTokens: 4096,
	}
	mgr.currentProviderName = "gpt-5.2"

	resp, err := mgr.handleModelCommand("nonexistent")
	require.NoError(t, err)
	assert.Contains(t, resp, "not found")
	assert.Contains(t, resp, "/model")
}

// TestHandleModelCommand_NoProviders verifies the empty providers case.
func TestHandleModelCommand_NoProviders(t *testing.T) {
	cfg := &config.Config{}
	mgr := New(Config{
		Cfg:          cfg,
	})

	resp, err := mgr.handleModelCommand("")
	require.NoError(t, err)
	assert.Contains(t, resp, "No providers configured")
}

// TestHandleModelCommand_ListAfterSwitch verifies that after a switch,
// the list marks the new active provider.
func TestHandleModelCommand_ListAfterSwitch(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gpt-5.2", Type: "openai", Model: "gpt-5.2"},
			{Name: "claude-haiku", Type: "anthropic", Model: "claude-3-5-haiku-20241022", APIKey: "sk-ant-test"},
		},
	}
	mgr := New(Config{
		Cfg:          cfg,
	})
	mgr.provider = &mockProvider{name: "openai"}
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "gpt-5.2",
			Name:          "gpt-5.2",
			ContextWindow: 128_000,
		},
		MaxTokens:     4096,
		MaxIterations: 50,
	}
	mgr.currentProviderName = "gpt-5.2"

	// Before switch: gpt-5.2 is active.
	resp, err := mgr.handleModelCommand("")
	require.NoError(t, err)
	assert.Contains(t, resp, "* gpt-5.2")
	assert.Contains(t, resp, " claude-haiku")

	// Switch.
	_, err = mgr.handleModelCommand("claude-haiku")
	require.NoError(t, err)

	// After switch: claude-haiku is active.
	resp, err = mgr.handleModelCommand("")
	require.NoError(t, err)
	assert.Contains(t, resp, " gpt-5.2")
	assert.Contains(t, resp, "* claude-haiku")
}

// TestHandleModelCommand_ViaTextSlash verifies /model via the text-based
// slash command handler.
func TestHandleModelCommand_ViaTextSlash(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gpt-5.2", Type: "openai", Model: "gpt-5.2"},
			{Name: "claude-haiku", Type: "anthropic", Model: "claude-3-5-haiku-20241022", APIKey: "sk-ant-test"},
		},
	}
	mgr := New(Config{
		Cfg:          cfg,
	})
	mgr.provider = &mockProvider{name: "openai"}
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "gpt-5.2",
			Name:          "gpt-5.2",
			ContextWindow: 128_000,
		},
		MaxTokens:     4096,
		MaxIterations: 50,
	}
	mgr.currentProviderName = "gpt-5.2"

	// /model (list)
	result := mgr.handleSlashCommand(channel.IncomingMessage{
		Content:   "/model",
		ThreadID:  "thread-1",
		MessageID: "msg-1",
	})
	resp := result.Reply.Content
	assert.Contains(t, resp, "Configured models (2)")
	assert.Contains(t, resp, "* gpt-5.2")

	// /model claude-haiku (switch)
	result = mgr.handleSlashCommand(channel.IncomingMessage{
		Content:   "/model claude-haiku",
		ThreadID:  "thread-1",
		MessageID: "msg-2",
	})
	resp = result.Reply.Content
	assert.Contains(t, resp, "Switched to **claude-haiku**")

	mgr.providerMu.RLock()
	defer mgr.providerMu.RUnlock()
	assert.Equal(t, "claude-haiku", mgr.currentProviderName)
}

// TestHandleModelCommand_ViaCommandHandler verifies /model via the typed
// CommandHandler path.
func TestHandleModelCommand_ViaCommandHandler(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gpt-5.2", Type: "openai", Model: "gpt-5.2"},
			{Name: "claude-haiku", Type: "anthropic", Model: "claude-3-5-haiku-20241022", APIKey: "sk-ant-test"},
		},
	}
	mgr := New(Config{
		Cfg:          cfg,
	})
	mgr.provider = &mockProvider{name: "openai"}
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "gpt-5.2",
			Name:          "gpt-5.2",
			ContextWindow: 128_000,
		},
		MaxTokens:     4096,
		MaxIterations: 50,
	}
	mgr.currentProviderName = "gpt-5.2"

	handler := mgr.buildCommandHandler()

	// /model list via typed command.
	resp, err := handler(t.Context(), channel.SlashCommand{Name: "model"})
	require.NoError(t, err)
	assert.Contains(t, resp, "Configured models (2)")
	assert.Contains(t, resp, "* gpt-5.2")

	// /model switch via typed command.
	resp, err = handler(t.Context(), channel.SlashCommand{Name: "model", Args: "claude-haiku"})
	require.NoError(t, err)
	assert.Contains(t, resp, "Switched to **claude-haiku**")

	mgr.providerMu.RLock()
	assert.Equal(t, "claude-haiku", mgr.currentProviderName)
	mgr.providerMu.RUnlock()
}

// contains is a simple substring check helper.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Skill command tests ---

// TestHandleSkillList_Empty verifies that /skill list returns the "no skills"
// message when no skills are defined.
func TestHandleSkillList_Empty(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SkillStore:   skill.NewStoreWithDirs(nil, nil),
	})

	resp, err := mgr.handleSkillList()
	require.NoError(t, err)
	assert.Contains(t, resp, "No skills found")
}

// TestHandleSkillReload verifies that /skill reload re-scans directories.
func TestHandleSkillReload(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SkillStore:   skill.NewStoreWithDirs(nil, nil),
	})

	resp, err := mgr.handleSkillReload()
	require.NoError(t, err)
	assert.Contains(t, resp, "Skills 已重新加载")
	assert.Contains(t, resp, "0 个 skill(s)")
}

// TestHandleSkillCommand_List verifies /skill and /skill list via handleSkillCommand.
func TestHandleSkillCommand_List(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SkillStore:   skill.NewStoreWithDirs(nil, nil),
	})

	// /skill (empty args)
	resp, err := mgr.handleSkillCommand("")
	require.NoError(t, err)
	assert.Contains(t, resp, "No skills found")

	// /skill list
	resp, err = mgr.handleSkillCommand("list")
	require.NoError(t, err)
	assert.Contains(t, resp, "No skills found")
}

// TestHandleSkillCommand_Reload verifies /skill reload via handleSkillCommand.
func TestHandleSkillCommand_Reload(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SkillStore:   skill.NewStoreWithDirs(nil, nil),
	})

	resp, err := mgr.handleSkillCommand("reload")
	require.NoError(t, err)
	assert.Contains(t, resp, "Skills 已重新加载")
}

// TestHandleSkillCommand_UnknownSub verifies /skill with unknown sub-command.
func TestHandleSkillCommand_UnknownSub(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SkillStore:   skill.NewStoreWithDirs(nil, nil),
	})

	resp, err := mgr.handleSkillCommand("unknown-skill")
	require.NoError(t, err)
	assert.Contains(t, resp, "Unknown /skill sub-command")
}

// TestSkillViaTextSlash_List verifies /skill via the text-based handler.
func TestSkillViaTextSlash_List(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SkillStore:   skill.NewStoreWithDirs(nil, nil),
	})

	result := mgr.handleSlashCommand(channel.IncomingMessage{
		Content:   "/skill",
		ThreadID:  "thread-1",
		MessageID: "msg-1",
	})
	resp := result.Reply.Content
	assert.Contains(t, resp, "No skills found")

	result = mgr.handleSlashCommand(channel.IncomingMessage{
		Content:   "/skill list",
		ThreadID:  "thread-1",
		MessageID: "msg-2",
	})
	resp = result.Reply.Content
	assert.Contains(t, resp, "No skills found")

	result = mgr.handleSlashCommand(channel.IncomingMessage{
		Content:   "/skill reload",
		ThreadID:  "thread-1",
		MessageID: "msg-3",
	})
	resp = result.Reply.Content
	assert.Contains(t, resp, "Skills 已重新加载")
}

// TestSkillViaCommandHandler verifies /skill via the typed CommandHandler.
func TestSkillViaCommandHandler(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SkillStore:   skill.NewStoreWithDirs(nil, nil),
	})
	handler := mgr.buildCommandHandler()

	// /skill list via typed command
	resp, err := handler(t.Context(), channel.SlashCommand{Name: "skill"})
	require.NoError(t, err)
	assert.Contains(t, resp, "No skills found")

	// /skill list via typed command with Args
	resp, err = handler(t.Context(), channel.SlashCommand{Name: "skill", Args: "list"})
	require.NoError(t, err)
	assert.Contains(t, resp, "No skills found")

	// /skill reload
	resp, err = handler(t.Context(), channel.SlashCommand{Name: "skill", Args: "reload"})
	require.NoError(t, err)
	assert.Contains(t, resp, "Skills 已重新加载")
}

// TestIsSkillActivation_NoSkill verifies isSkillActivation returns false
// for non-skill messages.
func TestIsSkillActivation_NoSkill(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
	})

	_, _, ok := mgr.isSkillActivation("/help")
	assert.False(t, ok)

	_, _, ok = mgr.isSkillActivation("hello")
	assert.False(t, ok)
}

// TestIsSkillActivation_ListNotActivation verifies /skill list and
// /skill reload are not treated as skill activations.
func TestIsSkillActivation_ListNotActivation(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
	})

	_, _, ok := mgr.isSkillActivation("/skill list")
	assert.False(t, ok, "/skill list should not be an activation")

	_, _, ok = mgr.isSkillActivation("/skill reload")
	assert.False(t, ok, "/skill reload should not be an activation")
}

// TestPrepareSkillActivation_NotFound verifies error handling for unknown skills.
func TestPrepareSkillActivation_NotFound(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
	})

	_, errMsg, err := mgr.prepareSkillActivation("nonexistent-skill", "")
	assert.Error(t, err)
	assert.Contains(t, errMsg, "未找到")
}
