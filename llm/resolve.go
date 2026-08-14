package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// defaultContextWindow is used when the model is unknown and no override is configured.
const defaultContextWindow int64 = 128_000

// ErrProviderNotFound is returned by BuildProvider when the requested name is
// empty or not present in the providers list. Callers decide between silent
// fallback (dedicated providers) and fail-fast (/model switch, github).
var ErrProviderNotFound = errors.New("provider not found")

// NewProviderFromConfig constructs a Provider from a config.ProviderConfig
// and returns it embedded in the ResolvedProvider — one object carrying both
// the instance and its resolved config. This is the low-level entry for
// callers that hold a ProviderConfig directly (deepresearch's provider
// snapshot); most callers use BuildProvider(cfg, name) or
// DefaultProvider(cfg) instead.
func NewProviderFromConfig(pCfg *config.ProviderConfig) (*ResolvedProvider, error) {
	resolved, err := resolveProviderConfig(pCfg)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %q: %w", pCfg.Name, err)
	}
	// Map the resolved fields onto NewNamedProvider's positional args
	// (type, name, apiKey, baseURL, model — the order matches the signature),
	// plus provider options resolved from the spec (retry / timeout).
	p, err := NewNamedProvider(resolved.Type, resolved.Name, resolved.APIKey, resolved.BaseURL, resolved.Model, providerOptionsFromSpec(&pCfg.Spec)...)
	if err != nil {
		return nil, fmt.Errorf("create provider %q: %w", pCfg.Name, err)
	}
	resolved.Provider = p
	return resolved, nil
}

// providerOptionsFromSpec 将 ProviderConfig.Spec 中可配置的 provider 行为
// （重试次数、请求超时）转换为 config.ProviderOption。未设置的字段不产生
// option，由各 provider 路径使用默认值。
func providerOptionsFromSpec(spec *config.ModelSpec) []config.ProviderOption {
	var opts []config.ProviderOption
	if spec.MaxRetries != nil {
		opts = append(opts, config.WithMaxRetries(*spec.MaxRetries))
	}
	if spec.Timeout > 0 {
		opts = append(opts, config.WithTimeout(spec.Timeout))
	}
	return opts
}

// BuildProvider resolves a named provider and constructs the Provider —
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
func BuildProvider(cfg *config.Config, name string) (*ResolvedProvider, error) {
	pCfg := cfg.ProviderConfig(name)
	if pCfg == nil {
		return nil, ErrProviderNotFound
	}
	resolved, err := NewProviderFromConfig(pCfg)
	if err != nil {
		return nil, err
	}
	// Session-level execution limits live on the Config, not the ProviderConfig
	// — fill them here so every caller gets a complete ResolvedProvider.
	resolved.MaxTokens = cfg.MaxTokens
	resolved.MaxIterations = cfg.GetMaxIterations()
	return resolved, nil
}

// DefaultProvider resolves and constructs the DEFAULT provider in one step —
// the standard entry point for frontends (TUI, ACP, channel) that need both
// the provider instance and its resolved config (ContextWindow / Thinking /
// MaxTokens). The constructed provider carries its config name
// (NewNamedProvider) for ledger grouping. Equivalent to BuildProvider(cfg, "").
func DefaultProvider(cfg *config.Config) (*ResolvedProvider, error) {
	if cfg.DefaultProviderName() == "" {
		return nil, fmt.Errorf("no provider configured; create %s", filepath.Join(config.BaseDir(), "config.yaml"))
	}
	return BuildProvider(cfg, "")
}

// ResolvedProvider carries a constructed provider instance plus its resolved
// config in one object — the return type of BuildProvider / DefaultProvider.
type ResolvedProvider struct {
	// Provider is the constructed Provider instance (nil on resolution
	// failure). Embedded so callers get provider + resolved config in one
	// object.
	Provider Provider

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

	// SupportsVision 标记模型是否支持图片（多模态）输入。
	// 由 ProviderConfig.Spec.Vision 显式配置解析而来；未配置时按模型名
	// 内置能力表（ModelSupportsVision）自动判断。为 false 时，若输入包含
	// 图片，agent 会用配置中第一个支持图片的 provider 描述图片后再交给
	// 当前模型（见 agent.describeImages）。
	SupportsVision bool

	// Session-level execution limits, resolved from the config (per-session
	// values, not per-provider — kept here so every caller of BuildProvider /
	// DefaultProvider gets them in one struct).
	MaxTokens     int
	MaxIterations int
}

// resolveProviderConfig resolves a single ProviderConfig into a
// ResolvedProvider. Package-internal: callers use the named entry points
// (BuildProvider / DefaultProvider / NewProvider).
func resolveProviderConfig(pCfg *config.ProviderConfig) (*ResolvedProvider, error) {
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
	} else if cw := ModelContextWindow(pCfg.Model); cw > 0 {
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
		if IsReasoningModelPrefix(pCfg.Model) {
			logger.New("llm").Warn(context.Background(),
				"thinking_level: none has no effect on reasoning models (o1/o3/o4/gpt-5); thinking stays on",
				"model", pCfg.Model)
		}
	default:
		thinkingEffort = pCfg.Spec.ThinkingLevel
	}

	// Resolve vision capability: explicit spec.vision override wins;
	// otherwise fall back to the built-in model-name capability table.
	// ModelSupportsVision is conservative (unknown names → false), so an
	// unmarked text-only model is never assumed vision-capable.
	supportsVision := ProviderConfigSupportsVision(pCfg)

	return &ResolvedProvider{
		Type:           pCfg.Type,
		Model:          pCfg.Model,
		BaseURL:        pCfg.BaseURL,
		APIKey:         apiKey,
		ContextWindow:  contextWindow,
		Name:           pCfg.Name,
		Thinking:       thinking,
		ThinkingEffort: thinkingEffort,
		SupportsVision: supportsVision,
	}, nil
}

// ProviderConfigSupportsVision reports whether a provider config's model
// accepts image input: an explicit spec.vision override wins, otherwise the
// built-in model-name capability table (ModelSupportsVision) is consulted.
// This is the single source of truth shared by ResolvedProvider resolution
// and the agent's vision-fallback delegate selection.
func ProviderConfigSupportsVision(pCfg *config.ProviderConfig) bool {
	if pCfg == nil {
		return false
	}
	if pCfg.Spec.Vision != nil {
		return *pCfg.Spec.Vision
	}
	return ModelSupportsVision(pCfg.Model)
}

func resolveAPIKey(pCfg *config.ProviderConfig) (key string, envName string) {
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
	case config.ProviderTypeOpenAI:
		return "OPENAI_API_KEY"
	case config.ProviderTypeOpenAIResponses:
		return "OPENAI_API_KEY"
	case config.ProviderTypeAnthropic:
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}

// PricingSchedule returns the provider's pricing override block verbatim
// (nil = unknown provider or no pricing configured — ResolveModelPriceAt
// then uses the built-in table alone). The type is config.PricingConfig, so
// there is NO translation: the config carries the resolver's own schema, one
// source of truth.
//
// This is the llm side of the pricing resolver: all pricing SEMANTICS
// (built-in table, versioning, band inheritance, time pinning) live here;
// config only loads user-facing YAML into config types.
func PricingSchedule(cfg *config.Config, providerName string) *config.PricingConfig {
	if cfg == nil || providerName == "" {
		return nil
	}
	pCfg := cfg.ProviderConfig(providerName)
	if pCfg == nil {
		return nil
	}
	return pCfg.Spec.Pricing
}
