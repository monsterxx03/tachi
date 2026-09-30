package jevask

// Generator for the Bash judgment gate's case file.
//
// pkg/jev's harness can only measure a state this package BUILT: a hand-written
// case file measures a prompt nobody sends, so the state in the file has to come
// from the same function the gate calls (docs/agents/jev.md, "Measuring a
// threshold"). The authored material is one file of commands with labels —
// pkg/jev/testdata/bash-ask/source.json — and every state, the question and the
// parsed/unparsed half of each group are rendered here.
//
//	BASH_ASK_CASES_SRC=$PWD/pkg/jev/testdata/bash-ask/source.json \
//	BASH_ASK_CASES_OUT=$PWD/pkg/jev/testdata \
//	go test ./agent/permission/jevask/ -run TestGenerateBashAskCases -v
//
// Both paths are absolute on purpose: `go test` runs the package's own directory,
// so a repo-relative path resolves somewhere else. Both are required and the test
// skips without them — one is authored material, the other a repo path, and
// neither has a default worth guessing. The generated file IS committed (the
// sweep has to be reproducible from it, and the source carries nothing
// deployment-specific), which makes hand edits to it pointless: the notes written
// into it say so.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/monsterxx03/tachi/agent/permission"
)

const (
	// srcEnv names the authored case file.
	srcEnv = "BASH_ASK_CASES_SRC"
	// outDirEnv names the directory the case file is written to — pkg/jev/testdata,
	// which is where the harness globs testdata/*.json.
	outDirEnv = "BASH_ASK_CASES_OUT"
	// caseFileName is the generated file, and what a sweep command passes to
	// JEV_EVAL_FILE.
	caseFileName = "bash-ask.json"
)

// sourceCase is one authored command: the ONLY authored part is the command, its
// label, and (when a rule matched it) that rule's text.
type sourceCase struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	// Rule is the ask rule the policy matched, verbatim. Empty means nothing
	// matched — which, since the default is allow, is the population no rule is
	// watching today.
	Rule string `json:"rule"`
	// Expect is the label: "true" = a careful user should see it first.
	Expect string `json:"expect"`
	// Note is carried into the generated file's per-case comment... which JSON
	// has no room for, so it documents the source and the review, not the file.
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
	// Group is DERIVED, not authored: parsed or unparsed comes from the same
	// parser the policy uses, so a case cannot be filed under a population it is
	// not in. The per-case table then shows whether failures cluster on one half.
	Group  string `json:"group"`
	Expect string `json:"expect"`
	State  string `json:"state"`
}

// Fixed session facts for every case. Constant on purpose: the only variables the
// sweep should see are the command and the rule.
const (
	caseCWD   = "/Users/dev/projects/app"
	caseRoot1 = "/Users/dev/projects/app"
	caseRoot2 = "/Users/dev/projects/shared-lib"
)

func TestGenerateBashAskCases(t *testing.T) {
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
			// A rule that matched and still says "run" is not a case this gate
			// can learn anything from: the rule is the user's own words.
			t.Fatalf("case %q: a rule matched, so the label is the rule's and must be true", c.ID)
		}
		state := State(Cmd{
			Command: c.Command,
			Rule:    c.Rule,
			CWD:     caseCWD,
			Roots:   []string{caseRoot1, caseRoot2},
		})
		out.Cases = append(out.Cases, genCase{
			ID:     c.ID,
			Group:  groupOf(c.Command, c.Expect),
			Expect: c.Expect,
			State:  state,
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

// groupOf derives the population: parsed or not (from the production parser), and
// the label. Naming it here means the corpus cannot claim a case is unparseable
// when it is not.
func groupOf(command, expect string) string {
	which := "ask"
	if expect == "false" {
		which = "run"
	}
	if _, err := permission.Segments(command); err != nil {
		return fmt.Sprintf("unparsed-%s", which)
	}
	return fmt.Sprintf("parsed-%s", which)
}
