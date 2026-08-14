package config

// ProviderConfig looks up a provider's raw config by name (aliases resolved first) and
// returns its raw ProviderConfig — for existence checks and reading Spec
// fields without full resolution. Most callers want BuildProvider instead
// (in llm). An empty name resolves to the DEFAULT provider.
func (c *Config) ProviderConfig(name string) *ProviderConfig {
	// Empty name → default provider; aliases resolved for both.
	target := c.resolveAlias(c.resolveName(name))
	// Look up the (possibly resolved) name in providers.
	for j := range c.Providers {
		if c.Providers[j].Name == target {
			return &c.Providers[j]
		}
	}
	return nil
}

// resolveName maps an empty name to the default provider's name — the shared
// `""` = default rule behind ProviderConfig / BuildProvider.
func (c *Config) resolveName(name string) string {
	if name != "" {
		return name
	}
	return c.DefaultProviderName()
}

// resolveAlias resolves an alias to the actual provider name.
// If the name is not an alias, it returns the name unchanged.
// Package-internal: alias expansion happens once at load
// (ExpandProviderAliases) and in the config-boundary exits (ProviderConfig,
// DefaultProviderName) — callers never see a raw alias.
func (c *Config) resolveAlias(name string) string {
	if c.ProviderAliases != nil {
		if t, ok := c.ProviderAliases[name]; ok {
			return t
		}
	}
	return name
}

// DefaultProviderName returns the DEFAULT provider's name: the top-level
// Provider field, or the single entry of Providers when unset. Aliases are
// resolved HERE so callers never need to know about provider_aliases — the
// returned name is always a real entry in c.Providers. This is the single
// exit point for "the default provider's config name": session metadata,
// /usage grouping and diagnostics all consume the real name without
// sprinkling resolveAlias at call sites.
func (c *Config) DefaultProviderName() string {
	name := ""
	if c.Provider != "" {
		name = c.Provider
	} else if len(c.Providers) == 1 {
		name = c.Providers[0].Name
	}
	if name == "" {
		return ""
	}
	return c.resolveAlias(name)
}

// ExpandProviderAliases rewrites every provider-reference field (top-level
// and nested) from a transient provider_aliases key (e.g. "main_provider") to
// the REAL provider config name. Called at the end of LoadFrom — this is the
// single place alias expansion happens; callers read provider fields and get
// real names without sprinkling resolveAlias. Runtime inputs (e.g. /model
// "fast", ad-hoc Config construction in tests) still go through
// ProviderConfig / DefaultProviderName / BuildProvider, which resolve aliases
// themselves — so no path needs the raw alias value.
//
// Keep this list in sync with every field that references a provider by
// name. The map itself is preserved (ProviderConfig needs it).
func (c *Config) ExpandProviderAliases() {
	if c == nil || len(c.ProviderAliases) == 0 {
		return
	}
	resolve := c.resolveAlias
	c.Provider = resolve(c.Provider)
	c.TitleProvider = resolve(c.TitleProvider)
	c.CommitProvider = resolve(c.CommitProvider)
	c.RunProvider = resolve(c.RunProvider)
	c.Subagent.Provider = resolve(c.Subagent.Provider)
	c.Review.Provider = resolve(c.Review.Provider)
	if adv := c.Review.Adversarial; adv != nil {
		for i := range adv.Models {
			adv.Models[i] = resolve(adv.Models[i])
		}
		adv.JudgeModel = resolve(adv.JudgeModel)
	}
	c.Memory.KeywordProvider = resolve(c.Memory.KeywordProvider)
	c.DeepResearch.QueryGeneratorProvider = resolve(c.DeepResearch.QueryGeneratorProvider)
	c.Dream.Provider = resolve(c.Dream.Provider)
}
