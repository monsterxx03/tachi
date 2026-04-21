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
	Provider       ResolvedProvider
	MaxTokens      int
	MaxIterations  int
	ThinkingBudget int64
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

	if pCfg.Type == "" {
		return nil, fmt.Errorf("provider %q has no type set", providerName)
	}

	model := pCfg.Model
	if flags.ModelSet {
		model = flags.Model
	}
	if model == "" {
		return nil, fmt.Errorf("model is required for provider %q; set it in config or use --model", providerName)
	}

	baseURL := pCfg.BaseURL
	if flags.BaseURLSet {
		baseURL = flags.BaseURL
	}

	apiKey, envName := resolveAPIKey(pCfg)
	if apiKey == "" {
		if envName != "" {
			return nil, fmt.Errorf("API key required for provider %q; set %s or add api_key in config", providerName, envName)
		}
		return nil, fmt.Errorf("API key required for provider %q; add api_key in config", providerName)
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
		Provider: ResolvedProvider{
			Type:    pCfg.Type,
			Model:   model,
			BaseURL: baseURL,
			APIKey:  apiKey,
		},
		MaxTokens:      maxTokens,
		MaxIterations:  maxIterations,
		ThinkingBudget: cfg.ThinkingBudget,
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

func resolveAPIKey(pCfg *ProviderConfig) (key string, envName string) {
	envName = envForProviderType(pCfg.Type)
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v, envName
		}
	}
	return pCfg.APIKey, envName
}

func envForProviderType(providerType string) string {
	switch providerType {
	case ProviderTypeOpenAI:
		return "OPENAI_API_KEY"
	case ProviderTypeAnthropic:
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}
