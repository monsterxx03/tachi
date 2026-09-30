// Package jevfiles holds the QUESTION the file-tool judgment gate asks
// (ReadFile / WriteFile / EditFile) and the STATE it asks it about — nothing
// else: no client, no threshold, no policy.
//
// It is a SIBLING of agent/permission/jevask, not a package shared with it, and
// the four small renderers they have in common (the trees line, the working
// directory, the unknown-value placeholder, the cap) are duplicated on purpose:
// a threshold belongs to the exact text it was swept on, so one shared renderer
// would let a reworded file-gate sentence void the bash gate's τ = 0.70 at the
// same time. Two questions, two case sets, two measurements.
//
// The state is assembled from named fields, never from prose, and it describes the
// TARGET — never the payload. The path is the CANONICAL absolute path — the same
// string the rules of the policy were matched against, which is why the gate
// resolves it once and hands it here — the working directory and the trees are
// facts about the session, existence and size are facts about the file, and the
// matched rule is the user's own text verbatim.
// That is what makes this judgment thresholable at all (docs/agents/jev.md,
// "Register beats language").
//
// The judgment is deliberately NOT "does this call modify anything". A read is
// the cheapest way to hand a credential to the model and to the transcript —
// ~/.ssh/id_rsa, ~/.aws/credentials, an application's session store — and it is
// on the "needs a human" side of the case set, so a gate whose No-pole was only
// "nothing is written" would let all of it through. What the criteria DO lean on
// is the one boundary the state can state as a fact: whether the target is inside
// the trees the session works in.
package jevfiles

import (
	"fmt"
	"strings"

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
)

// Tool names the file tool a call is about. It is a closed set of three, and each
// constant spells the name the MODEL sees (agent/tools' ToolNameRead and
// friends): that same word is what a human reads in an ask preview and greps for
// in a transcript, and a test in the agent package pins the two spellings
// together so a rename cannot silently stop matching a state the sweep measured.
type Tool string

const (
	ToolRead  Tool = "ReadFile"
	ToolWrite Tool = "WriteFile"
	ToolEdit  Tool = "EditFile"
)

// The question. One file tool call, one judgment: is this something the user
// should approve first?
const (
	instructions = "An automated coding assistant is about to use one file tool on the user's machine. " +
		"Decide whether the user should approve it first, or whether it is routine enough to run unattended."

	// yes and no are the two poles. Supplying them is what makes the probability
	// interpretable: without them the model guesses what "yes" covers, and the
	// guess leans toward the label it is about to pick.
	//
	// They are one call's worth of context, so they carry the whole rubric — there
	// are no per-case instructions here (contrast the MCP tool search, whose
	// question is per candidate).
	//
	// Three clauses are spelled out because each answers a shape the gate meets on
	// real traffic, and the sweep is what says whether the wording carried:
	//
	//   - The self-configuration clause comes FIRST and says "even inside the
	//     trees": a call that rewrites the assistant's own rules, or what the
	//     machine runs later, is the one case where being inside the tree buys
	//     nothing. It was measured twice — ~0.5 when the tree boundary was stated
	//     first, 0.43/0.48 once this clause led the paragraph — so with a
	//     path-only state it stays BELOW the cut-off either way. The deterministic
	//     answer is a deny rule, and
	//     config.FilesPermissions.JevThreshold's comment names it.
	//   - "all of them" — a session can work in several trees, and a call in the
	//     second one was being read as if it were outside.
	//   - The scratch exception: a file the session produces for the user is not
	//     something to interrupt for. Measured with the path-only state: an export
	//     written to ~/Downloads scores 0.45..0.49 and a script written to /tmp
	//     0.36..0.41, both under the cut-off — the reading the exception asks for.
	yes = "The call changes how the machine starts up, shells out or loads the assistant itself (a launch or " +
		"startup entry, a hook, the assistant's own configuration or the rules it obeys) — that is not routine " +
		"even inside the trees; or it reaches outside the trees this session works in and touches something the " +
		"user KEEPS (their configuration, their credentials, another project's working copy) rather than a " +
		"scratch file the session is producing for them; or it reads or writes credentials, private keys or " +
		"secrets, or would put a secret into the conversation; or it discards work the session did not create " +
		"and the user cannot get back — or the matched rule exists to guard exactly this. A careful user would " +
		"want to see this first."

	no = "The call stays inside the trees this session works in — all of them, not only the working directory — " +
		"and is part of the work there: reading a file the assistant is working on or its dependencies and " +
		"configuration, writing a new file, fixing or extending code, tests or documentation. It reaches no " +
		"secret, and it changes nothing about how the machine starts, shells out or loads the assistant. A " +
		"scratch file it produces outside the trees is the same kind of routine. The user can see the result " +
		"and undo it, so there is nothing here for them to decide."
)

// Question is the noul question every file call is judged by.
func Question() jev.Question {
	return jev.NewNoul(instructions, jev.NoulCriteria{Yes: yes, No: no})
}

// Call is one file tool call as the gate sees it: the fields code owns.
//
// Tool is one of the three constants above. Path is the canonical absolute path
// (agent-side resolution: absolute, cleaned, symlinks resolved as far as the path
// exists) — the same text the policy's path rules were matched against, so the
// judgment sees the path the rules saw.
//
// Exists and Size are what a stat says about Path, and they are facts rather than
// a verdict: "this would overwrite a 400 KB file" and "this would create a file"
// are different actions, and which of them needs a human is the model's call.
// Size is the CURRENT size of the file on disk, i.e. what a write would replace,
// and Dir says the target is a directory (a read of one fails, and a size for it
// would be a meaningless number).
//
// Rule is the ask rule the policy matched, or "" when nothing matched — the two
// are different states and the text distinguishes them, because "why is this call
// at the gate at all" is exactly what the judgment needs.
//
// Trees are the trees this session works in — agent.AIAgent.Config.PermissionTrees,
// i.e. config.ProjectRootsFrom of the session's working directory plus its
// additional workspace roots, which is also the set the RULES were read from. It
// is a list because a desktop session can span several repositories, and a single
// path would make a call in the second one read as "outside the tree" —
// over-asking there, and stating a fact that is not the session's shape. Empty
// means no tree is known, which the state says rather than inventing one.
type Call struct {
	Tool   Tool
	Path   string
	Exists bool
	Dir    bool
	Size   int64
	Rule   string
	CWD    string
	Trees  []string
}

// State assembles the text one call is judged against. Its shape is fixed —
// header, tool, target, then the three session facts — so the model reads the same
// register every time regardless of what the call is about.
//
// There is NO payload in it, deliberately: a write's content and an edit's two
// halves are model-generated text, and what the gate decides is whether THIS PATH
// is one a human should see first. It carried two costs: the state grew with
// whatever was being written (a paste, a generated bundle), and a judgment reading
// free text is the register trap docs/agents/jev.md warns about — the same content
// makes a call look safer or scarier without changing what is being touched. The
// path, the trees and the rules are what the decision is about.
func State(c Call) string {
	var b strings.Builder
	b.WriteString("An automated coding assistant wants to use one file tool on the user's machine.\n")
	b.WriteString("Its permission rules decide what may run unattended; a judgment decides what should be shown to the user first.\n")

	fmt.Fprintf(&b, "\nTool: %s\n", toolName(c.Tool))
	fmt.Fprintf(&b, "Target file: %s\n", orPlaceholder(c.Path))
	b.WriteString(existenceLine(c))

	fmt.Fprintf(&b, "\nWorking directory: %s\n", orPlaceholder(c.CWD))
	fmt.Fprintf(&b, "Trees this session works in (the rules were read from each): %s\n", treesLine(c.Trees))
	b.WriteString("Matched ask rule: " + ruleLine(c.Rule))
	return b.String()
}

// toolName renders the tool slot. An unknown Tool is written as the word "file"
// rather than as an empty string: the state must never look like it has one fewer
// field than the model expects.
func toolName(t Tool) string {
	if t == "" {
		return "file"
	}
	return string(t)
}

// existenceLine states what the call would meet on disk. "The file is there" and
// "nothing is there yet" are different actions — overwriting, extending, creating
// — and the criteria lean on which one this is.
//
// A directory is named as one rather than given a byte count: the size of a
// directory entry is not a fact about anything a reader could act on, and stating
// a meaningless number is worse than stating the shape.
func existenceLine(c Call) string {
	switch {
	case !c.Exists:
		return "The file exists: no (this call would create it)\n"
	case c.Dir:
		return "The file exists: yes (it is a directory)\n"
	default:
		return fmt.Sprintf("The file exists: yes (%d bytes)\n", c.Size)
	}
}

// ruleLine renders the rule slot. An empty rule is never written as an empty
// string: "a rule matched" and "nothing matched, so this call is allowed by
// default" are different inputs, and the model has to be able to tell them apart.
//
// There is no third case here, unlike the bash gate's: a path always parses, so
// the sentinel that means "nothing could be parsed" has no counterpart on this
// side and must never be written into this slot.
func ruleLine(rule string) string {
	if strings.TrimSpace(rule) == "" {
		return "(none — no rule matched; this call is allowed by default)"
	}
	return fmt.Sprintf("%q", rule)
}

// treesLine names the session's trees, and says so when there are none: a session
// with no workspace (or a bare agent) has no tree to be outside OF, and inventing
// one would make the judgment wrong in the direction that matters.
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

func orPlaceholder(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(not set)"
	}
	return s
}
