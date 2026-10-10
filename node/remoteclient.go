package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"
)

// remoteClient is this machine's side of a delegation: it submits work to a peer, reads back
// what that peer says about it, answers a question, cancels, and lists what this conversation
// asked for.
//
// It speaks the protocol ITSELF rather than going through the SDK's client, and that is a
// deliberate, load-bearing choice: the SDK's REST client discards any google.rpc.ErrorInfo
// whose domain is not `a2a-protocol.org` (internal/rest.ConvertErrorBody does the filtering),
// while this node's own refusals are filed under `tachi` on purpose (§9.1.1). Through the SDK
// client a refusal would arrive as "internal error" plus our human sentence — the REASON, which
// is the entire point of having one, would be gone, and the caller could not tell "not accepted,
// safe to retry" from "this task is over". Three endpoints and one response envelope is a small
// price for keeping that.
//
// What it hands back is in Tachi's terms (TaskView): the model never sees an address, a
// credential or an HTTP request (§八).
type remoteClient struct {
	peers       *peerDirectory
	delegations *delegationStore
	http        *http.Client

	// requestTimeout bounds ONE request. It is not the wait budget: the wait polls, so a long
	// wait is many short requests, and a peer that accepts work and then answers slowly is
	// normal (that is what the task handle is for).
	requestTimeout time.Duration
	// waitBudget bounds one local wait, and pollInterval how often the wait asks again.
	waitBudget   time.Duration
	pollInterval time.Duration
}

const (
	// The REST binding's paths, spelled here the way the receiving side spells them (§11.3, and
	// node/admission.go's restSendMessagePath). No prefix: this listener carries the protocol at
	// its root.
	restSendPath  = "/message:send"
	restTasksPath = "/tasks/"
	cancelSuffix  = ":cancel"
	a2aMediaType  = "application/a2a+json"
	a2aVersionHdr = a2a.SvcParamVersion

	defaultRequestTimeout = 10 * time.Second
	defaultWaitBudget     = 30 * time.Second
	defaultPollInterval   = 300 * time.Millisecond

	// answerExcerptLen bounds how much of a remote answer travels into a tool result or a
	// reminder. The result is for the model to read, not to archive: a long answer belongs in
	// the peer's task record, which stays queryable (§7.3 大结果做上限控制).
	answerExcerptLen = 4000
)

// SubmitRequest is one thing to ask a peer for.
type SubmitRequest struct {
	// Peer is the configured alias.
	Peer string
	// Prompt is the request text. It is what the peer's agent sees, and it is stored with the
	// record so a retry can send the SAME content (§9.2).
	Prompt string
	// Title is what the model will show for this work. Empty means "derive it from the prompt".
	Title string
	// LogicalID is the local conversation this belongs to. The record is filed under it, which
	// is what keeps `list` from showing another conversation's work.
	LogicalID string
	// ContinueFrom continues an existing local delegation: the peer resolves to the same remote
	// conversation, and the previous remote task is referenced rather than adopted (§8.1).
	ContinueFrom string
	// Wait bounds the local wait for an outcome. Zero means the default.
	Wait time.Duration
}

// Get asks the peer (or the record, when the peer cannot be reached) about one delegated task.
func (c *remoteClient) Get(ctx context.Context, id string) (*Delegation, error) {
	d, err := c.delegations.find(id)
	if err != nil {
		return nil, err
	}
	if d.RemoteTaskID == "" {
		// No remote task to ask about, for one of two reasons that both end here: the submission
		// was never accepted (the record is the only answer we can give), or the peer answered it
		// with a direct Message (the record already HOLDS the answer — see DirectAnswer).
		return d, nil
	}
	task, err := c.fetch(ctx, d)
	if err != nil {
		// The record is the answer we CAN give: it says what we last saw and when. §8.2 requires
		// exactly this distinction — a cached state, not a failure that hides it.
		return d, nil
	}
	return c.record(d, task)
}

// Wait polls until the task reaches a state that ends the wait, or the budget runs out.
//
// A timeout does NOT cancel the task (§8.2): the peer keeps working, and the caller may ask
// again. That is the whole reason the handle is stored before the wait starts.
func (c *remoteClient) Wait(ctx context.Context, id string, budget time.Duration) (*Delegation, error) {
	d, err := c.delegations.find(id)
	if err != nil {
		return nil, err
	}
	if budget <= 0 {
		budget = c.waitBudget
	}
	if d.RemoteTaskID == "" {
		// Nothing to poll: either the submission's fate is unknown, or the peer answered it
		// directly and the answer is already in the record. Waiting would not change either.
		return d, nil
	}
	deadline := time.Now().Add(budget)
	for {
		task, err := c.fetch(ctx, d)
		if err != nil {
			return d, err
		}
		d, err = c.record(d, task)
		if err != nil {
			return d, err
		}
		if settlesWait(task.Status.State) || !time.Now().Before(deadline) {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-time.After(c.pollInterval):
		}
	}
}

// Reply answers the question a peer is waiting on.
//
// The answer must name the question: the receiving side refuses one that does not, because the
// agent's answer channel cannot say which question a value belongs to (§7.4).
//
// It then WAITS, exactly as a submission does. Answering resumes the peer's turn, and a handle
// that comes back saying WORKING means the work just started — handing that back to the model as
// if the answer were done would be reporting a state nobody has reached yet (§6.2, §8.2).
func (c *remoteClient) Reply(ctx context.Context, id, questionID string, answer map[string]string, budget time.Duration) (*Delegation, error) {
	d, err := c.delegations.find(id)
	if err != nil {
		return nil, err
	}
	if d.RemoteTaskID == "" {
		return nil, noHandle(d, "no question to answer")
	}
	// The record is the authority on whether a question is open: it is what the peer's last
	// answer said, and an answer to a task that asks nothing would buy a round trip only to be
	// refused (§7.4). A caller that names a DIFFERENT question is refused here too, for the same
	// reason the receiving side refuses it: an answer must go to the question it answers.
	if d.QuestionID == "" {
		return nil, fmt.Errorf("node: delegation %s is not waiting for an answer (it is %s)",
			id, viewState(d.LastKnown.State))
	}
	if questionID != "" && questionID != d.QuestionID {
		return nil, fmt.Errorf("node: delegation %s is waiting for question %q, not %q", id, d.QuestionID, questionID)
	}
	questionID = d.QuestionID

	msg := a2a.NewMessage(a2a.MessageRoleUser, answerParts(answer)...)
	msg.TaskID = a2a.TaskID(d.RemoteTaskID)
	msg.Metadata = map[string]any{QuestionIDMetadataKey: questionID}
	msg.ContextID = d.RemoteContextID

	res, err := c.send(ctx, d, msg)
	if err != nil {
		return d, err
	}
	d, err = c.recordSend(d, res)
	if err != nil {
		return d, err
	}
	// Same two shapes as a submission: a direct Message ends it, a terminal task does.
	if res.Message != nil {
		return d, nil
	}
	if settlesWait(res.Task.Status.State) {
		return d, nil
	}
	return c.Wait(ctx, d.ID, budget)
}

// Cancel asks the peer to stop a task, and reports what the peer actually resolved to — not
// that a request was sent (§7.3). Side effects already performed are not rolled back.
func (c *remoteClient) Cancel(ctx context.Context, id string) (*Delegation, error) {
	d, err := c.delegations.find(id)
	if err != nil {
		return nil, err
	}
	if d.RemoteTaskID == "" {
		return nil, noHandle(d, "nothing to cancel")
	}

	body, err := c.do(ctx, d, http.MethodPost, restTasksPath+d.RemoteTaskID+cancelSuffix, nil)
	if err != nil {
		return d, err
	}
	task, err := decodeTask(body)
	if err != nil {
		return d, fmt.Errorf("node: %s answered a cancel with something this node cannot read: %w", d.Peer, err)
	}
	return c.record(d, task)
}

// Submit sends the request and records it — BEFORE the request leaves, so "we do not know
// whether it was accepted" is a state the record can be in rather than a guess (§9.2).
func (c *remoteClient) Submit(ctx context.Context, req SubmitRequest) (*Delegation, error) {
	// The alias is the only thing checked here: a peer whose CARD could not be read is not
	// necessarily unable to work (the card is how this machine describes a node, not how it
	// reaches one), and refusing on it would turn a slow well-known fetch into "cannot delegate".
	// Whether the peer is there is decided by the request itself — and if it is not, the record
	// says the outcome is unknown rather than inventing a refusal (§9.2).
	p, err := c.peers.Lookup(ctx, req.Peer)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("node: a delegation needs a prompt")
	}

	contextID, reference := "", ""
	if req.ContinueFrom != "" {
		prev, err := c.delegations.find(req.ContinueFrom)
		if err != nil {
			return nil, err
		}
		if prev.Peer != req.Peer {
			// A follow-up continues the conversation it belongs to: sending it to another node
			// would be a different conversation wearing the same words (§8.5).
			return nil, fmt.Errorf("node: %s was delegated to %s, not to %s", prev.ID, prev.Peer, req.Peer)
		}
		if prev.RemoteContextID == "" {
			// Either the submission's fate is unknown, or the peer answered it directly without
			// naming a conversation. Both mean "there is nothing here to continue" — and neither
			// may be turned into a fresh conversation behind the caller's back (§8.5).
			why := "its submission was never accepted"
			if prev.DirectAnswer {
				why = "it was answered directly, and the peer named no conversation"
			}
			return nil, fmt.Errorf("node: %s has no remote conversation yet (%s), so it cannot be continued",
				prev.ID, why)
		}
		contextID = prev.RemoteContextID
		// The PREVIOUS remote task is a reference, not this message's task: §8.1 is explicit
		// that a follow-up is new work in the same conversation.
		reference = prev.RemoteTaskID
	}

	title := req.Title
	if title == "" {
		title = deriveTitle(req.Prompt)
	}
	d := newDelegation(p.Name, req.LogicalID, title, req.Prompt, a2a.NewMessageID())
	d.RemoteContextID = contextID
	if err := c.delegations.put(d); err != nil {
		return nil, err
	}

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(req.Prompt))
	msg.ContextID = contextID
	if reference != "" {
		msg.ReferenceTasks = []a2a.TaskID{a2a.TaskID(reference)}
	}

	res, err := c.send(ctx, d, msg)
	if err != nil {
		return d, err
	}
	d, err = c.recordSend(d, res)
	if err != nil {
		return d, err
	}

	// A direct Message IS the outcome — there is no task to wait on.
	if res.Message != nil {
		return d, nil
	}
	// A task that is already finished carries its answer; one that is running or waiting is
	// handed back with what we know (§6.2, §8.1). The wait is bounded either way.
	if settlesWait(res.Task.Status.State) {
		return d, nil
	}
	return c.Wait(ctx, d.ID, req.Wait)
}

// sendResult is what one message:send answered with: the task the peer created, OR a direct
// Message that means no task exists at all (design §6.1 — the spec's SendMessageResponse is a
// oneOf of exactly one of these).
//
// The two are not variants of one thing and must not be flattened into one. A Task is a HANDLE:
// queryable, waitable, cancellable, continuable. A Message is an ANSWER that is simply over. The
// spec lets a peer pick either ("the agent MAY create a new Task ... or MAY return a direct
// Message response for simple interactions"), so a client that knows only the first turns a
// perfectly good answer into an error — which is exactly what this node used to do.
type sendResult struct {
	Task    *a2a.Task
	Message *a2a.Message
}

// send is one message:send and the Task or Message it answered with.
func (c *remoteClient) send(ctx context.Context, d *Delegation, msg *a2a.Message) (*sendResult, error) {
	payload := map[string]any{
		"message": msg,
		// Read the handle, then wait locally: the design's rule for every delegation (§6.2).
		"configuration": map[string]any{"returnImmediately": true},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	body, err := c.do(ctx, d, http.MethodPost, restSendPath, raw)
	if err != nil {
		return nil, err
	}

	// The envelope is the spec's StreamResponse: a task for work, a message for a reply. The spec
	// requires EXACTLY one, so "both" is as unreadable as "neither" and neither may be guessed at.
	var envelope struct {
		Task    *a2a.Task    `json:"task"`
		Message *a2a.Message `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("node: %s answered with something this node cannot read: %w", d.Peer, err)
	}
	switch {
	case envelope.Task != nil && envelope.Message != nil:
		return nil, fmt.Errorf("node: %s answered with a task and a message at once; the protocol allows exactly one", d.Peer)
	case envelope.Task != nil:
		return &sendResult{Task: envelope.Task}, nil
	case envelope.Message != nil:
		return &sendResult{Message: envelope.Message}, nil
	default:
		return nil, fmt.Errorf("node: %s answered with neither a task nor a message", d.Peer)
	}
}

// recordSend files what a message:send answered with, in whichever of the two shapes it came.
func (c *remoteClient) recordSend(d *Delegation, res *sendResult) (*Delegation, error) {
	if res.Task != nil {
		return c.record(d, res.Task)
	}
	return c.recordDirectAnswer(d, res.Message)
}

// recordDirectAnswer files a peer's direct Message.
//
// There is no task, no state to poll and nothing to cancel, so the record must neither pretend one
// exists nor leave the outcome looking UNKNOWN: the answer is right here, in the caller's hands.
//
// The contextId travels when the peer sent one (the spec REQUIRES it if the agent generated one),
// and that is what keeps `continue_from` able to continue the CONVERSATION even though this
// particular ask produced no task: the next message goes out on the same context, where the peer's
// own history can pick it up.
func (c *remoteClient) recordDirectAnswer(d *Delegation, msg *a2a.Message) (*Delegation, error) {
	now := time.Now().UTC()
	next := *d
	next.DirectAnswer = true
	next.LastKnown = lastKnown{
		// COMPLETED is the honest state for "this IS the answer": the interaction is over. Leaving
		// it UNSPECIFIED would read as "we do not know whether it was accepted", which is a
		// different fact — and the one the record reports when nothing came back at all.
		State: a2a.TaskStateCompleted,
		Text:  excerpt(messageText(msg)),
		At:    now,
	}
	// A direct Message cannot carry an answer back into the node's question slot, so whatever
	// question this machine was holding is over with it: there is no task left to answer into.
	next.QuestionID = ""
	if msg.ContextID != "" {
		next.RemoteContextID = msg.ContextID
	}
	if err := c.delegations.put(&next); err != nil {
		return d, err
	}
	return &next, nil
}

// noHandle explains why an action has nothing to act on, and the TWO reasons are different facts:
// a submission whose fate is unknown (nothing came back) versus a peer that ANSWERED with a
// Message (no task was ever going to come back). A caller that read one as the other would either
// retry work that already succeeded or give up on work the peer may still be running.
func noHandle(d *Delegation, what string) error {
	if d.DirectAnswer {
		return fmt.Errorf("node: delegation %s was answered directly by %s (no task was created), so there is %s",
			d.ID, d.Peer, what)
	}
	return fmt.Errorf("node: delegation %s was never accepted by %s (its outcome is unknown), so there is %s",
		d.ID, d.Peer, what)
}

// fetch reads the peer's current view of a task.
func (c *remoteClient) fetch(ctx context.Context, d *Delegation) (*a2a.Task, error) {
	body, err := c.do(ctx, d, http.MethodGet, restTasksPath+d.RemoteTaskID, nil)
	if err != nil {
		return nil, err
	}
	task, err := decodeTask(body)
	if err != nil {
		return nil, fmt.Errorf("node: %s answered a task query with something this node cannot read: %w", d.Peer, err)
	}
	return task, nil
}

// record writes what the peer just said into the local record, and returns the snapshot.
func (c *remoteClient) record(d *Delegation, task *a2a.Task) (*Delegation, error) {
	now := time.Now().UTC()
	next := *d
	if task.ID != "" {
		next.RemoteTaskID = string(task.ID)
	}
	if task.ContextID != "" {
		next.RemoteContextID = task.ContextID
	}
	next.LastKnown = lastKnown{
		State: task.Status.State,
		Text:  taskText(task),
		At:    now,
	}
	if task.Status.State == a2a.TaskStateInputRequired {
		if q := QuestionID(task.Status.Message); q != "" {
			next.QuestionID = q
		}
	} else {
		// The question is over: an answer was taken, the task failed, or it was cancelled.
		next.QuestionID = ""
	}
	if err := c.delegations.put(&next); err != nil {
		return d, err
	}
	return &next, nil
}

// do performs one request and classifies what came back.
//
// Every non-2xx is turned into a PeerRefusal carrying whatever reason the peer's answer had —
// our own domain included, which is the reason this client exists at all.
func (c *remoteClient) do(ctx context.Context, d *Delegation, method, path string, body []byte) ([]byte, error) {
	base, err := url.Parse(c.peers.baseURL(d.Peer))
	if err != nil {
		return nil, fmt.Errorf("node: peer %s has an unusable address: %w", d.Peer, err)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + path

	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", a2aMediaType)
	}
	req.Header.Set(a2aVersionHdr, string(a2a.Version))

	resp, err := c.http.Do(req)
	if err != nil {
		// Nothing about the request was considered: "we do not know", not a refusal.
		return nil, &PeerRefusal{Transport: true, Message: err.Error(), Peer: d.Peer}
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &PeerRefusal{Transport: true, Message: err.Error(), Peer: d.Peer}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return payload, nil
	}
	return nil, parseRefusal(resp.StatusCode, payload, d.Peer)
}

// PeerRefusal is a refusal a peer made, in the peer's own terms.
//
// `Domain == "tachi"` means the peer's admission layer refused (busy, finished, a mismatched
// answer); a protocol domain (or a transport failure) means something else happened — and the
// two must not be confused, because only our own refusals carry an action.
type PeerRefusal struct {
	Peer       string
	Status     int
	StatusText string
	Reason     string
	Domain     string
	Message    string
	Metadata   map[string]string
	// Transport is true when the peer never considered the request (dial failure, timeout,
	// unreadable body). "We do not know" is not "it said no".
	Transport bool
}

func (r *PeerRefusal) Error() string {
	if r.Transport {
		return fmt.Sprintf("node: %s could not be reached: %s", r.Peer, r.Message)
	}
	what := r.Message
	if what == "" {
		what = r.StatusText
	}
	if r.Reason == "" {
		return fmt.Sprintf("node: %s refused (%d): %s", r.Peer, r.Status, what)
	}
	return fmt.Sprintf("node: %s refused (%s): %s", r.Peer, r.Reason, what)
}

// NotAccepted reports whether the peer did NOT accept the request, which is the only case in
// which retrying with the same message id and content is safe (§9.2).
//
// A transport failure is exactly the case where it is NOT known, so it is not accepted-and-safe
// either: the caller keeps the record and reports "unknown".
func (r *PeerRefusal) NotAccepted() bool {
	if r.Transport {
		return false
	}
	switch r.Reason {
	case reasonContextBusy, reasonMessageIDReused:
		return true
	}
	return r.Status == http.StatusServiceUnavailable || r.Status == http.StatusConflict
}

// parseRefusal reads the spec's google.rpc.Status body (§11.6), keeping our own ErrorInfo.
func parseRefusal(status int, body []byte, peer string) *PeerRefusal {
	refusal := &PeerRefusal{Peer: peer, Status: status}
	var envelope struct {
		Error struct {
			Code    int               `json:"code"`
			Status  string            `json:"status"`
			Message string            `json:"message"`
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		refusal.Message = strings.TrimSpace(string(body))
		return refusal
	}
	refusal.StatusText = envelope.Error.Status
	refusal.Message = envelope.Error.Message
	for _, raw := range envelope.Error.Details {
		var info struct {
			Type     string            `json:"@type"`
			Reason   string            `json:"reason"`
			Domain   string            `json:"domain"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		if info.Type != errordetails.ErrorInfoType {
			continue
		}
		// Any domain is kept — that is the difference from the SDK's client, which drops
		// everything that is not the protocol's own.
		refusal.Reason, refusal.Domain, refusal.Metadata = info.Reason, info.Domain, info.Metadata
		break
	}
	return refusal
}

// decodeTask reads a task out of a response body, accepting both shapes the binding uses: the
// send envelope (`{"task": …}`) and the bare task a query or a cancel answers with.
func decodeTask(body []byte) (*a2a.Task, error) {
	var envelope struct {
		Task *a2a.Task `json:"task"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Task != nil {
		return envelope.Task, nil
	}
	var task a2a.Task
	if err := json.Unmarshal(body, &task); err != nil {
		return nil, err
	}
	if task.ID == "" {
		return nil, errors.New("the body carries no task")
	}
	return &task, nil
}

// settlesWait reports whether a state ends a local wait: a task that finished, failed or was
// cancelled will not change again, and one waiting for input will not move until the caller
// answers (§7.3).
func settlesWait(state a2a.TaskState) bool {
	return state.Terminal() || state == a2a.TaskStateInputRequired || state == a2a.TaskStateAuthRequired
}

// taskText is the part of a task that travels to the model: the question it is waiting on, or
// the answer it produced.
func taskText(task *a2a.Task) string {
	if task == nil {
		return ""
	}
	if task.Status.State == a2a.TaskStateInputRequired || task.Status.State == a2a.TaskStateAuthRequired {
		return excerpt(messageText(task.Status.Message))
	}
	for _, artifact := range task.Artifacts {
		if text := messageText(&a2a.Message{Parts: artifact.Parts}); text != "" {
			return excerpt(text)
		}
	}
	// A failed or cancelled task has nothing else: the status message says what happened.
	if task.Status.State.Terminal() {
		return excerpt(messageText(task.Status.Message))
	}
	return ""
}

func excerpt(s string) string {
	if len(s) <= answerExcerptLen {
		return s
	}
	return s[:answerExcerptLen] + "\n…（答复过长，已截断；完整结果在远端任务记录里）"
}

// answerParts renders an answer the way the receiving side reads one: one text part for a
// single question, and NAMED parts when several were asked (§7.4).
func answerParts(answer map[string]string) []*a2a.Part {
	if len(answer) == 1 {
		for _, v := range answer {
			return []*a2a.Part{a2a.NewTextPart(v)}
		}
	}
	named := make(map[string]any, len(answer))
	for k, v := range answer {
		named[k] = v
	}
	return []*a2a.Part{a2a.NewDataPart(named)}
}

// deriveTitle is the text a model and a reminder will show for this work.
func deriveTitle(prompt string) string {
	line := strings.TrimSpace(prompt)
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	const maxRunes = 60
	runes := []rune(line)
	if len(runes) > maxRunes {
		line = string(runes[:maxRunes]) + "…"
	}
	if line == "" {
		line = "远程任务"
	}
	return line
}

// baseURL is the address of one configured peer, for the requests above.
func (d *peerDirectory) baseURL(name string) string {
	for _, p := range d.peers {
		if p.Name == name {
			return p.URL
		}
	}
	return ""
}
