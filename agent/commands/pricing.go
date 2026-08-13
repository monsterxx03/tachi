package commands

import (
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// ResolveModelPrice resolves the effective price for a model RIGHT NOW,
// honouring per-provider price overrides (including time-of-use bands) from
// config and falling back to the built-in price table. The returned snapshot
// is pinned to now — time-of-use bands have been applied and are consumed.
//
// Thin adapter over llm.ResolveModelPriceAt: cfg implements
// llm.PriceScheduleSource, so all pricing semantics (built-in table,
// versioning, band inheritance, time pinning) live in the llm package.
//
// The three frontends locate the provider name differently (the TUI reads
// cfg.Provider, channel uses the resolved provider for the thread, ACP walks
// the session's provider name), so the name is a parameter — but everything
// after that is identical, and used to be copied three times. The ACP copy
// passed nil for all four overrides, silently ignoring configured prices and
// reporting wrong costs in /usage for any provider with custom pricing.
//
// providerName may be empty, in which case only built-in prices apply.
//
// Returns nil when no effective price exists (unknown model, no overrides)
// OR when the resolved price is fully zero (explicitly free) — both mean
// "costs 0" to every caller, so the distinction is not preserved (the
// ledger path via llm.ResolveModelPriceAt keeps it: it still records the
// row, band name and zero prices).
func ResolveModelPrice(cfg *config.Config, providerName, model string) *llm.ModelPrice {
	rp := llm.ResolveModelPriceAt(cfg, providerName, model, time.Now())
	if !rp.HasPrice() {
		return nil
	}
	return &rp.Price
}
