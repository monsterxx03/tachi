package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchTool_Name(t *testing.T) {
	tool := &WebFetchTool{}
	if tool.Name() != "WebFetch" {
		t.Errorf("expected name 'WebFetch', got %q", tool.Name())
	}
}

func TestWebFetchTool_Required(t *testing.T) {
	tool := &WebFetchTool{}
	required := tool.Required()
	if len(required) != 1 || required[0] != "url" {
		t.Errorf("expected required ['url'], got %v", required)
	}
}

func TestWebFetchTool_Properties(t *testing.T) {
	tool := &WebFetchTool{}
	props := tool.Properties()
	if _, ok := props["url"]; !ok {
		t.Error("expected 'url' property")
	}
	if _, ok := props["prompt"]; !ok {
		t.Error("expected 'prompt' property")
	}
}

func TestWebFetchTool_Parallel(t *testing.T) {
	tool := &WebFetchTool{}
	if !tool.Parallel() {
		t.Error("expected Parallel() to be true")
	}
}

func TestWebFetchTool_MissingURL(t *testing.T) {
	tool := &WebFetchTool{}
	_, err := tool.ExecuteContext(t.Context(), `{}`)
	if err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestWebFetchTool_EmptyURL(t *testing.T) {
	tool := &WebFetchTool{}
	_, err := tool.ExecuteContext(t.Context(), `{"url": ""}`)
	if err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestWebFetchTool_InvalidURL(t *testing.T) {
	tool := &WebFetchTool{}
	_, err := tool.ExecuteContext(t.Context(), `{"url": "not-a-valid-url"}`)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestWebFetchTool_UnsupportedScheme(t *testing.T) {
	tool := &WebFetchTool{}
	_, err := tool.ExecuteContext(t.Context(), `{"url": "ftp://example.com/file"}`)
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

func TestWebFetchTool_HTMLConversion(t *testing.T) {
	// Clear cache to avoid interference.
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body><h1>Hello</h1><p>World</p></body></html>`))
	}))
	defer srv.Close()

	tool := &WebFetchTool{}
	result, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if out.Code != 200 {
		t.Errorf("expected code 200, got %d", out.Code)
	}
	if out.ContentType != "text/html" {
		t.Errorf("expected content type 'text/html', got %q", out.ContentType)
	}
	// HTML should be converted to markdown: # Hello\n\nWorld
	if !strings.Contains(out.Content, "Hello") || !strings.Contains(out.Content, "World") {
		t.Errorf("expected markdown content to contain 'Hello' and 'World', got: %s", out.Content)
	}
}

func TestWebFetchTool_WithPrompt(t *testing.T) {
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("plain text"))
	}))
	defer srv.Close()

	tool := &WebFetchTool{}
	result, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`", "prompt": "extract key points"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if !strings.Contains(out.Content, "[WebFetch 提取指令: extract key points]") {
		t.Errorf("expected prompt marker in content, got: %s", out.Content)
	}
}

func TestWebFetchTool_Cache(t *testing.T) {
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("cached content"))
	}))
	defer srv.Close()

	tool := &WebFetchTool{}

	// First call — should hit the server.
	_, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 server call, got %d", callCount)
	}

	// Second call — should hit the cache.
	_, err = tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected cache hit (1 server call), got %d", callCount)
	}
}

func TestValidateWebFetchURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"simple https", "https://example.com", "https://example.com", false},
		{"http upgrade", "http://example.com", "https://example.com", false},
		{"with path", "https://example.com/path?a=1", "https://example.com/path?a=1", false},
		{"single label hostname", "https://localhost", "", true},
		{"with user", "https://user:pass@example.com", "", true},
		{"ftp scheme", "ftp://example.com", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateWebFetchURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got URL %q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsSameHost(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"https://example.com/a", "https://example.com/b", true},
		{"https://example.com", "https://www.example.com", true},
		{"https://www.example.com", "https://example.com", true},
		{"https://example.com", "https://other.com", false},
		{"http://example.com", "https://example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.a+"↔"+tt.b, func(t *testing.T) {
			if got := isSameHost(tt.a, tt.b); got != tt.want {
				t.Errorf("isSameHost(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
