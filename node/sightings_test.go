package node

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/agentref"
)

// The two windows are the operator's (config node.discovery.ttl / contact_ttl), and what they decide
// is different: how long a row lives, and how long this machine will still vouch for a machine it has
// proved. A value nobody set keeps the built-in default, because the one thing a mistyped duration
// must not be able to do is make this machine forget a machine the moment it finds it.
func TestTheDiscoveryWindowsAreConfigurable(t *testing.T) {
	store := newSightingsStore(nil)
	require.Equal(t, discoveryTTL, store.ttlWindow(), "the window this shipped with")
	require.Equal(t, contactTTL, store.contactWindow())

	store.setWindows(time.Minute, time.Second)
	require.Equal(t, time.Minute, store.ttlWindow())
	require.Equal(t, time.Second, store.contactWindow())

	// A machine found and proved now falls outside BOTH windows two minutes later, and both are the
	// configured ones rather than the built-in ones.
	store.confirm(candidate{Identity: discoveredID, Addrs: []string{"https://192.168.1.9:7080"}},
		"https://192.168.1.9:7080")
	require.Len(t, store.sightings(), 1)
	store.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	require.Empty(t, store.recent(store.contactWindow()),
		"past the configured contact window, so nothing is vouched for")
	require.Empty(t, store.sightings(), "and past the configured row window, so the row is gone")

	// Nothing set — or a value that could only be a mistake — keeps what was there.
	store.setWindows(0, -time.Second)
	require.Equal(t, time.Minute, store.ttlWindow())
	require.Equal(t, time.Second, store.contactWindow())
}

// Close is what an entry point calls on its way out, so the one thing it must never do is wait for
// something that will not happen. A store that was never started has no loop to wait for, and one
// that was started stops it — both are the promise that keeps a process able to quit, which is what
// breaking it cost once (node/discovery.go browseLoop).
func TestClosingAStoreReturnsWhetherOrNotItStarted(t *testing.T) {
	never := newSightingsStore(nil)
	require.NotPanics(t, never.Close)
	require.NotPanics(t, never.Close, "and calling it twice does not wait on something already waited for")

	started := newSightingsStore(nil)
	started.Start(t.Context())
	// A candidate that is already due, so the loop has something to do while it is being stopped: the
	// verifier answers nothing, which is the ordinary "not a Tachi node" result.
	started.setVerifier(func(context.Context, []string) (string, string, error) { return "", "", nil })
	started.feed(candidate{Identity: discoveredID, Addrs: []string{"https://192.168.71.168:9990"}})

	closed := make(chan struct{})
	go func() {
		started.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the verification loop does not leave on its cancel")
	}

	require.Empty(t, started.sightings(), "and what it held is dropped with it")
	require.False(t, started.verifyDue(context.Background()), "nothing is queued after a close")
}

// queued reports how many machines are waiting for a connection, read under the store's lock.
func queued(s *sightingsStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// The queue is bounded, and the bound is the directory's own cap on discovered machines rather than a
// second number: what feeds this queue is whoever is on the network (mDNS on the local network; with
// gossip, anyone who can reach the A2A port), so a stranger's packet rate must not be able to grow it.
// A machine already waiting still MERGES — refusing it too would throw away the address that just
// arrived, which is the one thing the queue exists to collect.
func TestTheCandidateQueueIsBounded(t *testing.T) {
	store := newSightingsStore(nil)
	for i := range maxPendingCandidates {
		store.feed(candidate{Identity: fmt.Sprintf("%032x", i), Addrs: []string{lanAddr}})
	}
	require.Equal(t, maxPendingCandidates, queued(store), "the queue filled up to its cap")

	store.feed(candidate{Identity: fmt.Sprintf("%032x", maxPendingCandidates), Addrs: []string{lanAddr}})
	store.mu.Lock()
	_, refused := store.pending[fmt.Sprintf("%032x", maxPendingCandidates)]
	store.mu.Unlock()
	require.Equal(t, maxPendingCandidates, queued(store), "one more machine is refused, not queued")
	require.False(t, refused, "and it is not in the queue at all")

	// A machine already waiting is still merged: the fresh address is what this machine wanted.
	first := fmt.Sprintf("%032x", 0)
	store.feed(candidate{Identity: first, Addrs: []string{lanAddr2}})
	store.mu.Lock()
	got := store.pending[first]
	store.mu.Unlock()
	require.ElementsMatch(t, []string{lanAddr, lanAddr2}, got.Addrs, "both addresses are held")
	require.Equal(t, maxPendingCandidates, queued(store), "and the queue did not grow")
}

// "It did not answer yet" has to end somewhere: the candidate queue is fed by anyone who can reach
// this machine, and a candidate that is never dropped is one queue slot plus one dial per retry
// window, forever. The machine is not blacklisted by this — it announces itself again, and comes back
// as a fresh candidate with a full budget.
func TestACandidateThatKeepsFailingIsDroppedRatherThanRetriedForever(t *testing.T) {
	store := newSightingsStore(nil)
	now := time.Now()
	store.now = func() time.Time { return now }
	store.setVerifier(func(context.Context, []string) (string, string, error) {
		return "", "", errors.New("connection refused")
	})
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr}})

	for attempt := 1; attempt < maxCandidateAttempts; attempt++ {
		require.True(t, store.verifyDue(context.Background()), "attempt %d was spent", attempt)
		require.Equal(t, 1, queued(store), "and it is still waiting after attempt %d", attempt)
		now = now.Add(discoveryRetry + time.Second)
	}
	// The attempt that spends the last of its budget is the one that ends it.
	require.True(t, store.verifyDue(context.Background()))
	require.Zero(t, queued(store), "after its last attempt it is dropped rather than rescheduled")
	require.False(t, store.verifyDue(context.Background()), "so nothing dials it again")

	// And the same machine coming back is a fresh candidate, not a banned one.
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr}})
	require.Equal(t, 1, queued(store), "it is tried again once it announces itself again")
	require.True(t, store.verifyDue(context.Background()), "with a full budget, because it is new")
}

// Which address gets dialled first is "the one advertised most recently", and it is prependAddrs'
// rule (node/peerdir.go). The store has to apply it in the SAME direction as the directory does, or a
// machine that moved is dialled at last instead of first. Pinned here because the argument order is
// easy to get backwards and nothing else notices: every other assertion about a list of addresses
// compares it as a set.
func TestTheNewestClaimGoesAtTheFront(t *testing.T) {
	store := newSightingsStore(nil)
	now := time.Now()
	store.now = func() time.Time { return now }

	// Waiting to be verified: the machine named lanAddr, and then lanAddr2 as well.
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr}})
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr2}})
	store.mu.Lock()
	queuedAddrs := store.pending[discoveredID].Addrs
	store.mu.Unlock()
	require.Equal(t, []string{lanAddr2, lanAddr}, queuedAddrs, "the newest claim is tried first")

	// And the same rule on the row it leaves behind once it is proved.
	store.confirm(candidate{Identity: discoveredID, Addrs: []string{lanAddr}}, lanAddr)
	store.confirm(candidate{Identity: discoveredID, Addrs: []string{lanAddr2}}, lanAddr2)
	sightings := store.sightings()
	require.Len(t, sightings, 1)
	require.Equal(t, []string{lanAddr2, lanAddr}, sightings[0].Addrs, "however many times it moves")
}

// Two sources saying different things about one machine: what this machine saw ITSELF outranks what a
// peer says about it, or a peer could demote a machine this one has just reached (Stale is "one
// connection, no retry schedule"). The zero Next a clockless source uses is stamped here, because a
// zero time compares as earlier than every real one and would win the merge below forever.
func TestTwoSourcesDisagreeingAboutOneMachineTakeTheFresherReading(t *testing.T) {
	store := newSightingsStore(nil)
	now := time.Now()
	store.now = func() time.Time { return now }

	// What mDNS saw: a real arrival time, and the machine itself said where it is.
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr}, Next: now})
	// What a peer said about the same machine: reached a while ago, so only worth one connection.
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr2}, Stale: true})

	store.mu.Lock()
	got := store.pending[discoveredID]
	store.mu.Unlock()
	require.False(t, got.Stale, "the machine this one saw itself is not demoted by a peer's word")
	require.ElementsMatch(t, []string{lanAddr, lanAddr2}, got.Addrs, "and both addresses are kept")
	require.False(t, got.Next.IsZero(), "a source with no clock is stamped with this store's")
	require.True(t, got.Next.Equal(now), "which is the earlier of the two, so an announcement "+
		"cannot push its own verification out of reach")

	// Both sources saying it is old keeps it old: the point is which reading wins, not that Stale is
	// never set.
	store.forget(discoveredID)
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr}, Stale: true})
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr2}, Stale: true})
	store.mu.Lock()
	got = store.pending[discoveredID]
	store.mu.Unlock()
	require.True(t, got.Stale, "one connection is all a claim nobody is fresh about is worth")
}

// A contact this machine made ITSELF — a round it asked and got an answer to, or a card it read — is
// what keeps a machine it reached in the set it may vouch for, with nothing announcing it and no
// candidate ever naming it. It is the same evidence confirm writes, from a dial this machine started
// rather than one a claim asked for (§三: 一次成功的交换必须算作一次接触).
func TestAContactThisMachineMadeIsRecorded(t *testing.T) {
	store := newSightingsStore(nil)
	now := time.Now()
	store.now = func() time.Time { return now }

	store.touch(discoveredID, lanAddr)
	recent := store.recent(store.contactWindow())
	require.Len(t, recent, 1)
	require.Equal(t, discoveredID, recent[0].Identity)
	require.Equal(t, lanAddr, recent[0].Served, "the address that answered, which is the only one proved")
	require.Equal(t, []string{lanAddr}, recent[0].Addrs)

	// A second contact from another address refreshes the time and puts that address first, without
	// dropping the one learned before it — the same rule confirm follows.
	now = now.Add(time.Minute)
	store.touch(discoveredID, lanAddr2)
	recent = store.recent(store.contactWindow())
	require.Len(t, recent, 1, "one machine, one row")
	require.Equal(t, lanAddr2, recent[0].Served)
	require.Equal(t, []string{lanAddr2, lanAddr}, recent[0].Addrs)
	require.True(t, recent[0].Seen.Equal(now))

	// Keeping it fresh is the whole point: it stays vouchable up to the window, and not past it.
	now = now.Add(contactTTL - time.Second)
	require.Len(t, store.recent(store.contactWindow()), 1)
	now = now.Add(2 * time.Second)
	require.Empty(t, store.recent(store.contactWindow()), "past the window it is not vouched for any more")

	// What it will not do is mint a row out of nothing: a contact names an identity and an address that
	// answered, and neither is optional.
	store.touch("", lanAddr)
	store.touch(discoveredID, "")
	store.touch("not-a-node-id", lanAddr)
	require.Empty(t, store.recent(store.contactWindow()))
}

// HOW this machine came to know a machine is every way at once, not the last way: the two suppliers
// fill different gaps (one crosses no router, the other cannot see a machine it was never pointed at),
// and a machine that arrived by one of them did not stop having arrived by the other. It is what the
// panel states and what the queue reads, so both halves are pinned here.
func TestEveryWayOneMachineWasHeardAboutIsKept(t *testing.T) {
	store := newSightingsStore(nil)
	now := time.Now()
	store.now = func() time.Time { return now }

	// Announced on the local network, then named by a peer — and merged while still awaiting
	// verification, which is the one place two claims about one machine meet.
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr}, Source: agentref.SourceMDNS})
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr2}, Source: agentref.SourceGossip})
	store.mu.Lock()
	got := store.pending[discoveredID]
	store.mu.Unlock()
	require.Equal(t, agentref.SourceMDNS|agentref.SourceGossip, got.Source,
		"the machine is one this one both saw and was told about")

	// And the row it leaves behind keeps both, through a second proof from the other way round: a
	// later sighting does not erase how this machine first heard about it.
	store.confirm(candidate{Identity: discoveredID, Addrs: []string{lanAddr}, Source: agentref.SourceMDNS}, lanAddr)
	store.confirm(candidate{Identity: discoveredID, Addrs: []string{lanAddr2}, Source: agentref.SourceGossip}, lanAddr2)
	sightings := store.sightings()
	require.Len(t, sightings, 1)
	require.Equal(t, agentref.SourceMDNS|agentref.SourceGossip, sightings[0].Source)
	require.Equal(t, "mDNS · gossip", agentref.SourceWord(sightings[0].Source),
		"which is what a reader is told, in words")
}

// Two candidates due at the same moment are not equal work: a machine still announcing itself on the
// local network is fresher evidence than somebody's memory of a machine it reached a while ago, so it
// gets the tick's one connection first (design §六, 先手压流言). Without this the order would be
// whatever a map happened to hand over — Go's map order is random, and a gossip answer can make a
// whole batch due at once, because feed stamps every clockless claim with the same instant.
func TestAMachineThisStoreHeardItselfIsVerifiedBeforeOneItWasToldAbout(t *testing.T) {
	// Fed in the order that would otherwise decide it: the relayed one first, so a map walk could
	// plausibly answer with it.
	store := newSightingsStore(nil)
	// A fixed clock, because "due at the same moment" is the case in hand: a gossip answer carries
	// claims with no time of their own, and feed stamps every one of them with this store's clock.
	now := time.Now()
	store.now = func() time.Time { return now }
	store.setVerifier(func(_ context.Context, addrs []string) (string, string, error) {
		// Each address answers with the identity the claim about it named, so neither candidate takes
		// the "named one identity and proved another" path — that is a different test's subject.
		if addrs[0] == lanAddr {
			return gossipID, lanAddr, nil
		}
		return discoveredID, addrs[0], nil
	})
	store.feed(candidate{Identity: gossipID, Addrs: []string{lanAddr}, Source: agentref.SourceGossip})
	store.feed(candidate{Identity: discoveredID, Addrs: []string{lanAddr2}, Source: agentref.SourceMDNS})

	store.mu.Lock()
	relayed, saw := store.pending[gossipID], store.pending[discoveredID]
	store.mu.Unlock()
	require.True(t, relayed.Next.Equal(saw.Next), "both are due at the same moment, which is the case in hand")

	require.True(t, store.verifyDue(context.Background()))
	require.Empty(t, store.pending[discoveredID], "the machine this one heard itself was verified first")
	_, relayedStillWaiting := store.pending[gossipID]
	require.True(t, relayedStillWaiting, "and the one it was told about is still waiting its turn")

	// The relayed one is not refused, only queued behind: it gets the next tick.
	require.True(t, store.verifyDue(context.Background()))
	store.mu.Lock()
	_, stillQueued := store.pending[gossipID]
	store.mu.Unlock()
	require.False(t, stillQueued, "and it is verified on the tick after")
}
