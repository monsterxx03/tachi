package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DelegateTask hands new work to another Tachi node and reports what came back.
//
// It is deliberately NOT a subagent (design §8.3): a remote task has its own identity, its own
// lifetime, its own permissions and its own answers, and pretending it were a local sub-agent
// would tangle inheritance, cancellation and paths. What the model gets back is a local id, a
// title and a state — and that id is what later turns use to continue, answer or cancel it.
const ToolNameDelegateTask = "DelegateTask"

// DelegateTaskTool submits work to a peer.
//
// One instance belongs to ONE conversation: the sending side files the record under the
// conversation it was built with, so a `list` (and the reminder) show each conversation its own
// work. The tools are attached to a run rather than registered, which is also what keeps them
// out of turns that must not delegate (§10.2.1: a cron or ambient turn gets no such tool).
type DelegateTaskTool struct {
	delegator RemoteDelegator
}

// NewDelegateTaskTool builds the tool over this machine's sending side.
func NewDelegateTaskTool(d RemoteDelegator) *DelegateTaskTool {
	return &DelegateTaskTool{delegator: d}
}

func (t *DelegateTaskTool) Name() string { return ToolNameDelegateTask }

// Parallel is false: a delegation is a side effect on another machine, and two of them racing
// would produce two tasks nobody asked for in an order nobody chose.
func (t *DelegateTaskTool) Parallel() bool { return false }

func (t *DelegateTaskTool) Description() string {
	return `Delegate a task to another Tachi node and get its result back. The node runs a real turn on ITS machine: its own workspace, its own tools, its own model. Use it when the work belongs somewhere else — files that live on that machine, or a long job you do not want to block on.

How to choose (never guess from "the last conversation"):
- New independent work: give "node" — the node id the reminder prints beside the name (a unique prefix of one is enough). That opens a NEW conversation on that node. A NAME is not accepted: a name is what its owner says it is, so it would decide where work goes by whoever claimed it.
- A follow-up to work you already delegated: give "continue_from" with the task_id from a previous result. That continues the SAME remote conversation; do not repeat the context the earlier task established.
- Exactly one of "node" and "continue_from" is required.

A reply may come back in one of three shapes: the task finished (with its result), the task is still running (you get the id and its state — ask again later with RemoteTask, do not submit it again), or the node asked a question and the task is waiting (answer it with RemoteTask action="reply", naming the question_id).

The prompt is the whole request: the other machine cannot see this conversation. Write what it needs — paths on THAT machine, what to do, what not to do — and state any read-only or "do not modify" intent explicitly. A tool allowlist on the receiving side is what actually limits what it may touch, not the wording of the prompt.

If the result says the outcome is unknown (a connection problem), the task may still have been accepted: query it with the same id instead of delegating again.`
}

func (t *DelegateTaskTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"node": {
			Type:        "string",
			Description: `Which node to delegate to, by its node id (a unique prefix of one is enough) — the value the peers reminder prints in parentheses after the name. Required unless "continue_from" is given.`,
		},
		"continue_from": {
			Type:        "string",
			Description: `A task_id from an earlier DelegateTask result, to continue THAT remote conversation. Required unless "node" is given.`,
		},
		"prompt": {
			Type:        "string",
			Description: "The request, written for a machine that cannot see this conversation: what to do, where, and what not to do.",
		},
		"title": {
			Type:        "string",
			Description: "Optional short label for this work (it is what later turns will show). Omit to use the first line of the prompt.",
		},
	}
}

func (t *DelegateTaskTool) Required() []string { return []string{"prompt"} }

type delegateTaskArgs struct {
	Node         string `json:"node"`
	ContinueFrom string `json:"continue_from"`
	Prompt       string `json:"prompt"`
	Title        string `json:"title"`
}

func (t *DelegateTaskTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var in delegateTaskArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// "Exactly one" is the design's rule (§8.1) and it is checked here rather than resolved by
	// preference: a model that named both meant something this tool cannot know, and picking one
	// would be inventing it. The refusal says how to fix it.
	node := strings.TrimSpace(in.Node)
	from := strings.TrimSpace(in.ContinueFrom)
	switch {
	case node != "" && from != "":
		return "", fmt.Errorf(`give either "node" (new work on that node) or "continue_from" (continue that conversation), not both`)
	case node == "" && from == "":
		return "", fmt.Errorf(`give "node" to delegate new work, or "continue_from" to continue an earlier task`)
	case strings.TrimSpace(in.Prompt) == "":
		return "", fmt.Errorf("a delegation needs a prompt: the other machine cannot see this conversation")
	}

	if t.delegator == nil {
		return "", fmt.Errorf("this session cannot delegate: the sending side is not available in this entry point")
	}
	// A run without a conversation (the pipe entry: no session, by design) files under the empty
	// scope instead of refusing. That keeps a script's one-off delegation working — it does get
	// the result — while `RemoteTask(action="list")` there shows only what such runs delegated:
	// a sessionless run can never see a conversation's tasks, and a conversation never sees its.
	conversation := LogicalConversation(ctx)

	info, err := t.delegator.Delegate(ctx, DelegationRequest{
		Peer:         node,
		ContinueFrom: from,
		Prompt:       in.Prompt,
		Title:        in.Title,
		LogicalID:    conversation,
	})
	if err != nil {
		// A failure that still knows something (a refusal that names the task, a connection that
		// left the outcome unknown) is reported as data: the model needs the id to ask again, and
		// an error string alone would make it delegate a second time.
		if info.ID == "" {
			return "", err
		}
		return renderTaskResult(info, err.Error())
	}
	return renderTaskResult(info, "")
}

// renderTaskResult is the one shape both delegation tools return: the task as data, plus a
// sentence when there is something the model must not misread (a refusal, an unknown outcome).
func renderTaskResult(info RemoteTaskInfo, note string) (string, error) {
	if note != "" && info.Note == "" {
		info.Note = note
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
