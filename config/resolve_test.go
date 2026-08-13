package config

import (
	"testing"

	"github.com/monsterxx03/tachi/llm"
)

func f64(v float64) *float64 { return &v }

// TestConfig_PricingSchedule covers the config side of the dependency-
// inverted pricing resolver: Config.PricingSchedule returns the provider's
// llm.PricingConfig VERBATIM (nil = unknown provider / no pricing) — the
// type is the resolver's own, so there is no translation to test, only the
// wiring.
func TestConfig_PricingSchedule(t *testing.T) {
	pricing := &llm.PricingConfig{
		InputPrice:  f64(1.5),
		OutputPrice: f64(7.5),
		Timezone:    "Asia/Shanghai",
		Bands: []llm.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(3.0)},
		},
	}
	cfg := &Config{
		Providers: []ProviderConfig{{
			Name: "custom",
			Spec: ModelSpec{Pricing: pricing},
		}},
	}

	if got := cfg.PricingSchedule("custom"); got != pricing {
		t.Errorf("PricingSchedule must return the block verbatim, got %+v", got)
	}

	// Unknown provider / no pricing / nil config → nil (built-in fallback).
	if got := cfg.PricingSchedule("missing"); got != nil {
		t.Errorf("unknown provider must yield nil, got %+v", got)
	}
	if got := (&Config{Providers: []ProviderConfig{{Name: "plain"}}}).PricingSchedule("plain"); got != nil {
		t.Errorf("provider without pricing must yield nil, got %+v", got)
	}
	if got := (*Config)(nil).PricingSchedule("x"); got != nil {
		t.Errorf("nil config must yield nil, got %+v", got)
	}
}
