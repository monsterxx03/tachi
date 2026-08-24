package llm

import (
	"math"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// prePeak is a fixed instant before DeepSeek's 峰谷定价 effective date
// (2026-08-17 00:00 北京时间) — the old flat prices are in effect there.
// Tests use fixed instants instead of time.Now() so the price table's
// versioning can never make them time-dependent.
var prePeak = time.Date(2026, 8, 16, 12, 0, 0, 0, tzAsiaShanghai)

func TestGetBuiltinModelPrice_DeepSeek(t *testing.T) {
	flash := &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02}
	pro := &ModelPrice{InputPrice: 3.0, OutputPrice: 6.0, CacheReadInputPrice: 0.025}
	tests := []struct {
		model string
		want  *ModelPrice
	}{
		{
			model: "deepseek-v4-flash",
			want:  flash,
		},
		{
			model: "deepseek-v4-pro",
			want:  pro,
		},
		{
			model: "DeepSeek-V4-Flash",
			want:  flash,
		},
		{
			model: "unknown-deepseek-model",
			want:  flash,
		},
		{
			model: "gpt-4",
			want:  nil,
		},
		{
			model: "claude-sonnet-4-6",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetBuiltinModelPriceAt(tt.model, prePeak)
			if tt.want == nil {
				if got != nil {
					t.Errorf("GetBuiltinModelPriceAt(%q, pre-peak) = %+v, want nil", tt.model, got)
				}
				return
			}
			if got == nil {
				t.Errorf("GetBuiltinModelPriceAt(%q, pre-peak) = nil, want %+v", tt.model, tt.want)
				return
			}
			if got.InputPrice != tt.want.InputPrice {
				t.Errorf("InputPrice = %v, want %v", got.InputPrice, tt.want.InputPrice)
			}
			if got.OutputPrice != tt.want.OutputPrice {
				t.Errorf("OutputPrice = %v, want %v", got.OutputPrice, tt.want.OutputPrice)
			}
			if got.CacheReadInputPrice != tt.want.CacheReadInputPrice {
				t.Errorf("CacheReadInputPrice = %v, want %v", got.CacheReadInputPrice, tt.want.CacheReadInputPrice)
			}
		})
	}
}

// TestGetBuiltinModelPriceAt_DeepSeekPeakVersions pins the versioned DeepSeek
// table: the old flat prices until 2026-08-17 00:00 北京时间, then 峰谷定价
// (空闲 = flat, 高峰 09:00-12:00 / 14:00-18:00 北京时间 at twice the off-peak
// prices), then from 2026-08-24 the peak window narrows to WORKDAYS only —
// weekends fall back to the flat off-peak price all day.
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
func TestGetBuiltinModelPriceAt_DeepSeekPeakVersions(t *testing.T) {
	// 8/16: old flat prices, no bands.
	old := GetBuiltinModelPriceAt("deepseek-v4-flash", prePeak)
	if old == nil {
		t.Fatal("old price = nil")
	}
	if len(old.Bands) != 0 {
		t.Errorf("pre-peak version must have no bands, got %+v", old.Bands)
	}

	// 8/17 起: bands present, flat = off-peak (空闲) prices.
	peak := GetBuiltinModelPriceAt("deepseek-v4-flash", time.Date(2026, 8, 17, 0, 0, 0, 0, tzAsiaShanghai))
	if peak == nil || len(peak.Bands) != 2 {
		t.Fatalf("peak version = %+v, want 2 bands", peak)
	}
	if peak.InputPrice != 1.5 || peak.OutputPrice != 4.5 || peak.CacheReadInputPrice != 0.05 {
		t.Errorf("flat (off-peak) prices = %+v, want 1.5/4.5/0.05", peak)
	}
	// 8/17-8/23 version: EVERY day has peak hours (no Days filter).
	for _, b := range peak.Bands {
		if len(b.Days) != 0 {
			t.Errorf("8/17 version band Days = %v, want nil (every day)", b.Days)
		}
	}

	// 8/24 起: workday-only peak bands (Mon-Fri), weekend = flat off-peak all day.
	weekday := GetBuiltinModelPriceAt("deepseek-v4-flash", time.Date(2026, 8, 24, 0, 0, 0, 0, tzAsiaShanghai))
	if weekday == nil || len(weekday.Bands) != 2 {
		t.Fatalf("weekday-peak version = %+v, want 2 bands", weekday)
	}
	if weekday.InputPrice != 1.5 || weekday.OutputPrice != 4.5 || weekday.CacheReadInputPrice != 0.05 {
		t.Errorf("flat (off-peak) prices = %+v, want 1.5/4.5/0.05", weekday)
	}
	for _, b := range weekday.Bands {
		if len(b.Days) != 5 {
			t.Errorf("8/24 version band Days = %v, want 5 weekdays (Mon-Fri)", b.Days)
		}
	}
}

// TestGetBuiltinModelPriceAt_DeepSeekPeakSelection exercises band selection at
// fixed instants, including timezone anchoring (北京时间 — the same instant
// expressed in UTC must hit the same band) and the 8/24 workday-only
// schedule (weekend peak hours fall back to the off-peak flat price).
func TestGetBuiltinModelPriceAt_DeepSeekPeakSelection(t *testing.T) {
	beijing := func(day, hour int) time.Time {
		return time.Date(2026, 8, day, hour, 0, 0, 0, tzAsiaShanghai)
	}
	cases := []struct {
		name string
		at   time.Time
		want float64 // input price at that instant
		band string
	}{
		// 8/17 (Monday) — 每天峰谷 version: peak hours every day of the week.
		{"off-peak 08:00", beijing(17, 8), 1.5, ""},
		{"peak 09:00", beijing(17, 9), 3.0, "peak"},
		{"peak 11:59", beijing(17, 11), 3.0, "peak"},
		{"between peaks 12:00", beijing(17, 12), 1.5, ""},
		{"between peaks 13:00", beijing(17, 13), 1.5, ""},
		{"peak 14:00", beijing(17, 14), 3.0, "peak"},
		{"peak 17:59", beijing(17, 17), 3.0, "peak"},
		{"off-peak 18:00", beijing(17, 18), 1.5, ""},
		{"off-peak 23:00", beijing(17, 23), 1.5, ""},
		{"same instant in UTC 06:30 (14:30 Beijing)", time.Date(2026, 8, 17, 6, 30, 0, 0, time.UTC), 3.0, "peak"},
		// 8/22 (Saturday) — still the 8/17 EVERY-day version: weekends had
		// peak hours before 8/24 (historical snapshots must stay correct).
		{"8/22 saturday 10:00 still peak", beijing(22, 10), 3.0, "peak"},
		// 8/24 (Monday) onward — workday-only version: weekdays peak…
		{"8/24 monday 10:00 peak", beijing(24, 10), 3.0, "peak"},
		{"8/25 tuesday 15:00 peak", beijing(25, 15), 3.0, "peak"},
		{"8/25 tuesday 08:00 off-peak", beijing(25, 8), 1.5, ""},
		// …and weekends fall back to the off-peak flat price ALL DAY.
		{"8/29 saturday 10:00 off-peak", beijing(29, 10), 1.5, ""},
		{"8/30 sunday 10:00 off-peak", beijing(30, 10), 1.5, ""},
		{"8/30 sunday 15:00 off-peak", beijing(30, 15), 1.5, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			price := GetBuiltinModelPriceAt("deepseek-v4-flash", tt.at)
			if price == nil {
				t.Fatal("price = nil")
			}
			snap, band := price.PriceAt(tt.at)
			if snap.InputPrice != tt.want {
				t.Errorf("InputPrice = %v, want %v (at %v)", snap.InputPrice, tt.want, tt.at)
			}
			if band != tt.band {
				t.Errorf("band = %q, want %q", band, tt.band)
			}
		})
	}

	// pro: peak 10:00 → 9.0/27.0/0.30 (weekday, post-8/24) and weekend off-peak.
	pro := GetBuiltinModelPriceAt("deepseek-v4-pro", beijing(25, 10))
	if pro == nil {
		t.Fatal("pro price = nil")
	}
	snap, band := pro.PriceAt(beijing(25, 10))
	if snap.InputPrice != 9.0 || snap.OutputPrice != 27.0 || snap.CacheReadInputPrice != 0.30 {
		t.Errorf("pro peak prices = %+v, want 9.0/27.0/0.30", snap)
	}
	if band != "peak" {
		t.Errorf("pro band = %q, want peak", band)
	}
	proWeekend, band := GetBuiltinModelPriceAt("deepseek-v4-pro", beijing(30, 10)).PriceAt(beijing(30, 10))
	if proWeekend.InputPrice != 4.5 || proWeekend.OutputPrice != 13.5 || band != "" {
		t.Errorf("pro weekend = %+v (band %q), want flat 4.5/13.5", proWeekend, band)
	}
}

// TestPriceAt covers the band-matching mechanics independent of the built-in
// table: no-bands identity, first-match-wins, midnight wrap, miss → flat.
func TestPriceAt(t *testing.T) {
	flat := ModelPrice{InputPrice: 1.0, OutputPrice: 2.0}

	// No bands: identity snapshot, empty band name.
	snap, band := flat.PriceAt(time.Date(2026, 8, 17, 10, 0, 0, 0, time.Local))
	if snap.InputPrice != 1.0 || band != "" {
		t.Errorf("no-bands PriceAt = (%+v, %q), want identity + empty", snap, band)
	}

	// First matching band wins; miss falls back to flat.
	withBands := flat
	withBands.Bands = []PriceBand{
		{Name: "a", StartHour: 9, EndHour: 12, InputPrice: 3.0},
		{Name: "b", StartHour: 14, EndHour: 18, InputPrice: 4.0},
	}
	at := func(h int) time.Time { return time.Date(2026, 8, 17, h, 0, 0, 0, time.Local) }
	snap, band = withBands.PriceAt(at(10))
	if snap.InputPrice != 3.0 || band != "a" {
		t.Errorf("band a: (%+v, %q)", snap, band)
	}
	snap, band = withBands.PriceAt(at(15))
	if snap.InputPrice != 4.0 || band != "b" {
		t.Errorf("band b: (%+v, %q)", snap, band)
	}
	snap, band = withBands.PriceAt(at(13))
	if snap.InputPrice != 1.0 || band != "" {
		t.Errorf("miss must fall back to flat: (%+v, %q)", snap, band)
	}
	if len(snap.Bands) != 0 {
		t.Error("snapshot must never carry Bands (final price only)")
	}

	// Midnight wrap: 23:00→07:00.
	night := flat
	night.Bands = []PriceBand{{Name: "night", StartHour: 23, EndHour: 7, InputPrice: 0.5}}
	snap, band = night.PriceAt(at(0))
	if snap.InputPrice != 0.5 || band != "night" {
		t.Errorf("00:00 in night band: (%+v, %q)", snap, band)
	}
	snap, band = night.PriceAt(at(23))
	if snap.InputPrice != 0.5 || band != "night" {
		t.Errorf("23:00 in night band: (%+v, %q)", snap, band)
	}
	snap, band = night.PriceAt(at(12))
	if snap.InputPrice != 1.0 || band != "" {
		t.Errorf("12:00 outside night band: (%+v, %q)", snap, band)
	}

	// Days filter: the same hours peak on weekdays only — weekends fall back
	// to flat ALL DAY. 2026-08-24 is a Monday, 08-29 a Saturday.
	workday := flat
	workday.Bands = []PriceBand{{
		Name: "peak", Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		StartHour: 9, EndHour: 12, InputPrice: 3.0,
	}}
	monday := func(h int) time.Time { return time.Date(2026, 8, 24, h, 0, 0, 0, time.Local) }
	saturday := func(h int) time.Time { return time.Date(2026, 8, 29, h, 0, 0, 0, time.Local) }
	snap, band = workday.PriceAt(monday(10))
	if snap.InputPrice != 3.0 || band != "peak" {
		t.Errorf("monday 10:00 must hit the workday band: (%+v, %q)", snap, band)
	}
	snap, band = workday.PriceAt(monday(13))
	if snap.InputPrice != 1.0 || band != "" {
		t.Errorf("monday 13:00 outside band hours: (%+v, %q)", snap, band)
	}
	snap, band = workday.PriceAt(saturday(10))
	if snap.InputPrice != 1.0 || band != "" {
		t.Errorf("saturday 10:00 must fall back to flat (weekend, band days miss): (%+v, %q)", snap, band)
	}

	// A band WITHOUT Days matches every weekday (the pre-days behavior).
	anyday := flat
	anyday.Bands = []PriceBand{{Name: "peak", StartHour: 9, EndHour: 12, InputPrice: 3.0}}
	snap, band = anyday.PriceAt(saturday(10))
	if snap.InputPrice != 3.0 || band != "peak" {
		t.Errorf("no-days band must match weekends too: (%+v, %q)", snap, band)
	}
}

func TestNormalizeCacheMissInput(t *testing.T) {
	tests := []struct {
		name         string
		input        int64
		cacheRead    int64
		providerType string
		want         int64
	}{
		{name: "openai subtracts cache read", input: 1_000, cacheRead: 300, providerType: config.ProviderTypeOpenAI, want: 700},
		{name: "anthropic keeps full input", input: 1_000, cacheRead: 300, providerType: config.ProviderTypeAnthropic, want: 1_000},
		{name: "cache read exceeds input clamps to 0", input: 100, cacheRead: 200, providerType: config.ProviderTypeOpenAI, want: 0},
		{name: "empty provider treated as openai family", input: 1_000, cacheRead: 300, providerType: "", want: 700},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeCacheMissInput(tt.input, tt.cacheRead, tt.providerType); got != tt.want {
				t.Errorf("NormalizeCacheMissInput() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCacheReadCreationPrice(t *testing.T) {
	// 0 means "not charged" (free) — no fallback to the input price.
	read, creation := CacheReadCreationPrice(&ModelPrice{InputPrice: 1.0, OutputPrice: 2.0})
	if read != 0 || creation != 0 {
		t.Errorf("unset = (%v, %v), want (0, 0) = free", read, creation)
	}

	// Explicit positive prices are returned as-is.
	read, creation = CacheReadCreationPrice(&ModelPrice{
		InputPrice:              1.0,
		CacheReadInputPrice:     0.02,
		CacheCreationInputPrice: 1.5,
	})
	if read != 0.02 || creation != 1.5 {
		t.Errorf("explicit = (%v, %v), want (0.02, 1.5)", read, creation)
	}
}

// TestGetBuiltinModelPrice_MiniMax pins the MiniMax paygo prices from
// https://platform.minimaxi.com/docs/guides/pricing-paygo . M3 uses the
// ≤512k tier (the >512k tier is intentionally not modeled — see
// minimaxVersions comment). Cache creation is "not listed" for M3
// (CacheCreationInputPrice = 0 per the "未列即不计费" rule); M2.7 系列
// charges ¥2.625/M explicitly.
func TestGetBuiltinModelPrice_GLM(t *testing.T) {
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.Local)
	tests := []struct {
		model string
		want  *ModelPrice
	}{
		{
			// GLM-5.3 与 GLM-5.2 同价（Source: https://open.bigmodel.cn/pricing）。
			model: "glm-5.3",
			want: &ModelPrice{
				InputPrice: 8.0, OutputPrice: 28.0, CacheReadInputPrice: 2.0,
				// CacheCreationInputPrice = 0（缓存存储限时免费）。
			},
		},
		{
			model: "GLM-5.3",
			want: &ModelPrice{
				InputPrice: 8.0, OutputPrice: 28.0, CacheReadInputPrice: 2.0,
			},
		},
		{
			model: "glm-5.2",
			want: &ModelPrice{
				InputPrice: 8.0, OutputPrice: 28.0, CacheReadInputPrice: 2.0,
			},
		},
		// Unknown GLM variant (e.g. glm-5) → unpriced, not the 5.3 table.
		{
			model: "glm-5",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetBuiltinModelPriceAt(tt.model, at)
			if tt.want == nil {
				if got != nil {
					t.Errorf("GetBuiltinModelPriceAt(%q) = %+v, want nil", tt.model, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("GetBuiltinModelPriceAt(%q, at) = nil", tt.model)
			}
			if got.InputPrice != tt.want.InputPrice ||
				got.OutputPrice != tt.want.OutputPrice ||
				got.CacheReadInputPrice != tt.want.CacheReadInputPrice ||
				got.CacheCreationInputPrice != tt.want.CacheCreationInputPrice {
				t.Errorf("GetBuiltinModelPriceAt(%q) = %+v, want %+v", tt.model, got, tt.want)
			}
			if len(got.Bands) != 0 {
				t.Errorf("GLM has no time-of-use bands, got %+v", got.Bands)
			}
		})
	}
}

func TestGetBuiltinModelPrice_MiniMax(t *testing.T) {
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.Local)
	tests := []struct {
		model string
		want  *ModelPrice
	}{
		{
			model: "MiniMax-M3",
			want: &ModelPrice{
				InputPrice: 2.10, OutputPrice: 8.40, CacheReadInputPrice: 0.42,
				// CacheCreationInputPrice = 0 (未列即不计费)
			},
		},
		{
			model: "minimax-m3",
			want: &ModelPrice{
				InputPrice: 2.10, OutputPrice: 8.40, CacheReadInputPrice: 0.42,
			},
		},
		{
			model: "MiniMax-M2.7",
			want: &ModelPrice{
				InputPrice: 2.1, OutputPrice: 8.4,
				CacheReadInputPrice: 0.42, CacheCreationInputPrice: 2.625,
			},
		},
		{
			model: "MiniMax-M2.7-highspeed",
			want: &ModelPrice{
				InputPrice: 4.2, OutputPrice: 16.8,
				CacheReadInputPrice: 0.42, CacheCreationInputPrice: 2.625,
			},
		},
		// Unknown MiniMax variant → M2.7 default (conservative fallback).
		{
			model: "MiniMax-M-future",
			want: &ModelPrice{
				InputPrice: 2.1, OutputPrice: 8.4,
				CacheReadInputPrice: 0.42, CacheCreationInputPrice: 2.625,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetBuiltinModelPriceAt(tt.model, at)
			if got == nil {
				t.Fatalf("GetBuiltinModelPriceAt(%q, at) = nil", tt.model)
			}
			if got.InputPrice != tt.want.InputPrice ||
				got.OutputPrice != tt.want.OutputPrice ||
				got.CacheReadInputPrice != tt.want.CacheReadInputPrice ||
				got.CacheCreationInputPrice != tt.want.CacheCreationInputPrice {
				t.Errorf("GetBuiltinModelPriceAt(%q) = %+v, want %+v", tt.model, got, tt.want)
			}
			if len(got.Bands) != 0 {
				t.Errorf("MiniMax has no time-of-use bands, got %+v", got.Bands)
			}
		})
	}
}

func TestCostForUsage(t *testing.T) {
	tests := []struct {
		name         string
		usage        *Usage
		price        *ModelPrice
		providerType string
		want         float64
	}{
		{
			name:         "nil usage",
			usage:        nil,
			price:        &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0},
			providerType: config.ProviderTypeOpenAI,
			want:         0,
		},
		{
			name:         "nil price",
			usage:        &Usage{InputTokens: 1000, OutputTokens: 500},
			price:        nil,
			providerType: config.ProviderTypeOpenAI,
			want:         0,
		},
		{
			name:         "zero price",
			usage:        &Usage{InputTokens: 1000, OutputTokens: 500},
			price:        &ModelPrice{InputPrice: 0, OutputPrice: 0},
			providerType: config.ProviderTypeOpenAI,
			want:         0,
		},
		{
			name:         "basic calculation",
			usage:        &Usage{InputTokens: 1_000_000, OutputTokens: 500_000},
			price:        &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0},
			providerType: config.ProviderTypeOpenAI,
			want:         1.0 + 1.0, // 1M input * ¥1 + 500K output * ¥2/1M
		},
		{
			name: "openai subtracts cache read",
			usage: &Usage{
				InputTokens:          1_000_000,
				OutputTokens:         500_000,
				CacheReadInputTokens: 300_000,
			},
			price:        &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
			providerType: config.ProviderTypeOpenAI,
			// Cache miss: 700K * 1 / 1M = 0.7
			// Cache read: 300K * 0.02 / 1M = 0.006
			// Output: 500K * 2 / 1M = 1.0
			// Total: 1.706
			want: 0.7 + 0.006 + 1.0,
		},
		{
			name: "anthropic does not subtract cache read",
			usage: &Usage{
				InputTokens:          1_000_000,
				OutputTokens:         500_000,
				CacheReadInputTokens: 300_000,
			},
			price:        &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
			providerType: config.ProviderTypeAnthropic,
			// Input kept at 1M (no subtraction): 1.0
			// Cache read: 300K * 0.02 / 1M = 0.006
			// Output: 500K * 2 / 1M = 1.0
			// Total: 2.006
			want: 1.0 + 0.006 + 1.0,
		},
		{
			name: "unset cache read price = free",
			usage: &Usage{
				InputTokens:          1_000_000,
				CacheReadInputTokens: 400_000,
			},
			price:        &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0}, // CacheReadInputPrice = 0
			providerType: config.ProviderTypeOpenAI,
			// Cache miss: 600K * 1 / 1M = 0.6; cache read free → 0.
			want: 0.6,
		},
		{
			name: "cache creation",
			usage: &Usage{
				InputTokens:              1_000_000,
				OutputTokens:             500_000,
				CacheCreationInputTokens: 200_000,
			},
			price: &ModelPrice{
				InputPrice:              1.0,
				OutputPrice:             2.0,
				CacheCreationInputPrice: 1.5,
			},
			providerType: config.ProviderTypeOpenAI,
			// Input (cache miss): 1M * 1 = 1.0
			// Cache creation: 200K * 1.5/1M = 0.3
			// Output: 500K * 2/1M = 1.0
			want: 1.0 + 0.3 + 1.0,
		},
		{
			name: "zero cache creation price = free",
			usage: &Usage{
				InputTokens:              1_000_000,
				CacheCreationInputTokens: 200_000,
			},
			price: &ModelPrice{
				InputPrice:  8.0,
				OutputPrice: 28.0,
				// CacheCreationInputPrice 未设 = 0 = 不计费
			},
			providerType: config.ProviderTypeOpenAI,
			// Input: 1M * 8 = 8.0; cache creation free → 0; no output.
			want: 8.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CostForUsage(tt.usage, tt.price, tt.providerType)
			if math.Abs(got-tt.want) > 0.0001 {
				t.Errorf("CostForUsage() = %v, want %v", got, tt.want)
			}
		})
	}
}
