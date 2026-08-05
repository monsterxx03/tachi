package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenAIConvertMessages_BasicRoles(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
		{Role: "user", Content: "How are you?"},
	}

	out := p.convertMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}

	if out[0].Role != "user" || out[0].Content != "Hello" {
		t.Errorf("msg 0: got role=%q content=%q", out[0].Role, out[0].Content)
	}
	if out[1].Role != "assistant" || out[1].Content != "Hi there!" {
		t.Errorf("msg 1: got role=%q content=%q", out[1].Role, out[1].Content)
	}
	if out[2].Role != "user" || out[2].Content != "How are you?" {
		t.Errorf("msg 2: got role=%q content=%q", out[2].Role, out[2].Content)
	}
}

func TestOpenAIConvertMessages_SteerBecomesUser(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "tool", ToolCallID: "tc1", Content: "result"},
		{Role: RoleSteer, Content: "Continue."},
	}

	out := p.convertMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}

	// Steer should be converted to "user" role
	if out[1].Role != "user" {
		t.Errorf("steer: expected role 'user', got %q", out[1].Role)
	}
	if out[1].Content != "Continue." {
		t.Errorf("steer: expected content 'Continue.', got %q", out[1].Content)
	}
}

func TestOpenAIConvertMessages_ToolRole(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "file1.txt", Name: "bash"},
	}

	out := p.convertMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}

	if out[0].Role != "assistant" {
		t.Errorf("expected assistant role, got %q", out[0].Role)
	}
	if len(out[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(out[0].ToolCalls))
	}
	if out[0].ToolCalls[0].ID != "tc1" {
		t.Errorf("tool call ID = %q", out[0].ToolCalls[0].ID)
	}

	if out[1].Role != "tool" {
		t.Errorf("expected tool role, got %q", out[1].Role)
	}
	if out[1].ToolCallID != "tc1" {
		t.Errorf("tool_call_id = %q", out[1].ToolCallID)
	}
}

func TestOpenAIConvertMessages_InvalidToolCallArgs(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"`}}, // truncated JSON
		}},
	}

	out := p.convertMessages(msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}

	if len(out[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(out[0].ToolCalls))
	}

	// Invalid JSON should be degraded to "{}"
	if out[0].ToolCalls[0].Function.Arguments != "{}" {
		t.Errorf("expected degraded empty JSON, got %q", out[0].ToolCalls[0].Function.Arguments)
	}
}

func TestOpenAIConvertMessages_EmptyToolCallArgs(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: ""}}, // empty args
		}},
	}

	out := p.convertMessages(msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}

	// Empty string: args != "" is false, so it's passed through as-is
	if out[0].ToolCalls[0].Function.Arguments != "" {
		t.Errorf("expected empty string for empty args, got %q", out[0].ToolCalls[0].Function.Arguments)
	}
}

func TestOpenAIConvertMessages_ValidToolCallArgs(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	msgs := []Message{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls -la"}`}},
		}},
	}

	out := p.convertMessages(msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}

	// Valid JSON should be preserved
	if out[0].ToolCalls[0].Function.Arguments != `{"cmd":"ls -la"}` {
		t.Errorf("expected preserved args, got %q", out[0].ToolCalls[0].Function.Arguments)
	}
}

func TestOpenAIConvertTools(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	tools := []Tool{
		NewTool("bash", "Run a command",
			map[string]ToolParameterProperty{
				"cmd": {Type: "string", Description: "The command"},
			},
			[]string{"cmd"},
		),
		NewTool("read", "Read a file",
			map[string]ToolParameterProperty{
				"path": {Type: "string", Description: "File path"},
			},
			[]string{"path"},
		),
	}

	out := p.convertTools(tools)
	if len(out) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(out))
	}

	if out[0].Function.Name != "bash" {
		t.Errorf("tool 0 name = %q", out[0].Function.Name)
	}
	if out[1].Function.Name != "read" {
		t.Errorf("tool 1 name = %q", out[1].Function.Name)
	}
}

func TestOpenAIConvertTools_Empty(t *testing.T) {
	p := NewOpenAIProvider("key", "", "gpt-4o")
	out := p.convertTools(nil)
	if len(out) != 0 {
		t.Errorf("expected 0 tools, got %d", len(out))
	}
}

func TestTachiTransport_UserAgent(t *testing.T) {
	oldVersion := Version
	Version = "test-version"
	t.Cleanup(func() { Version = oldVersion })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "tachi/test-version" {
			t.Errorf("User-Agent = %q, want 'tachi/test-version'", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(200)
	}))
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL, nil)
	client := &http.Client{
		Transport: &tachiTransport{base: http.DefaultTransport},
	}
	_, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTachiTransport_SessionID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-tachi-session-id") != "test-session" {
			t.Errorf("x-tachi-session-id = %q, want 'test-session'", r.Header.Get("x-tachi-session-id"))
		}
		w.WriteHeader(200)
	}))
	defer server.Close()

	ctx := WithSessionID(t.Context(), "test-session")
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	client := &http.Client{
		Transport: &tachiTransport{base: http.DefaultTransport},
	}
	_, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTachiTransport_NoSessionID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("x-tachi-session-id"); v != "" {
			t.Errorf("expected no session-id header, got %q", v)
		}
		w.WriteHeader(200)
	}))
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL, nil)
	client := &http.Client{
		Transport: &tachiTransport{base: http.DefaultTransport},
	}
	_, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTachiTransport_BaseTransportFallback(t *testing.T) {
	// When http.Client has nil Transport, tachiTransport wraps http.DefaultTransport
	p := NewOpenAIProvider("key", "https://api.example.com/v1", "gpt-4o")
	if p.client == nil {
		t.Error("expected non-nil client")
	}
}

// decodeRequestBody reads and JSON-decodes the request body sent to the test
// server, returning it as a generic map for assertions on top-level fields.
func decodeRequestBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

func TestDeepSeekThinking_NonStream(t *testing.T) {
	tests := []struct {
		name         string
		opts         ChatOptions
		wantThinking string // "enabled" | "disabled" | "" (不注入 thinking 字段)
		wantEffort   string // expected reasoning_effort ("" = absent)
	}{
		{
			name:         "disabled",
			opts:         ChatOptions{MaxTokens: 100, Thinking: boolPtr(false)},
			wantThinking: "disabled",
		},
		{
			name:         "explicit enabled",
			opts:         ChatOptions{MaxTokens: 100, Thinking: boolPtr(true)},
			wantThinking: "enabled",
		},
		{
			name:         "enabled with effort",
			opts:         ChatOptions{MaxTokens: 100, ThinkingEffort: "high"},
			wantThinking: "enabled",
			wantEffort:   "high",
		},
		{
			// 未知的 effort 值不会被本地过滤——原样透传给 API，由服务端校验枚举。
			name:         "unknown effort passthrough",
			opts:         ChatOptions{MaxTokens: 100, ThinkingEffort: "turbo"},
			wantThinking: "enabled",
			wantEffort:   "turbo",
		},
		{
			// 默认（nil thinking / 空 effort）不注入 thinking 字段——
			// 交给服务端默认（DeepSeek: thinking 开启, effort high）。
			name:         "default sends no thinking field",
			opts:         ChatOptions{MaxTokens: 100},
			wantThinking: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody = decodeRequestBody(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()

			p := NewOpenAIProvider("key", server.URL, "deepseek-v4-pro")
			_, err := p.CreateChat(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, tt.opts)
			requireNoError(t, err)

			gotEffort, _ := gotBody["reasoning_effort"].(string)
			if gotEffort != tt.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q", gotEffort, tt.wantEffort)
			}

			thinking, ok := gotBody["thinking"].(map[string]any)
			if tt.wantThinking == "" {
				if ok {
					t.Errorf("request body should NOT contain thinking field: %v", gotBody)
				}
				return
			}
			if !ok {
				t.Fatalf("request body missing top-level thinking field: %v", gotBody)
			}
			if thinking["type"] != tt.wantThinking {
				t.Errorf("thinking.type = %v, want %q", thinking["type"], tt.wantThinking)
			}
		})
	}
}

func TestDeepSeekThinking_NonDeepSeekModel_UsesStandardPath(t *testing.T) {
	// 非 DeepSeek 模型不注入顶层 thinking 字段，走标准 go-openai 路径；
	// reasoning_effort 原样透传（含未知值，由服务端校验）。
	tests := []struct {
		name       string
		effort     string
		wantEffort string // "" = absent
	}{
		{"known effort", "high", "high"},
		{"unknown effort passthrough", "turbo", "turbo"},
		{"no effort", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody = decodeRequestBody(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()

			p := NewOpenAIProvider("key", server.URL, "gpt-5.4")
			_, err := p.CreateChat(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, ChatOptions{
				MaxTokens:      100,
				ThinkingEffort: tt.effort,
			})
			requireNoError(t, err)

			if _, ok := gotBody["thinking"]; ok {
				t.Errorf("non-DeepSeek request must not carry a top-level thinking field: %v", gotBody)
			}
			gotEffort, _ := gotBody["reasoning_effort"].(string)
			if gotEffort != tt.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q", gotEffort, tt.wantEffort)
			}
		})
	}
}

func TestDeepSeekThinking_Stream(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeRequestBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// 模拟 DeepSeek 流式响应：reasoning_content + content + [DONE]
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "deepseek-v4-pro")
	disabled := false
	ch, err := p.CreateChatStream(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, ChatOptions{
		MaxTokens: 100,
		Thinking:  &disabled,
	})
	requireNoError(t, err)

	thinking, ok := gotBody["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("stream request body missing thinking:disabled: %v", gotBody)
	}

	var texts []string
	var thinkings []string
	var done bool
	for ev := range ch {
		switch ev.Type {
		case StreamEventThinkingDelta:
			thinkings = append(thinkings, ev.ThinkingDelta)
		case StreamEventTextDelta:
			texts = append(texts, ev.TextDelta)
		case StreamEventDone:
			done = true
		case StreamEventError:
			t.Fatalf("stream error: %v", ev.Error)
		}
	}
	if !done {
		t.Error("expected StreamEventDone")
	}
	if len(thinkings) != 1 || thinkings[0] != "thinking..." {
		t.Errorf("thinking deltas = %v, want [thinking...]", thinkings)
	}
	if len(texts) != 2 || texts[0] != "hello" || texts[1] != " world" {
		t.Errorf("text deltas = %v, want [hello  world]", texts)
	}
}

// TestOpenAIStreamUsage_CachedTokens verifies that cache-hit tokens reported
// by OpenAI-compatible providers (e.g. DeepSeek's prompt_tokens_details.
// cached_tokens) are captured into CacheReadInputTokens, so CalculateCost
// bills them at the cache-read price instead of the full input price.
func TestOpenAIStreamUsage_CachedTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// DeepSeek-style final usage chunk (stream_options.include_usage):
		// 768 of 841 prompt tokens were cache hits.
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":841,\"completion_tokens\":16," +
				"\"total_tokens\":857,\"prompt_tokens_details\":{\"cached_tokens\":768}," +
				"\"prompt_cache_hit_tokens\":768,\"prompt_cache_miss_tokens\":73}}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", server.URL, "deepseek-v4-flash")
	ch, err := p.CreateChatStream(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, ChatOptions{MaxTokens: 100})
	requireNoError(t, err)

	var doneEv StreamEvent
	for ev := range ch {
		if ev.Type == StreamEventDone {
			doneEv = ev
			break
		}
		if ev.Type == StreamEventError {
			t.Fatalf("stream error: %v", ev.Error)
		}
	}
	if doneEv.Usage == nil {
		t.Fatal("expected usage on StreamEventDone")
	}
	if doneEv.Usage.InputTokens != 841 {
		t.Errorf("InputTokens = %d, want 841", doneEv.Usage.InputTokens)
	}
	if doneEv.Usage.OutputTokens != 16 {
		t.Errorf("OutputTokens = %d, want 16", doneEv.Usage.OutputTokens)
	}
	if doneEv.Usage.CacheReadInputTokens != 768 {
		t.Errorf("CacheReadInputTokens = %d, want 768 (cached_tokens must be parsed)", doneEv.Usage.CacheReadInputTokens)
	}
}
