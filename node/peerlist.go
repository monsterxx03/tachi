package node

import (
	"context"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/agentref"
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
	// rows is the peer DIRECTORY, read through a provider rather than captured: a picker still
	// offering a machine that is no longer configured — or missing one that just announced itself —
	// would be lying about what this machine can reach. It reads the directory and nothing else: no
	// network, so a keystroke cannot wait on one.
	rows func() []agentref.Peer
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
func NewPeerList(rows func() []agentref.Peer, probe func(ctx context.Context, force bool) []PeerReport, log *logger.Logger) *PeerList {
	if log == nil {
		log = logger.New("node")
	}
	return &PeerList{rows: rows, probe: probe, log: log}
}

// directoryRows reads the provider, tolerating a list that is not there at all (an entry point that
// cannot delegate at all, or one that has not loaded its config yet).
func (l *PeerList) directoryRows() []agentref.Peer {
	if l == nil || l.rows == nil {
		return nil
	}
	return l.rows()
}

// Snapshot is the list as it stands, as the PICKER needs it: every configured node, with the
// identity this machine has pinned for it (if any) and the last probe's verdict (if there was one).
// It never blocks.
func (l *PeerList) Snapshot() []agentref.Peer {
	if l == nil {
		return nil
	}
	rows := l.directoryRows()

	l.mu.Lock()
	probed := make(map[string]probed, len(l.probed))
	for key, p := range l.probed {
		probed[key] = p
	}
	l.mu.Unlock()

	out := make([]agentref.Peer, 0, len(rows))
	for _, row := range rows {
		// A node this machine holds no identity for is still a row: the picker says why in its own
		// words rather than hiding it (node/peerdir.go Display).
		if got, ok := probed[row.Key]; ok {
			row.Checked = true
			row.Reachable = got.report.Reachable
			if got.report.Card != nil {
				row.Description = got.report.Card.Description
			}
		}
		out = append(out, row)
	}
	return out
}

// PeerDetail is one node as the BOARD shows it: the address this machine dials, the identity it
// pinned, what the node answered, and when that answer was taken.
//
// It is the same probe as Snapshot's, projected differently — one map of answers serves both, so
// opening the board and typing '#' cannot disagree about what is up.
type PeerDetail struct {
	Label string
	Name  string
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

// Details is the board's view: what this machine knows about each node, and the last answer about it.
func (l *PeerList) Details() []PeerDetail {
	if l == nil {
		return nil
	}
	rows := l.directoryRows()

	l.mu.Lock()
	probed := make(map[string]probed, len(l.probed))
	for key, p := range l.probed {
		probed[key] = p
	}
	l.mu.Unlock()

	out := make([]PeerDetail, 0, len(rows))
	for _, row := range rows {
		detail := PeerDetail{
			Label: row.Label,
			Name:  row.Name,
			// The identity THIS machine pinned, empty when it holds none. The board shows the
			// difference, because it is the difference between "reachable at that address" and
			// "known to be THAT machine".
			NodeID: row.NodeID,
			// Where this machine will dial, and the line the config wrote about the node: both are
			// facts the DIRECTORY holds, so the board has them before anyone has asked anything —
			// only the answer below has to wait for a probe.
			URL:  row.Addr,
			Note: row.Note,
		}
		if got, ok := probed[row.Key]; ok {
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
	if l == nil || l.probe == nil || len(l.directoryRows()) == 0 {
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
	if l == nil || l.probe == nil || len(l.directoryRows()) == 0 {
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
		next[r.Key] = probed{report: r, at: now}
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

// RowsOf turns the sending side — or the absence of one — into the picker's row provider.
//
// It exists so the two frontends that offer the '#' picker spell "the directory, if this process can
// delegate at all" the same way: with no sending side there are no peers by definition, and an empty
// list is the honest answer rather than one frontend inventing a config-only view the other lacks.
func RowsOf(sender func() *Sender) func() []agentref.Peer {
	return func() []agentref.Peer {
		s := sender()
		if s == nil {
			return nil
		}
		return s.DirectoryRows()
	}
}

// DirectoryRows is the directory as the picker needs it: one row per machine, with the identity this
// machine holds for it (if any). It reads nothing from the network.
func (s *Sender) DirectoryRows() []agentref.Peer {
	if s == nil {
		return nil
	}
	entries := s.peers.Entries()
	out := make([]agentref.Peer, 0, len(entries))
	for _, e := range entries {
		out = append(out, agentref.Peer{
			Key:    e.key(),
			Label:  e.Label,
			Name:   e.Name(),
			NodeID: e.Identity,
			Addr:   firstAddr(e),
			Note:   e.Note,
		})
	}
	return out
}
