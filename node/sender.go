package node

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/config"
)

// Sender is this machine's sending side: the peers it may delegate to, the records of what it
// delegated, and the client that talks to them.
//
// It is what the delegation tools are built over, and it is deliberately shaped around the
// LOCAL record rather than around the protocol: a tool call names a peer or a local id, and
// everything else — addresses, message ids, remote ids — is looked up here. The model never
// sees an address, a credential or a request (§八).
type Sender struct {
	peers   *peerDirectory
	records *delegationStore
	client  *remoteClient
}

// OpenSender builds the sending side over a peer list, loading this machine's records.
//
// It is called once per entry point rather than once per turn: the records are files, and a
// tool call must not pay a directory scan.
func OpenSender(peers []config.PeerConfig) (*Sender, error) {
	records, err := openDelegationStore(config.DelegationsDir())
	if err != nil {
		return nil, err
	}
	directory, err := newPeerDirectory(peers)
	if err != nil {
		return nil, err
	}
	client := &remoteClient{
		peers:          directory,
		delegations:    records,
		http:           &http.Client{},
		requestTimeout: defaultRequestTimeout,
		waitBudget:     defaultWaitBudget,
		pollInterval:   defaultPollInterval,
	}
	return &Sender{peers: directory, records: records, client: client}, nil
}

// PeerInfo is one node the model may delegate to, as a reminder or a refusal lists it.
type PeerInfo struct {
	Name string
	// Description is the peer's own account of itself (its card's description), empty when the
	// card could not be read.
	Description string
	// Note is the configured human line, used when there is no description.
	Note string
	// Reachable says whether the card was read. It is a fact about a moment, not a promise:
	// a node can go away between this line being rendered and a tool call using the alias.
	Reachable bool
}

// Peers describes every configured node, never failing: an unreachable one is reported as such
// rather than dropped, because "not listed" would read as "not configured" (§8.6).
func (s *Sender) Peers(ctx context.Context) []PeerInfo {
	if s == nil {
		return nil
	}
	snapshot := s.peers.Snapshot(ctx)
	out := make([]PeerInfo, 0, len(snapshot))
	for _, p := range snapshot {
		info := PeerInfo{Name: p.Name, Note: p.Note, Reachable: p.Card != nil}
		if p.Card != nil {
			info.Description = p.Card.Description
		}
		out = append(out, info)
	}
	return out
}

// Submit asks a peer for work.
func (s *Sender) Submit(ctx context.Context, req SubmitRequest) (TaskView, error) {
	d, err := s.client.Submit(ctx, req)
	if err != nil {
		if d != nil {
			return viewOf(d, err.Error()), err
		}
		return TaskView{}, err
	}
	return viewOf(d, ""), nil
}

// Get reports the last known state of one delegated task, asking the peer when it can.
func (s *Sender) Get(ctx context.Context, id string) (TaskView, error) {
	d, err := s.client.Get(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	return viewOf(d, ""), nil
}

// Wait waits for a delegated task to reach a state that ends the wait.
func (s *Sender) Wait(ctx context.Context, id string, budget time.Duration) (TaskView, error) {
	d, err := s.client.Wait(ctx, id, budget)
	if err != nil {
		if d != nil {
			return viewOf(d, err.Error()), err
		}
		return TaskView{}, err
	}
	return viewOf(d, ""), nil
}

// Reply answers the question a peer is waiting on, waiting boundedly for what the answer set
// off. A wait that runs out is not a failure: the task keeps working, and the view says so.
func (s *Sender) Reply(ctx context.Context, id, questionID string, answer map[string]string, budget time.Duration) (TaskView, error) {
	d, err := s.client.Reply(ctx, id, questionID, answer, budget)
	if err != nil {
		if d != nil {
			return viewOf(d, err.Error()), err
		}
		return TaskView{}, err
	}
	return viewOf(d, ""), nil
}

// Cancel asks the peer to stop a task.
func (s *Sender) Cancel(ctx context.Context, id string) (TaskView, error) {
	d, err := s.client.Cancel(ctx, id)
	if err != nil {
		if d != nil {
			return viewOf(d, err.Error()), err
		}
		return TaskView{}, err
	}
	return viewOf(d, ""), nil
}

// List reports what ONE local conversation delegated, newest first (§8.2).
//
// The filter is not a convenience: without it a `list` would show work from every conversation
// this machine has ever had, which is both useless and a leak between conversations (§8.5).
func (s *Sender) List(logicalID string) []TaskView {
	if s == nil {
		return nil
	}
	records := s.records.list(logicalID)
	out := make([]TaskView, 0, len(records))
	for _, d := range records {
		out = append(out, viewOf(d, ""))
	}
	return out
}

// QuestionView is the question a delegated task is waiting on: its id is what an answer must
// name (§7.4).
type QuestionView struct {
	ID   string `json:"question_id"`
	Text string `json:"text"`
}

// TaskView is one delegated task as the model sees it.
//
// The field names are the design's (§8.1's example), and the STATE tokens are the protocol's
// lowercased: a model that knows `input_required` from a tool result is being told the same
// thing the receiving side told this machine.
type TaskView struct {
	// ID is the local record's id (d_…): the only handle the model gets, and the one a follow-up
	// or an answer refers to.
	ID    string `json:"task_id"`
	Node  string `json:"node"`
	Title string `json:"title"`
	State string `json:"state"`
	// Result is the answer, or for a failed task the reason. It is an excerpt of the remote
	// record, which stays queryable (§7.3).
	Result string `json:"result,omitempty"`
	// Question is set while the task waits for input.
	Question *QuestionView `json:"question,omitempty"`
	// Note carries what a caller must not be told as a fact: a connection problem, a cached
	// state, a refusal from the peer.
	Note string `json:"note,omitempty"`
	// LastSeen is when this state was observed at the peer.
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// viewOf turns a record into the model's view.
func viewOf(d *Delegation, note string) TaskView {
	view := TaskView{
		ID:       d.ID,
		Node:     d.Peer,
		Title:    d.Title,
		State:    viewState(d.LastKnown.State),
		Result:   d.LastKnown.Text,
		LastSeen: d.LastKnown.At,
		Note:     note,
	}
	if d.QuestionID != "" {
		view.Question = &QuestionView{ID: d.QuestionID, Text: d.LastKnown.Text}
		// A task waiting for input has a question as its text, not a result: showing both would
		// put the question in the model's hands twice, once as if it were an answer.
		view.Result = ""
	}
	return view
}

// viewState renders a protocol state the way a tool result and a reminder both spell it.
func viewState(state a2a.TaskState) string {
	switch state {
	case a2a.TaskStateSubmitted:
		return "submitted"
	case a2a.TaskStateWorking:
		return "working"
	case a2a.TaskStateInputRequired:
		return "input_required"
	case a2a.TaskStateAuthRequired:
		return "auth_required"
	case a2a.TaskStateCompleted:
		return "completed"
	case a2a.TaskStateFailed:
		return "failed"
	case a2a.TaskStateCanceled:
		return "canceled"
	case a2a.TaskStateRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// InFlight reports whether a state means the task may still move on its own: a caller asks
// again later, and must not resubmit (§6.2 — a handle that is still SUBMITTED is in flight, the
// write simply has not landed yet).
func (v TaskView) InFlight() bool {
	switch v.State {
	case "submitted", "working":
		return true
	}
	return false
}

// Waiting reports whether the task is parked on a question this machine can answer.
func (v TaskView) Waiting() bool { return v.State == "input_required" }

// String is the one-line form a refusal or a log line uses.
func (v TaskView) String() string {
	return fmt.Sprintf("%s | %s | %s | %s", v.ID, v.Node, v.Title, v.State)
}
