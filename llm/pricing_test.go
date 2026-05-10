package llm

import (
	"math"
	"testing"
)

func TestGetBuiltinModelPrice_DeepSeek(t *testing.T) {
	tests := []struct {
		model string
		want  *ModelPrice
	}{
		{
			model: "deepseek-v4-flash",
			want:  &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
		},
		{
			model: "deepseek-chat",
			want:  &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
		},
		{
			model: "deepseek-v4-pro",
			want:  &ModelPrice{InputPrice: 3.0, OutputPrice: 6.0, CacheReadInputPrice: 0.1},
		},
		{
			model: "deepseek-reasoner",
			want:  &ModelPrice{InputPrice: 3.0, OutputPrice: 6.0, CacheReadInputPrice: 0.1},
		},
		{
			model: "DeepSeek-V4-Flash",
			want:  &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
		},
		{
			model: "unknown-deepseek-model",
			want:  &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
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
			got := GetBuiltinModelPrice(tt.model)
			if tt.want == nil {
				if got != nil {
					t.Errorf("GetBuiltinModelPrice(%q) = %+v, want nil", tt.model, got)
				}
				return
			}
			if got == nil {
				t.Errorf("GetBuiltinModelPrice(%q) = nil, want %+v", tt.model, tt.want)
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

func TestCalculateCost(t *testing.T) {
	tests := []struct {
		name  string
		usage *Usage
		price *ModelPrice
		want  float64
	}{
		{
			name:  "nil usage",
			usage: nil,
			price: &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0},
			want:  0,
		},
		{
			name:  "nil price",
			usage: &Usage{InputTokens: 1000, OutputTokens: 500},
			price: nil,
			want:  0,
		},
		{
			name:  "zero price",
			usage: &Usage{InputTokens: 1000, OutputTokens: 500},
			price: &ModelPrice{InputPrice: 0, OutputPrice: 0},
			want:  0,
		},
		{
			name:  "basic calculation",
			usage: &Usage{InputTokens: 1_000_000, OutputTokens: 500_000},
			price: &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0},
			want:  1.0 + 1.0, // 1M input * ¥1 + 500K output * ¥2/1M
		},
		{
			name: "with cache read",
			usage: &Usage{
				InputTokens:          1_000_000,
				OutputTokens:         500_000,
				CacheReadInputTokens: 300_000,
			},
			price: &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
			// Cache miss: 700K * 1 / 1M = 0.7
			// Cache read: 300K * 0.02 / 1M = 0.006
			// Output: 500K * 2 / 1M = 1.0
			// Total: 1.706
			want: 0.7 + 0.006 + 1.0,
		},
		{
			name: "cache read falls back to input price",
			usage: &Usage{
				InputTokens:          1_000_000,
				CacheReadInputTokens: 400_000,
			},
			price: &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0}, // no CacheReadInputPrice
			// Cache miss: 600K * 1 / 1M = 0.6
			// Cache read: 400K * 1 / 1M = 0.4 (fallback to input price)
			// Total: 1.0
			want: 1.0,
		},
		{
			name: "small tokens",
			usage: &Usage{
				InputTokens:  100,
				OutputTokens: 50,
			},
			price: &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0},
			want:  100.0/1_000_000*1.0 + 50.0/1_000_000*2.0,
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
			// Input (cache miss): 1M * 1 = 1.0
			// Cache creation: 200K * 1.5/1M = 0.3
			// Output: 500K * 2/1M = 1.0
			want: 1.0 + 0.3 + 1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateCost(tt.usage, tt.price)
			if math.Abs(got-tt.want) > 0.0001 {
				t.Errorf("CalculateCost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCalculateCost_DeepSeekV4Flash_RealWorld(t *testing.T) {
	// Simulate a real conversation: several turns with increasing context
	price := GetBuiltinModelPrice("deepseek-v4-flash")
	if price == nil {
		t.Fatal("price should not be nil for deepseek-v4-flash")
	}

	// Turn 1: small context, small output
	cost1 := CalculateCost(&Usage{
		InputTokens:  5_000,
		OutputTokens: 1_000,
	}, price)
	expected1 := 5000.0/1_000_000*1.0 + 1000.0/1_000_000*2.0 // 0.005 + 0.002 = 0.007
	if math.Abs(cost1-expected1) > 0.0001 {
		t.Errorf("turn 1 cost = %v, want %v", cost1, expected1)
	}

	// Turn 2: larger context (includes previous turn), larger output
	cost2 := CalculateCost(&Usage{
		InputTokens:  50_000,
		OutputTokens: 5_000,
	}, price)
	expected2 := 50000.0/1_000_000*1.0 + 5000.0/1_000_000*2.0 // 0.05 + 0.01 = 0.06
	if math.Abs(cost2-expected2) > 0.0001 {
		t.Errorf("turn 2 cost = %v, want %v", cost2, expected2)
	}

	// Verify total is approximately 0.067 CNY (less than ¥0.07 for a small conversation)
	total := cost1 + cost2
	if total > 0.1 {
		t.Errorf("total cost %v seems too high for a small conversation", total)
	}
}

func TestResolveModelPrice(t *testing.T) {
	inputPrice := 5.0
	outputPrice := 10.0

	tests := []struct {
		name               string
		model              string
		inputPrice         *float64
		outputPrice        *float64
		cacheReadPrice     *float64
		cacheCreationPrice *float64
		wantNil            bool
	}{
		{
			name:        "provider override",
			model:       "deepseek-v4-flash",
			inputPrice:  &inputPrice,
			outputPrice: &outputPrice,
			wantNil:     false,
		},
		{
			name:    "built-in deepseek",
			model:   "deepseek-chat",
			wantNil: false,
		},
		{
			name:    "unknown model no override",
			model:   "unknown-model",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveModelPrice(tt.model, tt.inputPrice, tt.outputPrice, tt.cacheReadPrice, tt.cacheCreationPrice)
			if tt.wantNil && got != nil {
				t.Errorf("ResolveModelPrice() = %+v, want nil", got)
			}
			if !tt.wantNil && got == nil {
				t.Errorf("ResolveModelPrice() = nil, want non-nil")
			}
			if tt.inputPrice != nil && got != nil && got.InputPrice != *tt.inputPrice {
				t.Errorf("InputPrice = %v, want %v", got.InputPrice, *tt.inputPrice)
			}
		})
	}
}
