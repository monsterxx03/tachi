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

	// EstimatedInputTokens is the heuristic estimate (used by TUI statusbar).
	// When > 0, it overrides LastInputTokens for context percentage display.
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
	sb.WriteString(fmt.Sprintf("  Total input (accumulated): %s\n", formatTokens(info.InputTokens)))
	if info.LastInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  Last input (context):      %s\n", formatTokens(info.LastInputTokens)))
	}
	if info.CacheReadInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  ↳ Cache read:  %s\n", formatTokens(info.CacheReadInputTokens)))
	}
	if info.CacheCreationInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  ↳ Cache created: %s\n", formatTokens(info.CacheCreationInputTokens)))
	}
	lastInput := info.LastInputTokens
	if lastInput == 0 {
		lastInput = info.InputTokens
	}
	cacheMissInput := max(lastInput-info.CacheReadInputTokens, 0)
	if cacheMissInput != lastInput {
		sb.WriteString(fmt.Sprintf("  ↳ Cache miss:  %s\n", formatTokens(cacheMissInput)))
	}
	sb.WriteString(fmt.Sprintf("  Output tokens: %s\n", formatTokens(info.OutputTokens)))
	sb.WriteString(fmt.Sprintf("  Total tokens:  %s\n", formatTokens(info.InputTokens+info.OutputTokens)))

	// Context percentage
	if info.ContextWindow > 0 {
		estInput := info.EstimatedInputTokens
		if estInput == 0 {
			estInput = lastInput
		}
		if estInput > 0 {
			pct := float64(estInput) / float64(info.ContextWindow) * 100
			sb.WriteString(fmt.Sprintf("  Context: %s / %s (%.0f%%)\n",
				formatTokens(estInput), formatTokens(info.ContextWindow), pct))
		}
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
// helpers
// ---------------------------------------------------------------------------

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
