package llm

import (
	"strings"
)

// ModelContextWindow returns the context window size (in tokens) for a given model.
// Returns 0 if unknown, meaning the caller should use a fallback.
func ModelContextWindow(model string) int64 {
	model = strings.ToLower(model)

	// Claude models (Anthropic)
	switch {
	case strings.Contains(model, "claude"):
		switch {
		case strings.Contains(model, "claude-sonnet-4-6") ||
			strings.Contains(model, "claude-opus"):
			return 1000_000
		case strings.Contains(model, "haiku"):
			return 200_000
		default:
			// Unknown Claude variant, assume 200K as safe default
			return 200_000
		}
	case strings.Contains(model, "gpt"):
		switch {
		case strings.Contains(model, "gpt-5.4") ||
			strings.Contains(model, "gpt-5.5"):
			return 1050_000
		case strings.Contains(model, "gpt-5.3-codex"):
			return 400_000
		default:
			return 400_000
		}
	case strings.Contains(model, "qwen"):
		return 1000_000
	case strings.Contains(model, "glm"):
		return 200_000
	case strings.Contains(model, "minimax"):
		return 204_800
	case strings.Contains(model, "kimi-k3"):
		return 1000_000
	case strings.Contains(model, "kimi"):
		return 256_000
	case strings.Contains(model, "deepseek"):
		return 1000_000
	case strings.Contains(model, "mimo-2.5"):
		return 1000_000
	}

	// Unknown model: return 0 to signal "use configured or hardcoded default"
	return 0
}

// isDeepSeek reports whether the model belongs to the DeepSeek family.
// DeepSeek models support thinking mode with a top-level "thinking" field
// and "reasoning_effort" (see api-docs.deepseek.com/guides/thinking_mode).
func isDeepSeek(model string) bool {
	return strings.Contains(strings.ToLower(model), "deepseek")
}

// isReasoningModelPrefix reports whether the model belongs to OpenAI's
// reasoning families (o1/o3/o4/gpt-5). These models reject max_tokens in
// favor of max_completion_tokens — the go-openai client validates this and
// errors out otherwise.
func isReasoningModelPrefix(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "gpt-5")
}

// IsReasoningModelPrefix reports whether the model belongs to OpenAI's
// reasoning families (o1/o3/o4/gpt-5). Exported for config validation —
// these models have mandatory reasoning and ignore "thinking off" requests.
func IsReasoningModelPrefix(model string) bool {
	return isReasoningModelPrefix(model)
}

// thinkingEffortOrder ranks thinking effort levels from lowest to highest.
// Used by NormalizeThinkingEffort to degrade unsupported requests to the
// nearest supported level.
var thinkingEffortOrder = []string{"low", "medium", "high", "xhigh", "max"}

// ThinkingEffortLevels returns the thinking effort levels a model actually
// supports, or nil when unknown (callers pass the requested effort through
// unchanged). Different models support different ranges:
//
//	deepseek-v4-flash: low, high            (max degrades to high)
//	deepseek-v4-pro:   low, high, max
//
// Source: https://api-docs.deepseek.com/zh-cn/guides/thinking_mode
func ThinkingEffortLevels(model string) []string {
	if !isDeepSeek(model) {
		return nil // unknown family — no restriction
	}
	if strings.Contains(strings.ToLower(model), "v4-flash") {
		return []string{"low", "high"}
	}
	// deepseek-v4-pro (and any other deepseek variant): low/high/max
	return []string{"low", "high", "max"}
}

// NormalizeThinkingEffort maps a requested thinking effort to the model's
// supported range. Effort is passed through unchanged when:
//   - empty (caller wants the model default), or
//   - the model family is unknown, or
//   - the requested level is already supported.
//
// Unsupported requests degrade to the highest supported level that does not
// exceed the requested one (e.g. deepseek-v4-flash + "max" → "high", or
// deepseek-v4-pro + "medium" → "low"). Unknown effort values are passed
// through as-is.
func NormalizeThinkingEffort(model, effort string) string {
	if effort == "" {
		return ""
	}
	levels := ThinkingEffortLevels(model)
	if len(levels) == 0 {
		return effort
	}
	for _, l := range levels {
		if l == effort {
			return effort
		}
	}
	// Requested level not supported: degrade to the highest supported level
	// at or below the request.
	reqIdx := -1
	for i, l := range thinkingEffortOrder {
		if l == effort {
			reqIdx = i
			break
		}
	}
	if reqIdx < 0 {
		// Completely unknown effort value — pass through and let the API decide.
		return effort
	}
	best := ""
	for _, l := range levels {
		for i, o := range thinkingEffortOrder {
			if o == l && i <= reqIdx {
				best = l
			}
		}
	}
	if best != "" {
		return best
	}
	// All supported levels rank above the request (edge case) — take the lowest.
	return levels[0]
}
