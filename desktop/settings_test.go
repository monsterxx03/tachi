package main

// The settings table's guard rails (design §八之三 ②).
//
// The classification in settings.go is only worth having if it cannot go stale, so these
// tests hold it to the WHOLE of config.Config instead of a list somebody maintains by hand:
// every yaml leaf must resolve to a row, and every row must match a real leaf. That is what
// turns "somebody has to remember to mark this one 需重启" into a failing build.
//
// The leaf list is derived by reflection rather than written down, because a hand-written
// list is exactly the thing that rots when a field is added — the failure this file exists
// to prevent.

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
)

// yamlLeafPaths returns one dot path per leaf a settings form could name, with "*" standing
// for a slice element or a map key (both are unbounded, and a form never addresses one).
//
// A field tagged `yaml:"-"` is skipped, which is what makes MCPServers (loaded from
// mcp.json, not config.yaml) correctly absent: it is not a key of this file at all.
func yamlLeafPaths(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface {
		if t.Kind() == reflect.Interface {
			*out = append(*out, prefix)
			return
		}
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		yamlLeafPaths(t.Elem(), prefix+".*", out)
	case reflect.Struct:
		if t.PkgPath() != "" && t.Name() == "Time" {
			*out = append(*out, prefix)
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			name := ""
			if tag, ok := f.Tag.Lookup("yaml"); ok {
				name = strings.Split(tag, ",")[0]
				if name == "-" {
					continue
				}
			}
			if name == "" {
				// yaml's own default is the lowercased field name.
				name = strings.ToLower(f.Name)
			}
			child := name
			if prefix != "" {
				child = prefix + "." + name
			}
			yamlLeafPaths(f.Type, child, out)
		}
	default:
		*out = append(*out, prefix)
	}
}

func configLeaves(t *testing.T) []string {
	t.Helper()
	var leaves []string
	yamlLeafPaths(reflect.TypeOf(config.Config{}), "", &leaves)
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

// TestSettingForPrefersTheMostSpecificRow: a leaf must not inherit its block's mode. The
// two rows that actually split are `checkpoints` — its flag is read per call, its three
// limits are baked into a manager — and the prefix match must respect segment boundaries,
// or `provider` would cover `provider_aliases` (a different fact with a different fate).
func TestSettingForPrefersTheMostSpecificRow(t *testing.T) {
	if row, _ := settingFor("checkpoints.enabled"); row.Mode != applyLazy {
		t.Errorf("checkpoints.enabled resolved to %v, want the lazy row", row.Mode)
	}
	if row, _ := settingFor("checkpoints.max_bytes"); row.Mode != applyPush {
		t.Errorf("checkpoints.max_bytes resolved to %v, want the push row", row.Mode)
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
