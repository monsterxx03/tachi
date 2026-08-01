package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
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

	// Thinking 思考模式显式开关（nil = 使用 provider/模型默认）。
	// 由 ProviderConfig.ThinkingLevel 解析而来："none" → false，其余 → nil。
	Thinking *bool
	// ThinkingEffort 思考强度（已按模型归一化到支持范围；空 = 模型默认）。
	ThinkingEffort string
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
	if pCfg.Spec.ContextWindow != nil && *pCfg.Spec.ContextWindow > 0 {
		contextWindow = *pCfg.Spec.ContextWindow
	} else if cw := llm.ModelContextWindow(pCfg.Model); cw > 0 {
		contextWindow = cw
	}

	// Resolve thinking level: "none" disables thinking mode; any other
	// non-empty level sets the effort (normalized to the model's supported
	// range — e.g. deepseek-v4-flash degrades "max" to "high"). Empty leaves
	// both nil/empty, meaning "use the provider/model default".
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
		thinkingEffort = llm.NormalizeThinkingEffort(pCfg.Model, pCfg.Spec.ThinkingLevel)
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
