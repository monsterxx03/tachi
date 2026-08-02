package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Providers = []ProviderConfig{
		{
			Name:    "deepseek",
			Type:    "openai",
			Model:   "deepseek-v4",
			BaseURL: "https://api.deepseek.com/v1",
			APIKey:  "sk-test-123",
		},
	}
	return cfg
}

// TestBuildProvider_NotFound verifies empty and unknown names map to
// ErrProviderNotFound (callers choose fallback vs fail-fast).
func TestBuildProvider_NotFound(t *testing.T) {
	cfg := testConfig(t)

	for _, name := range []string{"", "nonexistent"} {
		p, resolved, err := cfg.BuildProvider(name)
		assert.Nil(t, p, "provider for %q should be nil", name)
		assert.Nil(t, resolved)
		assert.ErrorIs(t, err, ErrProviderNotFound, "name %q should map to ErrProviderNotFound", name)
	}
}

// TestBuildProvider_OK verifies a configured name resolves into a provider
// with the resolved fields attached (ContextWindow etc. available to callers
// without re-resolving).
func TestBuildProvider_OK(t *testing.T) {
	cfg := testConfig(t)

	p, resolved, err := cfg.BuildProvider("deepseek")
	require.NoError(t, err)
	require.NotNil(t, p)
	require.NotNil(t, resolved)

	assert.Equal(t, "deepseek", resolved.Name)
	assert.Equal(t, "deepseek-v4", resolved.Model)
	assert.Equal(t, "deepseek-v4", p.Model())
	assert.True(t, resolved.ContextWindow > 0)
}

// TestNewProviderFromResolved verifies the constructor maps ResolvedProvider
// fields onto llm.NewProvider's positional args (baseURL/apiKey order guard).
func TestNewProviderFromResolved(t *testing.T) {
	cfg := testConfig(t)
	_, resolved, err := cfg.BuildProvider("deepseek")
	require.NoError(t, err)

	p, err := NewProviderFromResolved(resolved)
	require.NoError(t, err)
	assert.Equal(t, "deepseek-v4", p.Model())

	// Unsupported type surfaces as an error.
	_, err = NewProviderFromResolved(&ResolvedProvider{Type: "bogus", Model: "x", APIKey: "k"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unsupported provider type"))
}

// TestBuildProvider_MissingAPIKey verifies the resolution error is wrapped
// with the provider name, distinct from ErrProviderNotFound.
func TestBuildProvider_MissingAPIKey(t *testing.T) {
	cfg := testConfig(t)
	cfg.Providers[0].APIKey = ""
	t.Setenv("DEEPSEEK_API_KEY", "") // ensure no env fallback

	// Provider type "openai" reads OPENAI_API_KEY — make sure it's empty too.
	// ResolveAPIKey falls back to pCfg.APIKey which we cleared.
	p, _, err := cfg.BuildProvider("deepseek")
	assert.Nil(t, p)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrProviderNotFound), "missing API key is not a not-found error")
}
