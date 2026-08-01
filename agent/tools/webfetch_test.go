package tools

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/pkg/netutil"
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

func TestWebFetchTool_Truncation_SmallContent(t *testing.T) {
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("short content"))
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	tool := &WebFetchTool{ResultBaseDir: tmpDir}
	result, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Small content should pass through unchanged.
	if out.Content != "short content" {
		t.Errorf("expected 'short content', got %q", out.Content)
	}
}

func TestWebFetchTool_Truncation_LargeContent_FilePersistence(t *testing.T) {
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	const testMaxChars = 5000
	// Generate content that exceeds testMaxChars.
	largeContent := strings.Repeat("abcdefghij", testMaxChars/10+1) // > 5000 chars

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(largeContent))
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	tool := &WebFetchTool{ResultBaseDir: tmpDir, MaxReturnChars: testMaxChars}
	result, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Should contain the truncation marker and file path — but NOT preview content.
	if !strings.Contains(out.Content, "[WEBFETCH OUTPUT TOO LARGE]") {
		t.Error("output should contain [WEBFETCH OUTPUT TOO LARGE] marker")
	}
	if !strings.Contains(out.Content, "Use ReadFile") {
		t.Error("output should contain ReadFile instruction")
	}
	if !strings.Contains(out.Content, tmpDir) {
		t.Error("output should contain the file directory path")
	}

	// Verify a file was created.
	files, err := filepath.Glob(filepath.Join(tmpDir, "webfetch_*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 persisted file, got %d", len(files))
	}

	// Verify file content matches the original.
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != largeContent {
		t.Errorf("file content mismatch: got %d chars, want %d", len(data), len(largeContent))
	}
}

func TestWebFetchTool_Truncation_FallbackOnEmptyDir(t *testing.T) {
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	const testMaxChars = 5000
	largeContent := strings.Repeat("x", testMaxChars+1000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(largeContent))
	}))
	defer srv.Close()

	// Empty ResultBaseDir → should fall back to hard truncation.
	tool := &WebFetchTool{ResultBaseDir: "", MaxReturnChars: testMaxChars}
	result, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if !strings.Contains(out.Content, "[WEBFETCH OUTPUT TRUNCATED") {
		t.Error("fallback should use [WEBFETCH OUTPUT TRUNCATED] marker")
	}
	if len(out.Content) < testMaxChars {
		t.Errorf("truncated content should be at least %d chars, got %d", testMaxChars, len(out.Content))
	}
}

func TestWebFetchTool_CacheWithTruncation(t *testing.T) {
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	const testMaxChars = 5000
	largeContent := strings.Repeat("abcdefghij", testMaxChars/10+1)

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(largeContent))
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	tool := &WebFetchTool{ResultBaseDir: tmpDir, MaxReturnChars: testMaxChars}

	// First call — should hit server, save to file, return compact message.
	result, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 server call, got %d", callCount)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if !strings.Contains(out.Content, "[WEBFETCH OUTPUT TOO LARGE]") {
		t.Error("first call should trigger truncation")
	}

	// Second call with different prompt — should hit cache, still return compact message with correct prompt.
	result2, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`", "prompt": "extract key points"}`)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected cache hit (1 server call), got %d", callCount)
	}

	var out2 webFetchOutput
	if err := json.Unmarshal([]byte(result2), &out2); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	// Should contain the prompt marker AND the truncation marker.
	if !strings.Contains(out2.Content, "[WebFetch 提取指令: extract key points]") {
		t.Error("second call should include the new prompt")
	}
	if !strings.Contains(out2.Content, "[WEBFETCH OUTPUT TOO LARGE]") {
		t.Error("second call should also trigger truncation")
	}
}

// ---------------------------------------------------------------------------
// Firecrawl backend & reserved-address fallback
// ---------------------------------------------------------------------------

func TestWebFetchTool_SelectBackend(t *testing.T) {
	orig := netutil.LookupIP
	t.Cleanup(func() { netutil.LookupIP = orig })

	pub := func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	priv := func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }
	fail := func(host string) ([]net.IP, error) { return nil, errors.New("dns fail") }

	tests := []struct {
		name   string
		tool   WebFetchTool
		url    string
		lookup func(string) ([]net.IP, error)
		want   string
	}{
		{"zero value defaults to native", WebFetchTool{}, "https://example.com", pub, "native"},
		{"explicit native type", WebFetchTool{Type: "native", Key: "k"}, "https://example.com", pub, "native"},
		{"firecrawl without key", WebFetchTool{Type: "firecrawl"}, "https://example.com", pub, "native"},
		{"firecrawl public target", WebFetchTool{Type: "firecrawl", Key: "k"}, "https://example.com", pub, "firecrawl"},
		{"firecrawl reserved target", WebFetchTool{Type: "firecrawl", Key: "k"}, "https://example.com", priv, "native"},
		{"firecrawl dns failure", WebFetchTool{Type: "firecrawl", Key: "k"}, "https://example.com", fail, "native"},
		{"firecrawl private ip literal", WebFetchTool{Type: "firecrawl", Key: "k"}, "http://10.0.0.5/x", pub, "native"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			netutil.LookupIP = tt.lookup
			if got := tt.tool.selectBackend(tt.url); got != tt.want {
				t.Errorf("selectBackend(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestWebFetchTool_Firecrawl_ReservedFallbackToNative(t *testing.T) {
	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	firecrawlCalled := false
	fcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firecrawlCalled = true
	}))
	defer fcSrv.Close()

	// The target is a loopback address — even with type=firecrawl configured
	// the request must go through the native fetcher and reach this server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("internal content"))
	}))
	defer srv.Close()

	tool := &WebFetchTool{Type: "firecrawl", Key: "fc-test-key", BaseURL: fcSrv.URL}
	result, err := tool.ExecuteContext(t.Context(), `{"url": "`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if firecrawlCalled {
		t.Error("firecrawl backend must not be called for a reserved address")
	}
	if out.Code != 200 {
		t.Errorf("expected code 200, got %d", out.Code)
	}
	if !strings.Contains(out.Content, "internal content") {
		t.Errorf("expected native-fetched content, got: %s", out.Content)
	}
}

func TestWebFetchTool_Firecrawl_UsesFirecrawlBackend(t *testing.T) {
	orig := netutil.LookupIP
	t.Cleanup(func() { netutil.LookupIP = orig })
	netutil.LookupIP = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}

	webFetchCacheMu.Lock()
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheSize = 0
	webFetchCacheMu.Unlock()

	var gotBody firecrawlScrapeRequest
	fcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fc-test-key" {
			t.Errorf("expected 'Bearer fc-test-key' auth, got %q", got)
		}
		if got := r.URL.Path; got != "/v2/scrape" {
			t.Errorf("expected path /v2/scrape, got %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"data":{"markdown":"# Rendered by firecrawl\n\nJS content"}}`))
	}))
	defer fcSrv.Close()

	tool := &WebFetchTool{Type: "firecrawl", Key: "fc-test-key", BaseURL: fcSrv.URL}
	result, err := tool.ExecuteContext(t.Context(), `{"url": "https://example.com/page"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody.URL != "https://example.com/page" {
		t.Errorf("expected URL in request body, got %q", gotBody.URL)
	}
	if !gotBody.OnlyMainContent {
		t.Error("expected onlyMainContent=true in request body")
	}
	found := false
	for _, f := range gotBody.Formats {
		if f == "markdown" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'markdown' in formats, got %v", gotBody.Formats)
	}

	var out webFetchOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if !strings.Contains(out.Content, "Rendered by firecrawl") {
		t.Errorf("expected firecrawl markdown content, got: %s", out.Content)
	}
	if out.ContentType != "text/markdown" {
		t.Errorf("expected content type 'text/markdown', got %q", out.ContentType)
	}
	if out.Code != 200 {
		t.Errorf("expected code 200, got %d", out.Code)
	}
}

func TestWebFetchTool_Firecrawl_ErrorHint(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{http.StatusUnauthorized, "API key"},
		{http.StatusPaymentRequired, "credits"},
		{http.StatusTooManyRequests, "rate limited"},
		{http.StatusOK, ""},
	}
	for _, tt := range tests {
		if got := firecrawlErrorHint(tt.code); !strings.Contains(got, tt.want) {
			t.Errorf("firecrawlErrorHint(%d) = %q, want it to contain %q", tt.code, got, tt.want)
		}
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
