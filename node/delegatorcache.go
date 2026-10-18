package node

import (
	"context"
	"errors"
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
	// gossip is the hearsay half's loop (node/gossip.go), started beside the store and stopped with
	// it. Nil while gossip is off, which is the default.
	gossip *gossipLoop
	// closed is set by Close and read under mu, and it is what makes Close terminal: a sender asked
	// for after it must come back nil rather than start a second browser (the cache is not rebuilt by
	// then — an entry point is on its way out, and a goroutine still winding down may still ask).
	closed bool
	// onSighting is called when the browser VERIFIES a machine the directory did not have, and it
	// belongs to the entry point rather than to this cache: the cache holds the network, and the entry
	// point is what holds the views somebody is looking at. Nil is the common case — three of the four
	// entry points have no such view.
	onSighting func()
	// sessionsDir is where this entry point keeps the conversations a delegated call belongs to: a
	// file a peer sends back is written beside the session's own records (node/remoteclient.go
	// storeFile). It travels with the cache because the SENDER is built per entry point, and each
	// one knows its own store root — a process with no conversations (`-p`) passes "".
	sessionsDir string
	openFunc    func(peers []config.PeerConfig, ownName string, log *logger.Logger, store *sightingsStore) (*Sender, error)
}

// openSender is the default openFunc: this cache's own sending side, carrying the session root this
// entry point works in.
func (c *DelegatorCache) openSender(peers []config.PeerConfig, ownName string, log *logger.Logger, store *sightingsStore) (*Sender, error) {
	return OpenSender(peers, ownName, log, store, c.sessionsDir)
}

// DiscoveryOf is the discovery settings a config asks for, in the sending side's own terms: the one
// place the two spellings of "on, on these interfaces" meet, so no entry point has to remember which
// key maps to which field.
func DiscoveryOf(cfg *config.Config) Discovery {
	if cfg == nil {
		return Discovery{}
	}
	m := cfg.Node.Discovery.Mdns
	g := cfg.Node.Discovery.Gossip
	d := Discovery{
		Enabled:    m.Enabled,
		Interfaces: append([]string(nil), m.Interfaces...),
		Gossip:     g.Enabled,
		// The loop's own numbers, verbatim: zero means "whatever node/gossip.go ships with", and the
		// loop is what decides that (gossipTuning.orDefault) — the same arrangement as the two windows
		// below, so a default lives in exactly one place.
		GossipTuning: gossipTuning{
			Period:  g.Period,
			Fanout:  g.Fanout,
			Nudge:   g.Nudge,
			Timeout: g.Timeout,
			Mute:    g.Mute,
		},
		TTL:        cfg.Node.Discovery.TTL,
		ContactTTL: cfg.Node.Discovery.ContactTTL,
	}
	// Where this machine says it can be reached, when the operator said. This is the ONE mapping of
	// that key into the sending side (the node service reads the same key for its own Card,
	// node/config.go), and it stays empty rather than guessed: the TCP source address a peer sees is an
	// exit or a temporary port, never an A2A listening address (node/gossip.go). Only an address a peer
	// could actually dial reaches the loop (gossipSelfAddrs), and `listen` is deliberately not a second
	// source: a derived address can be the loopback one, which would name the machine that is being
	// told about it.
	if url := strings.TrimSpace(cfg.Node.PublicURL); url != "" {
		d.SelfAddrs = []string{url}
	}
	return d
}

// gossipSelfAddrs keeps the addresses worth putting in `self.addrs`: the ones a peer could even try
// to dial (node/gossip.go isGossipAddr — https, and an address on a private, link-local or CGNAT
// network).
//
// The check is the RECEIVING side's own, applied before the claim goes out, because the alternative is
// sending something the far end silently drops: `public_url` in the shape config.go's own example
// shows it (`https://nas.tail-scale.ts.net:7080`, a name rather than an address) would be a switch
// that looks on and introduces this machine to nobody. What is dropped is said out loud where the loop
// is built (startDiscovery) rather than here, because this is a pure function and the operator needs
// to hear about it once.
func gossipSelfAddrs(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if isGossipAddr(addr) {
			out = append(out, addr)
		}
	}
	return out
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
	// GossipTuning is that half's own numbers (config `node.discovery.gossip.period` / `fanout` /
	// `nudge` / `timeout` / `mute`), carried as one value so that turning one of them into a field on
	// this struct does not mean turning all five into fields. Zero means the built-in default
	// (node/gossip.go), which is what an unconfigured deployment and a hand-built Discovery both get.
	GossipTuning gossipTuning
	// SelfAddrs is where this machine says it can be reached, sent as `self.addrs` with every gossip
	// exchange (config `node.public_url`, and DiscoveryOf is the only place that mapping happens).
	//
	// One ADDRESS in a list rather than one string, because that is the shape addresses have everywhere
	// else here (a peer entry, a card, an announcement), and a receiver merges what it is given into an
	// address set either way. Empty is normal: a process that hosts no node service has no address to
	// name, and the field is optional on the wire (node/gossip.go selfPayload).
	SelfAddrs []string
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
//
// sessionsDir is where this entry point keeps its conversations — the root a peer's returned files
// are written under (see the field).
func NewDelegatorCache(ownName string, d Discovery, log *logger.Logger, onSighting func(), sessionsDir string) *DelegatorCache {
	c := &DelegatorCache{ownName: strings.TrimSpace(ownName), discovery: d, log: log, onSighting: onSighting, sessionsDir: sessionsDir}
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
	// Every switch goes off with it, so that the two ways of asking "may discovery run" agree (the
	// value's own account, and `any`): the flag is what stops a sending side being built, and this is
	// what makes the value itself true. BOTH halves, because gossip is one of the two ways discovery
	// can be on, and leaving it true would say a closed cache still looks for nodes.
	c.discovery.Enabled, c.discovery.Gossip = false, false
	browser, store, gossip := c.browser, c.store, c.gossip
	c.browser, c.store, c.gossip = nil, nil, nil
	c.current = nil
	c.mu.Unlock()
	// The asker first, then the producer, then what it fed. Neither wait is on anything another
	// package owns, so this is bounded by one in-flight connection at worst — and the loop must stop
	// before the store it feeds and the sender it reads.
	if gossip != nil {
		gossip.Close()
	}
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

// neighbours is the directory as the gossip loop pools from it: one entry per machine this machine may
// ask, read from the sending side the last peer list produced (node/gossip.go gossipSide).
//
// It is the DIRECTORY rather than a list of its own, and that is the whole of the neighbour rule (§四):
// a configured line is in it (asking it is its first contact), a machine this machine has proved is in
// it, and a machine that arrived by hearsay is not — until its own handshake has happened, at which
// point it is a sighting like any other. Nil when no sending side has been built yet, which is a round
// with nobody to ask rather than an error.
func (c *DelegatorCache) neighbours() []peerEntry {
	s := c.senderNow()
	if s == nil {
		return nil
	}
	return s.peers.Entries()
}

// exchange runs one round trip through the sending side's own client, so that the gossip route inherits
// the TLS pin, the address order and the per-attempt deadlines every other call has. The budget belongs
// to the round rather than here (node/gossip.go gossipSide.exchange): it is a property of the loop, and
// the loop passes the number the operator configured.
func (c *DelegatorCache) exchange(ctx context.Context, e peerEntry, payload []byte, budget time.Duration) ([]byte, error) {
	s := c.senderNow()
	if s == nil {
		return nil, errors.New("node: no sending side to gossip from")
	}
	return s.client.Exchange(ctx, e, payload, budget)
}

// senderNow is the sending side this cache last built, under the lock: what the loop and the two
// methods above read, and the reason a sender that was never asked for is nil rather than a rebuild.
func (c *DelegatorCache) senderNow() *Sender {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
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

	if c.discovery.Gossip {
		// The loop is the OTHER direction of the same switch: it asks, and what it learns comes back
		// through this object into the store above. It reads the store's own contact window, because
		// the question a received claim is judged by ("how old is too old to be worth a retry
		// schedule") is asked of this machine's clock and this machine's config.
		//
		// The addresses it introduces itself by are filtered to the ones a peer could dial, and an
		// operator whose `public_url` is none of them hears about it once, here: the far end drops such
		// a claim without a word, so silence on this side is how a switch ends up looking on while it
		// does nothing (gossipSelfAddrs).
		selfAddrs := gossipSelfAddrs(c.discovery.SelfAddrs)
		if len(c.discovery.SelfAddrs) > 0 && len(selfAddrs) == 0 && c.log != nil {
			c.log.Warn(ctx, "node: node.public_url is not an address a peer could dial — it has to be "+
				"an https address on a private, link-local or CGNAT network — so gossip will not "+
				"introduce this machine", "public_url", c.discovery.SelfAddrs[0])
		}
		loop := newGossipLoop(c, self, selfAddrs, store.contactWindow(), c.discovery.GossipTuning, c.log)
		loop.Start(context.WithoutCancel(ctx))
		c.gossip = loop
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
		open = c.openSender
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
