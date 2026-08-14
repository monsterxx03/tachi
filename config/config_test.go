package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		MaxIterations: new(20),
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

func TestProviderConfig(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "alpha", Type: "openai"},
			{Name: "beta", Type: "anthropic"},
		},
	}

	p := cfg.ProviderConfig("alpha")
	require.NotNil(t, p)
	assert.Equal(t, "openai", p.Type)

	p = cfg.ProviderConfig("beta")
	require.NotNil(t, p)
	assert.Equal(t, "anthropic", p.Type)

	assert.Nil(t, cfg.ProviderConfig("gamma"))
}

func TestProviderConfig_Alias(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "gpt-4o-mini", Type: "openai", Model: "gpt-4o-mini"},
			{Name: "claude-sonnet", Type: "anthropic", Model: "claude-sonnet-4-20250514"},
		},
		ProviderAliases: map[string]string{
			"fast": "gpt-4o-mini",
			"main": "claude-sonnet",
		},
	}

	// Alias resolves to actual provider.
	p := cfg.ProviderConfig("fast")
	require.NotNil(t, p)
	assert.Equal(t, "openai", p.Type)
	assert.Equal(t, "gpt-4o-mini", p.Model)

	p = cfg.ProviderConfig("main")
	require.NotNil(t, p)
	assert.Equal(t, "anthropic", p.Type)
	assert.Equal(t, "claude-sonnet-4-20250514", p.Model)

	// Direct provider name still works.
	p = cfg.ProviderConfig("gpt-4o-mini")
	require.NotNil(t, p)
	assert.Equal(t, "openai", p.Type)

	// Alias wins when both alias and provider name match.
	// The alias target must exist in the providers list.
	cfg2 := &Config{
		Providers: []ProviderConfig{
			{Name: "fast", Type: "anthropic", Model: "claude-sonnet"},
			{Name: "gpt-4o-mini", Type: "openai", Model: "gpt-4o-mini"},
		},
		ProviderAliases: map[string]string{
			"fast": "gpt-4o-mini",
		},
	}
	p = cfg2.ProviderConfig("fast")
	require.NotNil(t, p)
	assert.Equal(t, "openai", p.Type) // alias resolved to gpt-4o-mini

	// Unknown alias returns nil.
	assert.Nil(t, cfg.ProviderConfig("nonexistent"))

	// Nil alias map still works.
	cfg3 := &Config{
		Providers: []ProviderConfig{
			{Name: "alpha", Type: "openai"},
		},
	}
	p = cfg3.ProviderConfig("alpha")
	require.NotNil(t, p)
	assert.Equal(t, "openai", p.Type)
	assert.Nil(t, cfg3.ProviderConfig("beta"))
}

func TestProviderConfig_Alias_Chain(t *testing.T) {
	// Chained aliases are not supported: a → b → actual
	// Only one level of alias resolution is performed.
	cfg := &Config{
		Providers: []ProviderConfig{
			{Name: "actual", Type: "openai", Model: "gpt-4o"},
		},
		ProviderAliases: map[string]string{
			"a": "b",
			"b": "c",
			"c": "actual",
		},
	}
	// "a" resolves to "b", but "b" is not a provider name — returns nil.
	assert.Nil(t, cfg.ProviderConfig("a"))
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

// --- MCP JSON config tests ---

func TestLoadMCPConfig_NoFiles(t *testing.T) {
	// When no JSON files exist, returns nil, nil.
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	SetBaseDir(t.TempDir())

	servers, err := LoadMCPConfig("", t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, servers)
}

func TestLoadMCPConfig_GlobalOnly(t *testing.T) {
	// Set up a fake global base dir with mcp.json.
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "global-svc", Type: MCPTransportHTTP, URL: "https://global.example.com"},
	})

	// workDir = "" → skip project-level
	servers, err := LoadMCPConfig("", "")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "global-svc", servers[0].Name)
	assert.Empty(t, servers[0].Profile)
	assert.True(t, servers[0].IsEnabled()) // defaults applied
}

func TestLoadMCPConfig_ProjectOverridesGlobal(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "shared-svc", Type: MCPTransportHTTP, URL: "https://global.example.com"},
		{Name: "global-only", Type: MCPTransportStdio, Command: "/bin/global"},
	})

	projectDir := t.TempDir()
	writeMCPJSON(t, filepath.Join(projectDir, ".tachi", mcpConfigFileName), []MCPServerConfig{
		{Name: "shared-svc", Type: MCPTransportHTTP, URL: "https://project.example.com"},
		{Name: "project-only", Type: MCPTransportStdio, Command: "/bin/project"},
	})

	servers, err := LoadMCPConfig("", projectDir)
	require.NoError(t, err)
	require.Len(t, servers, 3)

	// shared-svc: project overrides global
	assert.Equal(t, "https://project.example.com", servers[0].URL)
	// global-only: preserved
	assert.Equal(t, "global-only", servers[1].Name)
	// project-only: appended
	assert.Equal(t, "project-only", servers[2].Name)
}

func TestLoadMCPConfig_WithProfile(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "always-on", Type: MCPTransportHTTP, URL: "https://always.example.com"},
	})
	writeMCPJSON(t, filepath.Join(globalDir, mcpProfileFileName("prod")), []MCPServerConfig{
		{Name: "platform-svc", Type: MCPTransportHTTP, URL: "https://prod.example.com/mcp"},
	})

	servers, err := LoadMCPConfig("prod", "")
	require.NoError(t, err)
	require.Len(t, servers, 2)

	// always-on: base, no profile
	assert.Equal(t, "always-on", servers[0].Name)
	assert.Empty(t, servers[0].Profile)

	// platform-svc: from profile, stamped
	assert.Equal(t, "platform-svc", servers[1].Name)
	assert.Equal(t, "prod", servers[1].Profile)
	assert.Equal(t, "https://prod.example.com/mcp", servers[1].URL)
}

func TestLoadMCPConfig_ProfileOverrideBase(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "platform-svc", Type: MCPTransportHTTP, URL: "https://default.example.com"},
	})
	writeMCPJSON(t, filepath.Join(globalDir, mcpProfileFileName("prod")), []MCPServerConfig{
		{Name: "platform-svc", Type: MCPTransportHTTP, URL: "https://prod.example.com"},
	})

	servers, err := LoadMCPConfig("prod", "")
	require.NoError(t, err)
	require.Len(t, servers, 1)

	// Profile overrides base for same-named server
	assert.Equal(t, "https://prod.example.com", servers[0].URL)
	assert.Equal(t, "prod", servers[0].Profile)
}

func TestLoadMCPConfig_ProfileNotFound(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "base-only", Type: MCPTransportHTTP, URL: "https://base.example.com"},
	})
	// No mcp.prod.json — should NOT be an error.

	servers, err := LoadMCPConfig("prod", "")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "base-only", servers[0].Name)
	assert.Empty(t, servers[0].Profile)
}

func TestLoadMCPConfig_ProjectProfile(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "base-svc", Type: MCPTransportHTTP, URL: "https://base.example.com"},
	})
	writeMCPJSON(t, filepath.Join(globalDir, mcpProfileFileName("prod")), []MCPServerConfig{
		{Name: "platform-svc", Type: MCPTransportHTTP, URL: "https://global-prod.example.com"},
	})

	projectDir := t.TempDir()
	writeMCPJSON(t, filepath.Join(projectDir, ".tachi", mcpProfileFileName("prod")), []MCPServerConfig{
		{Name: "platform-svc", Type: MCPTransportHTTP, URL: "https://project-prod.example.com"},
	})

	servers, err := LoadMCPConfig("prod", projectDir)
	require.NoError(t, err)
	require.Len(t, servers, 2)

	// platform-svc: project profile overrides global profile
	assert.Equal(t, "platform-svc", servers[1].Name)
	assert.Equal(t, "prod", servers[1].Profile)
	assert.Equal(t, "https://project-prod.example.com", servers[1].URL)
}

func TestLoadMCPConfig_DuplicateInFile(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "dup", Type: MCPTransportHTTP, URL: "https://a.example.com"},
		{Name: "dup", Type: MCPTransportHTTP, URL: "https://b.example.com"},
	})

	_, err := LoadMCPConfig("", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate mcp server name")
}

func TestLoadMCPConfig_EmptyName(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Type: MCPTransportHTTP, URL: "https://no-name.example.com"},
	})

	_, err := LoadMCPConfig("", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no name")
}

func TestLoadMCPConfig_DefaultsApplied(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "svc", Type: MCPTransportHTTP, URL: "https://example.com"},
	})

	servers, err := LoadMCPConfig("", "")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.True(t, servers[0].IsEnabled()) // Enabled defaults to true
}

func TestLoadMCPServers_Integration(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "global-svc", Type: MCPTransportHTTP, URL: "https://global.example.com"},
	})

	cfg := &Config{}
	err := cfg.LoadMCPServers("") // no project dir
	require.NoError(t, err)
	require.Len(t, cfg.MCPServers, 1)
	assert.Equal(t, "global-svc", cfg.MCPServers[0].Name)
}

func TestLoadMCPServers_NoFilesEmpty(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)
	// No mcp.json files at all.

	cfg := &Config{}
	err := cfg.LoadMCPServers("")
	require.NoError(t, err)
	// MCPServers stays empty when no JSON files exist
	assert.Empty(t, cfg.MCPServers)
}

func TestLoadMCPServers_ReplacesExisting(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "json-svc", Type: MCPTransportHTTP, URL: "https://json.example.com"},
	})

	// MCPServers could have been set elsewhere (e.g. programmatically) —
	// JSON replaces it.
	cfg := &Config{
		MCPServers: []MCPServerConfig{
			{Name: "stale-svc", Type: MCPTransportHTTP, URL: "https://stale.example.com"},
		},
	}
	err := cfg.LoadMCPServers("")
	require.NoError(t, err)
	require.Len(t, cfg.MCPServers, 1)
	assert.Equal(t, "json-svc", cfg.MCPServers[0].Name)
}

// writeMCPJSON creates a directory if needed and writes an mcp.json file.
func writeMCPJSON(t *testing.T, path string, servers []MCPServerConfig) {
	t.Helper()
	dir := filepath.Dir(path)
	require.NoError(t, os.MkdirAll(dir, 0700))
	data, err := json.Marshal(mcpConfigFile{Servers: servers})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
}

func TestTokenStorageName(t *testing.T) {
	// Stdio server: uses server name
	srv := &MCPServerConfig{Name: "test-mcp", Type: MCPTransportStdio}
	assert.Equal(t, "test-mcp", srv.TokenStorageName())
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

	// mcp_servers in YAML is ignored (MCPServers is yaml:"-").
	// Only active_mcp_profile is read from YAML.
	yamlContent := `provider: test
providers:
  - name: test
    type: openai
    model: gpt-4
    api_key: sk-test
mcp_servers:
  - name: ignored-svc
    type: http
    url: https://ignored.example.com
active_mcp_profile: test
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0600))

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	// mcp_servers in YAML is NOT loaded — MCPServers is yaml:"-"
	assert.Empty(t, cfg.MCPServers)
	// active_mcp_profile is still read — used by LoadMCPServers() later
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
	assert.False(t, toBool("yes")) // not a bool-like string
	assert.False(t, toBool(nil))
}

// --- review.adversarial config behavior (docs/2026-07-30, §3) ---

// TestReviewAdversarial_UnconfiguredNil pins the creasty/defaults behavior
// the design doc calls out: Adversarial is a pointer WITHOUT a default tag,
// so defaults.Set() does not allocate it when YAML omits the key. Callers
// (SetupAdversarialProviders, CheckAdversarialProviders) must check the
// pointer for nil before touching Models/JudgeModel.
func TestReviewAdversarial_UnconfiguredNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("providers:\n  - name: p\n    type: openai\n    model: m\n    api_key: k\n"), 0600))

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	assert.Nil(t, cfg.Review.Adversarial, "unconfigured adversarial: must stay nil (defaults does not allocate default-less pointers)")
}

// TestReviewAdversarial_ExplicitKeyAllocates verifies that once the
// `adversarial:` key IS present, the pointer is allocated (even with only
// removed legacy keys like `enabled:`/`rounds:`) — models/judge_model stay
// empty and the config loads fine (yaml.Unmarshal is non-strict).
func TestReviewAdversarial_ExplicitKeyAllocates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("review:\n  adversarial:\n    enabled: true\n    rounds: 5\n"), 0600))

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Review.Adversarial)
	assert.Empty(t, cfg.Review.Adversarial.Models)
	assert.Empty(t, cfg.Review.Adversarial.JudgeModel)
}

// TestReviewAdversarial_FullConfigLoad verifies models/judge_model round-trip.
func TestReviewAdversarial_FullConfigLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `review:
  adversarial:
    models:
      - claude-sonnet
      - gpt-4o
    judge_model: claude-opus
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0600))

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Review.Adversarial)
	assert.Equal(t, []string{"claude-sonnet", "gpt-4o"}, cfg.Review.Adversarial.Models)
	assert.Equal(t, "claude-opus", cfg.Review.Adversarial.JudgeModel)
}

func TestModelSpec_YAMLNested(t *testing.T) {
	// 新结构：模型级属性聚合在 provider 的 spec: 子结构下。
	yamlData := []byte(`
provider: deepseek
providers:
  - name: deepseek-v4-flash
    type: openai
    model: deepseek-v4-flash
    base_url: https://api.deepseek.com/v1
    api_key: sk-test
    spec:
      context_window: 262144
      thinking_level: high
      max_retries: 3
      timeout: 90s
      pricing:
        input_price: 2.5
        output_price: 8.0
        cache_read_input_price: 0.1
        cache_creation_input_price: 1.0
`)
	cfg := &Config{}
	require.NoError(t, yaml.Unmarshal(yamlData, cfg))
	require.Len(t, cfg.Providers, 1)

	p := cfg.Providers[0]
	require.NotNil(t, p.Spec.ContextWindow)
	assert.Equal(t, int64(262144), *p.Spec.ContextWindow)
	require.NotNil(t, p.Spec.Pricing)
	require.NotNil(t, p.Spec.Pricing.InputPrice)
	assert.Equal(t, 2.5, *p.Spec.Pricing.InputPrice)
	require.NotNil(t, p.Spec.Pricing.OutputPrice)
	assert.Equal(t, 8.0, *p.Spec.Pricing.OutputPrice)
	require.NotNil(t, p.Spec.Pricing.CacheReadInputPrice)
	assert.Equal(t, 0.1, *p.Spec.Pricing.CacheReadInputPrice)
	require.NotNil(t, p.Spec.Pricing.CacheCreationInputPrice)
	assert.Equal(t, 1.0, *p.Spec.Pricing.CacheCreationInputPrice)
	assert.Equal(t, "high", p.Spec.ThinkingLevel)
	require.NotNil(t, p.Spec.MaxRetries)
	assert.Equal(t, 3, *p.Spec.MaxRetries)
	assert.Equal(t, 90*time.Second, p.Spec.Timeout)

	// 旧版平铺字段（context_window / input_price 直接挂 provider 下）不再解析。
	oldYAML := []byte(`
providers:
  - name: legacy
    type: openai
    model: gpt-4o
    api_key: sk-test
    context_window: 262144
    input_price: 2.5
    thinking_level: high
`)
	cfgOld := &Config{}
	require.NoError(t, yaml.Unmarshal(oldYAML, cfgOld))
	require.Len(t, cfgOld.Providers, 1)
	assert.Nil(t, cfgOld.Providers[0].Spec.ContextWindow, "旧版平铺 context_window 不应再被解析")
	assert.Nil(t, cfgOld.Providers[0].Spec.Pricing, "旧版平铺 pricing 不应再被解析")
	assert.Equal(t, "", cfgOld.Providers[0].Spec.ThinkingLevel, "旧版平铺 thinking_level 不应再被解析")

	// Round-trip：嵌套 ModelSpec 序列化后能原样解析回来。
	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	cfg2 := &Config{}
	require.NoError(t, yaml.Unmarshal(out, cfg2))
	require.Len(t, cfg2.Providers, 1)
	assert.Equal(t, p, cfg2.Providers[0])
}

// TestDefaultProviderName_Alias: the main-provider name exit point must
// normalize provider_aliases itself — callers (session metadata, /usage
// grouping) consume the REAL config provider name without sprinkling
// resolveAlias at call sites.
func TestDefaultProviderName_Alias(t *testing.T) {
	cfg := &Config{
		Provider: "main_provider", // alias in the top-level provider field
		Providers: []ProviderConfig{
			{Name: "deepseek-v4-flash", Type: "openai", Model: "deepseek-chat"},
		},
		ProviderAliases: map[string]string{"main_provider": "deepseek-v4-flash"},
	}
	assert.Equal(t, "deepseek-v4-flash", cfg.DefaultProviderName())

	// No alias configured → field value passes through unchanged.
	plain := &Config{Provider: "deepseek-v4-flash"}
	assert.Equal(t, "deepseek-v4-flash", plain.DefaultProviderName())

	// Single-provider fallback is untouched by aliases.
	single := &Config{
		Providers:       []ProviderConfig{{Name: "deepseek-v4-flash"}},
		ProviderAliases: map[string]string{"fast": "deepseek-v4-flash"},
	}
	assert.Equal(t, "deepseek-v4-flash", single.DefaultProviderName())
}

// TestExpandProviderAliases: alias expansion happens ONCE at load
// (LoadFrom → ExpandProviderAliases) — every provider-reference field,
// top-level and nested, must hold the REAL provider name afterwards, so no
// call site outside config needs to know about aliases.
func TestExpandProviderAliases(t *testing.T) {
	cfg := &Config{
		Provider:       "main_provider",
		TitleProvider:  "fast",
		CommitProvider: "main_provider",
		RunProvider:    "fast",
		Subagent:       SubagentConfig{Provider: "main_provider"},
		Review: ReviewConfig{
			Provider: "fast",
			Adversarial: &AdversarialReviewConfig{
				Models:     []string{"fast", "main_provider"},
				JudgeModel: "fast",
			},
		},
		Memory:       MemoryConfig{KeywordProvider: "fast"},
		DeepResearch: DeepResearchConfig{QueryGeneratorProvider: "main_provider"},
		Dream:        DreamConfig{Provider: "fast"},
		Providers: []ProviderConfig{
			{Name: "deepseek-v4-flash", Type: "openai", Model: "deepseek-chat"},
			{Name: "deepseek-v4-pro", Type: "openai", Model: "deepseek-chat"},
		},
		ProviderAliases: map[string]string{
			"main_provider": "deepseek-v4-flash",
			"fast":          "deepseek-v4-pro",
		},
	}

	cfg.ExpandProviderAliases()

	assert.Equal(t, "deepseek-v4-flash", cfg.Provider)
	assert.Equal(t, "deepseek-v4-pro", cfg.TitleProvider)
	assert.Equal(t, "deepseek-v4-flash", cfg.CommitProvider)
	assert.Equal(t, "deepseek-v4-pro", cfg.RunProvider)
	assert.Equal(t, "deepseek-v4-flash", cfg.Subagent.Provider)
	assert.Equal(t, "deepseek-v4-pro", cfg.Review.Provider)
	if adv := cfg.Review.Adversarial; adv != nil {
		assert.Equal(t, []string{"deepseek-v4-pro", "deepseek-v4-flash"}, adv.Models)
		assert.Equal(t, "deepseek-v4-pro", adv.JudgeModel)
	}
	assert.Equal(t, "deepseek-v4-pro", cfg.Memory.KeywordProvider)
	assert.Equal(t, "deepseek-v4-flash", cfg.DeepResearch.QueryGeneratorProvider)
	assert.Equal(t, "deepseek-v4-pro", cfg.Dream.Provider)
	// The map is preserved — ProviderConfig and runtime inputs still need it.
	assert.Len(t, cfg.ProviderAliases, 2)

	// No aliases configured → no-op.
	plain := &Config{Provider: "deepseek-v4-flash"}
	plain.ExpandProviderAliases()
	assert.Equal(t, "deepseek-v4-flash", plain.Provider)
}
