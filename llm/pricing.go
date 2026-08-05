package llm

import "strings"

// ModelPrice represents the pricing per 1M tokens in CNY.
// Zero values mean "not configured" — no cost will be calculated for that category.
type ModelPrice struct {
	InputPrice              float64 // CNY per 1M input tokens (cache miss)
	OutputPrice             float64 // CNY per 1M output tokens
	CacheReadInputPrice     float64 // CNY per 1M cache read input tokens (0 = use InputPrice)
	CacheCreationInputPrice float64 // CNY per 1M cache creation input tokens (0 = use InputPrice)
}

// GetBuiltinModelPrice returns the built-in price for a given model, or nil if unknown.
// Unknown models return nil, meaning no cost will be calculated.
func GetBuiltinModelPrice(model string) *ModelPrice {
	model = strings.ToLower(model)

	switch {
	case strings.Contains(model, "deepseek"):
		return getDeepSeekPrice(model)
	}

	return nil
}

// getDeepSeekPrice returns pricing for DeepSeek models.
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
func getDeepSeekPrice(model string) *ModelPrice {
	switch {
	case strings.Contains(model, "deepseek-v4-flash"),
		strings.Contains(model, "deepseek-chat"):
		return &ModelPrice{
			InputPrice:          1.0,
			OutputPrice:         2.0,
			CacheReadInputPrice: 0.02,
		}
	case strings.Contains(model, "deepseek-v4-pro"),
		strings.Contains(model, "deepseek-reasoner"):
		// 2.5折优惠价 (有效期至 2026/05/31 23:59)
		return &ModelPrice{
			InputPrice:          3.0,
			OutputPrice:         6.0,
			CacheReadInputPrice: 0.1,
		}
	default:
		// Unknown DeepSeek variant — use flash pricing as conservative default
		return &ModelPrice{
			InputPrice:          1.0,
			OutputPrice:         2.0,
			CacheReadInputPrice: 0.02,
		}
	}
}

// ResolveModelPrice resolves the effective pricing for a model.
// It checks provider-level overrides first (nil ptr means "not set"), then falls
// back to built-in pricing. Returns nil if no pricing is available.
func ResolveModelPrice(model string, inputPrice, outputPrice, cacheReadPrice, cacheCreationPrice *float64) *ModelPrice {
	// Check for provider-level overrides
	if inputPrice != nil || outputPrice != nil || cacheReadPrice != nil || cacheCreationPrice != nil {
		p := &ModelPrice{}
		if inputPrice != nil {
			p.InputPrice = *inputPrice
		}
		if outputPrice != nil {
			p.OutputPrice = *outputPrice
		}
		if cacheReadPrice != nil {
			p.CacheReadInputPrice = *cacheReadPrice
		}
		if cacheCreationPrice != nil {
			p.CacheCreationInputPrice = *cacheCreationPrice
		}
		return p
	}

	// Fall back to built-in pricing
	return GetBuiltinModelPrice(model)
}

// NormalizeCacheMissInput returns the cache-miss input token count for a
// provider family. OpenAI-style APIs (openai / openai-res) report
// input_tokens INCLUDING cache-read tokens; Anthropic does not. Billing a
// hit token at both input and cache-read prices would double-count it, so
// cache reads are subtracted everywhere except Anthropic.
func NormalizeCacheMissInput(input, cacheRead int64, providerType string) int64 {
	if providerType == ProviderTypeAnthropic {
		return input
	}
	return max(input-cacheRead, 0)
}

// CacheReadCreationPrice resolves the effective cache read/creation unit
// prices for a model, falling back to the regular input price when unset
// (0 means "not configured").
func CacheReadCreationPrice(price *ModelPrice) (cacheRead, cacheCreation float64) {
	cacheRead = price.CacheReadInputPrice
	if cacheRead <= 0 {
		cacheRead = price.InputPrice
	}
	cacheCreation = price.CacheCreationInputPrice
	if cacheCreation <= 0 {
		cacheCreation = price.InputPrice
	}
	return cacheRead, cacheCreation
}

// costFromParts is the shared per-1M-token cost arithmetic over already
// normalized token counts and already resolved unit prices. Used by
// UsageRow.Cost (rows carry normalized tokens + snapshot prices) and by
// CostForUsage (raw API usage + resolved price).
func costFromParts(input, cacheRead, cacheCreation, output int64, inputPrice, outputPrice, cacheReadPrice, cacheCreationPrice float64) float64 {
	return float64(input)/1_000_000*inputPrice +
		float64(cacheRead)/1_000_000*cacheReadPrice +
		float64(cacheCreation)/1_000_000*cacheCreationPrice +
		float64(output)/1_000_000*outputPrice
}

// CostForUsage computes the CNY cost of an API call's usage, applying the
// usage ledger's exact billing rules (RecordingProvider.record +
// UsageRow.Cost): cache-miss input normalization per provider family and
// cache price fallbacks. Returns 0 for nil inputs or a fully unpriced model.
//
// providerType is llm.ProviderTypeAnthropic vs anything else (OpenAI-family)
// and selects the input normalization scale.
func CostForUsage(usage *Usage, price *ModelPrice, providerType string) float64 {
	if usage == nil || price == nil {
		return 0
	}
	if price.InputPrice == 0 && price.OutputPrice == 0 {
		return 0
	}
	cacheReadPrice, cacheCreationPrice := CacheReadCreationPrice(price)
	return costFromParts(
		NormalizeCacheMissInput(usage.InputTokens, usage.CacheReadInputTokens, providerType),
		usage.CacheReadInputTokens,
		usage.CacheCreationInputTokens,
		usage.OutputTokens,
		price.InputPrice,
		price.OutputPrice,
		cacheReadPrice,
		cacheCreationPrice,
	)
}
