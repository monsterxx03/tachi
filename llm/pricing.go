package llm

import "strings"

// ModelPrice represents the pricing per 1M tokens in CNY.
//
// A price of 0 for a category means "not charged" (free). Only explicit
// positive values are billed. A fully unpriced model (Input & Output both
// 0) incurs no cost at all.
//
// 缓存价格原则：厂商的计费项没列（= 0）就是不计费——收费的厂商会明码标价
// （如 Anthropic 的缓存写入、MiniMax 的 ¥2.625/M）。不要用 fallback 猜测
// 未列价格。
type ModelPrice struct {
	InputPrice              float64 // CNY per 1M input tokens (cache miss)
	OutputPrice             float64 // CNY per 1M output tokens
	CacheReadInputPrice     float64 // CNY per 1M cache read input tokens (0 = free)
	CacheCreationInputPrice float64 // CNY per 1M cache creation input tokens (0 = free)
}

// GetBuiltinModelPrice returns the built-in price for a given model, or nil if unknown.
// Unknown models return nil, meaning no cost will be calculated.
func GetBuiltinModelPrice(model string) *ModelPrice {
	model = strings.ToLower(model)

	switch {
	case strings.Contains(model, "deepseek"):
		return getDeepSeekPrice(model)
	case strings.Contains(model, "glm-5.2"):
		// 智谱 GLM-5.2（国内版）。Source: https://bigmodel.cn/pricing
		// 缓存存储（写入）限时免费 → CacheCreationInputPrice = 0。
		return &ModelPrice{
			InputPrice:          8.0,
			OutputPrice:         28.0,
			CacheReadInputPrice: 2.0,
		}
	case strings.Contains(model, "kimi-k3"):
		// 月之暗面 Kimi K3（国内版）。Source: https://platform.kimi.com
		// 官方价格表只有三价（输入/输出/缓存命中），未列缓存写入费 = 免费。
		return &ModelPrice{
			InputPrice:          20.0,
			OutputPrice:         100.0,
			CacheReadInputPrice: 2.0,
		}
	case strings.Contains(model, "mimo-v2.5-pro"):
		// 小米 MiMo-V2.5 Pro（国内版）。Source: https://mimo.mi.com/docs/zh-CN/price/pay-as-you-go
		// 缓存写入限时免费 → CacheCreationInputPrice = 0。
		return &ModelPrice{
			InputPrice:          3.0,
			OutputPrice:         6.0,
			CacheReadInputPrice: 0.025,
		}
	case strings.Contains(model, "mimo-v2.5"):
		// 小米 MiMo-V2.5（国内版）。Source: https://mimo.mi.com/docs/zh-CN/price/pay-as-you-go
		// 缓存写入限时免费 → CacheCreationInputPrice = 0。
		return &ModelPrice{
			InputPrice:          1.0,
			OutputPrice:         2.0,
			CacheReadInputPrice: 0.02,
		}
	}

	return nil
}

// getDeepSeekPrice returns pricing for DeepSeek models.
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
//
// DeepSeek 的上下文硬盘缓存（kv_cache）没有"缓存写入费"这一计费项：每个
// 请求自动触发缓存构建（落盘），官方文档只区分命中/未命中两类输入计费。
// 未列写入费 = 免费 → CacheCreationInputPrice = 0。
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
		return &ModelPrice{
			InputPrice:          3.0,
			OutputPrice:         6.0,
			CacheReadInputPrice: 0.025,
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

// CacheReadCreationPrice returns the cache read/creation unit prices as
// configured. 0 means "not charged" (free) — only explicit positive values
// are billed (厂商没列的价格就是不计费，不做 fallback 猜测).
func CacheReadCreationPrice(price *ModelPrice) (cacheRead, cacheCreation float64) {
	return price.CacheReadInputPrice, price.CacheCreationInputPrice
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
// UsageRow.Cost): cache-miss input normalization per provider family, and
// cache prices as configured (0 = not charged). Returns 0 for nil inputs or
// a fully unpriced model.
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
