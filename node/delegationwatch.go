package node

import (
	"context"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/agent/tools"
)

// DelegationPollInterval is how often the watcher asks about delegated tasks
// that have not settled. Short enough that a finished task wakes its
// conversation while the user is still around, long enough that a peer is not
// asked twenty times a minute.
const DelegationPollInterval = 20 * time.Second

// delegationWatchMaxAge bounds how long a record with no fresh observation stays
// polled. A peer can vanish mid-task; without this, every ghost record costs one
// request per interval, forever.
const delegationWatchMaxAge = 24 * time.Hour

// PollDelegations asks about every delegated task that has not settled, and
// reports the ones that just did — or that just started waiting for an answer.
//
// It is the remote half of the completion queue: the tool call and the turn that
// started the work may both be long gone, and an idle conversation has no other
// way to learn the peer moved. The record is the authority on what was already
// observed, so a state that has not changed is not reported again.
func (s *Sender) PollDelegations(ctx context.Context, sink func(logicalID string, e tasks.Event)) {
	if s == nil || s.client == nil || sink == nil {
		return
	}
	now := time.Now()
	for _, d := range s.records.all() {
		// Nothing to ask about: a direct answer has no handle, and a task whose
		// last known state is already terminal is finished news.
		if d.RemoteTaskID == "" || d.DirectAnswer || d.LastKnown.State.Terminal() {
			continue
		}
		if now.Sub(d.LastKnown.At) > delegationWatchMaxAge {
			continue
		}
		prev := d.LastKnown.State
		upd, err := s.client.Get(ctx, d.ID)
		if err != nil || upd == nil {
			// Unreachable is not a state change. The record keeps what it last saw,
			// and the next tick tries again.
			continue
		}
		state := upd.LastKnown.State
		if !settlesWait(state) || state == prev {
			continue
		}
		sink(d.LogicalID, delegationEvent(upd, s.viewOf(upd, "")))
	}
}

// delegationEvent turns one observed task into the queue's event. The handle it
// carries is the LOCAL delegation id — the same id every remote tool call names.
func delegationEvent(d *Delegation, view tools.RemoteTaskInfo) tasks.Event {
	e := tasks.Event{
		ID:           d.ID,
		Conversation: d.LogicalID,
		Kind:         tasks.KindRemote,
		Name:         d.ID,
		Command:      d.Title,
		Node:         view.Node,
	}
	switch {
	case d.QuestionID != "":
		// The peer is parked on an answer. This is not an ending, but it is the one
		// state an idle conversation must hear about: nobody else can answer it.
		e.Status = tasks.StatusWaiting
		e.QuestionID = d.QuestionID
		e.Stdout = view.QuestionText
	case d.LastKnown.State == a2a.TaskStateCompleted:
		e.Status = tasks.StatusExited
		e.Stdout = view.Result
	case d.LastKnown.State.Terminal():
		e.Status = tasks.StatusError
		e.Error = viewState(d.LastKnown.State)
		e.Stdout = view.Result
	default:
		// auth_required (or any other state a wait settles on): the task needs
		// something from this side before it can go on.
		e.Status = tasks.StatusWaiting
		e.Stdout = d.LastKnown.Text
	}
	return e
}
