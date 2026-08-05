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

func TestNormalizeCacheMissInput(t *testing.T) {
	tests := []struct {
		name         string
		input        int64
		cacheRead    int64
		providerType string
		want         int64
	}{
		{name: "openai subtracts cache read", input: 1_000, cacheRead: 300, providerType: ProviderTypeOpenAI, want: 700},
		{name: "anthropic keeps full input", input: 1_000, cacheRead: 300, providerType: ProviderTypeAnthropic, want: 1_000},
		{name: "cache read exceeds input clamps to 0", input: 100, cacheRead: 200, providerType: ProviderTypeOpenAI, want: 0},
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
	// Unset cache prices fall back to the regular input price.
	read, creation := CacheReadCreationPrice(&ModelPrice{InputPrice: 1.0, OutputPrice: 2.0})
	if read != 1.0 || creation != 1.0 {
		t.Errorf("fallback = (%v, %v), want (1.0, 1.0)", read, creation)
	}

	// Explicit cache prices are honored.
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
			providerType: ProviderTypeOpenAI,
			want:         0,
		},
		{
			name:         "nil price",
			usage:        &Usage{InputTokens: 1000, OutputTokens: 500},
			price:        nil,
			providerType: ProviderTypeOpenAI,
			want:         0,
		},
		{
			name:         "zero price",
			usage:        &Usage{InputTokens: 1000, OutputTokens: 500},
			price:        &ModelPrice{InputPrice: 0, OutputPrice: 0},
			providerType: ProviderTypeOpenAI,
			want:         0,
		},
		{
			name:         "basic calculation",
			usage:        &Usage{InputTokens: 1_000_000, OutputTokens: 500_000},
			price:        &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0},
			providerType: ProviderTypeOpenAI,
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
			providerType: ProviderTypeOpenAI,
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
			providerType: ProviderTypeAnthropic,
			// Input kept at 1M (no subtraction): 1.0
			// Cache read: 300K * 0.02 / 1M = 0.006
			// Output: 500K * 2 / 1M = 1.0
			// Total: 2.006
			want: 1.0 + 0.006 + 1.0,
		},
		{
			name: "cache read falls back to input price",
			usage: &Usage{
				InputTokens:          1_000_000,
				CacheReadInputTokens: 400_000,
			},
			price:        &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0}, // no CacheReadInputPrice
			providerType: ProviderTypeOpenAI,
			// Cache miss: 600K * 1 / 1M = 0.6
			// Cache read: 400K * 1 / 1M = 0.4 (fallback to input price)
			// Total: 1.0
			want: 1.0,
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
			providerType: ProviderTypeOpenAI,
			// Input (cache miss): 1M * 1 = 1.0
			// Cache creation: 200K * 1.5/1M = 0.3
			// Output: 500K * 2/1M = 1.0
			want: 1.0 + 0.3 + 1.0,
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
