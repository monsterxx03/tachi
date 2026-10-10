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

// IsThinkingLevelSupported reports whether a level is accepted by a provider
// that DECLARED its endpoint's efforts (llm.ResolvedProvider.ThinkingEfforts).
// An empty declared list means "every level is available", which is the case
// for every provider that doesn't set the field. "" and "default" (follow the
// provider's own thinking_level) are not efforts and are always supported.
//
// The distinction from IsValidThinkingLevel is the whole point: a level can be
// a perfectly valid argument that this particular endpoint will reject whole
// (see config.ModelSpec.ThinkingEfforts), and the frontends refuse such a
// level locally instead of letting the session's first request fail.
func IsThinkingLevelSupported(level string, supported []string) bool {
	if level == "" || level == "default" || len(supported) == 0 {
		return true
	}
	return slices.Contains(supported, level)
}

// SupportedThinkingLevels returns the levels selectable for a provider that
// declared supported, in ThinkingLevels order so the display order never
// depends on how the config listed them. "default" (follow the provider's own
// thinking_level) is not an effort and is always kept; an empty declared list
// returns every level.
func SupportedThinkingLevels(supported []string) []string {
	if len(supported) == 0 {
		return ThinkingLevels
	}
	out := make([]string, 0, len(supported)+1)
	for _, l := range ThinkingLevels {
		if l == "default" || slices.Contains(supported, l) {
			out = append(out, l)
		}
	}
	return out
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

// FormatThinkingOptions returns the levels valid for a provider (supported =
// its declared efforts; empty = all) with their descriptions, used for
// help/status output.
func FormatThinkingOptions(supported []string) string {
	var b strings.Builder
	for _, l := range SupportedThinkingLevels(supported) {
		fmt.Fprintf(&b, "- **%s** — %s\n", l, ThinkingLevelDescriptions[l])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// FormatThinkingStatus renders the /thinking status block: the current
// session's level plus the levels valid for the provider. current is the level
// to display ("default" when no per-session override is set).
func FormatThinkingStatus(current string, supported []string) string {
	return fmt.Sprintf(
		"🧠 当前思考级别: **%s**（仅当前会话生效）\n\n可选级别:\n%s\n\n使用 `/thinking <level>` 切换。",
		current, FormatThinkingOptions(supported))
}

// FormatThinkingRejected explains that a syntactically valid level is not
// accepted by the provider's endpoint, and lists what is. providerName is the
// provider the level would have been sent to (empty drops the parenthetical).
// Shared by every frontend so the three surfaces word it the same way.
func FormatThinkingRejected(level, providerName string, supported []string) string {
	who := ""
	if providerName != "" {
		who = fmt.Sprintf("（provider %s）", providerName)
	}
	return fmt.Sprintf("当前 provider%s 的端点不接受思考级别 **%s**。\n\n可选级别:\n%s",
		who, level, FormatThinkingOptions(supported))
}
