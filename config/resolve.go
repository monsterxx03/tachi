package config

import (
	"fmt"
	"os"
)

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
	Type    string
	Model   string
	BaseURL string
	APIKey  string
}

type ResolvedConfig struct {
	Provider      ResolvedProvider
	MaxTokens     int
	MaxIterations int
}

func Resolve(cfg *Config, flags CLIFlags) (*ResolvedConfig, error) {
	providerName := resolveProviderName(cfg, flags)
	if providerName == "" {
		return nil, fmt.Errorf("no provider configured; create ~/.tachi/config.yaml or use --provider")
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

	maxIterations := cfg.MaxIterations
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
	return &ResolvedProvider{
		Type:    pCfg.Type,
		Model:   pCfg.Model,
		BaseURL: pCfg.BaseURL,
		APIKey:  apiKey,
	}, nil
}

func EnvForProviderType(providerType string) string {
	switch providerType {
	case ProviderTypeOpenAI:
		return "OPENAI_API_KEY"
	case ProviderTypeAnthropic:
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}
