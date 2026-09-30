package jevfiles

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/pkg/jev"
)

// The state is the question's other half, so what it says — and what it refuses to
// say — is what the sweep measures. These assertions are about the SHAPE (a fact
// per line, in a fixed order, payloads inside an indented block) and about the two
// things a reader has to be able to tell apart: a call that creates a file from one
// that overwrites, and a rule that matched from one that did not.
func TestStateRendersTheFactsPerTool(t *testing.T) {
	base := Call{
		Path:  "/Users/dev/projects/app/src/main.go",
		Rule:  "",
		CWD:   "/Users/dev/projects/app/src",
		Trees: []string{"/Users/dev/projects/app"},
	}

	read := State(Call{Tool: ToolRead, Path: base.Path, Exists: true, Size: 4096, CWD: base.CWD, Trees: base.Trees})
	if !strings.Contains(read, "Tool: ReadFile") || !strings.Contains(read, "Target file: "+base.Path) {
		t.Fatalf("a read state must name the tool and the path:\n%s", read)
	}
	if !strings.Contains(read, "The file exists: yes (4096 bytes)") {
		t.Fatalf("existence and size are facts the criteria lean on:\n%s", read)
	}
	if strings.Contains(read, "Content it would write") || strings.Contains(read, "would replace") {
		t.Fatalf("a read has no payload block:\n%s", read)
	}

	write := State(Call{Tool: ToolWrite, Path: base.Path, Exists: false, CWD: base.CWD, Trees: base.Trees})
	if !strings.Contains(write, "The file exists: no (this call would create it)") {
		t.Fatalf("creating and overwriting are different actions:\n%s", write)
	}

	dir := State(Call{Tool: ToolRead, Path: base.Path, Exists: true, Dir: true, CWD: base.CWD})
	if !strings.Contains(dir, "The file exists: yes (it is a directory)") {
		t.Fatalf("a directory has no size to state:\n%s", dir)
	}
}

// The state describes the TARGET and never the payload: what a call would write is
// model-generated text, and the decision is about the path (a deploy script written
// to /tmp is decided by where it lands, not by what is in it).
func TestStateCarriesNoPayload(t *testing.T) {
	for _, tool := range []Tool{ToolRead, ToolWrite, ToolEdit} {
		state := State(Call{
			Tool: tool, Path: "/repo/x.go", Exists: true, Size: 10,
			CWD: "/repo", Trees: []string{"/repo"},
		})
		for _, forbidden := range []string{"Content it would write", "would replace", "characters):"} {
			if strings.Contains(state, forbidden) {
				t.Fatalf("no payload block belongs in a %s state:\n%s", tool, state)
			}
		}
		if !strings.Contains(state, "Target file: /repo/x.go") {
			t.Fatalf("the target is the whole subject:\n%s", state)
		}
	}
}

// The rule slot never renders as an empty string, and never invents the bash
// gate's unparseable sentinel: a path always parses.
func TestStateRuleSlot(t *testing.T) {
	none := State(Call{Tool: ToolRead, Path: "/repo/x", CWD: "/repo"})
	if !strings.Contains(none, "Matched ask rule: (none — no rule matched; this call is allowed by default)") {
		t.Fatalf("an unmatched call says so:\n%s", none)
	}
	matched := State(Call{Tool: ToolRead, Path: "/repo/x", Rule: "secrets/*", CWD: "/repo"})
	if !strings.Contains(matched, `Matched ask rule: "secrets/*"`) {
		t.Fatalf("a matched rule is quoted:\n%s", matched)
	}
	if strings.Contains(matched, "no-rule:unparseable") {
		t.Fatal("the bash-only sentinel must never reach a file state")
	}
}

// No trees known is stated as unknown rather than left to read as "nothing is
// outside anything".
func TestStateSaysWhenNoTreeIsKnown(t *testing.T) {
	state := State(Call{Tool: ToolRead, Path: "/repo/x", CWD: "/repo"})
	if !strings.Contains(state, "(unknown — treat the working directory above as the tree)") {
		t.Fatalf("a session with no tree must say so:\n%s", state)
	}
}

// The question is a noul with both poles supplied — the poles are what make a
// probability interpretable — and the label the gate thresholds on is one the
// answer can actually take.
func TestQuestionShape(t *testing.T) {
	q := Question()
	noul, ok := q.Criteria.(jev.NoulCriteria)
	if !ok {
		t.Fatalf("want a noul question, got %T", q.Criteria)
	}
	if noul.Yes == "" || noul.No == "" {
		t.Fatal("both poles must be supplied, or the model guesses what yes covers")
	}
	if q.Instructions == "" {
		t.Fatal("the instructions state the judgment in full; the question's key is not sent")
	}
	if ActionLabel != "true" {
		t.Fatalf("ActionLabel is the label a probability is read against, got %q", ActionLabel)
	}
	if QuestionID == "" {
		t.Fatal("the gate and the case file have to agree on the question id")
	}
}

// The criteria have to keep the two cases that pull in opposite directions: a read
// is not automatically safe (a key is the clearest needs-a-look case), and an
// edit inside the tree is not automatically risky (it is the work itself).
func TestCriteriaKeepBothDirections(t *testing.T) {
	if !strings.Contains(yes, "credentials") || !strings.Contains(yes, "outside the trees") {
		t.Fatal("the yes pole must cover secrets AND the tree boundary")
	}
	if !strings.Contains(no, "inside the trees") || !strings.Contains(no, "reading a file") {
		t.Fatal("the no pole must say what routine work is, reads included")
	}
}
