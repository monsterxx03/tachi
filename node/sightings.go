package node

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// The sightings store: the one place that answers "which machines has this process proved, and which
// is it still trying to".
//
// TWO MAPS AND A LOOP. They used to live inside the mDNS browser, which was right while mDNS was the
// only thing that could produce a candidate. It is not the only one any more — a received gossip
// entry is the second kind of candidate (docs/2026-10-15-a2a-gossip-design.md) — and the two obvious
// ways of adding a source are both wrong:
//
//   - a second queue with its own verification loop would double the dial rate. That rate is the
//     whole reason the loop is paced: every verification is a TLS connection to a machine that nobody
//     has vouched for, and how many candidates arrive is decided by whoever is on the network, so
//     this machine decides how fast it connects (one per tick, whatever the packets do);
//   - two sets of "confirmed machines" would be two answers to the one question the directory asks
//     (node/peerdir.go build), and the directory is keyed on identity precisely so that there is ONE
//     answer.
//
// So: one pending queue, one confirmed set, one verification loop — and a SOURCE is only something
// that feeds it. mDNS is one producer (node/discovery.go filters an announcement's SHAPE and hands
// the candidate over); a received gossip entry is another. What a source must NOT do is decide that
// its candidate is real: that is what the loop is for, and it is the only writer of the confirmed set
// (confirm) — which is why nothing anyone says on a network can cost this machine more than one
// connection.
type sightingsStore struct {
	log *logger.Logger

	// now is the clock, injectable so a test can age a sighting without waiting an hour. It is the
	// discovery side's ONE clock: a sighting's age is read against it, and an announcement is stamped
	// with it (node/discovery.go browseLoop) — a second one would be a second answer to "how old is
	// this".
	now func() time.Time

	// ttl and contact are the two windows everything here is read against (config
	// node.discovery.ttl / contact_ttl). They are fields rather than the constants used at the point
	// of use so that an operator can say how long this machine remembers: how long a row lives, and
	// how long this machine will still vouch for a machine it proved.
	//
	// Set once, before Start, by whoever brings the store up (node/delegatorcache.go
	// startDiscovery). A non-positive value keeps the built-in default (ttlWindow/contactWindow),
	// because zero here would mean "stop remembering the instant you see it", which nobody means.
	ttl     time.Duration
	contact time.Duration

	// verify connects to a candidate's addresses and answers with the identity the peer PROVED there
	// — and the address that answered, because those are two different facts and a machine that
	// vouches for another needs both: the identity is what it may be named by, and the address is the
	// only one it has evidence for (node/sightings.go confirm records it).
	//
	// It is set by the sending side as it is built (it needs that side's client and certificate), and
	// a store without one confirms nothing: a candidate nobody can verify is a candidate nobody
	// should believe.
	//
	// It takes the WHOLE address list, so that trying one machine is one pass with one deadline per
	// address (the client's own rule: short for the ones with a fallback behind them) rather than a
	// dial per tick in whatever order a map hands them over.
	verify func(ctx context.Context, addrs []string) (proved, served string, err error)

	// onSighting is called when a machine ENTERS the directory — a verified sighting that is new, or
	// one that named an address this machine had not learned. It is the entry point's hook (set by the
	// sending side while the store is built), and it exists because membership is the one change
	// nothing else would ever report: a view drawn from the directory has no way to notice.
	//
	// A machine that merely re-announces itself says nothing new and is NOT reported, and that is what
	// keeps this from being a heartbeat — announcements repeat every couple of minutes. Nor is a
	// DEPARTURE: nothing announces "I am going away", so a reader finds out on its next read, which is
	// where the sighting TTL is applied (sightings).
	onSighting func()

	mu      sync.Mutex
	pending map[string]candidate      // identity → candidate, awaiting verification
	seen    map[string]discoveredPeer // identity → verified sighting, with when it was last seen

	// cancel and done are the loop's lifetime, set by Start. done is NIL until then, which is what
	// makes Close safe on a store that never started — a process that cannot quit is the one outcome
	// this must never have (node/discovery.go browseLoop says what that cost once).
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// candidate is one machine that CLAIMS to be somewhere, waiting for a connection to be verified by
// one. Anyone on the network can produce one (a multicast announcement; a peer's word, with gossip),
// so nothing here is believed on its own: the only thing that turns a candidate into a sighting is
// verify proving an identity at one of its addresses.
//
// It is keyed by IDENTITY and carries every address it came with, because that is what verifying
// means: a machine has several addresses, one of them answers, and the others are not separate
// machines to be discovered. Treating each address as its own candidate is what made the first
// version of this take half a minute on a machine with a dozen IPv6 addresses — one dial per
// two-second tick, in map order, and the address that worked could be the twelfth.
type candidate struct {
	Identity string
	Addrs    []string
	// Next is when to try verifying it: a failure schedules a retry rather than a removal, because a
	// node announcing itself before it can answer is the ordinary case for a machine starting up.
	//
	// The ZERO value means "now". A source with no clock of its own (a claim that just came off a
	// socket) says it that way, and feed stamps it with the store's clock — because a zero time
	// compares as earlier than every real one, so leaving it would make the merge below keep "the
	// earliest retry" and get a time that is not a time.
	Next time.Time
	// Stale is set by a source that is handing over something it says is OLD (a peer's word about a
	// machine it reached a while ago): worth one connection, and not a retry schedule. A source that
	// saw the machine itself never sets it — what it saw is current by construction.
	//
	// Two sources disagreeing about one machine is resolved towards the FRESH reading (feed): what
	// this machine saw itself outranks what somebody else says about it.
	Stale bool
	// Attempts is how many verifications this machine has already spent on the candidate, and it is
	// what ends the retry schedule: a candidate that keeps failing is dropped rather than retried
	// until the process exits (maxCandidateAttempts). A merge with a newer claim about the same
	// machine keeps the HIGHER count, because relaxing it there is what would let whoever keeps
	// talking about an address reset the budget that bounds this machine's dials.
	Attempts int
	// Source is which of discovery's suppliers handed this candidate over: an announcement heard on
	// the local network, or a peer's word. It is a SET because one machine can arrive both ways, and
	// it has two readers: the queue prefers what this machine saw itself when two candidates are due
	// at the same moment (verifyDue), and the directory says where a row came from (peerEntry.Source).
	//
	// SourceConfigured can never appear here, and that is not an omission: a config line is not a
	// claim to be verified but a person's decision, which is why there is no queue to put it in.
	Source agentref.PeerSource
}

// The numbers below decide how long a sighting is worth something, how fast candidates are verified,
// and how much work a stranger's packets may cost this machine. They live here because the store is
// what spends them; a second copy anywhere else would be a second answer to the same question — which
// is also why the two WINDOWS are configurable without being spelled out in the config:
// `node.discovery.ttl` and `contact_ttl` default to these, and the defaults are here rather than in
// the config struct (node/delegatorcache.go setWindows).
const (
	// discoveryTTL is how long a machine that discovery alone found stays in the directory after the
	// last time it was seen. An hour is this machine's own choice rather than the announcement's TTL:
	// the announcement says how long its own records may be cached, not how long a person should keep
	// seeing a row for a node that went quiet (design §一.8).
	//
	// This is the DEFAULT: config's `node.discovery.ttl` overrides it (setWindows), and an
	// operator who has opinions about how long this machine remembers a machine sets it there.
	discoveryTTL = time.Hour

	// discoveryRetry is how long a candidate that could not be verified waits before it is tried
	// again. A node that is still starting up announces itself before it can answer, so a failure is
	// the ordinary first result rather than a reason to forget it.
	discoveryRetry = 30 * time.Second

	// discoveryTick paces the verification loop. Verification is a connection per candidate, and a
	// candidate is re-verified on a retry schedule rather than on every packet: a busy network would
	// otherwise turn announcements into connections at whatever rate they arrive.
	discoveryTick = 2 * time.Second

	// contactTTL is how long a machine this process has proved stays worth TALKING about: it bounds
	// what this machine will vouch for when a peer asks, and how old a claim may be before it is
	// worth one connection instead of a retry schedule. It is shorter than discoveryTTL on purpose,
	// and the two answer different questions — "may I still say I know this machine" versus "is the
	// row still in the directory" — with the tighter one on the side where being wrong means saying
	// something false about a machine to somebody else.
	//
	// This is the DEFAULT: config's `node.discovery.contact_ttl` overrides it (setWindows).
	contactTTL = 10 * time.Minute

	// maxCandidateAttempts bounds how many connections ONE candidate is worth before the machine
	// behind it is dropped. A machine still starting up announces itself before it can answer, so one
	// failure means nothing — but "keeps failing" has to end somewhere, because the queue is fed by
	// whoever is on the network (and, with gossip, by whoever can reach this node's A2A port): a
	// candidate that is never dropped is a queue slot plus one dial per retry window, forever.
	//
	// Five tries at discoveryRetry is about two and a half minutes, and a machine that really is
	// coming up announces itself again before that — which puts it back in the queue as a fresh
	// candidate with a full budget (for a machine this node heard about rather than saw, the way back
	// is a peer naming it again). So it is bounded, not banned.
	maxCandidateAttempts = 5

	// maxPendingCandidates caps how many machines may be waiting to be verified at once. It is
	// deliberately the SAME number as the directory's own cap on discovered machines
	// (maxDiscoveredPeers): a queue longer than the list it feeds is work nobody asked for. Anyone
	// who can reach this machine can add to it, so what a stranger's packet rate grows has to be
	// bounded — past this the store stops taking NEW identities and says so in one debug line (a
	// machine already waiting still merges, and a full queue drains as candidates are verified,
	// dropped or given up on).
	maxPendingCandidates = maxDiscoveredPeers
)

// newSightingsStore returns a store that is not running: Start launches its verification loop.
func newSightingsStore(log *logger.Logger) *sightingsStore {
	if log == nil {
		log = logger.New("node")
	}
	return &sightingsStore{
		log:     log,
		now:     time.Now,
		ttl:     discoveryTTL,
		contact: contactTTL,
		pending: make(map[string]candidate),
		seen:    make(map[string]discoveredPeer),
	}
}

// setWindows overrides the two windows from config, called once by whoever brings the store up
// (node/delegatorcache.go). A non-positive value is ignored rather than applied: the one thing a
// mistyped duration must not be able to do is make this machine forget a machine the moment it finds
// it.
func (s *sightingsStore) setWindows(ttl, contact time.Duration) {
	if s == nil {
		return
	}
	if ttl > 0 {
		s.ttl = ttl
	}
	if contact > 0 {
		s.contact = contact
	}
}

// ttlWindow is how long a row lives, and contactWindow is how long this machine will vouch for a
// machine — each answering with the built-in default when nothing set it.
func (s *sightingsStore) ttlWindow() time.Duration {
	if s == nil || s.ttl <= 0 {
		return discoveryTTL
	}
	return s.ttl
}

func (s *sightingsStore) contactWindow() time.Duration {
	if s == nil || s.contact <= 0 {
		return contactTTL
	}
	return s.contact
}

// setVerifier is how the sending side teaches the store to check a candidate. Called with the newest
// client each time the sending side is rebuilt.
func (s *sightingsStore) setVerifier(verify func(ctx context.Context, addrs []string) (proved, served string, err error)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.verify = verify
	s.mu.Unlock()
}

// Start runs the verification loop until the context is canceled or Close is called. It is called
// once, by whoever brought the store up (node/delegatorcache.go).
func (s *sightingsStore) Start(parent context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel, s.done = cancel, make(chan struct{})
	done := s.done
	go s.verifyLoop(ctx, done)
}

// Close stops the verification loop and drops both halves of the store.
//
// Dropping them is the same statement the directory made before this was a store of its own: a
// machine this process found and can no longer watch for is not an entry it may keep listing. It is
// also safe without Start and safe twice — an entry point calls it on its way out, and nothing here
// waits on anything another package owns (the promise that keeps a process able to quit).
func (s *sightingsStore) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		cancel, done := s.cancel, s.done
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
		s.mu.Lock()
		clear(s.pending)
		clear(s.seen)
		s.mu.Unlock()
	})
}

// verifyLoop turns candidates into sightings, one at a time.
//
// One at a time on purpose: each verification is a TLS connection to a machine that may be a
// stranger's, and the rate at which candidates arrive is chosen by whoever is on the network. A loop
// that took the next due candidate each tick bounds that rate at one connection per tick, per machine,
// whatever the network does — and with more than one source feeding the queue, that bound is still
// one, which is the reason the queue is shared.
func (s *sightingsStore) verifyLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(discoveryTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.verifyDue(ctx)
		}
	}
}

// verifyDue verifies the earliest candidate whose retry time has come, and reports whether it did.
func (s *sightingsStore) verifyDue(ctx context.Context) bool {
	s.mu.Lock()
	verify := s.verify
	now := s.clock()
	id, cand, found := "", candidate{}, false
	for machine, c := range s.pending {
		if c.Next.After(now) {
			continue
		}
		if !found || dueFirst(c, cand) {
			id, cand, found = machine, c, true
		}
	}
	s.mu.Unlock()
	if !found || verify == nil {
		return false
	}

	// The connection happens with no lock held: it is a network round trip, and the rule this
	// codebase follows is one lock at a time.
	proved, served, err := verify(ctx, cand.Addrs)
	switch {
	case err != nil:
		if cand.Stale {
			// Whoever handed this over said it was old, so it gets the one connection it was worth.
			// A machine that is still there will be told to this one again, and one that is gone has
			// cost this machine exactly one dial — which is the whole reason a second-hand address is
			// marked before it is queued (gossipEntry.Age).
			s.forget(id)
			return true
		}
		s.reschedule(id, cand, err)
	case proved == "":
		// No identity to prove: nothing at those addresses is serving a TLS surface this machine can
		// read (it may be plaintext, or something else entirely). Not an error worth a line per
		// attempt — it is a candidate that will never verify, so it is dropped rather than retried for
		// ever.
		s.forget(id)
	case proved != cand.Identity:
		// The claim named one identity and the machine at those addresses proved another: whatever
		// said so lied. Said out loud, because it is the one case where something on the network
		// deliberately misled this machine.
		s.log.Warn(ctx, "node: a discovery announcement named an identity the machine at that address "+
			"did not prove; dropping the candidate", "claimed", cand.Identity, "proved", proved,
			"addrs", cand.Addrs)
		s.forget(id)
	default:
		s.confirm(cand, served)
	}
	return true
}

// dueFirst reports whether a is the better candidate to spend this tick's one connection on than b.
//
// Two rules, in this order: the retry time that came first, and — for two that are due at the very
// same moment — the one this machine heard about ITSELF before the one it was told about (design §六,
// 先手压流言). The tie-break matters more than it looks: a gossip answer can make a whole batch due at
// once (feed stamps them all with the same clock), and without it the order would be whatever a map
// happened to hand over — while a machine still announcing itself on the local network is the more
// useful connection of the two, because its announcement is fresh evidence rather than somebody's
// memory of it.
func dueFirst(a, b candidate) bool {
	if !a.Next.Equal(b.Next) {
		return a.Next.Before(b.Next)
	}
	return heardDirectly(a) && !heardDirectly(b)
}

// heardDirectly reports whether this machine heard about the machine from the machine — an
// announcement on the local network, as opposed to a peer's word about it.
func heardDirectly(c candidate) bool { return c.Source&agentref.SourceMDNS != 0 }

// feed adds one candidate to the queue, merging it with one already waiting for the same identity.
//
// This is where a SOURCE hands over what it saw, and the only thing it decides is that this is worth
// a connection: the shape checks a source makes are its own (mDNS reads TXT records, gossip reads a
// peer's JSON), and none of them is trust.
//
// The queue is BOUNDED here (maxPendingCandidates) because this is the one door into it: the sources
// feeding it are the local network and, with gossip, whoever can reach this node's A2A port, and what
// a stranger's packet rate grows has to have an end. Past the cap a NEW identity is refused — the
// candidates already waiting are the ones this machine promised a connection to, and refusing the
// newcomer adds no dial while dropping an old one would break that promise.
func (s *sightingsStore) feed(c candidate) {
	if s == nil || !IsNodeID(c.Identity) {
		// A candidate with no identity cannot be keyed, merged, confirmed by name or reported as a
		// row, so it is not one this machine can hold. Dropped here rather than at each source: this
		// is the one door into the queue.
		return
	}
	// A source with no clock says "now" with a zero time, and it is stamped here rather than at each
	// source so that the merge below compares two real instants (see candidate.Next).
	if c.Next.IsZero() {
		c.Next = s.clock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.pending[c.Identity]; ok {
		// Already waiting: keep the earliest retry (so a machine that announces once a second does not
		// keep pushing its own verification out of reach) and the addresses learned since, because a
		// claim is about where a machine IS and the next one may be the one that works.
		if prev.Next.Before(c.Next) {
			c.Next = prev.Next
		}
		c.Addrs = prependAddrs(prev.Addrs, c.Addrs)
		// Two sources disagreeing is resolved towards the fresher reading: a peer's word about a
		// machine this machine has itself just reached is not a reason to stop retrying it.
		c.Stale = c.Stale && prev.Stale
		// The ways this machine heard about the machine are collected rather than replaced: a machine
		// announced on the network and later named by a peer is one machine this machine knows both
		// ways, and a reader asking "how do you know it" wants both answers.
		c.Source |= prev.Source
		// The attempts spent so far are not refunded by a new claim about the same machine: that is
		// the budget which keeps a claim from costing this machine dials forever.
		if prev.Attempts > c.Attempts {
			c.Attempts = prev.Attempts
		}
	} else if len(s.pending) >= maxPendingCandidates {
		s.log.Debug(context.Background(), "node: the candidate queue is full; not queuing one more",
			"node_id", c.Identity, "queued", len(s.pending))
		return
	}
	s.pending[c.Identity] = c
}

// reschedule keeps a candidate that could not be verified, for another attempt later — until it has
// been tried maxCandidateAttempts times, at which point it is dropped like any other candidate that
// will not answer. Nothing that follows from a stranger's claim may schedule dials forever.
func (s *sightingsStore) reschedule(id string, cand candidate, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[id]; !ok {
		return // forgotten while the attempt was in flight
	}
	cand.Attempts++
	if cand.Attempts >= maxCandidateAttempts {
		delete(s.pending, id)
		s.log.Debug(context.Background(), "node: a discovered node did not answer and has been "+
			"dropped; it will be tried again if it announces itself", "node_id", id,
			"addrs", cand.Addrs, "attempts", cand.Attempts, "error", err.Error())
		return
	}
	cand.Next = s.clock().Add(discoveryRetry)
	s.pending[id] = cand
	s.log.Debug(context.Background(), "node: a discovered node did not answer yet; will try again",
		"node_id", id, "addrs", cand.Addrs, "attempts", cand.Attempts, "error", err.Error())
}

// forget drops one candidate for good: nothing at those addresses will ever verify, so asking again
// is a connection spent on a known answer.
func (s *sightingsStore) forget(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// confirm records a verified machine: it entered the directory, and every address it came with
// travels with it, because they are all places this machine may try.
//
// served is the address that ANSWERED, which is a fact about this machine's own evidence rather than
// about the peer: it is the one address here that has been reached, it is what a vouching answer
// quotes, and it is why a caller has to hand it over (the identity alone would leave this machine
// unable to say where it had actually been).
func (s *sightingsStore) confirm(cand candidate, served string) {
	s.mu.Lock()
	prev, had := s.seen[cand.Identity]
	next := discoveredPeer{
		Identity: cand.Identity,
		Addrs:    append([]string(nil), cand.Addrs...),
		Served:   served,
		Source:   cand.Source,
		// The claim is confirmed now, whatever brought this machine into the store before it (see
		// Claimed: the handshake that verified this candidate is itself a contact).
		Claimed: true,
		Seen:    s.clock(),
	}
	if had {
		// An address once learned is not dropped, only reordered: what said "I am here" most recently
		// goes first (the same rule the directory applies to a card, node/peerdir.go).
		next.Addrs = prependAddrs(prev.Addrs, next.Addrs)
		// And every way this machine has heard about the machine is kept, for the same reason: the
		// two suppliers fill different gaps, and a machine that came by one of them did not stop
		// having come by the other.
		next.Source |= prev.Source
	}
	s.seen[cand.Identity] = next
	delete(s.pending, cand.Identity)
	// A machine this store had not CONFIRMED before, or one that told it about an address it did not
	// know: either way a reader has a row to draw or an address to try that it did not have a moment
	// ago. The ORDER is deliberately not part of it — it is "who spoke last", which changes on every
	// announcement, while what a reader cares about is whether there is somewhere new to dial.
	//
	// "Had" is asked of Claimed, not of the map: verifying this very candidate was a card read, and
	// touch records those, so by the time this runs the machine is usually already in the map. Asking
	// the map would answer "already listed" for the first machine anyone ever told us about, and the
	// one moment its row appeared would go unreported.
	entered := !had || !prev.Claimed || !sameAddrs(prev.Addrs, next.Addrs)
	s.mu.Unlock()

	s.log.Info(context.Background(), "node: discovered a node and proved its identity",
		"node_id", cand.Identity, "addrs", cand.Addrs)

	if entered && s.onSighting != nil {
		s.onSighting()
	}
}

// touch records one CONTACT this machine made itself — a round it asked and got an answer to, or a
// card it read — as opposed to a claim about a machine that somebody else made.
//
// It exists because there are two ways to be in contact with a machine and only one of them goes
// through the queue: a claim arrives as a candidate and confirm writes the sighting, while a
// connection this machine STARTS has no claim behind it — and the design counts both ("一次成功的交换
// 必须算作一次接触", and the two windows' table: 读通一次卡片，或者完成一次交换). Without this, a machine
// this process talks to every few minutes would never be one it may vouch for, simply because nobody
// ever named it in a candidate — and a mesh whose reach depends on real reachability (design §一.4)
// would be narrower than the evidence this machine actually holds.
//
// What it is NOT is a way in for a stranger: it is only called after a handshake that proved an
// identity (node/remoteclient.go contact reads that identity back out of the pin the handshake just
// filed), and the row it writes is keyed by that identity — the same key the directory merges on, so
// a machine that is both configured and reached stays ONE row. It therefore mints no sighting this
// machine has no evidence for; it only keeps what it has proved fresh enough to stand behind.
//
// Nothing calls onSighting: a machine that just answered a request from here is one the directory
// already has a row for (its config line, or the sighting it entered by), so no reader is looking at
// something that appeared.
//
// It adds no SOURCE either, and that is deliberate: reaching a machine outbound is not a way of
// coming to know it (there is nothing to learn — this machine picked the address). A machine that is
// in the config keeps saying so, and one that was announced keeps saying that; a touch only makes
// what this machine has proved fresh enough to stand behind.
func (s *sightingsStore) touch(identity, served string) {
	if s == nil || !IsNodeID(identity) || served == "" {
		return
	}
	s.mu.Lock()
	prev, had := s.seen[identity]
	next := prev
	next.Identity = identity
	next.Served = served
	next.Seen = s.clock()
	if had {
		// The address that just answered goes first, and one learned earlier is not dropped: the same
		// rule confirm applies, for the same reason (it is where this machine really got through).
		next.Addrs = prependAddrs(prev.Addrs, []string{served})
	} else {
		next.Addrs = []string{served}
	}
	s.seen[identity] = next
	s.mu.Unlock()
}

// sightings is the store's answer for the directory: the machines it has VERIFIED and seen recently,
// in a stable order.
//
// It reads memory and nothing else — the directory's cheap path calls it on every keystroke — and it
// applies the TTL on read, so a machine that went quiet stops being an entry without anything having
// to sweep it.
func (s *sightingsStore) sightings() []discoveredPeer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	out := make([]discoveredPeer, 0, len(s.seen))
	for _, seen := range s.seen {
		if now.Sub(seen.Seen) > s.ttlWindow() {
			delete(s.seen, seen.Identity)
			continue
		}
		out = append(out, seen)
	}
	// Sorted so a rebuild of the directory produces the same list twice: Go's map order is random,
	// and rows that shuffle between keystrokes are unusable (node/peerdir.go).
	slices.SortFunc(out, func(a, b discoveredPeer) int { return strings.Compare(a.Identity, b.Identity) })
	return out
}

// recent is the machines this store has seen within a window, stable-ordered, and it decides nothing
// about expiry: the window is the CALLER's question and there is more than one — a row lives for
// ttlWindow (sightings), while this machine will only vouch for a machine it touched within
// contactWindow. The default keeps the second one tighter, which is the design's stance; an operator
// who makes them equal is choosing to be as permissive about vouching as about listing.
func (s *sightingsStore) recent(within time.Duration) []discoveredPeer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	out := make([]discoveredPeer, 0, len(s.seen))
	for _, seen := range s.seen {
		if now.Sub(seen.Seen) > within {
			continue
		}
		out = append(out, seen)
	}
	slices.SortFunc(out, func(a, b discoveredPeer) int { return strings.Compare(a.Identity, b.Identity) })
	return out
}

// clock is the store's "now", and it answers even for a store that was never built: a browser whose
// store is nil (a test driving the browser's shape checks on its own) still has to stamp an
// announcement with something, and the wall clock is the only honest answer.
func (s *sightingsStore) clock() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

// sameAddrs reports whether two address lists are the same set of addresses, whatever order they came
// in. Both lists are built by prependAddrs, so neither holds a duplicate and comparing the lengths is
// enough to go with containment.
func sameAddrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, addr := range a {
		seen[addr] = true
	}
	for _, addr := range b {
		if !seen[addr] {
			return false
		}
	}
	return true
}
