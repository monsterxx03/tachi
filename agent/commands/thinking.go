package commands

import (
	"fmt"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/llm"
)

// ThinkingEffortLevels are the concrete selectable effort levels (without
// "default"), shared by /thinking and clients that only offer concrete
// levels (e.g. the ACP "Reasoning Effort" config option).
var ThinkingEffortLevels = []string{"none", "low", "medium", "high", "xhigh", "max"}

// ThinkingLevels are the accepted /thinking argument values:
// ThinkingEffortLevels plus "default" (revert to the provider/model default).
// "none" disables thinking mode; the other effort levels set the thinking
// effort. Levels are passed through to the API as-is — providers that support
// a subset map the effort server-side (see DeepSeek thinking_mode docs).
var ThinkingLevels = append(append([]string{}, ThinkingEffortLevels...), "default")

// ThinkingLevelDescriptions maps each level to a short human-readable
// description, used for autocomplete and help output.
var ThinkingLevelDescriptions = map[string]string{
	"none":    "Disable thinking mode",
	"low":     "Low thinking effort",
	"medium":  "Medium thinking effort",
	"high":    "High thinking effort",
	"xhigh":   "Extra-high thinking effort",
	"max":     "Maximum thinking effort",
	"default": "Use provider/model default",
}

// IsValidThinkingLevel reports whether level is a valid /thinking argument.
func IsValidThinkingLevel(level string) bool {
	return slices.Contains(ThinkingLevels, level)
}

// ThinkingOverrideFromLevel maps a non-default level to the (thinking, effort)
// values for AIAgent.SetThinking. "none" disables thinking mode (false);
// the remaining levels leave the thinking switch at nil (provider/model
// default) and set the effort — passed through unchanged for the API to map
// to its actual inference effort (no client-side normalization).
func ThinkingOverrideFromLevel(level string) (thinking *bool, effort string) {
	if level == "none" {
		v := false
		return &v, ""
	}
	return nil, level
}

// ThinkingLevelOf maps an agent's thinking config back to its level string:
// "none" when thinking is explicitly disabled, the effort level when set,
// or "" when unset (provider/model default). It is the inverse of
// ThinkingOverrideFromLevel — useful for displaying the current state.
func ThinkingLevelOf(thinking *bool, effort string) string {
	if thinking != nil && !*thinking {
		return "none"
	}
	return effort
}

// EffectiveThinking resolves the thinking config to apply for a session:
// the session's per-session ThinkingLevel override (set via /thinking) wins;
// an empty (or "default") level falls back to the provider's resolved
// defaults (resolved.Thinking / resolved.ThinkingEffort).
func EffectiveThinking(level string, resolved llm.ResolvedProvider) (thinking *bool, effort string) {
	if level == "" || level == "default" {
		return resolved.Thinking, resolved.ThinkingEffort
	}
	return ThinkingOverrideFromLevel(level)
}

// FormatThinkingOptions returns the valid /thinking levels with their
// descriptions, used for help/status output.
func FormatThinkingOptions() string {
	var b strings.Builder
	for _, l := range ThinkingLevels {
		fmt.Fprintf(&b, "- **%s** — %s\n", l, ThinkingLevelDescriptions[l])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// FormatThinkingStatus renders the /thinking status block: the current
// session's level plus the valid options. current is the level to display
// ("default" when no per-session override is set).
func FormatThinkingStatus(current string) string {
	return fmt.Sprintf(
		"🧠 当前思考级别: **%s**（仅当前会话生效）\n\n可选级别:\n%s\n\n使用 `/thinking <level>` 切换。",
		current, FormatThinkingOptions())
}
