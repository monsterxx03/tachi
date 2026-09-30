// Package jevask holds the QUESTION a Bash judgment gate asks and the STATE it
// asks it about — nothing else: no client, no threshold, no policy.
//
// It is a package of its own for the reason docs/agents/jev.md gives: the text is
// shared by two callers that must never disagree — the gate that SENDS it
// (agent/agent_permission_jev.go) and the measurement that swept the cut-off
// (generate_cases_test.go writes pkg/jev/testdata/bash-ask.json by calling the
// functions here). A threshold belongs to the prompt it was swept on, so editing
// a sentence here invalidates the number.
//
// The state is assembled from named fields, never from prose. The segments come
// from agent/permission's own parser — the same canonical output the rules were
// matched against — the working directory and the roots are facts about the
// session, and the matched rule is the user's own text verbatim. That is what
// makes this judgment thresholable at all (jev.md, "Register beats language").
//
// The judgment is deliberately NOT "does this command modify anything". A
// read-only command can still be exactly the thing a user wants to see first:
// a recursive grep over / or $HOME runs for minutes and looks like a hang, and
// reading ~/.ssh/id_rsa, ~/.aws/credentials or ~/.tachi/config.yaml hands
// credentials to the model and to the transcript. Both are on the
// "needs a human" side of the case set, and the criteria below say so — a gate
// whose No-pole was only "it is read-only" would let them through.
package jevask

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/pkg/jev"
)

const (
	// ActionLabel is the noul pole meaning "a human should see this first": the
	// label a gate thresholds on, and the one the generated case file names as
	// action_label. A question with the other pole is a different calibration.
	ActionLabel = "true"

	// QuestionID is the key one judgment is asked under. It never reaches the
	// model (only the instructions do), but the gate and the case file have to
	// agree on it like everything else about the question.
	QuestionID = "need_human"

	// maxCommandChars bounds the command text inside the state. A heredoc body
	// can be arbitrarily long, and the service's binding limit is state plus the
	// LONGEST question (docs/agents/jev.md, "Traps") — the cap keeps a huge
	// script from eating it, at the price of judging a truncated one.
	maxCommandChars = 1500
)

// The question. One command, one judgment: is this something the user should
// approve first?
const (
	instructions = "An automated coding assistant is about to run one shell command on the user's machine. " +
		"Decide whether the user should approve it first, or whether it is routine enough to run unattended."

	// yes and no are the two poles. Supplying them is what makes the probability
	// interpretable: without them the model guesses what "yes" covers, and the
	// guess leans toward the label it is about to pick.
	//
	// They are one command's worth of context, so they carry the whole rubric —
	// there are no per-case instructions here (contrast the MCP tool search,
	// whose question is per candidate). "Read-only" is deliberately NOT a
	// sufficient condition on the No side: see the package doc.
	yes = "The command touches data outside the working tree (a read-only scan of / or the home directory runs for " +
		"minutes and can look like a hang), or reads credentials, private keys or secrets, or changes or publishes " +
		"shared state, or rewrites history, or runs code fetched from the network, or deletes work that is not " +
		"reproducible — or it is the kind of action the matched rule exists to guard. A careful user would want to " +
		"see this first."

	no = "The command stays inside the working tree, is cheap, and either only reads or makes a change the user can " +
		"undo (building, testing, listing files, deleting build artifacts), and it reaches no network, no shared " +
		"state and no secret outside the tree — there is nothing here for the user to decide."
)

// Question is the noul question every command is judged by.
func Question() jev.Question {
	return jev.NewNoul(instructions, jev.NoulCriteria{Yes: yes, No: no})
}

// Cmd is one command as the gate sees it: the fields code owns.
//
// Rule is the ask rule the policy matched, or "" when nothing matched — the two
// are different states and the text distinguishes them, because "why is this
// command at the gate at all" is exactly what the judgment needs.
//
// Trees are the trees this session works in — agent.AIAgent.Config.PermissionTrees,
// i.e. config.ProjectRootsFrom of the session's working directory plus its
// additional workspace roots, which is also the set the RULES were read from.
// It is a list because a desktop session can span several repositories, and a
// single path would make a command in the second one read as "outside the tree"
// — over-asking there, and stating a fact that is not the session's shape.
// Empty means no tree is known, which the state says rather than inventing one.
type Cmd struct {
	Command string
	Rule    string
	CWD     string
	Trees   []string
	// Request is what the user asked for in THIS turn ("把 feature/login 删了").
	//
	// MEASURED, and the gate therefore leaves it EMPTY
	// (pkg/jev/testdata/bash-ask-context.json, jev-1.13.0, 19 pairs × 5 repeats):
	// carrying the line moved the probability by a mean of 0.009 and at most
	// 0.028, with the sign MIXED — `git branch -D feature/login` rose 0.006 when
	// the user had just asked for exactly that, `docker compose down -v` rose
	// 0.028, `git reset --hard` fell 0.016, `git push --force` did not move at
	// all. No pair crossed a label. Read as: the judgment answers what the
	// COMMAND is, and a request is context rather than permission — which is the
	// stability a probability gate wants (jev.md's register trap, the thing free
	// text risks, did not fire here).
	//
	// The field stays because that measurement has to remain reproducible from
	// the production assembler (jev.md: a case file generated by anything else
	// measures a prompt nobody sends). Whoever wants the request to change the
	// answer has to change the CRITERIA — "unless the user asked for exactly
	// this" — and then re-sweep, accepting that the sentence that asks can also
	// be the sentence that talks a command under the threshold.
	Request string
}

// State assembles the text one command is judged against. Its shape is fixed —
// header, command, then the three facts — so the model reads the same register
// every time regardless of what the command looks like.
func State(c Cmd) string {
	var b strings.Builder
	b.WriteString("An automated coding assistant wants to run this command on the user's machine.\n")
	b.WriteString("Its permission rules decide what may run unattended; a judgment decides what should be shown to the user first.\n")

	segments, err := permission.Segments(c.Command)
	unparsed := err != nil || len(segments) == 0
	var body string
	if unparsed {
		b.WriteString("\nThe command could not be parsed into segments, so it is shown as written.\n")
		b.WriteString("Command (as written):\n")
		body = indent(c.Command)
	} else {
		b.WriteString("\nCommand (canonical segments, one per line):\n")
		body = segmentsBlock(segments)
	}
	b.WriteString(capText(body))

	if strings.TrimSpace(c.Request) != "" {
		fmt.Fprintf(&b, "\nThe user's request in this turn was: %s\n", oneLine(c.Request))
	}

	fmt.Fprintf(&b, "\nWorking directory: %s\n", orPlaceholder(c.CWD))
	fmt.Fprintf(&b, "Trees this session works in (the rules were read from each): %s\n", treesLine(c.Trees))
	b.WriteString("Matched ask rule: " + ruleLine(c.Rule, unparsed))
	return b.String()
}

// segmentsBlock renders one segment per line, numbered.
func segmentsBlock(segments []string) string {
	var b strings.Builder
	for i, seg := range segments {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, seg)
	}
	return b.String()
}

// ruleLine renders the rule slot. An empty rule is never written as an empty
// string: the three cases (a rule matched / nothing matched / nothing could be
// parsed) are different inputs, and the model has to be able to tell them apart.
func ruleLine(rule string, unparsed bool) string {
	switch {
	case rule != "":
		return fmt.Sprintf("%q", rule)
	case unparsed:
		return "(none — the command could not be parsed, so no rule could be matched against it)"
	default:
		return "(none — no rule matched; this command is allowed by default)"
	}
}

// treesLine names the session's trees, and says so when there are none: a
// session with no workspace (or a bare agent) has no tree to be outside OF, and
// inventing one would make the judgment wrong in the direction that matters.
func treesLine(trees []string) string {
	live := make([]string, 0, len(trees))
	for _, t := range trees {
		if s := strings.TrimSpace(t); s != "" {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return "(unknown — treat the working directory above as the tree)"
	}
	return strings.Join(live, ", ")
}

// oneLine collapses the request to one line: the field is a sentence, and a
// multi-line paste must not read as more state fields.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func orPlaceholder(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(not set)"
	}
	return s
}

// capText bounds the command block — parsed or not — and says so, so a judged
// prefix is never mistaken for the whole command. It is applied to the RENDERED
// block rather than to the source: truncating the source first could cut a quote
// in half and change what the parser sees, which would make the gate judge a
// command the rules never matched.
func capText(s string) string {
	r := []rune(s)
	if len(r) <= maxCommandChars {
		return s
	}
	return string(r[:maxCommandChars]) + "\n… (truncated)\n"
}

// indent renders a multi-line command as a block: without it a script's second
// line reads as another field of the state.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
