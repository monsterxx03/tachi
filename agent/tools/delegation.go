package tools

import (
	"context"
	"time"
)

// The contract between the delegation tools and this machine's sending side.
//
// It is defined HERE, in the tool layer, and implemented by the node package (a NodeClient in
// the design's words). Two reasons, and both are load-bearing:
//
//   - the tools must not import the node package: `node` imports `agent` (it runs turns), so the
//     reverse would be a cycle;
//   - the model must never be handed a credential, an address or an HTTP request, so the
//     contract speaks in ids and titles (§八). What it does NOT carry is equally deliberate:
//     no peer URLs, no tokens, no remote task ids — those are the sending side's business, and a
//     tool argument or result that leaked one would be a fact the model could then get wrong.
//
// The implementation is reached through this interface by whichever entry point owns both sides
// (TUI, desktop, channel); the tools themselves are built per run with the conversation they
// belong to (see NewDelegateTaskTool).

// conversationKey carries the conversation a run belongs to.
type conversationKey struct{}

// WithLogicalConversation marks a run with the conversation its delegated work is filed under.
//
// It is set by the agent loop once the run's session exists — a session may be CREATED by the
// call that starts a run, so a tool handed an id at construction would have been handed
// nothing. What travels is the logical conversation (session.Session.LogicalConversationID),
// not the session id: local compaction moves a conversation to a successor session, and work
// filed under the old id would be lost to the conversation that asked for it.
func WithLogicalConversation(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, conversationKey{}, id)
}

// LogicalConversation returns the conversation this run belongs to, or "" when there is none
// (a run with no session). Callers that FILE something must refuse an empty answer rather than
// treat it as "every conversation": an unfiltered list would show another conversation's work.
func LogicalConversation(ctx context.Context) string {
	id, _ := ctx.Value(conversationKey{}).(string)
	return id
}

// RemoteTaskInfo is one delegated task as a TOOL RESULT describes it.
//
// The shape is the design's (§8.1, §8.2): a local id the model can refer to, the node it went
// to, a title, a state, and whatever came back — an answer, or a question to answer.
type RemoteTaskInfo struct {
	ID    string `json:"task_id"`
	Node  string `json:"node"`
	Title string `json:"title"`
	State string `json:"state"`
	// Result is the answer (or, for a failed task, why it failed).
	Result string `json:"result,omitempty"`
	// Question is the question the task is waiting on, present only while it waits.
	QuestionID   string `json:"question_id,omitempty"`
	QuestionText string `json:"question_text,omitempty"`
	// Note carries what must not be mistaken for the peer's own words: a connection problem, a
	// cached state, or the peer's refusal.
	Note string `json:"note,omitempty"`
	// LastSeen is when the peer last said anything about this task.
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// Waiting reports whether this task is parked on a question the caller can answer.
func (r RemoteTaskInfo) Waiting() bool { return r.QuestionID != "" }

// InFlight reports whether the task may still move on its own: a caller waits or asks again,
// and must not submit it a second time (§6.2 — a handle that is still `submitted` is in flight,
// the peer's status write simply has not landed yet).
func (r RemoteTaskInfo) InFlight() bool {
	return r.State == "submitted" || r.State == "working"
}

// PeerSummary is one node the model may delegate to, as the peers reminder lists it.
type PeerSummary struct {
	Name string `json:"name"`
	// NodeID is the identity THIS machine pinned for the peer on first contact, and it is empty
	// when it has never reached that node outbound. The model does not need it to delegate — but it
	// is the one fact about a peer nobody else can claim, it makes a name checkable, and it is
	// therefore also what the tool's `node` argument may carry instead of the name.
	NodeID string `json:"node_id,omitempty"`
	// Description is the peer's own account of itself, empty when it could not be reached.
	Description string `json:"description,omitempty"`
	// Skills are the names of the skills the peer ADVERTISES in its Agent Card — its own claim
	// about what it can do, which is why they are shown as the peer's words and not as this
	// machine's knowledge of it.
	Skills []string `json:"skills,omitempty"`
	// Note is the configured human line, used when there is no description.
	Note string `json:"note,omitempty"`
	// Reachable is a fact about a moment, not a promise.
	Reachable bool `json:"reachable"`
}

// DelegationRequest is one new piece of work.
//
// Prompt is the request as the peer's agent will read it, and it is the ONLY place the work is
// described: nothing about this conversation travels with it (§8.1 — a delegation does not copy
// the local session).
type DelegationRequest struct {
	// Peer is the configured node alias. Empty when ContinueFrom names the conversation.
	Peer string
	// ContinueFrom continues an existing delegated conversation (a local task id).
	ContinueFrom string
	Prompt       string
	// Title is what later turns will show for this work. Empty means "derive it from the prompt".
	Title string
	// LogicalID is the local conversation this belongs to: the sending side files the record
	// under it, which is what makes a later `list` show this conversation's work and nobody
	// else's.
	LogicalID string
	// Wait bounds the local wait for an outcome. Zero means the sending side's default.
	Wait time.Duration
}

// RemoteDelegator is what the tools need from this machine's sending side.
//
// Every method is conversation-aware in the same way: the identity travels as data (LogicalID)
// rather than being read from a process global, so one process serving several conversations
// cannot file one conversation's work under another's.
type RemoteDelegator interface {
	// Peers describes the nodes this machine may delegate to. It never fails: an unreachable
	// node is reported as unreachable rather than left out (§8.6 — "not listed" would read as
	// "not configured").
	Peers(ctx context.Context) []PeerSummary
	// Delegate submits new work. The result is the task as the peer last described it: finished,
	// running, or waiting for an answer.
	Delegate(ctx context.Context, req DelegationRequest) (RemoteTaskInfo, error)
	// Get reports one task's last known state, asking the peer when it can.
	Get(ctx context.Context, id string) (RemoteTaskInfo, error)
	// Wait waits boundedly for a task to reach a state that ends the wait. Running out is not a
	// failure: the task keeps working and the caller may ask again (§8.2).
	Wait(ctx context.Context, id string, budget time.Duration) (RemoteTaskInfo, error)
	// Reply answers the question a task is waiting on. The question must be named: the peer
	// refuses an answer that does not say what it answers (§7.4).
	Reply(ctx context.Context, id, questionID string, answer map[string]string, budget time.Duration) (RemoteTaskInfo, error)
	// Cancel asks the peer to stop a task and reports what it actually resolved to.
	Cancel(ctx context.Context, id string) (RemoteTaskInfo, error)
	// RemoteTasks reports what ONE conversation delegated, newest first. It is the same
	// operation the reminders read (systemreminder.RemoteTaskProvider), under one name so the
	// tool and the reminder cannot drift into two ideas of "this conversation's work".
	RemoteTasks(conversation string) []RemoteTaskInfo
}
