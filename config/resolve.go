package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// defaultContextWindow is used when the model is unknown and no override is configured.
const defaultContextWindow int64 = 128_000

// ErrProviderNotFound is returned by BuildProvider when the requested name is
// empty or not present in the providers list. Callers decide between silent
// fallback (dedicated providers) and fail-fast (/model switch, github).
var ErrProviderNotFound = errors.New("provider not found")

// NewProvider constructs an llm.Provider from this ProviderConfig and
// returns it embedded in the ResolvedProvider — one object carrying both the
// instance and its resolved config. This is the low-level entry for callers
// that hold a ProviderConfig directly (deepresearch's provider snapshot);
// most callers use cfg.BuildProvider(name) or cfg.DefaultProvider() instead.
func (pCfg *ProviderConfig) NewProvider() (*ResolvedProvider, error) {
	resolved, err := resolveProviderConfig(pCfg)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %q: %w", pCfg.Name, err)
	}
	// Map the resolved fields onto llm.NewNamedProvider's positional args
	// (type, name, apiKey, baseURL, model — the order matches the signature).
	p, err := llm.NewNamedProvider(resolved.Type, resolved.Name, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		return nil, fmt.Errorf("create provider %q: %w", pCfg.Name, err)
	}
	resolved.Provider = p
	return resolved, nil
}

// BuildProvider resolves a named provider and constructs the llm.Provider —
// the single home of the ProviderConfig → NewProvider dance that used to be
// hand-copied across frontends (channel /model, github, tui selectors, acp,
// main). An empty name resolves to the DEFAULT provider (same as
// DefaultProvider). Returns ErrProviderNotFound when neither the name nor
// the default resolves; resolution/construction errors are wrapped with
// context.
//
// The returned ResolvedProvider embeds the provider instance (Provider) plus
// its resolved config — callers that need ContextWindow / Model don't
// re-resolve.
func (c *Config) BuildProvider(name string) (*ResolvedProvider, error) {
	name = c.resolveName(name)
	if name == "" {
		return nil, ErrProviderNotFound
	}
	pCfg := c.ProviderConfig(name)
	if pCfg == nil {
		return nil, ErrProviderNotFound
	}
	resolved, err := pCfg.NewProvider()
	if err != nil {
		return nil, err
	}
	// Session-level execution limits live on the Config, not the ProviderConfig
	// — fill them here so every caller gets a complete ResolvedProvider.
	resolved.MaxTokens = c.MaxTokens
	resolved.MaxIterations = c.GetMaxIterations()
	return resolved, nil
}

// DefaultProvider resolves and constructs the DEFAULT provider in one step —
// the standard entry point for frontends (TUI, ACP, channel) that need both
// the provider instance and its resolved config (ContextWindow / Thinking /
// MaxTokens). The constructed provider carries its config name
// (NewNamedProvider) for ledger grouping. Equivalent to BuildProvider("").
func (c *Config) DefaultProvider() (*ResolvedProvider, error) {
	if c.DefaultProviderName() == "" {
		return nil, fmt.Errorf("no provider configured; create %s", filepath.Join(BaseDir(), "config.yaml"))
	}
	return c.BuildProvider("")
}

type ResolvedProvider struct {
	// Provider is the constructed llm.Provider instance (nil on resolution
	// failure). Embedded so callers get provider + resolved config in one
	// object.
	Provider llm.Provider

	Type          string
	Model         string
	BaseURL       string
	APIKey        string
	ContextWindow int64  // Resolved context window size (from model info or config override)
	Name          string // Provider config name (e.g., "gpt-5.2", "claude")

	// Thinking 思考模式显式开关（nil = 使用 provider/模型默认）。
	// 由 ProviderConfig.ThinkingLevel 解析而来："none" → false，其余 → nil。
	Thinking *bool
	// ThinkingEffort 思考强度（原样透传；空 = 模型默认）。
	ThinkingEffort string

	// Session-level execution limits, resolved from the config (per-session
	// values, not per-provider — kept here so every caller of BuildProvider /
	// DefaultProvider gets them in one struct).
	MaxTokens     int
	MaxIterations int
}

// ProviderConfig looks up a provider's raw config by name (aliases resolved first) and
// returns its raw ProviderConfig — for existence checks and reading Spec
// fields without full resolution. Most callers want BuildProvider instead.
// An empty name resolves to the DEFAULT provider.
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

func resolveAPIKey(pCfg *ProviderConfig) (key string, envName string) {
	envName = envForProviderType(pCfg.Type)
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v, envName
		}
	}
	return pCfg.APIKey, envName
}

// resolveProviderConfig resolves a single ProviderConfig into a
// ResolvedProvider. Package-internal: callers use the named entry points
// (BuildProvider / DefaultProvider / NewProvider).
func resolveProviderConfig(pCfg *ProviderConfig) (*ResolvedProvider, error) {
	if pCfg.Type == "" {
		return nil, fmt.Errorf("provider %q has no type set", pCfg.Name)
	}
	if pCfg.Model == "" {
		return nil, fmt.Errorf("model is required for provider %q", pCfg.Name)
	}
	apiKey, envName := resolveAPIKey(pCfg)
	if apiKey == "" {
		if envName != "" {
			return nil, fmt.Errorf("API key required for provider %q; set %s or add api_key in config", pCfg.Name, envName)
		}
		return nil, fmt.Errorf("API key required for provider %q; add api_key in config", pCfg.Name)
	}

	// Resolve context window: config override > model info lookup > default
	contextWindow := defaultContextWindow
	if pCfg.Spec.ContextWindow != nil && *pCfg.Spec.ContextWindow > 0 {
		contextWindow = *pCfg.Spec.ContextWindow
	} else if cw := llm.ModelContextWindow(pCfg.Model); cw > 0 {
		contextWindow = cw
	}

	// Resolve thinking level: "none" disables thinking mode; any other
	// non-empty level sets the effort. Levels are passed through to the API
	// unchanged — providers that accept a subset map the effort server-side
	// (e.g. DeepSeek's thinking_mode docs), so client-side normalization
	// would only distort the user's intent. Empty leaves both nil/empty,
	// meaning "use the provider/model default".
	var thinking *bool
	var thinkingEffort string
	switch pCfg.Spec.ThinkingLevel {
	case "":
		// model default
	case "none":
		v := false
		thinking = &v
		// o1/o3/o4/gpt-5 等推理模型的思考模式无法关闭：Thinking=false
		// 在 OpenAI 路径只会"不设置 ReasoningEffort"，模型仍会推理。
		if llm.IsReasoningModelPrefix(pCfg.Model) {
			logger.New("config").Warn(context.Background(),
				"thinking_level: none has no effect on reasoning models (o1/o3/o4/gpt-5); thinking stays on",
				"model", pCfg.Model)
		}
	default:
		thinkingEffort = pCfg.Spec.ThinkingLevel
	}

	return &ResolvedProvider{
		Type:           pCfg.Type,
		Model:          pCfg.Model,
		BaseURL:        pCfg.BaseURL,
		APIKey:         apiKey,
		ContextWindow:  contextWindow,
		Name:           pCfg.Name,
		Thinking:       thinking,
		ThinkingEffort: thinkingEffort,
	}, nil
}

func envForProviderType(providerType string) string {
	switch providerType {
	case llm.ProviderTypeOpenAI:
		return "OPENAI_API_KEY"
	case llm.ProviderTypeOpenAIResponses:
		return "OPENAI_API_KEY"
	case llm.ProviderTypeAnthropic:
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
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
