package config

import (
	"reflect"
	"strings"
	"testing"
)

// readPathFixture is a config with one of every shape the settings table addresses: a plain
// scalar, a pointer scalar (set and unset), a slice of structs, a slice of strings, a map of
// slices of structs, and a nested block.
func readPathFixture() *Config {
	return &Config{
		Language:        "zh",
		MaxTokens:       1000,
		MaxIterations:   new(int),  // set, and 0 — a real value, not "unset"
		TitleGeneration: new(bool), // set to false
		Providers:       []ProviderConfig{{Name: "p0", Model: "m0"}, {Name: "p1", Model: "m1"}},
		Hooks: HooksConfig{Events: map[string][]HookCommand{
			"pre_tool": {{Command: "echo hi", Timeout: "5s"}},
		}},
		Review: ReviewConfig{AllowedTools: []string{"Bash"}},
		TUI:    TUIConfig{InputHistoryLimit: 7},
	}
}

func TestReadPathWalksTheSameGrammarAsSavePartial(t *testing.T) {
	cfg := readPathFixture()
	tests := []struct {
		key  string
		want any
	}{
		{"language", "zh"},
		{"max_tokens", 1000},
		{"max_iterations", 0},       // a set pointer holding the zero value
		{"title_generation", false}, // …and one holding false: not the same as unset
		{"providers.0.name", "p0"},  // sequence index
		{"providers.1.model", "m1"}, // …and the other element
		{"review.allowed_tools", []string{"Bash"}},
		{"tui.input_history_limit", 7},
		{"hooks.events.pre_tool.0.command", "echo hi"}, // map key, then index
		// An unset pointer reads as its `default:` tag — the value the config's own accessors
		// report. hooks.enabled and lsp.enabled are both `default:"true"`.
		{"hooks.enabled", true},
		{"lsp.enabled", true},
		{"tui.notify_on_complete", true},
	}
	for _, tt := range tests {
		got, err := ReadPath(cfg, tt.key)
		if err != nil {
			t.Errorf("ReadPath(%q): %v", tt.key, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ReadPath(%q) = %#v, want %#v", tt.key, got, tt.want)
		}
	}
}

func TestReadPathRefusesAPathThatNamesNothing(t *testing.T) {
	cfg := readPathFixture()
	tests := []struct {
		key     string
		wantSub string // a fragment of the reason, so a wrong error is visible
	}{
		{"nope", "没有"},
		{"providers.2.model", "越界"},
		{"providers.x.model", "下标"},
		{"hooks.events.nope", "map"},
		{"mcp_servers", "没有"},                    // yaml:"-" — not a key of the file either
		{"language.deeper", "已经到底"},              // walked past a leaf
		{"review.adversarial.judge_model", "指针"}, // nil pointer in the middle of the path
		{"lsp..enabled", "empty segment"},
		{"", "empty path"},
	}
	for _, tt := range tests {
		got, err := ReadPath(cfg, tt.key)
		if err == nil {
			t.Errorf("ReadPath(%q) = %#v, want an error (%s)", tt.key, got, tt.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantSub) {
			t.Errorf("ReadPath(%q) error = %q, want it to mention %q", tt.key, err, tt.wantSub)
		}
	}
}

func TestSetPathWritesTheShapesABoundMethodReceives(t *testing.T) {
	cfg := readPathFixture()

	// A plain string.
	if err := SetPath(cfg, "language", "en"); err != nil {
		t.Fatalf("language: %v", err)
	}
	if cfg.Language != "en" {
		t.Errorf("Language = %q, want en", cfg.Language)
	}

	// A number as JSON delivers it (float64), into an int field — and the int64 field too.
	if err := SetPath(cfg, "max_tokens", float64(2000)); err != nil {
		t.Fatalf("max_tokens: %v", err)
	}
	if cfg.MaxTokens != 2000 {
		t.Errorf("MaxTokens = %d, want 2000", cfg.MaxTokens)
	}
	if err := SetPath(cfg, "lsp.max_file_size", float64(4096)); err != nil {
		t.Fatalf("lsp.max_file_size: %v", err)
	}
	if cfg.LSP.MaxFileSize != 4096 {
		t.Errorf("MaxFileSize = %d, want 4096", cfg.LSP.MaxFileSize)
	}

	// A nil POINTER field gets a fresh pointer, and an unset one allocates.
	cfg.LSP.Enabled = nil
	if err := SetPath(cfg, "lsp.enabled", false); err != nil {
		t.Fatalf("lsp.enabled: %v", err)
	}
	if cfg.LSP.Enabled == nil || *cfg.LSP.Enabled {
		t.Errorf("LSP.Enabled = %v, want a pointer to false", cfg.LSP.Enabled)
	}

	// A list as JSON delivers it ([]any of string).
	if err := SetPath(cfg, "review.allowed_tools", []any{"Bash", "Grep"}); err != nil {
		t.Fatalf("review.allowed_tools: %v", err)
	}
	if got := strings.Join(cfg.Review.AllowedTools, ","); got != "Bash,Grep" {
		t.Errorf("AllowedTools = %v", cfg.Review.AllowedTools)
	}

	// An element inside a sequence.
	if err := SetPath(cfg, "providers.1.model", "m1-new"); err != nil {
		t.Fatalf("providers.1.model: %v", err)
	}
	if cfg.Providers[1].Model != "m1-new" {
		t.Errorf("Providers[1].Model = %q", cfg.Providers[1].Model)
	}
}

func TestSetPathRefusesWhatItCannotWrite(t *testing.T) {
	cfg := readPathFixture()
	tests := []struct {
		key     string
		value   any
		wantSub string
	}{
		{"max_tokens", 1.5, "需要"},    // a non-integer into an int field
		{"max_tokens", "2000", "需要"}, // a string into an int field
		{"language", 42, "需要"},       // a number into a string field
		{"language", nil, "nil"},     // nil would clear the key (SavePartial refuses too)
		{"nope", "x", "没有"},
		{"providers", []any{"x"}, "叶子"}, // a sequence of structs is not a leaf
	}
	for _, tt := range tests {
		before, _ := ReadPath(cfg, tt.key)
		err := SetPath(cfg, tt.key, tt.value)
		if err == nil {
			t.Errorf("SetPath(%q, %#v) = nil, want an error", tt.key, tt.value)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantSub) {
			t.Errorf("SetPath(%q) error = %q, want it to mention %q", tt.key, err, tt.wantSub)
		}
		// A refused write must leave the value alone: a half-applied save is the state the
		// rollback in SaveSettings exists to prevent.
		after, _ := ReadPath(cfg, tt.key)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("SetPath(%q) changed %#v to %#v despite refusing", tt.key, before, after)
		}
	}
}

// TestSetPathReachesInsideAMapValue pins the one path shape whose WRITE works by accident of
// Go's slice semantics, so nobody has to rediscover it: a map value is a copy, but when that
// value is a SLICE its backing array is shared, so an element inside it is settable and the
// write lands in the caller's config. `hooks.events.<event>.<i>.command` is that shape.
func TestSetPathReachesInsideAMapValue(t *testing.T) {
	cfg := readPathFixture()
	if err := SetPath(cfg, "hooks.events.pre_tool.0.timeout", "30s"); err != nil {
		t.Fatalf("hooks.events.pre_tool.0.timeout: %v", err)
	}
	got, err := ReadPath(cfg, "hooks.events.pre_tool.0.timeout")
	if err != nil {
		t.Fatal(err)
	}
	if got != "30s" {
		t.Errorf("the write did not reach the map's own slice: got %#v", got)
	}
}

// TestValidateUpdatesSharesTheWritesTypeTable: the pre-flight check must reject exactly what
// the file write would, because its whole purpose is to keep a bad value out of memory before
// the write is attempted.
func TestValidateUpdatesSharesTheWritesTypeTable(t *testing.T) {
	if err := ValidateUpdates(map[string]any{"language": "en", "max_tokens": 100}); err != nil {
		t.Errorf("ValidateUpdates on good values: %v", err)
	}
	for _, tt := range []struct {
		name    string
		updates map[string]any
	}{
		{"nil value", map[string]any{"language": nil}},
		{"unsupported type", map[string]any{"language": struct{ X int }{}}},
	} {
		err := ValidateUpdates(tt.updates)
		if err == nil {
			t.Errorf("%s: ValidateUpdates = nil, want an error", tt.name)
			continue
		}
		for key := range tt.updates {
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s: error %q does not name the key %q", tt.name, err, key)
			}
		}
	}
}

// TestYAMLPathsKeepsTheLeafRulesShape: a collection of SCALARS is one leaf (the whole list is
// the value renderScalar accepts), while a collection of STRUCTS is one leaf per element. The
// structural walker is what the desktop's settings guard uses — it must be able to answer for a
// config with an empty provider list.
func TestYAMLPathsKeepsTheLeafRulesShape(t *testing.T) {
	paths := YAMLPaths()
	has := func(key string) bool {
		for _, p := range paths {
			if p == key {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"language",
		"providers.*.model", // a struct collection: one leaf per element
		"lsp.servers.*.command",
		"hooks.events.*.*.command", // a map of collections of structs
		"review.allowed_tools",     // a scalar collection: ONE leaf
		"permissions.bash.allow",
	} {
		if !has(want) {
			t.Errorf("YAMLPaths is missing %q", want)
		}
	}
	for _, unwanted := range []string{"review.allowed_tools.*", "mcp_servers", "providers"} {
		if has(unwanted) {
			t.Errorf("YAMLPaths should not contain %q", unwanted)
		}
	}
	if len(paths) < 100 {
		t.Errorf("YAMLPaths returned %d paths, which cannot be the whole config", len(paths))
	}
}

// TestPathsForExpandsAgainstTheValue: the concrete paths are the ones a form can label and a
// writer can address, so every wildcard is gone and an empty collection contributes nothing.
func TestPathsForExpandsAgainstTheValue(t *testing.T) {
	cfg := readPathFixture()
	paths := PathsFor(cfg)
	has := func(key string) bool {
		for _, p := range paths {
			if p == key {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"language",
		"providers.0.name", "providers.1.model",
		"hooks.events.pre_tool.0.command",
		"review.allowed_tools",
	} {
		if !has(want) {
			t.Errorf("PathsFor is missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"providers.2.model",              // past the end
		"lsp.servers.0.command",          // the fixture has no servers
		"review.adversarial.judge_model", // a nil pointer hides everything under it
	} {
		if has(unwanted) {
			t.Errorf("PathsFor should not contain %q", unwanted)
		}
	}
	// Every concrete path must be readable and writable-shaped: the two callers are the schema
	// and SaveSettings, and a path one of them cannot use is a path nobody should be offered.
	for _, p := range paths {
		if _, err := ReadPath(cfg, p); err != nil {
			t.Errorf("PathsFor offered %q, which ReadPath rejects: %v", p, err)
		}
	}
}

// TestSnapshotLeavesRestoresAnUnsetPointerExactly: the snapshot is the in-memory value, not
// what a form would READ. A nil pointer stays nil — restoring through ReadPath's default would
// write an explicit value the user never chose.
func TestSnapshotLeavesRestoresAnUnsetPointerExactly(t *testing.T) {
	cfg := readPathFixture()
	cfg.TitleGeneration = nil
	snap, err := SnapshotLeaves(cfg, []string{"title_generation", "language"})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPath(cfg, "title_generation", true); err != nil {
		t.Fatal(err)
	}
	if err := SetPath(cfg, "language", "en"); err != nil {
		t.Fatal(err)
	}
	RestoreLeaves(snap)
	if cfg.TitleGeneration != nil {
		t.Errorf("TitleGeneration = %v, want nil (the snapshot must not materialize a default)", *cfg.TitleGeneration)
	}
	if cfg.Language != "zh" {
		t.Errorf("Language = %q, want the snapshot value back", cfg.Language)
	}
}

// TestApplyUpdatesIsAllOrNothing pins the rollback inside one batch: a later failure must undo
// the earlier writes, because the caller has one message to write and no way to describe half a
// save.
func TestApplyUpdatesIsAllOrNothing(t *testing.T) {
	cfg := readPathFixture()
	err := ApplyUpdates(cfg, map[string]any{
		"language":            "en",   // fine
		"max_tokens":          "nope", // not an int
		"checkpoints.enabled": false,  // fine, and sorted after both
	})
	if err == nil {
		t.Fatal("ApplyUpdates accepted a bad batch")
	}
	if cfg.Language != "zh" {
		t.Errorf("Language = %q: a failed batch left the first write behind", cfg.Language)
	}
}
