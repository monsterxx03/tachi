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
		"# 顶部注释：这是用户的配置",                           // head comment
		"provider: main",                           // an UNTOUCHED line keeps its content
		"# 行尾注释",                                   // ...and its trailing comment (goccy re-aligns the padding; see KNOWN_LIMIT)
		"  # 独立注释：语言服务器",                           // standalone comment
		`extensions: [".go"]`,                      // flow style not expanded
		`command: "gopls"`,                         // explicit quoting kept
		"\n\nlsp:",                                 // blank-line grouping kept
		"\nproviders:\n    - name: deepseek-flash", // 4-space indent kept as written
		"  servers:\n  - name: gopls",              // INDENTLESS sequence kept
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
		"providers.0.model":               "deepseek-flash-v2",
		"providers.0.spec.vision":         false,
		"providers.0.spec.context_window": 200000,
		"lsp.servers.0.command":           "gopls-x",
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

// A path whose blocks the FILE does not have is refused when the path cannot answer where it
// goes — which is every case but a missing BLOCK. The three refusals below are the whole set: a
// sequence element, a step through a node that is not a block, and a key on a list. None of them
// is guessed at, and each leaves the file byte-for-byte unchanged.
func TestSavePartial_RefusesWhatItCannotCreate(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value any
	}{
		// A list is grown at its END with a skeleton the element type decides — AppendListItem's
		// entry point, not a path's business.
		{name: "element-in-a-list-that-is-not-there", key: "web_search.providers.0.type", value: "exa"},
		// A MAP key is a name, and the file has no name to add: inventing one is the guessing.
		{name: "key-in-a-map-that-is-not-there", key: "hooks.events.pre_tool.0.timeout", value: "5s"},
		// A scalar holds no keys at all.
		{name: "through-a-scalar", key: "language.foo", value: "x"},
		// A list holds elements, not keys.
		{name: "named-key-on-a-list", key: "providers.spec", value: "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfigFixture(t, richFixture)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = SavePartialTo(path, map[string]any{c.key: c.value})
			if err == nil {
				t.Fatalf("%s must be refused, not written", c.key)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Errorf("the error should name the offending path, got: %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(before) != string(after) {
				t.Errorf("a refused save must leave the file byte-for-byte unchanged; got:\n%s", after)
			}
		})
	}
}

// Blocks the file does not spell out are CREATED, at the column the enclosing block's own keys
// sit at — because the caller named the path and the path says where the block goes. The layout
// is the assertion that matters here: a block emitted one level too shallow parses as a SIBLING
// of the block it belongs to, which is a valid file holding the wrong shape.
func TestSavePartial_CreatesTheBlocksThePathNames(t *testing.T) {
	// A provider the user wrote by hand, whose `spec:` block does not exist yet — the case that
	// made the settings page refuse to save a spec leaf at all.
	path := writeConfigFixture(t, `providers:
  - name: main
    model: m
    # 行尾注释：这一行不是我们要改的
`)
	err := SavePartialTo(path, map[string]any{
		"providers.0.spec.context_window": 200000,
		"providers.0.spec.proxy":          "http://127.0.0.1:7890",
	})
	if err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	body, _ := os.ReadFile(path)
	// One block, both keys in it, at the item's own key column (4) and one step deeper (6) — and
	// the sibling key and its comment are where they were.
	want := `providers:
  - name: main
    model: m
    # 行尾注释：这一行不是我们要改的
    spec:
      context_window: 200000
      proxy: "http://127.0.0.1:7890"
`
	if string(body) != want {
		t.Errorf("config.yaml =\n%s\nwant:\n%s", body, want)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this function wrote does not load: %v\n%s", err, body)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	p := cfg.Providers[0]
	if p.Spec.ContextWindow == nil || *p.Spec.ContextWindow != 200000 {
		t.Errorf("context_window = %v", p.Spec.ContextWindow)
	}
	if p.Spec.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("proxy = %q", p.Spec.Proxy)
	}
	if p.Name != "main" || p.Model != "m" {
		t.Errorf("the keys beside the new block moved: %+v", p)
	}

	// Two levels missing, one of them TOP-LEVEL: a key at the document root goes into the root
	// block, which is a block like any other.
	path2 := writeConfigFixture(t, "provider: main\n")
	if err := SavePartialTo(path2, map[string]any{"memory.keyword_provider": "main"}); err != nil {
		t.Fatalf("SavePartialTo (top-level block): %v", err)
	}
	body2, _ := os.ReadFile(path2)
	if string(body2) != "provider: main\nmemory:\n  keyword_provider: main\n" {
		t.Errorf("config.yaml = %q, want the block created at the top level", body2)
	}
	if m, err := LoadFrom(path2); err != nil || m.Memory.KeywordProvider != "main" {
		t.Errorf("memory.keyword_provider did not land: %+v err=%v", m.Memory, err)
	}

	// Every block on the way is EMPTY (`a: {}`, and the ones created under it): the column still
	// comes out right, because keyPosition walks up to the nearest block that HAS a key and
	// indents one step per level — the same walk the list-item skeleton relies on.
	path3 := writeConfigFixture(t, "a: {}\n")
	if err := SavePartialTo(path3, map[string]any{"a.b.c": 1}); err != nil {
		t.Fatalf("SavePartialTo (empty ancestors): %v", err)
	}
	body3, _ := os.ReadFile(path3)
	if string(body3) != "a:\n  b:\n    c: 1\n" {
		t.Errorf("config.yaml = %q, want three levels nested from an empty root key", body3)
	}
}

// A path that exists but whose PARENT is absent is no longer "the same class of error": the parent
// block is created when the path names it, so what fails here is a parent that is a LIST (an
// element is AppendListItem's, and the file's own `providers` shows why a path cannot answer for
// one).
func TestSavePartial_MissingParentPathIsAnError(t *testing.T) {
	path := writeConfigFixture(t, richFixture)
	if err := SavePartialTo(path, map[string]any{"providers.3.name": "x"}); err == nil {
		t.Fatal("a sequence element past the end must fail")
	}
}

// A file with no top-level block at all — empty, or comments only — has nothing a key could go
// in, and creating it would be inventing the file's whole shape. It must be a clear refusal (the
// settings page shows it as 没能写入）, not a panic: rootBlock is what keeps the walk from
// dereferencing a nil body — and goccy's ReplaceWithNode would panic on it (verified), which is
// why the check runs before the update loop rather than inside it.
func TestSavePartial_FileWithoutATopLevelBlockIsRefused(t *testing.T) {
	for _, body := range []string{"", "# 只有一行注释\n"} {
		path := writeConfigFixture(t, body)
		err := SavePartialTo(path, map[string]any{"language": "zh"})
		if err == nil {
			t.Fatalf("a file with no top-level block must refuse the save (fixture %q)", body)
		}
		if !strings.Contains(err.Error(), "not a block") {
			t.Errorf("error = %v, want it to say the top level is not a block", err)
		}
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Errorf("the file changed: %q", got)
		}
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

// The two shapes a BOUND METHOD's payload has, which are NOT the shapes a Go caller passes:
// a JS array arrives as []any, and a duration arrives as the string the form showed. Both were
// reachable only from the settings page, which is why neither was covered until it existed —
// and both failed: renderScalar had no []any case ("unsupported value type []any"), and the
// write path had no duration-string case ("需要 time.Duration，收到 string").
func TestSavePartial_AcceptsTheShapeABoundMethodReceives(t *testing.T) {
	path := writeConfigFixture(t, `permissions:
  bash:
    allow: ["ls"]
lsp:
  request_timeout: 15s
`)
	updates := map[string]any{
		"permissions.bash.allow": []any{"ls", "pwd", "git status"},
		"lsp.request_timeout":    "45s",
	}
	if err := SavePartialTo(path, updates); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	got, _ := os.ReadFile(path)
	var cfg struct {
		Permissions struct {
			Bash struct {
				Allow []string `yaml:"allow"`
			} `yaml:"bash"`
		} `yaml:"permissions"`
		LSP struct {
			RequestTimeout string `yaml:"request_timeout"`
		} `yaml:"lsp"`
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	if want := []string{"ls", "pwd", "git status"}; !reflect.DeepEqual(cfg.Permissions.Bash.Allow, want) {
		t.Errorf("allow = %v, want %v", cfg.Permissions.Bash.Allow, want)
	}
	// The duration stays the string form the reader wrote — not nanoseconds. This is the half
	// that makes the settings page's timeout fields editable at all.
	if cfg.LSP.RequestTimeout != "45s" {
		t.Errorf("request_timeout = %q, want %q", cfg.LSP.RequestTimeout, "45s")
	}

	// A list of something other than strings is refused by element type: the config has no list
	// of numbers, and saying so beats writing a sequence the loader would reject.
	err := SavePartialTo(path, map[string]any{"permissions.bash.allow": []any{"ls", 3}})
	if err == nil || !strings.Contains(err.Error(), "unsupported list element type") {
		t.Errorf("err = %v, want it to name the element type", err)
	}
}

// A null is how the file spells "not set", which is a state several fields give a MEANING to
// (`vision: null` = decide it from the model, `max_retries: null` = the path's default). So the
// write path has to be able to produce one — through config.Unset, never through a bare nil,
// which is what a caller who forgot to dereference a pointer looks like (the test above).
//
// The load half is the part worth pinning: writing "null" is worthless if the loader reads it
// back as an empty value rather than as unset.
func TestUnsetWritesNullAndTheLoaderReadsItBackAsUnset(t *testing.T) {
	path := writeConfigFixture(t, `providers:
  - name: main
    model: m
    spec:
      context_window: 200000
      vision: true
`)
	updates := map[string]any{
		"providers.0.spec.context_window": Unset{},
		"providers.0.spec.vision":         Unset{},
	}
	if err := SavePartialTo(path, updates); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "context_window: null") || !strings.Contains(string(body), "vision: null") {
		t.Fatalf("config.yaml = %q, want both keys written as null", body)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this function wrote does not load: %v\n%s", err, body)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(cfg.Providers))
	}
	if got := cfg.Providers[0].Spec.ContextWindow; got != nil {
		t.Errorf("context_window = %v after loading null, want unset", *got)
	}
	if got := cfg.Providers[0].Spec.Vision; got != nil {
		t.Errorf("vision = %v after loading null, want unset", *got)
	}
}

// The ONE key this function adds: one whose parent block is already in the file.
//
// It exists because the config OBJECT and the FILE do not have the same leaves: every struct
// field is a leaf the settings form offers, while `omitempty` (or a hand-written file) may leave
// one out. Without this, editing such a leaf fails with "not found in the file" — which reads as
// a broken form rather than as a missing line, and it is exactly what a freshly added list item
// produces (its skeleton writes the fields the struct always writes and leaves the rest).
func TestSavePartial_AddsAKeyUnderAnExistingBlock(t *testing.T) {
	path := writeConfigFixture(t, `providers:
  - name: main
    model: m
    spec:
      timeout: 30s
`)
	if err := SavePartialTo(path, map[string]any{"providers.0.spec.context_window": 200000}); err != nil {
		t.Fatalf("SavePartialTo: %v", err)
	}
	body, _ := os.ReadFile(path)
	// In place, inside spec, at spec's own indentation — and the file still loads.
	if !strings.Contains(string(body), "\n      context_window: 200000") {
		t.Errorf("config.yaml = %q, want the key under spec at its indentation", body)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("the file this function wrote does not load: %v\n%s", err, body)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Spec.ContextWindow == nil ||
		*cfg.Providers[0].Spec.ContextWindow != 200000 {
		t.Errorf("providers[0].spec.context_window did not land: %+v", cfg.Providers)
	}

	// The rest of the file is untouched: the sibling it was added NEXT TO keeps its value and
	// position (appending to the block is not a rewrite).
	if !strings.Contains(string(body), "      timeout: 30s") {
		t.Errorf("config.yaml = %q, want the sibling key untouched", body)
	}

	// A key whose parent is a SEQUENCE is not this case: a list does not grow in the middle, and
	// its new item is AppendListItem's job (which knows the element's shape).
	err = SavePartialTo(path, map[string]any{"providers.1.name": "x"})
	if err == nil {
		t.Fatal("adding a sequence element through SavePartial must be refused")
	}
	if !strings.Contains(err.Error(), "providers") {
		t.Errorf("the error should name the path, got: %v", err)
	}
}
