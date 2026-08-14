package llm

import "github.com/monsterxx03/tachi/config"

// applyOptions applies a set of options to a config.ProviderOptions and
// returns it. ProviderOptions / ProviderOption / WithMaxRetries / WithTimeout
// live in config (pure data, shared with the config package); the provider
// constructors consume them here.
func applyOptions(opts []config.ProviderOption) config.ProviderOptions {
	var o config.ProviderOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}
