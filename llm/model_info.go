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
	case strings.Contains(model, "minimax-m3"):
		// MiniMax-M3: 1M 上下文 + 原生多模态(text/vision/video)。
		// Source: https://platform.minimaxi.com/docs/guides/text-generation
		return 1_000_000
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

// ModelSupportsVision reports whether a model is known to accept image
// (multi-modal) input, based on its name. This is the built-in capability
// table used as the fallback when the provider's spec does not explicitly
// set vision. It is deliberately conservative: only well-known vision
// families return true, and unknown model names return false — sending
// image parts to a text-only model errors out, whereas describing images
// through a vision-capable provider is always safe. Users can override the
// result per-provider via spec.vision (see config.ModelSpec.Vision).
func ModelSupportsVision(model string) bool {
	m := strings.ToLower(model)

	// Claude 3+ family — all variants accept images.
	if strings.Contains(m, "claude") {
		return true
	}
	// OpenAI vision-capable families (gpt-4o / gpt-4.1 / gpt-5 / o4 /
	// gpt-4-turbo / gpt-4-vision). o1/o3/gpt-4/gpt-4.5 are excluded: only
	// the listed variants reliably accept image input.
	if strings.Contains(m, "gpt-4o") || strings.Contains(m, "gpt-4.1") ||
		strings.Contains(m, "gpt-5") || strings.Contains(m, "o4") ||
		strings.Contains(m, "gpt-4-turbo") || strings.Contains(m, "gpt-4-vision") {
		return true
	}
	// Qwen vision variants (qwen-vl, qwen2.5-vl, qwen3-vl, ...). The plain
	// "qwen" family is text-only, so the "vl" marker is required.
	if strings.Contains(m, "qwen") && strings.Contains(m, "vl") {
		return true
	}
	// Zhipu GLM vision variants (glm-4v / glm-4.1v / glm-4.5v). The plain
	// glm family (glm-4, glm-4.5, glm-4.6) is text-only.
	if strings.Contains(m, "glm-4v") || strings.Contains(m, "glm-4.1v") || strings.Contains(m, "glm-4.5v") {
		return true
	}
	// Gemini family — all variants accept images.
	if strings.Contains(m, "gemini") {
		return true
	}
	// Xiaomi MiMo family — all variants accept images.
	if strings.Contains(m, "mimo") {
		return true
	}
	// Moonshot Kimi family — all variants accept images.
	if strings.Contains(m, "kimi") {
		return true
	}
	// MiniMax family — all variants accept images.
	if strings.Contains(m, "minimax") {
		return true
	}
	return false
}
