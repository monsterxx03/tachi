package systemreminder

import (
	"context"
	"fmt"
	"strings"

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

// DeferredToolReminder injects an <available-deferred-tools> block showing
// MCP tools that are available but not yet loaded. This lets the LLM know
// what tools it can search for via MCPSearchTools.
//
// With async MCP init, tools may not be known on the very first user message
// (deferredPool is empty). The reminder fires on the first message where
// undiscovered tools exist, whether that's message #1 or #N. It fires at
// most once per session (HasFired guard), but can re-fire when Dirty is set
// to true (e.g., user manually enabled an MCP server mid-session).
//
// Implements TaggedReminder so output gets its own <available-deferred-tools>
// block independent of <system-reminder>.
type DeferredToolReminder struct {
	Provider DeferredToolProvider
	Tracker  DeferredToolTracker
	HasFired bool // set to true after generating output; prevents repeats
	Dirty    bool // when true, re-fires even if HasFired (for mid-session toggle)
}

// WrapperTag implements the TaggedReminder interface.
func (r *DeferredToolReminder) WrapperTag() string {
	return "available-deferred-tools"
}

func (r *DeferredToolReminder) Generate(ctx context.Context, rctx Context) []string {
	if r.Provider == nil {
		return nil
	}
	// Fire at most once per session, unless marked Dirty (new tools added mid-session).
	if r.HasFired && !r.Dirty {
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
		// All tools are discovered — nothing to hint about.
		// Keep HasFired as-is, but clear Dirty since there's nothing to report.
		r.Dirty = false
		return nil
	}

	r.HasFired = true
	r.Dirty = false

	var lines []string
	for _, t := range undiscovered {
		desc := strings.SplitN(t.Description, "\n", 2)[0] // first line only
		runes := []rune(desc)
		if len(runes) > 100 {
			desc = string(runes[:100]) + "..."
		}
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
