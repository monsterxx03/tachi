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

// DeferredToolTracker reports which tools have already been discovered.
type DeferredToolTracker interface {
	Contains(name string) bool
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
	for _, t := range undiscovered {
		desc := strutil.FirstLineOrTruncate(t.Description, 100)
		lines = append(lines, fmt.Sprintf("  %s — %s", t.Name, desc))
	}

	// Add search hint at the end
	totalHint := fmt.Sprintf("(共 %d 个 MCP 工具可用。使用 MCPSearchTools 搜索并加载。)", len(all))
	if len(undiscovered) < len(all) {
		totalHint = fmt.Sprintf(
			"(共 %d 个 MCP 工具，%d 个已加载。使用 MCPSearchTools 搜索更多工具。)",
			len(all), len(all)-len(undiscovered))
	}
	lines = append(lines, "", "  "+totalHint)

	rctx.Info(ctx,
		"systemreminder: DeferredToolReminder fired",
		"undiscovered_count", len(undiscovered),
		"total_count", len(all))

	return []string{strings.Join(lines, "\n")}
}
