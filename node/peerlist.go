package node

import (
	"context"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// PeerList is what a picker needs to offer this machine's nodes: which ones exist,
// what this machine calls each of them, and whether the last look found it up.
//
// It exists because of two facts that pull in opposite directions:
//
//   - a picker asks on EVERY keystroke, so the answer has to be local and instant — the peers
//     are configuration, and the pinned identity is a small file;
//   - reachability is a network round trip per node, and a keystroke may not wait for one.
//
// So the list is a snapshot plus a background probe. Snapshot never touches the network;
// Probe answers by asking the entry point's own sender in a goroutine, and what it learns shows
// up in the NEXT snapshot. That ordering is deliberate: a row that says "checking…" for a moment
// is honest, while a row that blocks the UI for a second and then shows a stale answer is not.
//
// The probe is injected rather than built here. Every entry point that can delegate already
// holds a Sender (via DelegatorCache), and it has already loaded the identity, the certificate and
// the pinned peers — building a second one here would duplicate that work and, worse, would CREATE
// an identity and a self-signed certificate just to draw a list.
type PeerList struct {
	// peers is the configured list, read through a provider rather than captured: the peers are a
	// config leaf the reader can edit while the process runs, and a picker still offering a machine
	// the config no longer names would be lying about what this machine can reach.
	peers func() []config.PeerConfig
	// probe asks the network about every configured node. It is called from a goroutine and must be
	// safe there — an entry point resolves its sender per call rather than closing over state that
	// only its own thread may touch. force is the board's refresh travelling down.
	probe func(ctx context.Context, force bool) []PeerReport
	log   *logger.Logger

	mu sync.Mutex
	// probed is the last thing the network said about each alias, keyed by alias. Entries for
	// peers that have since been removed from the configuration stay until the next probe
	// replaces the map — the snapshot only ever reads aliases it is currently listing.
	probed map[string]probed
	// lastProbe bounds how often Probe actually goes out: a keystroke per probe would put an
	// HTTP request behind the picker for as long as the reader keeps typing.
	lastProbe time.Time
	// probing is true while a probe is out, and refreshing carries the channel a SYNCHRONOUS
	// refresh closes when its answer has landed (nil when none is running).
	probing    bool
	refreshing chan struct{}
}

// probed is one peer as the last probe found it, and when.
type probed struct {
	report PeerReport
	at     time.Time
}

const (
	// probeInterval is the shortest gap between two real probes. Short enough that the picker's
	// answer is about NOW when the reader opens it, long enough that typing does not queue up
	// requests (the sender's own card cache sits behind this too, and answers from memory when it
	// is fresh).
	probeInterval = 5 * time.Second
	// probeTimeout bounds one whole probe — every peer in the list, in parallel.
	probeTimeout = 15 * time.Second
)

// NewPeerList returns a list over the peers the provider reports.
//
// probe may be nil — a process that cannot delegate still knows which nodes EXIST, and both the
// picker and the board still offer them — in which case reachability is never checked. Its force
// argument is the board's refresh button travelling down: the implementation is what knows whether
// a cached card may be answered with.
func NewPeerList(peers func() []config.PeerConfig, probe func(ctx context.Context, force bool) []PeerReport, log *logger.Logger) *PeerList {
	if log == nil {
		log = logger.New("node")
	}
	return &PeerList{peers: peers, probe: probe, log: log}
}

// configuredPeers reads the provider, tolerating a list that is not there at all (an entry point
// that has not loaded its config yet).
func (l *PeerList) configuredPeers() []config.PeerConfig {
	if l == nil || l.peers == nil {
		return nil
	}
	return l.peers()
}

// Snapshot is the list as it stands, as the PICKER needs it: every configured node, with the
// identity this machine has pinned for it (if any) and the last probe's verdict (if there was one).
// It never blocks.
func (l *PeerList) Snapshot() []agentref.Peer {
	if l == nil {
		return nil
	}
	pinned := pinnedNodeIDs()
	peers := l.configuredPeers()

	l.mu.Lock()
	probed := make(map[string]probed, len(l.probed))
	for alias, p := range l.probed {
		probed[alias] = p
	}
	l.mu.Unlock()

	out := make([]agentref.Peer, 0, len(peers))
	for _, p := range peers {
		peer := agentref.Peer{
			Alias: p.Name,
			Note:  p.Note,
			// Absent from the record means this machine has never reached that node outbound.
			// The picker says so in its own words; what it must not do is show the name the node
			// reports about itself (node/knownpeers.go).
			NodeID: pinned[p.Name],
		}
		if got, ok := probed[p.Name]; ok {
			peer.Checked = true
			peer.Reachable = got.report.Reachable
			if got.report.Card != nil {
				peer.Description = got.report.Card.Description
			}
		}
		out = append(out, peer)
	}
	return out
}

// PeerDetail is one node as the BOARD shows it: the address this machine dials, the identity it
// pinned, what the node answered, and when that answer was taken.
//
// It is the same probe as Snapshot's, projected differently — one map of answers serves both, so
// opening the board and typing '#' cannot disagree about what is up.
type PeerDetail struct {
	Alias string
	// NodeID is the pinned identity, empty when this machine has never called that node.
	NodeID string
	URL    string
	Note   string
	// Reachable/Checked/Card/Err are the last probe's answer, and Checked false means nobody has
	// asked yet — which the board shows as such, never as "down".
	Reachable bool
	Checked   bool
	CheckedAt time.Time
	Card      *PeerCard
	Err       string
}

// Details is the board's view: configuration, pinned identity and the last answer, per node.
func (l *PeerList) Details() []PeerDetail {
	if l == nil {
		return nil
	}
	pinned := pinnedNodeIDs()
	peers := l.configuredPeers()

	l.mu.Lock()
	probed := make(map[string]probed, len(l.probed))
	for alias, p := range l.probed {
		probed[alias] = p
	}
	l.mu.Unlock()

	out := make([]PeerDetail, 0, len(peers))
	for _, p := range peers {
		detail := PeerDetail{
			Alias:  p.Name,
			NodeID: pinned[p.Name],
			URL:    p.URL,
			Note:   p.Note,
		}
		if got, ok := probed[p.Name]; ok {
			detail.Checked = true
			detail.CheckedAt = got.at
			detail.Reachable = got.report.Reachable
			detail.Card = got.report.Card
			detail.Err = got.report.Err
		}
		out = append(out, detail)
	}
	return out
}

// Probe refreshes reachability in the BACKGROUND: it returns immediately, and the answer lands in
// a later Snapshot/Details. Calls that arrive while one probe is running, or within probeInterval of
// the last one, do nothing at all — the picker calls this on every keystroke.
func (l *PeerList) Probe() { l.probeAsync(false) }

// ProbeNow refreshes reachability now, ignoring probeInterval: this is the board's refresh button,
// where a person pressed something and is waiting.
//
// It still refuses to stack with a probe already running — a second one would put two rounds of
// requests on the wire to learn the same thing — and the call itself stays asynchronous: the answer
// reaches the caller's NEXT read, so nobody's UI thread waits on a network.
func (l *PeerList) ProbeNow() { l.probeAsync(true) }

// Refresh probes NOW and returns once the answer has landed: the board's refresh button, where a
// person is waiting and a spinner is honest.
//
// It is synchronous where ProbeNow is not, and a second caller WAITS for the round already in
// flight rather than starting its own — two people pressing 刷新 must not put two rounds of requests
// on the wire to learn the same thing.
func (l *PeerList) Refresh(ctx context.Context) {
	if l == nil || l.probe == nil || len(l.configuredPeers()) == 0 {
		return
	}

	l.mu.Lock()
	if inflight := l.refreshing; inflight != nil {
		l.mu.Unlock()
		select {
		case <-inflight:
		case <-ctx.Done():
		}
		return
	}
	inflight := make(chan struct{})
	l.refreshing = inflight
	// Hold off the background probe for the duration: it would be a second round of requests
	// answering the same question, and the later arrival would win the write.
	l.probing = true
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.refreshing = nil
		l.probing = false
		l.mu.Unlock()
		close(inflight) // after the state is settled: a waiter must see the new answers
	}()

	l.store(l.probe(ctx, true))
}

func (l *PeerList) probeAsync(force bool) {
	if l == nil || l.probe == nil || len(l.configuredPeers()) == 0 {
		return
	}

	l.mu.Lock()
	if l.probing || (!force && time.Since(l.lastProbe) < probeInterval) {
		l.mu.Unlock()
		return
	}
	l.probing = true
	l.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		// Called with NO lock held: the probe reaches into the entry point's sender, which does its
		// own locking, and the rule this codebase follows is one lock at a time.
		reports := l.probe(ctx, force)
		l.store(reports)
		l.log.Debug(context.Background(), "peer list: probed the configured nodes",
			"peers", len(reports), "reachable", countReachable(reports), "forced", force)
	}()
}

// store records one round of answers and clears the in-flight marker.
//
// A peer that answered with nothing at all is NOT recorded as unreachable here: the sender promises
// an entry per configured peer, and a missing one means the probe did not get to it (a cancelled
// context, a peer removed mid-flight) — which is "not looked at yet", not "down". Leaving the alias
// out of the map is exactly that answer.
func (l *PeerList) store(reports []PeerReport) {
	now := time.Now()
	next := make(map[string]probed, len(reports))
	for _, r := range reports {
		next[r.Alias] = probed{report: r, at: now}
	}

	l.mu.Lock()
	l.probed = next
	l.probing = false
	l.lastProbe = now
	l.mu.Unlock()
}

// RefreshTimeout bounds one synchronous refresh (the board's button). The work itself is the same
// parallel probe the picker uses; this is only the ceiling a person's click waits behind.
const RefreshTimeout = probeTimeout

func countReachable(reports []PeerReport) int {
	n := 0
	for _, r := range reports {
		if r.Reachable {
			n++
		}
	}
	return n
}

// pinnedNodeIDs inverts the TOFU record into alias → node id, which is the direction a picker row
// needs (it shows the identity beside the name). It answers with an empty map when the record cannot
// be read: this is a label on a row, and a row that says "unknown" beats a picker that does not open
// (PeerAliases, the other direction, has the full reasoning).
func pinnedNodeIDs() map[string]string {
	pinned := readPinnedPeers()
	out := make(map[string]string, len(pinned))
	for nodeID, peer := range pinned {
		if peer.Alias != "" {
			out[peer.Alias] = nodeID
		}
	}
	return out
}
