package systemreminder

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// DeferredToolProvider provides MCP tool metadata for the reminder.
type DeferredToolProvider interface {
	All() []DeferredToolRecord
}

// DeferredToolRecord is a minimal representation of a deferred MCP tool.
type DeferredToolRecord struct {
	Name        string
	Description string
}

// DeferredToolSummary is one MCP server described in a line: what domains it
// covers, not which tools it holds.
type DeferredToolSummary struct {
	// Server is the server name ("lixinger").
	Server string
	// Text says what the server covers. Already one line.
	Text string
	// Source names where Text came from (the server's own description, a
	// generated summary, a local heuristic). Logged, not rendered: a thin line
	// is diagnosable only if the log says which layer produced it.
	Source string
}

// DeferredToolSummarizer is the optional other half of DeferredToolProvider: a
// provider that implements it can be described per SERVER instead of per tool.
//
// It returns nil when the per-server shape does not apply — the caller asks
// every time, so the answer can follow a setting that changed mid-session (the
// search matcher; see agent.mcpSearcher). The per-tool catalog stays the
// fallback, which is what the keyword matcher needs and what a server with no
// description to offer gets.
type DeferredToolSummarizer interface {
	Summaries() []DeferredToolSummary
}

// DeferredToolTracker reports which tools have already been discovered.
type DeferredToolTracker interface {
	Contains(name string) bool
}

// summaryLines renders the per-SERVER shape, or nil when the provider does not
// offer one.
//
// A server with nothing left to discover is skipped: the block is a hint about
// what can still be loaded, and a fully-loaded server has nothing to hint. The
// counts stay the reminder's own (it holds the full list and the tracker), so
// the two shapes can never disagree about how much is behind a line.
func (r *DeferredToolReminder) summaryLines(all, undiscovered []DeferredToolRecord) []string {
	summarizer, ok := r.Provider.(DeferredToolSummarizer)
	if !ok {
		return nil
	}
	summaries := summarizer.Summaries()
	if len(summaries) == 0 {
		return nil
	}
	total := map[string]int{}
	remaining := map[string]int{}
	for _, t := range all {
		total[serverOf(t.Name)]++
	}
	for _, t := range undiscovered {
		remaining[serverOf(t.Name)]++
	}
	lines := make([]string, 0, len(summaries))
	var sources []string
	for _, s := range summaries {
		left := remaining[s.Server]
		if left == 0 {
			continue
		}
		text := s.Text
		if text == "" {
			text = "（该 server 未提供描述）"
		}
		lines = append(lines, fmt.Sprintf("  mcp__%s — %d 个工具（%d 个未加载）：%s",
			s.Server, total[s.Server], left, text))
		sources = append(sources, s.Server+"="+s.Source)
	}
	if len(lines) == 0 {
		return nil
	}
	r.summarySources = strings.Join(sources, ",")
	return lines
}

// serverOf reads the server segment out of a tool name ("mcp__lixinger__x" →
// "lixinger"). An unexpected shape returns the name itself, which only means
// the line is filed under a name no server will match — never a panic, and the
// reader still sees the tool.
func serverOf(toolName string) string {
	rest, ok := strings.CutPrefix(toolName, "mcp__")
	if !ok {
		return toolName
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return rest
	}
	return server
}

// DeferredToolReminder injects a deferred-tools block showing
// MCP tools that are available but not yet loaded. This lets the LLM know
// what tools it can search for via MCPSearchTools. The block is merged into
// the single <system-reminder> wrapper.
//
// With async MCP init, tools may not be known on the very first user message
// (deferredPool is empty). The reminder fires on the first message where
// undiscovered tools exist, whether that's message #1 or #N. It fires at
// most once per session (the fired guard), but can re-fire after MarkDirty
// (e.g., user manually enabled an MCP server mid-session).
type DeferredToolReminder struct {
	Provider DeferredToolProvider
	Tracker  DeferredToolTracker

	// mu guards the fire bookkeeping below. Generate runs on the agent's turn
	// goroutine, while MarkDirty is reached from a frontend's OWN goroutine (a
	// user enabling an MCP server mid-session). A plain bool would make that a
	// data race, and a lost MarkDirty means the LLM is never told about the new
	// tools — exactly the failure the flag exists to prevent.
	mu       sync.Mutex
	hasFired bool // set once output was generated; prevents repeats
	dirty    bool // when true, re-fires even if hasFired (mid-session tool change)

	// summarySources is which layer described each server on the last render
	// ("lixinger=instructions,lixinger=..."), for the log line below. Written
	// and read on the turn goroutine only.
	summarySources string
}

// MarkDirty makes the reminder fire again on the next user message, even if it
// already fired in this session. Called when deferred tools appear mid-session
// (e.g. the user enables an MCP server).
func (r *DeferredToolReminder) MarkDirty() {
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()
}

// shouldFire reports whether the once-per-session guard allows generating right
// now — true on the first message, or again after a MarkDirty.
func (r *DeferredToolReminder) shouldFire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.hasFired || r.dirty
}

// markFired records that output was generated and clears the dirty flag.
func (r *DeferredToolReminder) markFired() {
	r.mu.Lock()
	r.hasFired = true
	r.dirty = false
	r.mu.Unlock()
}

// clearDirty drops the dirty flag WITHOUT marking the reminder as fired, for a
// dirty reminder that found nothing worth reporting (every tool already loaded).
// Without it the reminder would re-inspect the pool on every later message.
func (r *DeferredToolReminder) clearDirty() {
	r.mu.Lock()
	r.dirty = false
	r.mu.Unlock()
}

func (r *DeferredToolReminder) Generate(ctx context.Context, rctx Context) []string {
	if r.Provider == nil {
		return nil
	}
	// Fire at most once per session, unless marked Dirty (new tools added mid-session).
	if !r.shouldFire() {
		return nil
	}
	// Don't inject at tool-result boundaries — not meaningful there.
	if rctx.IsToolResult {
		return nil
	}

	all := r.Provider.All()
	if len(all) == 0 {
		return nil
	}

	// Filter to only undiscovered tools
	var undiscovered []DeferredToolRecord
	for _, t := range all {
		if r.Tracker == nil || !r.Tracker.Contains(t.Name) {
			undiscovered = append(undiscovered, t)
		}
	}

	if len(undiscovered) == 0 {
		// All tools are discovered — nothing to hint about. Keep hasFired as-is,
		// but clear dirty since there's nothing to report.
		r.clearDirty()
		return nil
	}

	r.markFired()

	var lines []string
	if lines = r.summaryLines(all, undiscovered); lines == nil {
		for _, t := range undiscovered {
			desc := strutil.FirstLineOrTruncate(t.Description, 100)
			lines = append(lines, fmt.Sprintf("  %s — %s", t.Name, desc))
		}
	}

	// Add search hint at the end. It says what to DO with MCPSearchTools rather
	// than which syntax to write: the tool's own description carries the
	// matcher's advice (a sentence for the Jev gate, keywords for the pool), and
	// repeating a syntax here is how the two drift apart.
	totalHint := fmt.Sprintf("(共 %d 个 MCP 工具可用。用 MCPSearchTools 告诉它你要做什么，用中英文都可以。)", len(all))
	if len(undiscovered) < len(all) {
		totalHint = fmt.Sprintf(
			"(共 %d 个 MCP 工具，%d 个已加载。用 MCPSearchTools 描述你要做什么，可以加载更多。)",
			len(all), len(all)-len(undiscovered))
	}
	lines = append(lines, "", "  "+totalHint)

	if r.summarySources != "" {
		rctx.Info(ctx, "systemreminder: deferred-tool summaries by source", "sources", r.summarySources)
	}

	rctx.Info(ctx,
		"systemreminder: DeferredToolReminder fired",
		"undiscovered_count", len(undiscovered),
		"total_count", len(all))

	return []string{strings.Join(lines, "\n")}
}
