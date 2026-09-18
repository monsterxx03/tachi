package main

// The settings table's guard rails (design §八之三 ②).
//
// The classification in settings.go is only worth having if it cannot go stale, so these
// tests hold it to the WHOLE of config.Config instead of a list somebody maintains by hand:
// every yaml leaf must resolve to a row, and every row must match a real leaf. That is what
// turns "somebody has to remember to mark this one 需重启" into a failing build.
//
// The leaf list comes from config.YAMLPaths — the same walker the settings SCHEMA is built
// on — so there is one definition of "what a leaf is" for the guard, the form, and the path
// API. A hand-written list here is exactly the thing that rots when a field is added.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

func configLeaves(t *testing.T) []string {
	t.Helper()
	leaves := config.YAMLPaths()
	if len(leaves) < 100 {
		t.Fatalf("only %d leaves found — the walker is wrong, not the config", len(leaves))
	}
	return leaves
}

// TestEveryConfigLeafHasASettingDisposition is the point of the table: a config field
// cannot be added without deciding what it takes to make it live. The failure names the
// leaf, so the fix is one row (or an applyUnused row saying which entry mode owns it).
func TestEveryConfigLeafHasASettingDisposition(t *testing.T) {
	for _, leaf := range configLeaves(t) {
		if _, ok := settingFor(leaf); !ok {
			t.Errorf("config leaf %q has no row in settingsTable: classify it "+
				"(applyLazy / applyPush / applyRestart / applyUnused) before adding it", leaf)
		}
	}
}

// TestNoSettingRowIsDead is the other half: a row that matches nothing is a typo — or a key
// that was renamed — left looking authoritative. It would also silently stop guarding the
// leaves it was written for.
func TestNoSettingRowIsDead(t *testing.T) {
	leaves := configLeaves(t)
	for _, row := range settingsTable {
		if !slices.ContainsFunc(leaves, row.covers) {
			t.Errorf("settingsTable row %q matches no yaml leaf under config.Config", row.Key)
		}
	}
}

// TestSettingsTableRules pins what each mode MEANS, so a row cannot claim more than it has:
// a lazy row that carries an Apply (something would be pushed twice), a push with neither
// code nor a reason, a restart row with code, a row with no reason at all.
func TestSettingsTableRules(t *testing.T) {
	seen := make(map[string]bool, len(settingsTable))
	for _, row := range settingsTable {
		if row.Key == "" {
			t.Error("a row has an empty Key")
			continue
		}
		if seen[row.Key] {
			t.Errorf("duplicate row for %q: the second one is unreachable", row.Key)
		}
		seen[row.Key] = true

		switch row.Mode {
		case applyLazy:
			if row.Apply != nil {
				t.Errorf("lazy row %q carries an Apply — lazy means the readers already see it", row.Key)
			}
		case applyPush:
			if row.Apply == nil && row.Note == "" {
				t.Errorf("push row %q has no Apply and no Note: needsRestart() reports a restart "+
					"whose reason nobody can read", row.Key)
			}
		case applyRestart, applyUnused:
			if row.Apply != nil {
				t.Errorf("row %q is %s but carries an Apply — nothing can push a value this mode says is unreachable", row.Key, row.Mode)
			}
			if row.Note == "" {
				t.Errorf("row %q is %s and must say why", row.Key, row.Mode)
			}
		default:
			t.Errorf("row %q has an unknown applyMode %d", row.Key, row.Mode)
		}

		// The two derived answers the form reads, pinned here because they are what the
		// reader actually sees. needsRestart is "no runtime path TODAY": a restart row, or a
		// push whose code is not written yet — never a property somebody sets.
		wantRestart := row.Mode == applyRestart || (row.Mode == applyPush && row.Apply == nil)
		if row.needsRestart() != wantRestart {
			t.Errorf("row %q: needsRestart() = %v, want %v (mode %s, Apply set: %v)",
				row.Key, row.needsRestart(), wantRestart, row.Mode, row.Apply != nil)
		}
		// An unused key is the one kind the form must not offer: a switch nothing reads is
		// worse than no switch.
		if row.offered() == (row.Mode == applyUnused) {
			t.Errorf("row %q: offered() = %v for mode %s", row.Key, row.offered(), row.Mode)
		}
	}
}

// TestSettingForPrefersTheMostSpecificRow: a leaf must not inherit its block's mode. `logs` is
// the block that actually splits — its level is hot, its rotation limits are frozen in an open
// file handle — and the prefix match must respect segment boundaries, or `provider` would cover
// `provider_aliases` (a different fact with a different fate).
func TestSettingForPrefersTheMostSpecificRow(t *testing.T) {
	if row, _ := settingFor("logs.level"); row.Mode != applyPush || row.needsRestart() {
		t.Errorf("logs.level resolved to (%v, needsRestart=%v), want the hot push row", row.Mode, row.needsRestart())
	}
	if row, _ := settingFor("logs.max_bytes"); row.Mode != applyRestart {
		t.Errorf("logs.max_bytes resolved to %v, want the block's restart row", row.Mode)
	}
	// Segment boundaries: a prefix is a whole segment, never a string prefix.
	if row, ok := settingFor("provider_aliases"); !ok || row.Mode != applyRestart {
		t.Errorf("provider_aliases resolved to (%v, %v), want the restart row of its own", row.Mode, ok)
	}
	if _, ok := settingFor("providersomething"); ok {
		t.Error(`"providersomething" matched a row: prefix matches must stop at segment boundaries`)
	}
}

// TestApplyToLiveAgentsLabelsFailures covers the fan-out's two jobs: reaching every session
// that HAS an agent (a run created but not built is not a session to change), and saying
// WHICH one refused, by title when it has one.
func TestApplyToLiveAgentsLabelsFailures(t *testing.T) {
	dir := t.TempDir()
	titled := newSessionManagerForTest(t, dir)
	titled.SetTitle("会话甲")
	untitled := newSessionManagerForTest(t, dir)

	d := newTestApp()
	d.getRun("no-agent") // built run, no agent: must be skipped

	fails := &agent.AIAgent{}
	succeeds := &agent.AIAgent{}
	// aaa: a titled session that fails → the line must name it by TITLE.
	if r := d.getRun("aaa"); true {
		r.agent, r.sm = fails, titled
	}
	// bbb: succeeds — a success is not reported at all.
	if r := d.getRun("bbb"); true {
		r.agent, r.sm = succeeds, untitled
	}
	// ccc: no manager built yet, so no title to read → the id stands in.
	if r := d.getRun("ccc"); true {
		r.agent = &agent.AIAgent{}
	}

	seen := 0
	failures := d.applyToLiveAgents(func(a *agent.AIAgent) error {
		seen++
		if a == succeeds {
			return nil
		}
		return errors.New("应用失败")
	})
	if seen != 3 {
		t.Errorf("fn ran for %d agents, want 3 (aaa, bbb, ccc — not the agent-less run)", seen)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %v, want one line per failing session (aaa has a title, ccc falls back to its id)", failures)
	}
	if !strings.Contains(failures[0], "会话甲") || !strings.Contains(failures[0], "应用失败") {
		t.Errorf("failure = %q, want the session's TITLE and the reason", failures[0])
	}
	if !strings.Contains(failures[1], "ccc") {
		t.Errorf("failure = %q, want the session ID when there is no title to name it by", failures[1])
	}
}

// TestLanguageRowClearsThePromptCache is the first row end to end, and it pins the trap it
// exists for: the prompt is memoized under a key with no language in it, so saving a new
// language does NOTHING until something drops the memo. The row is that something.
func TestLanguageRowClearsThePromptCache(t *testing.T) {
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	sm := newSessionManagerForTest(t, t.TempDir())
	sid := sm.Current().ID
	d.sm = sm
	d.getRun(sid).sm = sm

	before := d.systemPromptFor(sid)
	if !strings.Contains(before, "Reply in en") {
		t.Fatalf("fixture: the prompt does not carry the language: %q", before)
	}

	d.cfg.Language = "zh"
	if got := d.systemPromptFor(sid); got != before {
		t.Fatal("expected the memoized prompt to be served — the cache key carries no language")
	}

	row, ok := settingFor("language")
	if !ok || row.Apply == nil {
		t.Fatal("the language row must carry its runtime half")
	}
	if err := row.Apply(d, d.cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := len(d.promptCache); n != 0 {
		t.Errorf("promptCache holds %d entries after the apply, want 0", n)
	}
	if got := d.systemPromptFor(sid); !strings.Contains(got, "Reply in zh") {
		t.Errorf("the prompt after the apply does not carry the new language: %q", got)
	}
}

// --- the schema and the save: P2's backend half ---

// settingsFixture writes a config.yaml into a temp base dir and loads it the way the app
// does. Call it AFTER any session helper: those each point config.SetBaseDir at their own
// temp dir (it is a process global), and this one must be the last to set it.
func settingsFixture(t *testing.T, yamlText string) (*desktopApp, *AgentService, string) {
	t.Helper()
	dir := t.TempDir()
	config.SetBaseDir(dir)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	d := newTestApp()
	d.cfg = cfg
	return d, &AgentService{desk: d}, path
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestSettingsSchemaCoversEveryOfferedRow: the schema is built from the CONFIG's leaves with
// the table as the filter, so the two must agree in both directions — every row that claims to
// be offered shows up, and nothing another entry mode owns ever appears (a switch that does
// nothing is worse than no switch).
func TestSettingsSchemaCoversEveryOfferedRow(t *testing.T) {
	// A provider is in the fixture because the `providers` row's fields are its ELEMENTS: with
	// an empty list there is nothing for it to offer, which is correct (the design does not add
	// or remove providers) and would make the "every row appears" assertion meaningless.
	_, svc, _ := settingsFixture(t, `language: zh
max_tokens: 4096
providers:
  - name: main
    model: claude-x
`)
	schema := svc.GetSettingsSchema()
	if len(schema) < 50 {
		t.Fatalf("schema has %d fields — a config this size offers far more", len(schema))
	}
	byKey := make(map[string]SettingFieldVO, len(schema))
	for _, f := range schema {
		if _, dup := byKey[f.Key]; dup {
			t.Errorf("duplicate field %q", f.Key)
		}
		byKey[f.Key] = f
		if row, ok := settingFor(f.Key); !ok || !row.offered() {
			t.Errorf("%q is in the schema but its row is not offered (mode %s)", f.Key, row.Mode)
		}
	}
	for _, row := range settingsTable {
		if !row.offered() {
			continue
		}
		if !slices.ContainsFunc(schema, func(f SettingFieldVO) bool { return row.covers(f.Key) }) {
			t.Errorf("row %q (mode %s) offers no field in the schema", row.Key, row.Mode)
		}
	}

	// Values come from the live config, kinds from the value's own type, and an UNSET pointer
	// reads as its default — an empty box for "unset, therefore true" would misstate what is in
	// effect.
	if f := byKey["language"]; f.Value != "zh" || f.Kind != "string" || f.NeedsRestart {
		t.Errorf("language = %+v", f)
	}
	if f := byKey["max_tokens"]; f.Value != 4096 || f.Kind != "int" {
		t.Errorf("max_tokens = %+v", f)
	}
	if f := byKey["lsp.enabled"]; f.Value != true || f.Kind != "bool" {
		t.Errorf("lsp.enabled = %+v (an unset *bool must read as its default)", f)
	}
	// One block, two fates: the level is hot (a shared LevelVar reaches every logger), the
	// rotation limits are not (frozen in a rotatingWriter with an open fd) — and the ⟳ badge
	// follows the leaf, not the block.
	if f := byKey["logs.level"]; f.NeedsRestart {
		t.Errorf("logs.level = %+v, want it hot", f)
	}
	if f := byKey["logs.max_files"]; !f.NeedsRestart || f.Note == "" {
		t.Errorf("logs.max_files = %+v (a restart-only leaf must carry ⟳ and a reason)", f)
	}
}

// TestSaveSettingsAppliesAndWrites walks one save end to end: memory, the running process (the
// language row's Apply clears the prompt cache), and the file — in that order, with the file
// last because it is what survives the process.
func TestSaveSettingsAppliesAndWrites(t *testing.T) {
	sm := newSessionManagerForTest(t, t.TempDir()) // sets the base dir; the fixture re-sets it
	sid := sm.Current().ID
	d, svc, path := settingsFixture(t, "language: en\nmax_tokens: 1000\n")
	d.sm = sm
	d.getRun(sid).sm = sm
	_ = d.systemPromptFor(sid) // warm the prompt memo, so the clear is observable
	if len(d.promptCache) == 0 {
		t.Fatal("fixture: the prompt was not memoized")
	}

	msg := svc.SaveSettings(map[string]any{"language": "zh", "max_tokens": float64(2000)})
	if !strings.Contains(msg, "2 项已生效") {
		t.Errorf("summary = %q, want both fields reported live", msg)
	}
	if d.cfg.Language != "zh" || d.cfg.MaxTokens != 2000 {
		t.Errorf("memory: language=%q max_tokens=%d", d.cfg.Language, d.cfg.MaxTokens)
	}
	if n := len(d.promptCache); n != 0 {
		t.Errorf("the language row's Apply did not clear the prompt cache (%d entries)", n)
	}
	file := readFileT(t, path)
	if !strings.Contains(file, "language: zh") || !strings.Contains(file, "max_tokens: 2000") {
		t.Errorf("config.yaml = %q", file)
	}
}

// TestSaveSettingsRefusesBeforeTouchingAnything: a batch with one bad key changes NOTHING —
// not the file, not the memory. Each refusal names its own reason, because "保存失败" alone
// leaves the reader with a form full of values and no idea which one is the problem.
func TestSaveSettingsRefusesBeforeTouchingAnything(t *testing.T) {
	d, svc, path := settingsFixture(t, "language: en\n")
	before := readFileT(t, path)
	tests := []struct {
		name    string
		updates map[string]any
		wantSub string
	}{
		{"unknown key", map[string]any{"nope": "x"}, "未知配置项"},
		{"another entry mode's key", map[string]any{"tui.input_history_limit": float64(5)}, "别的入口模式"},
		{"nil value", map[string]any{"language": nil}, "值不合法"},
		{"wrong field type", map[string]any{"max_tokens": "1000"}, "改不了"},
		{"unreadable key", map[string]any{"providers.3.model": "m"}, "下标"},
	}
	for _, tt := range tests {
		msg := svc.SaveSettings(tt.updates)
		if !strings.Contains(msg, tt.wantSub) {
			t.Errorf("%s: summary = %q, want it to mention %q", tt.name, msg, tt.wantSub)
		}
		if got := readFileT(t, path); got != before {
			t.Errorf("%s: the file changed: %q", tt.name, got)
		}
	}
	if d.cfg.Language != "en" {
		t.Errorf("language = %q after the refusals, want it untouched", d.cfg.Language)
	}
}

// TestSaveSettingsRollsBackWhenTheFileRefusesAKey is the failure the write step can really
// hit: SavePartial only updates keys the FILE has, so a value that lives in memory but not in
// config.yaml is refused — and the change must then be gone from memory too, or the session
// runs on a setting the next restart contradicts.
//
// `review.thinking` is the key on purpose: it has no `default:` tag, so a load leaves it NIL
// (config.LoadFrom runs defaults.Set, which materializes the tagged ones). A nil pointer is
// the case SnapshotLeaves exists for — restoring it through the value a form reads would
// leave an explicit pointer behind instead of an unset field.
func TestSaveSettingsRollsBackWhenTheFileRefusesAKey(t *testing.T) {
	d, svc, path := settingsFixture(t, "language: en\n")
	before := readFileT(t, path)
	if d.cfg.Review.Thinking != nil {
		t.Fatalf("fixture: review.thinking should be unset, got %v", *d.cfg.Review.Thinking)
	}

	msg := svc.SaveSettings(map[string]any{"review.thinking": true})
	if !strings.Contains(msg, "没能写入 config.yaml") || !strings.Contains(msg, "已撤销") {
		t.Fatalf("summary = %q, want a write failure that says the change was undone", msg)
	}
	if d.cfg.Review.Thinking != nil {
		t.Errorf("Review.Thinking = %v after a failed save, want it unset again", *d.cfg.Review.Thinking)
	}
	if got := readFileT(t, path); got != before {
		t.Errorf("the file changed: %q", got)
	}
}

// TestSaveSettingsPushesNothingWhenTheFileRefusesTheKey: the failure path must leave the
// RUNNING process untouched too, not just the config object. A push has no undo — it moves a
// process-wide log level, rebuilds tools, reconnects servers — so it must not run before the
// write that can still fail. Otherwise the answer's 「这次改动已撤销，内存与文件都没变」 would
// be false and the process would sit on values neither the file nor the restored memory has.
func TestSaveSettingsPushesNothingWhenTheFileRefusesTheKey(t *testing.T) {
	beforeLevel := logger.CurrentLevel()
	t.Cleanup(func() { logger.SetLevel(beforeLevel) })
	logger.SetLevel("info")

	// logs.level is NOT in this file, so SavePartial refuses it (adding a key is a separate
	// feature) — exactly the write failure this test needs.
	d, svc, path := settingsFixture(t, "language: en\n")
	fileBefore := readFileT(t, path)

	msg := svc.SaveSettings(map[string]any{"logs.level": "debug"})
	if !strings.Contains(msg, "没能写入 config.yaml") || !strings.Contains(msg, "已撤销") {
		t.Fatalf("summary = %q, want a write failure that says the change was undone", msg)
	}
	if got := logger.CurrentLevel(); got != "info" {
		t.Errorf("log level = %q after a failed save: the push ran before the write that failed", got)
	}
	if d.cfg.Logs.Level == "debug" {
		t.Errorf("Logs.Level = %q after a failed save, want it rolled back", d.cfg.Logs.Level)
	}
	if got := readFileT(t, path); got != fileBefore {
		t.Errorf("the file changed: %q", got)
	}
}

// --- which keys are hot: the fence around the promise ---

// TestTheLikelyEditsStayHot is a fence, not a proof. Every key listed here is one a user can
// reasonably expect to change without restarting the app, and each needs its own code path: a
// reload method, a lazy read, or a cache key that carries the value. If a refactor silently
// makes one of them need a restart again, the failure lands HERE rather than on someone who
// saved a setting, saw nothing happen, and stopped trusting the page.
//
// Adding a key to this list is a promise; removing one is a decision that belongs in the design
// doc's 🔴 table, with its reason.
func TestTheLikelyEditsStayHot(t *testing.T) {
	for _, key := range []string{
		"language", "extra_system_prompt", // → the prompt cache is dropped
		"title_generation", "title_provider", "commit_provider", "run_provider", // → per-agent rebuild
		"providers", // → re-resolved per session, under each session's own provider name
		"provider",  // → lazy: the default for sessions that do not exist yet
		"logs.level",
		"permissions.bash.allow",
		"hooks.enabled",
		"web_search.max_results", "web_fetch.timeout", "tool_result.max_chars",
		"checkpoints.enabled", "checkpoints.max_files", "checkpoints.retain",
		"system_reminder.git_reminder",
		"lsp.enabled", "lsp.max_results", "memory.type", "memory.decay_half_life_days",
		"subagent.worktree", "subagent.max_concurrency",
		"active_mcp_profile", "mcp_tool_search.enabled", "mcp_tool_refresh.interval",
		"compact.auto", "compact.threshold", "oneoff.enabled", "max_tokens",
		"session_cleanup_max_count", "review.max_iterations",
	} {
		row, ok := settingFor(key)
		if !ok {
			t.Errorf("%s has no row in settingsTable", key)
			continue
		}
		if row.needsRestart() {
			t.Errorf("%s needs a restart again (mode %s): %s", key, row.Mode, row.Note)
		}
	}
}

// TestTheRestartOnlyKeysSayWhy is the other half: the keys that genuinely cannot be hot declare
// it WITH a reason, because a page that offers a switch nothing reads (or promises a restart the
// code cannot honour) is worse than one that admits the limit.
func TestTheRestartOnlyKeysSayWhy(t *testing.T) {
	for _, key := range []string{
		"logs.max_size", "logs.max_files", "provider_aliases", "debug.pprof.enabled", "herdr.enabled",
	} {
		row, ok := settingFor(key)
		if !ok {
			t.Errorf("%s has no row", key)
			continue
		}
		if !row.needsRestart() || row.Note == "" {
			t.Errorf("%s: needsRestart=%v note=%q — a restart-only key must say why", key, row.needsRestart(), row.Note)
		}
	}
}

// TestSaveSettingsMakesALogLevelChangeLive walks the one hot key whose effect is visible outside
// any session: the log level is process-wide, so the whole app's logging follows a save. It also
// pins the summary's two halves — the same block has a leaf that CANNOT be hot, and the answer
// has to distinguish them rather than calling the save a success.
func TestSaveSettingsMakesALogLevelChangeLive(t *testing.T) {
	before := logger.CurrentLevel()
	t.Cleanup(func() { logger.SetLevel(before) })

	_, svc, path := settingsFixture(t, "logs:\n  level: info\n  max_files: 3\n")

	msg := svc.SaveSettings(map[string]any{"logs.level": "debug"})
	if !strings.Contains(msg, "1 项已生效") {
		t.Errorf("summary = %q, want the level reported live", msg)
	}
	if got := logger.CurrentLevel(); got != "debug" {
		t.Errorf("logger level = %q after saving logs.level=debug", got)
	}
	if file := readFileT(t, path); !strings.Contains(file, "level: debug") {
		t.Errorf("config.yaml = %q", file)
	}

	msg = svc.SaveSettings(map[string]any{"logs.max_files": float64(5)})
	if !strings.Contains(msg, "需重启后生效") || !strings.Contains(msg, "logs.max_files") {
		t.Errorf("summary = %q, want the rotation limit reported as restart-only, by name", msg)
	}
}
