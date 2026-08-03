package tools

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// SearchResult is a single web search hit.
type SearchResult struct {
	Title       string `json:"title"`
	Link        string `json:"link"`
	Snippet     string `json:"snippet"`
	DisplayLink string `json:"displayLink,omitempty"`
}

// WebSearchResult is the tool output envelope.
type WebSearchResult struct {
	Query        string         `json:"query"`
	NumResults   int            `json:"numResults"`
	Results      []SearchResult `json:"results"`
	DurationMs   int64          `json:"durationMs"`
	Provider     string         `json:"provider"`
	ErrorMessage string         `json:"error,omitempty"`
}

type webSearchArgs struct {
	Query string `json:"query"`
	Num   *int   `json:"num"`
}

// webSearchProvider is a single search backend. Implementations are
// stateless and safe for concurrent use (they only read their config).
type webSearchProvider interface {
	// Type is the provider name surfaced in results and pause state (one of
	// config.WebSearchProviderExa, config.WebSearchProviderBrave). Must be
	// unique within one tool instance.
	Type() string
	// Search executes a search. A nil *searchErr means success.
	Search(ctx context.Context, client *http.Client, query string, num int) ([]SearchResult, *searchErr)
}

// searchErrKind classifies provider failures so the caller can decide whether
// the provider should be paused (quota exhausted) or just skipped.
type searchErrKind int

const (
	errOther searchErrKind = iota
	// errQuota means the provider's monthly credit/quota is exhausted —
	// the provider is paused until the next billing cycle.
	errQuota
	// errRateLimit means a transient rate limit (e.g. HTTP 429) — the
	// provider is skipped for this call but NOT paused.
	errRateLimit
)

// searchErr is a provider failure with a machine-readable kind.
type searchErr struct {
	kind    searchErrKind
	message string
}

func (e *searchErr) Error() string { return e.message }

// nextBillingCycle returns 00:00 local time on the 1st of next month — the
// start of the provider's next billing cycle. Quota-paused providers resume
// at this instant.
func nextBillingCycle(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
}

// WebSearchToolConfig carries the tool's construction options.
type WebSearchToolConfig struct {
	Providers  []config.WebSearchProviderConfig
	Timeout    time.Duration
	MaxResults int
	Proxy      string
}

// WebSearchTool performs web searches through a priority-ordered list of
// providers (first entry wins). When a provider fails with a quota error it
// is recorded as paused until the next billing cycle and the search falls
// back to the next provider; other failures just skip to the next provider.
// Fallbacks are silent to the user (WARN logs only) — an error is returned
// only when every provider fails.
//
// Derived state (provider list, HTTP client, pause store) is built lazily on
// first use and cached; initOnce makes that safe under concurrent execution
// (Parallel() returns true).
type WebSearchTool struct {
	Providers  []config.WebSearchProviderConfig
	Timeout    time.Duration
	MaxResults int
	Proxy      string // Optional proxy URL (e.g. socks5://127.0.0.1:1080)

	initOnce  sync.Once
	providers []webSearchProvider // lazily built by init(); nil until first use
	pause     *pauseStore         // lazily built by init() unless pre-set (tests)
	client    *http.Client        // lazily built by init()
}

// NewWebSearchTool builds a WebSearchTool from config.
func NewWebSearchTool(cfg WebSearchToolConfig) *WebSearchTool {
	return &WebSearchTool{
		Providers:  cfg.Providers,
		Timeout:    cfg.Timeout,
		MaxResults: cfg.MaxResults,
		Proxy:      cfg.Proxy,
	}
}

// init builds and caches all derived state exactly once (thread-safe via
// sync.Once): the provider list (reading config + env fallback), the pause
// store, and the HTTP client. A pre-set pause store (tests) is respected.
func (t *WebSearchTool) init() {
	t.initOnce.Do(func() {
		t.providers = t.buildProviders()
		if t.pause == nil {
			t.pause = &pauseStore{path: config.WebSearchPausePath()}
		}
		t.client = httpx.NewClient(t.Timeout, t.Proxy)
	})
}

// providerList returns the cached provider list (priority order + env
// fallback), building it once on first use.
func (t *WebSearchTool) providerList() []webSearchProvider {
	t.init()
	return t.providers
}

// pauseStore returns the pause store used by this tool.
func (t *WebSearchTool) pauseStore() *pauseStore {
	t.init()
	return t.pause
}

// getHTTPClient returns the cached *http.Client, reused across every search
// call to preserve connection pooling and proxy connections.
func (t *WebSearchTool) getHTTPClient() *http.Client {
	t.init()
	return t.client
}

func (t *WebSearchTool) Name() string { return ToolNameWebSearch }
func (t *WebSearchTool) Description() string {
	return "Performs a web search. Returns search results including titles, links, and snippets."
}
func (t *WebSearchTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"query": {Type: "string", Description: "The search query to execute"},
		"num":   {Type: "integer", Description: "Number of results to return (default: 5, max: 10)", Minimum: new(1.0), Maximum: new(10.0), Default: 5},
	}
}
func (t *WebSearchTool) Required() []string { return []string{"query"} }
func (t *WebSearchTool) Parallel() bool     { return true }

// Configured reports whether the tool has at least one provider with an API
// key, either from config or the EXA_API_KEY / BRAVE_API_KEY env fallback.
func (t *WebSearchTool) Configured() bool {
	return len(t.providerList()) > 0
}

// buildProviders materializes the configured provider list in priority order.
// If nothing is configured, providers are synthesized from the EXA_API_KEY
// (preferred) / BRAVE_API_KEY environment variables (legacy env fallback).
func (t *WebSearchTool) buildProviders() []webSearchProvider {
	var out []webSearchProvider
	for _, pc := range t.Providers {
		typ := strings.ToLower(pc.Type)
		if typ == "" {
			typ = config.WebSearchProviderExa // default provider (config defaults also fill this in)
		}
		switch typ {
		case config.WebSearchProviderBrave:
			if pc.Key != "" {
				out = append(out, &braveProvider{apiKey: pc.Key, baseURL: pc.BaseURL})
			}
		case config.WebSearchProviderExa:
			if pc.Key != "" {
				out = append(out, &exaProvider{apiKey: pc.Key, baseURL: pc.BaseURL})
			}
		}
	}
	if len(out) == 0 {
		if key := os.Getenv("EXA_API_KEY"); key != "" {
			out = append(out, &exaProvider{apiKey: key})
		} else if key := os.Getenv("BRAVE_API_KEY"); key != "" {
			out = append(out, &braveProvider{apiKey: key})
		}
	}
	return out
}

// activeProviders filters out providers that are quota-paused until their
// next billing cycle.
func (t *WebSearchTool) activeProviders() []webSearchProvider {
	all := t.providerList()
	if len(all) == 0 {
		return all
	}
	paused := t.pauseStore().pausedProviders(time.Now())
	if len(paused) == 0 {
		return all
	}
	out := make([]webSearchProvider, 0, len(all))
	for _, p := range all {
		if _, isPaused := paused[p.Type()]; isPaused {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (t *WebSearchTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var a webSearchArgs
	if err := parseArgs(args, &a); err != nil {
		return "", err
	}

	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}

	maxResults := t.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}
	numResults := 5
	if a.Num != nil && *a.Num > 0 {
		numResults = min(*a.Num, maxResults)
	}

	providers := t.activeProviders()
	if len(providers) == 0 {
		return "", t.noProvidersError()
	}

	return t.searchWithProviders(ctx, a.Query, numResults, providers)
}

// searchWithProviders tries each provider in order. Quota errors pause the
// provider until the next billing cycle; all failures are logged as WARNs and
// the search falls through to the next provider. An error is returned only
// when every provider fails.
func (t *WebSearchTool) searchWithProviders(ctx context.Context, query string, num int, providers []webSearchProvider) (string, error) {
	client := t.getHTTPClient()
	start := time.Now()
	var failures []string

	for _, p := range providers {
		results, serr := p.Search(ctx, client, query, num)
		if serr == nil {
			duration := time.Since(start).Milliseconds()
			// Record which provider served this search (query truncated to
			// keep the log line readable).
			logger.FromContext(ctx).Info(ctx, "WebSearch: provider served search",
				"provider", p.Type(),
				"query", strutil.Truncate(query, 120),
				"num", len(results),
				"duration_ms", duration)
			return marshalResult(&WebSearchResult{
				Query:      query,
				NumResults: len(results),
				Results:    results,
				DurationMs: duration,
				Provider:   p.Type(),
			})
		}

		switch serr.kind {
		case errQuota:
			resume := nextBillingCycle(time.Now())
			t.pauseStore().pause(p.Type(), serr.message, resume)
			logger.FromContext(ctx).Warn(ctx, "WebSearch: provider quota exhausted, pausing until next billing cycle",
				"provider", p.Type(), "resume_after", resume.Format(time.RFC3339), "reason", serr.message)
		case errRateLimit:
			logger.FromContext(ctx).Warn(ctx, "WebSearch: provider rate limited, falling back to next provider",
				"provider", p.Type(), "reason", serr.message)
		default:
			logger.FromContext(ctx).Warn(ctx, "WebSearch: provider failed, falling back to next provider",
				"provider", p.Type(), "reason", serr.message)
		}
		failures = append(failures, fmt.Sprintf("%s: %s", p.Type(), serr.message))
	}

	return "", fmt.Errorf("all web search providers failed: %s", strings.Join(failures, "; "))
}

// noProvidersError builds a user-facing error when no provider is usable.
func (t *WebSearchTool) noProvidersError() error {
	paused := t.pauseStore().pausedProviders(time.Now())
	if len(t.providerList()) > 0 && len(paused) > 0 {
		names := make([]string, 0, len(paused))
		for name, rec := range paused {
			names = append(names, fmt.Sprintf("%s (resumes %s)", name, rec.ResumeAfter.Format(strutil.TimeFormatDate)))
		}
		sort.Strings(names)
		return fmt.Errorf("no web search providers available: all configured providers are quota-paused: %s", strings.Join(names, ", "))
	}
	return fmt.Errorf("no web search provider API keys configured. Set web_search.providers in %s (type: %s or %s), or set EXA_API_KEY / BRAVE_API_KEY environment variable",
		filepath.Join(config.BaseDir(), "config.yaml"), config.WebSearchProviderExa, config.WebSearchProviderBrave)
}
