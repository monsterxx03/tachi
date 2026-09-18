package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// A list this file may grow: the three the settings page offers today, plus every other list of
// structs. NOT on it: a slice of scalars (one leaf — a form adds to it by typing a line) and a
// map (keyed by NAME, not by position).
func TestListItemPathsAreTheListsAFormCanGrow(t *testing.T) {
	got := ListItemPaths()
	want := map[string]bool{
		"providers":            true,
		"web_search.providers": true,
		"web_fetch.providers":  true,
		"lsp.servers":          true,
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("ListItemPaths() includes %q, which is not a list of structs the form can add to", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("ListItemPaths() is missing %q", p)
	}
	// The two shapes that must NOT be there, named so a future walk cannot quietly add them.
	for _, p := range append([]string{"permissions.bash.allow", "review.allowed_tools"}, mapPaths()...) {
		for _, got := range got {
			if got == p {
				t.Errorf("ListItemPaths() includes %q: that is not a list of items", p)
			}
		}
	}
}

// mapPaths names the map-of-lists the walk must skip: `hooks.events.pre_tool` is keyed by an
// event name, so appending "at the end" means inventing one.
func mapPaths() []string { return []string{"hooks.events.pre_tool", "hooks.events.post_tool"} }

const listFixture = `# 我的配置：注释得留着
language: zh
providers:
  - name: main
    type: openai
    model: m0
    base_url: https://api.example.com/v1   # 行尾注释
    api_key: k0
    spec:
      timeout: 30s
  - name: fast
    type: openai
    model: m1
    base_url: https://api.example.com/v1
    api_key: k1
title_provider: main
`

// AppendListItem + AppendListItemTo: the new element goes to the END, in memory and in the file,
// and what lands is an element the loader reads back — a struct-marshalled block, so it looks
// like the ones already in the file.
func TestAppendListItemWritesAWorkingElement(t *testing.T) {
	path := writeConfigFixture(t, listFixture)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	elementPath, literal, err := AppendListItem(cfg, "providers")
	if err != nil {
		t.Fatalf("AppendListItem: %v", err)
	}
	if elementPath != "providers.2" {
		t.Errorf("elementPath = %q, want providers.2 (appended at the END)", elementPath)
	}
	if len(cfg.Providers) != 3 || cfg.Providers[2].Name != "" {
		t.Errorf("memory after the append: %d providers, last = %+v", len(cfg.Providers), cfg.Providers[2])
	}
	if !strings.Contains(literal, `name: ""`) {
		t.Errorf("literal = %q, want the element marshalled as a block", literal)
	}
	if err := AppendListItemTo(path, "providers", literal); err != nil {
		t.Fatalf("AppendListItemTo: %v", err)
	}

	body, _ := os.ReadFile(path)
	// The file is hand-written and stays that way: comments, line comments and the existing
	// elements are all untouched.
	for _, want := range []string{"# 我的配置：注释得留着", "# 行尾注释", "name: main", "name: fast"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("config.yaml lost %q:\n%s", want, body)
		}
	}
	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this wrote does not load: %v\n%s", err, body)
	}
	if len(reloaded.Providers) != 3 {
		t.Fatalf("providers = %d after reload, want 3", len(reloaded.Providers))
	}
	if reloaded.Providers[2].Name != "" || reloaded.Providers[0].Name != "main" ||
		reloaded.Providers[0].Spec.Timeout.String() != "30s" {
		t.Errorf("reloaded providers = %+v", reloaded.Providers)
	}

	// The element is EDITABLE, which is the point of writing it: the leaves the skeleton left out
	// (the `omitempty` ones) are reachable through SavePartial's add-a-key rule, and the ones it
	// wrote are ordinary updates.
	if err := SavePartialTo(path, map[string]any{
		"providers.2.name":                "added",
		"providers.2.spec.context_window": 200000,
		"providers.2.spec.vision":         true,
	}); err != nil {
		t.Fatalf("editing the new element: %v", err)
	}
	final, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload after editing the new element: %v", err)
	}
	last := final.Providers[2]
	if last.Name != "added" || last.Spec.ContextWindow == nil || *last.Spec.ContextWindow != 200000 ||
		last.Spec.Vision == nil || !*last.Spec.Vision {
		t.Errorf("the new element did not take its values: %+v", last)
	}
	if final.Providers[0].Spec.Timeout.String() != "30s" {
		t.Errorf("editing the new element disturbed the first one: %+v", final.Providers[0])
	}
}

// Removal takes the element it was asked for and lets the others keep their order — the file is
// verified by identity, not by counting, so taking the wrong one fails loudly.
func TestRemoveListItemTakesTheRightOneAndCanBeUndone(t *testing.T) {
	path := writeConfigFixture(t, listFixture)
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	held, err := RemoveListItem(cfg, "providers.0")
	if err != nil {
		t.Fatalf("RemoveListItem: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "fast" {
		t.Fatalf("memory after removing index 0: %+v", cfg.Providers)
	}
	if err := RemoveListItemFrom(path, "providers.0"); err != nil {
		t.Fatalf("RemoveListItemFrom: %v", err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "name: main") {
		t.Errorf("the removed element is still in the file:\n%s", body)
	}
	if !strings.Contains(string(body), "name: fast") || !strings.Contains(string(body), "# 我的配置：注释得留着") {
		t.Errorf("removal took too much:\n%s", body)
	}
	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this wrote does not load: %v\n%s", err, body)
	}
	if len(reloaded.Providers) != 1 || reloaded.Providers[0].Name != "fast" {
		t.Errorf("reloaded providers = %+v", reloaded.Providers)
	}

	// The undo puts the element back WHERE IT WAS, not at the end: the caller's file write may
	// have failed, and reordering the list would be a change nobody asked for.
	if err := RestoreListItem(cfg, "providers.0", held); err != nil {
		t.Fatalf("RestoreListItem: %v", err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0].Name != "main" || cfg.Providers[1].Name != "fast" {
		t.Errorf("after the undo: %+v", cfg.Providers)
	}
}

// The refusals: a path that is not a list of items, and an index that is not there.
func TestListEditsRefuseWhatTheyCannotDo(t *testing.T) {
	cfg, err := LoadFrom(writeConfigFixture(t, listFixture))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		call func() error
	}{
		{"a slice of scalars is one leaf, not a list of items", func() error {
			_, _, err := AppendListItem(cfg, "review.allowed_tools")
			return err
		}},
		{"a block is not a list", func() error {
			_, _, err := AppendListItem(cfg, "logs")
			return err
		}},
		{"an element path needs an index", func() error {
			_, err := RemoveListItem(cfg, "providers")
			return err
		}},
		{"and the index must be there", func() error {
			_, err := RemoveListItem(cfg, "providers.7")
			return err
		}},
	}
	for _, tt := range tests {
		err := tt.call()
		if err == nil {
			t.Errorf("%s: expected a refusal", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), "providers") && !strings.Contains(err.Error(), "review") &&
			!strings.Contains(err.Error(), "logs") {
			t.Errorf("%s: the error should name the path, got: %v", tt.name, err)
		}
	}
	if len(cfg.Providers) != 2 {
		t.Errorf("a refused edit must not have touched the config: %+v", cfg.Providers)
	}
}

// ListItemLeaves is what a summary names an item by, and what a caller checks references against
// before removing one.
func TestListItemLeavesDescribesAnElement(t *testing.T) {
	cfg, err := LoadFrom(writeConfigFixture(t, listFixture))
	if err != nil {
		t.Fatal(err)
	}
	leaves, err := ListItemLeaves(cfg, "providers.0")
	if err != nil {
		t.Fatalf("ListItemLeaves: %v", err)
	}
	if leaves["name"] != "main" || leaves["model"] != "m0" || leaves["spec.timeout"] != "30s" {
		t.Errorf("leaves = %v", leaves)
	}
	if !reflect.DeepEqual(leaves["spec.timeout"], "30s") {
		t.Errorf("spec.timeout = %#v, want the string form a duration reads as", leaves["spec.timeout"])
	}
}

// A list whose block is not in the file AT ALL: the block is created, because the path says where
// it goes. This is the case a config that never configured web search is in, and "add the first
// one" is exactly when someone wants the button.
func TestAppendListItemCreatesTheMissingBlock(t *testing.T) {
	path := writeConfigFixture(t, "# 我的配置\nlanguage: zh\n")
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	elementPath, literal, err := AppendListItem(cfg, "web_search.providers")
	if err != nil {
		t.Fatalf("AppendListItem into an absent block: %v", err)
	}
	if elementPath != "web_search.providers.0" {
		t.Errorf("elementPath = %q", elementPath)
	}
	if err := AppendListItemTo(path, "web_search.providers", literal); err != nil {
		t.Fatalf("AppendListItemTo: %v", err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "# 我的配置") {
		t.Errorf("the comment was lost:\n%s", body)
	}
	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this wrote does not load: %v\n%s", err, body)
	}
	if len(reloaded.WebSearch.Providers) != 1 {
		t.Fatalf("web_search.providers = %+v, want one item", reloaded.WebSearch.Providers)
	}
	if reloaded.WebSearch.Providers[0].Type != WebSearchProviderExa {
		t.Errorf("the new search provider = %+v, want the type's default (exa) filled in",
			reloaded.WebSearch.Providers[0])
	}

	// A second one goes into the block that now exists.
	if _, literal2, err := AppendListItem(cfg, "web_search.providers"); err != nil {
		t.Fatalf("AppendListItem (second): %v", err)
	} else if err := AppendListItemTo(path, "web_search.providers", literal2); err != nil {
		t.Fatalf("AppendListItemTo (second): %v", err)
	}
	final, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload after the second add: %v", err)
	}
	if len(final.WebSearch.Providers) != 2 {
		t.Errorf("web_search.providers = %+v, want two items", final.WebSearch.Providers)
	}
}
