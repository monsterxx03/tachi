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
	"time"

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
		// A row that OWNS its write is the only writer of that leaf (the batch skips it), so it
		// must be a push WITH code — otherwise nothing would ever apply the value.
		if row.OwnsWrite && (row.Mode != applyPush || row.Apply == nil) {
			t.Errorf("row %q: OwnsWrite with mode %s and Apply set: %v — a row that owns its write "+
				"must have the code that does it", row.Key, row.Mode, row.Apply != nil)
		}
		// An unused key is the one kind the form must not offer: a switch nothing reads is
		// worse than no switch.
		if row.offered() == (row.Mode == applyUnused) {
			t.Errorf("row %q: offered() = %v for mode %s", row.Key, row.offered(), row.Mode)
		}
		// The form's left column. A row that IS offered must name its group, and a row that is
		// not must not: an unoffered key belongs to another entry mode, and a group for a field
		// no form draws is a claim about a page that never exists. Requiring them to match
		// offered() in both directions is what keeps Category a claim about THIS form rather
		// than a decoration.
		if row.offered() && row.Category == "" {
			t.Errorf("row %q is offered but names no Category: the settings page has nowhere to put it", row.Key)
		}
		if !row.offered() && row.Category != "" {
			t.Errorf("row %q is %s (never offered) but carries Category %q", row.Key, row.Mode, row.Category)
		}
		if row.Category != "" && !slices.Contains(settingsCategoryOrder, row.Category) {
			t.Errorf("row %q: Category %q is not in settingsCategoryOrder, so the nav would never draw it",
				row.Key, row.Category)
		}
	}

	// A row that overrides another row's key must share its group. `logs.level` is the one case
	// today (it is a more specific row than `logs`), and a disagreement would put two halves of
	// one block on two pages — the reader would have no way to tell why 日志级别 is not sitting
	// with the other log keys.
	for _, a := range settingsTable {
		for _, b := range settingsTable {
			if a.Key == b.Key || !a.covers(b.Key) {
				continue
			}
			if a.Category != b.Category {
				t.Errorf("rows %q and %q override one another but disagree on the group (%q vs %q)",
					a.Key, b.Key, a.Category, b.Category)
			}
		}
	}

	// The other direction: a declared group with no row is a page the nav offers and the form
	// cannot fill.
	for _, c := range settingsCategoryOrder {
		if !slices.ContainsFunc(settingsTable, func(r settingApply) bool { return r.Category == c }) {
			t.Errorf("category %q has no row — the nav would draw an empty page", c)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(settingsCategoryOrder)))) != len(settingsCategoryOrder) {
		t.Errorf("settingsCategoryOrder repeats a group: %v", settingsCategoryOrder)
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
	if row, _ := settingFor("logs.max_size"); row.Mode != applyRestart {
		t.Errorf("logs.max_size resolved to %v, want the block's restart row", row.Mode)
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

// makeConfigWriteFail makes the directory config.yaml lives in read-only, so the atomic write at
// the end of a save is the step that fails. It is the write failure a real user hits (a read-only
// home, a full disk) and — now that SavePartial creates the blocks a path names — the only one
// left that a save can run into at all, which is why the rollback and push-ordering tests below
// induce it this way instead of naming a key the file refuses.
//
// The directory (not the file) is what changes: 0500 still allows the READ that SavePartial starts
// with, and creating the tmp file is what gets refused. Not root-proof: a superuser bypasses the
// permission, and a test run as root would see the save succeed.
func makeConfigWriteFail(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	// Restored BEFORE t.TempDir's own cleanup (cleanups run last-registered-first), or that
	// cleanup cannot remove a directory it may not write to.
	t.Cleanup(func() { _ = os.Chmod(dir, info.Mode().Perm()) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
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
	if schema == nil {
		t.Fatal("GetSettingsSchema returned nil for a loaded config")
	}
	if len(schema.Fields) < 50 {
		t.Fatalf("schema has %d fields — a config this size offers far more", len(schema.Fields))
	}
	byKey := make(map[string]SettingFieldVO, len(schema.Fields))
	for _, f := range schema.Fields {
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
		if !slices.ContainsFunc(schema.Fields, func(f SettingFieldVO) bool { return row.covers(f.Key) }) {
			t.Errorf("row %q (mode %s) offers no field in the schema", row.Key, row.Mode)
		}
	}

	// The groups ride along with the fields: the page's nav IS Categories, so a field whose
	// group is not on that list is a field the page would silently drop, and a group with no
	// field is a nav entry that opens onto nothing.
	if !slices.Equal(schema.Categories, settingsCategoryOrder) {
		t.Errorf("categories = %v, want %v", schema.Categories, settingsCategoryOrder)
	}
	for _, f := range schema.Fields {
		if !slices.Contains(schema.Categories, f.Category) {
			t.Errorf("field %q is in group %q, which the nav does not list", f.Key, f.Category)
		}
	}
	for _, c := range schema.Categories {
		if !slices.ContainsFunc(schema.Fields, func(f SettingFieldVO) bool { return f.Category == c }) {
			t.Errorf("group %q has no field in this config", c)
		}
	}
	// Two placements the dot path does NOT predict, which is the whole reason the group is on
	// the row: the level sits with the rest of the log keys (it is a more specific row, and
	// overriding must not move a leaf to another page), and the reminders sit with the session
	// machinery rather than under anything named 提醒.
	if f := byKey["logs.level"]; f.Category != catAdvanced {
		t.Errorf("logs.level is in %q, want %q (same group as the row it overrides)", f.Category, catAdvanced)
	}
	if f := byKey["system_reminder.git_reminder"]; f.Category != catSession {
		t.Errorf("system_reminder.git_reminder is in %q, want %q", f.Category, catSession)
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

	res := svc.SaveSettings(map[string]any{"language": "zh", "max_tokens": float64(2000)})
	if !res.OK {
		t.Errorf("a save that reached the file reported OK = false: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "2 项已生效") {
		t.Errorf("summary = %q, want both fields reported live", res.Summary)
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
		res := svc.SaveSettings(tt.updates)
		// The page keeps the reader's typing on a refusal and clears it on a success, so the
		// flag has to be right even when the sentence happens to read like a failure anyway.
		if res.OK {
			t.Errorf("%s: reported OK = true for a batch that was refused", tt.name)
		}
		if !strings.Contains(res.Summary, tt.wantSub) {
			t.Errorf("%s: summary = %q, want it to mention %q", tt.name, res.Summary, tt.wantSub)
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
// hit: the write itself fails (a read-only home, a full disk), and the change must then be gone
// from memory too, or the session runs on a setting the next restart contradicts.
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
	makeConfigWriteFail(t, path)

	res := svc.SaveSettings(map[string]any{"review.thinking": true})
	if res.OK {
		t.Error("a batch whose write failed reported OK = true")
	}
	if !strings.Contains(res.Summary, "没能写入 config.yaml") || !strings.Contains(res.Summary, "已撤销") {
		t.Fatalf("summary = %q, want a write failure that says the change was undone", res.Summary)
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

	// The write fails while `logs.level` DOES have a push — exactly the ordering this pins.
	d, svc, path := settingsFixture(t, "language: en\n")
	fileBefore := readFileT(t, path)
	makeConfigWriteFail(t, path)

	res := svc.SaveSettings(map[string]any{"logs.level": "debug"})
	if res.OK {
		t.Error("a batch whose write failed reported OK = true")
	}
	if !strings.Contains(res.Summary, "没能写入 config.yaml") || !strings.Contains(res.Summary, "已撤销") {
		t.Fatalf("summary = %q, want a write failure that says the change was undone", res.Summary)
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

	res := svc.SaveSettings(map[string]any{"logs.level": "debug"})
	if !res.OK {
		t.Errorf("summary = %q, want a save that landed", res.Summary)
	}
	if !strings.Contains(res.Summary, "1 项已生效") {
		t.Errorf("summary = %q, want the level reported live", res.Summary)
	}
	if got := logger.CurrentLevel(); got != "debug" {
		t.Errorf("logger level = %q after saving logs.level=debug", got)
	}
	if file := readFileT(t, path); !strings.Contains(file, "level: debug") {
		t.Errorf("config.yaml = %q", file)
	}

	res = svc.SaveSettings(map[string]any{"logs.max_files": float64(5)})
	if !res.OK {
		t.Errorf("summary = %q, want the limit written even though only a restart makes it live", res.Summary)
	}
	if !strings.Contains(res.Summary, "需重启后生效") || !strings.Contains(res.Summary, "logs.max_files") {
		t.Errorf("summary = %q, want the rotation limit reported as restart-only, by name", res.Summary)
	}
}

// TestTheFormSeesAndSavesWhatTheFileHolds: a form does not send Go values, it sends what its
// inputs hold — a list as []any, a duration as the string in the box — and neither shape was
// reachable before the settings page existed. The write path had to be taught both (see
// config's renderScalar and assignValue); this is the test that says the pair works from the
// page's side, including what the page is SHOWN to begin with.
func TestTheFormSeesAndSavesWhatTheFileHolds(t *testing.T) {
	d, svc, path := settingsFixture(t, `permissions:
  bash:
    allow: ["ls"]
lsp:
  request_timeout: 15s
memory:
  timeout: 90s
`)
	byKey := map[string]SettingFieldVO{}
	for _, f := range svc.GetSettingsSchema().Fields {
		byKey[f.Key] = f
	}
	// The box shows the file's own form, not nanoseconds: a duration leaf is a plain string as
	// far as the form is concerned, which is why it needs no Kind of its own — and why `timeout:
	// 30s` in config.yaml stays `timeout: 30s` after an edit. Both duration types are here,
	// because they used to answer differently.
	for key, want := range map[string]string{
		"lsp.request_timeout": "15s",   // config.Duration, which marshals as a string
		"memory.timeout":      "1m30s", // time.Duration, which has no marshaller of its own
	} {
		if f := byKey[key]; f.Kind != "string" || f.Value != want {
			t.Errorf("%s = %+v, want kind \"string\" and value %q", key, f, want)
		}
	}
	if f := byKey["permissions.bash.allow"]; f.Kind != "string_list" {
		t.Errorf("permissions.bash.allow = %+v, want a string_list", f)
	}

	res := svc.SaveSettings(map[string]any{
		"permissions.bash.allow": []any{"ls", "pwd"}, // an array, as the bridge delivers one
		"lsp.request_timeout":    "45s",              // the text the box held
	})
	if !res.OK {
		t.Fatalf("save: ok=%v summary=%s", res.OK, res.Summary)
	}
	if got := time.Duration(d.cfg.LSP.RequestTimeout); got != 45*time.Second {
		t.Errorf("LSP.RequestTimeout = %v in memory, want 45s", got)
	}
	file := readFileT(t, path)
	if !strings.Contains(file, "45s") || !strings.Contains(file, "pwd") {
		t.Errorf("config.yaml = %q, want the duration in its written form and the new list", file)
	}
	if !strings.Contains(file, "allow: [ls, pwd]") {
		t.Errorf("config.yaml = %q, want the list rendered as a list", file)
	}
}

// TestSettingChoicesNameRealLeaves: the choices table's two failure modes. A key that is not a
// config leaf (a typo, or a field that was renamed) would offer a dropdown nobody ever sees, and
// a key on a row that is not offered would be dead code that looks alive.
//
// The leaves come from config.YAMLPaths — the same walker the schema is built on — so "is this a
// real key" is answered by the config, not by a second list here.
func TestSettingChoicesNameRealLeaves(t *testing.T) {
	leaves := make(map[string]bool)
	for _, l := range configLeaves(t) {
		leaves[l] = true
	}
	for key := range settingChoices {
		if !leaves[key] {
			t.Errorf("settingChoices names %q, which is not a leaf of config.Config", key)
		}
		row, ok := settingFor(key)
		if !ok || !row.offered() {
			t.Errorf("settingChoices names %q, whose row is not offered (mode %s): the dropdown "+
				"would never be drawn", key, row.Mode)
		}
	}
}

// TestSettingFoldedListsNameRealLists: settingFoldedLists is a DECLARATION about the config, so
// its two failure modes are the same shape as settingChoices': a path that is not a list of
// config.Config (a typo, or a field that was renamed) would fold nothing at all while looking like
// a decision, and a path whose row is not offered would fold a card that is never drawn.
//
// The membership check is against config.ListItemPaths — the same walker that decides which lists
// the page may grow — so "is this a real list" is answered by the config rather than by a second
// list here, and a declaration naming a list the page cannot draw (a []string, a map) fails too.
func TestSettingFoldedListsNameRealLists(t *testing.T) {
	lists := config.ListItemPaths()
	for _, path := range settingFoldedLists {
		if !slices.Contains(lists, path) {
			t.Errorf("settingFoldedLists names %q, which is not a list of config.Config "+
				"(config.ListItemPaths: %v)", path, lists)
		}
	}
	// And it must actually reach the page: the item's fold is decided from the schema, so a
	// declaration the schema does not carry is a rule nobody follows.
	_, svc, _ := settingsFixture(t, `providers:
  - name: main
    model: m
web_search:
  providers:
    - type: exa
`)
	byPath := map[string]SettingListVO{}
	for _, l := range svc.GetSettingsSchema().Lists {
		byPath[l.Path] = l
	}
	for _, path := range settingFoldedLists {
		if !byPath[path].Folded {
			t.Errorf("the schema's %s list is not marked folded: %+v", path, byPath[path])
		}
	}
	// The other direction, so `Folded` cannot become a field that is simply always true: a list
	// a reader SCANS keeps the size rule.
	if byPath["providers"].Folded {
		t.Errorf("providers is marked folded, but its items are what the size rule is for: %+v",
			byPath["providers"])
	}
}

// TestProviderChoicesComeFromTheConfig: the list is what the config DEFINES, so it cannot go
// stale the way a hard-coded one would. Two entries sharing a name collapse to one choice (they
// are one choice to a reader), and a config that defines no provider yields no options at all —
// which the page renders as a text box, since a dropdown with nothing in it is a trap.
func TestProviderChoicesComeFromTheConfig(t *testing.T) {
	_, svc, _ := settingsFixture(t, `provider: main
providers:
  - name: main
    model: claude-x
  - name: fast
    model: haiku
  - name: main
    model: duplicate-name
`)
	byKey := map[string]SettingFieldVO{}
	for _, f := range svc.GetSettingsSchema().Fields {
		byKey[f.Key] = f
	}
	want := []string{"main", "fast"}
	// The keys that NAME a provider, including the ones reached through a block row (subagent
	// and memory speak for eleven leaves each; only their `provider` leaf is a choice).
	for _, key := range []string{
		"provider", "title_provider", "commit_provider", "run_provider",
		"subagent.provider", "review.provider", "memory.keyword_provider",
	} {
		f, ok := byKey[key]
		if !ok {
			t.Errorf("%s is not in the schema at all", key)
			continue
		}
		if !slices.Equal(f.Options, want) {
			t.Errorf("%s options = %v, want %v", key, f.Options, want)
		}
	}

	// Plenty of string leaves are NOT choices — the definitions themselves (which PROVIDE the
	// names) and every plain string. A dropdown on those would be a way to save a value the
	// field does not mean.
	for _, key := range []string{"providers.0.name", "providers.0.model", "language", "subagent.worktree_branch"} {
		if f, ok := byKey[key]; ok && len(f.Options) != 0 {
			t.Errorf("%s carries options %v — it does not name a provider", key, f.Options)
		}
	}

	// No providers defined → no choices (not an empty dropdown).
	_, bare, _ := settingsFixture(t, "language: en\n")
	for _, f := range bare.GetSettingsSchema().Fields {
		if f.Key == "provider" || f.Key == "title_provider" {
			if len(f.Options) != 0 {
				t.Errorf("%s options = %v with no providers defined, want none", f.Key, f.Options)
			}
		}
	}
}

// TestProviderTypeOptionsAreAClosedSet: a provider's `type` is one of three strings the code can
// construct, and NOTHING validates it at load time — a typo parses fine and fails later, when a
// session resolves its provider and llm reaches its default branch. So the form presents the
// choice. The list comes from config's own constants, not from a list here.
//
// It is also the one choice declared on a leaf inside a LIST (`providers.*.type`): the item is
// the same decision however many providers the config holds, so the key is config's structural
// path and the lookup has to expand the index.
func TestProviderTypeOptionsAreAClosedSet(t *testing.T) {
	_, svc, _ := settingsFixture(t, `providers:
  - name: main
    model: m
  - name: fast
    model: f
`)
	byKey := map[string]SettingFieldVO{}
	for _, f := range svc.GetSettingsSchema().Fields {
		byKey[f.Key] = f
	}
	want := config.ProviderTypes()
	if len(want) != 3 {
		t.Fatalf("config.ProviderTypes() = %v, want the three the code constructs", want)
	}
	for _, key := range []string{"providers.0.type", "providers.1.type"} {
		if got := byKey[key].Options; !slices.Equal(got, want) {
			t.Errorf("%s options = %v, want %v", key, got, want)
		}
	}
	// A DIFFERENT list's `type` is a different vocabulary (web search backends), and the
	// structural key must not leak across: `providers.*.type` is two segments deep, and
	// `web_search.providers.0.type` is four.
	for _, f := range svc.GetSettingsSchema().Fields {
		if strings.HasPrefix(f.Key, "web_search.providers.") && strings.HasSuffix(f.Key, ".type") {
			if slices.Contains(f.Options, config.ProviderTypeAnthropic) {
				t.Errorf("%s carries provider types: %v", f.Key, f.Options)
			}
		}
	}
}

// TestSaveSettingsProviderProxy: a new spec leaf needs no desktop code — the form walks the
// config's own leaves and the `providers` row speaks for the whole block — so this pins the two
// things that would silently STOP being true: the field is drawn as an ordinary string (the
// proxy's empty value already means "no explicit proxy", so unlike vision/max_retries it must not
// be offered as 自动), and an edit reaches memory, the file, and the provider re-resolve the row
// pushes.
//
// The fixture's provider has NO `spec:` block at all: that is the case the settings page used to
// refuse outright ("not in the file, and its parent is not either"), and the one a hand-written
// provider actually has, since `omitempty` keeps a default-less block out of the file.
func TestSaveSettingsProviderProxy(t *testing.T) {
	d, svc, path := settingsFixture(t, `providers:
  - name: main
    type: openai
    model: m
    api_key: sk-test
`)
	byKey := map[string]SettingFieldVO{}
	for _, f := range svc.GetSettingsSchema().Fields {
		byKey[f.Key] = f
	}
	f, ok := byKey["providers.0.spec.proxy"]
	if !ok {
		t.Fatal("providers.0.spec.proxy is not offered — the providers row no longer covers the spec block")
	}
	if f.Kind != "string" || f.Optional || !f.IsSet || f.Category != "模型" {
		t.Errorf("providers.0.spec.proxy = %+v, want a plain string field in 模型", f)
	}

	res := svc.SaveSettings(map[string]any{"providers.0.spec.proxy": "http://127.0.0.1:7890"})
	if !res.OK {
		t.Fatalf("save: ok=%v summary=%s", res.OK, res.Summary)
	}
	if got := d.cfg.Providers[0].Spec.Proxy; got != "http://127.0.0.1:7890" {
		t.Errorf("Spec.Proxy in memory = %q, want the saved URL", got)
	}
	// Read the file back rather than matching its text: the writer decides how
	// a URL is quoted, the loader is what has to understand it.
	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := reloaded.Providers[0].Spec.Proxy; got != "http://127.0.0.1:7890" {
		t.Errorf("Spec.Proxy after reload = %q, want the saved URL (file: %q)", got, readFileT(t, path))
	}
}

// TestSaveSettingsUnsetsAnOptionalLeaf: the form's 自动 sends a null, and that has to reach the
// field as "unset" in memory, as `null` in the file, and back as unset when the page re-reads it.
// The three fields it exists for are the ones whose nil MEANS something (`vision: nil` = decide
// it from the model).
func TestSaveSettingsUnsetsAnOptionalLeaf(t *testing.T) {
	d, svc, path := settingsFixture(t, `providers:
  - name: main
    model: m
    spec:
      vision: true
      max_retries: 3
`)
	byKey := func() map[string]SettingFieldVO {
		out := map[string]SettingFieldVO{}
		for _, f := range svc.GetSettingsSchema().Fields {
			out[f.Key] = f
		}
		return out
	}
	// What the form draws: a three-state switch, currently 开.
	for _, key := range []string{"providers.0.spec.vision", "providers.0.spec.max_retries"} {
		f := byKey()[key]
		if !f.Optional || !f.IsSet {
			t.Errorf("%s = %+v, want an optional leaf that is currently set", key, f)
		}
	}
	// A pointer WITH a default is not optional in this sense: the default says what unset means,
	// so there is no third state to draw.
	if f := byKey()["lsp.enabled"]; f.Optional {
		t.Error("lsp.enabled is `*bool default:\"true\"` — unset and true are one fact, not two states")
	}

	res := svc.SaveSettings(map[string]any{"providers.0.spec.vision": nil, "providers.0.spec.max_retries": nil})
	if !res.OK {
		t.Fatalf("save: ok=%v summary=%s", res.OK, res.Summary)
	}
	if got := d.cfg.Providers[0].Spec.Vision; got != nil {
		t.Errorf("vision = %v in memory after 自动, want unset", *got)
	}
	if got := d.cfg.Providers[0].Spec.MaxRetries; got != nil {
		t.Errorf("max_retries = %v in memory after 自动, want unset", *got)
	}
	file := readFileT(t, path)
	if !strings.Contains(file, "vision: null") || !strings.Contains(file, "max_retries: null") {
		t.Errorf("config.yaml = %q, want both written as null", file)
	}
	if f := byKey()["providers.0.spec.vision"]; f.IsSet || f.Value != nil {
		t.Errorf("vision re-read as %+v, want it shown as 自动 again", f)
	}

	// And a nil for a leaf that has no unset state is still refused: the type check is what
	// catches a caller who sent one by mistake.
	res = svc.SaveSettings(map[string]any{"language": nil})
	if res.OK {
		t.Errorf("a nil for a leaf that cannot be unset was accepted: %s", res.Summary)
	}
}

// TestSettingsItemAddAndRemove walks the two methods the page's ＋ / 删除 use.
//
// The add WRITES IMMEDIATELY, so what it must be held to is: the item is in the file as well as in
// memory, the file still loads, and the item is then editable through the ordinary save (the
// fields its skeleton left out are added by SavePartial's add-a-key rule). The remove must take
// the item it was asked for and WARN about the keys still naming it.
func TestSettingsItemAddAndRemove(t *testing.T) {
	d, svc, path := settingsFixture(t, `provider: main
providers:
  - name: main
    type: openai
    model: m
    base_url: http://x/v1
    api_key: k
title_provider: main
`)
	// The schema declares what the page may grow — including a list with no items at all, which is
	// the case the fields alone cannot express.
	schema := svc.GetSettingsSchema()
	var providersList *SettingListVO
	for i := range schema.Lists {
		if schema.Lists[i].Path == "providers" {
			providersList = &schema.Lists[i]
		}
	}
	if providersList == nil {
		t.Fatalf("the schema does not offer the providers list: %+v", schema.Lists)
	}
	if providersList.Category != catModel {
		t.Errorf("providers is drawn in %q, want %q", providersList.Category, catModel)
	}

	added := svc.AddSettingsItem("providers")
	if !added.OK || added.ElementPath != "providers.1" {
		t.Fatalf("add = %+v, want it to name the new item", added)
	}
	if !strings.Contains(added.Summary, "providers.1") {
		t.Errorf("add summary = %q, want it to name the new item", added.Summary)
	}
	// NOTHING is written yet: the item is staged for the next save. That is the property this
	// method no longer has to satisfy (an item used to be written on the spot so its fields could be
	// saved at all), and leaving the page without saving must leave the file alone.
	if len(d.cfg.Providers) != 1 {
		t.Errorf("memory after the add: %+v, want the staged item NOT in the config yet", d.cfg.Providers)
	}
	if body := readFileT(t, path); strings.Contains(body, "name: \"\"") {
		t.Errorf("config.yaml already holds the new item — the add wrote it:\n%s", body)
	}

	// The staged item is EDITABLE at once, which is what makes staging workable: it has no leaves in
	// the config (it is not in the config), so the schema declares them from the element's type.
	schema = svc.GetSettingsSchema()
	staged := map[string]SettingFieldVO{}
	for _, f := range schema.Fields {
		if strings.HasPrefix(f.Key, "providers.1.") {
			staged[f.Key] = f
		}
	}
	if len(staged) == 0 {
		t.Fatalf("the schema draws no field for the staged item — the card would be empty")
	}
	if f, ok := staged["providers.1.name"]; !ok || f.Kind != "string" || f.Category != catModel {
		t.Errorf("providers.1.name = %+v (offered=%v), want a string field in %s", f, ok, catModel)
	}

	// ONE save carries the append and the values together — including a leaf the skeleton does not
	// spell out (`spec.context_window`, whose block the write creates).
	res := svc.SaveSettings(map[string]any{
		"providers.1.name":                "added",
		"providers.1.spec.context_window": 100000,
	})
	if !res.OK {
		t.Fatalf("saving the staged item: ok=%v summary=%s", res.OK, res.Summary)
	}
	final, err := config.LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this save wrote does not load: %v\n%s", err, readFileT(t, path))
	}
	if len(final.Providers) != 2 {
		t.Fatalf("providers after reload = %d, want 2", len(final.Providers))
	}
	if final.Providers[1].Name != "added" || final.Providers[1].Spec.ContextWindow == nil ||
		*final.Providers[1].Spec.ContextWindow != 100000 {
		t.Errorf("the new item did not take its values: %+v", final.Providers[1])
	}
	// The staged mark is consumed by the save: a second look at the schema describes the element from
	// the CONFIG (it is in it now), so no field carries the template's tell — a leaf the scaffold has
	// no value for while the element it describes does not exist. A `spec.proxy` on a real element is
	// a plain empty string; on a staged one it came from the template with the same shape, so the
	// check is the one thing only a real element can show: its values are READ, not scaffolded.
	schema = svc.GetSettingsSchema()
	var sawProxy bool
	for _, f := range schema.Fields {
		if f.Key != "providers.1.spec.proxy" {
			continue
		}
		sawProxy = true
		if f.Note != "" && strings.Contains(f.Note, "读取失败") {
			t.Errorf("the element is still described as missing after the save: %+v", f)
		}
	}
	if !sawProxy {
		t.Errorf("providers.1.spec.proxy is not offered after the save — the element is not in the schema")
	}

	// An item that is still only STAGED is cancelled, not scheduled for removal: it was never in the
	// config, and validating its index against the config would refuse — the path a reader takes
	// most often is "press ＋, look at the card, change my mind".
	if got := svc.AddSettingsItem("providers"); !got.OK {
		t.Fatalf("staging an item to cancel: %+v", got)
	}
	cancelled := svc.RemoveSettingsItem("providers.2")
	if !cancelled.OK {
		t.Fatalf("cancelling a staged item = %+v, want it to succeed", cancelled)
	}
	if !strings.Contains(cancelled.Summary, "取消") {
		t.Errorf("cancel summary = %q, want it to say the item was cancelled", cancelled.Summary)
	}
	if len(d.pendingAdds) != 0 || len(d.pendingRemoves) != 0 {
		t.Errorf("after the cancel: adds=%v removes=%v, want neither", d.pendingAdds, d.pendingRemoves)
	}

	// Removing reports what still points at it: `title_provider` names the item that just went.
	removed := svc.RemoveSettingsItem("providers.0")
	if !removed.OK {
		t.Fatalf("remove = %+v", removed)
	}
	if !strings.Contains(removed.Summary, "title_provider") {
		t.Errorf("remove summary = %q, want it to warn about the key still naming it", removed.Summary)
	}
	// Staged, not done: the item is still in the config and still in the file.
	if len(d.cfg.Providers) != 2 {
		t.Errorf("memory after the remove: %+v, want the item NOT removed yet", d.cfg.Providers)
	}
	if body := readFileT(t, path); !strings.Contains(body, "name: main") {
		t.Errorf("config.yaml lost the item before the save:\n%s", body)
	}
	// The save is what performs it, and it is one write for the removal and the edits around it.
	if res := svc.SaveSettings(map[string]any{"providers.1.model": "m-edited"}); !res.OK {
		t.Fatalf("saving the removal: ok=%v summary=%s", res.OK, res.Summary)
	}
	reloaded, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Providers) != 1 || reloaded.Providers[0].Name != "added" {
		t.Fatalf("providers = %+v, want only the added one", reloaded.Providers)
	}
	if reloaded.Providers[0].Model != "m-edited" {
		t.Errorf("the edit in the same save did not land: %+v", reloaded.Providers[0])
	}
	if strings.Contains(readFileT(t, path), "name: main") {
		t.Errorf("config.yaml still holds the removed item:\n%s", readFileT(t, path))
	}

	// A staged item that is dropped before the save cancels out: nothing is written for either
	// half, which is what "point保存后才生效" means in the other direction.
	if got := svc.AddSettingsItem("providers"); !got.OK {
		t.Fatalf("re-adding: %+v", got)
	}
	if got := svc.RemoveSettingsItem("providers.1"); !got.OK {
		t.Fatalf("removing the item just added: %+v", got)
	}
	if res := svc.SaveSettings(map[string]any{"providers.0.model": "m-again"}); !res.OK {
		t.Fatalf("saving after add-then-remove: ok=%v summary=%s", res.OK, res.Summary)
	}
	after, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Providers) != 1 {
		t.Errorf("providers = %+v, want the add-then-remove to have cancelled out", after.Providers)
	}

	// The refusals: a list that is not a list of items, and an item that is not there.
	if got := svc.AddSettingsItem("review.allowed_tools"); got.OK || !strings.Contains(got.Summary, "不是可以添加条目") {
		t.Errorf("adding to a slice of scalars = %+v, want a refusal", got)
	}
	if got := svc.RemoveSettingsItem("providers.9"); got.OK || !strings.Contains(got.Summary, "删不了") {
		t.Errorf("removing a missing item = %+v, want a refusal", got)
	}
	// A staged add whose index no longer matches the list refuses the whole save: the form is
	// working from a config that moved, and writing its values into another element would be silent.
	if got := svc.AddSettingsItem("providers"); !got.OK {
		t.Fatalf("staging for the stale-index case: %+v", got)
	}
	// Simulate another writer shrinking the list by hand: the staged index now points past the end.
	d.withConfig(func(c *config.Config) { c.Providers = c.Providers[:0] })
	res = svc.SaveSettings(map[string]any{"providers.1.name": "x"})
	if res.OK {
		t.Errorf("a save whose staged index no longer matches was accepted: %s", res.Summary)
	}
	if len(svc.desk.pendingAdds) == 0 {
		t.Errorf("the refusal consumed the staged add — the reader cannot retry")
	}
}
