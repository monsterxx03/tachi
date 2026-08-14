package llm

import (
	"errors"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Providers = []config.ProviderConfig{
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
	resolved, err := BuildProvider(cfg, "")
	assert.NoError(t, err, "empty name should resolve the default provider")
	assert.NotNil(t, resolved.Provider)
	assert.Equal(t, "deepseek", resolved.Name)

	// Unknown names still fail.
	resolved, err = BuildProvider(cfg, "nonexistent")
	assert.Nil(t, resolved)
	assert.ErrorIs(t, err, ErrProviderNotFound, "unknown name should map to ErrProviderNotFound")
}

// TestBuildProvider_NoDefault: with no default configured at all, the empty
// name surfaces ErrProviderNotFound (same as an unknown name).
func TestBuildProvider_NoDefault(t *testing.T) {
	cfg := &config.Config{} // no Provider field, no Providers
	resolved, err := BuildProvider(cfg, "")
	assert.Nil(t, resolved)
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

// TestBuildProvider_OK verifies a configured name resolves into a provider
// with the resolved fields attached (ContextWindow etc. available to callers
// without re-resolving).
func TestBuildProvider_OK(t *testing.T) {
	cfg := testConfig(t)

	resolved, err := BuildProvider(cfg, "deepseek")
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
	resolved, err := BuildProvider(cfg, "deepseek")
	assert.Nil(t, resolved)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrProviderNotFound), "missing API key is not a not-found error")
}

// TestBuildProvider_SpecOptions verifies spec.max_retries / spec.timeout are
// passed through to the constructed provider:
//   - openai path → the outer RetryProvider honors the configured retry count
//     (configured value, explicit 0 = disabled, unset = legacy default 2);
//   - anthropic / openai-res paths construct successfully (options flow into
//     the SDK client, which is not introspectable from here).
func TestBuildProvider_SpecOptions(t *testing.T) {
	three := 3
	zero := 0
	cases := []struct {
		name    string
		typ     string
		specMax *int // spec.max_retries 配置值（nil = 不配置）
		wantMax int  // openai 路径期望的 RetryProvider.MaxRetries
	}{
		{name: "openai-configured", typ: "openai", specMax: &three, wantMax: 3},
		{name: "openai-disabled", typ: "openai", specMax: &zero, wantMax: 0},
		{name: "openai-unset", typ: "openai", wantMax: 2}, // legacy default
		{name: "anthropic-configured", typ: "anthropic", specMax: &three},
		{name: "openai-res-configured", typ: "openai-res", specMax: &three},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Providers[0] = config.ProviderConfig{
				Name:    "p",
				Type:    c.typ,
				Model:   "some-model",
				APIKey:  "sk-test",
				BaseURL: "https://api.example.com/v1",
				Spec: config.ModelSpec{
					MaxRetries: c.specMax,
					Timeout:    90 * time.Second,
				},
			}
			resolved, err := BuildProvider(cfg, "p")
			require.NoError(t, err)
			require.NotNil(t, resolved.Provider)

			if c.typ == "openai" {
				rp, ok := resolved.Provider.(*RetryProvider)
				require.True(t, ok, "openai provider should be wrapped in RetryProvider")
				assert.Equal(t, c.wantMax, rp.Cfg().MaxRetries)
			}
		})
	}
}

// TestBuildProvider_SpecOptionsDefault: without spec options the openai path
// keeps the legacy default of 2 retries.
func TestBuildProvider_SpecOptionsDefault(t *testing.T) {
	cfg := testConfig(t)
	resolved, err := BuildProvider(cfg, "deepseek")
	require.NoError(t, err)
	rp, ok := resolved.Provider.(*RetryProvider)
	require.True(t, ok, "openai provider should be wrapped in RetryProvider")
	assert.Equal(t, 2, rp.Cfg().MaxRetries)
}
