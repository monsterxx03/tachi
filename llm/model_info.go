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
// reasoning families (o1/o3/o4/gpt-5/gpt-6). These models reject max_tokens in
// favor of max_completion_tokens — the go-openai client validates this and
// errors out otherwise.
func isReasoningModelPrefix(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "gpt-5") ||
		strings.HasPrefix(m, "gpt-6")
}

// IsReasoningModelPrefix reports whether the model belongs to OpenAI's
// reasoning families (o1/o3/o4/gpt-5/gpt-6). Exported for config validation —
// these models have mandatory reasoning and ignore "thinking off" requests.
func IsReasoningModelPrefix(model string) bool {
	return isReasoningModelPrefix(model)
}

// ModelSupportsReasoningEffortNone reports whether the Responses API may be
// asked to turn reasoning OFF for this model (reasoning.effort = "none").
//
// It is not the same question as isReasoningModelPrefix: gpt-6 Astra and
// gpt-6.1 Sol are reasoning models that REJECT "none"/"minimal", so sending one
// to them fails the whole request. The answer is therefore two facts together —
// it is a reasoning model at all (a non-reasoning one gets no reasoning field),
// and the registry does not record it as rejecting "none"
// (builtinModel.rejectsEffortNone), the same one place a model's other
// capabilities live.
//
// A model with no record keeps the answer it had before this capability
// existed (send it), because this query exists to STOP sending a value we now
// know is rejected — not to start withholding one on the strength of a missing
// row. The gap that leaves is a future variant of a family whose known members
// accept "none": it inherits the family record's answer, so a model OpenAI
// ships that rejects "none" needs its name added to the table
// (builtinModel.rejectsEffortNone) to be handled.
func ModelSupportsReasoningEffortNone(model string) bool {
	if !isReasoningModelPrefix(model) {
		return false
	}
	r := lookup(model)
	return r == nil || !r.rejectsEffortNone
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
