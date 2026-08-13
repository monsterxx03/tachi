package llm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// ModelPrice represents the pricing per 1M tokens in CNY.
//
// A price of 0 for a category means "not charged" (free). Only explicit
// positive values are billed. A fully unpriced model (Input & Output both
// 0) incurs no cost at all.
//
// 缓存价格原则：厂商的计费项没列（= 0）就是不计费——收费的厂商会明码标价
// （如 Anthropic 的缓存写入、MiniMax 的 ¥2.625/M）。不要用 fallback 猜测
// 未列价格。
//
// 分时计价（2026-08）：Bands 携带时段表（如 DeepSeek 峰谷定价），调用
// PriceAt(t) 取 t 时刻生效的快照；无 Bands 时 PriceAt 返回自身（行为与
// 旧版完全一致）。Location 为时段判定的基准时区（nil = 本地时区）——
// 厂商通常按官方时区分时（如 DeepSeek 按北京时间），本地时区可能错位。
type ModelPrice struct {
	InputPrice              float64 // CNY per 1M input tokens (cache miss)
	OutputPrice             float64 // CNY per 1M output tokens
	CacheReadInputPrice     float64 // CNY per 1M cache read input tokens (0 = free)
	CacheCreationInputPrice float64 // CNY per 1M cache creation input tokens (0 = free)

	Location *time.Location `json:"-"` // 时段判定基准时区；nil = 本地时区
	Bands    []PriceBand    `json:"-"` // 分时时段表（首个匹配生效）；空 = 平段价
}

// PriceBand is one time-of-use pricing band: effective during
// [StartHour, EndHour) in the model's Location (see ModelPrice.Location).
// EndHour <= StartHour wraps past midnight (e.g. 23:00→07:00); EndHour ==
// StartHour therefore covers the whole day. All four unit prices are the
// FINAL effective values (0 = not charged) — inheritance from the flat
// price is applied by the resolver BEFORE building the band (see
// ResolveModelPriceAt).
type PriceBand struct {
	Name                    string // band name written to the ledger row (UsageRow.Band); "" = unnamed
	StartHour               int    // 0-23, inclusive
	EndHour                 int    // 0-24, exclusive; <= StartHour wraps past midnight
	InputPrice              float64
	OutputPrice             float64
	CacheReadInputPrice     float64
	CacheCreationInputPrice float64
}

// matches reports whether hour falls in the band's [StartHour, EndHour)
// window (already converted to the band's timezone by PriceAt).
func (b PriceBand) matches(hour int) bool {
	if b.EndHour > b.StartHour {
		return hour >= b.StartHour && hour < b.EndHour
	}
	// Wrap past midnight (EndHour <= StartHour): e.g. 23:00→07:00.
	return hour >= b.StartHour || hour < b.EndHour
}

// PriceOverrides is implemented by both ModelPrice and PriceBand — both
// carry the four unit-price fields. ApplyPriceOverrides copies non-nil
// overrides onto the receiver (nil = keep the field's current value, an
// explicit 0 = free). One method to touch when a price dimension is added.
type PriceOverrides interface {
	ApplyPriceOverrides(input, output, cacheRead, cacheCreate *float64)
}

// ApplyPriceOverrides implements PriceOverrides for ModelPrice.
func (p *ModelPrice) ApplyPriceOverrides(input, output, cacheRead, cacheCreate *float64) {
	if input != nil {
		p.InputPrice = *input
	}
	if output != nil {
		p.OutputPrice = *output
	}
	if cacheRead != nil {
		p.CacheReadInputPrice = *cacheRead
	}
	if cacheCreate != nil {
		p.CacheCreationInputPrice = *cacheCreate
	}
}

// ApplyPriceOverrides implements PriceOverrides for PriceBand.
func (b *PriceBand) ApplyPriceOverrides(input, output, cacheRead, cacheCreate *float64) {
	if input != nil {
		b.InputPrice = *input
	}
	if output != nil {
		b.OutputPrice = *output
	}
	if cacheRead != nil {
		b.CacheReadInputPrice = *cacheRead
	}
	if cacheCreate != nil {
		b.CacheCreationInputPrice = *cacheCreate
	}
}

// PriceAt returns the effective price snapshot at time t (local time, or
// the model's configured Location) plus the name of the band that matched —
// "" when the flat price applies (no bands, or no band matched).
//
// The returned snapshot never carries NON-EMPTY Bands: band hits build a
// fresh snapshot, and a miss returns a flat copy that likewise omits the
// band table. It is the FINAL per-call price, exactly what the usage ledger
// stores per row. Callers that need the band name (ledger recording) read
// the second return value.
func (p *ModelPrice) PriceAt(t time.Time) (ModelPrice, string) {
	if len(p.Bands) == 0 {
		return *p, ""
	}
	loc := p.Location
	if loc == nil {
		loc = t.Location()
	}
	hour := t.In(loc).Hour()
	for _, b := range p.Bands {
		if b.matches(hour) {
			return ModelPrice{
				InputPrice:              b.InputPrice,
				OutputPrice:             b.OutputPrice,
				CacheReadInputPrice:     b.CacheReadInputPrice,
				CacheCreationInputPrice: b.CacheCreationInputPrice,
			}, b.Name
		}
	}
	// No band matched → flat snapshot. Bands are NOT carried: the snapshot is
	// final, consumers (ledger row, TUI estimate) never re-resolve it.
	return ModelPrice{
		InputPrice:              p.InputPrice,
		OutputPrice:             p.OutputPrice,
		CacheReadInputPrice:     p.CacheReadInputPrice,
		CacheCreationInputPrice: p.CacheCreationInputPrice,
	}, ""
}

// builtinPriceVersion is one version of the built-in price table for a
// model. EffectiveFrom is the instant (in the version's own timezone, e.g.
// Asia/Shanghai for DeepSeek's peak pricing) at which this version takes
// over; zero = always in effect (the initial version). The version whose
// EffectiveFrom is the latest one not after `at` wins — so a scheduled
// price change (DeepSeek 2026-08-17 峰谷定价) is written into the code in
// advance and switches automatically, while ledger rows keep snapshotting
// the price that was actually in effect at call time.
type builtinPriceVersion struct {
	EffectiveFrom time.Time
	Price         ModelPrice
}

// tzAsiaShanghai anchors DeepSeek's peak-pricing bands: the official
// schedule is defined in 北京时间. China has no DST since 1991, so a fixed
// +08:00 zone is exact and needs no tzdata.
var tzAsiaShanghai = time.FixedZone("Asia/Shanghai", 8*3600)

// deepSeekPeakEffectiveFrom is 2026-08-17 00:00 北京时间 — when DeepSeek's
// 峰谷定价 (peak/off-peak) takes effect per the official pricing page.
// 高峰时段 = 北京时间 09:00-12:00、14:00-18:00；空闲时段 = 高峰的一半。
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
var deepSeekPeakEffectiveFrom = time.Date(2026, 8, 17, 0, 0, 0, 0, tzAsiaShanghai)

// deepseekFlashPriceVersions: 老价（¥1/2/0.02）+ 2026-08-17 起峰谷价
// （空闲 ¥1.5/4.5/0.05，高峰 ¥3/9/0.10）。
var deepseekFlashPriceVersions = []builtinPriceVersion{
	{
		// 8/16 及以前：flat，无时段。
		Price: ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
	},
	{
		EffectiveFrom: deepSeekPeakEffectiveFrom,
		Price: ModelPrice{
			// 平段 = 空闲时段价。
			InputPrice: 1.5, OutputPrice: 4.5, CacheReadInputPrice: 0.05,
			Location: tzAsiaShanghai,
			Bands: []PriceBand{
				// 高峰：09:00-12:00、14:00-18:00（北京时间），空闲 = 高峰一半。
				{Name: "peak", StartHour: 9, EndHour: 12, InputPrice: 3.0, OutputPrice: 9.0, CacheReadInputPrice: 0.10},
				{Name: "peak", StartHour: 14, EndHour: 18, InputPrice: 3.0, OutputPrice: 9.0, CacheReadInputPrice: 0.10},
			},
		},
	},
}

// deepseekProPriceVersions: 老价（¥3/6/0.025）+ 2026-08-17 起峰谷价
// （空闲 ¥4.5/13.5/0.15，高峰 ¥9/27/0.30）。
var deepseekProPriceVersions = []builtinPriceVersion{
	{
		Price: ModelPrice{InputPrice: 3.0, OutputPrice: 6.0, CacheReadInputPrice: 0.025},
	},
	{
		EffectiveFrom: deepSeekPeakEffectiveFrom,
		Price: ModelPrice{
			InputPrice: 4.5, OutputPrice: 13.5, CacheReadInputPrice: 0.15,
			Location: tzAsiaShanghai,
			Bands: []PriceBand{
				{Name: "peak", StartHour: 9, EndHour: 12, InputPrice: 9.0, OutputPrice: 27.0, CacheReadInputPrice: 0.30},
				{Name: "peak", StartHour: 14, EndHour: 18, InputPrice: 9.0, OutputPrice: 27.0, CacheReadInputPrice: 0.30},
			},
		},
	},
}

// GetBuiltinModelPriceAt returns the built-in price for a given model as it
// was in effect at time at, or nil if unknown. Versioned entries
// (builtinPriceVersion.EffectiveFrom) switch automatically at their
// effective instant; unversioned models return their single price for any at.
//
// INVARIANT: each model's version slice MUST stay ordered by EffectiveFrom
// ascending (initial unversioned entry first) — the scan picks the last
// version whose EffectiveFrom is not after at, so an out-of-order slice
// silently resolves the wrong price. The DeepSeek version tests pin this.
//
// Use this (not GetBuiltinModelPrice) anywhere the call time is known —
// the usage ledger and the TUI statusbar both resolve "the price at call
// time", which is exactly the time-of-use semantics.
func GetBuiltinModelPriceAt(model string, at time.Time) *ModelPrice {
	versions := builtinVersionsFor(model)
	if len(versions) == 0 {
		return nil
	}
	// Linear scan (at most a handful of versions per model): the last
	// version whose EffectiveFrom is not after at wins.
	var cur *ModelPrice
	for i := range versions {
		if versions[i].EffectiveFrom.IsZero() || !at.Before(versions[i].EffectiveFrom) {
			p := versions[i].Price
			// Defensive: copy the Bands slice so callers can never mutate
			// the shared built-in table through a returned price.
			if p.Bands != nil {
				p.Bands = append([]PriceBand(nil), p.Bands...)
			}
			cur = &p
		}
	}
	return cur
}

// GetBuiltinModelPrice returns the built-in price in effect right now (see
// GetBuiltinModelPriceAt). Kept for compatibility — prefer the At variant
// when the call time is known, since the effective price may depend on it.
func GetBuiltinModelPrice(model string) *ModelPrice {
	return GetBuiltinModelPriceAt(model, time.Now())
}

// builtinVersionsFor returns the version list for a model, or nil if unknown.
func builtinVersionsFor(model string) []builtinPriceVersion {
	model = strings.ToLower(model)

	switch {
	case strings.Contains(model, "deepseek"):
		return deepseekVersions(model)
	case strings.Contains(model, "glm-5.2"):
		// 智谱 GLM-5.2（国内版）。Source: https://bigmodel.cn/pricing
		// 缓存存储（写入）限时免费 → CacheCreationInputPrice = 0。
		return []builtinPriceVersion{{Price: ModelPrice{
			InputPrice:          8.0,
			OutputPrice:         28.0,
			CacheReadInputPrice: 2.0,
		}}}
	case strings.Contains(model, "kimi-k3"):
		// 月之暗面 Kimi K3（国内版）。Source: https://platform.kimi.com
		// 官方价格表只有三价（输入/输出/缓存命中），未列缓存写入费 = 免费。
		return []builtinPriceVersion{{Price: ModelPrice{
			InputPrice:          20.0,
			OutputPrice:         100.0,
			CacheReadInputPrice: 2.0,
		}}}
	case strings.Contains(model, "mimo-v2.5-pro"):
		// 小米 MiMo-V2.5 Pro（国内版）。Source: https://mimo.mi.com/docs/zh-CN/price/pay-as-you-go
		// 缓存写入限时免费 → CacheCreationInputPrice = 0。
		return []builtinPriceVersion{{Price: ModelPrice{
			InputPrice:          3.0,
			OutputPrice:         6.0,
			CacheReadInputPrice: 0.025,
		}}}
	case strings.Contains(model, "mimo-v2.5"):
		// 小米 MiMo-V2.5（国内版）。Source: https://mimo.mi.com/docs/zh-CN/price/pay-as-you-go
		// 缓存写入限时免费 → CacheCreationInputPrice = 0。
		return []builtinPriceVersion{{Price: ModelPrice{
			InputPrice:          1.0,
			OutputPrice:         2.0,
			CacheReadInputPrice: 0.02,
		}}}
	}

	return nil
}

// deepseekVersions returns the DeepSeek version list for a model variant.
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
//
// DeepSeek 的上下文硬盘缓存（kv_cache）没有"缓存写入费"这一计费项：每个
// 请求自动触发缓存构建（落盘），官方文档只区分命中/未命中两类输入计费。
// 未列写入费 = 免费 → CacheCreationInputPrice = 0（各版本均适用）。
func deepseekVersions(model string) []builtinPriceVersion {
	switch {
	case strings.Contains(model, "deepseek-v4-flash"),
		strings.Contains(model, "deepseek-chat"):
		return deepseekFlashPriceVersions
	case strings.Contains(model, "deepseek-v4-pro"),
		strings.Contains(model, "deepseek-reasoner"):
		return deepseekProPriceVersions
	default:
		// Unknown DeepSeek variant — use flash pricing as conservative default
		return deepseekFlashPriceVersions
	}
}

// PricingConfig is the user-facing per-provider pricing override block (the
// YAML under `provider.spec.pricing:`), defined here so config.Config uses
// the SAME type as the resolver — one source of truth for the pricing
// schema, no translation layer. The yaml tags are struct metadata consumed
// by config's YAML loading; this package itself never imports yaml.
//
// All fields are optional: nil = not set (flat prices fall back to the
// built-in table; band fields inherit the flat price), explicit 0 = free.
// Timezone anchors band selection (IANA name, e.g. "Asia/Shanghai");
// empty = the instant's own local timezone.
type PricingConfig struct {
	InputPrice              *float64        `yaml:"input_price,omitempty"`
	OutputPrice             *float64        `yaml:"output_price,omitempty"`
	CacheReadInputPrice     *float64        `yaml:"cache_read_input_price,omitempty"`
	CacheCreationInputPrice *float64        `yaml:"cache_creation_input_price,omitempty"`
	Timezone                string          `yaml:"timezone,omitempty"`
	Bands                   []PriceBandSpec `yaml:"bands,omitempty"`
}

// HasAny reports whether at least one flat override is set — when none is,
// the flat price falls back to the built-in table (a bands-only override
// then inherits the built-in flat prices).
func (p *PricingConfig) HasAny() bool {
	return p.InputPrice != nil || p.OutputPrice != nil ||
		p.CacheReadInputPrice != nil || p.CacheCreationInputPrice != nil
}

// Apply copies non-nil price overrides onto dst (a ModelPrice or PriceBand)
// — the existing convention: unset fields keep their value, an explicit 0
// means free.
func (p *PricingConfig) Apply(dst PriceOverrides) {
	dst.ApplyPriceOverrides(p.InputPrice, p.OutputPrice, p.CacheReadInputPrice, p.CacheCreationInputPrice)
}

// PriceBandSpec is one raw time-of-use band in user-facing form: Start/End
// are "HH:MM" (24h, inclusive start, exclusive end; end <= start wraps past
// midnight, end == start = whole day; minutes must be 0). nil price fields
// inherit the flat price, explicit 0 = free.
type PriceBandSpec struct {
	Name                    string   `yaml:"name,omitempty"` // band name written to the ledger row ("" = unnamed)
	Start                   string   `yaml:"start"`
	End                     string   `yaml:"end"`
	InputPrice              *float64 `yaml:"input_price,omitempty"`
	OutputPrice             *float64 `yaml:"output_price,omitempty"`
	CacheReadInputPrice     *float64 `yaml:"cache_read_input_price,omitempty"`
	CacheCreationInputPrice *float64 `yaml:"cache_creation_input_price,omitempty"`
}

// PriceScheduleSource supplies per-provider pricing overrides to
// ResolveModelPriceAt. config.Config implements it — the dependency
// direction stays config → llm, so the whole pricing domain (schema,
// built-in table, versioning, band inheritance, time pinning) can live in
// this package without an import cycle.
type PriceScheduleSource interface {
	// PricingSchedule returns the provider's pricing override block, or nil
	// when the provider is unknown or has no pricing configured — the
	// resolver then falls back to the built-in table alone.
	PricingSchedule(providerName string) *PricingConfig
}

// ResolveModelPriceAt resolves the effective price for model at time at —
// the single pricing entry point for the usage ledger (RecordingProvider's
// resolver) and live estimates (statusbar). src supplies per-provider raw
// overrides; model selects the versioned built-in table entry at at. The
// returned ResolvedPrice is pinned to at: time-of-use bands have been
// applied, Bands consumed.
//
// Resolution order:
//  1. Flat (平段) price: src's override fields when any is set (unset fields
//     = 0/free, NOT built-in fallback); when the source provides bands but
//     no flat fields, the flat price falls back to the built-in table so a
//     bands-only override keeps the built-in base price.
//  2. Bands: source bands replace any built-in bands wholesale; a band's nil
//     price fields INHERIT the flat price (explicit 0 = free). Bands with
//     unparseable times are skipped (flat price) with a warn — a typo'd band
//     must not be silently ignored.
//  3. The snapshot is pinned to at via ModelPrice.PriceAt.
func ResolveModelPriceAt(src PriceScheduleSource, providerName, model string, at time.Time) ResolvedPrice {
	var flat *ModelPrice
	var bands []PriceBandSpec
	tzName := ""

	if src != nil {
		if p := src.PricingSchedule(providerName); p != nil {
			bands = p.Bands
			tzName = p.Timezone
			if p.HasAny() {
				// Partial override: only the set fields count (0 = free).
				flat = &ModelPrice{}
				p.Apply(flat)
			} else {
				// Pricing block present but fully unset → built-in flat
				// price (bands-only overrides inherit from it).
				flat = GetBuiltinModelPriceAt(model, at)
			}
		}
	}
	if flat == nil {
		flat = GetBuiltinModelPriceAt(model, at)
	}
	if flat == nil {
		if len(bands) > 0 {
			// Unknown model + bands-only override: don't silently drop the
			// user's bands. Fall back to a zero flat price — explicit band
			// prices still apply, inherited fields stay 0 (unpriced).
			flat = &ModelPrice{}
		} else {
			// Unknown model, no overrides → no price at all (unpriced row).
			return ResolvedPrice{}
		}
	}

	// Copy before mutating: never touch the shared built-in table.
	resolved := *flat
	if len(bands) > 0 {
		// Source bands replace any built-in bands wholesale.
		resolved.Bands = nil
		for _, bs := range bands {
			band, err := buildPriceBand(bs, &resolved)
			if err != nil {
				logger.Default().Warn(context.Background(), "pricing: skipping invalid band (flat price applies)",
					"provider", providerName, "model", model,
					"band", bs.Name, "start", bs.Start, "end", bs.End, "error", err)
				continue
			}
			resolved.Bands = append(resolved.Bands, band)
		}
		if tzName != "" {
			if loc, err := time.LoadLocation(tzName); err == nil {
				resolved.Location = loc
			} else {
				// Unknown IANA name: keep the current location (local or
				// built-in) rather than failing the whole resolution.
				logger.Default().Warn(context.Background(), "pricing: unknown timezone, using default",
					"provider", providerName, "timezone", tzName, "error", err)
			}
		}
	}

	snapshot, bandName := resolved.PriceAt(at)
	return ResolvedPrice{Price: snapshot, Band: bandName}
}

// buildPriceBand converts a raw band spec into a PriceBand, filling nil
// price fields from the flat price (inheritance). Returns an error for
// unparseable times — the band is then skipped (flat price applies).
func buildPriceBand(bs PriceBandSpec, flat *ModelPrice) (PriceBand, error) {
	start, err := parseBandHour(bs.Start)
	if err != nil {
		return PriceBand{}, err
	}
	end, err := parseBandHour(bs.End)
	if err != nil {
		return PriceBand{}, err
	}
	band := PriceBand{
		Name:      bs.Name,
		StartHour: start,
		EndHour:   end,
		// Inherit the flat price; explicit 0 in config = free.
		InputPrice:              flat.InputPrice,
		OutputPrice:             flat.OutputPrice,
		CacheReadInputPrice:     flat.CacheReadInputPrice,
		CacheCreationInputPrice: flat.CacheCreationInputPrice,
	}
	band.ApplyPriceOverrides(bs.InputPrice, bs.OutputPrice, bs.CacheReadInputPrice, bs.CacheCreationInputPrice)
	return band, nil
}

// parseBandHour parses "HH:MM" into an hour (0-23). Minutes must be zero —
// band granularity is one hour (see PriceBandSpec).
func parseBandHour(s string) (int, error) {
	hhmm := strings.Split(s, ":")
	if len(hhmm) != 2 {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	h, err := strconv.Atoi(hhmm[0])
	if err != nil {
		return 0, fmt.Errorf("invalid hour %q", hhmm[0])
	}
	m, err := strconv.Atoi(hhmm[1])
	if err != nil {
		return 0, fmt.Errorf("invalid minute %q", hhmm[1])
	}
	if h < 0 || h > 23 || m != 0 {
		return 0, fmt.Errorf("invalid band time %q (want whole hour 00-23)", s)
	}
	return h, nil
}

// ResolveModelPrice resolves the effective pricing for a model.
// It checks provider-level overrides first (nil ptr means "not set"), then falls
// back to built-in pricing. Returns nil if no pricing is available.
//
// Deprecated: no production callers remain — the pricing entry point is
// ResolveModelPriceAt (time-of-use aware, versioned built-in table, config
// overrides via PriceScheduleSource). Kept for the legacy tests in this
// package; the built-in fallback here is NOT time-aware (GetBuiltinModelPrice
// = at now), so do not reintroduce it as a production path.
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
