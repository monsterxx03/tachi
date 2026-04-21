package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, DefaultMaxTokens, cfg.MaxTokens)
	assert.Equal(t, DefaultMaxIterations, cfg.MaxIterations)
	assert.Empty(t, cfg.Provider)
	assert.Empty(t, cfg.Providers)
	assert.Equal(t, int64(0), cfg.ThinkingBudget)
}

func TestLoadFrom_NonExistent(t *testing.T) {
	cfg, err := LoadFrom("/tmp/tachi-test-nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxTokens, cfg.MaxTokens)
	assert.Equal(t, DefaultMaxIterations, cfg.MaxIterations)
}

func TestLoadFrom_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `provider: my-provider
max_tokens: 8000
max_iterations: 5
thinking_budget: 10000
providers:
  - name: my-provider
    type: anthropic
    model: claude-3
    base_url: https://api.example.com
    api_key: sk-test
  - name: backup
    type: openai
    model: gpt-4
    base_url: https://api.openai.com/v1
    api_key: sk-backup
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0600))

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, "my-provider", cfg.Provider)
	assert.Equal(t, 8000, cfg.MaxTokens)
	assert.Equal(t, 5, cfg.MaxIterations)
	assert.Equal(t, int64(10000), cfg.ThinkingBudget)
	assert.Len(t, cfg.Providers, 2)
	assert.Equal(t, "my-provider", cfg.Providers[0].Name)
	assert.Equal(t, "anthropic", cfg.Providers[0].Type)
	assert.Equal(t, "claude-3", cfg.Providers[0].Model)
	assert.Equal(t, "https://api.example.com", cfg.Providers[0].BaseURL)
	assert.Equal(t, "sk-test", cfg.Providers[0].APIKey)
}

func TestLoadFrom_ZeroValueDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `providers:
  - name: test
    type: openai
    model: gpt-4
    api_key: sk-test
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0600))

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxTokens, cfg.MaxTokens)
	assert.Equal(t, DefaultMaxIterations, cfg.MaxIterations)
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	original := &Config{
		Provider:       "test-provider",
		MaxTokens:      16000,
		MaxIterations:  20,
		ThinkingBudget: 5000,
		Providers: []ProviderConfig{
			{
				Name:    "test-provider",
				Type:    "openai",
				Model:   "gpt-4",
				BaseURL: "https://api.openai.com/v1",
				APIKey:  "sk-123",
			},
		},
	}

	data, err := yaml.Marshal(original)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))

	loaded, err := LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, original.Provider, loaded.Provider)
	assert.Equal(t, original.MaxTokens, loaded.MaxTokens)
	assert.Equal(t, original.MaxIterations, loaded.MaxIterations)
	assert.Equal(t, original.ThinkingBudget, loaded.ThinkingBudget)
	assert.Len(t, loaded.Providers, 1)
	assert.Equal(t, original.Providers[0], loaded.Providers[0])
}

func TestFindProvider(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "alpha", Type: "openai"},
			{Name: "beta", Type: "anthropic"},
		},
	}

	p := cfg.FindProvider("alpha")
	require.NotNil(t, p)
	assert.Equal(t, "openai", p.Type)

	p = cfg.FindProvider("beta")
	require.NotNil(t, p)
	assert.Equal(t, "anthropic", p.Type)

	assert.Nil(t, cfg.FindProvider("gamma"))
}

func TestResolve_FullConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := &Config{
		Provider:       "my-provider",
		MaxTokens:      8000,
		MaxIterations:  5,
		ThinkingBudget: 10000,
		Providers: []ProviderConfig{
			{
				Name:    "my-provider",
				Type:    "anthropic",
				Model:   "claude-3",
				BaseURL: "https://api.example.com",
				APIKey:  "sk-from-config",
			},
		},
	}

	resolved, err := Resolve(cfg, CLIFlags{})
	require.NoError(t, err)
	assert.Equal(t, "anthropic", resolved.Provider.Type)
	assert.Equal(t, "claude-3", resolved.Provider.Model)
	assert.Equal(t, "https://api.example.com", resolved.Provider.BaseURL)
	assert.Equal(t, "sk-from-config", resolved.Provider.APIKey)
	assert.Equal(t, 8000, resolved.MaxTokens)
	assert.Equal(t, 5, resolved.MaxIterations)
	assert.Equal(t, int64(10000), resolved.ThinkingBudget)
}

func TestResolve_FlagOverrides(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := &Config{
		Provider:      "test",
		MaxTokens:     8000,
		MaxIterations: 5,
		Providers: []ProviderConfig{
			{Name: "test", Type: "openai", Model: "gpt-4", BaseURL: "https://original.com", APIKey: "sk-test"},
		},
	}

	flags := CLIFlags{
		Model:            "gpt-4o",
		ModelSet:         true,
		BaseURL:          "https://override.com",
		BaseURLSet:       true,
		MaxTokens:        16000,
		MaxTokensSet:     true,
		MaxIterations:    20,
		MaxIterationsSet: true,
	}

	resolved, err := Resolve(cfg, flags)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", resolved.Provider.Model)
	assert.Equal(t, "https://override.com", resolved.Provider.BaseURL)
	assert.Equal(t, 16000, resolved.MaxTokens)
	assert.Equal(t, 20, resolved.MaxIterations)
}

func TestResolve_EnvOverridesAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	cfg := &Config{
		Provider: "test",
		Providers: []ProviderConfig{
			{Name: "test", Type: "anthropic", Model: "claude-3", APIKey: "sk-from-config"},
		},
	}

	resolved, err := Resolve(cfg, CLIFlags{})
	require.NoError(t, err)
	assert.Equal(t, "sk-from-env", resolved.Provider.APIKey)
}

func TestResolve_MissingAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := &Config{
		Provider: "test",
		Providers: []ProviderConfig{
			{Name: "test", Type: "openai", Model: "gpt-4"},
		},
	}

	_, err := Resolve(cfg, CLIFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API key required")
	assert.Contains(t, err.Error(), "OPENAI_API_KEY")
}

func TestResolve_MissingProvider(t *testing.T) {
	cfg := &Config{
		Provider: "nonexistent",
		Providers: []ProviderConfig{
			{Name: "test", Type: "openai", Model: "gpt-4", APIKey: "sk-test"},
		},
	}

	_, err := Resolve(cfg, CLIFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolve_NoProviderConfigured(t *testing.T) {
	cfg := DefaultConfig()
	_, err := Resolve(cfg, CLIFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no provider configured")
}

func TestResolve_SingleProviderAutoSelect(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := &Config{
		MaxTokens:     DefaultMaxTokens,
		MaxIterations: DefaultMaxIterations,
		Providers: []ProviderConfig{
			{Name: "only-one", Type: "openai", Model: "gpt-4", APIKey: "sk-test"},
		},
	}

	resolved, err := Resolve(cfg, CLIFlags{})
	require.NoError(t, err)
	assert.Equal(t, "gpt-4", resolved.Provider.Model)
}

func TestResolve_FlagSelectsProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := &Config{
		Provider:      "alpha",
		MaxTokens:     DefaultMaxTokens,
		MaxIterations: DefaultMaxIterations,
		Providers: []ProviderConfig{
			{Name: "alpha", Type: "openai", Model: "gpt-4", APIKey: "sk-a"},
			{Name: "beta", Type: "anthropic", Model: "claude-3", APIKey: "sk-b"},
		},
	}

	flags := CLIFlags{Provider: "beta", ProviderSet: true}
	resolved, err := Resolve(cfg, flags)
	require.NoError(t, err)
	assert.Equal(t, "anthropic", resolved.Provider.Type)
	assert.Equal(t, "claude-3", resolved.Provider.Model)
	assert.Equal(t, "sk-b", resolved.Provider.APIKey)
}

func TestResolve_MissingType(t *testing.T) {
	cfg := &Config{
		Provider: "test",
		Providers: []ProviderConfig{
			{Name: "test", Model: "gpt-4", APIKey: "sk-test"},
		},
	}

	_, err := Resolve(cfg, CLIFlags{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no type set")
}
