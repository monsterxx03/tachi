package commands

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent/skill"
)

// ---------------------------------------------------------------------------
// /usage formatting
// ---------------------------------------------------------------------------

// UsageReportInfo contains the data needed to format a usage report.
// This is a presentation-oriented struct — callers populate it from their
// own SessionUsageReport + local state.
type UsageReportInfo struct {
	SessionID     string
	Provider      string
	Model         string
	Title         string
	ContextWindow int64

	InputTokens              int64
	LastInputTokens          int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	OutputTokens             int64

	// EstimatedInputTokens is the local heuristic estimate (chars/4) of
	// the most recent API call's input tokens, set by estimateAndUpdateTokens
	// before each LLM call. This is the numerator used for context percentage
	// display across TUI, channel, and ACP modes.
	EstimatedInputTokens int64

	Cost float64

	ToolCalls map[string]*ToolCallStat
	MainCount int
	SubCount  int
}

// ToolCallStat records per-tool call counts and error counts.
type ToolCallStat struct {
	Count    int
	ErrCount int
}

// FormatUsageReport produces a markdown-formatted usage report string.
func FormatUsageReport(info *UsageReportInfo) string {
	var sb strings.Builder

	// Header
	sb.WriteString("📊 **Session Usage**\n\n")

	// Session info
	sb.WriteString(fmt.Sprintf("**Session:** `%s`\n", info.SessionID))
	provider := info.Provider
	if provider == "" {
		provider = "(unknown)"
	}
	sb.WriteString(fmt.Sprintf("**Provider:** %s\n", provider))
	sb.WriteString(fmt.Sprintf("**Model:** %s\n", info.Model))
	title := info.Title
	if title == "" {
		title = "(untitled)"
	}
	sb.WriteString(fmt.Sprintf("**Title:** %s\n\n", title))

	// Token usage
	sb.WriteString("**Token Usage**\n")
	sb.WriteString(fmt.Sprintf("  Total input (accumulated): %s\n", FormatTokens(info.InputTokens)))
	if info.LastInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  Last input (context):      %s\n", FormatTokens(info.LastInputTokens)))
	}
	if info.CacheReadInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  ↳ Cache read:  %s\n", FormatTokens(info.CacheReadInputTokens)))
	}
	if info.CacheCreationInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  ↳ Cache created: %s\n", FormatTokens(info.CacheCreationInputTokens)))
	}
	lastInput := info.LastInputTokens
	if lastInput == 0 {
		lastInput = info.InputTokens
	}
	cacheMissInput := max(lastInput-info.CacheReadInputTokens, 0)
	if cacheMissInput != lastInput {
		sb.WriteString(fmt.Sprintf("  ↳ Cache miss:  %s\n", FormatTokens(cacheMissInput)))
	}
	sb.WriteString(fmt.Sprintf("  Output tokens: %s\n", FormatTokens(info.OutputTokens)))
	sb.WriteString(fmt.Sprintf("  Total tokens:  %s\n", FormatTokens(info.InputTokens+info.OutputTokens)))

	// Context percentage — uses EstimatedInputTokens as the sole numerator.
	// No fallback: if the heuristic estimate is unavailable (0), the context
	// fraction is simply not shown.
	if info.ContextWindow > 0 && info.EstimatedInputTokens > 0 {
		pct := float64(info.EstimatedInputTokens) / float64(info.ContextWindow) * 100
		sb.WriteString(fmt.Sprintf("  Context: %s / %s (%.0f%%)\n",
			FormatTokens(info.EstimatedInputTokens), FormatTokens(info.ContextWindow), pct))
	}

	// Cost
	sb.WriteString("\n**Cost**\n")
	if info.Cost <= 0 {
		sb.WriteString("  No pricing data available\n")
	} else {
		sb.WriteString(fmt.Sprintf("  Total cost: **¥%.4f**\n", info.Cost))
	}

	// Tool calls
	sb.WriteString("\n**Tool Calls**\n")
	names := slices.Sorted(maps.Keys(info.ToolCalls))
	for _, name := range names {
		st := info.ToolCalls[name]
		line := fmt.Sprintf("  - **%s**: %d call(s)", name, st.Count)
		if st.ErrCount > 0 {
			line += fmt.Sprintf(" (%d failed)", st.ErrCount)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString(fmt.Sprintf("\n  **Total:** %d main + %d subagent = **%d** call(s)\n",
		info.MainCount, info.SubCount, info.MainCount+info.SubCount))

	return sb.String()
}

// ---------------------------------------------------------------------------
// /skill list formatting
// ---------------------------------------------------------------------------

// FormatSkillList produces a markdown-formatted list of available skills.
func FormatSkillList(metas []skill.SkillMeta) string {
	if len(metas) == 0 {
		return "No skills found. Create a skill by adding a `SKILL.md` file in `.tachi/skills/<name>/` or `~/.tachi/skills/<name>/`."
	}

	var sb strings.Builder
	sb.WriteString("**Available Skills:**\n\n")

	for _, meta := range metas {
		sourceTag := ""
		if meta.Source == "project" {
			sourceTag = " 🏠"
		}
		sb.WriteString(fmt.Sprintf("- **%s**%s\n", meta.Name, sourceTag))
		sb.WriteString(fmt.Sprintf("  %s\n", meta.Description))
		if len(meta.Tags) > 0 {
			sb.WriteString(fmt.Sprintf("  Tags: %s\n", strings.Join(meta.Tags, ", ")))
		}
		sb.WriteString(fmt.Sprintf("  Use `/%s` to activate\n\n", meta.Name))
	}
	sb.WriteString(fmt.Sprintf("%d skill(s) total", len(metas)))

	return sb.String()
}

// ---------------------------------------------------------------------------
// /mcp list formatting
// ---------------------------------------------------------------------------

// MCPServerInfo holds the data needed to format one MCP server entry.
type MCPServerInfo struct {
	Name      string
	Status    string // e.g. "🟢 Connected", "🔴 Disconnected", "⚪ Disabled"
	Transport string // e.g. "`stdio` — `cmd`" or "`http` — `url`"
	OAuth     string // optional OAuth status line (empty to omit)
	Tools     []MCPToolInfo
	// ToolsPending indicates tools exist but haven't been discovered yet.
	ToolsPending bool
}

// MCPToolInfo describes a single tool within an MCP server.
type MCPToolInfo struct {
	Name       string // short name (without mcp__server__ prefix)
	Discovered bool   // whether it's been loaded into the active set
}

// FormatMCPList produces a markdown-formatted list of MCP servers with their
// status, transport, OAuth info, and tool lists.
func FormatMCPList(servers []MCPServerInfo) string {
	if len(servers) == 0 {
		return "No MCP servers configured."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**MCP Servers** (%d)\n\n", len(servers)))

	for i, srv := range servers {
		sb.WriteString(fmt.Sprintf("**%s** [%s]\n%s\n", srv.Name, srv.Status, srv.Transport))

		if srv.OAuth != "" {
			sb.WriteString(fmt.Sprintf("OAuth: %s\n", srv.OAuth))
		}

		if len(srv.Tools) > 0 {
			discoveredCount := 0
			for _, t := range srv.Tools {
				if t.Discovered {
					discoveredCount++
				}
			}
			sb.WriteString(fmt.Sprintf("**%d** tools (%d loaded)\n", len(srv.Tools), discoveredCount))
			for _, t := range srv.Tools {
				marker := "○"
				if t.Discovered {
					marker = "✓"
				}
				sb.WriteString(fmt.Sprintf("- %s `%s`\n", marker, t.Name))
			}
		} else if srv.ToolsPending {
			sb.WriteString("_tools pending discovery_\n")
		}

		if i < len(servers)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func FormatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
