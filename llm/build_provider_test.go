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

// TestProviderNotFoundMessage: the not-found error answers both questions the
// reader actually has — which name was looked up and what IS defined (with the
// file that defines it). A bare "provider not found" leaves a startup failure
// with no next step, which is what config.yaml's `provider:` naming an entry
// that no provider file defines produces.
//
// The name reported is the one the lookup COMPARED, not the argument: an alias
// and the empty-"default" form would both be misleading verbatim.
func TestProviderNotFoundMessage(t *testing.T) {
	cfg := testConfig(t) // one provider named "deepseek"
	cfg.Provider = "goat-deepseek-v4.1-flash"
	cfg.Providers = []config.ProviderConfig{
		{Name: "deepseek-flash", SourceFile: "/home/u/.tachi/provider.deepseek.json"},
		{Name: "fresh-from-the-page"}, // added in the settings page: no file yet
	}
	cfg.ProviderAliases = map[string]string{"fast": "goat-deepseek-v4.1-flash"}

	_, err := BuildProvider(cfg, "")
	require.ErrorIs(t, err, ErrProviderNotFound)
	msg := err.Error()
	assert.Contains(t, msg, `"goat-deepseek-v4.1-flash"`, "报错要说它找的是哪个名字")
	assert.Contains(t, msg, "deepseek-flash (provider.deepseek.json)", "要列出现有的名字和它们所在的文件")
	assert.Contains(t, msg, "fresh-from-the-page", "内存里新加的条目也要出现（它还没有文件）")

	// An alias is reported under the name it resolved to.
	_, err = BuildProvider(cfg, "fast")
	require.ErrorIs(t, err, ErrProviderNotFound)
	assert.Contains(t, err.Error(), `"goat-deepseek-v4.1-flash"`)

	// Nothing defined at all: the fix is a file, so say where.
	_, err = BuildProvider(&config.Config{}, "whatever")
	require.ErrorIs(t, err, ErrProviderNotFound)
	assert.Contains(t, err.Error(), "provider.<name>.json")
}

// TestDefaultProvider_NoDefaultMessage: "nothing is defined" and "several are
// defined and none is chosen" are different problems with different fixes, and
// the error has to say which one the user is in.
func TestDefaultProvider_NoDefaultMessage(t *testing.T) {
	_, err := DefaultProvider(&config.Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider.<name>.json")

	cfg := testConfig(t)
	cfg.Provider = ""
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{Name: "second"})
	_, err = DefaultProvider(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "`provider`", "要告诉用户去 config.yaml 挑一个")
	assert.Contains(t, err.Error(), "deepseek", "要把候选列出来")
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
					Timeout:    config.Duration(90 * time.Second),
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

// TestBuildProvider_InvalidProxy: an unusable spec.proxy is refused when the
// provider is RESOLVED, not silently ignored. Falling back to a direct
// connection would look like the proxy works (until the request fails as a
// confusing network error), and the settings page saves exactly this path, so
// the typo surfaces while the user is still editing the field.
func TestBuildProvider_InvalidProxy(t *testing.T) {
	cases := []struct {
		name  string
		proxy string
		ok    bool
	}{
		{name: "no-scheme", proxy: "127.0.0.1:7890"},         // the plausible typo
		{name: "unsupported-scheme", proxy: "ftp://host:21"}, // http/https/socks5 only
		{name: "http", proxy: "http://127.0.0.1:7890", ok: true},
		{name: "https", proxy: "https://proxy.example:8443", ok: true},
		{name: "socks5", proxy: "socks5://127.0.0.1:1080", ok: true},
		{name: "unset", proxy: "", ok: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Providers[0].Spec.Proxy = c.proxy
			resolved, err := BuildProvider(cfg, "deepseek")
			if c.ok {
				require.NoError(t, err)
				require.NotNil(t, resolved)
				return
			}
			assert.Nil(t, resolved)
			require.Error(t, err)
			// The message names the provider and the offending value, so a
			// config with several providers says which one to fix.
			assert.Contains(t, err.Error(), `provider "deepseek"`)
			assert.Contains(t, err.Error(), "proxy")
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
