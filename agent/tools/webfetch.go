package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/monsterxx03/tachi/pkg/proxy"
)

const (
	webFetchMaxURLLength    = 2000
	webFetchTimeoutSec      = 60
	webFetchMaxRedirects    = 10
	webFetchMaxContentBytes = 10 * 1024 * 1024 // 10MB
	webFetchMaxReturnChars  = 100_000
	webFetchCacheTTL        = 15 * time.Minute
	webFetchMaxCacheSize    = 50 * 1024 * 1024 // 50MB
	webFetchUserAgent       = "Tachi/1.0"
)

// webFetchArgs defines the expected JSON arguments for WebFetch.
type webFetchArgs struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt,omitempty"`
}

// webFetchOutput is the JSON result returned to the LLM.
type webFetchOutput struct {
	URL         string `json:"url"`
	Bytes       int    `json:"bytes"`
	Code        int    `json:"code"`
	CodeText    string `json:"codeText"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
	DurationMs  int64  `json:"durationMs"`
}

// WebFetchTool fetches a URL and returns its content as markdown.
// HTML pages are automatically converted. It supports optional proxy
// and caches responses for 15 minutes.
type WebFetchTool struct {
	Timeout int    // HTTP request timeout in seconds (default 60)
	Proxy   string // Optional proxy URL

	clientOnce sync.Once
	httpClient *http.Client
}

func (t *WebFetchTool) Name() string        { return "WebFetch" }
func (t *WebFetchTool) Parallel() bool       { return true }
func (t *WebFetchTool) timeout() int         { if t.Timeout <= 0 { return webFetchTimeoutSec }; return t.Timeout }

func (t *WebFetchTool) Description() string {
	return "Fetches content from a specified URL and converts HTML to markdown. " +
		"Takes a URL and an optional prompt describing what to extract. " +
		"Returns the page content as markdown text. " +
		"HTTP URLs are automatically upgraded to HTTPS. " +
		"Results are cached for 15 minutes."
}

func (t *WebFetchTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"url":    {Type: "string", Description: "The URL to fetch content from"},
		"prompt": {Type: "string", Description: "Optional: describe what information you want to extract from the page"},
	}
}

func (t *WebFetchTool) Required() []string { return []string{"url"} }

func (t *WebFetchTool) getHTTPClient() *http.Client {
	t.clientOnce.Do(func() {
		c, err := proxy.NewHTTPClient(t.Proxy, time.Duration(t.timeout())*time.Second)
		if err != nil {
			c = &http.Client{Timeout: time.Duration(t.timeout()) * time.Second}
		}
		t.httpClient = c
	})
	return t.httpClient
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

type webFetchCacheEntry struct {
	content     string
	contentType string
	bytes       int
	code        int
	codeText    string
	storedAt    time.Time
	size        int
}

var (
	webFetchCacheStore = make(map[string]webFetchCacheEntry)
	webFetchCacheMu    sync.Mutex
	webFetchCacheSize  int
)

func webFetchCacheGet(key string) (webFetchCacheEntry, bool) {
	webFetchCacheMu.Lock()
	defer webFetchCacheMu.Unlock()
	e, ok := webFetchCacheStore[key]
	if !ok {
		return webFetchCacheEntry{}, false
	}
	if time.Since(e.storedAt) > webFetchCacheTTL {
		webFetchCacheSize -= e.size
		delete(webFetchCacheStore, key)
		return webFetchCacheEntry{}, false
	}
	return e, true
}

func webFetchCacheSet(key string, e webFetchCacheEntry) {
	webFetchCacheMu.Lock()
	defer webFetchCacheMu.Unlock()

	// Evict the whole cache if this insert would blow past the limit.
	// A single page is unlikely to exceed 50 MB, but be defensive.
	if webFetchCacheSize+e.size > webFetchMaxCacheSize {
		webFetchCacheStore = make(map[string]webFetchCacheEntry)
		webFetchCacheSize = 0
	}

	// Replace existing entry if present (update size delta).
	if old, ok := webFetchCacheStore[key]; ok {
		webFetchCacheSize -= old.size
	}

	webFetchCacheStore[key] = e
	webFetchCacheSize += e.size
}

// ---------------------------------------------------------------------------
// Execute
// ---------------------------------------------------------------------------

func (t *WebFetchTool) ExecuteContext(ctx context.Context, rawArgs string) (string, error) {
	var args webFetchArgs
	if err := parseArgs(rawArgs, &args); err != nil {
		return "", err
	}
	args.URL = strings.TrimSpace(args.URL)
	if args.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	// Validate.
	u, err := validateWebFetchURL(args.URL)
	if err != nil {
		return "", err
	}

	// Check cache.
	if e, ok := webFetchCacheGet(u); ok {
		out := buildWebFetchOutput(u, e, 0)
		return marshalResult(out)
	}

	start := time.Now()

	// Fetch with custom redirect handling.
	resp, err := t.fetchWithRedirects(ctx, u, 0)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	// Cross-host redirect → return a hint for the LLM to re-fetch.
	if isRedirect(resp.StatusCode) {
		out := crossHostRedirectOutput(u, resp)
		out.DurationMs = time.Since(start).Milliseconds()
		return marshalResult(out)
	}

	// Read body with size limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxContentBytes))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	bodyLen := len(body)

	contentType := resp.Header.Get("Content-Type")
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	// Convert to markdown.
	content, err := convertToMarkdown(body, contentType)
	if err != nil {
		return "", err
	}

	// Truncate for token budget.
	if len(content) > webFetchMaxReturnChars {
		content = content[:webFetchMaxReturnChars] + "\n\n[Content truncated due to length...]"
	}

	// Prepend prompt if given.
	if args.Prompt != "" {
		content = fmt.Sprintf("[WebFetch 提取指令: %s]\n\n--- 以下为网页内容 ---\n\n%s", args.Prompt, content)
	}

	// Populate cache (only for successful or non-redirect responses).
	webFetchCacheSet(u, webFetchCacheEntry{
		content:     content,
		contentType: contentType,
		bytes:       bodyLen,
		code:        resp.StatusCode,
		codeText:    resp.Status,
		storedAt:    time.Now(),
		size:        len(content),
	})

	out := buildWebFetchOutput(u, webFetchCacheEntry{
		content:     content,
		contentType: contentType,
		bytes:       bodyLen,
		code:        resp.StatusCode,
		codeText:    resp.Status,
	}, time.Since(start).Milliseconds())

	return marshalResult(out)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func validateWebFetchURL(raw string) (string, error) {
	if len(raw) > webFetchMaxURLLength {
		return "", fmt.Errorf("URL too long (max %d characters)", webFetchMaxURLLength)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}

	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported protocol %q (only http/https)", parsed.Scheme)
	}

	if parsed.User != nil {
		return "", fmt.Errorf("URLs with credentials are not supported")
	}

	hostname := parsed.Hostname()
	parts := strings.Split(hostname, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid hostname %q", hostname)
	}

	// Upgrade http → https, but not for loopback/localhost (local dev).
	if parsed.Scheme == "http" && !isLoopback(hostname) {
		parsed.Scheme = "https"
	}

	return parsed.String(), nil
}

func buildWebFetchOutput(rawURL string, e webFetchCacheEntry, durationMs int64) webFetchOutput {
	codeText := e.codeText
	// If the entry was stored with the full status line, extract just the text.
	if idx := strings.Index(codeText, " "); idx != -1 {
		codeText = strings.TrimSpace(codeText[idx:])
	}
	return webFetchOutput{
		URL:         rawURL,
		Bytes:       e.bytes,
		Code:        e.code,
		CodeText:    codeText,
		ContentType: e.contentType,
		Content:     e.content,
		DurationMs:  durationMs,
	}
}

func convertToMarkdown(body []byte, contentType string) (string, error) {
	if contentType == "text/markdown" {
		return string(body), nil
	}

	if contentType == "text/html" {
		markdown, err := md.ConvertString(string(body))
		if err != nil {
			return "", fmt.Errorf("HTML to markdown conversion failed: %w", err)
		}
		return markdown, nil
	}

	// For plain text and other text/* types, return as-is.
	if strings.HasPrefix(contentType, "text/") {
		return string(body), nil
	}

	// Binary / unknown — return a short notice.
	return fmt.Sprintf("[Binary content (%s, %d bytes) — cannot display]", contentType, len(body)), nil
}

// ---------------------------------------------------------------------------
// Redirect-aware HTTP fetch
// ---------------------------------------------------------------------------

// fetchWithRedirects performs a GET with custom redirect logic. Only
// same-host redirects (including www. addition/removal) are followed
// automatically. Cross-host redirects stop and return the redirect
// response so the tool can inform the LLM.
func (t *WebFetchTool) fetchWithRedirects(ctx context.Context, rawURL string, depth int) (*http.Response, error) {
	if depth > webFetchMaxRedirects {
		return nil, fmt.Errorf("too many redirects (max %d)", webFetchMaxRedirects)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/markdown, text/html, text/*, */*")
	httpReq.Header.Set("User-Agent", webFetchUserAgent)

	client := t.getHTTPClient()

	// Don't let the stdlib follow redirects — we handle them ourselves.
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	noRedirectClient := &http.Client{
		Transport: transport,
		Timeout:   client.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := noRedirectClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	// Follow same-host redirects.
	if isRedirect(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		if loc == "" {
			resp.Body.Close()
			return nil, fmt.Errorf("redirect missing Location header")
		}

		redirectURL, err := resolveRedirect(rawURL, loc)
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("invalid redirect location: %w", err)
		}

		if isSameHost(rawURL, redirectURL) {
			resp.Body.Close()
			return t.fetchWithRedirects(ctx, redirectURL, depth+1)
		}

		// Cross-host redirect — return the redirect response as-is.
		return resp, nil
	}

	return resp, nil
}

func isRedirect(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

func resolveRedirect(originalURL, location string) (string, error) {
	base, err := url.Parse(originalURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func isLoopback(hostname string) bool {
	return hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
}

func isSameHost(originalURL, redirectURL string) bool {
	orig, err := url.Parse(originalURL)
	if err != nil {
		return false
	}
	redir, err := url.Parse(redirectURL)
	if err != nil {
		return false
	}
	if orig.Scheme != redir.Scheme {
		return false
	}
	stripWWW := func(h string) string { return strings.TrimPrefix(h, "www.") }
	return stripWWW(orig.Hostname()) == stripWWW(redir.Hostname())
}

// ---------------------------------------------------------------------------
// Cross-host redirect output (used when the response itself is a redirect)
// ---------------------------------------------------------------------------

// crossHostRedirectOutput builds a user-friendly message when a cross-host
// redirect is encountered. The LLM can re-issue WebFetch with the new URL.
func crossHostRedirectOutput(originalURL string, resp *http.Response) webFetchOutput {
	loc := resp.Header.Get("Location")
	codeText := http.StatusText(resp.StatusCode)
	if codeText == "" {
		codeText = resp.Status
	}
	return webFetchOutput{
		URL:      originalURL,
		Bytes:    0,
		Code:     resp.StatusCode,
		CodeText: codeText,
		ContentType: "text/plain",
		Content: fmt.Sprintf(
			"REDIRECT DETECTED: The URL redirects to a different host.\n\n"+
				"Original URL: %s\nRedirect URL: %s\nStatus: %d %s\n\n"+
				"To fetch the content, please call WebFetch again with:\n  url: %q",
			originalURL, loc, resp.StatusCode, codeText, loc,
		),
	}
}
