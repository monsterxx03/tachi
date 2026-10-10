package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RemoteTask manages the work this conversation already delegated: asking about it, waiting for
// it, answering it, cancelling it, or listing what is still around.
//
// It exists because a delegated task outlives the tool call that started it (§8.4): the peer may
// finish long after the turn that asked, and the next turn needs a way to find that out without
// the model remembering anything. The handles are local ids (`d_…`) that earlier results
// carried, and `action="list"` is the way to recover one the model has lost.
const ToolNameRemoteTask = "RemoteTask"

// The actions, named as the design names them (§8.2).
const (
	RemoteActionGet    = "get"
	RemoteActionWait   = "wait"
	RemoteActionReply  = "reply"
	RemoteActionCancel = "cancel"
	RemoteActionList   = "list"
)

// RemoteTaskTool reads and steers the tasks this conversation delegated.
type RemoteTaskTool struct {
	delegator RemoteDelegator
	// waitBudget bounds one local wait. It is a property of the tool rather than a model
	// argument: a model that asked to wait "forever" would hold the turn open, and the design's
	// rule is that a wait is bounded and a timeout does not cancel anything (§8.2).
	waitBudget time.Duration
}

// DefaultRemoteWait is how long an action="wait" waits when the caller sets nothing.
//
// Long enough that a short task usually lands inside it (which is the point of waiting rather
// than polling), short enough that a turn is not held open by a remote machine's pacing.
const DefaultRemoteWait = 60 * time.Second

// NewRemoteTaskTool builds the tool over this machine's sending side.
func NewRemoteTaskTool(d RemoteDelegator, wait time.Duration) *RemoteTaskTool {
	if wait <= 0 {
		wait = DefaultRemoteWait
	}
	return &RemoteTaskTool{delegator: d, waitBudget: wait}
}

func (t *RemoteTaskTool) Name() string { return ToolNameRemoteTask }

func (t *RemoteTaskTool) Parallel() bool { return false }

func (t *RemoteTaskTool) Description() string {
	return `Check on, wait for, answer, cancel or list the tasks THIS conversation delegated to other nodes.

Actions:
- get: the last known state of one task (requires: task_id). It asks the node when it can, and says so when it cannot — a cached state is not a live one.
- wait: wait, bounded, until the task finishes, fails, or asks something (requires: task_id). A wait that runs out does NOT cancel the task: ask again later.
- reply: answer a question the task is waiting on (requires: task_id, question_id, answer). The question_id comes from the task's result or from the reminder; an answer that does not name the question is refused.
- cancel: ask the node to stop the task (requires: task_id). This reports what the node resolved to; side effects already performed are not undone.
- list: the tasks this conversation delegated, newest first. No task_id (optional: node filter). Use this to recover an id you no longer have.`

}

func (t *RemoteTaskTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"action": {
			Type:        "string",
			Description: "What to do with the task.",
			Enum:        []string{RemoteActionGet, RemoteActionWait, RemoteActionReply, RemoteActionCancel, RemoteActionList},
		},
		"task_id": {
			Type:        "string",
			Description: `The local task id (d_…) from an earlier DelegateTask result or from a list. Required for every action except "list".`,
		},
		"question_id": {
			Type:        "string",
			Description: `For action="reply": the id of the question being answered. It must match the question the task is waiting on.`,
		},
		"answer": {
			Type:        "string",
			Description: `For action="reply": the answer, when the task asked ONE thing.`,
		},
		"answers": {
			Type:        "object",
			Description: `For action="reply": when the task asked SEVERAL things, the answers keyed by the full question text ({"检查照片还是文档？": "照片"}). An answer that cannot be mapped to the questions asked is refused.`,
		},
		"node": {
			Type:        "string",
			Description: `For action="list": only this node's tasks.`,
		},
	}
}

func (t *RemoteTaskTool) Required() []string { return []string{"action"} }

type remoteTaskArgs struct {
	Action     string            `json:"action"`
	TaskID     string            `json:"task_id"`
	QuestionID string            `json:"question_id"`
	Answer     string            `json:"answer"`
	Answers    map[string]string `json:"answers"`
	Node       string            `json:"node"`
}

func (t *RemoteTaskTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var in remoteTaskArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if t.delegator == nil {
		return "", fmt.Errorf("this session cannot manage remote tasks: the sending side is not available in this entry point")
	}
	conversation := LogicalConversation(ctx)

	action := strings.ToLower(strings.TrimSpace(in.Action))
	id := strings.TrimSpace(in.TaskID)

	switch action {
	case RemoteActionList:
		return t.list(conversation, in.Node)

	case RemoteActionGet, RemoteActionWait, RemoteActionReply, RemoteActionCancel:
		if id == "" {
			return "", fmt.Errorf("action %q needs a task_id (use action=\"list\" to see the ids this conversation has)", action)
		}
	default:
		return "", fmt.Errorf("unknown action %q: use one of get, wait, reply, cancel, list", in.Action)
	}

	switch action {
	case RemoteActionGet:
		info, err := t.delegator.Get(ctx, id)
		if err != nil && info.ID == "" {
			return "", err
		}
		return renderTaskResult(info, errorNote(err))

	case RemoteActionWait:
		info, err := t.delegator.Wait(ctx, id, t.waitBudget)
		if err != nil && info.ID == "" {
			return "", err
		}
		return renderTaskResult(info, errorNote(err))

	case RemoteActionCancel:
		info, err := t.delegator.Cancel(ctx, id)
		if err != nil && info.ID == "" {
			return "", err
		}
		return renderTaskResult(info, errorNote(err))

	default: // reply
		answer, err := answerFromArgs(in)
		if err != nil {
			return "", err
		}
		info, err := t.delegator.Reply(ctx, id, strings.TrimSpace(in.QuestionID), answer, t.waitBudget)
		if err != nil && info.ID == "" {
			return "", err
		}
		return renderTaskResult(info, errorNote(err))
	}
}

func (t *RemoteTaskTool) list(conversation, node string) (string, error) {
	// The conversation filter comes from the RUN, never from a model argument: a `list` that
	// could be widened by the model would show another conversation's work (§8.5).
	views := t.delegator.RemoteTasks(conversation)
	if node != "" {
		kept := make([]RemoteTaskInfo, 0, len(views))
		for _, v := range views {
			if v.Node == node {
				kept = append(kept, v)
			}
		}
		views = kept
	}
	raw, err := json.Marshal(map[string]any{"tasks": views, "count": len(views)})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// answerFromArgs builds the answer the sending side will send.
//
// A single free-text answer and a named set are two spellings because the peer needs both: one
// question takes one text answer, several need their questions named (§7.4). Giving both is
// ambiguous, and giving neither answers nothing.
func answerFromArgs(in remoteTaskArgs) (map[string]string, error) {
	text := strings.TrimSpace(in.Answer)
	switch {
	case text != "" && len(in.Answers) > 0:
		return nil, fmt.Errorf(`give either "answer" (one question) or "answers" (several), not both`)
	case len(in.Answers) > 0:
		return in.Answers, nil
	case text != "":
		// The key is replaced by the sending side's mapping when the peer asked exactly one
		// question; naming it here would require knowing the question text, which the caller
		// already had to supply as "answers" if it wanted that.
		return map[string]string{"answer": text}, nil
	default:
		return nil, fmt.Errorf(`action="reply" needs an "answer" (or "answers" when several questions were asked)`)
	}
}

// errorNote is what a partially-known failure adds to the result: the model gets the task AND
// the reason it is incomplete, because a bare error would hide the id it needs next.
func errorNote(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
