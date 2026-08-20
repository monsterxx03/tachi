package llm

import (
	"strings"
)

// ModelContextWindow returns the context window size (in tokens) for a given
// model, or 0 if unknown (the caller then uses a configured or hardcoded
// default). The mapping lives in the built-in registry (builtin_models.go) —
// the same source of truth as pricing and vision support.
func ModelContextWindow(model string) int64 {
	if r := lookup(model); r != nil {
		return r.context
	}
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
// (multi-modal) input, based on its name — resolved from the built-in
// registry (builtin_models.go), the same source of truth as pricing and
// context windows. It is deliberately conservative: only well-known vision
// families return true, and unknown model names return false — sending image
// parts to a text-only model errors out, whereas describing images through a
// vision-capable provider is always safe. Users can override the result
// per-provider via spec.vision (see config.ModelSpec.Vision).
func ModelSupportsVision(model string) bool {
	if r := lookup(model); r != nil {
		return r.vision
	}
	return false
}
