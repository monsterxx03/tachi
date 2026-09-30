package jevfiles

// Generator for the file judgment gate's case file.
//
// pkg/jev's harness can only measure a state this package BUILT: a hand-written
// case file measures a prompt nobody sends, so the state in the file has to come
// from the same function the gate calls (docs/agents/jev.md, "Measuring a
// threshold"). The authored material is one file of calls with labels —
// pkg/jev/testdata/files-ask/source.json — and every state, the question and the
// tool+label grouping are rendered here.
//
//	FILES_ASK_CASES_SRC=$PWD/pkg/jev/testdata/files-ask/source.json \
//	FILES_ASK_CASES_OUT=$PWD/pkg/jev/testdata \
//	go test ./agent/permission/jevfiles/ -run TestGenerateFilesAskCases -v
//
// Both paths are absolute on purpose: `go test` runs the package's own directory,
// so a repo-relative path resolves somewhere else. Both are required and the test
// skips without them — one is authored material, the other a repo path, and
// neither has a default worth guessing. The generated file IS committed (the sweep
// has to be reproducible from it, and the source carries nothing
// deployment-specific), which makes hand edits to it pointless.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	// srcEnv names the authored case file.
	srcEnv = "FILES_ASK_CASES_SRC"
	// outDirEnv names the directory the case file is written to — pkg/jev/testdata,
	// which is where the harness globs testdata/*.json.
	outDirEnv = "FILES_ASK_CASES_OUT"
	// caseFileName is the generated file, and what a sweep command passes to
	// JEV_EVAL_FILE.
	caseFileName = "files-ask.json"
)

// sourceCase is one authored call: the ONLY authored part is the call's own
// fields, its label, and (when a rule matched it) that rule's text.
type sourceCase struct {
	ID   string `json:"id"`
	Tool string `json:"tool"` // "read", "write" or "edit"
	Path string `json:"path"`
	// Exists and Size are what a stat would say, authored rather than taken from
	// this machine's filesystem: a case file has to render the same state on every
	// machine that regenerates it.
	Exists bool  `json:"exists"`
	Size   int64 `json:"size,omitempty"`
	// Rule is the ask rule the policy matched, verbatim. Empty means nothing
	// matched — which, since the default is allow, is the population no rule is
	// watching today.
	Rule string `json:"rule,omitempty"`
	// Expect is the label: "true" = a careful user should see it first.
	Expect string `json:"expect"`
	// Note documents the source and the review, not the file: JSON has no room for
	// a comment beside a case.
	Note string `json:"note,omitempty"`
}

type sourceFile struct {
	Name  string       `json:"name"`
	Notes string       `json:"notes"`
	Cases []sourceCase `json:"cases"`
}

// The generated file's shape: exactly what pkg/jev's evalCaseFile reads.
type genFile struct {
	Name        string      `json:"name"`
	Notes       string      `json:"notes"`
	ActionLabel string      `json:"action_label"`
	Question    genQuestion `json:"question"`
	Cases       []genCase   `json:"cases"`
}

type genQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Yes          string `json:"yes"`
	No           string `json:"no"`
}

type genCase struct {
	ID string `json:"id"`
	// Group is DERIVED, not authored: the tool and the label. The per-case table
	// then shows whether failures cluster on one tool — a gate that is right about
	// reads and wrong about writes is a different bug from a bad threshold.
	Group  string `json:"group"`
	Expect string `json:"expect"`
	State  string `json:"state"`
}

// Fixed session facts for every case. Constant on purpose: the only variables the
// sweep should see are the call and the rule. They match the module doc of the
// gate and the source file's own note.
const caseCWD = "/Users/dev/projects/app/src"

func TestGenerateFilesAskCases(t *testing.T) {
	srcPath := os.Getenv(srcEnv)
	outDir := os.Getenv(outDirEnv)
	if srcPath == "" || outDir == "" {
		t.Skipf("set %s and %s to regenerate the case file", srcEnv, outDirEnv)
	}

	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	var src sourceFile
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatalf("parse source: %v", err)
	}
	if len(src.Cases) == 0 {
		t.Fatal("source has no cases")
	}

	out := genFile{
		Name:        src.Name,
		Notes:       src.Notes,
		ActionLabel: ActionLabel,
		Question: genQuestion{
			Type:         "noul",
			Instructions: instructions,
			Yes:          yes,
			No:           no,
		},
	}
	seen := make(map[string]bool, len(src.Cases))
	for _, c := range src.Cases {
		if c.ID == "" {
			t.Fatal("every case needs an id")
		}
		if seen[c.ID] {
			t.Fatalf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		if c.Expect != "true" && c.Expect != "false" {
			t.Fatalf("case %q: expect must be \"true\" or \"false\", got %q", c.ID, c.Expect)
		}
		if c.Rule != "" && c.Expect != "true" {
			// A rule that matched and still says "run" is not a case this gate can
			// learn anything from: the rule is the user's own words.
			t.Fatalf("case %q: a rule matched, so the label is the rule's and must be true", c.ID)
		}
		tool, err := sourceTool(c.Tool)
		if err != nil {
			t.Fatalf("case %q: %v", c.ID, err)
		}
		group, err := groupOf(c.Tool, c.Expect)
		if err != nil {
			t.Fatalf("case %q: %v", c.ID, err)
		}
		call := Call{
			Tool:   tool,
			Path:   c.Path,
			Exists: c.Exists,
			Size:   c.Size,
			Rule:   c.Rule,
			CWD:    caseCWD,
			Trees:  caseTreeList(),
		}
		out.Cases = append(out.Cases, genCase{
			ID: c.ID, Group: group, Expect: c.Expect, State: State(call),
		})
	}
	sort.Slice(out.Cases, func(i, j int) bool { return out.Cases[i].ID < out.Cases[j].ID })

	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dst := filepath.Join(outDir, caseFileName)
	if err := os.WriteFile(dst, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	t.Logf("wrote %s (%d cases)", dst, len(out.Cases))
}

// sourceTool maps the authored lowercase name onto the Tool the state renders. An
// unknown name is fatal rather than defaulted: a case filed under a tool it is not
// measures a question nobody asks.
func sourceTool(name string) (Tool, error) {
	switch name {
	case "read":
		return ToolRead, nil
	case "write":
		return ToolWrite, nil
	case "edit":
		return ToolEdit, nil
	}
	return "", fmt.Errorf("unknown tool %q (want read, write or edit)", name)
}

// groupOf derives the population: the tool and the label. Both halves of every
// tool must be present, which is how the harness can tell a threshold from a
// gate that never fires.
func groupOf(tool, expect string) (string, error) {
	which := "ask"
	if expect == "false" {
		which = "run"
	}
	if _, err := sourceTool(tool); err != nil {
		return "", err
	}
	return strings.ToLower(tool) + "-" + which, nil
}

// caseTreeList is the corpus's fixed tree set, spelled once.
func caseTreeList() []string {
	return []string{"/Users/dev/projects/app", "/Users/dev/projects/shared-lib"}
}
