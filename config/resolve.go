package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/llm"
)

// DefaultContextWindow is used when the model is unknown and no override is configured.
const DefaultContextWindow int64 = 128_000

type CLIFlags struct {
	Provider         string
	ProviderSet      bool
	Model            string
	ModelSet         bool
	BaseURL          string
	BaseURLSet       bool
	MaxTokens        int
	MaxTokensSet     bool
	MaxIterations    int
	MaxIterationsSet bool
}

type ResolvedProvider struct {
	Type          string
	Model         string
	BaseURL       string
	APIKey        string
	ContextWindow int64  // Resolved context window size (from model info or config override)
	Name          string // Provider config name (e.g., "gpt-5.2", "claude")
}

type ResolvedConfig struct {
	Provider      ResolvedProvider
	MaxTokens     int
	MaxIterations int
}

func Resolve(cfg *Config, flags CLIFlags) (*ResolvedConfig, error) {
	providerName := resolveProviderName(cfg, flags)
	if providerName == "" {
		return nil, fmt.Errorf("no provider configured; create %s or use --provider", filepath.Join(BaseDir(), "config.yaml"))
	}

	pCfg := cfg.FindProvider(providerName)
	if pCfg == nil {
		return nil, fmt.Errorf("provider %q not found in providers list", providerName)
	}

	overridden := *pCfg
	if flags.ModelSet {
		overridden.Model = flags.Model
	}
	if flags.BaseURLSet {
		overridden.BaseURL = flags.BaseURL
	}

	resolved, err := ResolveProviderConfig(&overridden)
	if err != nil {
		return nil, err
	}

	maxTokens := cfg.MaxTokens
	if flags.MaxTokensSet {
		maxTokens = flags.MaxTokens
	}

	maxIterations := cfg.GetMaxIterations()
	if flags.MaxIterationsSet {
		maxIterations = flags.MaxIterations
	}

	return &ResolvedConfig{
		Provider:       *resolved,
		MaxTokens:      maxTokens,
		MaxIterations:  maxIterations,
	}, nil
}

func resolveProviderName(cfg *Config, flags CLIFlags) string {
	if flags.ProviderSet {
		return flags.Provider
	}
	if cfg.Provider != "" {
		return cfg.Provider
	}
	if len(cfg.Providers) == 1 {
		return cfg.Providers[0].Name
	}
	return ""
}

func ResolveAPIKey(pCfg *ProviderConfig) (key string, envName string) {
	envName = EnvForProviderType(pCfg.Type)
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v, envName
		}
	}
	return pCfg.APIKey, envName
}

func ResolveProviderConfig(pCfg *ProviderConfig) (*ResolvedProvider, error) {
	if pCfg.Type == "" {
		return nil, fmt.Errorf("provider %q has no type set", pCfg.Name)
	}
	if pCfg.Model == "" {
		return nil, fmt.Errorf("model is required for provider %q", pCfg.Name)
	}
	apiKey, envName := ResolveAPIKey(pCfg)
	if apiKey == "" {
		if envName != "" {
			return nil, fmt.Errorf("API key required for provider %q; set %s or add api_key in config", pCfg.Name, envName)
		}
		return nil, fmt.Errorf("API key required for provider %q; add api_key in config", pCfg.Name)
	}

	// Resolve context window: config override > model info lookup > default
	contextWindow := DefaultContextWindow
	if pCfg.ContextWindow != nil && *pCfg.ContextWindow > 0 {
		contextWindow = *pCfg.ContextWindow
	} else if cw := llm.ModelContextWindow(pCfg.Model); cw > 0 {
		contextWindow = cw
	}

	return &ResolvedProvider{
		Type:          pCfg.Type,
		Model:         pCfg.Model,
		BaseURL:       pCfg.BaseURL,
		APIKey:        apiKey,
		ContextWindow: contextWindow,
		Name:          pCfg.Name,
	}, nil
}

func EnvForProviderType(providerType string) string {
	switch providerType {
	case llm.ProviderTypeOpenAI:
		return "OPENAI_API_KEY"
	case llm.ProviderTypeAnthropic:
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}

// ResolveSessionProvider finds and resolves a provider config by type and model.
// It searches the config's providers list for the first match on type, then
// checks if a more specific type+model match exists. If the model differs from
// the matched config, the model is overridden. Returns nil if no matching
// provider type is found in config.
func ResolveSessionProvider(cfg *Config, providerType, model string) (*ResolvedProvider, error) {
	// First pass: find a provider with matching type + model
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.Type == providerType && p.Model == model {
			return ResolveProviderConfig(p)
		}
	}

	// Second pass: find a provider with matching type (any model)
	// — the model will be overridden below.
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.Type == providerType {
			overridden := *p
			overridden.Model = model
			return ResolveProviderConfig(&overridden)
		}
	}

	return nil, fmt.Errorf("no provider with type %q found in config", providerType)
}
