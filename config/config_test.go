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
	assert.Equal(t, DefaultMaxIterations, *cfg.MaxIterations)
	assert.Empty(t, cfg.Provider)
	assert.Empty(t, cfg.Providers)
}

func TestLoadFrom_NonExistent(t *testing.T) {
	cfg, err := LoadFrom("/tmp/tachi-test-nonexistent/config.yaml")
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxTokens, cfg.MaxTokens)
	assert.Equal(t, DefaultMaxIterations, *cfg.MaxIterations)
}

func TestLoadFrom_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `provider: my-provider
max_tokens: 8000
max_iterations: 5
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
	assert.Equal(t, 5, *cfg.MaxIterations)
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
	assert.Equal(t, DefaultMaxIterations, *cfg.MaxIterations)
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	original := &Config{
		Provider:      "test-provider",
		MaxTokens:     16000,
		MaxIterations: intPtr(20),
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
	assert.Len(t, loaded.Providers, 1)
	assert.Equal(t, original.Providers[0], loaded.Providers[0])
}

func TestInputHistoryPath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	p, err := InputHistoryPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".tachi", "input_history"), p)
}

func TestTUIInputHistoryLimit(t *testing.T) {
	// Default via defaults.Set
	assert.Equal(t, 10, DefaultConfig().TUI.InputHistoryLimit)

	// Explicitly set
	assert.Equal(t, 5, (&Config{TUI: TUIConfig{InputHistoryLimit: 5}}).TUI.InputHistoryLimit)

	// Zero struct (no defaults applied)
	var c Config
	assert.Equal(t, 0, c.TUI.InputHistoryLimit)
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
		Provider:      "my-provider",
		MaxTokens:     8000,
		MaxIterations: intPtr(5),
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

	resolved, err := Resolve(cfg)
	require.NoError(t, err)
	assert.Equal(t, "anthropic", resolved.Provider.Type)
	assert.Equal(t, "claude-3", resolved.Provider.Model)
	assert.Equal(t, "https://api.example.com", resolved.Provider.BaseURL)
	assert.Equal(t, "sk-from-config", resolved.Provider.APIKey)
	assert.Equal(t, 8000, resolved.MaxTokens)
	assert.Equal(t, 5, resolved.MaxIterations)
}

func TestResolve_EnvOverridesAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	cfg := &Config{
		Provider: "test",
		Providers: []ProviderConfig{
			{Name: "test", Type: "anthropic", Model: "claude-3", APIKey: "sk-from-config"},
		},
	}

	resolved, err := Resolve(cfg)
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

	_, err := Resolve(cfg)
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

	_, err := Resolve(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolve_NoProviderConfigured(t *testing.T) {
	cfg := DefaultConfig()
	_, err := Resolve(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no provider configured")
}

func TestResolve_SingleProviderAutoSelect(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := &Config{
		MaxTokens:     DefaultMaxTokens,
		MaxIterations: intPtr(DefaultMaxIterations),
		Providers: []ProviderConfig{
			{Name: "only-one", Type: "openai", Model: "gpt-4", APIKey: "sk-test"},
		},
	}

	resolved, err := Resolve(cfg)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4", resolved.Provider.Model)
}

func TestResolve_ConfigSelectsProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := &Config{
		Provider:      "beta",
		MaxTokens:     DefaultMaxTokens,
		MaxIterations: intPtr(DefaultMaxIterations),
		Providers: []ProviderConfig{
			{Name: "alpha", Type: "openai", Model: "gpt-4", APIKey: "sk-a"},
			{Name: "beta", Type: "anthropic", Model: "claude-3", APIKey: "sk-b"},
		},
	}

	resolved, err := Resolve(cfg)
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

	_, err := Resolve(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no type set")
}

func TestIterationWarningThreshold(t *testing.T) {
	// Default via defaults.Set
	assert.Equal(t, 5, DefaultConfig().SystemReminder.IterationWarningThreshold)

	// Explicitly set
	assert.Equal(t, 3, (&Config{SystemReminder: SystemReminderConfig{IterationWarningThreshold: 3}}).SystemReminder.IterationWarningThreshold)

	// Zero struct (no defaults applied)
	var c Config
	assert.Equal(t, 0, c.SystemReminder.IterationWarningThreshold)
}

func TestTokenWarningThresholdPct(t *testing.T) {
	// Default via defaults.Set
	assert.Equal(t, 80, DefaultConfig().SystemReminder.TokenWarningThresholdPct)

	// Explicitly set
	assert.Equal(t, 90, (&Config{SystemReminder: SystemReminderConfig{TokenWarningThresholdPct: 90}}).SystemReminder.TokenWarningThresholdPct)

	// Zero struct (no defaults applied)
	var c Config
	assert.Equal(t, 0, c.SystemReminder.TokenWarningThresholdPct)
}

func TestEffectiveLanguage(t *testing.T) {
	// Default via defaults.Set
	assert.Equal(t, "English", DefaultConfig().Language)

	// Explicitly set
	assert.Equal(t, "Chinese", (&Config{Language: "Chinese"}).Language)

	// Zero struct (no defaults applied)
	var c Config
	assert.Equal(t, "", c.Language)
}

// --- MCP Profile expansion tests ---

func TestExpandMCPProfiles_NoProfile(t *testing.T) {
	cfg := &Config{
		MCPServers: []MCPServerConfig{
			{Name: "always-on", Type: MCPTransportHTTP, URL: "https://always.example.com"},
		},
	}
	err := cfg.ExpandMCPProfiles()
	require.NoError(t, err)
	assert.Len(t, cfg.MCPServers, 1)
	assert.Equal(t, "always-on", cfg.MCPServers[0].Name)
	assert.Empty(t, cfg.MCPServers[0].Profile)
}

func TestExpandMCPProfiles_ActiveProfile(t *testing.T) {
	cfg := &Config{
		MCPServers: []MCPServerConfig{
			{Name: "always-on", Type: MCPTransportHTTP, URL: "https://always.example.com"},
		},
		MCPProfiles: map[string][]MCPServerConfig{
			"test": {
				{Name: "platform-svc", Type: MCPTransportHTTP, URL: "https://test.example.com/mcp"},
			},
		},
		ActiveMCPProfile: "test",
	}
	err := cfg.ExpandMCPProfiles()
	require.NoError(t, err)
	assert.Len(t, cfg.MCPServers, 2)

	// always-on stays untouched
	assert.Equal(t, "always-on", cfg.MCPServers[0].Name)
	assert.Empty(t, cfg.MCPServers[0].Profile)

	// profile server stamped correctly
	assert.Equal(t, "platform-svc", cfg.MCPServers[1].Name)
	assert.Equal(t, "test", cfg.MCPServers[1].Profile)
	assert.Equal(t, "https://test.example.com/mcp", cfg.MCPServers[1].URL)
}

func TestExpandMCPProfiles_MultipleInProfile(t *testing.T) {
	cfg := &Config{
		MCPProfiles: map[string][]MCPServerConfig{
			"uat": {
				{Name: "svc-a", Type: MCPTransportHTTP, URL: "https://uat.example.com/a"},
				{Name: "svc-b", Type: MCPTransportHTTP, URL: "https://uat.example.com/b"},
			},
		},
		ActiveMCPProfile: "uat",
	}
	err := cfg.ExpandMCPProfiles()
	require.NoError(t, err)
	assert.Len(t, cfg.MCPServers, 2)
	assert.Equal(t, "svc-a", cfg.MCPServers[0].Name)
	assert.Equal(t, "uat", cfg.MCPServers[0].Profile)
	assert.Equal(t, "svc-b", cfg.MCPServers[1].Name)
	assert.Equal(t, "uat", cfg.MCPServers[1].Profile)
}

func TestExpandMCPProfiles_DefaultsApplied(t *testing.T) {
	cfg := &Config{
		MCPProfiles: map[string][]MCPServerConfig{
			"prod": {
				{Name: "svc", Type: MCPTransportHTTP, URL: "https://prod.example.com"},
			},
		},
		ActiveMCPProfile: "prod",
	}
	err := cfg.ExpandMCPProfiles()
	require.NoError(t, err)

	// Enabled defaults to true
	assert.True(t, cfg.MCPServers[0].IsEnabled())
}

func TestExpandMCPProfiles_MissingProfile(t *testing.T) {
	cfg := &Config{
		ActiveMCPProfile: "nonexistent",
	}
	err := cfg.ExpandMCPProfiles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in mcp_profiles")
}

func TestExpandMCPProfiles_ConflictWithMCPservers(t *testing.T) {
	cfg := &Config{
		MCPServers: []MCPServerConfig{
			{Name: "shared-name", Type: MCPTransportHTTP, URL: "https://always.example.com"},
		},
		MCPProfiles: map[string][]MCPServerConfig{
			"test": {
				{Name: "shared-name", Type: MCPTransportHTTP, URL: "https://test.example.com"},
			},
		},
		ActiveMCPProfile: "test",
	}
	err := cfg.ExpandMCPProfiles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name conflict")
	assert.Contains(t, err.Error(), "shared-name")
}

func TestExpandMCPProfiles_DuplicateInProfile(t *testing.T) {
	cfg := &Config{
		MCPProfiles: map[string][]MCPServerConfig{
			"test": {
				{Name: "dup", Type: MCPTransportHTTP, URL: "https://a.example.com"},
				{Name: "dup", Type: MCPTransportHTTP, URL: "https://b.example.com"},
			},
		},
		ActiveMCPProfile: "test",
	}
	err := cfg.ExpandMCPProfiles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate name")
	assert.Contains(t, err.Error(), "dup")
}

func TestExpandMCPProfiles_EmptyName(t *testing.T) {
	cfg := &Config{
		MCPProfiles: map[string][]MCPServerConfig{
			"test": {
				{Type: MCPTransportHTTP, URL: "https://no-name.example.com"},
			},
		},
		ActiveMCPProfile: "test",
	}
	err := cfg.ExpandMCPProfiles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no name")
}

func TestExpandMCPProfiles_ProfileNotSerialized(t *testing.T) {
	// Verify the Profile field does not appear in YAML output for individual servers.
	cfg := &Config{
		MCPServers: []MCPServerConfig{
			{Name: "svc", Type: MCPTransportHTTP, URL: "https://example.com", Profile: "test"},
		},
	}
	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	// The Profile field should be yaml:"-" and not appear under mcp_servers items.
	// Use a more targeted check than "profile" (which would match mcp_profiles key).
	assert.NotContains(t, string(data), "profile: test")
}

func TestTokenStorageName(t *testing.T) {
	// Stdio server: uses server name
	srv := &MCPServerConfig{Name: "test-mcp", Type: MCPTransportStdio}
	assert.Equal(t, "test-mcp", srv.TokenStorageName())

	// HTTP server without URL: falls back to server name
	srv2 := &MCPServerConfig{Name: "no-url-http", Type: MCPTransportHTTP}
	assert.Equal(t, "no-url-http", srv2.TokenStorageName())

	// HTTP server with URL: uses host
	srv3 := &MCPServerConfig{Name: "svc-a", Type: MCPTransportHTTP, URL: "https://gateway.internal.com/svc-a/mcp"}
	assert.Equal(t, "gateway.internal.com", srv3.TokenStorageName())

	// HTTP server with URL and port: sanitizes colon
	srv4 := &MCPServerConfig{Name: "svc-b", Type: MCPTransportHTTP, URL: "https://gateway.internal.com:8443/svc-b"}
	assert.Equal(t, "gateway.internal.com_8443", srv4.TokenStorageName())

	// Same host, different paths → same token storage key (the core feature)
	srv5 := &MCPServerConfig{Name: "svc-x", Type: MCPTransportHTTP, URL: "https://gateway.internal.com/svc-x/mcp"}
	srv6 := &MCPServerConfig{Name: "svc-y", Type: MCPTransportHTTP, URL: "https://gateway.internal.com/svc-y/mcp"}
	assert.Equal(t, srv5.TokenStorageName(), srv6.TokenStorageName())

	// Profile server (HTTP): host + profile suffix
	srv7 := &MCPServerConfig{Name: "platform-svc", Type: MCPTransportHTTP, URL: "https://test.example.com/mcp", Profile: "test"}
	assert.Equal(t, "test.example.com_test", srv7.TokenStorageName())

	// Profile server (stdio): name + profile suffix
	srv8 := &MCPServerConfig{Name: "platform-svc", Type: MCPTransportStdio, Profile: "prod"}
	assert.Equal(t, "platform-svc_prod", srv8.TokenStorageName())
}

func TestLoadFrom_WithProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yamlContent := `provider: test
providers:
  - name: test
    type: openai
    model: gpt-4
    api_key: sk-test
mcp_servers:
  - name: always-on
    type: http
    url: https://always.example.com
mcp_profiles:
  test:
    - name: platform-svc
      type: http
      url: https://test.example.com/mcp
active_mcp_profile: test
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0600))

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	assert.Len(t, cfg.MCPServers, 2)
	assert.Equal(t, "always-on", cfg.MCPServers[0].Name)
	assert.Empty(t, cfg.MCPServers[0].Profile)
	assert.Equal(t, "platform-svc", cfg.MCPServers[1].Name)
	assert.Equal(t, "test", cfg.MCPServers[1].Profile)
	assert.Equal(t, "test", cfg.ActiveMCPProfile)
}

// --- ActiveChannels tests ---

func TestActiveChannels_LegacyWeixinOnly(t *testing.T) {
	cc := &ChannelConfig{
		Weixin: WeixinConfig{
			Enabled:  true,
			StateDir: "/tmp/wx",
			Greeting: "hi",
		},
	}

	active := cc.ActiveChannels()
	assert.Len(t, active, 1)
	wx, ok := active["weixin"]
	require.True(t, ok)
	assert.Equal(t, true, wx["enabled"])
	assert.Equal(t, "/tmp/wx", wx["state_dir"])
	assert.Equal(t, "hi", wx["greeting"])
}

func TestActiveChannels_LegacyWeixinDisabled(t *testing.T) {
	cc := &ChannelConfig{
		Weixin: WeixinConfig{Enabled: false},
	}

	active := cc.ActiveChannels()
	assert.Empty(t, active)
}

func TestActiveChannels_GenericChannels(t *testing.T) {
	cc := &ChannelConfig{
		Channels: map[string]map[string]any{
			"mybot": {"enabled": true, "token": "abc"},
		},
	}

	active := cc.ActiveChannels()
	assert.Len(t, active, 1)
	mb, ok := active["mybot"]
	require.True(t, ok)
	assert.Equal(t, "abc", mb["token"])
}

func TestActiveChannels_GenericDisabled(t *testing.T) {
	cc := &ChannelConfig{
		Channels: map[string]map[string]any{
			"mybot": {"enabled": false},
		},
	}

	active := cc.ActiveChannels()
	assert.Empty(t, active)
}

func TestActiveChannels_GenericMissingEnabled(t *testing.T) {
	// If no "enabled" key, treat as disabled (defensive default).
	cc := &ChannelConfig{
		Channels: map[string]map[string]any{
			"mybot": {"token": "abc"},
		},
	}

	active := cc.ActiveChannels()
	assert.Empty(t, active)
}

func TestActiveChannels_LegacyOverridesGeneric(t *testing.T) {
	// When both generic and legacy weixin are set, generic takes precedence.
	cc := &ChannelConfig{
		Weixin: WeixinConfig{Enabled: true, StateDir: "/legacy"},
		Channels: map[string]map[string]any{
			"weixin": {"enabled": true, "state_dir": "/generic"},
		},
	}

	active := cc.ActiveChannels()
	assert.Len(t, active, 1)
	wx, ok := active["weixin"]
	require.True(t, ok)
	assert.Equal(t, "/generic", wx["state_dir"], "generic should take precedence over legacy")
}

func TestActiveChannels_Mixed(t *testing.T) {
	// Legacy weixin + generic other channel.
	cc := &ChannelConfig{
		Weixin: WeixinConfig{Enabled: true, StateDir: "/wx"},
		Channels: map[string]map[string]any{
			"mybot": {"enabled": true, "token": "x"},
		},
	}

	active := cc.ActiveChannels()
	assert.Len(t, active, 2)
	assert.Contains(t, active, "weixin")
	assert.Contains(t, active, "mybot")
}

func TestActiveChannels_Empty(t *testing.T) {
	cc := &ChannelConfig{}
	active := cc.ActiveChannels()
	assert.Empty(t, active)
}

// --- toBool tests ---

func TestToBool(t *testing.T) {
	assert.True(t, toBool(true))
	assert.False(t, toBool(false))
	assert.True(t, toBool(1))
	assert.False(t, toBool(0))
	assert.True(t, toBool(1.0))
	assert.False(t, toBool(0.0))
	assert.False(t, toBool("yes"))  // not a bool-like string
	assert.False(t, toBool(nil))
}
