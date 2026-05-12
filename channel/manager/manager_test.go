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
	ctx, cancel := context.WithCancel(context.Background())

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

	ctx := context.Background()
	out, err := handler(ctx, msg)

	// Expect error: initProvider was never called, so provider is nil.
	assert.Error(t, err)
	// Handler still returns a structured OutgoingMessage with error text.
	assert.Equal(t, "thread-1", out.ThreadID)
	assert.Equal(t, "msg-1", out.ReplyTo)
	assert.Contains(t, out.Content, "❌")
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
		context.Background(),
		nil,
		"test message",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(eventCh, aiAgent)
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
		context.Background(),
		nil,
		"edit something",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(eventCh, aiAgent)
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
		context.Background(),
		nil,
		"ask me something",
		"system prompt",
		llm.ChatOptions{MaxTokens: 4096},
	)

	result, err := mgr.drainEvents(eventCh, aiAgent)
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

func newInt64Ptr(v int64) *int64 { return &v }
