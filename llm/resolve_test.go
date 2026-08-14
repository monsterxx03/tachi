package llm

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPricingSchedule covers the llm side of the pricing resolver:
// PricingSchedule returns the provider's config.PricingConfig VERBATIM
// (nil = unknown provider / no pricing) — the type is the schema's own, so
// there is no translation to test, only the wiring.
func TestPricingSchedule(t *testing.T) {
	pricing := &config.PricingConfig{
		InputPrice:  f64(1.5),
		OutputPrice: f64(7.5),
		Timezone:    "Asia/Shanghai",
		Bands: []config.PriceBandSpec{
			{Name: "peak", Start: "09:00", End: "12:00", InputPrice: f64(3.0)},
		},
	}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "custom",
			Spec: config.ModelSpec{Pricing: pricing},
		}},
	}

	if got := PricingSchedule(cfg, "custom"); got != pricing {
		t.Errorf("PricingSchedule must return the block verbatim, got %+v", got)
	}

	// Unknown provider / no pricing / nil config → nil (built-in fallback).
	if got := PricingSchedule(cfg, "missing"); got != nil {
		t.Errorf("unknown provider must yield nil, got %+v", got)
	}
	if got := PricingSchedule(&config.Config{Providers: []config.ProviderConfig{{Name: "plain"}}}, "plain"); got != nil {
		t.Errorf("provider without pricing must yield nil, got %+v", got)
	}
	if got := PricingSchedule(nil, "x"); got != nil {
		t.Errorf("nil config must yield nil, got %+v", got)
	}
}

// TestResolveProviderConfig_ThinkingLevel pins the thinking-level resolution
// rules in resolveProviderConfig (moved from config alongside the resolver).
func TestResolveProviderConfig_ThinkingLevel(t *testing.T) {
	apiKey := "sk-test"

	// "none" → Thinking=false（显式关闭思考）
	pNone := &config.ProviderConfig{
		Name:   "deepseek-flash",
		Type:   "openai",
		Model:  "deepseek-v4-flash",
		APIKey: apiKey,
		Spec:   config.ModelSpec{ThinkingLevel: "none"},
	}
	resolved, err := resolveProviderConfig(pNone)
	require.NoError(t, err)
	require.NotNil(t, resolved.Thinking)
	assert.False(t, *resolved.Thinking)
	assert.Equal(t, "", resolved.ThinkingEffort)

	// 支持的级别 → 原样保留
	pHigh := &config.ProviderConfig{
		Name:   "deepseek-pro",
		Type:   "openai",
		Model:  "deepseek-v4-pro",
		APIKey: apiKey,
		Spec:   config.ModelSpec{ThinkingLevel: "high"},
	}
	resolved, err = resolveProviderConfig(pHigh)
	require.NoError(t, err)
	assert.Nil(t, resolved.Thinking)
	assert.Equal(t, "high", resolved.ThinkingEffort)

	// 级别原样透传 — DeepSeek API 自己映射 effort（v4-flash 支持 max，
	// medium/xhigh 也有服务端映射），客户端不做归一化。
	pMaxFlash := &config.ProviderConfig{
		Name:   "deepseek-flash",
		Type:   "openai",
		Model:  "deepseek-v4-flash",
		APIKey: apiKey,
		Spec:   config.ModelSpec{ThinkingLevel: "max"},
	}
	resolved, err = resolveProviderConfig(pMaxFlash)
	require.NoError(t, err)
	assert.Nil(t, resolved.Thinking)
	assert.Equal(t, "max", resolved.ThinkingEffort)

	// 空 → 两者都不设置（provider/模型默认）
	pEmpty := &config.ProviderConfig{
		Name:   "deepseek-pro",
		Type:   "openai",
		Model:  "deepseek-v4-pro",
		APIKey: apiKey,
	}
	resolved, err = resolveProviderConfig(pEmpty)
	require.NoError(t, err)
	assert.Nil(t, resolved.Thinking)
	assert.Equal(t, "", resolved.ThinkingEffort)

	// 其他模型：级别同样原样透传
	pClaude := &config.ProviderConfig{
		Name:   "claude",
		Type:   "anthropic",
		Model:  "claude-sonnet-4-6",
		APIKey: apiKey,
		Spec:   config.ModelSpec{ThinkingLevel: "max"},
	}
	resolved, err = resolveProviderConfig(pClaude)
	require.NoError(t, err)
	assert.Nil(t, resolved.Thinking)
	assert.Equal(t, "max", resolved.ThinkingEffort)
}

func TestResolveProviderConfig_SupportsVision(t *testing.T) {
	apiKey := "sk-test"

	// 未配置 spec.vision → 按模型名自动判断
	pDeepSeek := &config.ProviderConfig{
		Name:   "deepseek-flash",
		Type:   "openai",
		Model:  "deepseek-v4-flash",
		APIKey: apiKey,
	}
	resolved, err := resolveProviderConfig(pDeepSeek)
	require.NoError(t, err)
	assert.False(t, resolved.SupportsVision, "deepseek should be auto-detected as text-only")

	pClaude := &config.ProviderConfig{
		Name:   "claude",
		Type:   "anthropic",
		Model:  "claude-sonnet-4-6",
		APIKey: apiKey,
	}
	resolved, err = resolveProviderConfig(pClaude)
	require.NoError(t, err)
	assert.True(t, resolved.SupportsVision, "claude should be auto-detected as vision-capable")

	// 显式 vision: true 覆盖自动判断（未知/文本模型强制支持）
	pUnknown := &config.ProviderConfig{
		Name:   "custom-vl",
		Type:   "openai",
		Model:  "my-private-vl-model",
		APIKey: apiKey,
		Spec:   config.ModelSpec{Vision: boolPtr(true)},
	}
	resolved, err = resolveProviderConfig(pUnknown)
	require.NoError(t, err)
	assert.True(t, resolved.SupportsVision)

	// 显式 vision: false 覆盖自动判断（能力表命中的模型强制关闭）
	pClaudeNoVision := &config.ProviderConfig{
		Name:   "claude-text",
		Type:   "anthropic",
		Model:  "claude-sonnet-4-6",
		APIKey: apiKey,
		Spec:   config.ModelSpec{Vision: boolPtr(false)},
	}
	resolved, err = resolveProviderConfig(pClaudeNoVision)
	require.NoError(t, err)
	assert.False(t, resolved.SupportsVision)
}
