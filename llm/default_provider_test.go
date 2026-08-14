package llm

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultProvider_FullConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := &config.Config{
		Provider:      "my-provider",
		MaxTokens:     8000,
		MaxIterations: new(5),
		Providers: []config.ProviderConfig{
			{
				Name:    "my-provider",
				Type:    "anthropic",
				Model:   "claude-3",
				BaseURL: "https://api.example.com",
				APIKey:  "sk-from-config",
			},
		},
	}

	resolved, err := DefaultProvider(cfg)
	require.NoError(t, err)
	assert.Equal(t, "anthropic", resolved.Type)
	assert.Equal(t, "claude-3", resolved.Model)
	assert.Equal(t, "https://api.example.com", resolved.BaseURL)
	assert.Equal(t, "sk-from-config", resolved.APIKey)
	assert.Equal(t, 8000, resolved.MaxTokens)
	assert.Equal(t, 5, resolved.MaxIterations)
}

func TestDefaultProvider_EnvOverridesAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	cfg := &config.Config{
		Provider: "test",
		Providers: []config.ProviderConfig{
			{Name: "test", Type: "anthropic", Model: "claude-3", APIKey: "sk-from-config"},
		},
	}

	resolved, err := DefaultProvider(cfg)
	require.NoError(t, err)
	assert.Equal(t, "sk-from-env", resolved.APIKey)
}

func TestDefaultProvider_MissingAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := &config.Config{
		Provider: "test",
		Providers: []config.ProviderConfig{
			{Name: "test", Type: "openai", Model: "gpt-4"},
		},
	}

	_, err := DefaultProvider(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API key required")
	assert.Contains(t, err.Error(), "OPENAI_API_KEY")
}

func TestDefaultProvider_NoProviderConfigured(t *testing.T) {
	cfg := config.DefaultConfig()
	_, err := DefaultProvider(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no provider configured")
}

func TestDefaultProvider_SingleProviderAutoSelect(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := &config.Config{
		MaxTokens:     config.DefaultMaxTokens,
		MaxIterations: new(config.DefaultMaxIterations),
		Providers: []config.ProviderConfig{
			{Name: "only-one", Type: "openai", Model: "gpt-4", APIKey: "sk-test"},
		},
	}

	resolved, err := DefaultProvider(cfg)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4", resolved.Model)
}

func TestDefaultProvider_ConfigSelectsProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := &config.Config{
		Provider:      "beta",
		MaxTokens:     config.DefaultMaxTokens,
		MaxIterations: new(config.DefaultMaxIterations),
		Providers: []config.ProviderConfig{
			{Name: "alpha", Type: "openai", Model: "gpt-4", APIKey: "sk-a"},
			{Name: "beta", Type: "anthropic", Model: "claude-3", APIKey: "sk-b"},
		},
	}

	resolved, err := DefaultProvider(cfg)
	require.NoError(t, err)
	assert.Equal(t, "anthropic", resolved.Type)
	assert.Equal(t, "claude-3", resolved.Model)
	assert.Equal(t, "sk-b", resolved.APIKey)
}

func TestDefaultProvider_MissingType(t *testing.T) {
	cfg := &config.Config{
		Provider: "test",
		Providers: []config.ProviderConfig{
			{Name: "test", Model: "gpt-4", APIKey: "sk-test"},
		},
	}

	_, err := DefaultProvider(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no type set")
}
