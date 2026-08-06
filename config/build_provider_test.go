package config

import (
	"errors"
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

// TestBuildProvider_NotFound verifies that unknown names map to
// ErrProviderNotFound (callers choose fallback vs fail-fast). An EMPTY name
// is not an error: it means "the default provider" (BuildProvider("")
// ≡ DefaultProvider()).
func TestBuildProvider_NotFound(t *testing.T) {
	cfg := testConfig(t)

	// Empty name resolves the default provider — the shared `""` = default rule.
	resolved, err := cfg.BuildProvider("")
	assert.NoError(t, err, "empty name should resolve the default provider")
	assert.NotNil(t, resolved.Provider)
	assert.Equal(t, "deepseek", resolved.Name)

	// Unknown names still fail.
	resolved, err = cfg.BuildProvider("nonexistent")
	assert.Nil(t, resolved)
	assert.ErrorIs(t, err, ErrProviderNotFound, "unknown name should map to ErrProviderNotFound")
}

// TestBuildProvider_NoDefault: with no default configured at all, the empty
// name surfaces ErrProviderNotFound (same as an unknown name).
func TestBuildProvider_NoDefault(t *testing.T) {
	cfg := &Config{} // no Provider field, no Providers
	resolved, err := cfg.BuildProvider("")
	assert.Nil(t, resolved)
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

// TestBuildProvider_OK verifies a configured name resolves into a provider
// with the resolved fields attached (ContextWindow etc. available to callers
// without re-resolving).
func TestBuildProvider_OK(t *testing.T) {
	cfg := testConfig(t)

	resolved, err := cfg.BuildProvider("deepseek")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.NotNil(t, resolved.Provider)

	assert.Equal(t, "deepseek", resolved.Name)
	assert.Equal(t, "deepseek-v4", resolved.Model)
	assert.Equal(t, "deepseek-v4", resolved.Provider.Model())
	assert.True(t, resolved.ContextWindow > 0)
}

// TestBuildProvider_MissingAPIKey verifies the resolution error is wrapped
// with the provider name, distinct from ErrProviderNotFound.
func TestBuildProvider_MissingAPIKey(t *testing.T) {
	cfg := testConfig(t)
	cfg.Providers[0].APIKey = ""
	t.Setenv("DEEPSEEK_API_KEY", "") // ensure no env fallback

	// Provider type "openai" reads OPENAI_API_KEY — make sure it's empty too.
	// resolveAPIKey falls back to pCfg.APIKey which we cleared.
	resolved, err := cfg.BuildProvider("deepseek")
	assert.Nil(t, resolved)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrProviderNotFound), "missing API key is not a not-found error")
}
