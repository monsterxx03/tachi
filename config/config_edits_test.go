package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The structural half of a save: appends, updates on existing elements, and removals, applied to
// the in-memory config in that order and then written by ONE whole-config Save.
//
// The ordering rules are what these tests are really about — each one exists because the naive
// order silently edits the wrong thing, and each failure mode is named in the test that pins it.

const editsFixture = `language: zh
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

// loadEditsFixture is the object a form is looking at: the file, loaded.
func loadEditsFixture(t *testing.T, body string) (*Config, string) {
	t.Helper()
	path := writeEditsFixture(t, body)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func providerNamesOf(cfg *Config) []string {
	out := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		out = append(out, p.Name)
	}
	return out
}

// One batch, all three kinds: an append, an update and a removal, in the order the appends have to
// come first for.
//
// The appended element's values are ORDINARY updates — they resolve because the append ran before
// them, which is the whole reason the order is fixed. A pipeline that applied updates first would
// fail on a path the config does not have yet.
func TestApplyListEditsAppendsThenUpdatesThenRemoves(t *testing.T) {
	cfg, path := loadEditsFixture(t, editsFixture)
	edits := ListEdits{
		Appends: []string{"providers.3"},
		Updates: map[string]any{
			"providers.0.model":   "m0-edited",
			"providers.3.name":    "added",
			"providers.3.model":   "m3",
			"providers.3.api_key": "k3",
		},
		Removes: []string{"providers.1"},
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}

	// main (edited, still first), fast removed, spare, added.
	if want := "main,spare,added"; strings.Join(providerNamesOf(cfg), ",") != want {
		t.Errorf("providers = %v, want %v", providerNamesOf(cfg), want)
	}
	if cfg.Providers[0].Model != "m0-edited" {
		t.Errorf("providers.0.model = %q, want the update applied", cfg.Providers[0].Model)
	}
	if cfg.Providers[2].Model != "m3" {
		t.Errorf("providers[2] = %+v, want the values the batch sent for the new element", cfg.Providers[2])
	}

	// …and the same edit through the file: what the settings page does. The object is written
	// whole, so the only question is whether it round-trips.
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this wrote does not load: %v", err)
	}
	if want := "main,spare,added"; strings.Join(providerNamesOf(got), ",") != want {
		t.Errorf("providers after a reload = %v, want %v", providerNamesOf(got), want)
	}
	if got.Providers[0].Model != "m0-edited" || got.Providers[2].Model != "m3" {
		t.Errorf("the leaf updates did not survive the round trip: %+v", got.Providers)
	}
}

// The removal goes LAST, and this is what that rule protects: a removal shifts every later index up
// by one, so an update applied afterwards names a different element than the caller meant.
//
// The fixture makes the two orders disagree on purpose — the update and the removal touch adjacent
// elements, so a pipeline that removed first would write into `spare` instead of `fast`.
func TestApplyListEditsUpdatesBeforeRemoving(t *testing.T) {
	cfg, _ := loadEditsFixture(t, editsFixture)
	edits := ListEdits{
		Updates: map[string]any{"providers.1.model": "fast-edited"},
		Removes: []string{"providers.0"},
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}
	if want := "fast,spare"; strings.Join(providerNamesOf(cfg), ",") != want {
		t.Fatalf("providers = %v, want %v (main removed)", providerNamesOf(cfg), want)
	}
	if cfg.Providers[0].Name != "fast" || cfg.Providers[0].Model != "fast-edited" {
		t.Errorf("providers[0] = %+v, want the update on `fast` — the edit named index 1 BEFORE the "+
			"removal shifted it to 0", cfg.Providers[0])
	}
	if cfg.Providers[1].Name != "spare" || cfg.Providers[1].Model != "m2" {
		t.Errorf("providers[1] = %+v, want `spare` untouched", cfg.Providers[1])
	}
}

// A new element starts from its TYPE's defaults, not from zeroes: `web_search.providers` declares
// `type: exa`, so an item the form just added is a working one.
func TestApplyListEditsAppendsAWorkingElement(t *testing.T) {
	cfg, _ := loadEditsFixture(t, "language: zh\n")
	edits := ListEdits{
		Appends: []string{"web_search.providers.0"},
		Updates: map[string]any{"web_search.providers.0.key": "sk-exa"},
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}
	if len(cfg.WebSearch.Providers) != 1 {
		t.Fatalf("web_search.providers = %+v, want one item", cfg.WebSearch.Providers)
	}
	p := cfg.WebSearch.Providers[0]
	if p.Type != WebSearchProviderExa {
		t.Errorf("the new item's type = %q, want the type's default %q", p.Type, WebSearchProviderExa)
	}
	if p.Key != "sk-exa" {
		t.Errorf("the new item's key = %q, want the batch's update", p.Key)
	}
}

// Two appends into one list gain two elements: that is what two presses of ＋ mean, and their
// indices are consecutive because each one lands at the list's END — the first makes the second's
// index true (see ListEdits.Appends).
func TestApplyListEditsAppendsOneElementPerPath(t *testing.T) {
	cfg, _ := loadEditsFixture(t, editsFixture)
	edits := ListEdits{
		Appends: []string{"providers.3", "providers.4"},
		Updates: map[string]any{"providers.3.name": "first", "providers.4.name": "second"},
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}
	if want := "main,fast,spare,first,second"; strings.Join(providerNamesOf(cfg), ",") != want {
		t.Errorf("providers = %v, want %v", providerNamesOf(cfg), want)
	}
}

// An append whose index is not the list's end is REFUSED, and this is the failure the element path
// exists to catch: a form working from a config that moved would otherwise write its values into
// an element somebody else is using (`providers.2` is `spare`, not a free slot).
func TestApplyListEditsRefusesAnAppendThatIsNotTheEnd(t *testing.T) {
	cfg, _ := loadEditsFixture(t, editsFixture)
	err := ApplyListEdits(cfg, ListEdits{
		Appends: []string{"providers.1"},
		Updates: map[string]any{"providers.1.name": "would-have-overwritten-fast"},
	})
	if err == nil {
		t.Fatal("appending at index 1 of a three-element list was accepted")
	}
	if !strings.Contains(err.Error(), "现在有 3 项") {
		t.Errorf("err = %v, want it to say how long the list is", err)
	}
	if want := "main,fast,spare"; strings.Join(providerNamesOf(cfg), ",") != want {
		t.Errorf("providers = %v, want the list untouched", providerNamesOf(cfg))
	}
	if cfg.Providers[1].Name != "fast" {
		t.Errorf("providers[1] = %+v, want `fast` untouched", cfg.Providers[1])
	}
}

// A removal the list cannot do is refused, and nothing else in the batch is left behind: the caller
// rolls back from its snapshot (see SaveSettings), which is why this one only has to be an error.
func TestApplyListEditsRefusesARemovalOffTheEnd(t *testing.T) {
	cfg, _ := loadEditsFixture(t, editsFixture)
	err := ApplyListEdits(cfg, ListEdits{Removes: []string{"providers.7"}})
	if err == nil {
		t.Fatal("removing index 7 of a three-element list was accepted")
	}
	if !strings.Contains(err.Error(), "没有第 7 项") {
		t.Errorf("err = %v, want it to name the element and the list's length", err)
	}
	if len(cfg.Providers) != 3 {
		t.Errorf("providers = %+v, want the list untouched", cfg.Providers)
	}
}

// A new element may carry an 自动 leaf (config.Unset), and the config object ends up with nil there —
// which is what "not set" means for these fields.
//
// The file half of this rule is now the marshal's: an unset optional leaf is `omitempty`, so it is
// simply a key that is not written (see TestSaveLeavesAnUnsetLeafOutOfTheFile for the round trip).
func TestApplyListEditsAppendsAnElementWithAnUnsetLeaf(t *testing.T) {
	cfg, _ := loadEditsFixture(t, "language: zh\nproviders:\n  - name: main\n    model: m\n")
	edits := ListEdits{
		Appends: []string{"providers.1"},
		Updates: map[string]any{
			"providers.1.name":             "added",
			"providers.1.spec.vision":      Unset{},
			"providers.1.spec.proxy":       "http://127.0.0.1:7890",
			"providers.1.spec.max_retries": 3,
		},
	}
	if err := ApplyListEdits(cfg, edits); err != nil {
		t.Fatalf("ApplyListEdits: %v", err)
	}
	spec := cfg.Providers[1].Spec
	if spec.Vision != nil {
		t.Errorf("vision = %v, want nil (自动)", spec.Vision)
	}
	if spec.Proxy != "http://127.0.0.1:7890" || spec.MaxRetries == nil || *spec.MaxRetries != 3 {
		t.Errorf("the other values did not land: %+v", spec)
	}
}

// Save writes the object whole, so an optional leaf that is UNSET is a key the file does not have —
// and reading the file back gives nil again, which is the state the form draws as 自动.
func TestSaveLeavesAnUnsetLeafOutOfTheFile(t *testing.T) {
	cfg, path := loadEditsFixture(t, editsFixture)
	if err := SetPath(cfg, "providers.0.spec.vision", true); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "vision: true") {
		t.Errorf("a SET optional leaf must be in the file:\n%s", body)
	}

	if err := SetPath(cfg, "providers.0.spec.vision", Unset{}); err != nil {
		t.Fatal(err)
	}
	if err := SaveTo(path, cfg); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	body, _ = os.ReadFile(path)
	if strings.Contains(string(body), "vision") {
		t.Errorf("an unset optional leaf must not be spelled out:\n%s", body)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Providers[0].Spec.Vision != nil {
		t.Errorf("vision = %v, want nil after the round trip", *got.Providers[0].Spec.Vision)
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
	cfg, _ := loadEditsFixture(t, editsFixture)
	edits := ListEdits{
		Appends: []string{"providers.3"},
		Updates: map[string]any{
			"providers.0.model":   "m0-edited",
			"providers.2.api_key": "k2-edited",
			"providers.3.name":    "added",
		},
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
	if want := "main,fast,spare"; strings.Join(providerNamesOf(cfg), ",") != want {
		t.Errorf("providers = %v, want %v (the removed element back at its own index)", providerNamesOf(cfg), want)
	}
	if cfg.Providers[0].Model != "m0" {
		t.Errorf("providers.0.model = %q, want the pre-edit value", cfg.Providers[0].Model)
	}
	if cfg.Providers[2].APIKey != "k2" {
		t.Errorf("providers.2.api_key = %q, want the pre-edit value (the element the removal shifted)",
			cfg.Providers[2].APIKey)
	}
}
