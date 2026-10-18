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
	// known is the TOFU record, for the one thing a reminder needs beyond the configuration: the
	// identity this machine pinned for a peer, which makes a name checkable (and is what a tool
	// argument may carry instead of the name).
	known *knownPeers
}

// OpenSender builds the sending side over a peer list, loading this machine's records.
//
// It is called once per entry point rather than once per turn: the records are files, and a
// tool call must not pay a directory scan.
//
// sessionsDir is where THIS process keeps the conversations a call belongs to — the entry point's
// own store root, not a global lookup — because a file a peer sends back is written into the
// session's own directory (session.SaveReceivedFile). A process with no conversations (`-p`) passes
// "" and the file goes to spillDir instead.
func OpenSender(peers []config.PeerConfig, ownName string, log *logger.Logger, store *sightingsStore, sessionsDir string) (*Sender, error) {
	records, err := openDelegationStore(config.DelegationsDir())
	if err != nil {
		return nil, err
	}
	directory, err := newPeerDirectory(peers)
	if err != nil {
		return nil, err
	}
	// What each peer proved the first time this machine reached it (§4.2: TOFU remembers the first
	// identity it saw, and says so). Keyed by identity — which is what a config line finds its own
	// record through, by the address the pin was filed under.
	known, err := openKnownPeers(config.PeerIdentitiesPath(), log)
	if err != nil {
		return nil, err
	}
	// The directory resolves identities through the pin record, so it needs it.
	directory.known = known
	// Our own identity, and the certificate it signs: what we present to a peer so it can read
	// WHO is calling out of the TLS handshake. It lives in the NODE's data dir even on a machine
	// that hosts nothing — being a caller means having a key, and a machine that only calls out
	// still has to say who it is (the alternative would be an anonymous caller, which is exactly
	// what a remote surface on TLS refuses: node/auth.go).
	identity, err := LoadOrCreateIdentity(config.A2ADir())
	if err != nil {
		return nil, err
	}
	ourCert, err := loadOrCreateCertificate(config.A2ADir(), identity)
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
		sessionsDir:    sessionsDir,
		http:           &http.Client{},
		requestTimeout: defaultRequestTimeout,
		waitBudget:     defaultWaitBudget,
		pollInterval:   defaultPollInterval,
		// Where a successful call is recorded as a CONTACT with that peer, when this process has a
		// discovery side: what this machine has reached recently is what it may vouch for (node/
		// sightings.go touch). Assigned below, beside the store the directory reads.
	}
	// The Agent Card is read through the SAME client, and that is not a detail: the certificate a
	// Tachi node serves is self-signed, so no system verifier accepts it, and on an mTLS surface
	// the card needs a client certificate as well. The library's default resolver can read
	// neither — through it every peer would look unreachable and the reminder would say so.
	directory.resolveEntry = client.fetchCard
	// Discovery feeds the directory, and the directory feeds discovery's verifier: a sighting enters
	// only after that connection has proved an identity, and the same client is what makes it — the
	// card it reads is cached where the directory will find it (node/peers.go attachCard).
	//
	// What the directory reads is the store, not one of its sources: the store is what holds the
	// confirmed set (node/sightings.go), a browser is only one of the things that fill it.
	if store != nil {
		directory.discover = store.sightings
		store.setVerifier(client.verifierOf())
		client.store = store
	}
	return &Sender{peers: directory, records: records, client: client, known: known}, nil
}

// Peers describes every node in the directory, never failing: an unreachable one is reported as such
// rather than dropped, because "not listed" would read as "not configured" (§8.6). A node that
// discovery found is described here too — it is a node this machine may delegate to — and a node this
// machine holds no identity for is listed and marked, because the alternative is a reader who cannot
// tell "not configured" from "not reachable yet".
func (s *Sender) Peers(ctx context.Context) []tools.PeerSummary {
	if s == nil {
		return nil
	}
	snapshot := s.peers.Snapshot(ctx)
	out := make([]tools.PeerSummary, 0, len(snapshot))
	for _, p := range snapshot {
		info := tools.PeerSummary{Name: p.Name(), NodeID: p.Identity, Note: p.Note, Reachable: p.Card != nil}
		if p.Card != nil {
			info.Description = p.Card.Description
			// The peer's OWN claim about what it can do. Names only: the reminder is a list the model
			// reads every turn (when it changes), and a description per skill would be a paragraph
			// per node.
			for _, skill := range p.Card.Skills {
				name := strings.TrimSpace(skill.Name)
				if name == "" {
					name = strings.TrimSpace(skill.ID)
				}
				if name != "" {
					info.Skills = append(info.Skills, name)
				}
			}
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

// displayName is the name to SHOW for the peer a record belongs to: the name this machine uses NOW,
// resolved through the identity the record binds.
//
// A record keeps the name it was created with, and after a rename that stale name is one the config
// no longer knows — the reader would see a machine that is not configured anywhere. The identity is
// what the record really binds, so it is what resolves the current name (and it costs nothing: the
// directory is keyed by identity).
func (s *Sender) displayName(d *Delegation) string {
	if s != nil {
		if e, ok := s.peers.byIdentity(d.PeerNodeID); ok {
			return e.Name()
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
		NodeID:   d.PeerNodeID,
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
