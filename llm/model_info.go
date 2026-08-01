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
