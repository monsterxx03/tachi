package mcp

// Dump the configured MCP pool as the tool-search measurement's corpus.
//
// The tool-search gate's threshold is swept against the tool TEXT it judges, and
// a pool is only ever known locally — which MCP servers a machine has, and what
// their descriptions say, are not facts a repo can carry. So the corpus of a
// real-pool sweep is dumped here, on demand, and lives outside git; the
// threshold it produces is what gets recorded (docs/agents/jev.md).
//
// This runs the PRODUCTION path — config.LoadMCPServers + PopulateFromConnect —
// so the names, hints and summaries are the bytes the gate would send, whitelist
// and search-hint overrides included. A hand-written corpus cannot be checked
// against that; a dump cannot disagree with it.
//
//	Dump:  TOOL_SEARCH_POOL_OUT=/path/pool.json go test ./agent/mcp/ -run TestDumpDeferredPool -v
//
// It CONNECTS to every enabled server and reads the real home's config, which is
// why it is env-gated rather than a normal test: with the knob set it touches
// ~/.tachi (reading config and tokens, and refreshing a token if one has
// expired). Without it the test skips and the package's other tests stay
// hermetic.
//
// The output is the `corpus.json` shape `pkg/jev`'s tool-search harness reads
// (see pkg/jev/testdata/tool-search/corpus.json and tool_search_cases_test.go) —
// one record per tool, fields named as `jevsearch.Candidate` takes them.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

const (
	// dumpPoolOutEnv names the file to write. Unset means skip: the dump has no
	// sensible default destination (it must not land in the repo).
	dumpPoolOutEnv = "TOOL_SEARCH_POOL_OUT"
	// dumpPoolHomeEnv overrides the tachi home the servers are read from, for a
	// dump against a non-default config.
	dumpPoolHomeEnv = "TOOL_SEARCH_HOME"
	// dumpPoolWorkDirEnv adds a project scope to the mcp.json search
	// (`.tachi/mcp.json` under this directory). Empty uses the global files only.
	dumpPoolWorkDirEnv = "TOOL_SEARCH_WORKDIR"
)

// dumpedTool mirrors the corpus record. Kept in step with pkg/jev's `toolRecord`
// — the two are the two ends of one file format, and a field renamed on one side
// shows up as an empty field on the other (a question text missing its hint, not
// a crash), so both sides carry this pointer.
type dumpedTool struct {
	Name    string `json:"name"`
	Server  string `json:"server"`
	Hint    string `json:"hint"`
	Summary string `json:"summary"`
}

type dumpedCorpus struct {
	Notes string       `json:"notes"`
	Tools []dumpedTool `json:"tools"`
}

func TestDumpDeferredPool(t *testing.T) {
	out := envOrSkip(t, dumpPoolOutEnv)
	if home := os.Getenv(dumpPoolHomeEnv); home != "" {
		config.SetBaseDir(home)
	}
	workDir := os.Getenv(dumpPoolWorkDirEnv)
	ctx := t.Context()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.LoadMCPServers(workDir); err != nil {
		t.Fatalf("load mcp servers: %v", err)
	}
	if len(cfg.MCPServers) == 0 {
		t.Fatalf("no MCP servers configured (home=%s workdir=%q)", config.BaseDir(), workDir)
	}

	mgr := NewManager(ctx, cfg, logger.FromContext(ctx))
	_, all, errs := mgr.PopulateFromConnect(ctx, cfg)
	for _, err := range errs {
		// A server that failed to connect is a HOLE in the corpus, not a reason
		// to abandon the dump — but it has to be visible, or the sweep silently
		// measures a pool smaller than production's.
		t.Errorf("server did not connect: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no tools discovered")
	}

	pool := mgr.Pool().All()
	perServer := map[string]int{}
	corpus := dumpedCorpus{Tools: make([]dumpedTool, 0, len(pool))}
	for _, dt := range pool {
		perServer[dt.ServerName]++
		corpus.Tools = append(corpus.Tools, dumpedTool{
			Name:    dt.Name,
			Server:  dt.ServerName,
			Hint:    dt.SearchHint,
			Summary: strutil.FirstLineOrTruncate(dt.Description, 100),
		})
	}
	names := make([]string, 0, len(perServer))
	for name := range perServer {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("  %-16s %d tools", name, perServer[name])
	}
	corpus.Notes = "Dumped from a live MCP configuration by " +
		"agent/mcp/pool_dump_test.go (production pool: whitelists and search-hint overrides applied). " +
		"Local only — never commit a real pool. Fields are the ones jevsearch.Candidate takes."

	raw, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		t.Fatalf("marshal corpus: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(out), err)
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
	t.Logf("wrote %s: %d tools over %d servers", out, len(corpus.Tools), len(perServer))
}

// envOrSkip returns the value of an env var, or skips the test when it is unset
// (the knob IS the gate: see the file comment).
func envOrSkip(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	return v
}
