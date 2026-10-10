package systemreminder

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/agent/tools"
)

// The two reminders the delegation tools rely on, and they answer two different questions
// (design §8.6): which nodes exist, and what this conversation has already asked them.
//
// Both are registered unconditionally and both are silent when the run has no delegator — the
// gate is per RUN (§10.2.1: a cron or ambient turn must not be told about delegation at all),
// and the run's own delegator is what `Context.Delegator` carries. So a reminder never decides
// whether delegation is allowed; it only describes what the run already has.

// RemoteTaskProvider is what these reminders need from this machine's sending side: the tasks
// one conversation delegated, and the nodes it may delegate to.
//
// Defined here rather than importing node, the same way BackgroundTaskProvider is: a reminder
// must be answerable by anything that can list, including a test double.
type RemoteTaskProvider interface {
	// RemoteTasks reports the tasks ONE conversation delegated, newest first.
	RemoteTasks(conversation string) []tools.RemoteTaskInfo
	// Peers describes the configured nodes. It must not fail: an unreachable node is reported
	// as unreachable rather than left out (§8.6 — "not listed" would read as "not configured").
	Peers(ctx context.Context) []tools.PeerSummary
}

// RemoteTasksReminder tells the model what this conversation has outstanding elsewhere.
//
// It exists because a delegated task outlives the turn that started it and the tool result that
// described it: after a compaction, a long conversation, or simply several turns of other work,
// the id is no longer in front of the model — and a reminder is cheaper than letting it guess or
// re-delegate (§8.6's second layer).
//
// It is a PER-TURN summary, deliberately: the state of a task changes without this
// conversation doing anything, and a question waiting for an answer has to stay visible. Within
// a turn the collector's own dedup keeps it from repeating verbatim.
type RemoteTasksReminder struct {
	// Max bounds how many tasks are listed, newest first among equals — except that waiting
	// ones always come first: an unanswered question is the only item here that needs the model
	// to ACT.
	Max int
}

// DefaultRemoteTaskListMax is how many tasks the reminder shows before it starts counting.
const DefaultRemoteTaskListMax = 5

func (r RemoteTasksReminder) Generate(ctx context.Context, rctx Context) []string {
	if rctx.Delegator == nil {
		return nil
	}
	// The scope is asked for AS IT IS, empty included: a run with no conversation has the
	// sessionless scope as its scope (see agent/tools.LogicalConversation), and what it sees there
	// is exactly what RemoteTask(action="list") would show it. Refusing to describe it would leave
	// the model with a tool result it can never refer back to.
	tasks := rctx.Delegator.RemoteTasks(rctx.LogicalConversation)
	if len(tasks) == 0 {
		return nil
	}

	ordered := make([]tools.RemoteTaskInfo, 0, len(tasks))
	for _, t := range tasks {
		if t.Waiting() {
			ordered = append(ordered, t)
		}
	}
	for _, t := range tasks {
		if !t.Waiting() {
			ordered = append(ordered, t)
		}
	}

	limit := r.Max
	if limit <= 0 {
		limit = DefaultRemoteTaskListMax
	}
	shown := ordered
	hidden := 0
	if len(shown) > limit {
		hidden = len(shown) - limit
		shown = shown[:limit]
	}

	lines := make([]string, 0, len(shown)+2)
	lines = append(lines, "当前对话关联的远程任务（有限摘要；状态是上次观察到的时间点，不保证节点此刻可达）：")
	for _, t := range shown {
		line := fmt.Sprintf("- %s | %s | %s | %s", t.ID, t.Node, t.Title, stateLabel(t))
		if t.Waiting() {
			line += fmt.Sprintf(" | question_id=%s", t.QuestionID)
		}
		lines = append(lines, line)
	}
	if hidden > 0 {
		lines = append(lines,
			fmt.Sprintf("还有 %d 项未列出：用 RemoteTask(action=\"list\") 查询全部。", hidden))
	}
	lines = append(lines,
		"回答待答问题用 RemoteTask(action=\"reply\", task_id=…, question_id=…, answer=…)；查最新状态用 action=\"get\"；停止用 action=\"cancel\"。未列出的任务不等于已结束或已删除。")
	_ = ctx
	return lines
}

// PeersReminder tells the model which nodes it may delegate to.
//
// It is separate from the task reminder because it answers a different question: the nodes are a
// fact about this MACHINE, not about the conversation, so a run with no conversation (the pipe
// entry) is told them too — otherwise it would have the tool and no way to know a node's name.
//
// It is CHANGE-detected rather than repeated every turn: the peer list rarely moves, and a stable
// block re-sent fifty times is tokens spent telling the model what it already read (the same
// discipline the tool view uses for its schema list). It speaks again when the list itself
// changes — a node added, removed, renamed, or its reachability flipping. The memory is keyed by
// the scope the run is in, so one conversation's silence does not hide another's first telling
// (and the empty scope, being a scope, has its own).
type PeersReminder struct {
	mu sync.Mutex
	// last is what each scope was last told, so an unchanged list stays quiet. Keyed by the run's
	// conversation (empty for a sessionless run), not by process: several conversations of one
	// process each get their first telling, and each goes quiet on its own.
	last map[string]string
}

func (r *PeersReminder) Generate(ctx context.Context, rctx Context) []string {
	if rctx.Delegator == nil {
		return nil
	}
	peers := rctx.Delegator.Peers(ctx)
	if len(peers) == 0 {
		return nil
	}

	lines := make([]string, 0, len(peers)+2)
	lines = append(lines, "可以委派工作的节点（用 DelegateTask，node 参数填名字；prompt 要写清在它那台机器上做什么）：")
	for _, p := range peers {
		what := p.Description
		if what == "" {
			what = p.Note
		}
		if what == "" {
			what = "没有更多描述"
		}
		state := ""
		if !p.Reachable {
			// A fact about a moment, said as one: a node that did not answer the card request
			// may still accept work, and a reminder may not claim otherwise (§8.6).
			state = "（上次联系不上；描述来自配置）"
		}
		lines = append(lines, fmt.Sprintf("- %s：%s%s", p.Name, what, state))
	}

	// The change test is on the RENDERED lines, not on the peer structs: what the model was
	// told is the only thing that decides whether telling it again teaches anything.
	rendered := strings.Join(lines, "\n")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = make(map[string]string)
	}
	if r.last[rctx.LogicalConversation] == rendered {
		return nil
	}
	r.last[rctx.LogicalConversation] = rendered
	return lines
}

// stateLabel renders a task state the way a reminder reads it — the model's own vocabulary from
// the tool results, in a sentence a person could read too.
func stateLabel(t tools.RemoteTaskInfo) string {
	switch t.State {
	case "submitted", "working":
		return "进行中"
	case "input_required", "auth_required":
		return "等待回答"
	case "completed":
		return "已完成"
	case "failed":
		return "失败"
	case "canceled":
		return "已取消"
	default:
		return "状态未知"
	}
}
