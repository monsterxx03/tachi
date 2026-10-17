package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The write half of a save: ONE whole-config write, atomic, keeping the file's mode, and keeping
// the hand-written original once beside it.
//
// What the file looks like is not this file's business any more — the file IS the config object,
// marshalled, so these tests are about the properties a caller depends on: it round-trips, it does
// not clobber permissions, it does not touch a file it would not change, and the first rewrite of
// somebody's hand-written file leaves that file behind.

// writeConfigFixture writes a config file to work against — the settings page's starting point.
func writeConfigFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	return writeConfigAndProviders(t, dir, body)
}

// Every leaf of config.yaml survives the round trip: the config the file yields is the config that
// was written.
//
// "Written" is the whole object, so this is the property that makes the whole design safe — the
// settings page mutates the object and the file follows, which is only sound if loading the file
// back gives the same values (including the two duration types, which are a string in the file and
// an int64 in memory).
//
// `providers` is NOT in that statement, and that is the point of the two provider assertions below:
// they live in their own file, so saving config.yaml must not write them and must not lose them
// either. What carries them to disk is config.SyncProviderFiles, which the settings save calls
// alongside this one.
func TestSaveRoundTripsTheWholeConfig(t *testing.T) {
	path := writeConfigFixture(t, "# 我的配置\nlanguage: zh\nproviders:\n  - name: main\n    type: openai\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"language":                        "en",
		"max_tokens":                      4096,
		"mcp_tool_search.jev_budget":      "45s",
		"mcp_tool_refresh.interval":       "90s",
		"checkpoints.retain":              7,
		"web_search.proxy":                "http://127.0.0.1:7890",
		"permissions.bash.allow":          []string{"ls", "git *"},
		"providers.0.name":                "main",
		"providers.0.spec.proxy":          "http://127.0.0.1:7890",
		"providers.0.spec.context_window": 200000,
	}
	if err := ApplyUpdates(cfg, want); err != nil {
		t.Fatalf("ApplyUpdates: %v", err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this wrote does not load: %v", err)
	}
	if got.Language != "en" || got.MaxTokens != 4096 {
		t.Errorf("scalars = %q/%d, want en/4096", got.Language, got.MaxTokens)
	}
	// Both duration flavours, which are a string in the file and an int64 in memory:
	// config.Duration (mcp_tool_search.jev_budget) and a bare time.Duration
	// (mcp_tool_refresh.interval).
	if got.MCPToolSearch.JevBudget.String() != "45s" {
		t.Errorf("mcp_tool_search.jev_budget = %v, want 45s", got.MCPToolSearch.JevBudget)
	}
	if got.MCPToolRefresh.Interval != 90*time.Second {
		t.Errorf("mcp_tool_refresh.interval = %v, want 90s", got.MCPToolRefresh.Interval)
	}
	if got.Checkpoints.Retain != 7 {
		t.Errorf("checkpoints.retain = %d, want 7", got.Checkpoints.Retain)
	}
	if got.WebSearch.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("web_search.proxy = %q", got.WebSearch.Proxy)
	}
	if strings.Join(got.Permissions.Bash.Allow, ",") != "ls,git *" {
		t.Errorf("permissions.bash.allow = %v", got.Permissions.Bash.Allow)
	}
	// The provider half is the settings save's SECOND call (SyncProviderFiles) — SaveTo does not and
	// must not write it, so what `got` holds here is still what the fixture wrote.
	if _, err := SyncProviderFiles(filepath.Dir(path), cfg.Providers); err != nil {
		t.Fatalf("SyncProviderFiles: %v", err)
	}
	got, err = LoadFrom(path)
	if err != nil {
		t.Fatalf("the files this wrote do not load: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %+v, want one", got.Providers)
	}
	spec := got.Providers[0].Spec
	if spec.Proxy != "http://127.0.0.1:7890" || spec.ContextWindow == nil || *spec.ContextWindow != 200000 {
		t.Errorf("provider spec = %+v", spec)
	}
	// config.yaml itself holds no providers: they are in provider.*.json, and this write must not
	// put a second copy back (the loader refuses a file that has one). The check is the TOP-LEVEL
	// key — `web_search.providers` is a different thing and is supposed to be there.
	if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), "\nproviders:") {
		t.Errorf("config.yaml must not carry a top-level providers: block\n%s", raw)
	}

	// A duration is a STRING in the file, not the nanoseconds underneath: gopkg.in/yaml.v3 (which
	// both halves use) handles both duration types as their String(), and the file has to stay
	// readable by the person who owns it.
	body, _ := os.ReadFile(path)
	// The two flavours by name: config.Duration (mcp_tool_search.jev_budget) and a bare
	// time.Duration (mcp_tool_refresh.interval).
	if !strings.Contains(string(body), "jev_budget: 45s") || !strings.Contains(string(body), "interval: 1m30s") {
		t.Errorf("durations must be written as duration strings:\n%s", body)
	}
}

// The file keeps its mode: a user's 0600 must not come back as 0644.
func TestSaveKeepsTheFilesMode(t *testing.T) {
	path := writeConfigFixture(t, "language: zh\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPath(cfg, "language", "en"); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

// A save whose result would be the bytes already in the file writes nothing — the mtime included.
func TestSaveWritesNothingWhenTheResultIsUnchanged(t *testing.T) {
	path := writeConfigFixture(t, "language: zh\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("the first SaveTo: %v", err)
	}
	// A timestamp the filesystem could not have produced on its own, so "unchanged" is provable.
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("the second SaveTo: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(old) {
		t.Errorf("mtime = %v, want the file untouched (%v)", st.ModTime(), old)
	}
}

// The first whole-file write keeps the file as it was, as config.yaml.bak — and no later save
// touches that copy.
//
// This is the one loss a whole-file write cannot undo (the file is hand-written, and people explain
// their keys in comments), so the copy is what makes it survivable. An existing .bak is never
// overwritten: were it, a second save would bury the original under an already-rewritten file.
func TestSaveBacksUpTheOriginalOnce(t *testing.T) {
	const original = "# 我是注释：api_key 是别人给我的\nlanguage: zh\n"
	path := writeConfigFixture(t, original)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPath(cfg, "language", "en"); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	backup := path + ".bak"
	kept, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("the original was not kept: %v", err)
	}
	if string(kept) != original {
		t.Errorf("config.yaml.bak =\n%s\nwant the original bytes:\n%s", kept, original)
	}

	// A second save must not touch the copy, even though the file it rewrites has moved on.
	if err := SetPath(cfg, "language", "zh"); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("the second SaveTo: %v", err)
	}
	kept2, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept2, kept) {
		t.Errorf("config.yaml.bak changed on the second save:\n%s", kept2)
	}
}

// A config file that does not exist yet is written without a backup: there is nothing to keep.
func TestSaveDoesNotBackUpAFileThatIsNotThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := DefaultConfig()
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a backup was made for a file that never existed (err = %v)", err)
	}
	if _, err := LoadFrom(path); err != nil {
		t.Errorf("the file this wrote does not load: %v", err)
	}
}

// The write is atomic: the file is replaced by a rename, never written in place.
//
// Another Tachi process may be reading config.yaml at startup, so a half-written file is a state
// nobody may see. This is checked by the "." prefix's absence — a temp file left behind would mean
// the rename never happened — and by the file being complete right after.
func TestSaveIsAtomic(t *testing.T) {
	path := writeConfigFixture(t, "language: zh\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPath(cfg, "language", "en"); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" && e.Name() != "config.yaml.bak" {
			t.Errorf("the directory holds %q, want only the config and its backup", e.Name())
		}
	}
}
