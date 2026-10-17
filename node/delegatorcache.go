package node

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// DelegatorCache is this machine's sending side, kept up to date against the live config.
//
// Every entry point needs exactly the same thing, and none of the three obvious ways of getting
// it is right on its own:
//
//   - building it at startup freezes the peer list, which is a config leaf the reader can edit
//     while the process runs (the `peers` settings row is applyLazy — it takes effect on the
//     next turn);
//   - building it per turn re-scans the delegated-task records (a directory read) on every
//     turn of every conversation, including the ones that never delegate;
//   - keeping it in a process global makes tests fight each other and hides the dependency.
//
// So: one cache per entry point, keyed by the peer list itself. The caller holds it as a field
// and asks for the delegator on the turn it is about to run.
//
// The key is the CONFIGURED peers — name, address and note — because that is everything that
// decides which nodes exist and where they are. A note change rebuilds too: it is what the
// reminder says about an unreachable node, and a stale description is worse than a rebuild.
type DelegatorCache struct {
	mu      sync.Mutex
	current *Sender
	key     string
	log     *logger.Logger
	// ownName is what this machine calls itself, sent with every delegation so a peer's records
	// can say who asked (node.name).
	ownName string
	// discovery is how this machine FINDS nodes on its own, and it is held HERE rather than in a
	// sender because a sender is rebuilt whenever the peer list changes while the network underneath
	// it is the same network: one browser per process, one UDP socket, one set of sightings.
	discovery Discovery
	// store is what every source of discovery feeds — the candidate queue, the confirmed set and the
	// verification loop (node/sightings.go). It is held here, beside the sources, because it outlives
	// each of them: a browser comes and goes with the sending side, and the store is what the
	// directory reads (node/sender.go OpenSender hands it over) and what a second source will feed.
	// Nil while discovery is off, which is the default.
	store *sightingsStore
	// browser is that browser, started the first time anyone asks for a sender. Nil while discovery
	// is off, which is the default.
	browser *mdnsBrowser
	// closed is set by Close and read under mu, and it is what makes Close terminal: a sender asked
	// for after it must come back nil rather than start a second browser (the cache is not rebuilt by
	// then — an entry point is on its way out, and a goroutine still winding down may still ask).
	closed bool
	// onSighting is called when the browser VERIFIES a machine the directory did not have, and it
	// belongs to the entry point rather than to this cache: the cache holds the network, and the entry
	// point is what holds the views somebody is looking at. Nil is the common case — three of the four
	// entry points have no such view.
	onSighting func()
	openFunc   func(peers []config.PeerConfig, ownName string, log *logger.Logger, store *sightingsStore) (*Sender, error)
}

// DiscoveryOf is the discovery settings a config asks for, in the sending side's own terms: the one
// place the two spellings of "on, on these interfaces" meet, so no entry point has to remember which
// key maps to which field.
func DiscoveryOf(cfg *config.Config) Discovery {
	if cfg == nil {
		return Discovery{}
	}
	m := cfg.Node.Discovery.Mdns
	return Discovery{
		Enabled:    m.Enabled,
		Interfaces: append([]string(nil), m.Interfaces...),
		Gossip:     cfg.Node.Discovery.Gossip.Enabled,
		TTL:        cfg.Node.Discovery.TTL,
		ContactTTL: cfg.Node.Discovery.ContactTTL,
	}
}

// Discovery is how this machine looks for nodes on its own (config `node.discovery`): multicast on its
// own network, and — separately — asking peers it already knows who else they can vouch for.
//
// It is a plain value rather than the config struct so that a cache does not have to be handed a
// Config, and so that "off" is the zero value: every existing caller gets the behaviour it had. The
// two sources are separate switches and one store: either may feed it, and what it has proved is one
// answer either way (node/sightings.go).
type Discovery struct {
	Enabled    bool
	Interfaces []string
	// Gossip is the hearsay half (config `node.discovery.gossip.enabled`).
	Gossip bool
	// TTL and ContactTTL are the store's two windows (config `node.discovery.ttl` / `contact_ttl`).
	// Zero means "whatever the store shipped with", which is what an unconfigured process and a
	// hand-built Config both get.
	TTL        time.Duration
	ContactTTL time.Duration
}

// any reports whether either source is on, which is what decides whether the discovery side exists at
// all.
func (d Discovery) any() bool { return d.Enabled || d.Gossip }

// NewDelegatorCache returns a cache that builds senders with OpenSender, and — when discovery is on
// — browses for nodes as those senders come and go.
//
// onSighting, when it is not nil, is called (from the browser's own goroutine) every time discovery
// verifies a machine the directory did not have: a row that has appeared, and the one moment a reader
// who is already looking needs to be told (see mdnsBrowser.onSighting).
func NewDelegatorCache(ownName string, d Discovery, log *logger.Logger, onSighting func()) *DelegatorCache {
	c := &DelegatorCache{ownName: strings.TrimSpace(ownName), discovery: d, log: log, onSighting: onSighting, openFunc: OpenSender}
	// Registered here rather than where discovery starts: the gossip route is answered by the NODE
	// SERVICE, which is built from config and has no way to reach this object — and an answer from a
	// process whose sending side has not come up yet is legitimately "I know nobody" rather than an
	// error (node/gossip.go currentMesh).
	registerMesh(c)
	return c
}

// Close stops discovery, and it is TERMINAL: nothing asked of this cache afterwards starts a browser
// or hands out a sender. It is what an entry point calls on its way out, because the browser holds
// UDP sockets and a goroutine that outlive every sender — and because the senders already handed out
// belong to turns that are winding down, this one is called with no lock held on the caller's side.
func (c *DelegatorCache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	// The switch goes off with it, so that the two ways of asking "may discovery run" agree: the flag
	// is what stops a sender being built, this is what makes the value itself true.
	c.discovery.Enabled = false
	browser, store := c.browser, c.store
	c.browser, c.store = nil, nil
	c.current = nil
	c.mu.Unlock()
	// The producer first, then what it fed. Neither wait is on anything another package owns, so this
	// is bounded by one in-flight connection at worst.
	if browser != nil {
		browser.Close()
	}
	if store != nil {
		store.Close()
	}
	// Last, and it says which mesh it is unregistering: a process that closed one cache and opened
	// another must not lose the second one's registration here.
	clearMesh(c)
}

// vouches is what this process may say about other machines: the ones it has proved ITSELF, recently
// enough to stand behind, each with the address that answered (node/gossip.go mesh).
//
// "Recently enough" is contactTTL, and it is shorter than the hour a row lives in the directory, for
// the same reason the two numbers are different: what this machine will DIAL is its own business,
// while what it will say about another machine to a third one is a statement it should be able to
// stand behind.
//
// A sighting with no address recorded is skipped rather than guessed at — there is nothing to tell
// anyone — which also means a machine this process has heard about and never reached is never repeated.
func (c *DelegatorCache) vouches() []gossipEntry {
	if c == nil {
		return nil
	}
	// Read under the lock, then let it go: the store has a lock of its own, and this codebase's rule
	// is one at a time.
	c.mu.Lock()
	store := c.store
	c.mu.Unlock()
	if store == nil {
		return nil
	}
	recent := store.recent(store.contactWindow())
	now := store.clock()
	out := make([]gossipEntry, 0, len(recent))
	for _, seen := range recent {
		if seen.Served == "" {
			continue
		}
		out = append(out, gossipEntry{
			ID:   seen.Identity,
			Addr: seen.Served,
			Age:  int(now.Sub(seen.Seen).Seconds()),
		})
	}
	return out
}

// receive takes what a peer said and hands it to the same queue an announcement goes into: candidates,
// each of which costs one connection before it can become a sighting (node/sightings.go).
//
// It drops everything when this process has no store — a machine that asked this node who it knows
// cannot be answered, so there is nothing here to learn from either. That is the "no discovery side"
// case, not an error: a node service can be hosted by a process that never opened a sending side.
func (c *DelegatorCache) receive(cands []candidate) {
	if c == nil || len(cands) == 0 {
		return
	}
	c.mu.Lock()
	store := c.store
	c.mu.Unlock()
	if store == nil {
		return
	}
	for _, cand := range cands {
		store.feed(cand)
	}
}

// startDiscovery brings the discovery side up the first time it is needed — the store every source
// feeds (node/sightings.go), and with mDNS on, the browser that is its first source. Called with
// c.mu held, and safe to call when it is already up.
//
// Discovery that cannot start is NOT an error the caller has to handle: the deployment it fails in
// (no multicast-capable interface, the port taken by the system responder) is a real one, and the
// answer to it is the state before discovery existed — this machine has to be told about peers. It
// is said once in the log, and discovery stays off for this process.
func (c *DelegatorCache) startDiscovery(ctx context.Context) {
	if c.store != nil || !c.discovery.any() {
		return
	}
	// This machine's own identity, read HERE rather than by the caller: it is a disk read (and a key
	// and certificate the first time), it is only needed by discovery that is actually going to run,
	// and the sender built below reads the same file anyway. An identity that cannot be read turns
	// discovery off (selfID) — a browser that cannot tell its own announcements from a stranger's is
	// not a browser this machine should run.
	self := c.selfID()
	if self == "" {
		return
	}
	store := newSightingsStore(c.log)
	store.setWindows(c.discovery.TTL, c.discovery.ContactTTL)
	// The hook is handed over BEFORE anything starts: a sighting is what it is about, and a loop that
	// was already running could verify one with nowhere to say so.
	store.onSighting = c.onSighting

	// The multicast half, when it is on. A machine that cannot browse is not a machine that cannot
	// discover — what fails here is about interfaces and ports, and the hearsay half needs neither —
	// so this failure turns off THIS source and says so once, and leaves the rest of discovery alone.
	if c.discovery.Enabled {
		browser, err := newMDNSBrowser(self, config.MdnsDiscoveryConfig{
			Enabled:    c.discovery.Enabled,
			Interfaces: c.discovery.Interfaces,
		}, c.log, store)
		if err != nil {
			if c.log != nil {
				c.log.Warn(ctx, "node: mDNS discovery is on but cannot start; this machine has to be "+
					"told about peers on its network, or find them by gossip", "cause", err)
			}
			c.discovery.Enabled = false
		} else {
			browser.Start(context.WithoutCancel(ctx))
			c.browser = browser
		}
	}

	if c.browser != nil || c.discovery.Gossip {
		// The store comes up when it has a source: the browser that just started, or gossip — which
		// feeds it from the other direction, since a peer's answer arrives at the node service rather
		// than here (node/gossip.go, DelegatorCache.receive).
		store.Start(context.WithoutCancel(ctx))
		c.store = store
	}
}

// For returns the sending side for this peer list, as the TOOL layer sees it, rebuilding it when the
// list changed.
//
// It returns nil — meaning "this process cannot delegate" — in three cases, and all are states
// the entry points should simply live with rather than report per turn:
//
//   - no peers are configured: there is nothing to delegate to, and `RemoteTask` on its own
//     could only show records this machine can no longer reach;
//   - the records directory cannot be opened: a broken disk must not cost the user their local
//     chat (the entry points log it), and the next turn tries again rather than caching the
//     failure, so a transient problem heals itself;
//   - the cache has been closed: an entry point on its way out asks no more, and a sender built
//     then would start a browser (Close).
//
// It returns a genuinely nil INTERFACE when there is nothing: returning a typed nil *Sender would
// make `if d == nil` false at every call site, which is how a "no peers" machine starts to look like
// an available one.
func (c *DelegatorCache) For(ctx context.Context, peers []config.PeerConfig) tools.RemoteDelegator {
	if sender := c.ForSender(ctx, peers); sender != nil {
		return sender
	}
	return nil
}

// ForSender is For with the concrete type kept: the node picker and the node board need more than
// the tool-layer contract — the address this machine dials, and each peer's card — and that contract
// is deliberately blind to both (see the comment on RemoteDelegator). Callers that only delegate
// should ask For instead.
func (c *DelegatorCache) ForSender(ctx context.Context, peers []config.PeerConfig) *Sender {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Closed is the end of it: an entry point is on its way out, and a sender built now would start a
	// second browser (node/delegatorcache.go Close).
	if c.closed {
		return nil
	}
	// With no configured peers there is still work to do when discovery is on: the whole point of it
	// is a node that has no config line.
	//
	// Read under the lock, like every other read of discovery: startDiscovery and selfID WRITE
	// discovery when a source cannot come up, and the entry points ask this from several goroutines (a
	// turn, the picker, a background probe).
	if len(peers) == 0 && !c.discovery.any() {
		return nil
	}
	key := peerSignature(peers) + "\x02" + discoverySignature(c.discovery)
	if c.current != nil && c.key == key {
		return c.current
	}

	c.startDiscovery(ctx)
	open := c.openFunc
	if open == nil {
		open = OpenSender
	}
	sender, err := open(peers, c.ownName, c.log, c.store)
	if err != nil {
		if c.log != nil {
			c.log.Warn(context.Background(), "delegation is unavailable this turn", "cause", err)
		}
		return nil
	}
	c.current, c.key = sender, key
	return sender
}

// WarmUp asks every node this machine knows who it is, in the background, and returns at once.
//
// It exists because the first thing that wants a peer's IDENTITY is something a reader is waiting
// on: the delegation reminder reads the directory on the very first turn, and the node picker draws
// its rows. An identity only exists once a handshake has happened — and reading the card is the
// cheapest request that proves one — so the offer is to pay for that while the window is still
// drawing, rather than making the first '#' or the first turn wait for a round trip per node.
//
// What it warms is the sending side's own state: the pins written for the peers that answer (so a
// config line can name its identity, and the reminder speaks in identities), the card cache (so the
// first read is a read and not a request), and — with discovery on — the browser, which ForSender is
// what starts. A node that does not answer is remembered as not answering for the same cardTTL, so
// warming a dead peer cannot become a per-turn cost either.
//
// The entry points that have a picker do the same round through the list they already own, because
// it is the same round: node.PeerList.Probe is what fills their rows, and those rows are read
// through the sending side this cache hands out. This one is for the entry point that has no list.
//
// NOTHING waits for it, and that is the point: startup must not be held by a peer that is slow,
// absent, or on a network that no longer exists. A round that fails is therefore silent — the first
// turn reports the same failure, with a reader in front of it.
func (c *DelegatorCache) WarmUp(ctx context.Context, peers []config.PeerConfig) {
	if c == nil {
		return
	}
	go func() {
		// Bounded, so that a peer which never answers cannot outlive the startup it belongs to; the
		// caller's context still ends it sooner (that process is going away, and so is this round).
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		sender := c.ForSender(ctx, peers)
		if sender == nil {
			// Nothing to warm: no peers and discovery off, or this cache is already closed.
			return
		}
		reports := sender.PeerReports(ctx, false)
		if c.log != nil {
			c.log.Debug(ctx, "node: asked every node who it is before a turn needed the answer",
				"peers", len(reports), "reachable", countReachable(reports))
		}
	}()
}

// selfID is this machine's own node id, loaded (or created) so that a browser can recognise its own
// announcements. It is called by startBrowser and nothing else, so the disk read happens only when a
// browser is actually going to run. It is the same identity the sending side uses — the one file,
// read twice — and a failure here is not fatal: discovery stays off rather than running without
// knowing what "not me" means.
func (c *DelegatorCache) selfID() string {
	identity, err := LoadOrCreateIdentity(config.A2ADir())
	if err != nil {
		if c.log != nil {
			c.log.Warn(context.Background(), "node: cannot read this machine's identity; discovery off",
				"cause", err)
		}
		c.discovery.Enabled = false
		return ""
	}
	return identity.NodeID
}

// discoverySignature is the part of a rebuild key that belongs to discovery: both switches, and which
// interfaces the multicast half browses on. Gossip belongs in it because the mesh the route answers
// from IS this object (node/gossip.go): a sender built while gossip was off is a different sending
// side from one built while it was on.
func discoverySignature(d Discovery) string {
	mdns := "off"
	if d.Enabled {
		mdns = "on:" + strings.Join(d.Interfaces, ",")
	}
	gossip := "off"
	if d.Gossip {
		gossip = "on"
	}
	return mdns + "\x03" + gossip
}

// peerSignature is what a rebuild is keyed by.
func peerSignature(peers []config.PeerConfig) string {
	parts := make([]string, 0, len(peers))
	for _, p := range peers {
		// A separator that cannot occur in a name or a URL, so two different lists cannot
		// produce one signature by running into each other.
		parts = append(parts, strings.Join([]string{p.Name, p.URL, p.Note}, "\x00"))
	}
	return strings.Join(parts, "\x01")
}
