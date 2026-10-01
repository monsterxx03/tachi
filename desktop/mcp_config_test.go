package main

// The MCP LOAD side: the shared server set comes from the GLOBAL mcp.json files (~/.tachi) only,
// and the profile switch installs a profile's servers through the config's one guarded writer.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// mcpConfigFixture prepares an app whose global home carries one DISABLED server in mcp.json.
// Disabled on purpose: what these tests pin is the config LOAD and the switch's bookkeeping, and a
// disabled server is never connected — so the fixture needs no server process and spawns nothing.
func mcpConfigFixture(t *testing.T) (*desktopApp, *AgentService, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, t.TempDir())
	sid := sm.Current().ID
	d, svc, _ := settingsFixture(t, "language: zh\n")
	d.sm = sm

	writeMCPConfigFile(t, "mcp.json",
		`{"servers":[{"name":"base-server","type":"stdio","command":"true","enabled":false}]}`)

	d.mcp = mcp.NewManager(context.Background(), d.cfg, logger.New("mcp-config-test"))
	// A profile switch refuses while initialization is in flight; a fixture has nothing to wait for.
	d.mcp.MarkInitDone()

	r := d.getRun(sid)
	r.sm = sm
	// A bare agent carrying the two things the reconcile needs. It is enough because the switch
	// only reads them to reach the code under test — no turn runs.
	r.agent = &agent.AIAgent{Config: agent.AgentConfig{
		FullConfig: d.cfg,
		MCPManager: d.mcp,
		Logger:     logger.New("settings-test"),
	}}
	return d, svc, sid
}

// writeMCPConfigFile writes an MCP config file into the active tachi home — the BaseDir
// settingsFixture set, which stands in for ~/.tachi.
func writeMCPConfigFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(config.BaseDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// serverNames lists the configured servers, so a failure reads as names rather than a struct dump.
func serverNames(servers []config.MCPServerConfig) []string {
	out := make([]string, 0, len(servers))
	for _, srv := range servers {
		out = append(out, srv.Name)
	}
	return out
}

// TestSwitchMCPProfileToInstallsTheProfilesServers: the desktop's own switch path loads the
// profile, reconciles the connections, and writes both config fields — under cfgMu, and only
// after the reconcile returned. This pins the end state.
func TestSwitchMCPProfileToInstallsTheProfilesServers(t *testing.T) {
	d, _, _ := mcpConfigFixture(t)
	writeMCPConfigFile(t, "mcp.prod.json",
		`{"servers":[{"name":"prod-server","type":"stdio","command":"true","enabled":false}]}`)

	if err := d.switchMCPProfileTo("prod"); err != nil {
		t.Fatalf("switchMCPProfileTo: %v", err)
	}
	if got := d.cfg.ActiveMCPProfile; got != "prod" {
		t.Errorf("ActiveMCPProfile = %q, want prod", got)
	}
	names := serverNames(d.cfg.MCPServers)
	if len(names) != 2 || names[0] != "base-server" || names[1] != "prod-server" {
		t.Errorf("MCPServers = %v, want the base file plus the profile overlay", names)
	}
}

// TestSwitchMCPProfileToLeavesTheConfigAloneWhenTheProfileIsUnknown is the rollback that used to
// be vacuous: the profile field was written before the switch, so a failure had nothing to restore
// it to (the "previous" value it captured was already the new one). Writing only after a
// successful reconcile means there is nothing to roll back — the config still describes the
// servers that are actually loaded.
func TestSwitchMCPProfileToLeavesTheConfigAloneWhenTheProfileIsUnknown(t *testing.T) {
	d, _, _ := mcpConfigFixture(t)

	if err := d.switchMCPProfileTo("nope"); err == nil {
		t.Fatal("switching to a profile no file provides must fail")
	}
	if got := d.cfg.ActiveMCPProfile; got != "" {
		t.Errorf("ActiveMCPProfile = %q after a failed switch, want it untouched", got)
	}
	if got := serverNames(d.cfg.MCPServers); len(got) != 0 {
		t.Errorf("MCPServers = %v after a failed switch, want it untouched", got)
	}
}

// TestSaveSettingsAppliesAnOwnedKeyThroughItsRow: `active_mcp_profile` is the one row that OWNS
// the write of its leaf (OwnsWrite) — the batch must not pre-apply it, and the row must still
// write the value in the branch where there is nothing to switch (MCP off), or the setting would
// reach the file and not the process.
func TestSaveSettingsAppliesAnOwnedKeyThroughItsRow(t *testing.T) {
	sm := newSessionManagerForTest(t, t.TempDir())
	sid := sm.Current().ID
	d, svc, path := settingsFixture(t, "active_mcp_profile: \"\"\n")
	d.sm = sm
	d.getRun(sid).sm = sm

	res := svc.SaveSettings(map[string]any{"active_mcp_profile": "prod"}, nil, nil)
	if !strings.Contains(res.Summary, "1 项已生效") {
		t.Errorf("summary = %q, want the key reported live", res.Summary)
	}
	if got := d.cfg.ActiveMCPProfile; got != "prod" {
		t.Errorf("ActiveMCPProfile = %q, want the row to have written it", got)
	}
	if file := readFileT(t, path); !strings.Contains(file, "active_mcp_profile: prod") {
		t.Errorf("config.yaml = %q", file)
	}
}
