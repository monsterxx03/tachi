package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one-save-many-edits pipeline: appends, updates on existing elements, and removals, in that
// order, written to config.yaml ONCE.
//
// The ordering rules are what these tests are really about — each one exists because the naive
// order silently edits the wrong thing, and each failure mode is named in the test that pins it.

const editsFixture = `# 我的配置：注释得留着
language: zh
providers:
  - name: main
    type: openai
    model: m0
    api_key: k0
  - name: fast
    type: openai
    model: m1
    api_key: k1
  - name: spare
    type: openai
    model: m2
    api_key: k2
`

func writeEditsFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// One save, one write: an append, an update and a removal all land in a single file image.
//
// This is the property the whole pipeline exists for. Three separate writes would leave two windows
// where the file holds an item whose values have not been written — a config nobody asked for, and
// one the memory half cannot describe. Asserted by counting: the file must be a legal config with
// ALL three edits in it (a partial write would fail to load or be missing an edit).
func TestApplyConfigEditsWritesOneFileForAllThreeKinds(t *testing.T) {
	path := writeEditsFixture(t, editsFixture)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	edits := ListEdits{
		Appends: []ListAppend{{
			List: "providers", Index: 3,
			Values: map[string]any{"name": "added", "model": "m3", "api_key": "k3"},
		}},
		Updates: map[string]any{"providers.0.model": "m0-edited"},
		Removes: []string{"providers.1"},
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}
	if err := ApplyConfigEditsTo(path, edits); err != nil {
		t.Fatalf("ApplyConfigEditsTo: %v", err)
	}

	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "# 我的配置") {
		t.Errorf("the comment was lost:\n%s", body)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this wrote does not load: %v\n%s", err, body)
	}
	names := make([]string, 0, len(got.Providers))
	for _, p := range got.Providers {
		names = append(names, p.Name)
	}
	// main (edited -> still first), fast removed, spare, added.
	if want := []string{"main", "spare", "added"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("providers = %v, want %v", names, want)
	}
	if got.Providers[0].Model != "m0-edited" {
		t.Errorf("providers.0.model = %q, want the update applied", got.Providers[0].Model)
	}
}

// The removal goes LAST, and this is what that rule protects: a removal shifts every later index
// up by one, so an update applied afterwards names a different element than the caller meant.
//
// The fixture makes the two orders disagree on purpose — the update and the removal touch adjacent
// elements, so a pipeline that removed first would write "m0-removed-edited" into `spare`.
func TestApplyConfigEditsUpdatesBeforeRemoving(t *testing.T) {
	path := writeEditsFixture(t, editsFixture)
	edits := ListEdits{
		Updates: map[string]any{"providers.1.model": "fast-edited"},
		Removes: []string{"providers.0"},
	}
	if err := ApplyConfigEditsTo(path, edits); err != nil {
		t.Fatalf("ApplyConfigEditsTo: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	names := make([]string, 0, len(got.Providers))
	for _, p := range got.Providers {
		names = append(names, p.Name)
	}
	if want := []string{"fast", "spare"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("providers = %v, want %v (main removed)", names, want)
	}
	if got.Providers[0].Name != "fast" || got.Providers[0].Model != "fast-edited" {
		t.Errorf("providers[0] = %+v, want the update on `fast` — the edit named index 1 BEFORE the "+
			"removal shifted it to 0", got.Providers[0])
	}
	if got.Providers[1].Name != "spare" || got.Providers[1].Model != "m2" {
		t.Errorf("providers[1] = %+v, want `spare` untouched", got.Providers[1])
	}
}

// An append carries the values of the element it adds, and they land in the element itself.
//
// This is the rule that makes "the new item's fields" one edit rather than two: an update path
// cannot reach an element the file does not have yet, so the values have to travel WITH the append.
func TestApplyConfigEditsAppendCarriesItsValues(t *testing.T) {
	path := writeEditsFixture(t, "language: zh\nweb_search:\n  proxy: http://127.0.0.1:7890\n")
	edits := ListEdits{Appends: []ListAppend{{
		List: "web_search.providers", Index: 0,
		Values: map[string]any{"type": "brave", "key": "sk-brave"},
	}}}
	if err := ApplyConfigEditsTo(path, edits); err != nil {
		t.Fatalf("ApplyConfigEditsTo: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.WebSearch.Providers) != 1 {
		t.Fatalf("web_search.providers = %+v, want one item", got.WebSearch.Providers)
	}
	p := got.WebSearch.Providers[0]
	if p.Type != WebSearchProviderBrave || p.Key != "sk-brave" {
		t.Errorf("the new item = %+v, want the values the caller sent", p)
	}
	if got.WebSearch.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("web_search.proxy = %q, want the block's own scalar untouched", got.WebSearch.Proxy)
	}
}

// A claimed index that disagrees with the list's real length refuses the whole edit — in memory and
// in the file, with nothing changed.
//
// The alternative is writing the values into the wrong element, silently: the caller is working
// from a form whose list has moved under it (another window, a hand edit), and the index it sends
// is the only thing that says which element it means.
func TestApplyConfigEditsRefusesAStaleIndexClaim(t *testing.T) {
	path := writeEditsFixture(t, editsFixture)
	before, _ := os.ReadFile(path)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	edits := ListEdits{Appends: []ListAppend{{
		List: "providers", Index: 9, // the fixture has three
		Values: map[string]any{"name": "added"},
	}}}
	if err := ApplyListEdits(cfg, edits); err == nil {
		t.Error("ApplyListEdits accepted an append claiming index 9 on a list of three")
	}
	if err := ApplyConfigEditsTo(path, edits); err == nil {
		t.Error("ApplyConfigEditsTo accepted an append claiming index 9 on a list of three")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("the file changed despite the refusal:\n%s", after)
	}
	if len(cfg.Providers) != 3 {
		t.Errorf("memory changed despite the refusal: %+v", cfg.Providers)
	}
}

// NoIndexClaim is the escape hatch for a caller that is not coordinating with a form: the element
// still goes to the end, there is simply nothing to check it against.
func TestApplyConfigEditsAppendWithoutAnIndexClaim(t *testing.T) {
	path := writeEditsFixture(t, editsFixture)
	edits := ListEdits{Appends: []ListAppend{{
		List: "providers", Index: NoIndexClaim,
		Values: map[string]any{"name": "added", "model": "m3"},
	}}}
	if err := ApplyConfigEditsTo(path, edits); err != nil {
		t.Fatalf("ApplyConfigEditsTo: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.Providers) != 4 || got.Providers[3].Name != "added" {
		t.Errorf("providers = %+v, want the item appended", got.Providers)
	}
}

// A rollback puts back ALL THREE kinds: a removed element at its own index, an edited leaf to what
// it held, and a grown list to its old length.
//
// The caller's shape is the point — the snapshot is taken before the edit and used when the FILE
// write fails afterwards, so the config object never keeps a change the file refused. The removal
// is in the middle of the batch because that is where its index would be destroyed by a wrong
// restore order.
func TestSnapshotAndRestoreListEdits(t *testing.T) {
	path := writeEditsFixture(t, editsFixture)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	edits := ListEdits{
		Appends: []ListAppend{{List: "providers", Index: 3, Values: map[string]any{"name": "added"}}},
		Updates: map[string]any{"providers.0.model": "m0-edited", "providers.2.api_key": "k2-edited"},
		Removes: []string{"providers.1"},
	}
	snap, err := SnapshotListEdits(cfg, edits)
	if err != nil {
		t.Fatalf("SnapshotListEdits: %v", err)
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}
	// The edit really happened (or the restore below would prove nothing).
	if len(cfg.Providers) != 3 || cfg.Providers[0].Model != "m0-edited" {
		t.Fatalf("the edit did not apply: %+v", cfg.Providers)
	}

	RestoreListEdits(cfg, snap)
	names := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		names = append(names, p.Name)
	}
	if want := "main,fast,spare"; strings.Join(names, ",") != want {
		t.Errorf("providers = %v, want %v (the removed element back at its own index)", names, want)
	}
	if cfg.Providers[0].Model != "m0" {
		t.Errorf("providers.0.model = %q, want the pre-edit value", cfg.Providers[0].Model)
	}
	if cfg.Providers[2].APIKey != "k2" {
		t.Errorf("providers.2.api_key = %q, want the pre-edit value (the element the removal shifted)",
			cfg.Providers[2].APIKey)
	}
}

// A removal that would take the wrong element is refused — verified on a re-parse of the edit
// alone, by the SURVIVORS' identity rather than by counting.
func TestRemoveElementNodeRefusesAnOutOfRangeIndex(t *testing.T) {
	path := writeEditsFixture(t, editsFixture)
	before, _ := os.ReadFile(path)
	edits := ListEdits{Removes: []string{"providers.7"}}
	if err := ApplyConfigEditsTo(path, edits); err == nil {
		t.Error("removing index 7 of a three-element list was accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("the file changed despite the refusal:\n%s", after)
	}
}

// A save whose leaf update names a path the file cannot hold leaves the file byte-for-byte alone:
// the append that came before it must not survive a batch that failed.
//
// `providers.0.spec.proxy` is the path: `spec` is the block the update would have to create, and
// this test's fixture has no `spec` anywhere — the case that used to be refused outright.
func TestApplyConfigEditsLeavesNoPartialEditWhenOneFails(t *testing.T) {
	path := writeEditsFixture(t, "language: zh\nproviders:\n  - name: main\n    model: m0\n")
	before, _ := os.ReadFile(path)
	edits := ListEdits{
		Appends: []ListAppend{{List: "providers", Index: 1, Values: map[string]any{"name": "added"}}},
		// A path through a SCALAR: there is no block to put this key in, and that refusal is the
		// one thing an update cannot work around.
		Updates: map[string]any{"language.foo": "x"},
	}
	if err := ApplyConfigEditsTo(path, edits); err == nil {
		t.Fatal("an update through a scalar was accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("the file changed despite the failed batch:\n%s", after)
	}
}

// A new element may carry an 自动 leaf (config.Unset), and the file must not spell it out.
//
// The optional leaves are exactly the ones a form can say 自动 about, and their yaml tags are
// `omitempty` — so a nil pointer is left out of the element altogether and reading the file back
// gives nil, which is what was asked for. This pins BOTH halves of that: the element the file holds
// (no `vision` key at all) and the element the config object holds.
//
// The updates are the shape the real caller sends: every leaf of the new element, keyed by its full
// path, alongside the append that carries them (see desktop's pendingAppends). The ones inside the
// new element must be dropped by updatesOutsideAppends — a path the config does not have yet is one
// an update cannot reach.
func TestApplyConfigEditsAppendWithAnUnsetLeaf(t *testing.T) {
	path := writeEditsFixture(t, "language: zh\nproviders:\n  - name: main\n    model: m\n")
	values := map[string]any{
		"name": "added", "spec.vision": Unset{}, "spec.proxy": "http://127.0.0.1:7890", "spec.max_retries": 3,
	}
	updates := map[string]any{}
	for tail, v := range values {
		updates["providers.1."+tail] = v
	}
	edits := ListEdits{
		Appends: []ListAppend{{List: "providers", Index: 1, Values: values}},
		Updates: updates,
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := SnapshotListEdits(cfg, edits)
	if err != nil {
		t.Fatalf("SnapshotListEdits: %v", err)
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}
	if err := ApplyConfigEditsTo(path, edits); err != nil {
		t.Fatalf("ApplyConfigEditsTo: %v", err)
	}

	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "vision") {
		t.Errorf("an unset optional leaf must not be spelled out in the new element:\n%s", body)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this wrote does not load: %v\n%s", err, body)
	}
	spec := got.Providers[1].Spec
	if spec.Vision != nil {
		t.Errorf("vision = %v, want nil (自动)", *spec.Vision)
	}
	if spec.Proxy != "http://127.0.0.1:7890" || spec.MaxRetries == nil || *spec.MaxRetries != 3 {
		t.Errorf("the other values did not land: %+v", spec)
	}
	// The memory half agrees with the file — the two must not disagree about what was written.
	mem := cfg.Providers[1].Spec
	if mem.Vision != nil || mem.Proxy != spec.Proxy || mem.MaxRetries == nil || *mem.MaxRetries != 3 {
		t.Errorf("memory = %+v, file = %+v", mem, spec)
	}
	// …and the rollback drops the whole element, so there is nothing to put back field by field.
	RestoreListEdits(cfg, snap)
	if len(cfg.Providers) != 1 {
		t.Errorf("providers after the rollback = %+v, want the new element gone", cfg.Providers)
	}
}

// Paths() is the set the caller pushes at the running process and counts in its summary: the
// updates, plus every leaf a new element carries. A removal contributes nothing — there is no leaf
// left to name.
func TestListEditsPathsNameEveryWrittenLeaf(t *testing.T) {
	edits := ListEdits{
		Updates: map[string]any{"providers.0.model": "m"},
		Appends: []ListAppend{{
			List: "web_search.providers", Index: 2,
			Values: map[string]any{"type": "brave", "key": "k"},
		}},
		Removes: []string{"providers.1"},
	}
	got := edits.Paths()
	want := map[string]bool{
		"providers.0.model":           true,
		"web_search.providers.2.type": true,
		"web_search.providers.2.key":  true,
	}
	if len(got) != len(want) {
		t.Fatalf("Paths() = %v, want %d entries", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("Paths() includes %q, which is not a leaf this edit writes", p)
		}
	}
}
