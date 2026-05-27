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

// CalculateCost computes the cost in CNY from a Usage and ModelPrice.
// Returns 0 if usage or price is nil, or if the price is entirely zero.
func CalculateCost(usage *Usage, price *ModelPrice) float64 {
	if usage == nil || price == nil {
		return 0
	}
	if price.InputPrice == 0 && price.OutputPrice == 0 {
		return 0
	}

	// Cache miss input tokens = total input - cache read (if cache read is reported).
	// API reports InputTokens as total (cache miss + cache hit).
	cacheMissInput := max(usage.InputTokens-usage.CacheReadInputTokens, 0)

	// Cache read price falls back to regular input price if not set.
	cacheReadPrice := price.CacheReadInputPrice
	if cacheReadPrice <= 0 {
		cacheReadPrice = price.InputPrice
	}

	// Cache creation price falls back to regular input price if not set.
	cacheCreationPrice := price.CacheCreationInputPrice
	if cacheCreationPrice <= 0 {
		cacheCreationPrice = price.InputPrice
	}

	cost := float64(cacheMissInput)/1_000_000*price.InputPrice +
		float64(usage.CacheReadInputTokens)/1_000_000*cacheReadPrice +
		float64(usage.CacheCreationInputTokens)/1_000_000*cacheCreationPrice +
		float64(usage.OutputTokens)/1_000_000*price.OutputPrice

	return cost
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
