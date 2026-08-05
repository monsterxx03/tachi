package llm

import (
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
