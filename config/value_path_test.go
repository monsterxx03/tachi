package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestReadPathWalksTheDottedPathGrammar(t *testing.T) {
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
		{"language", nil, "nil"},     // nil is refused: clearing a leaf takes config.Unset, in memory as in the file
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

// ApplyUpdates is where a value is validated: the FIELD's own type decides, and a mismatch is an
// error rather than a conversion — so a bad value never reaches the file, which is what the
// pre-flight check this replaced used to guarantee.
func TestApplyUpdatesRefusesWhatTheFieldCannotHold(t *testing.T) {
	cfg, err := LoadFrom(writeConfigFixture(t, "language: zh\nmax_tokens: 100\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdates(cfg, map[string]any{"language": "en", "max_tokens": 200}); err != nil {
		t.Errorf("ApplyUpdates on good values: %v", err)
	}
	for _, tt := range []struct {
		name    string
		updates map[string]any
	}{
		{"nil value", map[string]any{"language": nil}},
		{"unsupported type", map[string]any{"language": struct{ X int }{}}},
		{"a number where a string goes", map[string]any{"language": 3}},
		{"a non-integer where an int goes", map[string]any{"max_tokens": 1.5}},
	} {
		err := ApplyUpdates(cfg, tt.updates)
		if err == nil {
			t.Errorf("%s: ApplyUpdates = nil, want an error", tt.name)
			continue
		}
		for key := range tt.updates {
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s: error %q does not name the key %q", tt.name, err, key)
			}
		}
	}
}

// …and a batch that fails is put back WHOLE: every leaf it already wrote goes back to what it
// held, so a half-applied batch is not a state the caller can be left holding.
func TestApplyUpdatesIsAllOrNone(t *testing.T) {
	cfg, err := LoadFrom(writeConfigFixture(t, "language: zh\nmax_tokens: 100\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Sorted order puts `language` first, so the bad `max_tokens` fails AFTER it was written —
	// which is the only way this test can see the rollback at all.
	if err := ApplyUpdates(cfg, map[string]any{"language": "en", "max_tokens": "not a number"}); err == nil {
		t.Fatal("a string in an int field was accepted")
	}
	if cfg.Language != "zh" {
		t.Errorf("language = %q, want the pre-batch value: the batch half-landed", cfg.Language)
	}
	if cfg.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want it untouched", cfg.MaxTokens)
	}
}

// TestYAMLPathsKeepsTheLeafRulesShape: a collection of SCALARS is one leaf (the whole list is one
// value), while a collection of STRUCTS is one leaf per element. The
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

// A duration leaf answers with the STRING the file holds, whichever of the config's two
// duration types it is declared as.
//
// The two used to answer a binding differently, and both answers were wrong for a form:
// time.Duration has no marshaller of its own, so it crossed as 30000000000 (nanoseconds — not
// something a person edits); config.Duration marshals as a string, so it crossed as "30s" and
// then could not be SAVED at all, because the write path only accepted an integer. The path
// API is the file's own view of the config, and `timeout: 30s` is what the file holds.
func TestDurationLeavesReadAndWriteAsTheFileHolds(t *testing.T) {
	cfg := &Config{}
	cfg.LSP.RequestTimeout = Duration(45 * time.Second) // config.Duration
	cfg.Memory.Timeout = 90 * time.Second               // time.Duration
	cfg.MCPToolRefresh.Interval = 2 * time.Minute       // …and one under a block

	for _, tt := range []struct{ key, want string }{
		{"lsp.request_timeout", "45s"},
		{"memory.timeout", "1m30s"},
		{"mcp_tool_refresh.interval", "2m0s"},
	} {
		got, err := ReadPath(cfg, tt.key)
		if err != nil {
			t.Fatalf("ReadPath(%q): %v", tt.key, err)
		}
		if got != tt.want {
			t.Errorf("ReadPath(%q) = %#v, want %q", tt.key, got, tt.want)
		}
	}

	// Writing takes the same form, and the struct holds a real duration afterwards.
	if err := SetPath(cfg, "lsp.request_timeout", "1m30s"); err != nil {
		t.Fatalf("SetPath with a duration string: %v", err)
	}
	if cfg.LSP.RequestTimeout != Duration(90*time.Second) {
		t.Errorf("LSP.RequestTimeout = %v, want 1m30s", time.Duration(cfg.LSP.RequestTimeout))
	}
	if got, _ := ReadPath(cfg, "lsp.request_timeout"); got != "1m30s" {
		t.Errorf("round trip = %#v, want %q", got, "1m30s")
	}
	// A NUMBER is still accepted: nothing was removed, and that is what a caller holding the
	// raw value passes.
	if err := SetPath(cfg, "memory.timeout", 5*time.Second); err != nil {
		t.Fatalf("SetPath with a raw duration: %v", err)
	}
	if cfg.Memory.Timeout != 5*time.Second {
		t.Errorf("Memory.Timeout = %v, want 5s", cfg.Memory.Timeout)
	}

	// A string that is not a duration is refused BY NAME, and the refusal changes nothing: this
	// is the mistake a text box makes (typing "45 seconds"), so the message has to be the one
	// the reader needs.
	err := SetPath(cfg, "lsp.request_timeout", "45 seconds")
	if err == nil {
		t.Fatal("SetPath took a string that is not a duration")
	}
	if !strings.Contains(err.Error(), "lsp.request_timeout") || !strings.Contains(err.Error(), "时长") {
		t.Errorf("err = %v, want the key and the word 时长", err)
	}
	if cfg.LSP.RequestTimeout != Duration(90*time.Second) {
		t.Errorf("LSP.RequestTimeout = %v after the refusal, want it untouched", time.Duration(cfg.LSP.RequestTimeout))
	}
}

// DescribeLeaf answers the two questions a form cannot read off the VALUE: what control this
// leaf wants, and whether "unset" is a state of its own. The second one is the reason it exists —
// an unset *bool reads as nothing at all, and `vision: nil` means "decide it from the model",
// which is neither true nor false.
func TestDescribeLeafTellsAnUnsetLeafFromAnEmptyOne(t *testing.T) {
	cfg := &Config{Language: "zh", Providers: []ProviderConfig{{Name: "main", Model: "m"}}}
	tests := []struct {
		key      string
		kind     string
		optional bool
		isSet    bool
	}{
		// A plain scalar: a text box, always set.
		{"language", "string", false, true},
		// Optional, and unset: a switch whose third state is 自动.
		{"providers.0.spec.vision", "bool", true, false},
		{"providers.0.spec.max_retries", "int", true, false},
		{"providers.0.spec.context_window", "int", true, false},
		{"providers.0.spec.credit_rate", "float", true, false},
		// The proxy is a plain string: "" is its own "no explicit proxy" state, so there is no
		// third state for a form to draw (unlike vision/max_retries/context_window above).
		{"providers.0.spec.proxy", "string", false, true},
		// A pointer WITH a `default:` tag is not "optional" in this sense: the default says what
		// unset means, so unset and the default are one fact and the form shows a plain checkbox.
		{"lsp.enabled", "bool", false, false},
		// And a list is the whole list as one value.
		{"review.allowed_tools", "string_list", false, true},
	}
	for _, tt := range tests {
		info, err := DescribeLeaf(cfg, tt.key)
		if err != nil {
			t.Errorf("DescribeLeaf(%q): %v", tt.key, err)
			continue
		}
		if info.Kind != tt.kind || info.Optional != tt.optional || info.IsSet != tt.isSet {
			t.Errorf("DescribeLeaf(%q) = %+v, want {Kind:%s Optional:%v IsSet:%v}",
				tt.key, info, tt.kind, tt.optional, tt.isSet)
		}
	}

	// Setting one makes IsSet true; clearing it with Unset puts it back — and a leaf that is not
	// optional refuses to be cleared at all (an int has no null).
	if err := SetPath(cfg, "providers.0.spec.vision", true); err != nil {
		t.Fatalf("SetPath: %v", err)
	}
	if info, _ := DescribeLeaf(cfg, "providers.0.spec.vision"); !info.IsSet {
		t.Error("vision reads as unset after being set")
	}
	if err := SetPath(cfg, "providers.0.spec.vision", Unset{}); err != nil {
		t.Fatalf("SetPath(Unset): %v", err)
	}
	if cfg.Providers[0].Spec.Vision != nil {
		t.Errorf("vision = %v after Unset, want nil", *cfg.Providers[0].Spec.Vision)
	}
	if info, _ := DescribeLeaf(cfg, "providers.0.spec.vision"); info.IsSet {
		t.Error("vision reads as set after Unset")
	}
	err := SetPath(cfg, "language", Unset{})
	if err == nil || !strings.Contains(err.Error(), "不是可选项") {
		t.Errorf("err = %v, want a refusal naming the reason", err)
	}
	if cfg.Language != "zh" {
		t.Errorf("language = %q after a refused Unset, want it untouched", cfg.Language)
	}
}

// Secret is the fourth thing DescribeLeaf answers (after Kind, Optional and IsSet), and it is
// derived from the leaf's own yaml name rather than declared anywhere: the same name is a credential
// wherever it appears, so a new provider in a new block is masked without a form being told about it.
//
// The half that matters as much as the secrets is the PLAIN list: `keyword_provider` sits next to
// the api_key in the same struct and is a provider NAME, and a name-based rule that matched by
// substring would hide a value the reader came to this page to read.
func TestDescribeLeafMarksCredentialLeaves(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{{Name: "main", APIKey: "sk-main"}},
		Jev:       JevConfig{APIKey: "sk-jev"},
		WebSearch: WebSearchConfig{Providers: []WebSearchProviderConfig{{Type: WebSearchProviderExa, Key: "exa-k"}}},
		WebFetch:  WebFetchConfig{Providers: []WebFetchProviderConfig{{Type: WebFetchProviderFirecrawl, Key: "fc-k"}}},
		Web:       WebConfig{APIKey: "web-k"},
	}
	secrets := []string{
		"providers.0.api_key",
		"jev.api_key",
		"web_search.providers.0.key",
		"web_fetch.providers.0.key",
		"web.api_key",
	}
	for _, key := range secrets {
		info, err := DescribeLeaf(cfg, key)
		if err != nil {
			t.Errorf("DescribeLeaf(%q): %v", key, err)
			continue
		}
		if !info.Secret {
			t.Errorf("DescribeLeaf(%q) says it is not a credential", key)
		}
	}
	plain := []string{
		"providers.0.name",
		"providers.0.base_url",
		"memory.keyword_provider",
		"web_search.providers.0.base_url",
		"web_fetch.providers.0.type",
		"web.addr",
	}
	for _, key := range plain {
		info, err := DescribeLeaf(cfg, key)
		if err != nil {
			t.Errorf("DescribeLeaf(%q): %v", key, err)
			continue
		}
		if info.Secret {
			t.Errorf("DescribeLeaf(%q) calls a plain leaf a credential", key)
		}
	}
}

// Every name in secretLeafNames must be a name this config actually uses. The set is written by
// hand (see its comment), so an entry left behind by a renamed field is a rule that silently does
// nothing — nothing else in the code can notice, because "no leaf has this name" is exactly what a
// working rule looks like from here.
func TestEverySecretLeafNameIsALeafOfTheConfig(t *testing.T) {
	used := make(map[string]bool)
	for _, path := range YAMLPaths() {
		segs := strings.Split(path, ".")
		used[segs[len(segs)-1]] = true
	}
	for name := range secretLeafNames {
		if !used[name] {
			t.Errorf("secretLeafNames holds %q, which is not a leaf of the config", name)
		}
	}
}

// The element template is the one description of a leaf that does not come from a value, and a
// credential added ON THE PAGE (`providers` ＋) must be masked exactly like one read from the file.
func TestElementFieldsCarrySecret(t *testing.T) {
	for _, tc := range []struct {
		list string
		tail string
		want bool
	}{
		{"providers", "api_key", true},
		{"providers", "base_url", false},
		{"web_search.providers", "key", true},
		{"web_search.providers", "base_url", false},
		{"web_fetch.providers", "key", true},
	} {
		fields, err := ElementFields(tc.list)
		if err != nil {
			t.Errorf("ElementFields(%q): %v", tc.list, err)
			continue
		}
		found := false
		for _, f := range fields {
			if f.Tail != tc.tail {
				continue
			}
			found = true
			if f.Secret != tc.want {
				t.Errorf("%s.%s: Secret = %v, want %v", tc.list, tc.tail, f.Secret, tc.want)
			}
		}
		if !found {
			t.Errorf("%s has no %q leaf in its template", tc.list, tc.tail)
		}
	}
}
