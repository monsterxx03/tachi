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
			model: "deepseek-chat",
			want:  flash,
		},
		{
			model: "deepseek-v4-pro",
			want:  pro,
		},
		{
			model: "deepseek-reasoner",
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
// prices). Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
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
}

// TestGetBuiltinModelPriceAt_DeepSeekPeakSelection exercises band selection at
// fixed instants, including timezone anchoring (北京时间 — the same instant
// expressed in UTC must hit the same band).
func TestGetBuiltinModelPriceAt_DeepSeekPeakSelection(t *testing.T) {
	beijing := func(hour int) time.Time {
		return time.Date(2026, 8, 17, hour, 0, 0, 0, tzAsiaShanghai)
	}
	cases := []struct {
		name string
		at   time.Time
		want float64 // input price at that instant
		band string
	}{
		{"off-peak 08:00", beijing(8), 1.5, ""},
		{"peak 09:00", beijing(9), 3.0, "peak"},
		{"peak 11:59", beijing(11), 3.0, "peak"},
		{"between peaks 12:00", beijing(12), 1.5, ""},
		{"between peaks 13:00", beijing(13), 1.5, ""},
		{"peak 14:00", beijing(14), 3.0, "peak"},
		{"peak 17:59", beijing(17), 3.0, "peak"},
		{"off-peak 18:00", beijing(18), 1.5, ""},
		{"off-peak 23:00", beijing(23), 1.5, ""},
		{"same instant in UTC 06:30 (14:30 Beijing)", time.Date(2026, 8, 17, 6, 30, 0, 0, time.UTC), 3.0, "peak"},
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

	// pro: peak 10:00 → 9.0/27.0/0.30.
	pro := GetBuiltinModelPriceAt("deepseek-v4-pro", beijing(10))
	if pro == nil {
		t.Fatal("pro price = nil")
	}
	snap, band := pro.PriceAt(beijing(10))
	if snap.InputPrice != 9.0 || snap.OutputPrice != 27.0 || snap.CacheReadInputPrice != 0.30 {
		t.Errorf("pro peak prices = %+v, want 9.0/27.0/0.30", snap)
	}
	if band != "peak" {
		t.Errorf("pro band = %q, want peak", band)
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
