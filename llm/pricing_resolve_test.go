package llm

import (
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
)

func f64(v float64) *float64 { return &v }

// cfgWithPricing wraps a pricing block in a minimal config.Config carrying a
// single provider named "custom" — the provider PricingSchedule looks up.
func cfgWithPricing(p *config.PricingConfig) *config.Config {
	return &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "custom",
			Spec: config.ModelSpec{Pricing: p},
		}},
	}
}

// cstFixed is a fixed +08:00 zone used to build test instants: band
// selection must be machine-timezone independent (the built-in DeepSeek
// table anchors Asia/Shanghai, config bands default to the instants' own
// zone). time.Local would make these tests fail on non-UTC+8 machines.
var cstFixed = time.FixedZone("CST", 8*3600)

// atHour is a fixed +08:00 instant on 2026-08-17 (tests never rely on
// time.Now() so band selection is deterministic and portable).
func atHour(h int) time.Time {
	return time.Date(2026, 8, 17, h, 0, 0, 0, cstFixed)
}

// TestResolveModelPriceAt_Bands: a source band applies at its hours (with
// its name), the flat price applies outside it.
func TestResolveModelPriceAt_Bands(t *testing.T) {
	cfg := cfgWithPricing(&config.PricingConfig{
		InputPrice:  f64(1.0),
		OutputPrice: f64(2.0),
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(3.0)},
		},
	})

	in := ResolveModelPriceAt(cfg, "custom", "some-model", atHour(10))
	if in.Price.InputPrice != 3.0 || in.Price.OutputPrice != 2.0 || in.Band != "peak" {
		t.Errorf("in-band = %+v (band %q), want 3.0/2.0 + peak", in.Price, in.Band)
	}
	out := ResolveModelPriceAt(cfg, "custom", "some-model", atHour(13))
	if out.Price.InputPrice != 1.0 || out.Price.OutputPrice != 2.0 || out.Band != "" {
		t.Errorf("off-band = %+v (band %q), want flat 1.0/2.0", out.Price, out.Band)
	}
}

// TestResolveModelPriceAt_BandInheritance: unset band fields inherit the flat
// price (output + cache prices here), an explicit 0 in the band = free.
func TestResolveModelPriceAt_BandInheritance(t *testing.T) {
	cfg := cfgWithPricing(&config.PricingConfig{
		InputPrice:              f64(1.0),
		OutputPrice:             f64(2.0),
		CacheReadInputPrice:     f64(0.05),
		CacheCreationInputPrice: f64(0.5),
		Bands: []config.PriceBandSpec{
			{
				Name: "night", Start: "23:00", End: "07:00",
				InputPrice:          f64(0.5), // inherit output/cache from flat
				CacheReadInputPrice: f64(0),   // explicit 0 = free
			},
		},
	})

	rp := ResolveModelPriceAt(cfg, "custom", "m", atHour(2)) // inside 23:00-07:00
	if rp.Price.InputPrice != 0.5 || rp.Price.OutputPrice != 2.0 ||
		rp.Price.CacheReadInputPrice != 0 || rp.Price.CacheCreationInputPrice != 0.5 {
		t.Errorf("band = %+v, want inherited output 2.0 / explicit-free cache-read / inherited creation 0.5", rp.Price)
	}
	if rp.Band != "night" {
		t.Errorf("Band = %q, want night", rp.Band)
	}
}

// TestResolveModelPriceAt_BandParseFailureSkipped: an unparseable band time
// (non-whole-hour minute) is skipped — the flat price applies instead of the
// whole resolution failing.
func TestResolveModelPriceAt_BandParseFailureSkipped(t *testing.T) {
	cfg := cfgWithPricing(&config.PricingConfig{
		InputPrice: f64(1.0),
		Bands: []config.PriceBandSpec{
			{Start: "09:30", End: "12:00", InputPrice: f64(9.0)}, // 09:30 → invalid
		},
	})
	rp := ResolveModelPriceAt(cfg, "custom", "m", atHour(10))
	if rp.Price.InputPrice != 1.0 || rp.Band != "" {
		t.Errorf("invalid band must be skipped: %+v (band %q)", rp.Price, rp.Band)
	}
}

// TestResolveModelPriceAt_BandsOnlyUsesBuiltinFlat: a source with only bands
// (no flat fields) inherits the flat price from the BUILT-IN table — so a
// bands-only override keeps the built-in base price.
func TestResolveModelPriceAt_BandsOnlyUsesBuiltinFlat(t *testing.T) {
	cfg := cfgWithPricing(&config.PricingConfig{
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(0.9)},
		},
	})
	rp := ResolveModelPriceAt(cfg, "custom", "deepseek-chat", atHour(10))
	if rp.Price.InputPrice != 0.9 {
		t.Errorf("band input = %v, want 0.9 (override)", rp.Price.InputPrice)
	}
	// atHour(10) is 2026-08-17 10:00 +08:00 — after DeepSeek's 峰谷 effective
	// date, so the built-in flat is the OFF-PEAK price (output ¥4.5).
	if rp.Price.OutputPrice != 4.5 {
		t.Errorf("flat output = %v, want 4.5 (inherited from built-in deepseek-chat off-peak)", rp.Price.OutputPrice)
	}
}

// TestResolveModelPriceAt_Timezone: the pricing timezone anchors band
// selection — the same instant expressed in UTC resolves against 北京时间.
func TestResolveModelPriceAt_Timezone(t *testing.T) {
	cfg := cfgWithPricing(&config.PricingConfig{
		InputPrice:  f64(1.0),
		OutputPrice: f64(2.0),
		Timezone:    "Asia/Shanghai",
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(3.0)},
		},
	})
	// 2026-08-17 06:30 UTC == 14:30 北京时间 → outside 09:00-12:00 → flat.
	rp := ResolveModelPriceAt(cfg, "custom", "m", time.Date(2026, 8, 17, 6, 30, 0, 0, time.UTC))
	if rp.Band != "" || rp.Price.InputPrice != 1.0 {
		t.Errorf("UTC 06:30 (14:30 Beijing) must be off-band flat: %+v (band %q)", rp.Price, rp.Band)
	}
	// 2026-08-17 01:30 UTC == 09:30 北京时间 → in peak.
	rp = ResolveModelPriceAt(cfg, "custom", "m", time.Date(2026, 8, 17, 1, 30, 0, 0, time.UTC))
	if rp.Band != "peak" || rp.Price.InputPrice != 3.0 {
		t.Errorf("UTC 01:30 (09:30 Beijing) must hit peak: %+v (band %q)", rp.Price, rp.Band)
	}
}

// TestResolveModelPriceAt_UnknownModelBandsOnly (review Bug #2 regression):
// an unknown model with a bands-only override must still apply the band's
// explicit prices (zero flat price), not silently drop the user's config.
func TestResolveModelPriceAt_UnknownModelBandsOnly(t *testing.T) {
	cfg := cfgWithPricing(&config.PricingConfig{
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(2.0), OutputPrice: f64(4.0)},
		},
	})
	rp := ResolveModelPriceAt(cfg, "custom", "no-such-model", atHour(10))
	if rp.Price.InputPrice != 2.0 || rp.Price.OutputPrice != 4.0 || rp.Band != "peak" {
		t.Errorf("unknown-model bands must apply: %+v (band %q)", rp.Price, rp.Band)
	}
	// Outside the band: zero flat (unpriced), still resolves rather than nil.
	out := ResolveModelPriceAt(cfg, "custom", "no-such-model", atHour(13))
	if out.Price.InputPrice != 0 || out.Band != "" {
		t.Errorf("off-band unknown model must be zero flat: %+v (band %q)", out.Price, out.Band)
	}
}

// TestResolveModelPriceAt_BandsReplaceBuiltin: source bands replace the
// built-in table's bands wholesale — at an instant where the BUILT-IN
// DeepSeek peak would apply (10:00 北京, post-8/17), a bands-only override
// with its own schedule must NOT see the built-in peak price.
func TestResolveModelPriceAt_BandsReplaceBuiltin(t *testing.T) {
	beijing := time.FixedZone("Asia/Shanghai", 8*3600)
	postPeak := time.Date(2026, 8, 17, 10, 0, 0, 0, beijing) // built-in: peak (¥3/9/0.10)
	cfg := cfgWithPricing(&config.PricingConfig{
		// Bands-only override: flat inherits built-in (1.5/4.5/0.05), custom
		// band covers 20:00-22:00 — 10:00 must be FLAT.
		Bands: []config.PriceBandSpec{
			{Name: "evening", Start: "20:00", End: "22:00", InputPrice: f64(1.0)},
		},
	})
	rp := ResolveModelPriceAt(cfg, "custom", "deepseek-chat", postPeak)
	if rp.Price.InputPrice != 1.5 || rp.Band != "" {
		t.Errorf("10:00 with custom bands = %+v (band %q), want built-in flat 1.5 (built-in peak replaced)", rp.Price, rp.Band)
	}
}

// TestResolveModelPriceAt_NoSource: nil config → built-in table alone
// (versioned: pre-peak instant resolves the old flat prices).
func TestResolveModelPriceAt_NoSource(t *testing.T) {
	rp := ResolveModelPriceAt(nil, "", "deepseek-chat", prePeak)
	if !rp.HasPrice() {
		t.Fatal("no source must still resolve built-in prices")
	}
	if rp.Price.InputPrice != 1.0 || rp.Price.OutputPrice != 2.0 || rp.Price.CacheReadInputPrice != 0.02 {
		t.Errorf("pre-peak deepseek-chat = %+v, want old prices 1.0/2.0/0.02", rp.Price)
	}
	if rp.Band != "" {
		t.Errorf("pre-peak Band = %q, want empty (flat version)", rp.Band)
	}
}

// TestResolveModelPriceAt_UnknownNoSource: unknown model with no source →
// no price at all.
func TestResolveModelPriceAt_UnknownNoSource(t *testing.T) {
	rp := ResolveModelPriceAt(nil, "", "totally-unknown-model-xyz", atHour(10))
	if rp.HasPrice() {
		t.Errorf("unknown model must be unpriced, got %+v", rp.Price)
	}
}
