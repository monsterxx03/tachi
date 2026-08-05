package llm

import (
	"testing"
)

func TestEffortFromString_Default(t *testing.T) {
	result := effortFromString("")
	if result != "high" {
		t.Errorf("effortFromString('') = %q, want 'high'", result)
	}
}

func TestEffortFromString_Valid(t *testing.T) {
	valid := []string{"low", "medium", "high", "xhigh", "max"}
	for _, e := range valid {
		result := effortFromString(e)
		if string(result) != e {
			t.Errorf("effortFromString(%q) = %q, want %q", e, result, e)
		}
	}
}

func TestEffortFromString_Invalid(t *testing.T) {
	// Invalid values should fall back to "high"
	result := effortFromString("extreme")
	if result != "high" {
		t.Errorf("effortFromString('extreme') = %q, want 'high'", result)
	}
}

func TestResolveModelPrice_PartialOverride(t *testing.T) {
	// Only override output price, input price should be 0 (not fallback to built-in)
	outputPrice := 8.0
	price := ResolveModelPrice("deepseek-v4-flash", nil, &outputPrice, nil, nil)
	if price == nil {
		t.Fatal("expected non-nil price")
	}
	if price.InputPrice != 0 {
		t.Errorf("InputPrice = %v, want 0 (no override)", price.InputPrice)
	}
	if price.OutputPrice != 8.0 {
		t.Errorf("OutputPrice = %v, want 8.0", price.OutputPrice)
	}
	if price.CacheReadInputPrice != 0 {
		t.Errorf("CacheReadInputPrice = %v, want 0", price.CacheReadInputPrice)
	}
}
