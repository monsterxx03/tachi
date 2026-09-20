package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// The generated layer of a server summary: one LLM call per server per TOOL
// SET, cached on disk so a restart, a new session or the next process reuses it.
//
// Two things it deliberately is not:
//
//   - it is never on the turn's critical path. Generate returns the cached text
//     or "" (the caller falls back to the heuristic) and starts the call in the
//     background; the reminder runs inside turn preparation, and a model call
//     there would add its latency to the first message of every session.
//   - it is never trusted over the server's own words. A server that describes
//     itself wins (see ServerSummarizer), so the cache only exists for the ones
//     that do not.
const (
	// summaryPromptVersion is part of the cache key. Changing the prompt MUST
	// invalidate every stored summary, or the cache would keep serving text
	// nobody asks for any more — the one failure mode a cache is allowed to have
	// and the one nobody notices.
	summaryPromptVersion = "1"
	// summaryMaxTokens bounds a paragraph, not a document.
	summaryMaxTokens = 300
	// summaryTimeout bounds one generation. It runs in the background, so it is
	// generous; a hung provider must not leak a goroutine forever.
	summaryTimeout = 45 * time.Second
	// summaryToolTextLimit is how much of a tool's description the summarizer
	// reads — the same first line the reminder shows, so the summary describes
	// what the reader could have seen.
	summaryToolTextLimit = 100
)

// SummaryGenerator produces one paragraph per MCP server, with the result cached
// on disk.
type SummaryGenerator struct {
	// Provider resolves the model to summarize with, at call time (a session can
	// switch providers).
	Provider func() llm.Provider
	// Cache is where summaries live. Required for caching; nil disables it and
	// every Generate call would regenerate.
	Cache *SummaryCache
	// Logger is optional; failures are logged at debug and never surfaced — a
	// missing summary is a thinner reminder, not an error.
	Logger *logger.Logger
	// SessionID anchors the ledger row to a session (optional).
	SessionID func() string
	// Kick runs the background work. nil = `go f()`; tests pass a synchronous
	// one so the assertion after a call can see the result.
	Kick func(func())
	// OnReady is called after a summary has been generated and cached. The
	// reminder fires at most once per session, so without it a session would
	// keep the heuristic line it rendered while the call was still running and
	// only a LATER session would see the generated one. The caller marks the
	// reminder dirty — the same signal a newly discovered tool sends, because
	// the block's content is what changed.
	OnReady func()

	mu       sync.Mutex
	inFlight map[string]bool
	failed   map[string]bool
}

// SummaryKey is the hash a summary is valid for: the prompt version plus every
// tool's name and the text the summarizer reads, sorted. A renamed tool, an
// edited description, an added or removed tool — all of them change the key; a
// restart does not.
func SummaryKey(tools []*DeferredTool) string {
	sorted := make([]*DeferredTool, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	fmt.Fprintf(h, "v%s\x00", summaryPromptVersion)
	for _, t := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00", t.Name, summaryToolText(t))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// summaryToolText is what the summarizer is shown about one tool.
func summaryToolText(t *DeferredTool) string {
	return strutil.FirstLineOrTruncate(t.Description, summaryToolTextLimit)
}

// Generate answers with the cached summary for this tool set, or with "" while
// a background generation runs. It never blocks and never returns an error the
// caller has to handle: the fallback is the heuristic layer, which always works.
func (g *SummaryGenerator) Generate(server string, tools []*DeferredTool) (string, error) {
	if g == nil || len(tools) == 0 {
		return "", nil
	}
	key := SummaryKey(tools)
	if g.Cache != nil {
		if text, ok := g.Cache.Load(server, key); ok {
			return text, nil
		}
	}
	if g.giveUp(server, key) {
		// The same tool set already failed to summarize: retrying it on every
		// message would spend a call per turn to fail the same way.
		return "", nil
	}

	g.mu.Lock()
	if g.inFlight == nil {
		g.inFlight = map[string]bool{}
	}
	flightKey := server + "\x00" + key
	if g.inFlight[flightKey] {
		g.mu.Unlock()
		return "", nil
	}
	g.inFlight[flightKey] = true
	g.mu.Unlock()

	kick := g.Kick
	if kick == nil {
		kick = func(f func()) { go f() }
	}
	kick(func() {
		defer func() {
			g.mu.Lock()
			delete(g.inFlight, flightKey)
			g.mu.Unlock()
		}()
		if _, err := g.generateNow(server, tools, key); err != nil {
			g.markFailed(server, key)
		}
	})
	return "", nil
}

// generateNow is the work itself: cache, call, store. Exported to the package's
// tests only — production reaches it through Generate's background kick.
func (g *SummaryGenerator) generateNow(server string, tools []*DeferredTool, key string) (string, error) {
	if g.Provider == nil {
		return "", fmt.Errorf("no provider for MCP server summaries")
	}
	provider := g.Provider()
	if provider == nil {
		return "", fmt.Errorf("no provider for MCP server summaries")
	}

	ctx, cancel := context.WithTimeout(context.Background(), summaryTimeout)
	defer cancel()
	ctx = llm.WithUsageKind(ctx, llm.UsageKindMCPSummary)
	if g.SessionID != nil {
		if sid := g.SessionID(); sid != "" {
			ctx = llm.WithSessionID(ctx, sid)
		}
	}

	prompt := summaryPrompt(server, tools)
	resp, err := provider.CreateChat(ctx, []llm.Message{
		{Role: "system", Content: summarySystemPrompt},
		{Role: "user", Content: prompt},
	}, nil, llm.ChatOptions{MaxTokens: summaryMaxTokens, Thinking: new(false)})
	if err != nil {
		return "", fmt.Errorf("summarize MCP server %q: %w", server, err)
	}
	text := strings.TrimSpace(resp.Content)
	if text == "" {
		return "", fmt.Errorf("summarize MCP server %q: empty response", server)
	}
	if g.Cache != nil {
		if err := g.Cache.Store(server, key, text, provider.Model(), len(tools)); err != nil {
			// The text is still usable this once; the cache is what failed.
			g.log().Warn(ctx, "MCP: server summary not cached", "server", server, "err", err)
		}
	}
	g.log().Info(ctx, "MCP: server summary generated",
		"server", server, "tools", len(tools), "model", provider.Model(), "chars", len(text))
	if g.OnReady != nil {
		g.OnReady()
	}
	return text, nil
}

func (g *SummaryGenerator) log() *logger.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return logger.Default()
}

func (g *SummaryGenerator) giveUp(server, key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failed[server+"\x00"+key]
}

func (g *SummaryGenerator) markFailed(server, key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failed == nil {
		g.failed = map[string]bool{}
	}
	g.failed[server+"\x00"+key] = true
}

const summarySystemPrompt = `You describe one MCP server to another agent that cannot see its tools. That agent will search this server by describing what it needs, so your text is what makes it think of this server at all.

Write about the DOMAINS and SCENARIOS the server covers, and the kind of data or actions it offers.

Rules:
- 1 to 2 sentences. No markdown, no preamble, no lists.
- Do NOT enumerate the tools or copy their names; name at most one or two if that is the clearest way to say what the server is for.
- End with exactly: （共 <N> 个工具）
- Write in the language most of the tool descriptions are in; keep proper nouns in their original form.`

// summaryPrompt lists the tools the model has to summarize. The names are here
// — the model has to know what it is describing — while the OUTPUT is asked not
// to repeat them.
func summaryPrompt(server string, tools []*DeferredTool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Server: %s\nTools (%d):\n", server, len(tools))
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s — %s\n", t.Name, summaryToolText(t))
	}
	fmt.Fprintf(&b, "\nN = %d", len(tools))
	return b.String()
}

// SummaryCache is generated summaries on disk, one small JSON file per server.
//
// Per server rather than one file for all of them: a server's tool set changes
// on its own schedule, so a rewrite of the whole file for one server's new tool
// would make every other server's entry a concurrent-write hazard. Writes are
// atomic (tmp + rename, 0600) like every other state file here, so a reader
// — another Tachi process, or this one after a crash — sees the old text or the
// new one, never half of one.
type SummaryCache struct {
	dir string
}

// NewSummaryCache returns a cache under dir (usually config.MCPSummariesDir()).
// An empty dir disables writes and always misses.
func NewSummaryCache(dir string) *SummaryCache { return &SummaryCache{dir: dir} }

// Dir is where the cache lives, for logs and tests.
func (c *SummaryCache) Dir() string { return c.dir }

type summaryRecord struct {
	Hash        string    `json:"hash"`
	Text        string    `json:"text"`
	Model       string    `json:"model,omitempty"`
	ToolCount   int       `json:"tool_count"`
	GeneratedAt time.Time `json:"generated_at"`
}

// Load returns the stored summary for this server when it was generated from
// the same tool set. Every failure (no file, unreadable, corrupt, stale hash)
// is the same answer: a miss, with the caller falling back.
func (c *SummaryCache) Load(server, hash string) (string, bool) {
	if c == nil || c.dir == "" {
		return "", false
	}
	raw, err := os.ReadFile(c.path(server))
	if err != nil {
		return "", false
	}
	var rec summaryRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", false
	}
	if rec.Hash != hash || strings.TrimSpace(rec.Text) == "" {
		return "", false
	}
	return rec.Text, true
}

// Store writes one server's summary.
func (c *SummaryCache) Store(server, hash, text, model string, toolCount int) error {
	if c == nil || c.dir == "" {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(summaryRecord{
		Hash: hash, Text: text, Model: model, ToolCount: toolCount, GeneratedAt: time.Now(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFilePrivate(c.path(server), append(raw, '\n'))
}

// path is the file for one server. The name is sanitized: a server name comes
// from mcp.json and could carry a separator, and a cache write must not be able
// to escape its own directory.
func (c *SummaryCache) path(server string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, server)
	return filepath.Join(c.dir, safe+".json")
}
