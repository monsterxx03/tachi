package commands

import (
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// ResolveModelPrice resolves the effective price for a model, honouring
// per-provider price overrides from config and falling back to the built-in
// price table.
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
// Note llm.ResolveModelPrice already falls back to the built-in price table
// when all four overrides are nil, so a provider config without prices needs
// no extra handling here.
func ResolveModelPrice(cfg *config.Config, providerName, model string) *llm.ModelPrice {
	if cfg != nil && providerName != "" {
		if pCfg := cfg.FindProvider(providerName); pCfg != nil {
			return llm.ResolveModelPrice(
				model,
				pCfg.InputPrice,
				pCfg.OutputPrice,
				pCfg.CacheReadInputPrice,
				pCfg.CacheCreationInputPrice,
			)
		}
	}
	return llm.ResolveModelPrice(model, nil, nil, nil, nil)
}
