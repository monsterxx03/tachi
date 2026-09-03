package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

func TestDiffMCPServers(t *testing.T) {
	oldCfgs := []config.MCPServerConfig{
		{Name: "kept-svc", Type: config.MCPTransportHTTP, URL: "https://a.example.com", Profile: "old"},
		{Name: "toggled-svc", Type: config.MCPTransportHTTP, URL: "https://b.example.com"},
		{Name: "changed-svc", Type: config.MCPTransportHTTP, URL: "https://old.example.com"},
		{Name: "removed-svc", Type: config.MCPTransportStdio, Command: "/bin/old"},
	}
	disabled := false
	oldCfgs[1].Enabled = &disabled

	newCfgs := []config.MCPServerConfig{
		// Same server, only Profile re-stamped → still kept.
		{Name: "kept-svc", Type: config.MCPTransportHTTP, URL: "https://a.example.com", Profile: "new"},
		// Same server, runtime toggle vs fresh-from-JSON → kept (Enabled ignored).
		{Name: "toggled-svc", Type: config.MCPTransportHTTP, URL: "https://b.example.com"},
		// URL changed → changed.
		{Name: "changed-svc", Type: config.MCPTransportHTTP, URL: "https://new.example.com"},
		// Brand new → added.
		{Name: "added-svc", Type: config.MCPTransportStdio, Command: "/bin/new"},
	}

	diff := diffMCPServers(oldCfgs, newCfgs)

	require.Len(t, diff.kept, 2)
	assert.Equal(t, "kept-svc", diff.kept[0].Name)
	assert.Equal(t, "toggled-svc", diff.kept[1].Name)
	// Kept entries come from the OLD list (runtime state preserved).
	assert.NotNil(t, diff.kept[1].Enabled)
	assert.False(t, diff.kept[1].IsEnabled())

	require.Len(t, diff.removed, 1)
	assert.Equal(t, "removed-svc", diff.removed[0].Name)

	require.Len(t, diff.changed, 1)
	assert.Equal(t, "changed-svc", diff.changed[0].Name)
	assert.Equal(t, "https://new.example.com", diff.changed[0].URL)

	require.Len(t, diff.added, 1)
	assert.Equal(t, "added-svc", diff.added[0].Name)
}

// newMCPTestAgent builds an agent wired to a real (empty) MCP manager, for
// exercising SwitchMCPProfile without live server connections.
func newMCPTestAgent(t *testing.T, cfg *config.Config) *AIAgent {
	t.Helper()
	a := newTestAgent(t, &mockStreamProvider{})
	a.Config.Logger = logger.Default()
	a.Config.FullConfig = cfg
	a.Config.MCPManager = mcp.NewManager(t.Context(), cfg, logger.Default())
	return a
}

// writeMCPTestJSON writes an MCP config file (profile files included).
func writeMCPTestJSON(t *testing.T, path string, servers []config.MCPServerConfig) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data, err := json.Marshal(mcpConfigFileShim{Servers: servers})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
}

// mcpConfigFileShim mirrors config's internal JSON envelope.
type mcpConfigFileShim struct {
	Servers []config.MCPServerConfig `json:"servers"`
}

func TestSwitchMCPProfile_RefusesWhileInitializing(t *testing.T) {
	cfg := config.DefaultConfig()
	a := newMCPTestAgent(t, cfg)
	// Manager's InitDone is never marked → switch must refuse.

	_, err := a.SwitchMCPProfile(t.Context(), "prod", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initialization")
}

func TestSwitchMCPProfile_RefusesWithoutManager(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{})
	a.Config.FullConfig = config.DefaultConfig()
	a.Config.MCPManager = nil

	_, err := a.SwitchMCPProfile(t.Context(), "prod", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestSwitchMCPProfile_LoadFailureRollsBack(t *testing.T) {
	oldBase := config.BaseDir()
	defer config.SetBaseDir(oldBase)
	globalDir := t.TempDir()
	config.SetBaseDir(globalDir)

	writeMCPTestJSON(t, filepath.Join(globalDir, "mcp.json"), []config.MCPServerConfig{
		{Name: "svc-a", Type: config.MCPTransportHTTP, URL: "https://a.example.com"},
	})
	// Invalid profile JSON → LoadMCPServers fails.
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "mcp.bad.json"), []byte("{nope"), 0600))

	cfg := config.DefaultConfig()
	cfg.ActiveMCPProfile = ""
	require.NoError(t, cfg.LoadMCPServers(""))
	require.Len(t, cfg.MCPServers, 1)

	a := newMCPTestAgent(t, cfg)
	a.Config.MCPManager.MarkInitDone()

	_, err := a.SwitchMCPProfile(t.Context(), "bad", "")
	require.Error(t, err)

	// State rolled back: profile unchanged, server list untouched.
	assert.Equal(t, "", cfg.ActiveMCPProfile)
	require.Len(t, cfg.MCPServers, 1)
	assert.Equal(t, "svc-a", cfg.MCPServers[0].Name)
}

func TestSwitchMCPProfile_Reconciles(t *testing.T) {
	oldBase := config.BaseDir()
	defer config.SetBaseDir(oldBase)
	globalDir := t.TempDir()
	config.SetBaseDir(globalDir)

	// Base: two always-on servers. dev profile adds svc-dev; empty profile
	// adds nothing (empty servers list). All URLs point at a refused port so
	// revive attempts fail fast and hermetically.
	writeMCPTestJSON(t, filepath.Join(globalDir, "mcp.json"), []config.MCPServerConfig{
		{Name: "svc-a", Type: config.MCPTransportHTTP, URL: "http://127.0.0.1:1/mcp", Timeout: config.Duration(200 * time.Millisecond)},
		{Name: "svc-b", Type: config.MCPTransportHTTP, URL: "http://127.0.0.1:1/mcp", Timeout: config.Duration(200 * time.Millisecond)},
	})
	writeMCPTestJSON(t, filepath.Join(globalDir, "mcp.dev.json"), []config.MCPServerConfig{
		{Name: "svc-dev", Type: config.MCPTransportHTTP, URL: "http://127.0.0.1:1/mcp"},
	})
	writeMCPTestJSON(t, filepath.Join(globalDir, "mcp.empty.json"), []config.MCPServerConfig{})

	cfg := config.DefaultConfig()
	cfg.ActiveMCPProfile = "dev"
	require.NoError(t, cfg.LoadMCPServers(""))
	require.Len(t, cfg.MCPServers, 3)

	a := newMCPTestAgent(t, cfg)
	a.Config.MCPManager.MarkInitDone()

	// Simulate a runtime toggle on svc-b before the switch. Neither server
	// is connected (no real MCP server here), so the switch retries svc-a
	// (enabled) while svc-b (disabled) is left alone.
	disabled := false
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == "svc-b" {
			cfg.MCPServers[i].Enabled = &disabled
		}
	}

	res, err := a.SwitchMCPProfile(t.Context(), "empty", "")
	require.NoError(t, err)

	assert.Equal(t, "empty", cfg.ActiveMCPProfile)
	// svc-b kept (disabled); svc-a was revived → retried → connect error,
	// so it is reported as an error rather than "kept running".
	assert.ElementsMatch(t, []string{"svc-b"}, res.Kept)
	assert.ElementsMatch(t, []string{"svc-dev"}, res.Removed)
	assert.Empty(t, res.Added)
	assert.Empty(t, res.Reconned)
	require.Len(t, res.Errors, 1)
	assert.Contains(t, res.Errors[0].Error(), "svc-a")

	// Final config: base servers only, reusing the OLD objects (runtime
	// toggle on svc-b preserved, svc-dev dropped).
	require.Len(t, cfg.MCPServers, 2)
	assert.Equal(t, "svc-a", cfg.MCPServers[0].Name)
	assert.Equal(t, "svc-b", cfg.MCPServers[1].Name)
	assert.NotNil(t, cfg.MCPServers[1].Enabled)
	assert.False(t, cfg.MCPServers[1].IsEnabled())
	assert.Empty(t, cfg.MCPServers[0].Profile) // kept objects keep their old stamp
	assert.False(t, a.Config.MCPManager.IsConnected("svc-a"))
}

func TestSwitchMCPProfile_DuplicateInProgress(t *testing.T) {
	cfg := config.DefaultConfig()
	a := newMCPTestAgent(t, cfg)
	a.Config.MCPManager.MarkInitDone()

	// Simulate an in-flight switch holding the mutex.
	a.mcpSwitchMu.Lock()
	defer a.mcpSwitchMu.Unlock()

	_, err := a.SwitchMCPProfile(t.Context(), "prod", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already in progress")
}

func TestSwitchMCPProfile_UnknownProfile(t *testing.T) {
	oldBase := config.BaseDir()
	defer config.SetBaseDir(oldBase)
	globalDir := t.TempDir()
	config.SetBaseDir(globalDir)

	writeMCPTestJSON(t, filepath.Join(globalDir, "mcp.json"), []config.MCPServerConfig{
		{Name: "svc", Type: config.MCPTransportHTTP, URL: "https://svc.example.com"},
	})

	cfg := config.DefaultConfig()
	require.NoError(t, cfg.LoadMCPServers(""))

	a := newMCPTestAgent(t, cfg)
	a.Config.MCPManager.MarkInitDone()

	// No mcp.ghost.json anywhere — the switch must refuse instead of
	// "succeeding" as a silent no-op (LoadMCPConfig skips missing files).
	_, err := a.SwitchMCPProfile(t.Context(), "ghost", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// State untouched.
	assert.Equal(t, "", cfg.ActiveMCPProfile)
	require.Len(t, cfg.MCPServers, 1)
	assert.Equal(t, "svc", cfg.MCPServers[0].Name)
}

func TestMCPSwitchBudget(t *testing.T) {
	disabled := false
	servers := []config.MCPServerConfig{
		{Name: "a", Timeout: config.Duration(2 * time.Second)},
		{Name: "b", Timeout: config.Duration(7 * time.Second)},
		{Name: "off", Timeout: config.Duration(30 * time.Second), Enabled: &disabled},
	}
	// Batch budget = the largest ENABLED server's timeout + margin.
	assert.Equal(t, 7*time.Second+profileSwitchMargin, mcpSwitchBudget(servers))

	// Nothing to connect → falls back to the config default.
	assert.Equal(t, time.Duration(config.DefaultMCPServerTimeout)+profileSwitchMargin, mcpSwitchBudget(nil))
}

func TestSwitchMCPProfile_ChangedServerConnectError(t *testing.T) {
	oldBase := config.BaseDir()
	defer config.SetBaseDir(oldBase)
	globalDir := t.TempDir()
	config.SetBaseDir(globalDir)

	writeMCPTestJSON(t, filepath.Join(globalDir, "mcp.json"), []config.MCPServerConfig{
		{Name: "svc", Type: config.MCPTransportHTTP, URL: "https://old.example.com"},
	})
	// Profile changes the URL to a refused port → connect must fail.
	writeMCPTestJSON(t, filepath.Join(globalDir, "mcp.move.json"), []config.MCPServerConfig{
		{Name: "svc", Type: config.MCPTransportHTTP, URL: "http://127.0.0.1:1/mcp", Timeout: config.Duration(200 * time.Millisecond)},
	})

	cfg := config.DefaultConfig()
	cfg.ActiveMCPProfile = ""
	require.NoError(t, cfg.LoadMCPServers(""))

	a := newMCPTestAgent(t, cfg)
	a.Config.MCPManager.MarkInitDone()

	res, err := a.SwitchMCPProfile(t.Context(), "move", "")
	require.NoError(t, err) // switch itself succeeds; per-server error reported

	require.Len(t, res.Errors, 1)
	assert.Contains(t, res.Errors[0].Error(), "svc")
	assert.Empty(t, res.Reconned)
	assert.Equal(t, "move", cfg.ActiveMCPProfile)
	// Server config updated in place even though the connection failed.
	require.Len(t, cfg.MCPServers, 1)
	assert.Equal(t, "http://127.0.0.1:1/mcp", cfg.MCPServers[0].URL)
	assert.False(t, a.Config.MCPManager.IsConnected("svc"))
}
