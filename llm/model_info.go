package llm

import (
	"slices"
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

// ModelThinkingEfforts returns the effort levels the model's own API accepts,
// from the built-in registry (builtin_models.go) — the same source of truth as
// pricing, context windows and vision. nil means "no claim", i.e. every level
// is available, which is the answer for every model the registry says nothing
// about (and for every model before this capability existed).
//
// A level outside this set is not ignored by the API but REJECTED, which fails
// the whole request — so a frontend offers only these, and a configured
// thinking level outside them is a config contradiction worth warning about.
// A provider whose endpoint differs from the vendor (a gateway mapping effort
// to its own parameter, say) overrides the answer per provider with
// spec.thinking_efforts — explicit config wins, exactly as spec.vision wins
// over ModelSupportsVision.
//
// The returned slice is a copy: the registry is shared state, and callers put
// this set on a resolved provider they then hand around.
func ModelThinkingEfforts(model string) []string {
	r := lookup(model)
	if r == nil {
		return nil
	}
	return slices.Clone(r.thinkingEfforts)
}

// ModelSupportsReasoningEffortNone reports whether the Responses API may be
// asked to turn reasoning OFF for this model (reasoning.effort = "none").
//
// It is not the same question as isReasoningModelPrefix: gpt-6 Astra and
// gpt-6.1 Sol are reasoning models that REJECT "none"/"minimal", so sending one
// to them fails the whole request. The answer is therefore two facts together —
// it is a reasoning model at all (a non-reasoning one gets no reasoning field),
// and the registry does not exclude "none" from the model's effort set
// (builtinModel.thinkingEfforts), the same one place a model's other
// capabilities live.
//
// A model with no record keeps the answer it had before this capability
// existed (send it), because this query exists to STOP sending a value we now
// know is rejected — not to start withholding one on the strength of a missing
// row. The gap that leaves is a future variant of a family whose known members
// accept "none": it inherits the family record's answer, so a model OpenAI
// ships that rejects "none" needs its name added to the table
// (builtinModel.thinkingEfforts) to be handled.
func ModelSupportsReasoningEffortNone(model string) bool {
	if !isReasoningModelPrefix(model) {
		return false
	}
	efforts := ModelThinkingEfforts(model)
	return efforts == nil || slices.Contains(efforts, "none")
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
