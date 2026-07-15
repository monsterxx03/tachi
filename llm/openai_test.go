package llm

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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