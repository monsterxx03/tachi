package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testMCPTool builds a minimal MCPTool wrapper for pool seeding.
func testMCPTool(server, toolName string) MCPTool {
	return MCPTool{
		serverName: server,
		serverTool: &mcp.Tool{
			Name:        toolName,
			Description: "test tool " + toolName,
		},
	}
}

// seedPoolWith tools adds tools to the manager's deferred pool.
func seedPool(m *Manager, tools ...MCPTool) {
	for _, t := range tools {
		m.pool.Add(NewDeferredToolFromMCPTool(t, ""))
	}
}

// seedSessionSet registers a per-session discovered set pointing at a custom
// persistence path, bypassing the config-derived default so tests can use a
// temp dir without touching the real ~/.tachi.
func seedSessionSet(m *Manager, sessionID, path string) *DiscoveredSet {
	set := NewDiscoveredSet(path)
	m.mu.Lock()
	m.sets[sessionID] = set
	m.mu.Unlock()
	return set
}

func TestManager_SetFor_LazilyCreatesAndIsolatesSessions(t *testing.T) {
	// Point the config-derived persistence dir at a temp dir so SetFor's
	// lazily created sets never touch the real ~/.tachi.
	config.SetBaseDir(t.TempDir())
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"), testMCPTool("postgres", "list"))

	// Session A discovers pg__query; session B discovers pg__list. Both
	// persist to their own files.
	a := m.SetFor("sess-a")
	b := m.SetFor("sess-b")
	a.Add("mcp__postgres__query")
	b.Add("mcp__postgres__list")

	// Same session → same instance (lazy creation is idempotent).
	assert.Same(t, a, m.SetFor("sess-a"))

	// Sets are isolated.
	assert.ElementsMatch(t, []string{"mcp__postgres__query"}, m.SetFor("sess-a").List())
	assert.ElementsMatch(t, []string{"mcp__postgres__list"}, m.SetFor("sess-b").List())

	// Each persisted to its own per-session file.
	_, errA := os.Stat(config.MCPDiscoveredFile("sess-a"))
	require.NoError(t, errA)
	_, errB := os.Stat(config.MCPDiscoveredFile("sess-b"))
	require.NoError(t, errB)
}

func TestManager_SetFor_EmptySessionID(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	assert.Nil(t, m.SetFor(""))
	assert.Nil(t, m.SetIfExists(""))
}

func TestManager_SetFor_RestoresPersistedSession(t *testing.T) {
	config.SetBaseDir(t.TempDir())

	// A previous process session "sess-a" had discovered pg__query,
	// persisted at the config-derived per-session path.
	prev := NewDiscoveredSet(config.MCPDiscoveredFile("sess-a"))
	prev.Add("mcp__postgres__query")

	// Fresh manager simulating a restart: SetFor lazily creates the session's
	// set and restores it from that same path. A different session starts
	// empty — no leakage between sessions.
	m2 := NewManager(t.Context(), nil, nil)
	seedPool(m2, testMCPTool("postgres", "query"), testMCPTool("postgres", "list"))

	restored := m2.SetFor("sess-a")
	assert.ElementsMatch(t, []string{"mcp__postgres__query"}, restored.List())
	assert.Empty(t, m2.SetFor("sess-b").List(), "a fresh session must not inherit another session's tools")
}

func TestManager_SetFor_InjectsAutoLoadBaseline(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"))

	// Simulate PopulateFromConnect recording auto-load tools.
	m.mu.Lock()
	m.autoLoadNames = append(m.autoLoadNames, "mcp__postgres__query")
	m.mu.Unlock()

	// A set created after population carries the auto-load baseline.
	set := m.SetFor("sess-a")
	assert.True(t, set.Contains("mcp__postgres__query"), "auto-load tools must be in every session's set")
}

func TestManager_BackfillAutoLoadIntoExistingSets(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"))

	// A set was created before the pool finished populating.
	path := filepath.Join(t.TempDir(), "sess-a", "mcp_discovered.json")
	seedSessionSet(m, "sess-a", path)
	m.mu.Lock()
	m.autoLoadNames = append(m.autoLoadNames, "mcp__postgres__query")
	m.mu.Unlock()

	m.backfillAutoLoadIntoSets()

	assert.True(t, m.SetFor("sess-a").Contains("mcp__postgres__query"))
}

func TestManager_RestoreDiscoveredFromDisk_FiltersByPool(t *testing.T) {
	// Pool has pg__query and pg__list; gh__pr is NOT in the pool.
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"), testMCPTool("postgres", "list"))

	path := filepath.Join(t.TempDir(), "sess-a", "mcp_discovered.json")
	set := seedSessionSet(m, "sess-a", path)

	// Simulate a previous session that discovered all three, then config
	// dropped the github server: the persisted file still lists gh__pr.
	set.AddAll([]string{
		"mcp__postgres__query",
		"mcp__postgres__list",
		"mcp__github__pr",
	})
	require.NoError(t, set.Save())

	// Restart: fresh set for the same session, pool repopulated above.
	m2 := NewManager(t.Context(), nil, nil)
	seedPool(m2, testMCPTool("postgres", "query"), testMCPTool("postgres", "list"))
	set2 := seedSessionSet(m2, "sess-a", path)

	m2.restoreDiscoveredFromDisk(context.Background(), set2)

	assert.Equal(t, []string{"mcp__postgres__query", "mcp__postgres__list"}, set2.List())
	// The persisted file should now only contain surviving tools.
	loaded, err := set2.LoadFromDisk()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"mcp__postgres__query", "mcp__postgres__list"}, loaded)
}

func TestManager_RestoreDiscoveredFromDisk_PreservesAutoLoad(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"), testMCPTool("postgres", "list"))

	path := filepath.Join(t.TempDir(), "sess-a", "mcp_discovered.json")
	set := seedSessionSet(m, "sess-a", path)
	m.mu.Lock()
	m.autoLoadNames = []string{"mcp__postgres__query"}
	m.mu.Unlock()

	// Auto-load tools are already in the set (as SetFor would inject them).
	set.addNoPersist("mcp__postgres__query")

	// Simulate a prior session that also discovered pg__list via search.
	set.Add("mcp__postgres__list")

	// Restart: fresh set, auto-load re-added, persisted list restored.
	m2 := NewManager(t.Context(), nil, nil)
	seedPool(m2, testMCPTool("postgres", "query"), testMCPTool("postgres", "list"))
	set2 := seedSessionSet(m2, "sess-a", path)
	m2.mu.Lock()
	m2.autoLoadNames = []string{"mcp__postgres__query"}
	m2.mu.Unlock()
	set2.addNoPersist("mcp__postgres__query") // auto-load path

	m2.restoreDiscoveredFromDisk(context.Background(), set2)

	// auto-load tool survives (not clobbered by restore) and the searched
	// tool comes back.
	assert.ElementsMatch(t, []string{"mcp__postgres__query", "mcp__postgres__list"}, set2.List())
}

func TestManager_RestoreDiscoveredFromDisk_PoolEmptyKeepsHistory(t *testing.T) {
	// Pool is empty (servers still connecting / all failed). Persisted tools
	// must NOT be dropped — they may come back once a server connects.
	m := NewManager(t.Context(), nil, nil)

	path := filepath.Join(t.TempDir(), "sess-a", "mcp_discovered.json")
	set := seedSessionSet(m, "sess-a", path)
	set.AddAll([]string{"mcp__wiki__getArticleDetail", "mcp__wiki__getArticleTree"})

	m.restoreDiscoveredFromDisk(context.Background(), set)

	// History is kept in memory AND on disk.
	assert.ElementsMatch(t, []string{"mcp__wiki__getArticleDetail", "mcp__wiki__getArticleTree"}, set.List())
	loaded, err := set.LoadFromDisk()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"mcp__wiki__getArticleDetail", "mcp__wiki__getArticleTree"}, loaded)
}

func TestManager_RestoreDiscoveredFromDisk_ConfiguredServerNotConnectedKept(t *testing.T) {
	// Server "wiki" is configured but did not connect this run (connection
	// failed). Its persisted tools must be kept, not judged stale.
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("wiki", "getArticleTree")) // wiki connected, one tool
	m.mu.Lock()
	m.serverCfgs = map[string]config.MCPServerConfig{"wiki": {Name: "wiki"}}
	m.mu.Unlock()

	path := filepath.Join(t.TempDir(), "sess-a", "mcp_discovered.json")
	set := seedSessionSet(m, "sess-a", path)
	// getArticleTree is in the pool; getArticleDetail's server is configured
	// but its tool wasn't discovered (connection hiccup).
	set.AddAll([]string{"mcp__wiki__getArticleTree", "mcp__wiki__getArticleDetail"})

	m.restoreDiscoveredFromDisk(context.Background(), set)

	assert.ElementsMatch(t, []string{"mcp__wiki__getArticleTree", "mcp__wiki__getArticleDetail"}, set.List(),
		"configured-but-unconnected server's tools must be kept")
}

func TestManager_RestoreDiscoveredFromDisk_ServerRemovedFromConfig(t *testing.T) {
	// The github server was removed from config entirely: its tools are real
	// stale entries and must be dropped from memory and the persisted file.
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"))
	m.mu.Lock()
	m.serverCfgs = map[string]config.MCPServerConfig{"postgres": {Name: "postgres"}}
	m.mu.Unlock()

	path := filepath.Join(t.TempDir(), "sess-a", "mcp_discovered.json")
	set := seedSessionSet(m, "sess-a", path)
	set.AddAll([]string{"mcp__postgres__query", "mcp__github__pr"})

	m.restoreDiscoveredFromDisk(context.Background(), set)

	assert.ElementsMatch(t, []string{"mcp__postgres__query"}, set.List())
	loaded, err := set.LoadFromDisk()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"mcp__postgres__query"}, loaded, "removed server's tools must be cleaned from the file")
}

func TestManager_RestoreDiscoveredFromDisk_CorruptFile(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"))

	path := filepath.Join(t.TempDir(), "sess-a", "mcp_discovered.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{corrupt"), 0o600))
	set := seedSessionSet(m, "sess-a", path)

	// Corrupt file must not panic; set stays empty.
	m.restoreDiscoveredFromDisk(context.Background(), set)
	assert.Empty(t, set.List())
}

func TestManager_RestoreDiscoveredFromDisk_MissingFile(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	seedPool(m, testMCPTool("postgres", "query"))

	path := filepath.Join(t.TempDir(), "sess-a", "does-not-exist.json")
	set := seedSessionSet(m, "sess-a", path)

	m.restoreDiscoveredFromDisk(context.Background(), set) // no-op on missing file
	assert.Empty(t, set.List())
}

func TestManager_EachDiscoveredSet(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	seedSessionSet(m, "sess-a", filepath.Join(t.TempDir(), "a.json"))
	seedSessionSet(m, "sess-b", filepath.Join(t.TempDir(), "b.json"))

	var seen []string
	m.EachDiscoveredSet(func(s *DiscoveredSet) {
		seen = append(seen, s.persistPath)
	})
	assert.Len(t, seen, 2)
}

func TestManager_SetForBeforePopulateThenBackfill(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	m := NewManager(t.Context(), nil, nil)
	// Pool NOT populated yet — servers still connecting.

	// Previous process: the session had discovered pg__query and gh__pr.
	prev := NewDiscoveredSet(config.MCPDiscoveredFile("sess-a"))
	prev.AddAll([]string{"mcp__postgres__query", "mcp__github__pr"})

	// SetFor fires before the pool is populated (turn starts while MCP is
	// still connecting): everything must be kept, nothing dropped.
	set := m.SetFor("sess-a")
	assert.ElementsMatch(t, []string{"mcp__postgres__query", "mcp__github__pr"}, set.List(),
		"pool not populated yet: keep the whole history")

	// Servers connect: pool fills with pg tools; github is gone from config.
	seedPool(m, testMCPTool("postgres", "query"))
	m.mu.Lock()
	m.serverCfgs = map[string]config.MCPServerConfig{"postgres": {Name: "postgres"}}
	m.mu.Unlock()

	// Backfill re-runs restore, which can now tell that github is gone.
	m.backfillAutoLoadIntoSets()
	assert.ElementsMatch(t, []string{"mcp__postgres__query"}, set.List(),
		"backfill after populate must drop real stale entries")
	loaded, err := set.LoadFromDisk()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"mcp__postgres__query"}, loaded)
}
