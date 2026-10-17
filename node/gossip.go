package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
			answer = gossipAnswer(here.vouches(), caller, self)
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

// gossipAnswer is what this node says when asked: the machines it vouches for, minus the two that
// would be answers about the asker or about this node itself.
//
// Truncating is allowed here and refusing is not, which is the asymmetry worth knowing: a short answer
// is incomplete, and nothing in this exchange promises completeness (the machines left out are ones
// this node can name again later). A short REQUEST would be a different thing — the peer's list is its
// whole claim, so that one is refused instead (maxGossipEntries).
func gossipAnswer(vouched []gossipEntry, caller, self string) []gossipEntry {
	out := make([]gossipEntry, 0, len(vouched))
	for _, entry := range vouched {
		if entry.ID == caller || entry.ID == self {
			continue
		}
		out = append(out, entry)
		if len(out) == maxGossipEntries {
			break
		}
	}
	return out
}
