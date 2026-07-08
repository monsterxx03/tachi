package acp

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

func TestBuildModelState(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet"},
		},
	}

	state := buildModelState(cfg, "anthropic")
	require.NotNil(t, state)
	assert.Equal(t, acp.ModelId("anthropic/claude-3-5-sonnet"), state.CurrentModelId)
	require.Len(t, state.AvailableModels, 2)
	assert.Equal(t, acp.ModelId("openai/gpt-4o-mini"), state.AvailableModels[0].ModelId)
	assert.Equal(t, "openai (gpt-4o-mini)", state.AvailableModels[0].Name)
	assert.Equal(t, acp.ModelId("anthropic/claude-3-5-sonnet"), state.AvailableModels[1].ModelId)
	assert.Equal(t, "anthropic (claude-3-5-sonnet)", state.AvailableModels[1].Name)
}

func TestBuildModelState_NoProviders(t *testing.T) {
	state := buildModelState(&config.Config{}, "")
	assert.Nil(t, state)
}

func TestBuildModelState_NilConfig(t *testing.T) {
	state := buildModelState(nil, "")
	assert.Nil(t, state)
}

func TestBuildModelState_CurrentNotMatched_FallsBackToFirst(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet"},
		},
	}

	// Current name doesn't match any provider — should fall back to first.
	state := buildModelState(cfg, "unknown")
	require.NotNil(t, state)
	assert.Equal(t, acp.ModelId("openai/gpt-4o-mini"), state.CurrentModelId)
}

func TestBuildModelState_SkipsEmptyNameOrModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "", Type: "openai", Model: "gpt-4o-mini"},       // skipped — empty name
			{Name: "empty-model", Type: "openai", Model: ""},       // skipped — empty model
			{Name: "valid", Type: "openai", Model: "gpt-4o-mini"},  // included
		},
	}

	state := buildModelState(cfg, "valid")
	require.NotNil(t, state)
	assert.Len(t, state.AvailableModels, 1)
	assert.Equal(t, "valid (gpt-4o-mini)", state.AvailableModels[0].Name)
}

func TestBuildModelConfigOption(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet"},
		},
	}

	opt, current := buildModelConfigOption(cfg, "anthropic")
	require.NotNil(t, opt)
	require.NotNil(t, opt.Select)
	assert.Equal(t, acp.SessionConfigId(modelConfigID), opt.Select.Id)
	assert.Equal(t, modelConfigName, opt.Select.Name)
	assert.Equal(t, "anthropic", current)
	require.NotNil(t, opt.Select.Options.Ungrouped)
	assert.Len(t, *opt.Select.Options.Ungrouped, 2)
	assert.Equal(t, acp.SessionConfigValueId("openai"), (*opt.Select.Options.Ungrouped)[0].Value)
	assert.Equal(t, acp.SessionConfigValueId("anthropic"), (*opt.Select.Options.Ungrouped)[1].Value)
}

func TestBuildModelConfigOption_NoProviders(t *testing.T) {
	opt, current := buildModelConfigOption(&config.Config{}, "")
	assert.Nil(t, opt)
	assert.Empty(t, current)
}

func TestSwitchSessionModel(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet", APIKey: "sk-anthropic"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := agent.NewAIAgent(provider, 0)

	sess := &ACPSession{
		cfg:   cfg,
		agent: aiAgent,
	}

	err = switchSessionModel(sess, "anthropic")
	require.NoError(t, err)

	assert.Equal(t, "anthropic", sess.resolveProviderName())
	assert.Equal(t, "anthropic", sess.ProviderType())
	assert.Equal(t, "claude-3-5-sonnet", sess.agent.Model())
}

func TestSwitchSessionModel_UnknownProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := agent.NewAIAgent(provider, 0)
	sess := &ACPSession{cfg: cfg, agent: aiAgent}

	err = switchSessionModel(sess, "unknown")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSetSessionConfigOption(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
			{Name: "anthropic", Type: "anthropic", Model: "claude-3-5-sonnet", APIKey: "sk-anthropic"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := agent.NewAIAgent(provider, 0)

	ta := NewTachiAgent(cfg, "test")
	sess := ta.sessions.New(context.Background(), "/tmp", cfg, aiAgent, nil, nil)

	resp, err := ta.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sess.ID),
			ConfigId:  acp.SessionConfigId(modelConfigID),
			Value:     acp.SessionConfigValueId("anthropic"),
		},
	})
	require.NoError(t, err)
	assert.Len(t, resp.ConfigOptions, 1)
	assert.Equal(t, "anthropic", sess.resolveProviderName())
	assert.Equal(t, "claude-3-5-sonnet", sess.agent.Model())
}

func TestSetSessionConfigOption_UnsupportedConfig(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: "openai", Model: "gpt-4o-mini", APIKey: "sk-openai"},
		},
	}

	provider, err := llm.NewProvider("openai", "sk-openai", "", "gpt-4o-mini")
	require.NoError(t, err)
	aiAgent := agent.NewAIAgent(provider, 0)

	ta := NewTachiAgent(cfg, "test")
	sess := ta.sessions.New(context.Background(), "/tmp", cfg, aiAgent, nil, nil)

	resp, err := ta.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: acp.SessionId(sess.ID),
			ConfigId:  acp.SessionConfigId("unsupported"),
			Value:     acp.SessionConfigValueId("anthropic"),
		},
	})
	assert.Error(t, err)
	assert.Empty(t, resp.ConfigOptions)
}
