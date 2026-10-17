package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// Gossip: discovery by hearsay, over the surface this node already serves.
//
// WHAT IT ADDS. Multicast does not cross a router, and a machine nobody has configured is only
// findable by something on its own network — so a fleet spread over several networks has had exactly
// one answer: write every machine down in `peers:`. Gossip is the other one: ask a node this machine
// already knows WHO ELSE it can vouch for, and it answers with machines it has itself reached
// (design: docs/2026-10-15-a2a-gossip-design.md).
//
// WHY THIS SURFACE. The route rides the remote A2A surface, so it needs no new port, no new handshake
// and no new trust: the caller's identity is the one its certificate proves, exactly as it is for a
// delegation, and a request with no certificate is not answered at all. Nothing here is an A2A
// protocol route — it is a private path on the same listener, refused in this node's own words rather
// than through the spec's error mapping.
//
// WHAT AN ANSWER MEANS: nothing. Every entry is a CLAIM by a machine this node has decided to talk to
// (it reached this node, or this node reached it), and no more than that: the address is where the
// PEER reached that machine, which is not necessarily where this one can. So an entry becomes a
// CANDIDATE and nothing else — the same door an mDNS announcement goes through
// (node/sightings.go feed), where it costs one connection and becomes a machine this node knows only
// if that connection proves an identity. A liar can spend this machine's connections and cannot make
// it believe anything.
//
// The other direction is the same rule read backwards: what goes OUT is drawn from what this process
// has proved ITSELF, and only recently (contactTTL), each entry quoting the address that answered. A
// machine this node has merely heard about is never repeated, which is what stops a rumour from
// spreading through it — and why a mesh's reach ends where actual reachability does.
const (
	// gossipPath is the private route, spelled the way the local control interface spells its own
	// (`/local/v1/handshake`): a version, and a name. It is not an A2A path — the spec's routes are
	// mounted at the root of this listener, and this one is deliberately beside them.
	gossipPath = "/node/v1/gossip"

	// maxGossipEntries bounds ONE side of an exchange, and it is deliberately the same number as the
	// directory's cap on discovered machines rather than a second one (node/peerdir.go): what a peer
	// may put in front of this machine is bounded by the same list this machine's answer is drawn
	// from. A request over it is REFUSED rather than truncated — a quietly shortened list would read
	// as "that is all I know", which is a different and false answer.
	maxGossipEntries = maxDiscoveredPeers

	// maxGossipBodyBytes caps one request body. The listener's own limit is the floor under every
	// route (maxRequestBodyBytes, 256 MiB — a size that belongs to file parts); a gossip exchange is a
	// list of small entries, so the honest limit here is the small one, and a body past it is refused
	// before this node allocates its way through it.
	maxGossipBodyBytes = 64 << 10

	// maxGossipAddrs bounds how many addresses ONE entry may carry, and its exact value matters less
	// than its being there: a machine's own account of where it is (`self.addrs`) is a handful of
	// addresses, one per interface family, while the same field is also 64 KiB of JSON a peer
	// composed. Without this, one entry can hand this machine a dial list of thousands — every one of
	// them a TLS connection carrying this machine's certificate, and (before a candidate is dropped)
	// a retry schedule behind it. The excess is dropped rather than refused, because what is past the
	// cap is not a lie about the shape of the exchange, only more addresses than any machine has.
	maxGossipAddrs = 8
)

// gossipEntry is one machine a peer says it can vouch for, as it travels: the identity, the address
// the peer reached it at, and how long ago that was.
//
// Age is SECONDS and not a timestamp, for the reason a receiver can act on: clocks on two machines
// disagree, so a claim about "when" is only readable as a distance from a shared present, and the
// receiver is the one who has a present. What it is used for is on both sides of the wire — the
// sender refuses to say anything older than the window (node/sightings.go contactWindow), and the
// receiver marks what is past it as
// worth one connection instead of a retry schedule (candidate.Stale).
type gossipEntry struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
	Age  int    `json:"age,omitempty"`
}

// gossipPayload is one exchange, in either direction. The request may carry `self`; the answer never
// does — the machine that asked just reached this one, so where this node can be reached is something
// it already knows better than this node could tell it.
type gossipPayload struct {
	// Self is where the SENDER believes it can be reached (config `node.public_url`). It is the one
	// field about the sender itself, and it exists because a machine that only calls out is otherwise
	// invisible: the TCP source address a server sees is an exit or a temporary port, never an A2A
	// listening address. As a claim it is handled like every other one — dial it, and see.
	Self  *gossipSelf   `json:"self,omitempty"`
	Peers []gossipEntry `json:"peers,omitempty"`
}

// gossipSelf is the sender's own account of where it is.
type gossipSelf struct {
	Addrs []string `json:"addrs,omitempty"`
}

// mesh is this process's discovery side as the gossip route sees it: what it may say about others,
// and where what a peer said goes. One per process, and it is whatever owns the sightings store
// (node/delegatorcache.go) — the route itself lives in the node service, which is built from config
// and deliberately holds neither.
type mesh interface {
	// vouches is what to tell a peer that asks: machines this process has proved itself, recently
	// enough to say so, each with the address that answered.
	vouches() []gossipEntry
	// receive takes what a peer said, already reduced to candidates by the handler (which is where
	// the wire's shape is known): nothing here is believed, and each candidate is dialled once before
	// it can become a sighting.
	receive(cands []candidate)
}

// The registered mesh, and the reason a package-level variable is acceptable here: the node service
// is built from config (node/config.go StartOrAttachNode) and cannot be handed the object the entry
// point owns, so the hand-off has to happen somewhere outside both. It holds ONE value, it is only
// read, and a stale registration is inert — a closed cache vouches for nothing and receives nothing
// (DelegatorCache.vouches). The alternative, a second discovery state inside the node service, would
// be a second answer to "what has this machine proved".
var (
	meshMu       sync.Mutex
	meshRegister mesh
)

// registerMesh is called by whoever brings the detection side up (NewDelegatorCache), and unregisters
// it (clearMesh) when that is closed.
func registerMesh(m mesh) {
	meshMu.Lock()
	meshRegister = m
	meshMu.Unlock()
}

// clearMesh undoes registerMesh, and says which mesh it is undoing: a process that closed one cache
// and opened another must not have its registration dropped by the closing of the first.
func clearMesh(m mesh) {
	meshMu.Lock()
	if meshRegister == m {
		meshRegister = nil
	}
	meshMu.Unlock()
}

// currentMesh is the registered mesh, or nil when this process has none — which is the answer for a
// process that hosts a node without a sending side (there is nothing it could vouch for), and for
// every test that drives the handler on its own.
func currentMesh() mesh {
	meshMu.Lock()
	defer meshMu.Unlock()
	return meshRegister
}

// gossipHandler answers one exchange.
//
// enabled is a startup fact like the rest of what this surface says about itself (its Card, its
// address): flipping it takes a restart, and it is decided by the operator in config rather than by a
// caller. It is the ONE switch for both directions — asking and answering — because a machine can
// only name machines it has itself reached, and it reached them over the network it is now being
// asked about.
func gossipHandler(enabled bool, self string, contactWindow time.Duration, log *logger.Logger) http.Handler {
	if log == nil {
		log = logger.New("node")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "gossip is a POST of one exchange", http.StatusMethodNotAllowed)
			return
		}
		// The identity comes out of the connection, never out of the request: on this surface a
		// caller presents a certificate or it is nobody (node/auth.go). A surface on loopback is
		// plaintext and identifies no one, so gossip is simply not answered there — there is nobody to
		// answer, and an anonymous answer would name this machine's peers to whoever asked.
		caller, err := callerNodeID(r)
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if !enabled {
			http.Error(w, "gossip is off on this node", http.StatusForbidden)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGossipBodyBytes))
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			http.Error(w, fmt.Sprintf("a gossip exchange is at most %d bytes", maxGossipBodyBytes),
				http.StatusRequestEntityTooLarge)
			return
		case err != nil:
			http.Error(w, "the request body could not be read", http.StatusBadRequest)
			return
		}
		var payload gossipPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "the request body is not a gossip exchange", http.StatusBadRequest)
			return
		}
		if len(payload.Peers) > maxGossipEntries {
			// Refused rather than truncated: a shortened list reads as "this is all I know", which is
			// an answer this node would be making up.
			http.Error(w, fmt.Sprintf("one exchange names at most %d machines", maxGossipEntries),
				http.StatusBadRequest)
			return
		}

		// Everything this node is willing to act on: candidates, in the order the peer listed them,
		// with the ones this node cannot use already dropped (gossipCandidates).
		here := currentMesh()
		if here != nil {
			here.receive(gossipCandidates(payload, caller, self, contactWindow, log))
		} else {
			// No sending side in this process: it has proved nothing and has nowhere to put a claim.
			// Not an error — the route exists because the node service does, and the two are separate
			// on purpose (node/delegatorcache.go).
			log.Debug(r.Context(), "node: a peer asked who this machine knows, and this process has no "+
				"discovery side to answer with", "caller", caller)
		}

		var answer []gossipEntry
		if here != nil {
			answer = gossipEntriesFor(here.vouches(), caller, self)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(gossipPayload{Peers: answer}); err != nil {
			// The response is already going out; there is nothing to say to the caller instead.
			log.Debug(r.Context(), "node: could not write a gossip answer", "caller", caller, "error", err)
		}
	})
}

// gossipCandidates reduces one received exchange to candidates: what this machine could act on, and
// nothing else.
//
// The checks are about SHAPE, and a shape check is not trust (the same rule the mDNS half follows):
// they exist so that a peer cannot spend this machine's connections on things that could never be a
// machine of this fleet — an identity that is not a node id, an address that is not somewhere on a
// local network or not HTTPS. What survives is still only a claim, and the dial behind the queue is
// what decides.
//
// Three things are done with the entries a peer names, and the third is not a drop:
//
//   - the caller's own id is dropped: a peer that names itself in its list has already told this
//     machine where it is (Self), and the list is about other machines;
//   - this machine's own id is dropped: a peer cannot introduce a machine to itself, and a claim to be
//     this node is the one thing that cannot be true (this machine is not a candidate for itself);
//   - anything older than the contact window is KEPT and marked (candidate.Stale): it gets one
//     connection instead of a retry schedule. The address may well still be good, so refusing to try
//     it would lose a real machine; retrying it forever on the strength of a stale claim would let a
//     peer own this machine's dial budget.
func gossipCandidates(payload gossipPayload, caller, self string, contactWindow time.Duration, log *logger.Logger) []candidate {
	if contactWindow <= 0 {
		// Nobody told this node how old is too old (config node.discovery.contact_ttl unset): the
		// default the discovery side ships with is the only honest answer.
		contactWindow = contactTTL
	}
	out := make([]candidate, 0, len(payload.Peers)+1)
	// Where the CALLER says it can be reached. Its identity is not a claim — the certificate proved it
	// on this very connection — but its address is, so it enters the queue like everything else and is
	// dialled once; the only difference is that this node already knows which identity to expect there.
	// This is how a machine that only ever CALLS becomes known to the one it calls.
	if payload.Self != nil && caller != self {
		if addrs := gossipAddrs(payload.Self.Addrs); len(addrs) > 0 {
			out = append(out, candidate{Identity: caller, Addrs: addrs})
		}
	}
	for _, entry := range payload.Peers {
		// A peer that names itself in its list is repeating what it already said above, and this
		// machine cannot be introduced to itself — a claim to be this node is the one claim that is
		// false by construction.
		if !IsNodeID(entry.ID) || entry.ID == caller || entry.ID == self {
			log.Debug(context.Background(), "node: ignoring a machine a peer named",
				"why", "not a node id, or the peer itself, or this machine", "id", entry.ID)
			continue
		}
		if !isGossipAddr(entry.Addr) {
			log.Debug(context.Background(), "node: ignoring an address a peer named",
				"why", "not an https address on a local network", "addr", entry.Addr, "id", entry.ID)
			continue
		}
		// Next is deliberately left at its zero value: a claim that has just arrived is due NOW. The
		// store stamps that zero with its own clock as it queues the claim (node/sightings.go feed),
		// so a second-hand claim takes the same place a fresh announcement does — due now, and still
		// subject to the tick (one connection per tick, whatever the network does).
		out = append(out, candidate{
			Identity: entry.ID,
			Addrs:    []string{entry.Addr},
			Stale:    entry.Age > int(contactWindow.Seconds()),
		})
	}
	return out
}

// gossipAddrs keeps the addresses out of a list a peer sent that this machine could even try, and
// stops at maxGossipAddrs: one entry is one candidate, and a candidate is a pass over this machine's
// dials rather than a list a peer decides the length of.
func gossipAddrs(addrs []string) []string {
	out := make([]string, 0, maxGossipAddrs)
	for _, addr := range addrs {
		if len(out) == maxGossipAddrs {
			break
		}
		if isGossipAddr(addr) {
			out = append(out, addr)
		}
	}
	return out
}

// isGossipAddr reports whether an address a peer named is one worth a connection: HTTPS, and somewhere
// on a local network.
//
// Both halves are borrowed rather than restated, because each is already the rule somewhere else: this
// program's peers are HTTPS and nothing else (node/peers.go checkPeerURL, node/peerdir.go dialAddrs),
// and an address has to be one this machine could plausibly reach (isLocalAddr, which is what an mDNS
// announcement's addresses go through too). A second spelling of either is how the two ends of
// discovery start disagreeing about what is dialable.
//
// It is checked at the DOOR and not only where the dial happens: an address that could never be
// dialled would otherwise sit in the candidate queue being retried for its whole window, spending this
// machine's connections on a peer's typo.
func isGossipAddr(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || u.Scheme != mdnsScheme {
		return false
	}
	return isLocalAddr(addr)
}

// gossipEntriesFor is the list this machine hands to ONE peer: what it vouches for, minus anything
// about the peer being told (it just reached this machine, so this machine has nothing to say about
// where it is) and minus anything about this machine itself (which cannot be true).
//
// Truncating is allowed here and refusing is not, and BOTH directions use this: a short list is
// incomplete, and nothing in this exchange promises completeness — the machines left out are ones this
// machine can name again next round. What must not happen is an over-long list ARRIVING: a peer's list
// is its whole claim about the fleet, so a request carrying more than maxGossipEntries is refused
// rather than quietly shortened (the handler), which is also why the sending side truncates its own.
func gossipEntriesFor(vouched []gossipEntry, except ...string) []gossipEntry {
	out := make([]gossipEntry, 0, len(vouched))
entries:
	for _, entry := range vouched {
		for _, id := range except {
			if id != "" && entry.ID == id {
				continue entries
			}
		}
		out = append(out, entry)
		if len(out) == maxGossipEntries {
			break
		}
	}
	return out
}

// ── The client half: one round, and what decides the next one ─────────────────
//
// A round is one POST to one neighbour (design §二「一次往返」): this machine's own account of where it
// is, plus the machines it has proved itself; the answer is the same shape back, and every entry in it
// goes into the same candidate queue an announcement goes into. None of it is on a READER's path — a
// round changes what the directory will say next time somebody reads it, and that is the whole of its
// effect: no reader waits, and nothing promises when.
//
// It runs for as long as the process does, and it runs quietly. "Quiet" is the only steady state this
// system has, and it is defined by the loop itself: a round that changes nothing moves nothing.

const (
	// gossipPeriod is the steady cadence: one round per this, asking gossipFanout neighbours.
	//
	// It is a heartbeat rather than a reaction, and it is what keeps a machine known only by hearsay
	// from falling out of anybody's table — a row lives discoveryTTL, and each round refreshes the
	// machines that answer. The arithmetic that has to hold (§四): period × pool / fanout < contactTTL,
	// or a neighbour in a large pool is never reached in time to be refreshed. Three machines × three
	// minutes / two ≈ 4.5 minutes against a ten-minute window; a pool that grows past about ten wants a
	// larger fanout or a longer contactTTL, so those two numbers are not independent of this one.
	gossipPeriod = 3 * time.Minute

	// gossipFanout is how many neighbours ONE round asks: two small requests, and two directions to
	// hear from.
	gossipFanout = 2

	// gossipNudge is when the next round moves to once something changed — a machine this one has just
	// proved, or its own addresses changing. It is not a broadcast: it is this machine asking somebody
	// sooner, and it is where convergence comes from (§四).
	gossipNudge = 10 * time.Second

	// gossipTimeout bounds ONE exchange, addresses and body together. It is short on purpose: a round
	// is a heartbeat, so it must not borrow the delegation budget, where one dead neighbour would hold
	// it for the whole request timeout.
	gossipTimeout = 5 * time.Second

	// gossipMute is how long a neighbour that failed a round is skipped. It is of the same order as the
	// card cache (cardTTL), for the same reason: a configured machine that is switched off would
	// otherwise cost one timeout every time its turn came round.
	gossipMute = 5 * time.Minute

	// gossipDebounce is the floor between two nudges. Changes arrive as a STRING rather than one at a
	// time — a round can bring back five machines, and they are proved one verification tick apart — so
	// without a floor that string would pin the cadence to the fast one and it would never settle.
	//
	// It is measured from the last nudge that was APPLIED, and that is what its size has to answer for:
	// it must still be running when the round a nudge brought forward has come and gone, or the machine
	// proved right after that round pulls the next one in again — and the cadence is the fast one after
	// all. That is why it is a multiple of gossipNudge rather than of the tick: the nudge is what the
	// round's arrival is scheduled by, and one tick (the interval the stages of a string are spread
	// over) would be over before the round had even happened.
	gossipDebounce = 2 * gossipNudge
)

// gossipSide is the process a loop runs in, as the loop needs to see it. It exists so that every rule
// below (who to ask, when to ask again, what counts as a change) can be driven by hand in a test: a
// round is timing and selection, and neither is about a network.
type gossipSide interface {
	// neighbours is the directory as the loop pools from it: one entry per machine this machine may
	// ask, with the addresses it would dial. Both kinds of member are there — a configured line, even
	// one never contacted (asking it is its first contact), and a machine this machine has proved
	// itself. A machine that arrived by hearsay is NOT here until its own handshake has happened,
	// which is the whole of the "rumours do not spread" rule read from the other side.
	neighbours() []peerEntry
	// exchange runs one round trip with one neighbour and answers with its reply.
	exchange(ctx context.Context, e peerEntry, payload []byte) ([]byte, error)
	// vouches is what this machine may say about others: the machines it has proved itself, recently
	// enough to stand behind (node/sightings.go contactWindow).
	vouches() []gossipEntry
	// receive is where an answer goes: the same candidate queue an announcement goes into, where every
	// entry costs one connection before it can become anything.
	receive(cands []candidate)
}

// gossipLoop asks neighbours who else they know. One per process, held by whoever holds the sending
// side (node/delegatorcache.go) and stopped with it.
type gossipLoop struct {
	side gossipSide
	log  *logger.Logger

	// self is this machine's own node id — never ask it, never name it — and selfAddrs is where it says
	// it can be reached (config `node.public_url`). Those addresses are a claim like every other: the
	// peer dials them before believing anything, which is what lets a machine that only ever CALLS
	// become known to the one it calls.
	self      string
	selfAddrs []string

	// contactWindow is how old a peer's claim about a machine may be before it is worth one connection
	// instead of a retry schedule — this machine's own window (config contact_ttl), because the
	// question is asked of this machine's clock.
	contactWindow time.Duration

	// The numbers above, as fields: a test runs the same logic at millisecond scale, and nothing else
	// varies them. fanout is a count, which is why it is not with the durations.
	period, nudge, mute, debounce time.Duration
	fanout                        int
	// clock is the loop's only time source: it stamps what a neighbour contributed and decides when the
	// next round is due.
	clock func() time.Time
	// jitter spreads a delay out, so that two machines do not ask each other in lockstep forever. It is
	// injected because the timing it produces is what a test asserts about.
	jitter func(time.Duration) time.Duration

	// The tables below are the loop's own state, and only its goroutine touches them (Start runs it
	// once, and step is what that goroutine calls).
	//
	// asked is when each neighbour was last asked, keyed the way the directory keys a machine
	// (peerEntry.key): its identity once one is proved, its address before that.
	asked map[string]time.Time
	// contributed is how many machines a neighbour's last answer taught this one. It is the whole of
	// "who sees something I cannot" (§四): no prior about skills or subnets, only results — a neighbour
	// that has produced new machines is the one that might again.
	contributed map[string]int
	// mutedUntil is when a neighbour whose round failed may be asked again.
	mutedUntil map[string]time.Time
	// content is the directory's CONTENT as of the last look: one signature per machine, its identity
	// and the SET of its addresses, with nothing about time in it (§四 稳态 — an age changes every
	// round and would make every round a change). Only this can say "nothing changed".
	content map[string]bool
	// next is when the next round is due, and lastNudge when one was last brought forward. A zero next
	// means "due now", which is what makes the startup round the first tick.
	next, lastNudge time.Time

	// The loop's lifetime, set by Start. done is NIL until then, which is what makes Close safe on a
	// loop that never started.
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// newGossipLoop returns a loop over one process's sending side, with the shipped numbers and the wall
// clock. It is not running: Start is what launches it.
func newGossipLoop(side gossipSide, self string, selfAddrs []string, contactWindow time.Duration, log *logger.Logger) *gossipLoop {
	if log == nil {
		log = logger.New("node")
	}
	if contactWindow <= 0 {
		contactWindow = contactTTL
	}
	return &gossipLoop{
		side:          side,
		log:           log,
		self:          self,
		selfAddrs:     append([]string(nil), selfAddrs...),
		contactWindow: contactWindow,
		period:        gossipPeriod,
		fanout:        gossipFanout,
		nudge:         gossipNudge,
		mute:          gossipMute,
		debounce:      gossipDebounce,
		clock:         time.Now,
		jitter:        defaultGossipJitter,
		asked:         make(map[string]time.Time),
		contributed:   make(map[string]int),
		mutedUntil:    make(map[string]time.Time),
	}
}

// Start runs the loop until the context is canceled or Close is called. Called once, by whoever
// brought the sending side up (node/delegatorcache.go startDiscovery).
//
// The FIRST thing that happens is a tick, and that is deliberate rather than incidental: this is
// started from inside the call that builds the sending side, so a round that ran any sooner would find
// no neighbours and push the startup round a whole period away.
func (l *gossipLoop) Start(parent context.Context) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	l.cancel, l.done = cancel, make(chan struct{})
	done := l.done
	go l.run(ctx, done)
}

// run ticks the watcher. Its tick is the verification loop's, because that is the rate at which the
// directory can change under it: a machine a round brought back is proved one tick at a time, and a
// slower watcher would see them arrive in batches that look like one change.
//
// A round inside a tick is synchronous, so a neighbour that is slow to answer delays the next look by
// up to the exchange budget. That is the trade the design asks for (one round at a time, whatever the
// network does) — and the budget is a couple of seconds because of it.
func (l *gossipLoop) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(discoveryTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.step(ctx, l.clock())
		}
	}
}

// Close stops the loop. It is safe without Start and safe twice — the same promise the store and the
// browser make, because an entry point calls it on its way out and nothing here may make that wait.
func (l *gossipLoop) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.mu.Lock()
		cancel, done := l.cancel, l.done
		l.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
	})
}

// step is one turn of the watcher: notice what changed, and ask if a round is due.
//
// The directory is read ONCE here and that one reading answers every question below — what changed,
// who may be asked, what this machine already knows. It is one moment, so it is one read: rebuilding it
// per question would be three readings of the same instant, and this runs every couple of seconds for
// as long as the process does (node/peerdir.go build explains why a reading is cheap, not why it is
// free).
func (l *gossipLoop) step(ctx context.Context, now time.Time) {
	pool := l.side.neighbours()
	l.observe(now, pool)
	if now.Before(l.next) {
		return
	}
	l.round(ctx, now, pool)
	// A round leaves the cadence where it was: what it brought back is not known to be new until the
	// queue behind it has proved it, and observe is what sees that happen — possibly several ticks
	// later, one machine at a time.
	l.next = now.Add(l.jitter(l.period))
}

// observe looks at the directory and pulls the next round in when its content changed.
//
// The criterion is the CONTENT — a machine this one did not have, or an address one of them did not —
// and it is deliberately not "somebody answered", which happens every round, nor anything carrying a
// timestamp, which changes without anything happening (§四 稳态).
//
// The design's other trigger, this machine's own addresses changing, has nothing to look at here: they
// come from config and are read once, when the loop is built (node/delegatorcache.go), so within one
// loop's life they cannot move.
func (l *gossipLoop) observe(now time.Time, pool []peerEntry) {
	here := contentOf(pool)
	if l.content == nil {
		// The first look is the state this machine is starting from, not a change: without this, a
		// process that already knows a fleet would nudge itself the moment it came up.
		l.content = here
		return
	}
	if sameContent(l.content, here) {
		return
	}
	l.content = here
	if now.Sub(l.lastNudge) < l.debounce {
		// A string of arrivals, one verification tick apart — and the round the first of them brought
		// forward has either not happened yet or has only just happened, which is what the floor is
		// sized for. The next round already exists; pulling it in again for every machine in the string
		// is what would pin the cadence to the fast one. Note this is a floor on the NUDGE and not on
		// the round: the first arrival is what brings the round forward, and a later one could only
		// ever move it later (the line below keeps the EARLIER of the two).
		return
	}
	if when := now.Add(l.jitter(l.nudge)); when.Before(l.next) {
		l.next = when
	}
	l.lastNudge = now
}

// round asks this round's neighbours, one after another. pool is the reading step took.
func (l *gossipLoop) round(ctx context.Context, now time.Time, pool []peerEntry) {
	l.forgetGone(pool)
	// What this machine already knows, as of the START of the round: what a neighbour taught it is
	// measured against the directory as it stood when the round began. Nothing inside a round can move
	// that set — every entry an answer brings is a candidate, and a candidate is proved by the queue
	// behind this loop rather than here — so one reading for the round is also the reading at each
	// exchange.
	known := identitiesOf(pool)
	for _, e := range l.pick(pool, now) {
		if ctx.Err() != nil {
			return
		}
		l.ask(ctx, e, now, known)
	}
}

// forgetGone drops what this loop remembers about machines the directory no longer holds.
//
// A row expires when its machine goes quiet (sightings, discoveryTTL), and three tables keyed by every
// machine this process has ever asked would otherwise be the one thing here that only grows. What a
// departed machine loses with its row is its place in the rotation and its mute — the second is
// deliberate: a machine that comes BACK is one somebody has just proved, so it should be asked, not
// skipped on the strength of a failure from before it left.
func (l *gossipLoop) forgetGone(pool []peerEntry) {
	if len(l.asked) == 0 && len(l.contributed) == 0 && len(l.mutedUntil) == 0 {
		return
	}
	here := make(map[string]bool, len(pool))
	for _, e := range pool {
		here[e.key()] = true
	}
	for key := range l.asked {
		if !here[key] {
			delete(l.asked, key)
		}
	}
	for key := range l.contributed {
		if !here[key] {
			delete(l.contributed, key)
		}
	}
	for key := range l.mutedUntil {
		if !here[key] {
			delete(l.mutedUntil, key)
		}
	}
}

// pick is who to ask this round: at most fanout neighbours from the pool, least recently asked first,
// and never one that is muted.
//
// Round-robin is the backbone (§四) and the other two rules only order it: a machine never asked comes
// first (the zero time sorts before every real one), and among machines asked at the same moment, the
// one that brought something last time — the neighbour whose view differs from this one's. A pool
// smaller than the fanout is asked whole, and nothing is repeated within a round.
func (l *gossipLoop) pick(pool []peerEntry, now time.Time) []peerEntry {
	type choice struct {
		e      peerEntry
		key    string
		asked  time.Time
		helped int
	}
	cands := make([]choice, 0, len(pool))
	for _, e := range pool {
		key := e.key()
		if e.Identity != "" && e.Identity == l.self {
			continue // asking this machine would be telling it about itself
		}
		if len(e.dialAddrs()) == 0 {
			continue // no address that can prove an identity: nothing to ask
		}
		if until, ok := l.mutedUntil[key]; ok && until.After(now) {
			continue
		}
		cands = append(cands, choice{e: e, key: key, asked: l.asked[key], helped: l.contributed[key]})
	}
	slices.SortStableFunc(cands, func(a, b choice) int {
		if c := a.asked.Compare(b.asked); c != 0 {
			return c
		}
		if a.helped != b.helped {
			return b.helped - a.helped
		}
		// Key last, so that the order is a property of the pool rather than of the iteration order of a
		// map nobody here controls.
		return strings.Compare(a.key, b.key)
	})
	out := make([]peerEntry, 0, len(cands))
	for _, c := range cands {
		if len(out) >= l.fanout {
			break
		}
		out = append(out, c.e)
	}
	return out
}

// ask is one exchange with one neighbour, and everything that follows from it.
//
// Every outcome is a fact about THIS MOMENT: a success clears a mute and records what it taught this
// machine, a failure mutes the neighbour for gossipMute so that the next turn does not spend another
// timeout on it. Neither is an error anybody has to handle — a round that fails writes nothing else,
// and the next one happens anyway (§四: 一轮失败什么都不做).
//
// known is the identities the directory held when the ROUND started (round reads it once): what a
// neighbour taught this machine is a claim this one had not heard of.
func (l *gossipLoop) ask(ctx context.Context, e peerEntry, now time.Time, known map[string]bool) {
	key := e.key()
	payload, err := json.Marshal(gossipPayload{
		Self:  l.selfPayload(),
		Peers: gossipEntriesFor(l.side.vouches(), e.Identity, l.self),
	})
	if err != nil {
		// Nothing on the wire can be built out of what this machine knows. That is not about the
		// neighbour, so it does not mute it.
		l.log.Warn(ctx, "node: could not build a gossip exchange", "cause", err)
		return
	}
	raw, err := l.side.exchange(ctx, e, payload)
	if err != nil {
		l.mutedUntil[key] = now.Add(l.mute)
		l.log.Debug(ctx, "node: a gossip round failed; that neighbour is skipped for a while",
			"peer", e.Display(), "cause", err)
		return
	}
	delete(l.mutedUntil, key)
	l.asked[key] = now
	// The exchange itself — the handshake that proved who answered — was recorded as a contact on the
	// way here (node/remoteclient.go contact, node/sightings.go touch), which is what keeps this
	// neighbour fresh enough to keep vouching for.

	var answer gossipPayload
	if err := json.Unmarshal(raw, &answer); err != nil {
		// The exchange happened and the answer is not readable: a peer of another vintage, or something
		// else on that port. Worth a line, and the neighbour is not muted — it answered.
		l.log.Debug(ctx, "node: a gossip answer could not be read", "peer", e.Display(), "cause", err)
		return
	}
	// What it taught this machine, measured against what the directory held BEFORE the answer was
	// handed over: a claim this machine had not heard of. Whether the dial behind that claim succeeds
	// is the queue's business, not this measure's.
	novel := 0
	for _, entry := range answer.Peers {
		if !known[entry.ID] {
			novel++
		}
	}
	l.contributed[key] = novel

	cands := gossipCandidates(answer, e.Identity, l.self, l.contactWindow, l.log)
	if len(cands) > 0 {
		l.log.Debug(ctx, "node: a gossip answer brought machines to try", "peer", e.Display(),
			"entries", len(answer.Peers), "new", novel)
	}
	l.side.receive(cands)
}

// selfPayload is this machine's own account of where it is, or nil when it has none (a process that
// hosts no node service, or one whose operator never said).
func (l *gossipLoop) selfPayload() *gossipSelf {
	if len(l.selfAddrs) == 0 {
		return nil
	}
	return &gossipSelf{Addrs: append([]string(nil), l.selfAddrs...)}
}

// contentOf is the content of ONE reading of the directory: one signature per machine (contentKey).
func contentOf(entries []peerEntry) map[string]bool {
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[contentKey(e)] = true
	}
	return out
}

// identitiesOf is the set of identities ONE reading of the directory holds: what "a machine this one
// did not have" is measured against. A configured line nobody has contacted yet has no identity to
// contribute — the loop's pool holds it, but there is nothing to compare.
func identitiesOf(entries []peerEntry) map[string]bool {
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Identity != "" {
			out[e.Identity] = true
		}
	}
	return out
}

// contentKey is one machine's contribution to the content signature: WHO it is and the SET of
// addresses this machine could dial — as a set, so that the same addresses in another order are the
// same content (the order says who spoke last, and changes on every announcement), and with no time
// in it at all (an age changes without anything happening).
func contentKey(e peerEntry) string {
	addrs := append([]string(nil), e.dialAddrs()...)
	slices.Sort(addrs)
	return e.key() + "\x00" + strings.Join(addrs, "\x01")
}

// sameContent reports whether two content signatures are the same set of machines.
func sameContent(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// defaultGossipJitter moves a delay by up to a tenth of itself in either direction, so that machines
// that came up together do not keep asking each other in lockstep. It is a jitter and not a strategy:
// it cannot make a round happen sooner than the change that asked for it.
func defaultGossipJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d / 10)
	return d - time.Duration(spread) + time.Duration(rand.Int63n(2*spread+1))
}
