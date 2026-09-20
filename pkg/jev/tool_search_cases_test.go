package jev_test

// Generator (and drift guard) for the tool-search case file.
//
// The question a tool-search gate asks is PER CASE: one candidate tool at a
// time, with that tool's own name, server, hint and summary in the text. A
// hand-written case file cannot hold that (the same tool text would be retyped
// into every case it appears in, and a retyped prompt is a DIFFERENT prompt —
// the kind of drift the measurement exists to avoid). So the case file is
// generated from two authored inputs instead:
//
//	testdata/tool-search/corpus.json   the tool texts (a fixture mirroring real MCP servers)
//	testdata/tool-search/queries.json  the judged intents, zh and en
//
// Generate:    TOOL_SEARCH_GEN=1 go test ./pkg/jev/ -run TestGenerateToolSearchCases
// Drift guard: go test ./pkg/jev/ -run TestGenerateToolSearchCases   (compares, fails on a diff)
//
// The guard is the point of the default mode: editing the corpus or the intents
// without regenerating would leave a committed measurement instrument that no
// longer describes what it measures. Once the gate itself exists
// (agent/mcp/jev_search.go), this file's question text must come from THAT
// builder instead of the constants below — the two must not be able to drift
// apart, and a lockstep test is step 4 of docs/2026-09-20-jev-tool-search-design.md.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	toolSearchDir       = "testdata/tool-search"
	toolSearchCasesPath = "testdata/tool-search-noul.json"
	// toolSearchGenEnv turns the drift guard into a writer.
	toolSearchGenEnv = "TOOL_SEARCH_GEN"
)

// The question text. English, like the other case files and like the tool
// descriptions the model sees; the STATE is whatever language the model wrote
// its query in, which is the variable this file is about.
const (
	// toolSearchFrame is the file-level instruction: the shape of the question
	// with the candidate left out. It is never sent — every case overrides it —
	// but it is what the file MEANS, and validateEvalFile requires it.
	toolSearchFrame = "The state is a request from the user. Decide whether the tool described below can carry that request out directly."
	// toolSearchQuestionFmt is the production per-candidate text: the tool's
	// name, the server it lives on, the keywords SearchHint carries, and the
	// first line of its description — then the question itself.
	toolSearchQuestionFmt = "Tool %s (server: %s; related terms: %s): %s\nCan this tool carry the request out directly?"

	// The criteria. Supplying them is what makes the probability
	// interpretable: without them the model guesses what "yes" covers, and the
	// guess leans toward the label it is about to pick.
	toolSearchYes = "Calling this tool now would directly carry out what the request asks for."
	toolSearchNo  = "The request asks for something else, or this tool needs input the request does not provide, or its action is only RELATED to what was asked rather than the same action."
)

// toolRecord is one entry of the corpus fixture.
type toolRecord struct {
	Name    string `json:"name"`
	Server  string `json:"server"`
	Hint    string `json:"hint"`
	Summary string `json:"summary"`
}

type toolCorpus struct {
	Notes string       `json:"notes"`
	Tools []toolRecord `json:"tools"`
}

// toolIntent is one judged intent, written once per language.
type toolIntent struct {
	ID     string   `json:"id"`
	ZH     string   `json:"zh"`
	EN     string   `json:"en"`
	Expect []string `json:"expect"`
	// Distract are near-misses (same domain, different action) or, in the
	// no_tool arm, tools that must NOT be offered at all.
	Distract []string `json:"distract"`
}

type toolQuerySet struct {
	Notes   string       `json:"notes"`
	Intents []toolIntent `json:"intents"`
	NoTool  []toolIntent `json:"no_tool"`
}

func loadJSON[T any](t *testing.T, path string) T {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return v
}

// buildToolSearchCases turns the two authored inputs into the case file.
//
// One intent in one language emits one case per tool it names: the tool that
// should do the job (expect true) and the near-misses (expect false). Two
// languages of one intent share a `pair`, so the report can show whether the
// SAME judgment moved when only the language changed.
func buildToolSearchCases(t *testing.T) evalCaseFile {
	t.Helper()
	corpus := loadJSON[toolCorpus](t, filepath.Join(toolSearchDir, "corpus.json"))
	queries := loadJSON[toolQuerySet](t, filepath.Join(toolSearchDir, "queries.json"))

	byName := make(map[string]toolRecord, len(corpus.Tools))
	for _, tool := range corpus.Tools {
		if _, dup := byName[tool.Name]; dup {
			t.Fatalf("corpus names %s twice", tool.Name)
		}
		byName[tool.Name] = tool
	}

	instructions := func(name string) string {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("queries name %s, which the corpus does not define", name)
		}
		return fmt.Sprintf(toolSearchQuestionFmt, tool.Name, tool.Server, tool.Hint, tool.Summary)
	}

	var cases []evalCase
	add := func(intent toolIntent, lang, query string, arm string, tools []string, expect string) {
		for _, name := range tools {
			cases = append(cases, evalCase{
				ID:    fmt.Sprintf("%s-%s-%s-%s", intent.ID, lang, caseIDFor(name), expect),
				Group: lang + "-" + arm,
				// Pair members are the same (intent, tool, label) in two
				// languages — the comparison that answers "does the language
				// move this judgment". The label is part of the key because a
				// pair with disagreeing labels is an authoring error to the
				// harness, and one tool may legitimately be a distractor for
				// one intent and the answer for another.
				Pair:         fmt.Sprintf("%s-%s-%s", intent.ID, caseIDFor(name), expect),
				Expect:       expect,
				State:        mustJSON(t, query),
				Instructions: instructions(name),
			})
		}
	}
	for _, intent := range queries.Intents {
		add(intent, "zh", intent.ZH, "match", intent.Expect, "true")
		add(intent, "zh", intent.ZH, "match", intent.Distract, "false")
		add(intent, "en", intent.EN, "match", intent.Expect, "true")
		add(intent, "en", intent.EN, "match", intent.Distract, "false")
	}
	for _, intent := range queries.NoTool {
		add(intent, "zh", intent.ZH, "nomatch", intent.Distract, "false")
		add(intent, "en", intent.EN, "nomatch", intent.Distract, "false")
	}

	file := evalCaseFile{
		Name:        "tool-search-noul",
		ActionLabel: "true",
		Notes: "Generated by tool_search_cases_test.go from testdata/tool-search/ — edit the corpus or the " +
			"intents and regenerate, never this file. Groups are <lang>-<match|nomatch>: `nomatch` is the arm " +
			"where the right answer is no tool at all, and every tool the gate lets through there is a wrong " +
			"tool offered to the model. Pairs link the zh and en forms of one intent, which is where a " +
			"language-shaped threshold shows up. Per-case instructions ARE the production question text " +
			"(see the design doc §六).",
		Question: evalQuestion{
			Type:         "noul",
			Instructions: toolSearchFrame,
			Yes:          toolSearchYes,
			No:           toolSearchNo,
		},
		Cases: cases,
	}
	file.Notes += "\n\nIntents: " + queries.Summary()
	return file
}

// Summary is the authoring-side view of what the file contains, carried into
// the report so a reader can tell which intents a bad number came from.
func (q toolQuerySet) Summary() string {
	var ids []string
	for _, i := range q.Intents {
		ids = append(ids, i.ID)
	}
	var none []string
	for _, i := range q.NoTool {
		none = append(none, i.ID)
	}
	return fmt.Sprintf("%d matched + %d no-tool → %s | no-tool: %s",
		len(q.Intents), len(q.NoTool), strings.Join(ids, ", "), strings.Join(none, ", "))
}

// caseIDFor makes a tool name usable inside a case id: the mcp__ prefix and
// the server separator go away, because the id is there to be read in a table.
func caseIDFor(tool string) string {
	return strings.ReplaceAll(strings.TrimPrefix(tool, "mcp__"), "__", "-")
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal case state: %v", err)
	}
	return raw
}

func TestGenerateToolSearchCases(t *testing.T) {
	file := buildToolSearchCases(t)
	if err := validateEvalFile(file); err != nil {
		t.Fatalf("generated case file is not well formed: %v", err)
	}
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("marshal case file: %v", err)
	}
	out = append(out, '\n')

	if os.Getenv(toolSearchGenEnv) == "1" {
		if err := os.WriteFile(toolSearchCasesPath, out, 0o644); err != nil {
			t.Fatalf("write %s: %v", toolSearchCasesPath, err)
		}
		t.Logf("wrote %s: %d cases", toolSearchCasesPath, len(file.Cases))
		return
	}

	got, err := os.ReadFile(toolSearchCasesPath)
	if err != nil {
		t.Fatalf("read %s (generate it with %s=1): %v", toolSearchCasesPath, toolSearchGenEnv, err)
	}
	if string(got) != string(out) {
		t.Fatalf("%s is stale — the corpus or the intents changed. Regenerate with %s=1 go test ./pkg/jev/ -run TestGenerateToolSearchCases",
			toolSearchCasesPath, toolSearchGenEnv)
	}
}
