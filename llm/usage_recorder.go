package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// Usage billing ledger — records every LLM API call's token usage (with a
// price snapshot taken at call time) into an append-only, per-day JSONL file
// under <home>/usage/. The ledger is the single source of truth for cost
// accounting (/usage); the session/subagent/oneoff transcripts stay purely
// for debugging and never participate in cost.
//
// See docs/2026-08-05-usage-billing.md.

// UsageKind classifies an LLM call for cost grouping. The constant set aligns
// with OneOffMeta.Kind (agent/oneoff_recorder.go) — keep them in sync.
type UsageKind string

const (
	UsageKindConversation     UsageKind = "conversation"
	UsageKindTitle            UsageKind = "title"
	UsageKindKeyword          UsageKind = "keyword"
	UsageKindCompact          UsageKind = "compact"
	UsageKindCommit           UsageKind = "commit"
	UsageKindReview           UsageKind = "review"
	UsageKindAmbient          UsageKind = "ambient"
	UsageKindDream            UsageKind = "dream"
	UsageKindSubagent         UsageKind = "subagent"
	UsageKindResearchQuery    UsageKind = "research-query"
	UsageKindResearchReport   UsageKind = "research-report"
	UsageKindGithubDiscussion UsageKind = "github-discussion"
	UsageKindGithubPR         UsageKind = "github-pr"
)

// ctxKeyUsageKind is the context key for the per-call usage kind.
type ctxKeyUsageKind struct{}

// WithUsageKind injects a usage kind into the context. RecordingProvider
// reads it when recording the call; the default is conversation.
func WithUsageKind(ctx context.Context, kind UsageKind) context.Context {
	return context.WithValue(ctx, ctxKeyUsageKind{}, kind)
}

// UsageKindFromCtx returns the usage kind from the context, or
// UsageKindConversation when none is set.
func UsageKindFromCtx(ctx context.Context) UsageKind {
	if kind, ok := ctx.Value(ctxKeyUsageKind{}).(UsageKind); ok && kind != "" {
		return kind
	}
	return UsageKindConversation
}

// UsageRow is one ledger line — a single LLM API call. It is self-contained:
// token counts use the normalized cache-miss input scale (see below) and the
// price fields hold the FINAL effective unit prices (per 1M tokens, CNY) as
// resolved at call time — never re-resolved against current config later.
type UsageRow struct {
	TS        time.Time `json:"ts"`
	SessionID string    `json:"session_id,omitempty"` // empty = session-less (global)
	Kind      UsageKind `json:"kind"`
	Provider  string    `json:"provider"` // config provider name (e.g. "deepseek-v4-flash")
	Model     string    `json:"model"`

	// Token counts. InputTokens is ALWAYS cache-miss (unhit) — OpenAI-family
	// APIs report input_tokens including cache-read; the writer subtracts it.
	// The three categories are mutually exclusive, so a cache-hit token is
	// billed exactly once (at CacheReadPrice).
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`

	// Effective unit prices (CNY per 1M tokens) at call time. Fallbacks
	// (0 → InputPrice) are applied BEFORE writing; 0 here means genuinely
	// unpriced (no price table entry → counted as "unpriced", cost 0).
	InputPrice         float64 `json:"input_price"`
	OutputPrice        float64 `json:"output_price"`
	CacheReadPrice     float64 `json:"cache_read_price"`
	CacheCreationPrice float64 `json:"cache_creation_price"`
}

// Cost computes this row's cost in CNY from its own price snapshot.
// Rows carry already-normalized input (cache-miss scale) and already-resolved
// unit prices, so this is pure arithmetic via the shared costFromParts.
func (r *UsageRow) Cost() float64 {
	return costFromParts(
		r.InputTokens, r.CacheReadInputTokens, r.CacheCreationInputTokens, r.OutputTokens,
		r.InputPrice, r.OutputPrice, r.CacheReadPrice, r.CacheCreationPrice,
	)
}

// Unpriced reports whether the row had no effective price at call time.
func (r *UsageRow) Unpriced() bool {
	return r.InputPrice == 0 && r.OutputPrice == 0
}

// UsageRecorder appends UsageRows to per-day JSONL files under dir.
// It is safe for concurrent use (mutex + single-write O_APPEND semantics).
// Files are kept forever — no retention, no sweep (user decision).
type UsageRecorder struct {
	mu       sync.Mutex
	dir      string
	file     *os.File
	fileDate string // "2006-01-02" of the open file
}

// NewUsageRecorder creates a recorder writing to dir. The directory is
// created lazily on first Record — constructing a recorder has no side
// effects (safe for tests that never record).
func NewUsageRecorder(dir string) *UsageRecorder {
	return &UsageRecorder{dir: dir}
}

// Record appends one row. Failure is returned to the caller, which must only
// warn — recording must never break the LLM call itself.
func (r *UsageRecorder) Record(row UsageRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, err := r.fileFor(row.TS)
	if err != nil {
		return err
	}
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Correctness depends on a SINGLE write(2) per line: with O_APPEND the
	// offset positioning and the write are atomic, so concurrent writers
	// (including cross-process, e.g. TUI + tachi -c) never interleave lines.
	_, err = f.Write(data)
	return err
}

// Close closes the open day file. Idempotent; safe to leave unclosed
// (the file stays open for the process lifetime in production).
func (r *UsageRecorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
		r.fileDate = ""
	}
}

// fileFor returns the day file for ts, rotating when the date changes.
func (r *UsageRecorder) fileFor(ts time.Time) (*os.File, error) {
	date := ts.Format("2006-01-02")
	if r.file != nil && r.fileDate == date {
		return r.file, nil
	}
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	if err := os.MkdirAll(r.dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(r.dir, date+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	r.file = f
	r.fileDate = date
	return f, nil
}

// Rows returns all ledger rows whose session_id equals sessionID, scanning
// day files from (and including) the given lower bound date onward. An empty
// sessionID returns session-less (global) rows only. from may be zero to scan
// all files. Rows are returned in file order (oldest first).
//
// Files are streamed line-by-line (bufio) — the ledger is kept forever, so
// whole-file reads would grow linearly with age.
func (r *UsageRecorder) Rows(sessionID string, from time.Time) ([]UsageRow, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lower := ""
	if !from.IsZero() {
		lower = from.Format("2006-01-02")
	}
	var rows []UsageRow
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if lower != "" && name < lower+".jsonl" {
			continue
		}
		f, err := os.Open(filepath.Join(r.dir, name))
		if err != nil {
			continue // best-effort: skip unreadable files
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var row UsageRow
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				continue // best-effort: skip malformed lines
			}
			if row.SessionID == sessionID {
				rows = append(rows, row)
			}
		}
		_ = f.Close()
	}
	return rows, nil
}

// PriceResolver resolves the effective price for a model at call time.
// The agent constructs it closing over the config + the provider's config
// name (llm.Provider.Name() only returns the type name, which cannot be used
// to look up per-provider price overrides).
type PriceResolver func(model string) *ModelPrice

// RecordingProvider wraps a Provider and records every successful LLM call's
// usage into the ledger. It MUST be the outermost decorator (outside
// RetryProvider): otherwise a retried logical call would record one row per
// successful attempt.
type RecordingProvider struct {
	inner        Provider
	rec          *UsageRecorder
	providerName string // config provider name (e.g. "deepseek-v4-flash"); "" = fall back to inner.Name()
	price        PriceResolver
}

// WrapRecordingProvider wraps inner with usage recording. Returns inner
// unchanged when rec is nil (recording disabled) — no overhead, no behavior
// change. Idempotent for the same recorder: already-wrapped instances are
// returned as-is, so repeated wrapping at different provider-creation points
// can never double-record a call.
//
// providerName is the CONFIG provider name (e.g. "deepseek-v4-flash"),
// written into ledger rows for grouping — llm.Provider.Name() only returns
// the type name (openai/anthropic), which cannot distinguish two providers
// of the same type. Empty falls back to inner.Name().
// price may be nil: rows are then written with zero prices (counted as
// unpriced) but tokens are still tracked.
func WrapRecordingProvider(inner Provider, rec *UsageRecorder, providerName string, price PriceResolver) Provider {
	if inner == nil || rec == nil {
		return inner
	}
	if rp, ok := inner.(*RecordingProvider); ok && rp.rec == rec {
		return inner // already wrapped with this recorder — never double-record
	}
	return &RecordingProvider{inner: inner, rec: rec, providerName: providerName, price: price}
}

func (p *RecordingProvider) Name() string  { return p.inner.Name() }
func (p *RecordingProvider) Model() string { return p.inner.Model() }

func (p *RecordingProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	resp, err := p.inner.CreateChat(ctx, messages, tools, opts)
	if err == nil && resp != nil && resp.Usage != nil {
		if recErr := p.record(ctx, opts, resp.Usage); recErr != nil {
			logger.FromContext(ctx).Warn(ctx, "usage ledger: record failed", recErr)
		}
	}
	return resp, err
}

func (p *RecordingProvider) CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error) {
	innerCh, err := p.inner.CreateChatStream(ctx, messages, tools, opts)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		for ev := range innerCh {
			// Honor ctx cancellation: a consumer that stops draining early
			// must not leak this goroutine (or block the inner provider's
			// channel close) — abandon the passthrough instead.
			select {
			case <-ctx.Done():
				return
			default:
			}
			if ev.Type == StreamEventDone && ev.Usage != nil {
				if recErr := p.record(ctx, opts, ev.Usage); recErr != nil {
					logger.FromContext(ctx).Warn(ctx, "usage ledger: record failed", recErr)
				}
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// record assembles the ledger row for a completed call.
func (p *RecordingProvider) record(ctx context.Context, opts ChatOptions, u *Usage) error {
	kind := normalizeUsageKind(UsageKindFromCtx(ctx))
	sid := opts.SessionID
	if sid == "" {
		sid, _ = SessionIDFromCtx(ctx)
	}
	if kind == UsageKindSubagent {
		sid = normalizeSubagentSessionID(sid)
	}

	// Price snapshot at call time; fallbacks applied before writing.
	pr := &ModelPrice{}
	if p.price != nil {
		if resolved := p.price(p.inner.Model()); resolved != nil {
			pr = resolved
		}
	}
	cacheReadPrice, cacheCreationPrice := CacheReadCreationPrice(pr)

	// Normalize input to cache-miss scale (shared rule, see
	// NormalizeCacheMissInput): OpenAI-family APIs (openai / openai-res)
	// report input_tokens INCLUDING cache-read tokens; Anthropic does not.
	// Billing a hit token at both input and cache-read prices would
	// double-count it.
	input := NormalizeCacheMissInput(u.InputTokens, u.CacheReadInputTokens, p.inner.Name())

	provider := p.providerName
	if provider == "" {
		provider = p.Name() // fallback for bare wrapping (tests, ad-hoc callers)
	}

	return p.rec.Record(UsageRow{
		TS:                       time.Now(),
		SessionID:                sid,
		Kind:                     kind,
		Provider:                 provider,
		Model:                    p.Model(),
		InputTokens:              input,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		InputPrice:               pr.InputPrice,
		OutputPrice:              pr.OutputPrice,
		CacheReadPrice:           cacheReadPrice,
		CacheCreationPrice:       cacheCreationPrice,
	})
}

// normalizeUsageKind maps any kind drift onto the closed constant set
// (docs §5): multi-round /review used "review-round-N" in one spot — fold
// it back to review so per-kind filtering never misses rows.
func normalizeUsageKind(kind UsageKind) UsageKind {
	if strings.HasPrefix(string(kind), "review-round-") {
		return UsageKindReview
	}
	return kind
}

// normalizeSubagentSessionID maps a subagent's composite session ID
// ("<parentID>:<shortID>", see agent/subagent/executor.go) back to the parent
// session, so /usage session filtering catches subagent rows. A bare shortID
// (no parent session) falls to the global bucket (empty session_id) instead
// of becoming an invisible orphan.
func normalizeSubagentSessionID(sid string) string {
	if sid == "" {
		return ""
	}
	if before, _, ok := strings.Cut(sid, ":"); ok {
		return before
	}
	return ""
}
