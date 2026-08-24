package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPricingConfigYAMLUnmarshal: PricingConfig must load from the
// user-facing YAML through yaml.v3 with the same schema as the historical
// config.ModelPricing (the tags live on the type in this package). The
// flat-price path is covered by the existing spec tests; this pins the
// time-of-use fields (timezone + bands).
func TestPricingConfigYAMLUnmarshal(t *testing.T) {
	const doc = `
provider: ds
providers:
  - name: ds
    type: openai
    model: deepseek-chat
    base_url: https://api.deepseek.com
    api_key: sk-test
    spec:
      pricing:
        timezone: "Asia/Shanghai"
        input_price: 1.5
        output_price: 4.5
        cache_read_input_price: 0.05
        bands:
          - name: peak
            days: [1,2,3,4,5]
            start: "09:00"
            end: "12:00"
            input_price: 3.0
            output_price: 9.0
`
	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(doc), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := cfg.Providers[0].Spec.Pricing
	if p == nil {
		t.Fatal("pricing block not parsed")
	}
	if p.Timezone != "Asia/Shanghai" {
		t.Errorf("timezone = %q, want Asia/Shanghai", p.Timezone)
	}
	if len(p.Bands) != 1 || p.Bands[0].Name != "peak" || p.Bands[0].Start != "09:00" ||
		p.Bands[0].End != "12:00" || p.Bands[0].InputPrice == nil || *p.Bands[0].InputPrice != 3.0 ||
		p.Bands[0].OutputPrice == nil || *p.Bands[0].OutputPrice != 9.0 {
		t.Errorf("bands = %+v, want the peak band", p.Bands)
	}
	// days: 1-7 numbers (1=Monday … 7=Sunday), loaded as-is.
	if len(p.Bands[0].Days) != 5 || p.Bands[0].Days[0] != 1 || p.Bands[0].Days[4] != 5 {
		t.Errorf("band days = %v, want [1 2 3 4 5]", p.Bands[0].Days)
	}

	// Type sanity: the field IS PricingConfig (the schema's own type).
	if _, ok := any(p).(*PricingConfig); !ok {
		t.Errorf("Spec.Pricing = %T, want *PricingConfig", p)
	}
}
