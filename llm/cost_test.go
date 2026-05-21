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

func TestCalculateUsageCosts(t *testing.T) {
	price := &ModelPrice{InputPrice: 2.0, OutputPrice: 4.0}

	usages := []Usage{
		{InputTokens: 100_000, OutputTokens: 50_000},
		{InputTokens: 200_000, OutputTokens: 100_000},
	}

	total := CalculateUsageCosts(usages, price)
	// Usage 0: 100K*2/1M + 50K*4/1M = 0.2 + 0.2 = 0.4
	// Usage 1: 200K*2/1M + 100K*4/1M = 0.4 + 0.4 = 0.8
	// Total: 1.2
	expected := 0.4 + 0.8
	if total < expected-0.0001 || total > expected+0.0001 {
		t.Errorf("CalculateUsageCosts() = %v, want %v (within tolerance)", total, expected)
	}
}

func TestCalculateUsageCosts_NilPrice(t *testing.T) {
	usages := []Usage{
		{InputTokens: 100_000, OutputTokens: 50_000},
	}
	total := CalculateUsageCosts(usages, nil)
	if total != 0 {
		t.Errorf("CalculateUsageCosts(nil price) = %v, want 0", total)
	}
}

func TestCalculateUsageCosts_Empty(t *testing.T) {
	total := CalculateUsageCosts(nil, &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0})
	if total != 0 {
		t.Errorf("CalculateUsageCosts(empty) = %v, want 0", total)
	}
}

func TestCalculateCost_NegativeCacheRead(t *testing.T) {
	// Edge case: CacheReadInputTokens exceeds InputTokens (shouldn't happen,
	// but test defensive handling)
	usage := &Usage{
		InputTokens:          100,
		OutputTokens:         50,
		CacheReadInputTokens: 200, // more than total input
	}
	price := &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.5}
	cost := CalculateCost(usage, price)
	// Input: max(100-200, 0) = 0, so no input cost
	// Cache read: 200 * 0.5 / 1M = 0.0001
	// Output: 50 * 2 / 1M = 0.0001
	// Total: 0.0002
	expected := 200.0/1_000_000*0.5 + 50.0/1_000_000*2.0
	if cost != expected {
		t.Errorf("CalculateCost() = %v, want %v", cost, expected)
	}
}

func TestCalculateCost_ZeroCacheReadPriceFallback(t *testing.T) {
	// CacheReadInputPrice = 0 means "not configured" → use InputPrice as fallback
	usage := &Usage{
		InputTokens:          1_000_000,
		CacheReadInputTokens: 500_000,
	}
	price := &ModelPrice{InputPrice: 1.0, OutputPrice: 0} // no cache read price set
	cost := CalculateCost(usage, price)
	// Cache miss: 500K / 1M * 1 = 0.5
	// Cache read: 500K / 1M * 1 = 0.5 (fallback to InputPrice)
	// Total: 1.0
	expected := 1.0
	if cost != expected {
		t.Errorf("CalculateCost() = %v, want %v", cost, expected)
	}
}

func TestCalculateCost_AllFields(t *testing.T) {
	usage := &Usage{
		InputTokens:              1_000_000,
		OutputTokens:             500_000,
		CacheReadInputTokens:     200_000,
		CacheCreationInputTokens: 100_000,
	}
	price := &ModelPrice{
		InputPrice:              2.0,
		OutputPrice:             4.0,
		CacheReadInputPrice:     0.5,
		CacheCreationInputPrice: 1.5,
	}
	cost := CalculateCost(usage, price)
	// Cache miss input: (1M - 200K) * 2 / 1M = 1.6
	// Cache read: 200K * 0.5 / 1M = 0.1
	// Cache creation: 100K * 1.5 / 1M = 0.15
	// Output: 500K * 4 / 1M = 2.0
	// Total: 3.85
	expected := 1.6 + 0.1 + 0.15 + 2.0
	if cost != expected {
		t.Errorf("CalculateCost() = %v, want %v", cost, expected)
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

func TestCostSummary_ZeroValues(t *testing.T) {
	// Sanity check: CostSummary with zero values
	cs := CostSummary{}
	if cs.TotalCost != 0 || cs.MainCost != 0 || cs.SubagentCost != 0 {
		t.Error("expected all zero CostSummary")
	}
}