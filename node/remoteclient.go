package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/errordetails"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
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
	// known is what each peer proved the first time it was reached (TOFU). It is consulted by the
	// TLS verification, which runs per handshake, so it is a field rather than a local.
	known *knownPeers
	// ourCert is THIS node's certificate, presented on every call: it is what the peer reads the
	// caller's identity out of. It is this machine's identity, not a per-peer credential — the same
	// certificate answers in both directions (see node/tls.go).
	ourCert tls.Certificate
	// ourName is what this node calls itself (node.name). It travels with every call so the peer's
	// records can say who asked — as a LABEL: the peer files it under `self_reported_name`, because
	// any key can claim any name (node/auth.go).
	ourName string
	// spillDir is where an answer too long for the model is kept WHOLE, beside the other oversized
	// tool results (config.ToolResultsDir): the pointer in the shortened text has to be a path this
	// machine can actually read (boundAnswer).
	spillDir string
	http     *http.Client

	// clients caches one HTTP client per peer: a TLS configuration belongs to ONE peer (its pin,
	// its address), and a client is the object that carries one. Built lazily so a peer this
	// machine never calls costs nothing.
	mu      sync.Mutex
	clients map[string]*http.Client

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

	// cardBodyLimit bounds how much of an Agent Card this client will read. A card is a small
	// JSON document; anything enormous is a misconfigured or hostile endpoint, and reading it
	// whole would be paying for that misconfiguration in memory.
	cardBodyLimit = 1 << 20

	// answerExcerptLen bounds how much of a remote answer travels into a tool result or a
	// reminder. The result is for the model to read, not to archive: a long answer belongs in
	// the peer's task record, which stays queryable (§7.3 大结果做上限控制).
	//
	// It counts CHARACTERS, and that is a decision rather than an accident: the budget is an
	// amount of text for a model to read, so measuring it in bytes would make it worth a third as
	// much to a Chinese answer as to an English one (see excerpt).
	answerExcerptLen = 4000

	// excerptNote travels with an answer that had to be shortened AND could not be kept whole (a
	// spill that failed: no configured directory, a full disk, permissions). It says BOTH halves of
	// what a reader needs — the rest is not lost, and this handle cannot read it.
	excerptNote = "\n（答复过长，已截断；完整答复留在对端的任务记录里，工具这边拿不到更多，需要就分块索取）"
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
	// A follow-up may name NO peer at all, and that is the normal shape of one (§8.1's own example
	// is `{"continue_from": "d_7f3a", "prompt": …}`): the record says which machine it belongs to,
	// and §8.1 puts the resolution on this side ("服务端由它解析远端 nodeId/contextId"). The tool's
	// contract — exactly one of node / continue_from (tools.DelegateTask) — is written against that,
	// so a continuation that arrives here without an alias is doing what it was told.
	if req.ContinueFrom != "" && strings.TrimSpace(req.Peer) == "" {
		peer, err := c.peerForContinuation(ctx, req.ContinueFrom)
		if err != nil {
			return nil, err
		}
		req.Peer = peer
	}

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
		// A follow-up continues the conversation it belongs to: sending it to another MACHINE
		// would be a different conversation wearing the same words (§8.5). The check is on the
		// identity the record holds, not on the local alias, because the alias is this machine's
		// name for a peer and may be pointed somewhere else tomorrow (design §4.1: 持久化远端绑定
		// 使用 node_id). That is what lets a rename stay a rename.
		if prev.PeerNodeID != "" {
			got, err := c.verifyPeerIdentity(ctx, req.Peer)
			if err != nil {
				return nil, err
			}
			if got != "" && got != prev.PeerNodeID {
				return nil, fmt.Errorf("node: %s belongs to node %s, but %s is node %s",
					prev.ID, prev.PeerNodeID, req.Peer, got)
			}
		} else if prev.Peer != req.Peer {
			// No identity to compare — a plaintext peer (loopback), where there is nothing else to
			// go on. The alias is all this record ever had.
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

// peerForContinuation is which machine a follow-up belongs to, asked of the RECORD rather than the
// caller — the whole reason a continuation does not have to name a peer (Submit).
//
// It resolves in three steps, cheapest first, and each one exists because the others cannot cover
// the case it does:
//
//  1. the pin record: if this identity has been reached under an alias, that alias is the name the
//     config uses NOW (no request);
//  2. the alias the record was created with: the everyday case, where nothing was renamed;
//  3. asking the configured peers which one this identity is — the only way a RENAME can be
//     followed, because a record stores the alias it was made with and the new alias is a word
//     only the config knows. One authenticated card request per peer (the same one the
//     confirmation below makes anyway), on a path that only runs after a rename.
//
// The identity is what makes 1 and 3 possible at all, which is why the ORDER matters: asking the
// recorded alias first would turn a rename into "not configured any more" and lose a conversation
// that is still there (§4.1: 别名变化不能改变任务归属).
//
// What this does NOT do is decide whether the machine is still the right one: that comparison
// happens in Submit, before any message is sent (§8.1).
func (c *remoteClient) peerForContinuation(ctx context.Context, id string) (string, error) {
	prev, err := c.delegations.find(id)
	if err != nil {
		return "", err
	}
	if current := c.currentAliasFor(prev.PeerNodeID); current != "" {
		return current, nil
	}
	if _, ok := c.peers.configOnly(prev.Peer); ok {
		return prev.Peer, nil
	}
	if prev.PeerNodeID != "" {
		for _, alias := range c.peers.configOnlyNames() {
			if got, err := c.verifyPeerIdentity(ctx, alias); err == nil && got == prev.PeerNodeID {
				return alias, nil
			}
		}
		return "", fmt.Errorf("node: %s belongs to node %s, which no configured peer answers as; "+
			"configure that node again to continue it", id, prev.PeerNodeID)
	}
	// A plaintext (loopback) record has no identity to look for and only ever had the alias.
	return "", fmt.Errorf("node: %s was delegated to %q, which is not a configured peer any more", id, prev.Peer)
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
		Text:  c.boundAnswer(d.ID, messageText(msg)),
		At:    now,
	}
	// A direct Message cannot carry an answer back into the node's question slot, so whatever
	// question this machine was holding is over with it: there is no task left to answer into.
	next.QuestionID = ""
	c.stampPeerIdentity(&next)
	if msg.ContextID != "" {
		next.RemoteContextID = msg.ContextID
	}
	if err := c.delegations.put(&next); err != nil {
		return d, err
	}
	return &next, nil
}

// stampPeerIdentity records WHICH machine this record's peer turned out to be.
//
// The value comes from the TLS handshake (node/tls.go), which is the only thing that verified it —
// so it is a fact this machine checked rather than one a peer claimed, and the record keeps it for
// the reason the field exists: an alias reconfigured to another machine must not silently adopt
// the tasks of the old one. A plaintext (loopback) peer has no verified identity, and the field
// stays empty rather than being filled with a guess.
func (c *remoteClient) stampPeerIdentity(next *Delegation) {
	if c.known == nil {
		return
	}
	if id, ok := c.known.nodeID(next.Peer); ok {
		next.PeerNodeID = id
	}
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
		Text:  c.boundAnswer(d.ID, taskText(task)),
		At:    now,
	}
	c.stampPeerIdentity(&next)
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
	pc, ok := c.peers.configOnly(d.Peer)
	if !ok {
		// The record names a peer that is no longer configured. Refused rather than sent
		// unsigned: an unsigned request to whatever now answers at that address would be a
		// delegation to a machine this node does not know.
		return nil, &UnknownPeerError{Name: d.Peer}
	}
	base, err := url.Parse(pc.URL)
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
	if c.ourName != "" {
		// See node/auth.go: a label, not a credential.
		req.Header.Set(nodeNameHeader, c.ourName)
	}

	resp, err := c.clientFor(pc).Do(req)
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
// the answer it produced. It extracts, and does NOT bound: bounding either shrinks the text or
// keeps the whole thing on this machine first, and that decision belongs to boundAnswer.
func taskText(task *a2a.Task) string {
	if task == nil {
		return ""
	}
	if task.Status.State == a2a.TaskStateInputRequired || task.Status.State == a2a.TaskStateAuthRequired {
		return messageText(task.Status.Message)
	}
	for _, artifact := range task.Artifacts {
		if text := messageText(&a2a.Message{Parts: artifact.Parts}); text != "" {
			return text
		}
	}
	// A failed or cancelled task has nothing else: the status message says what happened.
	if task.Status.State.Terminal() {
		return messageText(task.Status.Message)
	}
	return ""
}

// excerpt bounds a peer's answer for the model's benefit. It reports whether it had to cut, because
// the caller still holds the whole text and may be able to keep it (boundAnswer).
//
// The budget is CHARACTERS (strutil owns the mechanics, by house rule; Bash, WebFetch and SubAgent
// spill on the same unit). A byte cap — which this was — means two things at once and neither is
// intended: a Chinese answer is worth a third as much as an English one, and the cut can land
// inside a multi-byte character — an invalid UTF-8 tail, which the json on the way out turns into a
// replacement character, so the last thing a reader sees is corruption rather than a truncation.
func excerpt(s string) (string, bool) {
	kept := strutil.TruncatePlain(s, answerExcerptLen)
	return kept, kept != s
}

// boundAnswer keeps an answer inside the budget the model reads — and, when it does not fit, keeps
// the WHOLE thing on this machine before shortening it.
//
// Shortening in place used to be the end of the text. The full answer is here for one instant (it
// was just decoded off the wire) and, once the record holds an excerpt, NOTHING on this machine has
// the rest again: the peer keeps its own copy, and every route back to us (get, wait, list) bounds
// it the same way. So the model could not reach the remainder at any price, which is what "ask the
// peer for less next time" was really saying.
//
// Spilling is the house's shape for this (Bash, WebFetch, SubAgent), with one deliberate
// difference: unlike theirs, the head is KEPT. The first characters of an answer are usually the
// answer, and this same text is what a reminder shows — a pointer alone would make every long
// answer cost a ReadFile, and turn a summary into a file path.
func (c *remoteClient) boundAnswer(localID, text string) string {
	kept, cut := excerpt(text)
	if !cut {
		return text
	}
	path, err := c.spillAnswer(localID, text)
	if err != nil {
		// A spill that cannot happen (no directory, a full disk, permissions) must not cost the
		// answer its first characters as well: the excerpt still beats a failed tool call.
		return kept + excerptNote
	}
	return kept + fmt.Sprintf("\n（答复过长，已截断；完整答复就在本机：%s —— 用 ReadFile 分页读，或向对端分块索取）", path)
}

// spillAnswer writes the full text beside the other oversized tool results and returns its path.
//
// The NAME comes from the local record instead of a timestamp, and that is about more than tidiness:
// this text travels into the reminder, so the path is part of a rendered prompt — and a reminder
// whose rendering changes for no reason is one the prompt prefix pays for on every turn
// (docs/agents/prompt-cache.md）。A stable name also makes a re-spill — the same answer seen again
// by a `get` — overwrite its own file instead of filling the directory with near-copies.
//
// The file is a COPY of what the peer said, on THIS machine: it is deliberately not a remote path,
// which is the thing the design forbids handing the model as if it were locally readable (§6.4).
func (c *remoteClient) spillAnswer(localID, text string) (string, error) {
	if c.spillDir == "" {
		return "", errors.New("node: no directory is configured for oversized answers")
	}
	path := filepath.Join(c.spillDir, "remote_"+localID+".txt")
	if err := fileutil.WriteFilePrivate(path, []byte(text)); err != nil {
		return "", err
	}
	return path, nil
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

// verifyPeerIdentity establishes which machine answers at a peer's configured address, WITHOUT
// sending it any work.
//
// It exists for the one decision that must be made before a message leaves: whether the node the
// caller named is the one a record belongs to (see Submit). The proof is the same handshake every
// call goes through — the card request is just the cheapest authenticated request there is — and
// the answer comes out of the TOFU record, which is written by the handshake and by nothing else.
//
// An empty answer means the peer has no identity to prove: a plaintext (loopback) address, where
// the caller falls back to the alias.
func (c *remoteClient) verifyPeerIdentity(ctx context.Context, alias string) (string, error) {
	if _, err := c.fetchCard(ctx, alias, ""); err != nil {
		return "", err
	}
	id, _ := c.known.nodeID(alias)
	return id, nil
}

// currentAliasFor is the name this machine NOW uses for a node id, or "" when it is not configured
// any more.
//
// It exists so a rename reads as a rename everywhere: a record keeps the alias it was created
// with, and showing that stale name would have the model (and the reader) calling a peer by a name
// the config no longer knows (node/peers.go would refuse it as unknown). The lookup is local: the
// TOFU record already maps each configured alias to the key it proved.
func (c *remoteClient) currentAliasFor(nodeID string) string {
	if nodeID == "" || c.known == nil {
		return ""
	}
	for _, alias := range c.peers.configOnlyNames() {
		if id, ok := c.known.nodeID(alias); ok && id == nodeID {
			return alias
		}
	}
	return ""
}

// fetchCard reads one peer's Agent Card over that peer's own TLS client.
//
// It is a hand-written GET rather than agentcard.DefaultResolver for the reason in OpenSender: the
// resolver verifies the peer's certificate the way the system does, which a self-signed node
// certificate never passes, and it presents no client certificate either. Both of those are the
// POINT of this node's TLS, so the card has to travel over the same connection a delegation does.
func (c *remoteClient) fetchCard(ctx context.Context, alias, baseURL string) (*a2a.AgentCard, error) {
	pc, ok := c.peers.configOnly(alias)
	if !ok {
		return nil, &UnknownPeerError{Name: alias}
	}
	if pc.URL == "" {
		pc.URL = baseURL
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(pc.URL, "/")+a2asrv.WellKnownAgentCardPath, nil)
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	req.Header.Set(a2aVersionHdr, string(a2a.Version))

	resp, err := c.clientFor(pc).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cardBodyLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node: %s answered the card request with %d", alias, resp.StatusCode)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, fmt.Errorf("node: %s answered the card request with something unreadable: %w", alias, err)
	}
	if card.Name == "" {
		return nil, fmt.Errorf("node: %s served an empty card", alias)
	}
	return &card, nil
}

// clientFor returns the HTTP client for one peer: the shared one for plaintext loopback, and a
// per-peer client carrying its TLS pin for anything else.
//
// The pin is read ONCE per peer per process. A peer's KEY changing at runtime is not a
// case worth handling (the sender is rebuilt when the peer list changes — node/delegatorcache.go,
// and a re-keyed peer is a mismatch the pin refuses by design), and re-reading it per request
// would put a lock and a map lookup in front of every poll.
func (c *remoteClient) clientFor(pc peer) *http.Client {
	u, err := url.Parse(pc.URL)
	if err != nil || u.Scheme != "https" {
		return c.http
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clients == nil {
		c.clients = make(map[string]*http.Client, 1)
	}
	if cl, ok := c.clients[pc.Name]; ok {
		return cl
	}
	cl := &http.Client{Transport: &http.Transport{
		TLSClientConfig: peerTLSConfig(c.ourCert, c.known, pc.Name),
	}}
	c.clients[pc.Name] = cl
	return cl
}
