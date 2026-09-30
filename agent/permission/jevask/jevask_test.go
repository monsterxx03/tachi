package jevask

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/pkg/jev"
)

func TestStateParsedCommand(t *testing.T) {
	got := State(Cmd{
		Command: "cd /tmp && git branch -D feature",
		Rule:    "git branch -D *",
		CWD:     "/Users/dev/projects/app/src",
		Root:    "/Users/dev/projects/app",
	})

	// The segments are the PARSER's canonical output, not the raw text: the
	// judgment and the rules have to be looking at the same command.
	for _, want := range []string{
		"Command (canonical segments, one per line):",
		"  1. cd /tmp",
		"  2. git branch -D feature",
		"Working directory: /Users/dev/projects/app/src",
		"Project root (the tree the rules are built against): /Users/dev/projects/app",
		`Matched ask rule: "git branch -D *"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("state is missing %q\n---\n%s", want, got)
		}
	}
}

// TestStateRuleSlot: "a rule matched", "nothing matched" and "nothing could be
// parsed" are three different inputs. Writing an empty slot for the last two
// would make the model guess why the command is at the gate at all.
func TestStateRuleSlot(t *testing.T) {
	noRule := State(Cmd{Command: "go test ./...", CWD: "/w"})
	if !strings.Contains(noRule, "(none — no rule matched; this command is allowed by default)") {
		t.Errorf("unmatched command must say so, got:\n%s", noRule)
	}

	unparsed := State(Cmd{Command: `echo "unclosed`, CWD: "/w"})
	if !strings.Contains(unparsed, "could not be parsed into segments") {
		t.Errorf("unparseable command must be labelled, got:\n%s", unparsed)
	}
	if !strings.Contains(unparsed, `echo "unclosed`) {
		t.Errorf("an unparseable command is judged AS WRITTEN, got:\n%s", unparsed)
	}
	if !strings.Contains(unparsed, "(none — the command could not be parsed") {
		t.Errorf("unparseable command must not claim a rule was consulted, got:\n%s", unparsed)
	}
}

// TestStateIndentsScripts: a heredoc arrives as one blob, and without indenting
// its second line reads as another field of the state.
func TestStateIndentsScripts(t *testing.T) {
	got := State(Cmd{Command: "cat > /tmp/x.sh <<'EOF'\nrm -rf $HOME\nEOF", CWD: "/w"})
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "rm -rf") || strings.HasPrefix(line, "EOF") {
			t.Fatalf("script body must be indented, found a bare line %q\n---\n%s", line, got)
		}
	}
}

func TestStateTruncatesHugeCommands(t *testing.T) {
	got := State(Cmd{Command: "echo " + strings.Repeat("x", maxCommandChars+100), CWD: "/w"})
	if !strings.Contains(got, "… (truncated)") {
		t.Error("a truncated command must say so — a judged prefix is not the whole command")
	}
}

// TestStateRendersTheRequestLine: the request field is empty in production (the
// paired experiment in pkg/jev/testdata/bash-ask-context.json rejected carrying
// it), and the line is rendered only when a case sets it.
func TestStateRendersTheRequestLine(t *testing.T) {
	without := State(Cmd{Command: "git status", CWD: "/w", Root: "/w"})
	if strings.Contains(without, "request in this turn") {
		t.Errorf("an empty request must render nothing, got:\n%s", without)
	}
	with := State(Cmd{Command: "git status", CWD: "/w", Root: "/w", Request: "看看改了啥\n\n再跑测试"})
	if !strings.Contains(with, "The user's request in this turn was: 看看改了啥 再跑测试") {
		t.Errorf("the request must be rendered on one line, got:\n%s", with)
	}
}

func TestStateMissingFacts(t *testing.T) {
	got := State(Cmd{Command: "ls"})
	if !strings.Contains(got, "Working directory: (not set)") ||
		!strings.Contains(got, "(unknown — treat the working directory above as the tree)") {
		t.Errorf("absent facts must be explicit, got:\n%s", got)
	}
}

func TestQuestionType(t *testing.T) {
	q := Question()
	if err := q.Validate(); err != nil {
		t.Fatalf("question must be sendable: %v", err)
	}
	crit, ok := q.Criteria.(jev.NoulCriteria)
	if !ok {
		t.Fatalf("criteria is %T, want a noul rubric", q.Criteria)
	}
	if crit.Yes == "" || crit.No == "" {
		t.Fatal("both poles must be stated, or the probability means nothing")
	}
	// The No pole must not be satisfied by "it only reads": reading a private
	// key or scraping the home directory is exactly what this gate is for.
	if !strings.Contains(crit.No, "no secret") || !strings.Contains(crit.Yes, "credentials") {
		t.Error("the rubric must treat secrets and whole-disk reads as needing a human")
	}
}
