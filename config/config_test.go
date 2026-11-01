package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/creasty/defaults"
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
	writeConfigAndProviders(t, dir, yaml)

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
	writeConfigAndProviders(t, dir, yaml)

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxTokens, cfg.MaxTokens)
	assert.Equal(t, DefaultMaxIterations, *cfg.MaxIterations)
	// The tag on JevConfig.Timeout, held against its constant: the two are the same value
	// written twice (a tag cannot name a constant), and this is what notices the day one of
	// them moves. A config that never mentions the block still gets the bound.
	assert.Equal(t, DefaultJevTimeout, cfg.Jev.Timeout)
	// The question cap has no `default:` tag — its answer depends on base_url —
	// so a config that never mentions the block must still resolve to the
	// reference service's number rather than to 0 questions per request.
	assert.Equal(t, 0, cfg.Jev.MaxQuestionsPerRequest, "the field stays unset, the resolution happens on read")
	assert.Equal(t, DefaultJevQuestionsPerRequest, cfg.Jev.QuestionsPerRequest())
	// The file gate's three leaves live two structs deep (permissions.files.*),
	// which is one level deeper than anything else defaults.Set reaches: without
	// this the gate could arrive with τ = 0 (every call asks) and a zero budget
	// (no judgment ever completes) on a config that never mentioned the block.
	// The threshold's tag is pinned against its constant for the same reason the
	// timeout's is — a tag cannot name one.
	assert.Equal(t, FilesJevAuto, cfg.Permissions.Files.Jev)
	assert.Equal(t, FilesJevThreshold, cfg.Permissions.Files.JevThreshold)
	assert.Equal(t, DefaultFilesJevBudget, cfg.Permissions.Files.JevBudget)
	// Same tag/constant duplication for cron.execution_timeout: a tag cannot name a
	// constant, so this line is what notices the day the tag or the constant moves.
	// The number decides whether a scheduled job finishes or is cut off mid-way.
	assert.Equal(t, DefaultCronExecutionTimeout, cfg.Cron.ExecutionTimeout)
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

	// The two halves of a real install: the preferences in config.yaml, the endpoint definitions in
	// provider.deepseek.json beside it. yaml.Marshal(original) would write both into one file, which
	// the loader now refuses on purpose — so this writes what an install actually looks like.
	providers := original.Providers
	original.Providers = nil
	data, err := yaml.Marshal(original)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	providerPath := writeProviderFixture(t, dir, providers)

	loaded, err := LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, original.Provider, loaded.Provider)
	assert.Equal(t, original.MaxTokens, loaded.MaxTokens)
	assert.Equal(t, original.MaxIterations, loaded.MaxIterations)
	assert.Len(t, loaded.Providers, 1)
	assert.Equal(t, providers[0].Name, loaded.Providers[0].Name)
	assert.Equal(t, providers[0].Model, loaded.Providers[0].Model)
	assert.Equal(t, providers[0].APIKey, loaded.Providers[0].APIKey)
	// The definition knows which file it came from: that is what a save writes back to.
	assert.Equal(t, providerPath, loaded.Providers[0].SourceFile)
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

	servers, err := LoadMCPConfig("")
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

	servers, err := LoadMCPConfig("")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "global-svc", servers[0].Name)
	assert.Empty(t, servers[0].Profile)
	assert.True(t, servers[0].IsEnabled()) // defaults applied
}

// TestLoadMCPConfig_IgnoresProjectScope pins the removed scope: a .tachi/mcp.json in a working
// tree is not read at all, from any tree.
func TestLoadMCPConfig_IgnoresProjectScope(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	tree := t.TempDir()
	t.Chdir(tree) // the tree the process thinks it is in

	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), []MCPServerConfig{
		{Name: "global-svc", Type: MCPTransportHTTP, URL: "https://global.example.com"},
	})
	writeMCPJSON(t, filepath.Join(tree, ".tachi", mcpConfigFileName), []MCPServerConfig{
		{Name: "project-svc", Type: MCPTransportStdio, Command: "/bin/project"},
	})

	servers, err := LoadMCPConfig("")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "global-svc", servers[0].Name)
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

	servers, err := LoadMCPConfig("prod")
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

	servers, err := LoadMCPConfig("prod")
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

	servers, err := LoadMCPConfig("prod")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "base-only", servers[0].Name)
	assert.Empty(t, servers[0].Profile)
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

	_, err := LoadMCPConfig("")
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

	_, err := LoadMCPConfig("")
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

	servers, err := LoadMCPConfig("")
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
	err := cfg.LoadMCPServers()
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
	err := cfg.LoadMCPServers()
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
	err := cfg.LoadMCPServers()
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

func TestListMCPProfiles(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	// Base file and non-mcp files must be excluded; profiles sorted.
	writeMCPJSON(t, filepath.Join(globalDir, mcpConfigFileName), nil)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "notes.json"), []byte("{}"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, mcpProfileFileName("prod")), []byte("{}"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, mcpProfileFileName("dev")), []byte("{}"), 0600))

	assert.Equal(t, []string{"dev", "prod"}, ListMCPProfiles())
}

func TestListMCPProfiles_None(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	SetBaseDir(t.TempDir())

	assert.Empty(t, ListMCPProfiles())
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
	writeConfigAndProviders(t, dir, yamlContent)

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

// The synthesized legacy map is the whole config everyone downstream sees, so the
// notify switch has to ride in it — otherwise `channel.weixin.notify: true` on the
// legacy path would be read by nobody and the NotifyUser tool would never mount.
func TestActiveChannels_LegacyWeixinCarriesNotifySwitch(t *testing.T) {
	on := &ChannelConfig{Weixin: WeixinConfig{Enabled: true, Notify: true}}
	assert.Equal(t, true, on.ActiveChannels()["weixin"]["notify"])

	off := &ChannelConfig{Weixin: WeixinConfig{Enabled: true}}
	assert.Equal(t, false, off.ActiveChannels()["weixin"]["notify"])
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
	writeConfigAndProviders(t, dir, "providers:\n  - name: p\n    type: openai\n    model: m\n    api_key: k\n")

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
	writeConfigAndProviders(t, dir, yaml)

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
      proxy: socks5://127.0.0.1:1080
      pricing:
        input_price: 2.5
        output_price: 8.0
        cache_read_input_price: 0.1
        cache_creation_input_price: 1.0
      credit_rate: 0.05
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
	assert.Equal(t, Duration(90*time.Second), p.Spec.Timeout)
	assert.Equal(t, "socks5://127.0.0.1:1080", p.Spec.Proxy)
	require.NotNil(t, p.Spec.CreditRate)
	assert.Equal(t, 0.05, *p.Spec.CreditRate)

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

func TestLoadFrom_ProviderVisionFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := `providers:
  - name: text-only
    type: openai
    model: deepseek-v4-flash
    api_key: sk-test
    spec:
      vision: false
  - name: explicit-vision
    type: openai
    model: my-private-vl
    api_key: sk-test
    spec:
      vision: true
  - name: auto-detect
    type: anthropic
    model: claude-sonnet-4-6
    api_key: sk-test
`
	writeConfigAndProviders(t, dir, yaml)

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Len(t, cfg.Providers, 3)

	assert.NotNil(t, cfg.Providers[0].Spec.Vision)
	assert.False(t, *cfg.Providers[0].Spec.Vision, "explicit vision: false loads as false")

	assert.NotNil(t, cfg.Providers[1].Spec.Vision)
	assert.True(t, *cfg.Providers[1].Spec.Vision, "explicit vision: true loads as true")

	assert.Nil(t, cfg.Providers[2].Spec.Vision, "omitted vision stays nil (auto-detect at resolve time)")
}

// --- writing a toggle back into a user-authored mcp.json ---------------------
//
// The desktop's MCP panel persists 启用/禁用 into the file the server came from, and these
// pin the two halves of that promise: the edit touches ONE value's bytes (everything else in
// a hand-written file survives, including fields this package does not model), and the file a
// server is written to is the one it came from — not "the active profile", which would shadow
// a base entry or lose the change on the next profile switch.

func TestSetServerEnabledInFileReplacesTheValueOnly(t *testing.T) {
	// Shaped like a real mcp.json: hand-written key order, a nested object, an array of args,
	// and a field (x-note) this package does not model.
	const original = `{
  "servers": [
    {
      "name": "chrome-devtools",
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@latest"],
      "x-note": "keep me",
      "enabled": true
    },
    {
      "name": "tapd",
      "type": "stdio",
      "command": "mcp-server-tapd",
      "env": {"TAPD_ACCESS_TOKEN": "secret"},
      "enabled": true
    }
  ]
}
`
	const want = `{
  "servers": [
    {
      "name": "chrome-devtools",
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@latest"],
      "x-note": "keep me",
      "enabled": false
    },
    {
      "name": "tapd",
      "type": "stdio",
      "command": "mcp-server-tapd",
      "env": {"TAPD_ACCESS_TOKEN": "secret"},
      "enabled": true
    }
  ]
}
`
	path := filepath.Join(t.TempDir(), mcpConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	require.NoError(t, SetServerEnabledInFile(path, "chrome-devtools", false))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(got), "only the one boolean may change")

	// And the change is what the loader reports afterwards.
	servers, err := loadMCPJSONFile(path)
	require.NoError(t, err)
	require.Len(t, servers, 2)
	assert.False(t, servers[0].IsEnabled())
	assert.True(t, servers[1].IsEnabled())
}

func TestSetServerEnabledInFileAppendsTheKeyWhenAbsent(t *testing.T) {
	// "enabled" missing means enabled (the defaults tag), so the write APPENDS the choice —
	// with the file's own indentation, and a comma only when the entry has other fields.
	const original = "{\n  \"servers\": [\n    {\n      \"name\": \"a\",\n      \"type\": \"stdio\"\n    },\n    {\n      \"name\": \"b\",\n      \"type\": \"stdio\"\n    }\n  ]\n}\n"
	const want = "{\n  \"servers\": [\n    {\n      \"name\": \"a\",\n      \"type\": \"stdio\",\n      \"enabled\": false\n    },\n    {\n      \"name\": \"b\",\n      \"type\": \"stdio\"\n    }\n  ]\n}\n"
	path := filepath.Join(t.TempDir(), mcpConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	require.NoError(t, SetServerEnabledInFile(path, "a", false))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

func TestSetServerEnabledInFileKeepsTabsAndMode(t *testing.T) {
	// A tab-indented file (the indentation is measured from the entry, not assumed) and a
	// mode the write must not change: it is the user's file, not one of ours.
	const original = "{\n\t\"servers\": [\n\t\t{\n\t\t\t\"name\": \"a\",\n\t\t\t\"type\": \"stdio\"\n\t\t}\n\t]\n}\n"
	const want = "{\n\t\"servers\": [\n\t\t{\n\t\t\t\"name\": \"a\",\n\t\t\t\"type\": \"stdio\",\n\t\t\t\"enabled\": true\n\t\t}\n\t]\n}\n"
	path := filepath.Join(t.TempDir(), mcpConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(original), 0o640))

	require.NoError(t, SetServerEnabledInFile(path, "a", true))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), st.Mode().Perm())
}

func TestSetServerEnabledInFileRefusesOtherFilesAndBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, mcpConfigFileName)
	require.NoError(t, os.WriteFile(path,
		[]byte("{\n  \"servers\": [\n    {\"name\": \"a\", \"enabled\": true}\n  ]\n}\n"), 0o644))

	// A name this file does not define: a normal outcome (it may live in another file), and
	// reported as such rather than as a parse error.
	err := SetServerEnabledInFile(path, "elsewhere", false)
	require.ErrorIs(t, err, ErrServerNotInFile)
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Contains(t, string(after), `"enabled": true`, "a refused write must not touch the file")

	// Broken JSON is refused too — and left alone for the user to repair.
	broken := filepath.Join(dir, "broken.json")
	require.NoError(t, os.WriteFile(broken, []byte(`{"servers": [ {`), 0o644))
	require.Error(t, SetServerEnabledInFile(broken, "a", false))
	afterBroken, readErr := os.ReadFile(broken)
	require.NoError(t, readErr)
	assert.Equal(t, `{"servers": [ {`, string(afterBroken))
}

// TestSourceFileIsTheFileThatWon pins the routing rule the write-back depends on: a server's
// SourceFile names the mcp.json it was READ from, with the winner of a name clash (profile over
// base) being the one that answers.
func TestSourceFileIsTheFileThatWon(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	globalBase := filepath.Join(globalDir, mcpConfigFileName)
	globalProfile := filepath.Join(globalDir, mcpProfileFileName("prod"))

	writeMCPJSON(t, globalBase, []MCPServerConfig{
		{Name: "base-only", Type: MCPTransportStdio, Command: "/bin/base"},
		{Name: "profile-only", Type: MCPTransportStdio, Command: "/bin/shadowed"},
	})
	writeMCPJSON(t, globalProfile, []MCPServerConfig{
		{Name: "profile-only", Type: MCPTransportStdio, Command: "/bin/profile"},
		{Name: "both", Type: MCPTransportStdio, Command: "/bin/profile-only-in-profile"},
	})

	servers, err := LoadMCPConfig("prod")
	require.NoError(t, err)
	byName := map[string]string{}
	for _, s := range servers {
		byName[s.Name] = s.SourceFile
	}
	assert.Equal(t, globalBase, byName["base-only"], "a base entry belongs to the base file")
	assert.Equal(t, globalProfile, byName["profile-only"], "the overriding file wins, not the base one")
	assert.Equal(t, globalProfile, byName["both"])
}

// TestToggleSurvivesAProfileRoundTrip is the whole point in one test: what the panel writes
// for a profile server is still there — and still scoped to that profile — after a reload,
// including a switch back to the default profile, where the server must NOT appear at all.
func TestToggleSurvivesAProfileRoundTrip(t *testing.T) {
	oldBase := BaseDir()
	defer SetBaseDir(oldBase)
	globalDir := t.TempDir()
	SetBaseDir(globalDir)

	basePath := filepath.Join(globalDir, mcpConfigFileName)
	profilePath := filepath.Join(globalDir, mcpProfileFileName("prod"))
	writeMCPJSON(t, basePath, []MCPServerConfig{{Name: "plain", Type: MCPTransportStdio, Command: "/bin/plain"}})
	writeMCPJSON(t, profilePath, []MCPServerConfig{{Name: "profiled", Type: MCPTransportStdio, Command: "/bin/profiled"}})

	// Turn OFF the profile server and ON the base one, each into its own file.
	require.NoError(t, SetServerEnabledInFile(profilePath, "profiled", false))
	require.NoError(t, SetServerEnabledInFile(basePath, "plain", true))

	// The base file must not have grown a "profiled" entry (that would shadow nothing today
	// and be lost the moment the profile changes).
	baseRaw, err := os.ReadFile(basePath)
	require.NoError(t, err)
	assert.NotContains(t, string(baseRaw), "profiled")

	servers, err := LoadMCPConfig("prod")
	require.NoError(t, err)
	require.Len(t, servers, 2)
	for _, s := range servers {
		switch s.Name {
		case "profiled":
			assert.False(t, s.IsEnabled(), "the profile server's toggle must survive")
			assert.Equal(t, profilePath, s.SourceFile)
		case "plain":
			assert.True(t, s.IsEnabled())
			assert.Equal(t, basePath, s.SourceFile)
		}
	}

	// Switching back to the default profile: that server is simply not part of it.
	servers, err = LoadMCPConfig("")
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "plain", servers[0].Name)
}

// 提示音开关的默认值：没设 = 开。和其它 nil 即默认的读法一致——一个没写这一行的配置
// 不该因此把提示音关掉。
func TestBadgeAlertsDefaultToOn(t *testing.T) {
	on, off := true, false

	assert.True(t, BadgeConfig{}.AlertsEnabled(), "未设置时应当默认开")
	assert.True(t, BadgeConfig{Alert: &on}.AlertsEnabled(), "显式 true 是开")
	assert.False(t, BadgeConfig{Alert: &off}.AlertsEnabled(), "显式 false 是关")
}

// The per-request question cap is a property of the ENDPOINT, so leaving it
// unset has to resolve from base_url — a known gateway takes its own measured
// cap, everything else the reference service's. Getting this wrong is a whole
// search degrading to the keyword path (an over-long request is an HTTP 400 the
// client does not retry), which is why the table is worth having at all.
func TestJevQuestionsPerRequestResolvesFromBaseURL(t *testing.T) {
	// The reference service, and anything we do not recognise.
	for _, base := range []string{"", "https://api.typesafe.ai", "https://gw.example.com/v1", "not a url at all"} {
		got := JevConfig{BaseURL: base}.QuestionsPerRequest()
		assert.Equal(t, DefaultJevQuestionsPerRequest, got, "base_url %q", base)
	}

	// A known gateway, with the scheme written or omitted — the path is the
	// deployment's business, the host is what identifies the service.
	for _, base := range []string{
		"https://api.commandcode.ai/provider",
		"https://api.commandcode.ai/provider/v1",
		"API.commandcode.ai/provider",
	} {
		assert.Equal(t, jevGatewayQuestionCaps["api.commandcode.ai"], JevConfig{BaseURL: base}.QuestionsPerRequest(),
			"base_url %q", base)
	}

	// An explicit value always decides, on any endpoint — including a number
	// that disagrees with the table.
	for _, n := range []int{1, 20, 500} {
		assert.Equal(t, n, JevConfig{BaseURL: "https://api.commandcode.ai/provider", MaxQuestionsPerRequest: n}.QuestionsPerRequest())
		assert.Equal(t, n, JevConfig{MaxQuestionsPerRequest: n}.QuestionsPerRequest())
	}
}

// 读者只需要填自己的 key / 主题：官方地址是这两个服务"开箱即用"的那一半，没写就落到它上面。
// 自建只是把同一个字段换成另一个地址，代码里没有第二套路径。
func TestPushURLsDefaultToThePublicServices(t *testing.T) {
	cfg := &Config{}
	require.NoError(t, defaults.Set(cfg))
	assert.Equal(t, "https://api.day.app", cfg.Push.Bark.URL)
	assert.Equal(t, "https://ntfy.sh", cfg.Push.Ntfy.URL)
	// 默认地址不等于"配好了"：没有 key / topic 就什么都不发。
	assert.False(t, cfg.Push.Wants())
	cfg.Push.Enabled = true
	assert.False(t, cfg.Push.Wants(), "两个渠道都还没填 key/topic")
	cfg.Push.Ntfy.Topic = "tachi-test"
	assert.True(t, cfg.Push.Wants())
}
