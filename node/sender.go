package node

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
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
func OpenSender(peers []config.PeerConfig, ownName string, log *logger.Logger) (*Sender, error) {
	records, err := openDelegationStore(config.DelegationsDir())
	if err != nil {
		return nil, err
	}
	directory, err := newPeerDirectory(peers)
	if err != nil {
		return nil, err
	}
	// What each peer proved the first time this machine reached it (§4.2: TOFU remembers the first
	// identity it saw, and says so). Keyed by alias, which is what a config entry is keyed by.
	urls := make(map[string]string, len(peers))
	for _, p := range peers {
		urls[p.Name] = p.URL
	}
	known, err := openKnownPeers(config.PeerIdentitiesPath(), urls, log)
	if err != nil {
		return nil, err
	}
	// Our own identity, and the certificate it signs: what we present to a peer so it can read
	// WHO is calling out of the TLS handshake. It lives in the NODE's data dir even on a machine
	// that hosts nothing — being a caller means having a key, and a machine that only calls out
	// still has to say who it is (the alternative would be an anonymous caller, which is exactly
	// what a remote surface on TLS refuses: node/auth.go).
	identity, err := LoadOrCreateIdentity(config.NodeDir())
	if err != nil {
		return nil, err
	}
	ourCert, err := loadOrCreateCertificate(config.NodeDir(), identity)
	if err != nil {
		return nil, err
	}
	client := &remoteClient{
		peers:          directory,
		delegations:    records,
		known:          known,
		ourCert:        ourCert,
		ourName:        strings.TrimSpace(ownName),
		spillDir:       config.ToolResultsDir(),
		http:           &http.Client{},
		requestTimeout: defaultRequestTimeout,
		waitBudget:     defaultWaitBudget,
		pollInterval:   defaultPollInterval,
	}
	// The Agent Card is read through the SAME client, and that is not a detail: the certificate a
	// Tachi node serves is self-signed, so no system verifier accepts it, and on an mTLS surface
	// the card needs a client certificate as well. The library's default resolver can read
	// neither — through it every peer would look unreachable and the reminder would say so.
	directory.resolve = func(ctx context.Context, alias, baseURL string) (*a2a.AgentCard, error) {
		return client.fetchCard(ctx, alias, baseURL)
	}
	return &Sender{peers: directory, records: records, client: client}, nil
}

// Peers describes every configured node, never failing: an unreachable one is reported as such
// rather than dropped, because "not listed" would read as "not configured" (§8.6).
func (s *Sender) Peers(ctx context.Context) []tools.PeerSummary {
	if s == nil {
		return nil
	}
	snapshot := s.peers.Snapshot(ctx)
	out := make([]tools.PeerSummary, 0, len(snapshot))
	for _, p := range snapshot {
		info := tools.PeerSummary{Name: p.Name, Note: p.Note, Reachable: p.Card != nil}
		if p.Card != nil {
			info.Description = p.Card.Description
		}
		out = append(out, info)
	}
	return out
}

// Delegate asks a peer for work (the tool layer's name for a submission).
func (s *Sender) Delegate(ctx context.Context, req tools.DelegationRequest) (tools.RemoteTaskInfo, error) {
	d, err := s.client.Submit(ctx, SubmitRequest{
		Peer: req.Peer, Prompt: req.Prompt, Title: req.Title,
		LogicalID: req.LogicalID, ContinueFrom: req.ContinueFrom, Wait: req.Wait,
	})
	if err != nil {
		if d != nil {
			return s.viewOf(d, err.Error()), err
		}
		return tools.RemoteTaskInfo{}, err
	}
	return s.viewOf(d, ""), nil
}

// Get reports the last known state of one delegated task, asking the peer when it can.
func (s *Sender) Get(ctx context.Context, id string) (tools.RemoteTaskInfo, error) {
	d, err := s.client.Get(ctx, id)
	if err != nil {
		return tools.RemoteTaskInfo{}, err
	}
	return s.viewOf(d, ""), nil
}

// Wait waits for a delegated task to reach a state that ends the wait.
func (s *Sender) Wait(ctx context.Context, id string, budget time.Duration) (tools.RemoteTaskInfo, error) {
	d, err := s.client.Wait(ctx, id, budget)
	if err != nil {
		if d != nil {
			return s.viewOf(d, err.Error()), err
		}
		return tools.RemoteTaskInfo{}, err
	}
	return s.viewOf(d, ""), nil
}

// Reply answers the question a peer is waiting on, waiting boundedly for what the answer set
// off. A wait that runs out is not a failure: the task keeps working, and the view says so.
func (s *Sender) Reply(ctx context.Context, id, questionID string, answer map[string]string, budget time.Duration) (tools.RemoteTaskInfo, error) {
	d, err := s.client.Reply(ctx, id, questionID, answer, budget)
	if err != nil {
		if d != nil {
			return s.viewOf(d, err.Error()), err
		}
		return tools.RemoteTaskInfo{}, err
	}
	return s.viewOf(d, ""), nil
}

// Cancel asks the peer to stop a task.
func (s *Sender) Cancel(ctx context.Context, id string) (tools.RemoteTaskInfo, error) {
	d, err := s.client.Cancel(ctx, id)
	if err != nil {
		if d != nil {
			return s.viewOf(d, err.Error()), err
		}
		return tools.RemoteTaskInfo{}, err
	}
	return s.viewOf(d, ""), nil
}

// RemoteTasks reports what ONE local conversation delegated, newest first (§8.2).
//
// The filter is not a convenience: without it a `list` would show work from every conversation
// this machine has ever had, which is both useless and a leak between conversations (§8.5).
func (s *Sender) RemoteTasks(logicalID string) []tools.RemoteTaskInfo {
	if s == nil {
		return nil
	}
	records := s.records.list(logicalID)
	out := make([]tools.RemoteTaskInfo, 0, len(records))
	for _, d := range records {
		out = append(out, s.viewOf(d, ""))
	}
	return out
}

// displayName is the name to SHOW for the peer a record belongs to: the alias this machine uses
// NOW, when the record's identity is still configured under one.
//
// A record keeps the alias it was created with, and after a rename that stale name is one the
// config no longer knows — the model would read it in a reminder and then have its tool call
// refused as an unknown peer. The identity is what the record really binds (see
// remoteClient.currentAliasFor), so it is what resolves the current name.
func (s *Sender) displayName(d *Delegation) string {
	if s != nil && s.client != nil {
		if cur := s.client.currentAliasFor(d.PeerNodeID); cur != "" {
			return cur
		}
	}
	return d.Peer
}

// viewOf turns a record into the view the tools and the reminder both speak in.
//
// A task waiting for input carries the QUESTION as its text and no result: handing the same
// string over twice would put a question in the model's hands as if it were an answer.
func (s *Sender) viewOf(d *Delegation, note string) tools.RemoteTaskInfo {
	view := tools.RemoteTaskInfo{
		ID:       d.ID,
		Node:     s.displayName(d),
		Title:    d.Title,
		State:    viewState(d.LastKnown.State),
		Result:   d.LastKnown.Text,
		LastSeen: d.LastKnown.At,
		Note:     note,
	}
	if d.QuestionID != "" {
		view.QuestionID, view.QuestionText = d.QuestionID, d.LastKnown.Text
		view.Result = ""
	}
	if d.DirectAnswer {
		// The answer is in hand and there is no handle (see Delegation.DirectAnswer). Said here
		// because the alternative is a model that tries to `wait` on or `cancel` something the
		// peer never created — and said as a NOTE rather than a refusal because the conversation
		// is still continuable when the peer named one (`continue_from`).
		direct := "the peer answered with a direct message rather than a task: this result IS the outcome, " +
			"there is no handle to query, wait on or cancel"
		if note == "" {
			note = direct
		} else {
			note += "; " + direct
		}
		view.Note = note
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
