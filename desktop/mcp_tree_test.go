package main

// The MCP LOAD side, per session tree (desktop/agent_mcp.go's reloadMCPForActiveTree): the shared
// server set is read from the tree the DISPLAYED session is in, not from the process root that
// bootstrap used.

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// mcpTreeFixture prepares an app whose displayed session works in `tree`, with the tree carrying
// its own .tachi/mcp.json. The entry is DISABLED on purpose: what this pins is the config LOAD
// (which tree the set is read from), and a disabled server is never connected — so the test needs
// no fixture process and spawns nothing.
func mcpTreeFixture(t *testing.T, tree string) (*desktopApp, *AgentService, string) {
	t.Helper()
	mcpFile := filepath.Join(tree, ".tachi")
	if err := os.MkdirAll(mcpFile, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := `{"servers":[{"name":"proj-server","type":"stdio","command":"true","enabled":false}]}`
	if err := os.WriteFile(filepath.Join(mcpFile, "mcp.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}

	sm := newSessionManagerForTest(t, tree) // the session's primary tree IS tree
	sid := sm.Current().ID
	d, svc, _ := settingsFixture(t, "language: zh\n")
	d.sm = sm
	d.mcp = mcp.NewManager(context.Background(), d.cfg, logger.New("mcp-tree-test"))
	// SwitchMCPProfile refuses while initialization is in flight; a fixture has nothing to wait for.
	d.mcp.MarkInitDone()

	r := d.getRun(sid)
	r.sm = sm
	// A bare agent carrying the two things the reconcile needs. It is enough because
	// SwitchMCPProfile only reads them to reach the code under test — no turn runs.
	r.agent = &agent.AIAgent{Config: agent.AgentConfig{
		FullConfig: d.cfg,
		MCPManager: d.mcp,
		Logger:     logger.New("settings-test"),
	}}
	return d, svc, sid
}

// serverNames lists the configured servers, so a failure reads as names rather than a struct dump.
func serverNames(servers []config.MCPServerConfig) []string {
	out := make([]string, 0, len(servers))
	for _, srv := range servers {
		out = append(out, srv.Name)
	}
	return out
}

// TestActivateSessionLoadsTheProjectsMCPServers is the whole point: opening a session re-reads
// that session's tree, so a project's .tachi/mcp.json decides the shared server set — not the
// process root bootstrap read before any session existed.
func TestActivateSessionLoadsTheProjectsMCPServers(t *testing.T) {
	tree := t.TempDir()
	d, svc, sid := mcpTreeFixture(t, tree)

	if got := serverNames(d.cfg.MCPServers); len(got) != 0 {
		t.Fatalf("fixture: MCPServers = %v, want the empty bootstrap set", got)
	}

	if got := svc.ActivateSession(sid); got != "ok" {
		t.Fatalf("ActivateSession = %q, want ok", got)
	}

	if got := serverNames(d.cfg.MCPServers); len(got) != 1 || got[0] != "proj-server" {
		t.Errorf("MCPServers after activating the session = %v, want [proj-server] from the session's tree", got)
	}
}

// TestReloadMCPForActiveTreeIsIdempotent: it is called from every place the displayed tree can
// move, so the unchanged case has to be free of consequences — the same tree diffs to nothing and
// no server is reconnected.
func TestReloadMCPForActiveTreeIsIdempotent(t *testing.T) {
	d, _, sid := mcpTreeFixture(t, t.TempDir())

	d.mu.Lock()
	d.activeID = sid
	d.mu.Unlock()
	d.reloadMCPForActiveTree()
	first := append([]config.MCPServerConfig(nil), d.cfg.MCPServers...)
	if got := serverNames(first); len(got) != 1 || got[0] != "proj-server" {
		t.Fatalf("first reconcile produced %v, want [proj-server]", got)
	}

	d.reloadMCPForActiveTree()
	if !reflect.DeepEqual(first, d.cfg.MCPServers) {
		t.Errorf("second reconcile changed the set:\n%+v\n%+v", first, d.cfg.MCPServers)
	}
}

// TestReloadMCPForActiveTreeWithoutAManager: the call sites run in every desktop configuration, so
// the no-MCP case (no shared manager) has to be a quiet no-op rather than a nil dereference.
func TestReloadMCPForActiveTreeWithoutAManager(t *testing.T) {
	d, _, sid := mcpTreeFixture(t, t.TempDir())
	d.mcp = nil
	d.mu.Lock()
	d.activeID = sid
	d.mu.Unlock()
	d.reloadMCPForActiveTree() // must not panic
}
