package commands

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
)

func f64(v float64) *float64 { return &v }

func TestResolveModelPrice_UsesProviderOverrides(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			Name:        "custom",
			Type:        "anthropic",
			Model:       "claude-sonnet-4-5",
			InputPrice:  f64(1.5),
			OutputPrice: f64(7.5),
		}},
	}

	// A model absent from the built-in table: only the overrides can supply a price,
	// which is exactly the case ACP's /usage used to get wrong.
	price := ResolveModelPrice(cfg, "custom", "claude-sonnet-4-5")
	if price == nil {
		t.Fatal("expected a price")
	}
	if price.InputPrice != 1.5 {
		t.Errorf("InputPrice = %v, want 1.5 (configured override)", price.InputPrice)
	}
	if price.OutputPrice != 7.5 {
		t.Errorf("OutputPrice = %v, want 7.5 (configured override)", price.OutputPrice)
	}
}

func TestResolveModelPrice_CacheOverrides(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			Name:                    "custom",
			CacheReadInputPrice:     f64(0.3),
			CacheCreationInputPrice: f64(3.75),
		}},
	}

	price := ResolveModelPrice(cfg, "custom", "some-model")
	if price == nil {
		t.Fatal("expected a price")
	}
	if price.CacheReadInputPrice != 0.3 {
		t.Errorf("CacheReadInputPrice = %v, want 0.3", price.CacheReadInputPrice)
	}
	if price.CacheCreationInputPrice != 3.75 {
		t.Errorf("CacheCreationInputPrice = %v, want 3.75", price.CacheCreationInputPrice)
	}
}

// TestResolveModelPrice_FallsBackToBuiltin covers a provider that exists in
// config but sets no prices: the built-in table must still apply.
// deepseek-chat is used because the built-in table only covers DeepSeek models.
func TestResolveModelPrice_FallsBackToBuiltin(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "plain", Type: "openai"}},
	}

	withProvider := ResolveModelPrice(cfg, "plain", "deepseek-chat")
	noProvider := ResolveModelPrice(cfg, "", "deepseek-chat")

	if withProvider == nil || noProvider == nil {
		t.Fatalf("expected built-in price for a known model (got %v / %v)", withProvider, noProvider)
	}
	if *withProvider != *noProvider {
		t.Errorf("a provider without prices should match the built-in lookup: %+v vs %+v",
			withProvider, noProvider)
	}
}

func TestResolveModelPrice_UnknownProviderName(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "known", InputPrice: f64(99)}},
	}

	price := ResolveModelPrice(cfg, "missing", "deepseek-chat")
	builtin := ResolveModelPrice(nil, "", "deepseek-chat")

	if price == nil || builtin == nil {
		t.Fatalf("expected built-in price (got %v / %v)", price, builtin)
	}
	if price.InputPrice == 99 {
		t.Error("prices from an unrelated provider must not leak in")
	}
	if *price != *builtin {
		t.Errorf("unknown provider should fall back to built-in: %+v vs %+v", price, builtin)
	}
}

func TestResolveModelPrice_NilConfig(t *testing.T) {
	if got := ResolveModelPrice(nil, "anything", "deepseek-chat"); got == nil {
		t.Error("nil config should still resolve built-in prices")
	}
	if got := ResolveModelPrice(nil, "", "totally-unknown-model-xyz"); got != nil {
		t.Errorf("unknown model with no config should yield nil, got %+v", got)
	}
}
