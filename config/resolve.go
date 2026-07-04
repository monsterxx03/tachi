package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/llm"
)

// DefaultContextWindow is used when the model is unknown and no override is configured.
const DefaultContextWindow int64 = 128_000

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

func Resolve(cfg *Config) (*ResolvedConfig, error) {
	providerName := ResolveProviderName(cfg)
	if providerName == "" {
		return nil, fmt.Errorf("no provider configured; create %s", filepath.Join(BaseDir(), "config.yaml"))
	}

	pCfg := cfg.FindProvider(providerName)
	if pCfg == nil {
		return nil, fmt.Errorf("provider %q not found in providers list", providerName)
	}

	resolved, err := ResolveProviderConfig(pCfg)
	if err != nil {
		return nil, err
	}

	return &ResolvedConfig{
		Provider:      *resolved,
		MaxTokens:     cfg.MaxTokens,
		MaxIterations: cfg.GetMaxIterations(),
	}, nil
}

func ResolveProviderName(cfg *Config) string {
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

