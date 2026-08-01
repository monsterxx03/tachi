package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
)

func TestWebSearchTool_Name(t *testing.T) {
	tool := WebSearchTool{}
	if tool.Name() != "WebSearch" {
		t.Errorf("Expected name 'WebSearch', got '%s'", tool.Name())
	}
}

func TestWebSearchTool_Required(t *testing.T) {
	tool := WebSearchTool{}
	required := tool.Required()
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("Expected required ['query'], got %v", required)
	}
}

func TestWebSearchTool_Properties(t *testing.T) {
	tool := WebSearchTool{}
	props := tool.Properties()

	if _, ok := props["query"]; !ok {
		t.Error("Expected 'query' property")
	}
	if _, ok := props["num"]; !ok {
		t.Error("Expected 'num' property")
	}
}

func TestWebSearchTool_Execute_MissingQuery(t *testing.T) {
	tool := WebSearchTool{}
	args := `{}`
	_, err := tool.ExecuteContext(context.TODO(), args)
	if err == nil {
		t.Error("Expected error for missing query")
	}
}

func TestWebSearchTool_Execute_EmptyQuery(t *testing.T) {
	tool := WebSearchTool{}
	args := `{"query": ""}`
	_, err := tool.ExecuteContext(context.TODO(), args)
	if err == nil {
		t.Error("Expected error for empty query")
	}
}

func TestWebSearchTool_Execute_NoAPIKey(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")
	tool := WebSearchTool{}
	args := `{"query": "test"}`
	_, err := tool.ExecuteContext(context.TODO(), args)
	if err == nil {
		t.Error("Expected error when no API key is configured")
	}
	if !strings.Contains(err.Error(), "no web search provider API keys configured") {
		t.Errorf("Unexpected error message: %v", err)
	}
}

func TestWebSearchResult_Marshal(t *testing.T) {
	result := &WebSearchResult{
		Query:      "test query",
		NumResults: 2,
		Results: []SearchResult{
			{
				Title:   "Test Title 1",
				Link:    "https://example.com/1",
				Snippet: "Test snippet 1",
			},
			{
				Title:   "Test Title 2",
				Link:    "https://example.com/2",
				Snippet: "Test snippet 2",
			},
		},
		DurationMs: 100,
		Provider:   config.WebSearchProviderBrave,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Failed to marshal result: %v", err)
	}

	var unmarshaled WebSearchResult
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if unmarshaled.Query != result.Query {
		t.Errorf("Query mismatch: expected '%s', got '%s'", result.Query, unmarshaled.Query)
	}
	if unmarshaled.NumResults != result.NumResults {
		t.Errorf("NumResults mismatch: expected %d, got %d", result.NumResults, unmarshaled.NumResults)
	}
	if len(unmarshaled.Results) != len(result.Results) {
		t.Errorf("Results length mismatch: expected %d, got %d", len(result.Results), len(unmarshaled.Results))
	}
}

// ── quota classification ─────────────────────────────────────────────────

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		headers http.Header
		want    searchErrKind
	}{
		// Exa: 402 is documented as credits exhausted / budget exceeded.
		{"exa no more credits", 402, `{"requestId":"x","error":"Account credits exhausted","tag":"NO_MORE_CREDITS"}`, nil, errQuota},
		{"exa api key budget exceeded", 402, `{"tag":"API_KEY_BUDGET_EXCEEDED"}`, nil, errQuota},
		{"exa team budget exceeded", 402, `{"tag":"TEAM_BUDGET_EXCEEDED"}`, nil, errQuota},
		// Exa: 429 is documented as rate limit — transient, no pause.
		{"exa rate limit 429", 429, `{"error":"You've exceeded your Exa rate limit of 10 requests per second"}`, nil, errRateLimit},
		// Brave: 429 with no quota header = transient rate limit.
		{"brave 429 no header", 429, `{"error":{"code":"RATE_LIMITED"}}`, nil, errRateLimit},
		{"brave 429 generic body", 429, `Request rate limit exceeded for plan`, nil, errRateLimit},
		// Brave: 429 with monthly quota remaining = 0 → real exhaustion.
		// (canonical header key — http.Header.Get normalizes to "X-Ratelimit-Remaining")
		{"brave monthly quota exhausted", 429, `{"error":{"code":"RATE_LIMITED"}}`, http.Header{"X-Ratelimit-Remaining": {"1, 0"}}, errQuota},
		{"brave monthly quota remaining", 429, `{"error":{"code":"RATE_LIMITED"}}`, http.Header{"X-Ratelimit-Remaining": {"1, 14523"}}, errRateLimit},
		// Body wording fallbacks (explicit credit exhaustion only).
		{"insufficient credits text", 403, `insufficient credits for this account`, nil, errQuota},
		{"budget exceeded text", 403, `Your key budget exceeded`, nil, errQuota},
		// Brave 429 body embeds quota_limit/quota_current metadata — a bare
		// "quota" word must NOT be treated as exhaustion.
		{"brave 429 quota metadata not exhaustion", 429, `{"error":{"meta":{"quota_limit":2000,"quota_current":15},"code":"RATE_LIMITED"}}`, nil, errRateLimit},
		// Everything else.
		{"server error", 500, `internal server error`, nil, errOther},
		{"invalid key", 401, `invalid api key`, nil, errOther},
		{"forbidden", 403, `forbidden`, nil, errOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyError(tc.status, tc.body, tc.headers)
			if got == nil || got.kind != tc.want {
				t.Errorf("classifyError(%d, %q) kind = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestMonthlyQuotaExhausted(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"monthly exhausted", "1, 0", true},
		{"monthly exhausted negative", "0, -1", true},
		{"monthly remaining", "1, 14523", false},
		{"missing header", "", false},
		{"single value", "1", false},
		{"malformed", "1, abc", false},
		{"whitespace only", "  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("X-RateLimit-Remaining", tc.value)
			}
			if got := monthlyQuotaExhausted(h); got != tc.want {
				t.Errorf("monthlyQuotaExhausted(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// ── billing cycle ────────────────────────────────────────────────────────

func TestNextBillingCycle(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"mid month", time.Date(2026, 8, 15, 14, 30, 0, 0, loc), time.Date(2026, 9, 1, 0, 0, 0, 0, loc)},
		{"first of month", time.Date(2026, 8, 1, 0, 0, 0, 0, loc), time.Date(2026, 9, 1, 0, 0, 0, 0, loc)},
		{"december rolls to next year", time.Date(2026, 12, 31, 23, 59, 59, 0, loc), time.Date(2027, 1, 1, 0, 0, 0, 0, loc)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextBillingCycle(tc.now); !got.Equal(tc.want) {
				t.Errorf("nextBillingCycle(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

// ── pause store ──────────────────────────────────────────────────────────

func TestPauseStore_PauseAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paused.json")
	store := &pauseStore{path: path}
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	if got := store.pausedProviders(now); len(got) != 0 {
		t.Fatalf("expected no paused providers initially, got %v", got)
	}

	resume := nextBillingCycle(now)
	store.pause(config.WebSearchProviderExa, "rate limited (429)", resume)

	paused := store.pausedProviders(now)
	if len(paused) != 1 || paused[config.WebSearchProviderExa].ResumeAfter != resume {
		t.Fatalf("expected exa paused with resume %v, got %v", resume, paused)
	}

	// Pause state must persist on disk (a fresh store reading the same file).
	store2 := &pauseStore{path: path}
	if got := store2.pausedProviders(now); len(got) != 1 {
		t.Fatalf("expected exa still paused from disk, got %v", got)
	}

	// After the resume time the provider is active again and the file pruned.
	later := resume.Add(time.Hour)
	if got := store2.pausedProviders(later); len(got) != 0 {
		t.Fatalf("expected exa active after billing cycle, got %v", got)
	}
	if got := store2.load().Providers; len(got) != 0 {
		t.Errorf("pause file should be pruned, still contains %v", got)
	}
}

func TestPauseStore_PreservesOtherProviders(t *testing.T) {
	store := &pauseStore{path: filepath.Join(t.TempDir(), "paused.json")}
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	resume := nextBillingCycle(now)

	store.pause(config.WebSearchProviderExa, "quota", resume)
	store.pause(config.WebSearchProviderBrave, "quota", resume)

	paused := store.pausedProviders(now)
	if len(paused) != 2 {
		t.Fatalf("expected both providers paused, got %v", paused)
	}
}

// ── fallback / priority ──────────────────────────────────────────────────

type fakeProvider struct {
	name    string
	results []SearchResult
	err     *searchErr
	calls   int
}

func (f *fakeProvider) Type() string { return f.name }
func (f *fakeProvider) Search(_ context.Context, _ *http.Client, _ string, _ int) ([]SearchResult, *searchErr) {
	f.calls++
	return f.results, f.err
}

func newTestTool(t *testing.T) *WebSearchTool {
	t.Helper()
	return &WebSearchTool{
		MaxResults: 10,
		pause:      &pauseStore{path: filepath.Join(t.TempDir(), "paused.json")},
	}
}

func TestSearchWithProviders_FirstSucceeds(t *testing.T) {
	tool := newTestTool(t)
	first := &fakeProvider{name: config.WebSearchProviderBrave, results: []SearchResult{{Title: "T", Link: "https://x", Snippet: "s"}}}
	second := &fakeProvider{name: config.WebSearchProviderExa, results: []SearchResult{{Title: "U", Link: "https://y"}}}

	out, err := tool.searchWithProviders(context.Background(), "q", 5, []webSearchProvider{first, second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf(`"provider":"%s"`, config.WebSearchProviderBrave)) {
		t.Errorf("expected provider brave in output, got: %s", out)
	}
	if second.calls != 0 {
		t.Errorf("second provider should not be called when first succeeds")
	}
}

func TestSearchWithProviders_FallbackOnQuota(t *testing.T) {
	tool := newTestTool(t)
	first := &fakeProvider{name: config.WebSearchProviderExa, err: &searchErr{kind: errQuota, message: "credits exhausted (402)"}}
	second := &fakeProvider{name: config.WebSearchProviderBrave, results: []SearchResult{{Title: "T", Link: "https://x"}}}

	out, err := tool.searchWithProviders(context.Background(), "q", 5, []webSearchProvider{first, second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf(`"provider":"%s"`, config.WebSearchProviderBrave)) {
		t.Errorf("expected fallback to brave, got: %s", out)
	}

	// The quota-hit provider must be marked paused.
	paused := tool.pause.pausedProviders(time.Now())
	if _, ok := paused[config.WebSearchProviderExa]; !ok {
		t.Errorf("expected exa to be paused after quota error, got %v", paused)
	}
}

func TestSearchWithProviders_FallbackOnRateLimit_NoPause(t *testing.T) {
	tool := newTestTool(t)
	first := &fakeProvider{name: config.WebSearchProviderBrave, err: &searchErr{kind: errRateLimit, message: "rate limited (429)"}}
	second := &fakeProvider{name: config.WebSearchProviderExa, results: []SearchResult{{Title: "T", Link: "https://x"}}}

	out, err := tool.searchWithProviders(context.Background(), "q", 5, []webSearchProvider{first, second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf(`"provider":"%s"`, config.WebSearchProviderExa)) {
		t.Errorf("expected fallback to exa, got: %s", out)
	}

	// A transient rate limit must NOT pause the provider.
	if paused := tool.pause.pausedProviders(time.Now()); len(paused) != 0 {
		t.Errorf("rate limits must not pause providers, got %v", paused)
	}
}

func TestSearchWithProviders_FallbackOnOtherError_NoPause(t *testing.T) {
	tool := newTestTool(t)
	first := &fakeProvider{name: config.WebSearchProviderBrave, err: &searchErr{kind: errOther, message: "invalid api key"}}
	second := &fakeProvider{name: config.WebSearchProviderExa, results: []SearchResult{{Title: "T", Link: "https://x"}}}

	if _, err := tool.searchWithProviders(context.Background(), "q", 5, []webSearchProvider{first, second}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if paused := tool.pause.pausedProviders(time.Now()); len(paused) != 0 {
		t.Errorf("non-quota errors must not pause providers, got %v", paused)
	}
}

func TestSearchWithProviders_AllFail(t *testing.T) {
	tool := newTestTool(t)
	first := &fakeProvider{name: config.WebSearchProviderBrave, err: &searchErr{kind: errQuota, message: "rate limited"}}
	second := &fakeProvider{name: config.WebSearchProviderExa, err: &searchErr{kind: errOther, message: "500"}}

	_, err := tool.searchWithProviders(context.Background(), "q", 5, []webSearchProvider{first, second})
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
	if !strings.Contains(err.Error(), "all web search providers failed") {
		t.Errorf("unexpected error message: %v", err)
	}
	if !strings.Contains(err.Error(), config.WebSearchProviderBrave) || !strings.Contains(err.Error(), config.WebSearchProviderExa) {
		t.Errorf("error should mention all failed providers: %v", err)
	}
}

// ── active provider filtering ────────────────────────────────────────────

func TestActiveProviders_SkipsPaused(t *testing.T) {
	tool := &WebSearchTool{
		Providers: []config.WebSearchProviderConfig{
			{Type: config.WebSearchProviderExa, Key: "k1"},
			{Type: config.WebSearchProviderBrave, Key: "k2"},
		},
		pause: &pauseStore{path: filepath.Join(t.TempDir(), "paused.json")},
	}
	tool.pause.pause(config.WebSearchProviderExa, "quota", nextBillingCycle(time.Now()))

	active := tool.activeProviders()
	if len(active) != 1 || active[0].Type() != config.WebSearchProviderBrave {
		t.Errorf("expected only brave active, got %d providers", len(active))
	}
}

func TestExecuteContext_AllProvidersPaused(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")
	tool := &WebSearchTool{
		Providers: []config.WebSearchProviderConfig{
			{Type: config.WebSearchProviderBrave, Key: "k"},
			{Type: config.WebSearchProviderExa, Key: "k2"},
		},
		pause: &pauseStore{path: filepath.Join(t.TempDir(), "paused.json")},
	}
	tool.pause.pause(config.WebSearchProviderBrave, "quota", nextBillingCycle(time.Now()))
	tool.pause.pause(config.WebSearchProviderExa, "quota", nextBillingCycle(time.Now()))

	_, err := tool.ExecuteContext(context.TODO(), `{"query": "test"}`)
	if err == nil {
		t.Fatal("expected error when all providers are paused")
	}
	if !strings.Contains(err.Error(), "quota-paused") {
		t.Errorf("expected error to mention quota-paused providers, got: %v", err)
	}
}

// ── Configured ───────────────────────────────────────────────────────────

func TestWebSearchTool_Configured(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")

	tool := &WebSearchTool{}
	if tool.Configured() {
		t.Error("zero-value tool with no env key should not be configured")
	}

	tool = &WebSearchTool{Providers: []config.WebSearchProviderConfig{{Type: config.WebSearchProviderExa, Key: "k"}}}
	if !tool.Configured() {
		t.Error("tool with a keyed provider should be configured")
	}

	tool = &WebSearchTool{Providers: []config.WebSearchProviderConfig{{Type: config.WebSearchProviderExa, Key: ""}}}
	if tool.Configured() {
		t.Error("tool with only empty-key providers should not be configured")
	}

	// Default provider type (empty) resolves to exa.
	tool = &WebSearchTool{Providers: []config.WebSearchProviderConfig{{Key: "k"}}}
	if !tool.Configured() {
		t.Error("tool with a defaulted (exa) provider should be configured")
	}

	t.Setenv("BRAVE_API_KEY", "env-key")
	tool = &WebSearchTool{}
	if !tool.Configured() {
		t.Error("BRAVE_API_KEY env fallback should configure the tool")
	}

	// EXA_API_KEY takes precedence over BRAVE_API_KEY in env fallback.
	t.Setenv("EXA_API_KEY", "exa-env-key")
	tool = &WebSearchTool{}
	providers := tool.providerList()
	if len(providers) != 1 || providers[0].Type() != config.WebSearchProviderExa {
		t.Errorf("EXA_API_KEY should win env fallback, got %d providers", len(providers))
	}
}

// ── provider list caching ────────────────────────────────────────────────

func TestWebSearchTool_ProviderListCached(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "env-key")
	t.Setenv("EXA_API_KEY", "")
	tool := &WebSearchTool{}

	a := tool.providerList()
	b := tool.providerList()
	if len(a) != 1 || a[0].Type() != config.WebSearchProviderBrave {
		t.Fatalf("expected env-fallback brave provider, got %v", a)
	}
	// Same backing array across calls — the list is built once, not rebuilt.
	if &a[0] != &b[0] {
		t.Error("provider list should be cached, got a fresh allocation")
	}

	// Environment is read once per tool instance (first use), not per call.
	t.Setenv("BRAVE_API_KEY", "")
	if len(tool.providerList()) != 1 {
		t.Error("cached provider list must not change after env mutation")
	}
}

func TestWebSearchTool_ConcurrentInit(t *testing.T) {
	tool := NewWebSearchTool(WebSearchToolConfig{
		Providers: []config.WebSearchProviderConfig{
			{Type: config.WebSearchProviderBrave, Key: "k1"},
			{Type: config.WebSearchProviderExa, Key: "k2"},
		},
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tool.providerList()
			_ = tool.getHTTPClient()
			_ = tool.pauseStore()
		}()
	}
	wg.Wait()
	if got := len(tool.providerList()); got != 2 {
		t.Errorf("expected 2 providers after concurrent init, got %d", got)
	}
}

// ── provider HTTP tests (httptest) ───────────────────────────────────────

func TestBraveProvider_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "test-key" {
			t.Errorf("missing X-Subscription-Token header")
		}
		if r.URL.Query().Get("q") != "hello world" {
			t.Errorf("unexpected query: %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"T1","url":"https://a.com","description":"d1"},
			{"title":"T2","url":"https://b.com","description":"d2"}
		]}}`)
	}))
	defer srv.Close()

	p := &braveProvider{apiKey: "test-key", baseURL: srv.URL}
	results, serr := p.Search(context.Background(), srv.Client(), "hello world", 5)
	if serr != nil {
		t.Fatalf("unexpected error: %v", serr)
	}
	if len(results) != 2 || results[0].Title != "T1" || results[0].Snippet != "d1" {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestBraveProvider_RateLimit_NoQuotaHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"Request rate limit exceeded for plan"}`)
	}))
	defer srv.Close()

	p := &braveProvider{apiKey: "k", baseURL: srv.URL}
	_, serr := p.Search(context.Background(), srv.Client(), "q", 5)
	if serr == nil || serr.kind != errRateLimit {
		t.Fatalf("expected errRateLimit for 429 without quota header, got %+v", serr)
	}
}

func TestBraveProvider_MonthlyQuotaExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "1, 0") // 0 requests left this month
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"RATE_LIMITED"}}`)
	}))
	defer srv.Close()

	p := &braveProvider{apiKey: "k", baseURL: srv.URL}
	_, serr := p.Search(context.Background(), srv.Client(), "q", 5)
	if serr == nil || serr.kind != errQuota {
		t.Fatalf("expected errQuota for exhausted monthly quota, got %+v", serr)
	}
}

func TestExaProvider_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing x-api-key header")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if body["query"] != "hello" || body["numResults"] != float64(5) {
			t.Errorf("unexpected body: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[
			{"title":"E1","url":"https://e.com","text":"snippet text","author":"a1"}
		]}`)
	}))
	defer srv.Close()

	p := &exaProvider{apiKey: "test-key", baseURL: srv.URL}
	results, serr := p.Search(context.Background(), srv.Client(), "hello", 5)
	if serr != nil {
		t.Fatalf("unexpected error: %v", serr)
	}
	if len(results) != 1 || results[0].Title != "E1" || results[0].Snippet != "snippet text" {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestExaProvider_QuotaNoMoreCredits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"requestId":"x","error":"Account credits exhausted","tag":"NO_MORE_CREDITS"}`)
	}))
	defer srv.Close()

	p := &exaProvider{apiKey: "k", baseURL: srv.URL}
	_, serr := p.Search(context.Background(), srv.Client(), "q", 5)
	if serr == nil || serr.kind != errQuota {
		t.Fatalf("expected errQuota for 402, got %+v", serr)
	}
}

func TestExaProvider_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"You've exceeded your Exa rate limit of 10 requests per second"}`)
	}))
	defer srv.Close()

	p := &exaProvider{apiKey: "k", baseURL: srv.URL}
	_, serr := p.Search(context.Background(), srv.Client(), "q", 5)
	if serr == nil || serr.kind != errRateLimit {
		t.Fatalf("expected errRateLimit for 429, got %+v", serr)
	}
}
