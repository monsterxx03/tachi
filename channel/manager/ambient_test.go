package manager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/channel"
)

func TestWhisperGuard_NonDirectedGroupChat(t *testing.T) {
	// Non-directed group chat message with whisper enabled → should be buffered.
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SessionStore: newTempSessionStore(t),
	})

	handler := mgr.buildHandler()

	result := handler(t.Context(), channel.IncomingMessage{
		ThreadID:  "wave:group:gc_123",
		MessageID: "msg-1",
		Content:   "someone said something",
		Sender:    "张三",
		Directed:  false,
		GroupChat: true,
	})

	assert.True(t, result.Buffered, "non-directed group message should be buffered")
	assert.False(t, result.Steered)
}

func TestWhisperGuard_DirectedGroupChat(t *testing.T) {
	// Directed group chat message → should NOT be buffered (goes through normal turn).
	cfg := config.DefaultConfig()
	cfg.Providers = []config.ProviderConfig{{
		Name:    "test",
		Type:    "openai",
		Model:   "test-model",
		APIKey:  "fake-key",
		BaseURL: "http://localhost:1234",
	}}
	cfg.Provider = "test"
	mgr := New(Config{
		Cfg:          cfg,
		SessionStore: newTempSessionStore(t),
	})

	handler := mgr.buildHandler()

	result := handler(t.Context(), channel.IncomingMessage{
		ThreadID:  "wave:group:gc_123",
		MessageID: "msg-1",
		Content:   "@bot help me",
		Sender:    "张三",
		Directed:  true,
		GroupChat: true,
	})

	// Directed message should NOT be buffered — it enters the normal agent turn path.
	// Since we don't have a real provider, it will error, but it should not be Buffered.
	assert.False(t, result.Buffered)
	assert.False(t, result.Steered)
}

func TestWhisperGuard_NonGroupChat(t *testing.T) {
	// Non-group-chat message with Directed=false → should NOT be buffered.
	// (Only group chat messages go through the ambient pipeline.)
	cfg := config.DefaultConfig()
	cfg.Providers = []config.ProviderConfig{{
		Name:    "test",
		Type:    "openai",
		Model:   "test-model",
		APIKey:  "fake-key",
		BaseURL: "http://localhost:1234",
	}}
	cfg.Provider = "test"
	mgr := New(Config{
		Cfg:          cfg,
		SessionStore: newTempSessionStore(t),
	})

	handler := mgr.buildHandler()

	result := handler(t.Context(), channel.IncomingMessage{
		ThreadID:  "wave:user:u_123",
		MessageID: "msg-1",
		Content:   "hello",
		Sender:    "张三",
		Directed:  false,
		GroupChat: false,
	})

	// Without GroupChat=true, whisper guard doesn't trigger.
	assert.False(t, result.Buffered)
}

func TestWhisperGuard_Disabled(t *testing.T) {
	// Whisper disabled → non-directed group messages should NOT be buffered.
	cfg := config.DefaultConfig()
	enabled := false
	cfg.Channel.Whisper.Enabled = &enabled
	cfg.Providers = []config.ProviderConfig{{
		Name:    "test",
		Type:    "openai",
		Model:   "test-model",
		APIKey:  "fake-key",
		BaseURL: "http://localhost:1234",
	}}
	cfg.Provider = "test"
	mgr := New(Config{
		Cfg:          cfg,
		SessionStore: newTempSessionStore(t),
	})

	handler := mgr.buildHandler()

	result := handler(t.Context(), channel.IncomingMessage{
		ThreadID:  "wave:group:gc_123",
		MessageID: "msg-1",
		Content:   "someone said something",
		Sender:    "张三",
		Directed:  false,
		GroupChat: true,
	})

	assert.False(t, result.Buffered, "whisper disabled: should not buffer")
}

func TestAmbientBuffer_Batching(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SessionStore: newTempSessionStore(t),
	})

	handler := mgr.buildHandler()

	// Send 3 non-directed messages
	for i := range 3 {
		result := handler(t.Context(), channel.IncomingMessage{
			ThreadID:  "wave:group:gc_456",
			MessageID: "msg-" + string(rune('a'+i)),
			Content:   "message " + string(rune('a'+i)),
			Sender:    "user" + string(rune('1'+i)),
			Directed:  false,
			GroupChat: true,
		})
		assert.True(t, result.Buffered)
	}

	// Verify buffer has all 3 messages
	ta, ok := mgr.threadActivations.Load("wave:group:gc_456")
	require.True(t, ok)
	require.NotNil(t, ta)

	ta.mu.Lock()
	assert.Equal(t, 3, len(ta.ambientPending))
	assert.True(t, ta.groupChat)
	assert.NotNil(t, ta.ambientTimer)
	ta.mu.Unlock()
}

func TestAmbientBuffer_Cap(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Channel.Whisper.AmbientMaxBuffer = 3 // Very small cap for testing
	mgr := New(Config{
		Cfg:          cfg,
		SessionStore: newTempSessionStore(t),
	})

	handler := mgr.buildHandler()

	// Send 5 messages (exceeds cap of 3)
	for i := range 5 {
		handler(t.Context(), channel.IncomingMessage{
			ThreadID:  "wave:group:gc_cap",
			MessageID: "msg-" + string(rune('a'+i)),
			Content:   "message " + string(rune('0'+i)),
			Sender:    "user",
			Directed:  false,
			GroupChat: true,
		})
	}

	ta, ok := mgr.threadActivations.Load("wave:group:gc_cap")
	require.True(t, ok)
	require.NotNil(t, ta)

	ta.mu.Lock()
	// Should only have the most recent 3 messages (FIFO drop)
	assert.Equal(t, 3, len(ta.ambientPending))
	assert.Equal(t, "message 2", ta.ambientPending[0].content)
	assert.Equal(t, "message 3", ta.ambientPending[1].content)
	assert.Equal(t, "message 4", ta.ambientPending[2].content)
	ta.mu.Unlock()
}

func TestAmbientSteer_ActiveTurn(t *testing.T) {
	// When an agent turn is active, ambient messages should be marked as Steered.
	cfg := config.DefaultConfig()
	mgr := New(Config{
		Cfg:          cfg,
		SessionStore: newTempSessionStore(t),
	})

	// Manually set up a thread with an active turn.
	ta := &threadActivation{
		steerRespCh: make(chan string, 1),
		resultCh:    make(chan handlerResult, 1),
		groupChat:   true,
	}
	ta.ctx, ta.cancel = t.Context(), func() {}
	mgr.threadActivations.Store("wave:group:gc_active", ta)

	handler := mgr.buildHandler()

	result := handler(t.Context(), channel.IncomingMessage{
		ThreadID:  "wave:group:gc_active",
		MessageID: "msg-ambient",
		Content:   "ambient message",
		Sender:    "张三",
		Directed:  false,
		GroupChat: true,
	})

	// When turn is active, ambient messages are injected via steer (Steered=true).
	assert.True(t, result.Steered)
	assert.False(t, result.Buffered)

	ta.mu.Lock()
	assert.Equal(t, 1, len(ta.ambientPending))
	assert.Equal(t, "ambient message", ta.ambientPending[0].content)
	ta.mu.Unlock()
}

func TestFormatAmbientForSteer(t *testing.T) {
	msgs := []ambientMsg{
		{content: "hello", sender: "张三", timestamp: time.Now()},
		{content: "world", sender: "李四", timestamp: time.Now()},
	}

	result := formatAmbientForSteer(msgs)

	assert.Contains(t, result, "--- BEGIN AMBIENT GROUP CHAT (UNTRUSTED) ---")
	assert.Contains(t, result, "[群聊] 张三: hello")
	assert.Contains(t, result, "[群聊] 李四: world")
	assert.Contains(t, result, "--- END AMBIENT GROUP CHAT ---")
}

func TestBuildAmbientPrompt(t *testing.T) {
	history := []ambientMsg{
		{content: "早上好", sender: "张三", timestamp: time.Date(2026, 6, 18, 14, 29, 0, 0, time.Local)},
		{content: "早！", sender: "Tachi", timestamp: time.Date(2026, 6, 18, 14, 29, 10, 0, time.Local)},
	}
	msgs := []ambientMsg{
		{content: "CI failed again", sender: "张三", timestamp: time.Date(2026, 6, 18, 14, 30, 0, 0, time.Local)},
		{content: "I'll look into it", sender: "李四", timestamp: time.Date(2026, 6, 18, 14, 30, 15, 0, time.Local)},
	}

	result := buildAmbientPrompt(history, msgs)

	// Should include history section
	assert.Contains(t, result, "--- PREVIOUS AMBIENT CONVERSATION (UNTRUSTED) ---")
	assert.Contains(t, result, "张三: 早上好")
	assert.Contains(t, result, "Tachi: 早！")
	assert.Contains(t, result, "--- END PREVIOUS AMBIENT ---")
	// Should include current batch section
	assert.Contains(t, result, "--- CURRENT AMBIENT MESSAGES (UNTRUSTED) ---")
	assert.Contains(t, result, "张三: CI failed again")
	assert.Contains(t, result, "李四: I'll look into it")
	assert.Contains(t, result, "--- END CURRENT AMBIENT ---")
}

func TestBuildAmbientPrompt_NoHistory(t *testing.T) {
	msgs := []ambientMsg{
		{content: "hello", sender: "张三", timestamp: time.Now()},
	}

	result := buildAmbientPrompt(nil, msgs)

	// Without history, only the current batch section should appear
	assert.NotContains(t, result, "--- PREVIOUS AMBIENT CONVERSATION ---")
	assert.Contains(t, result, "--- CURRENT AMBIENT MESSAGES (UNTRUSTED) ---")
	assert.Contains(t, result, "张三: hello")
}

func TestIsSilence(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := New(Config{Cfg: cfg})

	assert.True(t, mgr.isSilence("[SILENT]"))
	assert.True(t, mgr.isSilence("[silent]"))
	assert.True(t, mgr.isSilence("[Silent]"))
	assert.True(t, mgr.isSilence("  [SILENT]  "))
	assert.True(t, mgr.isSilence("  [silent]\n"))
	assert.True(t, mgr.isSilence("[SILENT] nothing to add"))
	assert.True(t, mgr.isSilence("没什么要说的，[SILENT]"))
	assert.False(t, mgr.isSilence("I think we should help"))
	assert.False(t, mgr.isSilence(""))
}

func TestChannelWhisperConfig_Defaults(t *testing.T) {
	// defaults are applied via creasty/defaults when loaded through DefaultConfig()
	cfg := config.DefaultConfig()

	assert.True(t, cfg.Channel.Whisper.WhisperEnabled())
	assert.Equal(t, "[SILENT]", cfg.Channel.Whisper.SilenceMarker)
	assert.Equal(t, 30*time.Second, cfg.Channel.Whisper.AmbientBatchWindow)
	assert.Equal(t, 5, cfg.Channel.Whisper.AmbientMaxIterations)
	assert.Equal(t, 50, cfg.Channel.Whisper.AmbientMaxBuffer)
	assert.Empty(t, cfg.Channel.Whisper.AmbientTools)
	assert.Equal(t, 0, cfg.Channel.Whisper.AmbientMaxTokens) // zero; fallback is agent.DefaultMaxTokens in code
}

func TestChannelWhisperConfig_CustomValues(t *testing.T) {
	enabled := false
	cfg := config.ChannelWhisperConfig{
		Enabled:              &enabled,
		AmbientBatchWindow:   10 * time.Second,
		AmbientMaxIterations: 3,
		AmbientMaxBuffer:     20,
		SilenceMarker:        "SKIP",
		AmbientTools:         []string{"MemoryRecall", "MemoryRecord"},
		AmbientMaxTokens:     2048,
	}

	assert.False(t, cfg.WhisperEnabled())
	assert.Equal(t, 10*time.Second, cfg.AmbientBatchWindow)
	assert.Equal(t, 3, cfg.AmbientMaxIterations)
	assert.Equal(t, 20, cfg.AmbientMaxBuffer)
	assert.Equal(t, "SKIP", cfg.SilenceMarker)
	assert.Equal(t, []string{"MemoryRecall", "MemoryRecord"}, cfg.AmbientTools)
	assert.Equal(t, 2048, cfg.AmbientMaxTokens)
}
