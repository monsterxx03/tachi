package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	md "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/netutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

const (
	webFetchMaxURLLength    = 2000
	webFetchMaxRedirects    = 10
	webFetchMaxContentBytes = 10 * 1024 * 1024 // 10MB
	webFetchCacheTTL        = 15 * time.Minute
	webFetchMaxCacheSize    = 50 * 1024 * 1024 // 50MB
	webFetchUserAgent       = "Tachi/1.0"

	// firecrawlDefaultBaseURL is the hosted Firecrawl API endpoint, used
	// when WebFetchConfig.BaseURL is empty (e.g. self-hosted instances).
	firecrawlDefaultBaseURL = "https://api.firecrawl.dev"
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

// webFetchFetchResult is a backend-agnostic fetch outcome. Both the native
// fetcher and the Firecrawl backend produce one, so caching, truncation and
// output shaping stay identical regardless of backend.
type webFetchFetchResult struct {
	content     string
	contentType string
	bytes       int
	code        int
	codeText    string
}

// webFetchBackend is a single fetch backend. Implementations are stateless
// and safe for concurrent use. Errors are classified via *searchErr so the
// caller can decide whether to pause the backend (quota exhausted), skip it
// (rate limit) or just fall through (other failures).
type webFetchBackend interface {
	// Type is the backend name surfaced in results, logs and pause state
	// (config.WebFetchProviderFirecrawl, config.WebFetchProviderNative).
	Type() string
	// Fetch fetches the URL, returning the processed result. A nil
	// *searchErr means success.
	Fetch(ctx context.Context, client *http.Client, u string) (webFetchFetchResult, *searchErr)
}

// WebFetchToolConfig carries the tool's construction options.
type WebFetchToolConfig struct {
	Providers      []config.WebFetchProviderConfig
	Timeout        time.Duration
	Proxy          string
	ResultBaseDir  string // Directory for oversized result files (default: ~/.tachi/tool_results)
	MaxReturnChars int    // Max chars returned to LLM; 0 = no limit
}

// WebFetchTool fetches a URL and returns its content as markdown.
// HTML pages are automatically converted. It supports optional proxy,
// caches responses for 15 minutes, and saves oversized results to disk.
//
// Backends are tried in priority order (see Providers): configured
// API-keyed backends (firecrawl) first — only for targets that resolve to a
// non-reserved address — and the built-in native fetcher always as the final
// fallback. Native needs no configuration and never pauses. When a backend's
// quota/credits are exhausted (HTTP 402) it is paused until the next billing
// cycle; rate limits (HTTP 429) only skip to the next backend. Fallbacks are
// silent to the user (WARN logs only).
type WebFetchTool struct {
	Providers      []config.WebFetchProviderConfig
	Timeout        time.Duration // HTTP request timeout (default 60s)
	Proxy          string        // Optional proxy URL
	ResultBaseDir  string        // Directory for oversized result files
	MaxReturnChars int           // Max chars returned to LLM; 0 = no limit

	initOnce sync.Once
	backends []webFetchBackend // lazily built by init(); nil until first use
	pause    *pauseStore       // lazily built by init() unless pre-set (tests)
	client   *http.Client      // lazily built by init()
}

// NewWebFetchTool builds a WebFetchTool from config.
func NewWebFetchTool(cfg WebFetchToolConfig) *WebFetchTool {
	return &WebFetchTool{
		Providers:      cfg.Providers,
		Timeout:        cfg.Timeout,
		Proxy:          cfg.Proxy,
		ResultBaseDir:  cfg.ResultBaseDir,
		MaxReturnChars: cfg.MaxReturnChars,
	}
}

// init builds and caches all derived state exactly once (thread-safe via
// sync.Once): the configured backend list, the pause store, and the HTTP
// client. A pre-set pause store (tests) is respected.
func (t *WebFetchTool) init() {
	t.initOnce.Do(func() {
		t.backends = t.buildBackends()
		if t.pause == nil {
			t.pause = &pauseStore{path: config.WebFetchPausePath()}
		}
		t.client = httpx.NewClient(t.Timeout, t.Proxy)
	})
}

// backendList returns the cached configured backend list (firecrawl
// providers; native is appended per-request as the final fallback).
func (t *WebFetchTool) backendList() []webFetchBackend {
	t.init()
	return t.backends
}

// pauseStore returns the pause store used by this tool.
func (t *WebFetchTool) pauseStore() *pauseStore {
	t.init()
	return t.pause
}

// getHTTPClient returns the cached *http.Client, reused across every fetch
// call to preserve connection pooling and proxy connections.
func (t *WebFetchTool) getHTTPClient() *http.Client {
	t.init()
	return t.client
}

func (t *WebFetchTool) Name() string   { return ToolNameWebFetch }
func (t *WebFetchTool) Parallel() bool { return true }

func (t *WebFetchTool) Description() string {
	return "Fetches content from a URL and converts HTML to markdown. " +
		"Takes a URL and an optional prompt describing what to extract. " +
		"Results are cached for 15 minutes."
}

func (t *WebFetchTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"url":    {Type: "string", Description: "The URL to fetch content from"},
		"prompt": {Type: "string", Description: "Optional: describe what information you want to extract from the page"},
	}
}

func (t *WebFetchTool) Required() []string { return []string{"url"} }

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

	backends := t.activeBackends(u)
	if len(backends) == 0 {
		return "", fmt.Errorf("no web fetch backends available")
	}

	return t.fetchWithBackends(ctx, args, u, backends)
}

// fetchWithBackends tries each backend in priority order. Cache hits and
// successful fetches are logged with the serving backend; quota errors
// (errQuota) pause the backend until the next billing cycle, rate limits
// (errRateLimit) and other failures just fall through to the next backend.
// An error is returned only when every backend fails.
func (t *WebFetchTool) fetchWithBackends(ctx context.Context, args webFetchArgs, u string, backends []webFetchBackend) (string, error) {
	client := t.getHTTPClient()
	var failures []string

	for _, b := range backends {
		// The cache key includes the backend because firecrawl returns
		// JS-rendered content, which differs from the raw-HTML markdown of
		// the native fetcher — the two must never share entries.
		cacheKey := b.Type() + ":" + u

		// Cache hit — same shaping as a fresh fetch (truncation + prompt).
		if e, ok := webFetchCacheGet(cacheKey); ok {
			logger.FromContext(ctx).Info(ctx, "WebFetch: provider served cache hit",
				"provider", b.Type(), "url", u)
			return t.shapeOutput(ctx, u, e, args.Prompt, 0)
		}

		start := time.Now()
		res, serr := b.Fetch(ctx, client, u)
		if serr == nil {
			logger.FromContext(ctx).Info(ctx, "WebFetch: provider served fetch",
				"provider", b.Type(), "url", u,
				"duration_ms", time.Since(start).Milliseconds())

			// Cache the full markdown content (before prompt / truncation).
			webFetchCacheSet(cacheKey, webFetchCacheEntry{
				content:     res.content,
				contentType: res.contentType,
				bytes:       res.bytes,
				code:        res.code,
				codeText:    res.codeText,
				storedAt:    time.Now(),
				size:        len(res.content),
			})

			return t.shapeOutput(ctx, u, webFetchCacheEntry{
				content:     res.content,
				contentType: res.contentType,
				bytes:       res.bytes,
				code:        res.code,
				codeText:    res.codeText,
			}, args.Prompt, time.Since(start).Milliseconds())
		}

		switch serr.kind {
		case errQuota:
			resume := nextBillingCycle(time.Now())
			t.pauseStore().pause(b.Type(), serr.message, resume)
			logger.FromContext(ctx).Warn(ctx, "WebFetch: provider quota exhausted, pausing until next billing cycle",
				"provider", b.Type(), "resume_after", resume.Format(time.RFC3339), "reason", serr.message)
		case errRateLimit:
			logger.FromContext(ctx).Warn(ctx, "WebFetch: provider rate limited, falling back to next backend",
				"provider", b.Type(), "reason", serr.message)
		default:
			logger.FromContext(ctx).Warn(ctx, "WebFetch: provider failed, falling back to next backend",
				"provider", b.Type(), "reason", serr.message)
		}
		failures = append(failures, fmt.Sprintf("%s: %s", b.Type(), serr.message))
	}

	return "", fmt.Errorf("all web fetch backends failed: %s", strings.Join(failures, "; "))
}

// ---------------------------------------------------------------------------
// Backend selection & fallback
// ---------------------------------------------------------------------------

// buildBackends materializes the configured backend list in priority order.
// Only API-keyed backends are built here; the native fetcher is appended
// per-request as the final fallback and needs no configuration.
func (t *WebFetchTool) buildBackends() []webFetchBackend {
	var out []webFetchBackend
	for _, pc := range t.Providers {
		typ := strings.ToLower(pc.Type)
		if typ == "" {
			typ = config.WebFetchProviderFirecrawl // default backend
		}
		switch typ {
		case config.WebFetchProviderFirecrawl:
			if pc.Key != "" {
				out = append(out, &firecrawlBackend{apiKey: pc.Key, baseURL: pc.BaseURL})
			}
		}
	}
	return out
}

// reservedTarget reports whether a URL resolves to a reserved address —
// private IPs, loopback, link-local, cloud metadata, documentation ranges.
// Such targets must never be sent to an external service, so firecrawl is
// skipped for them and the native fetcher is used instead.
func reservedTarget(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true // conservative: treat as reserved
	}
	reserved, err := netutil.HostIsReserved(parsed.Hostname())
	if err != nil {
		// Resolution failed — conservative fallback keeps behavior
		// identical to the pre-firecrawl world (native reports the error).
		return true
	}
	return reserved
}

// activeBackends returns the backends usable for a URL, in priority order:
// configured firecrawl backends that are not quota-paused, followed by the
// built-in native fetcher as the final fallback. Reserved targets (private
// IPs, loopback, ...) short-circuit straight to native — such addresses must
// never reach an external service, so no pause state is consulted.
func (t *WebFetchTool) activeBackends(rawURL string) []webFetchBackend {
	if reservedTarget(rawURL) {
		return []webFetchBackend{&nativeBackend{}}
	}

	var out []webFetchBackend
	paused := t.pauseStore().pausedProviders(time.Now())
	for _, b := range t.backendList() {
		if _, isPaused := paused[b.Type()]; isPaused {
			continue
		}
		out = append(out, b)
	}
	// Native always falls back at the end.
	out = append(out, &nativeBackend{})
	return out
}

// ---------------------------------------------------------------------------
// Native backend (built-in fallback)
// ---------------------------------------------------------------------------

// nativeBackend performs the original local HTTP fetch: same-host redirect
// following, HTML→markdown conversion, and cross-host redirect hints. It
// needs no API key, cannot hit quota limits, and is therefore never paused.
type nativeBackend struct{}

func (b *nativeBackend) Type() string { return config.WebFetchProviderNative }

func (b *nativeBackend) Fetch(ctx context.Context, client *http.Client, u string) (webFetchFetchResult, *searchErr) {
	// Fetch with custom redirect handling.
	resp, err := fetchWithRedirects(ctx, client, u, 0)
	if err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: fmt.Sprintf("fetch failed: %v", err)}
	}
	defer resp.Body.Close()

	// Cross-host redirect → return a hint for the LLM to re-fetch.
	if isRedirect(resp.StatusCode) {
		out := crossHostRedirectOutput(u, resp)
		return webFetchFetchResult{
			content:     out.Content,
			contentType: out.ContentType,
			bytes:       out.Bytes,
			code:        out.Code,
			codeText:    out.CodeText,
		}, nil
	}

	// Read body with size limit.
	body, _, err := httpx.ReadAllLimitedLenient(resp.Body, webFetchMaxContentBytes)
	if err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: fmt.Sprintf("read body: %v", err)}
	}
	bodyLen := len(body)

	contentType := resp.Header.Get("Content-Type")
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	// Convert to markdown.
	content, err := convertToMarkdown(body, contentType)
	if err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: err.Error()}
	}

	return webFetchFetchResult{
		content:     content,
		contentType: contentType,
		bytes:       bodyLen,
		code:        resp.StatusCode,
		codeText:    resp.Status,
	}, nil
}

// ---------------------------------------------------------------------------
// Firecrawl backend
// ---------------------------------------------------------------------------

// firecrawlScrapeRequest is the request body for Firecrawl POST /v2/scrape.
type firecrawlScrapeRequest struct {
	URL             string   `json:"url"`
	Formats         []string `json:"formats"`
	OnlyMainContent bool     `json:"onlyMainContent"`
}

// firecrawlScrapeResponse is the response body for Firecrawl POST /v2/scrape.
type firecrawlScrapeResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Markdown string `json:"markdown"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// firecrawlBackend fetches URLs through the Firecrawl scrape API and
// returns the cleaned markdown. The caller (activeBackends) has already
// validated the target as a non-reserved public address.
type firecrawlBackend struct {
	apiKey  string
	baseURL string
}

func (b *firecrawlBackend) Type() string { return config.WebFetchProviderFirecrawl }

func (b *firecrawlBackend) Fetch(ctx context.Context, client *http.Client, u string) (webFetchFetchResult, *searchErr) {
	base := strings.TrimSuffix(b.baseURL, "/")
	if base == "" {
		base = firecrawlDefaultBaseURL
	}

	payload, err := json.Marshal(firecrawlScrapeRequest{
		URL:             u,
		Formats:         []string{"markdown"},
		OnlyMainContent: true,
	})
	if err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: fmt.Sprintf("marshal firecrawl request: %v", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", base+"/v2/scrape", bytes.NewReader(payload))
	if err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: err.Error()}
	}
	defer resp.Body.Close()

	respBody, _, err := httpx.ReadAllLimitedLenient(resp.Body, webFetchMaxContentBytes)
	if err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: fmt.Sprintf("read firecrawl response: %v", err)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Classify quota/rate-limit errors so the caller can pause or fall
		// back: HTTP 402 = credits exhausted → pause until next billing
		// cycle; HTTP 429 = transient rate limit → skip; other failures →
		// ordinary fallback.
		serr := classifyError(resp.StatusCode, string(respBody), resp.Header)
		serr.message = fmt.Sprintf("firecrawl API returned %s%s: %s",
			resp.Status, firecrawlErrorHint(resp.StatusCode), strutil.Truncate(string(respBody), 500))
		return webFetchFetchResult{}, serr
	}

	var parsed firecrawlScrapeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return webFetchFetchResult{}, &searchErr{kind: errOther, message: fmt.Sprintf("parse firecrawl response: %v", err)}
	}

	if parsed.Data.Markdown == "" {
		return webFetchFetchResult{}, &searchErr{kind: errOther,
			message: fmt.Sprintf("firecrawl returned no markdown content (success=%v, error=%q)", parsed.Success, parsed.Error)}
	}

	return webFetchFetchResult{
		content:     parsed.Data.Markdown,
		contentType: "text/markdown",
		bytes:       len(parsed.Data.Markdown),
		code:        resp.StatusCode,
		codeText:    resp.Status,
	}, nil
}

// firecrawlErrorHint appends a human-readable hint for common Firecrawl
// HTTP status codes so the LLM can act on the failure.
func firecrawlErrorHint(code int) string {
	switch code {
	case http.StatusUnauthorized:
		return " (API key invalid — check web_fetch.key)"
	case http.StatusPaymentRequired:
		return " (insufficient Firecrawl credits)"
	case http.StatusTooManyRequests:
		return " (rate limited — retry later)"
	}
	return ""
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
	if parsed.Scheme == "http" && !netutil.IsLoopbackHost(hostname) {
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
func fetchWithRedirects(ctx context.Context, client *http.Client, rawURL string, depth int) (*http.Response, error) {
	if depth > webFetchMaxRedirects {
		return nil, fmt.Errorf("too many redirects (max %d)", webFetchMaxRedirects)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/markdown, text/html, text/*, */*")
	httpReq.Header.Set("User-Agent", webFetchUserAgent)

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
			return fetchWithRedirects(ctx, client, redirectURL, depth+1)
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
		URL:         originalURL,
		Bytes:       0,
		Code:        resp.StatusCode,
		CodeText:    codeText,
		ContentType: "text/plain",
		Content: fmt.Sprintf(
			"REDIRECT DETECTED: The URL redirects to a different host.\n\n"+
				"Original URL: %s\nRedirect URL: %s\nStatus: %d %s\n\n"+
				"To fetch the content, please call WebFetch again with:\n  url: %q",
			originalURL, loc, resp.StatusCode, codeText, loc,
		),
	}
}

// ---------------------------------------------------------------------------
// File-based truncation for oversized WebFetch results
// ---------------------------------------------------------------------------

// shapeOutput applies the shared result shaping for a fetched or cached
// entry — truncation (file-based) + prompt prefix — and builds the final
// marshaled output. Used by both the cache-hit and fresh-fetch paths so the
// two can never drift.
func (t *WebFetchTool) shapeOutput(ctx context.Context, u string, e webFetchCacheEntry, prompt string, durationMs int64) (string, error) {
	// Apply file-based truncation if content exceeds the limit.
	content := t.truncateWebFetchOutput(ctx, e.content, u)

	// Prepend prompt if given.
	if prompt != "" {
		content = fmt.Sprintf("[WebFetch 提取指令: %s]\n\n--- 以下为网页内容 ---\n\n%s", prompt, content)
	}

	out := buildWebFetchOutput(u, webFetchCacheEntry{
		content:     content,
		contentType: e.contentType,
		bytes:       e.bytes,
		code:        e.code,
		codeText:    e.codeText,
	}, durationMs)
	return marshalResult(out)
}

// truncateWebFetchOutput checks if the fetched content exceeds
// MaxReturnChars and, if so, saves the full output to disk and
// returns a compact message with the file path so the LLM can read more
// via ReadFile — no preview content is included to avoid context bloat.
//
// When MaxReturnChars <= 0, the result is returned unchanged (no limit).
// When ResultBaseDir is empty, falls back to a simple inline truncation.
func (t *WebFetchTool) truncateWebFetchOutput(ctx context.Context, content string, rawURL string) string {
	maxChars := t.MaxReturnChars
	// Rune semantics throughout: maxChars is a character count, not bytes.
	if maxChars <= 0 || utf8.RuneCountInString(content) <= maxChars {
		return content
	}

	// Fall back to simple truncation when no storage directory is configured.
	if t.ResultBaseDir == "" {
		return hardTruncateWebFetch(content, maxChars)
	}

	// Generate a deterministic filename based on URL + timestamp.
	safeName := sanitizeURLForFilename(rawURL)
	filename := fmt.Sprintf("webfetch_%s_%d.txt", safeName, time.Now().UnixNano())
	filepath := filepath.Join(t.ResultBaseDir, filename)

	// Write the full result to disk.
	if err := fileutil.WriteFilePrivate(filepath, []byte(content)); err != nil {
		logger.FromContext(ctx).Error(ctx, "WebFetch: truncateWebFetchOutput: failed to write file", err, "path", filepath)
		return hardTruncateWebFetch(content, maxChars)
	}

	logger.FromContext(ctx).Info(ctx, "WebFetch: result too large, saved to file", "char_count", utf8.RuneCountInString(content), "path", filepath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "[WEBFETCH OUTPUT TOO LARGE] Full output (%d chars) exceeds limit (%d chars).\n",
		utf8.RuneCountInString(content), maxChars)
	fmt.Fprintf(&sb, "Use ReadFile to read the full output from:\n  %s",
		filepath)
	return sb.String()
}

// hardTruncateWebFetch performs a simple truncation without file persistence.
// Used as fallback when ResultBaseDir is empty or file I/O fails. Truncation
// is rune-safe (strutil) so multi-byte characters are never split mid-sequence.
func hardTruncateWebFetch(content string, maxChars int) string {
	truncated := strutil.TruncatePlain(content, maxChars)
	truncatedRunes := utf8.RuneCountInString(content) - utf8.RuneCountInString(truncated)
	return fmt.Sprintf(
		"[WEBFETCH OUTPUT TRUNCATED at %d chars]\n%s\n...\n[... %d chars truncated. "+
			"Use a more specific URL or prompt to narrow the response.]",
		maxChars, truncated, truncatedRunes,
	)
}

// sanitizeURLForFilename extracts a safe filename component from a URL.
// Uses the hostname + a simple hash of the path to keep filenames readable.
func sanitizeURLForFilename(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "unknown"
	}

	// Use hostname as the readable prefix.
	host := strings.NewReplacer(
		".", "_",
		":", "_",
		"/", "_",
		"\\", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	).Replace(parsed.Hostname())

	// Truncate hostname to avoid overly long filenames.
	if len(host) > 40 {
		host = host[:40]
	}

	return host
}
