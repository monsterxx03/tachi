package config

import (
	"reflect"
	"strings"
	"testing"
)

// A list this file may grow: the three the settings page offers today, plus every other list of
// structs. NOT on it: a slice of scalars (one leaf — a form adds to it by typing a line) and a
// map (keyed by NAME, not by position).
//
// The walk stops at a list of structs and does not descend into its elements, so a nested list
// (`providers.spec.pricing.bands`) is invisible while a list reached through a plain struct field
// (`jev.pricing.bands`) is not. That asymmetry is why a new field of a struct-bearing pointer type
// shows up here: add it to want, and the message below says so.
func TestListItemPathsAreTheListsAFormCanGrow(t *testing.T) {
	got := ListItemPaths()
	want := map[string]bool{
		"providers":            true,
		"web_search.providers": true,
		"web_fetch.providers":  true,
		"jev.pricing.bands":    true,
		// The peers a form can grow: this machine's send-side directory (one row per node it
		// may delegate to).
		"peers": true,
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("ListItemPaths() includes %q. If you just added a list of structs to Config, "+
				"add it to want — the settings page may then offer it. If not, this is not a list of "+
				"items a form grows.", p)
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

const listFixture = `language: zh
providers:
  - name: main
    type: openai
    model: m0
    base_url: https://api.example.com/v1
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

// Removal takes the element it was asked for and lets the others keep their order.
func TestRemoveListItemTakesTheRightOneAndCanBeUndone(t *testing.T) {
	cfg, err := LoadFrom(writeConfigFixture(t, listFixture))
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

	// The undo puts the element back WHERE IT WAS, not at the end: the caller's file write may
	// have failed, and reordering the list would be a change nobody asked for.
	if err := RestoreListItem(cfg, "providers.0", held); err != nil {
		t.Fatalf("RestoreListItem: %v", err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0].Name != "main" || cfg.Providers[1].Name != "fast" {
		t.Errorf("after the undo: %+v", cfg.Providers)
	}
}

// The refusals: a path that is not a list of items, and an element path that is not one.
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
			return ApplyListEdits(cfg, ListEdits{Appends: []string{"review.allowed_tools"}})
		}},
		{"a block is not a list", func() error {
			return ApplyListEdits(cfg, ListEdits{Appends: []string{"logs"}})
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

// ElementFields describes an element that does not exist yet: the leaves of the TYPE, with the
// type's own defaults. It is what lets the settings page draw a card for an item just staged with
// ＋ — the one question a live config cannot answer.
func TestElementFieldsDescribeAStagedItem(t *testing.T) {
	fields, err := ElementFields("web_search.providers")
	if err != nil {
		t.Fatalf("ElementFields: %v", err)
	}
	byTail := make(map[string]ElementField, len(fields))
	for _, f := range fields {
		byTail[f.Tail] = f
	}
	if f, ok := byTail["type"]; !ok {
		t.Fatalf("no `type` leaf: %+v", fields)
	} else if f.Value != string(WebSearchProviderExa) {
		t.Errorf("type = %v, want the element type's default (%s)", f.Value, WebSearchProviderExa)
	}
	if f, ok := byTail["key"]; !ok {
		t.Errorf("no `key` leaf: %+v", fields)
	} else if f.Kind != "string" {
		t.Errorf("key.Kind = %q, want string", f.Kind)
	}
	// Nested collections are not leaves a form writes as one value.
	for _, f := range fields {
		if strings.Contains(f.Tail, ".*") {
			t.Errorf("ElementFields includes %q, which a form cannot draw as one input", f.Tail)
		}
	}
}

// An element's leaves are described whether or not the list currently HAS an element: the shape is
// the type's, which is the point (an empty list is exactly when someone wants the first item).
func TestElementFieldsWorkForAProvider(t *testing.T) {
	fields, err := ElementFields("providers")
	if err != nil {
		t.Fatalf("ElementFields: %v", err)
	}
	tails := make([]string, 0, len(fields))
	for _, f := range fields {
		tails = append(tails, f.Tail)
	}
	for _, want := range []string{"name", "type", "model", "base_url", "api_key", "spec.proxy", "spec.vision"} {
		if !contains(tails, want) {
			t.Errorf("ElementFields(providers) is missing %q: %v", want, tails)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
