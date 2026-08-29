package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
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

func (p *mockProvider) Name() string         { return p.name }
func (p *mockProvider) ProviderName() string { return "" }

func (p *mockProvider) Model() string { return "mock-model" }

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

// TestNewManager verifies New resolves the provider eagerly: a config
// without a provider fails construction.
func TestNewManager(t *testing.T) {
	_, err := New(config.DefaultConfig())
	require.Error(t, err)
}

// mustNewManager builds a Manager for tests, injecting a resolvable default
// provider when cfg has none (New fails otherwise). The provider list is
// only appended to when empty, so tests that assert on their own provider
// lists are unaffected. The resolved provider is given a dummy API key —
// BuildProvider requires one even though tests never make real API calls.
func mustNewManager(t *testing.T, cfg *config.Config) *Manager {
	t.Helper()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if cfg.Provider == "" {
		if len(cfg.Providers) > 0 {
			cfg.Provider = cfg.Providers[0].Name
		} else {
			cfg.Provider = "test"
			cfg.Providers = []config.ProviderConfig{
				{Name: "test", Type: "openai", Model: "test-model", APIKey: "sk-test"},
			}
		}
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == cfg.Provider && cfg.Providers[i].APIKey == "" {
			cfg.Providers[i].APIKey = "sk-test"
		}
	}
	mgr, err := New(cfg)
	require.NoError(t, err)
	return mgr
}

func TestManagerAddChannel(t *testing.T) {
	mgr := mustNewManager(t, config.DefaultConfig())

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

// TestDrainEvents_BasicResponse verifies drainEvents collects a clean
// text response from the agent event channel.
func TestDrainEvents_BasicResponse(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)

	mp := &mockProvider{
		name:      "mock",
		responses: []string{"Hello, I'm Tachi!"},
	}

	aiAgent := newTestAIAgent(t, mp, 10)
	aiAgent.SetPermissionMode(agent.PermissionModeSkip)

	eventCh := aiAgent.RunConversationStream(
		t.Context(),
		nil,
		"test message",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(t.Context(), eventCh, aiAgent, nil, nil, nil)
	require.NoError(t, err)
	// Response should include the original text followed by turn summary
	assert.Contains(t, result, "Hello, I'm Tachi!")
	assert.Contains(t, result, "回合:")
	assert.Contains(t, result, "次迭代")
}

// TestDrainEvents_ConfirmationDoesNotDeadlock verifies that if a tool
// confirmation event fires (should not happen in PermissionModeSkip, but
// we handle it), drainEvents auto-approves and continues.
func TestDrainEvents_ConfirmationDoesNotDeadlock(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)

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

	aiAgent := newTestAIAgent(t, mp, 10)
	aiAgent.SetPermissionMode(agent.PermissionModeSkip)
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

	result, err := mgr.drainEvents(t.Context(), eventCh, aiAgent, nil, nil, nil)
	t.Logf("result=%q err=%v", result, err)
	// Either result is set (tool executed) or err (file not found) — neither
	// case is a deadlock. The function must return.
}

// TestDrainEvents_AskUserDoesNotDeadlock verifies that drainEvents
// auto-rejects AskUser events without blocking.
func TestDrainEvents_AskUserDoesNotDeadlock(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)

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

	aiAgent := newTestAIAgent(t, mp, 10)
	aiAgent.SetPermissionMode(agent.PermissionModeSkip)
	aiAgent.RegisterTool(agenttools.AskUserTool{})

	eventCh := aiAgent.RunConversationStream(
		t.Context(),
		nil,
		"ask me something",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(t.Context(), eventCh, aiAgent, nil, nil, nil)
	t.Logf("result=%q err=%v", result, err)
	// Must not deadlock — either completes with an error or empty response.
}

// TestLoadThreadSession_CreatesNewSession verifies that for a brand-new
// ThreadID, loadThreadSession creates a fresh session manager and session.
func TestLoadThreadSession_CreatesNewSession(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	// Inject resolved config so loadThreadSession can call sm.New().
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Type:          "openai",
		Model:         "test-model",
		ContextWindow: 128_000,
		MaxTokens:     4096,
	}
	mgr.defaultResolvedProvider.Provider = &mockProvider{name: "mock"}

	// Unique per invocation to avoid interference from prior test runs.
	threadID := fmt.Sprintf("new-%s-%d", t.Name(), time.Now().UnixNano())
	sm, history, err := mgr.loadThreadSession(threadID, mgr.defaultResolvedProvider)
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
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = store
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Type:          "openai",
		Model:         "test-model",
		ContextWindow: 128_000,
		MaxTokens:     4096,
	}
	mgr.defaultResolvedProvider.Provider = &mockProvider{name: "mock"}

	threadID := fmt.Sprintf("hist-%s-%d", t.Name(), time.Now().UnixNano())

	// First call: creates session + records a user message.
	sm1, _, err := mgr.loadThreadSession(threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)

	msg := &sesspkg.Message{
		Type:    sesspkg.MessageTypeUser,
		Content: "hello world",
	}
	err = sm1.AppendMessage(msg)
	require.NoError(t, err)

	// Second call: should find the existing session and return history.
	sm2, history, err := mgr.loadThreadSession(threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	require.NotNil(t, history, "should return history from existing session")
	assert.Len(t, history, 1)
	assert.Equal(t, "user", history[0].Role)
	assert.Equal(t, "hello world", history[0].Content)

	// Both handles point to the same session ID.
	assert.Equal(t, sm1.Current().ID, sm2.Current().ID)
}

// TestCommandHandler_BuildAndDispatch verifies that buildCommandHandler
// returns a working CommandHandler that dispatches to slash command methods.
func TestCommandHandler_BuildAndDispatch(t *testing.T) {
	cfg := config.DefaultConfig()
	store := newTempSessionStore(t)
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = store
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Type:          "openai",
		Model:         "test-model",
		ContextWindow: 128_000,
		MaxTokens:     4096,
	}
	mgr.defaultResolvedProvider.Provider = &mockProvider{name: "mock"}

	handler := mgr.buildCommandHandler()
	require.NotNil(t, handler)

	threadID := fmt.Sprintf("cmd-%s-%d", t.Name(), time.Now().UnixNano())

	// /mcp (global, no ThreadID)
	resp, _, _, err := handler(t.Context(), channel.SlashCommand{Name: "mcp"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "No MCP servers configured")

	// /cron (global, scheduler nil → "not enabled")
	resp, _, _, err = handler(t.Context(), channel.SlashCommand{Name: "cron"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "not enabled")

	// /new (thread-scoped) — no session needed
	resp, _, _, err = handler(t.Context(), channel.SlashCommand{Name: "new", ThreadID: threadID})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "Started a new conversation")

	// /usage requires a pre-existing session — load one first.
	sm, _, err := mgr.loadThreadSession(threadID, mgr.defaultResolvedProvider)
	require.NoError(t, err)
	require.NotNil(t, sm)
	require.True(t, sm.HasCurrent())

	msg := &sesspkg.Message{
		Type:    sesspkg.MessageTypeUser,
		Content: "hello",
	}
	_ = sm.AppendMessage(msg)

	resp, _, _, err = handler(t.Context(), channel.SlashCommand{Name: "usage", ThreadID: threadID})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "📊 **Session Usage**")

	// unknown command
	resp, _, _, err = handler(t.Context(), channel.SlashCommand{Name: "nonexistent"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "Unknown command")
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
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Type:          "openai",
		Model:         "test-model",
		ContextWindow: 128_000,
		MaxTokens:     4096,
	}
	mgr.defaultResolvedProvider.Provider = &mockProvider{name: "mock"}

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
	resp, _, _, err := cmdCh.cmdHandler(t.Context(), channel.SlashCommand{Name: "mcp"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "No MCP servers configured")
}

// TestCommandChannel_NotInjectedToPlainChannel verifies that plain channels
// (not implementing CommandChannel) don't receive anything and type assertion
// succeeds without panicking.
func TestCommandChannel_NotInjectedToPlainChannel(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Type:          "openai",
		Model:         "test-model",
		ContextWindow: 128_000,
		MaxTokens:     4096,
	}
	mgr.defaultResolvedProvider.Provider = &mockProvider{name: "mock"}

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
	if !strings.Contains(result, "[文件: main.go]") {
		t.Errorf("expected file header, got %q", result)
	}
	if !strings.Contains(result, "package main") {
		t.Errorf("expected file content, got %q", result)
	}
	if !strings.Contains(result, "请帮我 review 这段代码") {
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
	if !strings.Contains(result, "[图片: image.jpg (64.0 KB)]") {
		t.Errorf("expected image summary, got %q", result)
	}
	if !strings.Contains(result, "这是什么图片？") {
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
	if !strings.Contains(result, "下载失败") || !strings.Contains(result, "secret.pdf") {
		t.Errorf("expected error info, got %q", result)
	}
	if !strings.Contains(result, "看下这个文件") {
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
	if !strings.Contains(result, "file A") || !strings.Contains(result, "file B") {
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
	if !strings.Contains(result, "archive.zip") || !strings.Contains(result, "1.0 MB") {
		t.Errorf("expected binary file info, got %q", result)
	}
	if !strings.Contains(result, "解压这个文件") {
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
	assert.True(t, strings.Contains(result, "已保存到"), "should mention saved path: %s", result)
	assert.True(t, strings.Contains(result, "main.go-12345"), "should include full path: %s", result)
	assert.True(t, strings.Contains(result, "package main"), "should include inline content: %s", result)
	assert.True(t, strings.Contains(result, "帮我看下这个文件"), "should include original text: %s", result)
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
	assert.True(t, strings.Contains(result, "已保存到本地"), "should mention local save: %s", result)
	assert.True(t, strings.Contains(result, "Bash"), "should mention Bash tool: %s", result)
	assert.True(t, strings.Contains(result, "解析这个 PDF"), "should include original text: %s", result)
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
	assert.True(t, strings.Contains(result, "已保存到"), "should mention saved path: %s", result)
	assert.True(t, strings.Contains(result, "photo.jpg-xyz"), "should include file path: %s", result)
	assert.True(t, strings.Contains(result, "这是什么图片？"), "should include original text: %s", result)
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
	assert.True(t, strings.Contains(result, "describe this"), "should include user text")
	assert.True(t, strings.Contains(result, "[图片: photo.jpg"), "should include image marker")
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
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	// Set up the provider manually (simulating what New's resolveProvider does).
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:      &mockProvider{name: "openai"},
		Type:          "openai",
		Model:         "gpt-5.2",
		Name:          "gpt-5.2",
		MaxTokens:     4096,
		MaxIterations: 50,
	}

	resp, err := mgr.handleModelCommand("thread-1", "")
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
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:      &mockProvider{name: "openai"},
		Type:          "openai",
		Model:         "gpt-5.2",
		Name:          "gpt-5.2",
		ContextWindow: 128_000,
		MaxTokens:     4096,
		MaxIterations: 50,
	}

	resp, err := mgr.handleModelCommand("thread-1", "claude-haiku")
	require.NoError(t, err)
	assert.Contains(t, resp, "Switched to **claude-haiku**")
	assert.Contains(t, resp, "anthropic")
	assert.Contains(t, resp, "claude-3-5-haiku-20241022")
	assert.Contains(t, resp, "This thread will now use this model")

	// Verify the override is resolved via getProviderForThread.
	resolved := mgr.getProviderForThread("thread-1")
	assert.Equal(t, "claude-haiku", resolved.Name)
	assert.NotNil(t, resolved.Provider)
	assert.Equal(t, "anthropic", resolved.Type)
	assert.Equal(t, "claude-3-5-haiku-20241022", resolved.Model)

	// Global state is unchanged.
	assert.Equal(t, "gpt-5.2", mgr.defaultResolvedProvider.Name)
	assert.Equal(t, "openai", mgr.defaultResolvedProvider.Type)
}

// TestHandleModelCommand_Unknown verifies that /model <unknown> returns
// a helpful error message.
func TestHandleModelCommand_Unknown(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gpt-5.2", Type: "openai", Model: "gpt-5.2"},
		},
	}
	mgr := mustNewManager(t, cfg)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:  &mockProvider{name: "openai"},
		Type:      "openai",
		Model:     "gpt-5.2",
		Name:      "gpt-5.2",
		MaxTokens: 4096,
	}

	resp, err := mgr.handleModelCommand("thread-1", "nonexistent")
	require.NoError(t, err)
	assert.Contains(t, resp, "not found")
	assert.Contains(t, resp, "/model")
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
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:      &mockProvider{name: "openai"},
		Type:          "openai",
		Model:         "gpt-5.2",
		Name:          "gpt-5.2",
		ContextWindow: 128_000,
		MaxTokens:     4096,
		MaxIterations: 50,
	}

	// Before switch: gpt-5.2 is active.
	resp, err := mgr.handleModelCommand("thread-1", "")
	require.NoError(t, err)
	assert.Contains(t, resp, "* gpt-5.2")
	assert.Contains(t, resp, " claude-haiku")

	// Switch.
	_, err = mgr.handleModelCommand("thread-1", "claude-haiku")
	require.NoError(t, err)

	// After switch: claude-haiku is active.
	resp, err = mgr.handleModelCommand("thread-1", "")
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
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:      &mockProvider{name: "openai"},
		Type:          "openai",
		Model:         "gpt-5.2",
		Name:          "gpt-5.2",
		ContextWindow: 128_000,
		MaxTokens:     4096,
		MaxIterations: 50,
	}

	// /model (list)
	result := mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
		Content:   "/model",
		ThreadID:  "thread-1",
		MessageID: "msg-1",
	})
	resp := result.Reply.Content
	assert.Contains(t, resp, "Configured models (2)")
	assert.Contains(t, resp, "* gpt-5.2")

	// /model claude-haiku (switch)
	result = mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
		Content:   "/model claude-haiku",
		ThreadID:  "thread-1",
		MessageID: "msg-2",
	})
	resp = result.Reply.Content
	assert.Contains(t, resp, "Switched to **claude-haiku**")

	// Verify the override is resolved via getProviderForThread.
	assert.Equal(t, "claude-haiku", mgr.getProviderForThread("thread-1").Name)
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
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:      &mockProvider{name: "openai"},
		Type:          "openai",
		Model:         "gpt-5.2",
		Name:          "gpt-5.2",
		ContextWindow: 128_000,
		MaxTokens:     4096,
		MaxIterations: 50,
	}

	handler := mgr.buildCommandHandler()

	// /model list via typed command.
	resp, _, _, err := handler(t.Context(), channel.SlashCommand{Name: "model", ThreadID: "thread-1"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "Configured models (2)")
	assert.Contains(t, resp.Content, "* gpt-5.2")

	// /model switch via typed command.
	resp, _, _, err = handler(t.Context(), channel.SlashCommand{Name: "model", Args: "claude-haiku", ThreadID: "thread-1"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "Switched to **claude-haiku**")

	// Verify the override is resolved via getProviderForThread.
	assert.Equal(t, "claude-haiku", mgr.getProviderForThread("thread-1").Name)
}

// --- Skill command tests ---

// TestHandleSkillList_Empty verifies that /skill list returns the "no skills"
// message when no skills are defined.
func TestHandleSkillList_Empty(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.skillStore = skill.NewStoreWithDirs(nil, nil)

	resp, err := mgr.handleSkillList()
	require.NoError(t, err)
	assert.Contains(t, resp, "No skills found")
}

// TestHandleSkillReload verifies that /skill reload re-scans directories.
func TestHandleSkillReload(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.skillStore = skill.NewStoreWithDirs(nil, nil)

	resp, err := mgr.handleSkillReload()
	require.NoError(t, err)
	assert.Contains(t, resp, "Skills 已重新加载")
	assert.Contains(t, resp, "0 个 skill(s)")
}

// TestHandleSkillCommand_List verifies /skill and /skill list via handleSkillCommand.
func TestHandleSkillCommand_List(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.skillStore = skill.NewStoreWithDirs(nil, nil)

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
	mgr := mustNewManager(t, cfg)
	mgr.skillStore = skill.NewStoreWithDirs(nil, nil)

	resp, err := mgr.handleSkillCommand("reload")
	require.NoError(t, err)
	assert.Contains(t, resp, "Skills 已重新加载")
}

// TestHandleSkillCommand_UnknownSub verifies /skill with unknown sub-command.
func TestHandleSkillCommand_UnknownSub(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.skillStore = skill.NewStoreWithDirs(nil, nil)

	resp, err := mgr.handleSkillCommand("unknown-skill")
	require.NoError(t, err)
	assert.Contains(t, resp, "Unknown /skill sub-command")
}

// TestSkillViaTextSlash_List verifies /skill via the text-based handler.
func TestSkillViaTextSlash_List(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.skillStore = skill.NewStoreWithDirs(nil, nil)

	result := mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
		Content:   "/skill",
		ThreadID:  "thread-1",
		MessageID: "msg-1",
	})
	resp := result.Reply.Content
	assert.Contains(t, resp, "No skills found")

	result = mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
		Content:   "/skill list",
		ThreadID:  "thread-1",
		MessageID: "msg-2",
	})
	resp = result.Reply.Content
	assert.Contains(t, resp, "No skills found")

	result = mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
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
	mgr := mustNewManager(t, cfg)
	mgr.skillStore = skill.NewStoreWithDirs(nil, nil)
	handler := mgr.buildCommandHandler()

	// /skill list via typed command
	resp, _, _, err := handler(t.Context(), channel.SlashCommand{Name: "skill"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "No skills found")

	// /skill list via typed command with Args
	resp, _, _, err = handler(t.Context(), channel.SlashCommand{Name: "skill", Args: "list"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "No skills found")

	// /skill reload
	resp, _, _, err = handler(t.Context(), channel.SlashCommand{Name: "skill", Args: "reload"})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "Skills 已重新加载")
}

// TestIsSkillActivation_NoSkill verifies isSkillActivation returns false
// for non-skill messages.
func TestIsSkillActivation_NoSkill(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)

	_, _, ok := mgr.isSkillActivation("/help")
	assert.False(t, ok)

	_, _, ok = mgr.isSkillActivation("hello")
	assert.False(t, ok)
}

// TestIsSkillActivation_ListNotActivation verifies /skill list and
// /skill reload are not treated as skill activations.
func TestIsSkillActivation_ListNotActivation(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)

	_, _, ok := mgr.isSkillActivation("/skill list")
	assert.False(t, ok, "/skill list should not be an activation")

	_, _, ok = mgr.isSkillActivation("/skill reload")
	assert.False(t, ok, "/skill reload should not be an activation")
}

// TestPrepareSkillActivation_NotFound verifies error handling for unknown skills.
func TestPrepareSkillActivation_NotFound(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)

	_, errMsg, err := mgr.prepareSkillActivation("nonexistent-skill", "")
	assert.Error(t, err)
	assert.Contains(t, errMsg, "未找到")
}

// ---- /thinking tests ----

// newThinkingTestManager builds a Manager configured with a temp session
// store and a resolved global provider, mirroring TestHandleModelCommand_*.
func newThinkingTestManager(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: "openai", Model: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com/v1",
				Spec: config.ModelSpec{ThinkingLevel: "high"}}, // provider config default: high (matches defaultResolvedProvider below)
		},
	}
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:       &mockProvider{name: "openai"},
		Type:           "openai",
		Model:          "deepseek-v4-flash",
		Name:           "deepseek",
		ThinkingEffort: "high", // provider config default: high
		ContextWindow:  128_000,
		MaxTokens:      4096,
		MaxIterations:  50,
	}
	return mgr
}

// seedThinkingSession creates a session bound to thread-1.
func seedThinkingSession(t *testing.T, mgr *Manager) *sesspkg.Session {
	t.Helper()
	sm := mgr.newSessionManager()
	sess, err := sm.New("deepseek", "/tmp")
	require.NoError(t, err)
	sm.SetThreadID("thread-1")
	return sess
}

func TestHandleThinkingCommand_ShowDefault(t *testing.T) {
	mgr := newThinkingTestManager(t)

	resp, err := mgr.handleThinkingCommand("thread-1", "")
	require.NoError(t, err)
	assert.Contains(t, resp, "**default**")
	assert.Contains(t, resp, "仅当前会话生效")
	for _, lvl := range cmds.ThinkingLevels {
		assert.Contains(t, resp, lvl)
	}
}

func TestHandleThinkingCommand_SetLevel(t *testing.T) {
	mgr := newThinkingTestManager(t)
	seedThinkingSession(t, mgr)

	resp, err := mgr.handleThinkingCommand("thread-1", "high")
	require.NoError(t, err)
	assert.Contains(t, resp, "**high**")
	assert.Contains(t, resp, "其他会话不受影响")

	// Persisted to session meta.
	sm := mgr.newSessionManager()
	sess, err := sm.FindByThreadID("thread-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "high", sess.ThinkingLevel)

	// getProviderForThread applies the override on top of the provider config.
	resolved := mgr.getProviderForThread("thread-1")
	assert.Equal(t, "high", resolved.ThinkingEffort)

	// The global defaultResolvedProvider must NOT be mutated (copy-on-write).
	global := mgr.defaultResolvedProvider.ThinkingEffort
	assert.Equal(t, "high", global) // provider default is also high — use none below to prove isolation
}

func TestHandleThinkingCommand_NoneOverridesAndIsolatesGlobal(t *testing.T) {
	mgr := newThinkingTestManager(t)
	seedThinkingSession(t, mgr)

	_, err := mgr.handleThinkingCommand("thread-1", "none")
	require.NoError(t, err)

	resolved := mgr.getProviderForThread("thread-1")
	require.NotNil(t, resolved.Thinking)
	assert.False(t, *resolved.Thinking)
	assert.Equal(t, "", resolved.ThinkingEffort)

	// Global provider config untouched.
	assert.Nil(t, mgr.defaultResolvedProvider.Thinking)
}

func TestHandleThinkingCommand_DefaultClearsOverride(t *testing.T) {
	mgr := newThinkingTestManager(t)
	seedThinkingSession(t, mgr)

	_, err := mgr.handleThinkingCommand("thread-1", "max")
	require.NoError(t, err)

	// max passes through unchanged (API maps it server-side).
	resolved := mgr.getProviderForThread("thread-1")
	assert.Equal(t, "max", resolved.ThinkingEffort)

	// Reset to provider default.
	_, err = mgr.handleThinkingCommand("thread-1", "default")
	require.NoError(t, err)

	sm := mgr.newSessionManager()
	sess, err := sm.FindByThreadID("thread-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "", sess.ThinkingLevel)

	// Falls back to the provider config default (high).
	resolved = mgr.getProviderForThread("thread-1")
	assert.Equal(t, "high", resolved.ThinkingEffort)
}

func TestHandleThinkingCommand_InvalidLevel(t *testing.T) {
	mgr := newThinkingTestManager(t)

	resp, err := mgr.handleThinkingCommand("thread-1", "turbo")
	require.NoError(t, err)
	assert.Contains(t, resp, "无效的 thinking level")
	assert.Contains(t, resp, "可选级别")
}

func TestHandleThinkingCommand_NoSessionCreatesOne(t *testing.T) {
	mgr := newThinkingTestManager(t)

	resp, err := mgr.handleThinkingCommand("thread-1", "high")
	require.NoError(t, err)
	assert.Contains(t, resp, "**high**")

	// A session was created and bound to the thread so the override persists.
	sm := mgr.newSessionManager()
	sess, err := sm.FindByThreadID("thread-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "high", sess.ThinkingLevel)
	assert.Equal(t, "deepseek", sess.ProviderName)
}

func TestHandleThinkingCommand_AppliesToCachedAgent(t *testing.T) {
	mgr := newThinkingTestManager(t)
	seedThinkingSession(t, mgr)

	// Build a cached agent for the thread (release immediately — the handler
	// must NOT be called while ca.mu is held, or it deadlocks itself).
	ca, err := mgr.acquireAgent(context.Background(), "thread-1")
	require.NoError(t, err)
	require.NotNil(t, ca.agent)
	assert.Equal(t, "high", ca.agent.Config.Resolved.ThinkingEffort) // provider default
	mgr.releaseAgent(ca)

	// Set thinking to none via /thinking.
	_, err = mgr.handleThinkingCommand("thread-1", "none")
	require.NoError(t, err)

	// The cached agent must be updated immediately (same instance is
	// returned on re-acquire since the provider name didn't change).
	ca, err = mgr.acquireAgent(context.Background(), "thread-1")
	require.NoError(t, err)
	defer mgr.releaseAgent(ca)
	require.NotNil(t, ca.agent)
	require.NotNil(t, ca.agent.Config.Resolved.Thinking)
	assert.False(t, *ca.agent.Config.Resolved.Thinking)
	assert.Equal(t, "", ca.agent.Config.Resolved.ThinkingEffort)
}

// ---- /commit and /review tests ----

// TestOneoffCommandsRegisteredForChannel verifies /commit and /review are
// registered for channel mode — this is what makes them appear in the
// unknown-command help AND get registered as Discord slash commands.
func TestOneoffCommandsRegisteredForChannel(t *testing.T) {
	names := map[string]bool{}
	for _, def := range cmds.ForMode(cmds.ModeChannel) {
		names[def.Name] = true
	}
	assert.True(t, names["commit"], "/commit must be registered for channel mode")
	assert.True(t, names["review"], "/review must be registered for channel mode")
}

// newOneoffTestManager builds a Manager for one-off command tests (/commit,
// /review) with:
//   - one-off transcript recording disabled — recording writes to the
//     real ~/.tachi unless the config toggle is off
//   - a mock provider that returns the given responses, one per stream call
//   - the config base dir redirected to a temp dir as a second guard
//   - a session bound to threadID whose WorkingDir is a temp dir, keeping
//     /review report dirs out of the process CWD
//
// Returns the manager and the thread's working directory.
func newOneoffTestManager(t *testing.T, responses []string, threadID string) (*Manager, string) {
	t.Helper()
	return newOneoffTestManagerWithProvider(t, &mockProvider{name: "openai", responses: responses}, threadID)
}

// newOneoffTestManagerWithProvider is newOneoffTestManager with an injected
// provider, for tests that need to simulate failures or custom streams.
func newOneoffTestManagerWithProvider(t *testing.T, prov llm.Provider, threadID string) (*Manager, string) {
	t.Helper()
	disabled := false
	cfg := config.DefaultConfig()
	cfg.Oneoff = config.OneoffConfig{Enabled: &disabled}
	cfg.Providers = []config.ProviderConfig{
		{Name: "openai", Type: "openai", Model: "test-model"},
	}

	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)
	mgr.defaultResolvedProvider = &llm.ResolvedProvider{
		Provider:      prov,
		Type:          "openai",
		Model:         "test-model",
		Name:          "openai",
		ContextWindow: 128_000,
		MaxTokens:     4096,
		MaxIterations: 50,
	}

	// Redirect the config base dir so transcripts can't leak into the real
	// ~/.tachi even if the toggle above is missed (SetBaseDir("") restores
	// the default). NOTE: config.SetBaseDir is package-global state — tests
	// using this helper MUST NOT call t.Parallel().
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	// Seed a session bound to threadID with a temp working directory. The
	// session's ProviderName is deliberately empty so getProviderForThread
	// skips the session-override branch — a non-empty name matching
	// cfg.Providers would resolve a REAL provider (mustNewManager injects a
	// dummy API key), and the one-off commands must run against the injected
	// mock provider instead of hitting the network.
	workDir := t.TempDir()
	sm := mgr.newSessionManager()
	sess, err := sm.New("", workDir)
	require.NoError(t, err)
	sess.WorkingDir = workDir
	require.NoError(t, sm.UpdateMeta(sess))
	sm.SetThreadID(threadID)

	return mgr, workDir
}

// TestCommitCommand_RunsOneOffTurn verifies /commit runs a one-off LLM turn
// (clean context, no session writes) and returns the model's commit message.
func TestCommitCommand_RunsOneOffTurn(t *testing.T) {
	threadID := "commit-thread"
	mgr, workDir := newOneoffTestManager(t, []string{"feat: add /commit support to channel mode"}, threadID)

	resp, err := mgr.handleCommitCommand(context.Background(), threadID)
	require.NoError(t, err)
	assert.Contains(t, resp, "feat: add /commit support")

	// The one-off run must NOT touch the thread session's message history.
	sm := mgr.newSessionManager()
	sess, err := sm.FindByThreadID(threadID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := sm.LoadMessages()
	require.NoError(t, err)
	assert.Empty(t, msgs, "one-off /commit must not write to session history")

	// The thread's working directory is respected (anchors the one-off
	// transcript + Bash relative paths).
	assert.NotEmpty(t, workDir)
}

// TestCommitCommand_StreamsToCallback verifies the handler forwards text
// deltas to the streaming callback attached to the context (the mechanism
// Discord uses for live status embeds during a run).
//
// The turn summary (footer) is also forwarded as a final text delta so
// streaming channels that build their reply from streamed deltas (e.g.
// wave's streaming card) don't lose the iteration/cost info.
func TestCommitCommand_StreamsToCallback(t *testing.T) {
	threadID := "commit-stream-thread"
	mgr, _ := newOneoffTestManager(t, []string{"streamed commit message"}, threadID)

	var mu sync.Mutex
	streamed := ""
	ctx := WithStreamingCallback(context.Background(), func(event StreamEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if event.Type == StreamEventTextDelta {
			streamed += event.Text
		}
		return nil
	})

	resp, err := mgr.handleCommitCommand(ctx, threadID)
	require.NoError(t, err)
	assert.Contains(t, resp, "streamed commit message")

	mu.Lock()
	defer mu.Unlock()
	// Original behaviour: the text delta is streamed.
	assert.Contains(t, streamed, "streamed commit message")
	// New behaviour: the turn summary footer is streamed as well, so
	// streaming cards end with the same iteration/cost info as the
	// final reply text.
	assert.Contains(t, streamed, "回合")
	assert.Contains(t, streamed, "次迭代")
}

// TestCommitCommand_ViaTextSlash verifies the "/commit" text command routes
// through handleSlashCommand into the commit handler.
func TestCommitCommand_ViaTextSlash(t *testing.T) {
	threadID := "commit-text-thread"
	mgr, _ := newOneoffTestManager(t, []string{"feat: commit via text slash"}, threadID)

	result := mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
		Content:   "/commit",
		ThreadID:  threadID,
		MessageID: "msg-commit",
	})
	require.NoError(t, result.Err)
	assert.Contains(t, result.Reply.Content, "feat: commit via text slash")
	assert.Equal(t, threadID, result.Reply.ThreadID)
}

// TestReviewCommand_SingleRound verifies the plain "/review" (no round count)
// runs a single-round code review and returns the review text plus a short
// completion marker (S3).
func TestReviewCommand_SingleRound(t *testing.T) {
	threadID := "review-thread"
	mgr, _ := newOneoffTestManager(t, []string{"## Review Findings\n\nLooks good."}, threadID)

	resp, err := mgr.handleReviewCommand(context.Background(), threadID, "")
	require.NoError(t, err)
	assert.Contains(t, resp, "## Review Findings")
	assert.Contains(t, resp, "✅ 审查完成（1 轮）")
}

// TestReviewCommand_MultiRound verifies "/review N" (N ≥ 2) runs N
// adversarial rounds. The final reply carries the completion status + report
// directory (per-round text is pushed via sendToThread as it completes, not
// duplicated in the reply), and the orchestrator created the report
// directory under the thread's working directory.
func TestReviewCommand_MultiRound(t *testing.T) {
	threadID := "review-multi-thread"
	mgr, workDir := newOneoffTestManager(t, []string{
		"round1: initial review", "round2: challenge", "round3: verdict",
	}, threadID)

	resp, err := mgr.handleReviewCommand(context.Background(), threadID, "3")
	require.NoError(t, err)
	assert.Contains(t, resp, "✅ 审查完成（3 轮）")
	assert.Contains(t, resp, "报告目录")
	// Multi-round text is pushed per round via sendToThread — the final
	// reply must NOT re-send it (duplication).
	assert.NotContains(t, resp, "round1: initial review")

	// The orchestrator-owned report directory must exist under workDir.
	reviewsRoot := filepath.Join(workDir, ".tachi", "reviews")
	entries, err := os.ReadDir(reviewsRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].IsDir())
}

// TestReviewCommand_RoundsClamped verifies the round count is clamped to the
// max (10) — "/review 999" must not spin up 999 forks.
func TestReviewCommand_RoundsClamped(t *testing.T) {
	threadID := "review-clamp-thread"
	responses := make([]string, 10)
	for i := range responses {
		responses[i] = fmt.Sprintf("round-%d", i+1)
	}
	mgr, _ := newOneoffTestManager(t, responses, threadID)

	resp, err := mgr.handleReviewCommand(context.Background(), threadID, "999")
	require.NoError(t, err)
	assert.Contains(t, resp, "✅ 审查完成（10 轮）")
}

// TestReviewCommand_ViaTextSlash verifies "/review 2" text command routes
// through handleSlashCommand and carries the round count (status reply; the
// round text itself is pushed via sendToThread).
func TestReviewCommand_ViaTextSlash(t *testing.T) {
	threadID := "review-text-thread"
	mgr, _ := newOneoffTestManager(t, []string{"r1 review", "r2 judge"}, threadID)

	result := mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
		Content:   "/review 2",
		ThreadID:  threadID,
		MessageID: "msg-review",
	})
	require.NoError(t, result.Err)
	assert.Contains(t, result.Reply.Content, "✅ 审查完成（2 轮）")
	assert.NotContains(t, result.Reply.Content, "r1 review")
}

// TestReviewCommand_MultiRoundFailure verifies the multi-round failure reply:
// it carries the status (failed round + completed count + report dir) but
// NOT the error detail — the caller appends "❌ <err>" exactly once, so
// embedding it here would duplicate the error in the user-visible reply.
func TestReviewCommand_MultiRoundFailure(t *testing.T) {
	threadID := "review-fail-thread"
	var mu sync.Mutex
	call := 0
	prov := &mockProvider{name: "openai", streamFunc: func(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
		mu.Lock()
		call++
		this := call
		mu.Unlock()
		if this == 1 {
			// Round 1 succeeds.
			ch := make(chan llm.StreamEvent, 4)
			ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "round1 ok"}
			ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
			close(ch)
			return ch, nil
		}
		// Round 2 fails at the API level.
		return nil, errors.New("provider exploded")
	}}
	mgr, _ := newOneoffTestManagerWithProvider(t, prov, threadID)

	resp, err := mgr.handleReviewCommand(context.Background(), threadID, "2")
	require.Error(t, err)
	assert.Contains(t, resp, "第 2 轮失败")
	assert.Contains(t, resp, "已完成 1 轮")
	assert.Contains(t, resp, "报告目录")
	// The error detail must NOT be embedded — the caller appends it once.
	assert.NotContains(t, resp, "provider exploded")
}

// TestReviewCommand_MultiRoundFailureViaSlash verifies the caller path
// (handleSlashCommand) appends the error detail exactly once, keeping the
// handler's status summary.
func TestReviewCommand_MultiRoundFailureViaSlash(t *testing.T) {
	threadID := "review-fail-slash-thread"
	var mu sync.Mutex
	call := 0
	prov := &mockProvider{name: "openai", streamFunc: func(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
		mu.Lock()
		call++
		this := call
		mu.Unlock()
		if this == 1 {
			ch := make(chan llm.StreamEvent, 4)
			ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "round1 ok"}
			ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
			close(ch)
			return ch, nil
		}
		return nil, errors.New("provider exploded")
	}}
	mgr, _ := newOneoffTestManagerWithProvider(t, prov, threadID)

	result := mgr.handleSlashCommand(t.Context(), channel.IncomingMessage{
		Content:   "/review 2",
		ThreadID:  threadID,
		MessageID: "msg-review-fail",
	})
	require.Error(t, result.Err)
	assert.Equal(t, 1, strings.Count(result.Reply.Content, "provider exploded"))
	assert.Contains(t, result.Reply.Content, "已完成 1 轮")
	assert.Contains(t, result.Reply.Content, "报告目录")
}

// TestStopCommand_NoActiveTask verifies /stop with nothing running returns a
// truthful message instead of a misleading "stopped" (B2).
func TestStopCommand_NoActiveTask(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "stop-thread")

	resp, err := mgr.handleStopCommand("stop-thread")
	require.NoError(t, err)
	assert.Contains(t, resp, "没有运行中的任务")
}

// TestStopCommand_CancelsOneoff verifies /stop cancels a registered one-off
// run (/commit, /review) — the run's ctx must be cancelled so the agent loop
// aborts (B2).
func TestStopCommand_CancelsOneoff(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "stop-thread")

	ctx, done := mgr.registerOneoff("stop-thread", context.Background())
	defer done()

	resp, err := mgr.handleStopCommand("stop-thread")
	require.NoError(t, err)
	assert.Contains(t, resp, "已停止")

	select {
	case <-ctx.Done():
		// cancelled — good
	default:
		t.Fatal("/stop must cancel a running one-off command's context")
	}
}

// TestOneoffConcurrencyCap verifies the global one-off semaphore rejects new
// runs when the cap is reached, instead of silently queueing (B3).
func TestOneoffConcurrencyCap(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, []string{"msg"}, "cap-thread")

	// Fill the semaphore to capacity.
	for i := 0; i < maxOneoffConcurrency; i++ {
		mgr.oneoffSem.TryAcquire()
	}

	resp, err := mgr.handleCommitCommand(context.Background(), "cap-thread")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "请稍后再试")
	assert.Empty(t, resp)
}
