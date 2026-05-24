package systemreminder

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/pkg/debuglog"
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
// most once per session (HasFired guard).
//
// Implements TaggedReminder so output gets its own <available-deferred-tools>
// block independent of <system-reminder>.
type DeferredToolReminder struct {
	Provider  DeferredToolProvider
	Tracker   DeferredToolTracker
	HasFired  bool // set to true after generating output; prevents repeats
}

// WrapperTag implements the TaggedReminder interface.
func (r *DeferredToolReminder) WrapperTag() string {
	return "available-deferred-tools"
}

func (r *DeferredToolReminder) Generate(ctx Context) []string {
	if r.Provider == nil {
		return nil
	}
	// Fire at most once per session. This works correctly with async MCP
	// init: if the pool is empty on the first message, we skip; when MCP
	// connects and tools become available, the reminder fires on the next
	// message (only once).
	if r.HasFired {
		return nil
	}
	// Don't inject at tool-result boundaries — not meaningful there.
	if ctx.IsToolResult {
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
		return nil
	}

	r.HasFired = true

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

	debuglog.DefaultLogger.Log(
		"systemreminder: DeferredToolReminder: %d undiscovered of %d total (fired once)",
		len(undiscovered), len(all))

	return []string{strings.Join(lines, "\n")}
}
