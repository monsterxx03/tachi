package node

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

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

	// verify connects to a candidate's addresses and answers with the identity the peer PROVED there,
	// which is the only thing that turns a candidate into a sighting. It is set by the sending side as
	// it is built (it needs that side's client and certificate), and a store without one confirms
	// nothing: a candidate nobody can verify is a candidate nobody should believe.
	//
	// It takes the WHOLE address list, so that trying one machine is one pass with one deadline per
	// address (the client's own rule: short for the ones with a fallback behind them) rather than a
	// dial per tick in whatever order a map hands them over.
	verify func(ctx context.Context, addrs []string) (string, error)

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
	Next time.Time
}

// newSightingsStore returns a store that is not running: Start launches its verification loop.
func newSightingsStore(log *logger.Logger) *sightingsStore {
	if log == nil {
		log = logger.New("node")
	}
	return &sightingsStore{
		log:     log,
		now:     time.Now,
		pending: make(map[string]candidate),
		seen:    make(map[string]discoveredPeer),
	}
}

// setVerifier is how the sending side teaches the store to check a candidate. Called with the newest
// client each time the sending side is rebuilt.
func (s *sightingsStore) setVerifier(verify func(ctx context.Context, addrs []string) (string, error)) {
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
		if !found || c.Next.Before(cand.Next) {
			id, cand, found = machine, c, true
		}
	}
	s.mu.Unlock()
	if !found || verify == nil {
		return false
	}

	// The connection happens with no lock held: it is a network round trip, and the rule this
	// codebase follows is one lock at a time.
	proved, err := verify(ctx, cand.Addrs)
	switch {
	case err != nil:
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
		s.confirm(cand)
	}
	return true
}

// feed adds one candidate to the queue, merging it with one already waiting for the same identity.
//
// This is where a SOURCE hands over what it saw, and the only thing it decides is that this is worth
// a connection: the shape checks a source makes are its own (mDNS reads TXT records, gossip reads a
// peer's JSON), and none of them is trust.
func (s *sightingsStore) feed(c candidate) {
	if s == nil || !IsNodeID(c.Identity) {
		// A candidate with no identity cannot be keyed, merged, confirmed by name or reported as a
		// row, so it is not one this machine can hold. Dropped here rather than at each source: this
		// is the one door into the queue.
		return
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
		c.Addrs = prependAddrs(c.Addrs, prev.Addrs)
	}
	s.pending[c.Identity] = c
}

// reschedule keeps a candidate that could not be verified, for another attempt later.
func (s *sightingsStore) reschedule(id string, cand candidate, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[id]; !ok {
		return // forgotten while the attempt was in flight
	}
	cand.Next = s.clock().Add(discoveryRetry)
	s.pending[id] = cand
	s.log.Debug(context.Background(), "node: a discovered node did not answer yet; will try again",
		"node_id", id, "addrs", cand.Addrs, "error", err.Error())
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
func (s *sightingsStore) confirm(cand candidate) {
	s.mu.Lock()
	prev, had := s.seen[cand.Identity]
	next := discoveredPeer{Identity: cand.Identity, Addrs: append([]string(nil), cand.Addrs...), Seen: s.clock()}
	if had {
		// An address once learned is not dropped, only reordered: what said "I am here" most recently
		// goes first (the same rule the directory applies to a card, node/peerdir.go).
		next.Addrs = prependAddrs(next.Addrs, prev.Addrs)
	}
	s.seen[cand.Identity] = next
	delete(s.pending, cand.Identity)
	// A machine this list did not have, or one that told it about an address it did not know: either
	// way a reader has a row to draw or an address to try that it did not have a moment ago. The
	// ORDER is deliberately not part of it — it is "who spoke last", which changes on every
	// announcement, while what a reader cares about is whether there is somewhere new to dial.
	entered := !had || !sameAddrs(prev.Addrs, next.Addrs)
	s.mu.Unlock()

	s.log.Info(context.Background(), "node: discovered a node and proved its identity",
		"node_id", cand.Identity, "addrs", cand.Addrs)

	if entered && s.onSighting != nil {
		s.onSighting()
	}
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
		if now.Sub(seen.Seen) > discoveryTTL {
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
