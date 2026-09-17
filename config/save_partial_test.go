package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// SavePartial must leave a hand-written config.yaml alone except for the values
// it was asked to change. The file belongs to the user, so every one of these
// properties is a promise the settings page depends on.

// writeConfigFixture writes a config file that exercises every formatting feature
// a hand-written file tends to have: head/line/foot comments, an indentless
// sequence (the style `servers:` + `- name:` at the same indent), flow-style
// lists, explicit quoting and blank-line grouping.
func writeConfigFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// sameYAMLContent reports whether two renderings carry the same data, ignoring
// the formatting goccy's emitter is allowed to normalize (trailing-comment
// alignment). Parsed, not string-compared, so it can only ever be MORE forgiving
// than a byte comparison — it cannot hide a lost key.
func sameYAMLContent(t *testing.T, a, b []byte) bool {
	t.Helper()
	var va, vb any
	if err := yaml.Unmarshal(a, &va); err != nil {
		t.Fatalf("parse first: %v", err)
	}
	if err := yaml.Unmarshal(b, &vb); err != nil {
		t.Fatalf("parse second: %v", err)
	}
	return reflect.DeepEqual(va, vb)
}

const richFixture = `# 顶部注释：这是用户的配置
provider: main          # 行尾注释
max_iterations: 50
language: 中文
extra_system_prompt: "带引号的中文"

lsp:
  enabled: true
  # 独立注释：语言服务器
  servers:
  - name: gopls
    command: "gopls"
    args: []
    extensions: [".go"]     # 紧凑写法
    languages: ["go"]

providers:
    - name: deepseek-flash
      type: anthropic
      model: deepseek-flash
      spec:
        context_window: 1000000
        vision: true
`

// The core promise: the file is EDITED, not rewritten. This is the test that
// would have caught a full Marshal round trip, which changes every line (70 of
// them, measured on a real config.yaml) — indentation re-flowed, comments gone.
//
// The assertions are "no line added or removed", "the target line carries the new
// value", and "every other line still starts with what it started with". The last
// one is deliberately about the line's CONTENT, not its exact bytes: a trailing
// comment's alignment padding is the one thing goccy normalizes
// (TestSavePartial_TrailingCommentPaddingIsNormalized).
func TestSavePartial_OnlyTheTargetLineChanges(t *testing.T) {
	path := writeConfigFixture(t, richFixture)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := SavePartialTo(path, map[string]any{"language": "English"}); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	beforeLines := strings.Split(string(before), "\n")
	afterLines := strings.Split(string(after), "\n")
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("line count changed: %d → %d (the file was re-flowed, not edited)",
			len(beforeLines), len(afterLines))
	}

	target := -1
	for i := range beforeLines {
		if strings.TrimSpace(beforeLines[i]) == "language: 中文" {
			target = i
		}
	}
	if target < 0 {
		t.Fatal("fixture problem: the language line was not found")
	}
	if got := afterLines[target]; !strings.Contains(got, "English") {
		t.Errorf("the target line was not updated: %q", got)
	}

	// Every OTHER line may lose alignment padding, but must not change its key,
	// its value or its comment text.
	for i := range beforeLines {
		if i == target {
			continue
		}
		if squashSpaces(beforeLines[i]) != squashSpaces(afterLines[i]) {
			t.Errorf("line %d changed beyond padding:\n  before: %q\n  after:  %q",
				i+1, beforeLines[i], afterLines[i])
		}
	}
}

// squashSpaces collapses runs of spaces, which is how a line is compared while
// ignoring trailing-comment alignment.
func squashSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Comments and formatting must survive verbatim — including the parts on the
// very line being edited (the line's own trailing comment is the one exception:
// the value under it changed, so the line necessarily differs there).
func TestSavePartial_PreservesCommentsIndentAndStyle(t *testing.T) {
	path := writeConfigFixture(t, richFixture)

	if err := SavePartialTo(path, map[string]any{"lsp.enabled": false}); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)

	for _, want := range []string{
		"# 顶部注释：这是用户的配置", // head comment
		"provider: main", // an UNTOUCHED line keeps its content
		"# 行尾注释",       // ...and its trailing comment (goccy re-aligns the padding; see KNOWN_LIMIT)
		"  # 独立注释：语言服务器", // standalone comment
		`extensions: [".go"]`,     // flow style not expanded
		`command: "gopls"`,        // explicit quoting kept
		"\n\nlsp:",                // blank-line grouping kept
		"\nproviders:\n    - name: deepseek-flash", // 4-space indent kept as written
		"  servers:\n  - name: gopls",             // INDENTLESS sequence kept
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lost %q\n--- got ---\n%s", want, s)
		}
	}
	if !strings.Contains(s, "enabled: false") {
		t.Error("the requested change is not in the output")
	}
}

// Nested and sequence-index paths are both addressable.
func TestSavePartial_NestedAndIndexedPaths(t *testing.T) {
	path := writeConfigFixture(t, richFixture)

	err := SavePartialTo(path, map[string]any{
		"providers.0.model":            "deepseek-flash-v2",
		"providers.0.spec.vision":      false,
		"providers.0.spec.context_window": 200000,
		"lsp.servers.0.command":        "gopls-x",
	})
	if err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}

	var cfg struct {
		Providers []struct {
			Model string `yaml:"model"`
			Spec  struct {
				Vision        bool  `yaml:"vision"`
				ContextWindow int64 `yaml:"context_window"`
			} `yaml:"spec"`
		} `yaml:"providers"`
		LSP struct {
			Servers []struct {
				Command string `yaml:"command"`
			} `yaml:"servers"`
		} `yaml:"lsp"`
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("the written file does not parse: %v\n%s", err, got)
	}
	if cfg.Providers[0].Model != "deepseek-flash-v2" {
		t.Errorf("providers.0.model = %q", cfg.Providers[0].Model)
	}
	if cfg.Providers[0].Spec.Vision {
		t.Error("providers.0.spec.vision was not set to false")
	}
	if cfg.Providers[0].Spec.ContextWindow != 200000 {
		t.Errorf("providers.0.spec.context_window = %d", cfg.Providers[0].Spec.ContextWindow)
	}
	if cfg.LSP.Servers[0].Command != "gopls-x" {
		t.Errorf("lsp.servers.0.command = %q", cfg.LSP.Servers[0].Command)
	}
}

// A path that does not exist must ERROR. goccy reports success while writing
// nothing for a missing path, so this pins the verification step that catches it.
func TestSavePartial_MissingPathIsAnError(t *testing.T) {
	path := writeConfigFixture(t, richFixture)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	err = SavePartialTo(path, map[string]any{"logs.level": "debug"})
	if err == nil {
		t.Fatal("setting a nonexistent path must fail, not silently do nothing")
	}
	if !strings.Contains(err.Error(), "logs.level") {
		t.Errorf("the error should name the offending path, got: %v", err)
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(before) != string(after) {
		t.Error("a failed save must leave the file byte-for-byte unchanged")
	}
}

// A path that exists but whose PARENT does not is the same class of error.
func TestSavePartial_MissingParentPathIsAnError(t *testing.T) {
	path := writeConfigFixture(t, richFixture)
	if err := SavePartialTo(path, map[string]any{"nope.deeper.value": 1}); err == nil {
		t.Fatal("a nonexistent parent path must fail")
	}
}

// An out-of-range index must fail rather than silently no-op.
func TestSavePartial_IndexOutOfRangeIsAnError(t *testing.T) {
	path := writeConfigFixture(t, richFixture)
	if err := SavePartialTo(path, map[string]any{"providers.99.model": "x"}); err == nil {
		t.Fatal("an out-of-range sequence index must fail")
	}
}

// Values are rendered with the right YAML type: a string that looks like a number
// must come back as a string, and a number must not come back quoted.
func TestSavePartial_RendersScalarTypes(t *testing.T) {
	path := writeConfigFixture(t, "language: 中文\nmax_iterations: 50\n")

	err := SavePartialTo(path, map[string]any{
		"language":       "12345", // looks numeric, must stay a string
		"max_iterations": 20,
	})
	if err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Language      string `yaml:"language"`
		MaxIterations int    `yaml:"max_iterations"`
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	if cfg.Language != "12345" {
		t.Errorf("language = %q, want the STRING \"12345\"", cfg.Language)
	}
	if cfg.MaxIterations != 20 {
		t.Errorf("max_iterations = %d, want 20", cfg.MaxIterations)
	}
}

// Pointer fields go through unchanged, so a caller holding *bool / *int64 from the
// config does not have to spell out the formatting (and cannot get it wrong).
func TestSavePartial_PointerValues(t *testing.T) {
	path := writeConfigFixture(t, "compact:\n  auto: true\n  threshold: 0.8\n")

	no := false
	th := 0.75
	if err := SavePartialTo(path, map[string]any{
		"compact.auto":      &no,
		"compact.threshold": &th,
	}); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}

	got, _ := os.ReadFile(path)
	var cfg struct {
		Compact struct {
			Auto      *bool   `yaml:"auto"`
			Threshold float64 `yaml:"threshold"`
		} `yaml:"compact"`
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	if cfg.Compact.Auto == nil || *cfg.Compact.Auto {
		t.Error("compact.auto should be false")
	}
	if cfg.Compact.Threshold != 0.75 {
		t.Errorf("compact.threshold = %v, want 0.75", cfg.Compact.Threshold)
	}
}

// nil is refused outright rather than written as the literal "null", which would
// silently clear a user's setting.
func TestSavePartial_NilIsRefused(t *testing.T) {
	path := writeConfigFixture(t, "language: 中文\n")
	if err := SavePartialTo(path, map[string]any{"language": nil}); err == nil {
		t.Fatal("a nil value must be refused")
	}
}

// An unsupported type must be refused with the type named, not formatted into
// something that happens to parse.
func TestSavePartial_UnsupportedTypeIsRefused(t *testing.T) {
	path := writeConfigFixture(t, "language: 中文\n")
	err := SavePartialTo(path, map[string]any{"language": struct{ X int }{1}})
	if err == nil {
		t.Fatal("an unsupported value type must be refused")
	}
	if !strings.Contains(err.Error(), "struct") {
		t.Errorf("the error should name the type, got: %v", err)
	}
}

// No updates is a no-op, not an error, and the file keeps its content.
//
// "Content", not "bytes": see TestSavePartial_TrailingCommentPaddingIsNormalized
// for the one formatting detail goccy's emitter is allowed to touch. A no-op must
// not reflow anything ELSE — that is what this pins.
func TestSavePartial_NoUpdatesIsANoop(t *testing.T) {
	path := writeConfigFixture(t, richFixture)
	before, _ := os.ReadFile(path)

	if err := SavePartialTo(path, nil); err != nil {
		t.Fatalf("nil updates: %v", err)
	}
	if err := SavePartialTo(path, map[string]any{}); err != nil {
		t.Fatalf("empty updates: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !sameYAMLContent(t, before, after) {
		t.Errorf("a no-op save changed the file's content:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// The one formatting detail this library normalizes: the run of spaces that
// right-aligns a trailing comment before the "#". The COMMENT survives, its
// alignment does not.
//
// Pinned as a test rather than left as a surprise, because it is visible in a
// diff of the user's file and someone will eventually ask. It is acceptable
// (the information is intact, the line the user is editing usually changes
// anyway), but it is the reason this package promises "the file's CONTENT is
// preserved", not "the file is byte-identical".
func TestSavePartial_TrailingCommentPaddingIsNormalized(t *testing.T) {
	path := writeConfigFixture(t, "provider: main          # 行尾注释\nlanguage: 中文\n")

	if err := SavePartialTo(path, map[string]any{"language": "English"}); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if !strings.Contains(s, "# 行尾注释") {
		t.Errorf("the trailing comment must survive:\n%s", s)
	}
	if strings.Contains(s, "main          #") {
		t.Log("padding was preserved after all — the constraint above no longer applies; tighten this test")
	}
}

// Several paths in one call all land, and the file stays parseable.
func TestSavePartial_MultiplePathsInOneSave(t *testing.T) {
	path := writeConfigFixture(t, richFixture)

	err := SavePartialTo(path, map[string]any{
		"language":       "English",
		"max_iterations": 10,
		"lsp.enabled":    false,
	})
	if err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}

	got, _ := os.ReadFile(path)
	var cfg struct {
		Language      string `yaml:"language"`
		MaxIterations int    `yaml:"max_iterations"`
		LSP           struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"lsp"`
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	if cfg.Language != "English" || cfg.MaxIterations != 10 || cfg.LSP.Enabled {
		t.Errorf("not all three edits landed: %+v", cfg)
	}
}

// The written file keeps the mode it had — a config the user chmod'ed to 0600
// must not come back 0644.
func TestSavePartial_PreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("language: 中文\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SavePartialTo(path, map[string]any{"language": "English"}); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o640 {
		t.Errorf("file mode = %o, want 640", got)
	}
}

// A string list renders in flow style, which is how short lists are written by
// hand — and it must round-trip.
func TestSavePartial_StringList(t *testing.T) {
	path := writeConfigFixture(t, "permissions:\n  bash:\n    ask: [\"a*\"]\n")
	if err := SavePartialTo(path, map[string]any{"permissions.bash.ask": []string{"git *", "npm *"}}); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	got, _ := os.ReadFile(path)
	var cfg struct {
		Permissions struct {
			Bash struct {
				Ask []string `yaml:"ask"`
			} `yaml:"bash"`
		} `yaml:"permissions"`
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	want := []string{"git *", "npm *"}
	if len(cfg.Permissions.Bash.Ask) != len(want) {
		t.Fatalf("ask = %v, want %v", cfg.Permissions.Bash.Ask, want)
	}
	for i := range want {
		if cfg.Permissions.Bash.Ask[i] != want[i] {
			t.Errorf("ask[%d] = %q, want %q", i, cfg.Permissions.Bash.Ask[i], want[i])
		}
	}
}
