package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/agentref"
)

// fakeMesh is a discovery side a test drives by hand: what it would say, and what it was handed.
type fakeMesh struct {
	entries []gossipEntry
	got     []candidate
}

func (f *fakeMesh) vouches() []gossipEntry { return f.entries }

func (f *fakeMesh) receive(cands []candidate) { f.got = append(f.got, cands...) }

// registerFakeMesh installs one for the length of a test. The registration is process-wide (the node
// service cannot be handed the object that owns the sending side), so a test that installs one takes
// it away again.
func registerFakeMesh(t *testing.T, f *fakeMesh) {
	t.Helper()
	registerMesh(f)
	t.Cleanup(func() { clearMesh(f) })
}

// A surface on loopback is plaintext and identifies nobody, so there is nobody to answer: gossip is
// refused there rather than answered anonymously, which would hand this machine's peers to anyone who
// can open a socket.
func TestGossipIsRefusedWhereNobodyIsIdentified(t *testing.T) {
	h := startWith(t, t.TempDir(), func(o *Options) { o.Gossip = true })

	resp, err := http.Post("http://"+h.RemoteAddr()+gossipPath, "application/json",
		strings.NewReader(`{"peers":[]}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"a caller with no certificate is nobody, and nobody is not told who this machine knows")
}

// Off is off: the route exists (it is one expression in the mux, like the Card) and it says no. It is a
// startup fact and the operator's decision, so the answer is a refusal rather than an empty list —
// "I am not answering" and "I know nobody" are different statements.
func TestGossipSaysNoWhenItIsOff(t *testing.T) {
	h := tlsNode(t, t.TempDir(), func(o *Options) { o.Gossip = false })
	registerFakeMesh(t, &fakeMesh{entries: []gossipEntry{{ID: claimedID, Addr: lanAddr}}})

	caller := newSenderIdentity(t)
	status, body, err := callAs(t, h, caller, http.MethodPost, gossipPath, `{"peers":[]}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, status, "body=%s", body)
}

// The whole exchange, over the real TLS surface, against a discovery side a test owns: what comes in
// becomes candidates, and what goes out is what this process vouches for.
func TestAGossipExchangeIsAnsweredAndLearnedFrom(t *testing.T) {
	dir := t.TempDir()
	h := tlsNode(t, dir, func(o *Options) { o.Gossip = true })
	self, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	caller := newSenderIdentity(t)

	mesh := &fakeMesh{entries: []gossipEntry{
		{ID: claimedID, Addr: lanAddr, Age: 30},
		{ID: caller.NodeID, Addr: lanAddr, Age: 5}, // about the asker: already said below, not an answer
		{ID: self.NodeID, Addr: lanAddr, Age: 5},   // about this machine: cannot be true
	}}
	registerFakeMesh(t, mesh)

	body := fmt.Sprintf(`{"self":{"addrs":["%s"]},"peers":[{"id":"%s","addr":"%s","age":12}]}`,
		lanAddr, discoveredID, lanAddr2)
	status, raw, err := callAs(t, h, caller, http.MethodPost, gossipPath, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)

	var answer gossipPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &answer))
	require.Len(t, answer.Peers, 1, "only the machine neither the asker nor this one is: %s", raw)
	require.Equal(t, claimedID, answer.Peers[0].ID)
	require.Equal(t, 30, answer.Peers[0].Age, "and how long ago this machine proved it")
	require.Nil(t, answer.Self, "an answer never carries where the answerer is: the asker just reached it")

	// What the peer said arrived as candidates: where the CALLER says it can be reached (whose identity
	// the certificate proved on this very connection), and the machine it named.
	require.Len(t, mesh.got, 2)
	require.Equal(t, caller.NodeID, mesh.got[0].Identity)
	require.Equal(t, []string{lanAddr}, mesh.got[0].Addrs)
	require.Equal(t, discoveredID, mesh.got[1].Identity)
	require.Equal(t, []string{lanAddr2}, mesh.got[1].Addrs)
	require.False(t, mesh.got[1].Stale, "12 seconds old is worth a retry schedule")
}

// A claim is only ever a candidate, and the shape checks are what keeps a peer from spending this
// machine's connections on things that could never be a machine of this fleet.
func TestAClaimBecomesACandidateOrNothing(t *testing.T) {
	const (
		caller = "11112222333344445555666677778888"
		self   = "88887777666655554444333322221111"
	)
	for _, tc := range []struct {
		name   string
		entry  gossipEntry
		why    string
		stale  bool
		absent bool
	}{
		{name: "a machine this machine could reach", entry: gossipEntry{ID: claimedID, Addr: lanAddr}},
		{name: "an identity that is not a node id", entry: gossipEntry{ID: "nas", Addr: lanAddr}, absent: true},
		{name: "an address that is not https", entry: gossipEntry{ID: claimedID, Addr: "http://192.168.1.9:7080"}, absent: true},
		{name: "an address on the public internet", entry: gossipEntry{ID: claimedID, Addr: "https://8.8.8.8:7080"}, absent: true},
		{name: "an address with no host", entry: gossipEntry{ID: claimedID, Addr: "https://"}, absent: true},
		{name: "the asker naming itself", entry: gossipEntry{ID: caller, Addr: lanAddr}, absent: true},
		{name: "a claim to be this machine", entry: gossipEntry{ID: self, Addr: lanAddr}, absent: true},
		{name: "a claim older than this machine will vouch for",
			entry: gossipEntry{ID: claimedID, Addr: lanAddr, Age: int(contactTTL.Seconds()) + 60}, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gossipCandidates(gossipPayload{Peers: []gossipEntry{tc.entry}}, caller, self, contactTTL, nil)
			if tc.absent {
				require.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			require.Equal(t, tc.entry.ID, got[0].Identity)
			require.Equal(t, []string{tc.entry.Addr}, got[0].Addrs)
			require.Equal(t, tc.stale, got[0].Stale,
				"an old claim is worth one connection rather than a retry schedule")
			require.Equal(t, agentref.SourceGossip, got[0].Source,
				"and every one of these came from a peer's word, which is what the queue and the "+
					"panel both read")
		})
	}
}

// Where the asker says it can be reached is the one entry whose identity is not a claim: the
// certificate proved it on this connection, and the address is what gets dialled.
func TestTheAskerCanIntroduceItself(t *testing.T) {
	const self = "88887777666655554444333322221111"
	caller := claimedID

	got := gossipCandidates(gossipPayload{Self: &gossipSelf{Addrs: []string{lanAddr, "https://8.8.8.8:7080"}}},
		caller, self, contactTTL, nil)
	require.Len(t, got, 1, "the reachable address and not the one nothing could dial")
	require.Equal(t, caller, got[0].Identity)
	require.Equal(t, []string{lanAddr}, got[0].Addrs)
	require.Equal(t, agentref.SourceGossip, got[0].Source,
		"the identity is proved, the address is not — and the way this machine heard it is the same "+
			"either way")

	// Two Tachis on one machine share an identity (one data dir), so a sibling process calling this
	// one must not be introduced to itself.
	require.Empty(t, gossipCandidates(gossipPayload{Self: &gossipSelf{Addrs: []string{lanAddr}}},
		self, self, contactTTL, nil))
}

// One entry carries a handful of addresses and no more, and the cap is here rather than only at the
// dial because the field is 64 KiB of JSON a peer composed: without it, one entry hands this machine a
// dial list of thousands — every one of them a connection carrying this machine's certificate.
func TestOneEntryCarriesOnlySoManyAddresses(t *testing.T) {
	addrs := make([]string, 0, maxGossipAddrs*3)
	for i := range maxGossipAddrs * 3 {
		addrs = append(addrs, fmt.Sprintf("https://192.168.1.%d:7080", i+1))
	}

	got := gossipCandidates(gossipPayload{Self: &gossipSelf{Addrs: addrs}}, claimedID, "", contactTTL, nil)
	require.Len(t, got, 1)
	require.Equal(t, addrs[:maxGossipAddrs], got[0].Addrs,
		"the first ones the peer named, and none past the cap")
}

// What this machine refuses, and why each refusal is worth its own code: a caller can tell them apart,
// and none of them is "we do not know" about a request that was understood.
func TestGossipRefusesWhatItCannotAnswer(t *testing.T) {
	h := tlsNode(t, t.TempDir(), func(o *Options) { o.Gossip = true })
	registerFakeMesh(t, &fakeMesh{})
	caller := newSenderIdentity(t)

	status, body, err := callAs(t, h, caller, http.MethodGet, gossipPath, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusMethodNotAllowed, status, "body=%s", body)

	status, body, err = callAs(t, h, caller, http.MethodPost, gossipPath, "not json at all")
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, status, "body=%s", body)

	// A list longer than this machine will hold is refused rather than truncated: a shortened list
	// reads as "this is all I know", which would be an answer this node made up.
	var big strings.Builder
	big.WriteString(`{"peers":[`)
	for i := 0; i <= maxGossipEntries; i++ {
		if i > 0 {
			big.WriteByte(',')
		}
		fmt.Fprintf(&big, `{"id":"%s","addr":"%s"}`, claimedID, lanAddr)
	}
	big.WriteString(`]}`)
	status, body, err = callAs(t, h, caller, http.MethodPost, gossipPath, big.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, status, "body=%s", body)

	// And a body nobody could have meant as an exchange is cut off before it is read.
	huge := `{"peers":[]}` + strings.Repeat(" ", maxGossipBodyBytes)
	status, body, err = callAs(t, h, caller, http.MethodPost, gossipPath, huge)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, status, "body=%s", body)
}

// What goes out is what this process has PROVED itself, and only while it can still stand behind it.
// A machine it has merely heard about is never repeated: that is what stops a rumour from spreading
// through it, and it is why the payload carries the age at all.
func TestOnlyWhatThisProcessHasProvedIsRepeated(t *testing.T) {
	cache := NewDelegatorCache("", Discovery{Gossip: true}, nil, nil)
	t.Cleanup(cache.Close)
	store := newSightingsStore(nil)
	cache.store = store

	// A machine it has proved, at an address that answered.
	store.confirm(candidate{Identity: claimedID, Addrs: []string{lanAddr2, lanAddr}}, lanAddr2)
	vouched := cache.vouches()
	require.Len(t, vouched, 1)
	require.Equal(t, claimedID, vouched[0].ID)
	require.Equal(t, lanAddr2, vouched[0].Addr, "the address that ANSWERED, not the first one claimed")
	require.Less(t, vouched[0].Age, 60)

	// A candidate it has merely heard about is not something it can vouch for: nothing was proved.
	cache.receive([]candidate{{Identity: discoveredID, Addrs: []string{lanAddr}}})
	require.Len(t, cache.vouches(), 1)
	store.mu.Lock()
	_, queued := store.pending[discoveredID]
	store.mu.Unlock()
	require.True(t, queued, "but it is queued for exactly the one connection that would prove it")

	// And a machine it proved long enough ago is one it stops talking about, though the directory may
	// still list it: two windows, two questions.
	store.now = func() time.Time { return time.Now().Add(contactTTL + time.Minute) }
	require.Empty(t, cache.vouches())
	require.Len(t, store.sightings(), 1, "the row is still there: an hour is a different question")
}

// A process with no discovery side has nothing to say and nowhere to put a claim, and it says so
// rather than failing: the node service can be hosted by a process that never opened a sending side.
func TestAGossipExchangeWithoutADiscoverySideIsEmpty(t *testing.T) {
	h := tlsNode(t, t.TempDir(), func(o *Options) { o.Gossip = true })
	clearMesh(nil) // no mesh registered: this is that process
	caller := newSenderIdentity(t)

	status, raw, err := callAs(t, h, caller, http.MethodPost, gossipPath,
		fmt.Sprintf(`{"peers":[{"id":"%s","addr":"%s"}]}`, discoveredID, lanAddr))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)

	var answer gossipPayload
	require.NoError(t, json.Unmarshal([]byte(raw), &answer))
	require.Empty(t, answer.Peers)
}

// lanAddr and lanAddr2 are addresses that pass every shape check a candidate goes through: https, and
// on a private network. Nothing dials them in these tests — the mesh is a fake, and the store's loop
// is only started when a test starts it.
const (
	lanAddr  = "https://192.168.1.9:7080"
	lanAddr2 = "https://192.168.71.168:7080"
)

// ── the client half ──────────────────────────────────────────────────────────
//
// A round is timing and selection, and neither of those is about a network: the loop is driven here by
// hand — a fake process to pool from, a fake exchange to answer, and a clock the test moves itself.

// The three neighbours these tests ask, spelled as node ids because that is what they are (and what a
// pooled entry is keyed by).
const (
	gossipA  = "aaaa0000000000000000000000000001"
	gossipB  = "aaaa0000000000000000000000000002"
	gossipC  = "aaaa0000000000000000000000000003"
	gossipD  = "aaaa0000000000000000000000000004"
	gossipE  = "aaaa0000000000000000000000000005"
	gossipMe = "aaaa00000000000000000000000000ff"
)

// fakeSide is the process a loop runs in, driven by hand: the neighbours it pools from, the round
// trips it would make, and everything it was handed.
type fakeSide struct {
	entries []peerEntry
	// said is what this machine vouches for (the sending side's answer to vouches).
	said []gossipEntry
	// exchangeErr, keyed by the neighbour's key, is how a round trip fails; a neighbour with no entry
	// here answers.
	exchangeErr map[string]error
	// answer, keyed the same way, is what a neighbour's reply carries.
	answer map[string][]gossipEntry

	asked    []string        // the neighbours asked, in order
	sent     []gossipPayload // what went out, in the same order
	received []candidate     // what the answers handed to the queue
	// budgets is the budget each round was given, in the same order as asked: the loop's `timeout`,
	// which is the one number the sending side does NOT own (node/gossip.go gossipSide.exchange).
	budgets []time.Duration
}

func (f *fakeSide) neighbours() []peerEntry   { return f.entries }
func (f *fakeSide) vouches() []gossipEntry    { return f.said }
func (f *fakeSide) receive(cands []candidate) { f.received = append(f.received, cands...) }

func (f *fakeSide) exchange(_ context.Context, e peerEntry, payload []byte, budget time.Duration) ([]byte, error) {
	var sent gossipPayload
	if err := json.Unmarshal(payload, &sent); err != nil {
		return nil, err
	}
	f.asked = append(f.asked, e.key())
	f.sent = append(f.sent, sent)
	f.budgets = append(f.budgets, budget)
	if err := f.exchangeErr[e.key()]; err != nil {
		return nil, err
	}
	return json.Marshal(gossipPayload{Peers: f.answer[e.key()]})
}

// gossipPeer is one machine in the pool: an identity and where to dial it.
func gossipPeer(id string, addrs ...string) peerEntry {
	return peerEntry{Identity: id, Addrs: addrs}
}

// quietLoop is a loop over a fake side with the timing a test can reason about: no jitter, and
// otherwise the numbers the process itself runs with (the tuning a caller with no config gives it:
// zero, which is "the shipped default").
func quietLoop(side gossipSide) *gossipLoop {
	l := newGossipLoop(side, "", nil, contactTTL, gossipTuning{}, nil)
	l.jitter = func(d time.Duration) time.Duration { return d }
	return l
}

// countingSide is a process that counts how many times the directory was READ: one turn of the watcher
// has one moment to look at, so it has one reading (node/gossip.go step).
type countingSide struct {
	fakeSide
	reads int
}

func (c *countingSide) neighbours() []peerEntry {
	c.reads++
	return c.fakeSide.neighbours()
}

// One turn of the watcher reads the directory ONCE, and everything it needs comes out of that reading:
// what changed, who may be asked, and what this machine already knows. Each of those used to be its own
// reading, and a reading rebuilds the whole directory (node/peerdir.go build) — three times, every
// couple of seconds, for as long as the process runs, all answering the same question about the same
// instant.
func TestATurnOfTheWatcherReadsTheDirectoryOnce(t *testing.T) {
	side := &countingSide{fakeSide: fakeSide{entries: []peerEntry{
		gossipPeer(gossipA, lanAddr), gossipPeer(gossipB, lanAddr),
	}}}
	l := quietLoop(side)
	l.fanout = 2

	l.step(t.Context(), time.Now())
	require.Equal(t, 2, len(side.asked), "the round still asked its neighbours")
	require.Equal(t, 1, side.reads, "and the directory was read once for all of it")
}

// The pool is the directory, and the round-robin is what orders it: a machine this one has never asked
// comes first, and nothing is asked twice within a round.
func TestARoundAsksTheMachinesItHasNotAskedLately(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{
		gossipPeer(gossipA, lanAddr), gossipPeer(gossipB, lanAddr), gossipPeer(gossipC, lanAddr),
	}}
	l := quietLoop(side)
	l.fanout = 2
	at := time.Now()

	l.round(t.Context(), at, side.entries)
	require.Equal(t, []string{gossipA, gossipB}, side.asked, "two of the three, in directory order")

	l.round(t.Context(), at.Add(time.Minute), side.entries)
	require.Equal(t, []string{gossipC, gossipA}, side.asked[2:],
		"then the one never asked, and behind it the one asked longest ago")
}

// A configured line is in the pool before it has ever been contacted — asking it IS its first contact
// (the handshake behind it files the pin) — and "recently contacted" is never a condition of being in
// the pool: a machine that dropped out when it was not refreshed could never be refreshed (§四).
func TestAConfiguredLineNobodyHasContactedIsStillAsked(t *testing.T) {
	line := peerEntry{Addrs: []string{lanAddr}, cfgAddr: lanAddr, Source: agentref.SourceConfigured}
	side := &fakeSide{entries: []peerEntry{line}}
	l := quietLoop(side)

	l.round(t.Context(), time.Now(), side.entries)
	require.Equal(t, []string{"@" + lanAddr}, side.asked, "a line with no identity is still a neighbour")

	l.round(t.Context(), time.Now().Add(contactTTL+time.Hour), side.entries)
	require.Len(t, side.asked, 2, "and it does not fall out of the pool for being quiet")
}

// What a round never asks: this machine itself (it would be telling it about itself), a neighbour with
// no address that can prove an identity, and one whose round just failed.
func TestARoundSkipsThisMachineAndMutedNeighbours(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{
		gossipPeer(gossipA, lanAddr), gossipPeer(gossipB, lanAddr), gossipPeer(gossipC, lanAddr),
		gossipPeer(gossipMe, lanAddr),
		{Identity: gossipD, Addrs: []string{"http://192.168.1.9:7080"}},
	}}
	l := quietLoop(side)
	l.fanout = 9
	l.self = gossipMe
	at := time.Now()
	l.mutedUntil[gossipB] = at.Add(l.mute)

	l.round(t.Context(), at, side.entries)
	require.Equal(t, []string{gossipA, gossipC}, side.asked,
		"not this machine, not the one with nothing dialable, and not the muted one")

	// A mute is a delay rather than a blacklist: past it the neighbour is asked again.
	l.round(t.Context(), at.Add(l.mute+time.Second), side.entries)
	require.Contains(t, side.asked, gossipB, "the mute expired on its own")
}

// The loop runs the numbers the operator asked for, and the shipped default wherever they asked for
// nothing — the defaults live in node/gossip.go and nowhere else (gossipTuning.orDefault), so there is
// one place to read what "unconfigured" means.
func TestTheLoopRunsTheConfiguredNumbersAndDerivesTheFloor(t *testing.T) {
	shipped := newGossipLoop(&fakeSide{}, "", nil, contactTTL, gossipTuning{}, nil)
	require.Equal(t, gossipPeriod, shipped.period)
	require.Equal(t, gossipFanout, shipped.fanout)
	require.Equal(t, gossipNudge, shipped.nudge)
	require.Equal(t, gossipTimeout, shipped.timeout)
	require.Equal(t, gossipMute, shipped.mute)
	require.Equal(t, 2*gossipNudge, shipped.debounce,
		"the floor between nudges is derived, not a setting of its own")

	tuned := newGossipLoop(&fakeSide{}, "", nil, contactTTL, gossipTuning{
		Period:  90 * time.Second,
		Fanout:  5,
		Nudge:   3 * time.Second,
		Timeout: time.Second,
		Mute:    time.Minute,
	}, nil)
	require.Equal(t, 90*time.Second, tuned.period)
	require.Equal(t, 5, tuned.fanout)
	require.Equal(t, 3*time.Second, tuned.nudge)
	require.Equal(t, time.Second, tuned.timeout)
	require.Equal(t, time.Minute, tuned.mute)
	require.Equal(t, 6*time.Second, tuned.debounce, "and it follows the CONFIGURED nudge, not the shipped one")

	// A value that could only be a mistake keeps the shipped one: nothing here is a way to say "never",
	// and a loop that never rounds again is not what a typo should buy.
	broken := newGossipLoop(&fakeSide{}, "", nil, contactTTL, gossipTuning{
		Period:  -time.Minute,
		Fanout:  -1,
		Nudge:   -time.Second,
		Timeout: -time.Second,
		Mute:    -time.Minute,
	}, nil)
	require.Equal(t, gossipPeriod, broken.period)
	require.Equal(t, gossipFanout, broken.fanout)
	require.Equal(t, gossipNudge, broken.nudge)
	require.Equal(t, gossipTimeout, broken.timeout)
	require.Equal(t, gossipMute, broken.mute)
}

// The budget travels with EVERY round: it belongs to the loop (it is a property of the round rather
// than of the sending side), so a configured timeout reaches the client on each exchange instead of
// being read once, when a sending side happened to be built.
func TestEveryRoundCarriesTheLoopsOwnBudget(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{gossipPeer(gossipA, lanAddr)}}
	l := quietLoop(side)
	l.timeout = 250 * time.Millisecond

	l.round(t.Context(), time.Now(), side.entries)
	require.Equal(t, []time.Duration{250 * time.Millisecond}, side.budgets)
}

// "Who sees something I cannot" is measured rather than assumed (§四): with the round-robin unable to
// separate two neighbours, the one whose last answer taught this machine machines goes first.
func TestANeighbourThatBroughtSomethingIsAskedFirst(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{
		gossipPeer(gossipA, lanAddr), gossipPeer(gossipB, lanAddr),
	}}
	l := quietLoop(side)
	l.fanout = 1
	at := time.Now()
	l.asked[gossipA], l.asked[gossipB] = at, at
	l.contributed[gossipB] = 4

	l.round(t.Context(), at, side.entries)
	require.Equal(t, []string{gossipB}, side.asked)
}

// Steady state is defined by the loop itself (§四): the criterion is the directory's CONTENT — a
// machine this one did not have, or an address one of them did not — and never "somebody answered"
// (which happens every round) nor anything carrying a timestamp (which changes without anything
// happening).
func TestOnlyAContentChangeBringsTheNextRoundForward(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{gossipPeer(gossipA, lanAddr)}}
	l := quietLoop(side)
	at := time.Now()

	// The first tick is the baseline and the startup round: what this machine already knows is not a
	// change, or a process that came up knowing a fleet would nudge itself immediately.
	l.step(t.Context(), at)
	require.Len(t, side.asked, 1, "the startup round happens on the first tick")
	require.Equal(t, at.Add(l.period), l.next)

	// The same machine, the same address, an older sighting: not a change.
	moved := []peerEntry{gossipPeer(gossipA, lanAddr)}
	moved[0].LastSeen = at.Add(time.Hour)
	side.entries = moved
	l.step(t.Context(), at.Add(l.debounce))
	require.Equal(t, at.Add(l.period), l.next, "an age is not content")

	// A machine this one did not have is.
	side.entries = append(side.entries, gossipPeer(gossipB, lanAddr))
	later := at.Add(2 * l.debounce)
	l.step(t.Context(), later)
	require.Equal(t, later.Add(l.nudge), l.next, "a new machine brings the next round in")

	// And a machine telling this one about a place it did not know is a change too — the other half of
	// the same criterion. (The cadence is put back first: the round brought forward above is already
	// sooner than the nudge below would be, so nothing could move it further.)
	l.next = later.Add(l.period)
	side.entries = []peerEntry{gossipPeer(gossipA, lanAddr, lanAddr2)}
	newer := later.Add(l.debounce)
	l.step(t.Context(), newer)
	require.Equal(t, newer.Add(l.nudge), l.next, "a new address is content as much as a new machine")

	// The ORDER of those addresses, though, is not content at all: it says who spoke last, and that
	// changes on every announcement, while the set of places this machine may dial does not
	// (contentKey sorts them). Without that, a peer that merely re-announced itself would look like a
	// machine that moved.
	l.next = newer.Add(l.period)
	side.entries = []peerEntry{gossipPeer(gossipA, lanAddr2, lanAddr)}
	l.step(t.Context(), newer.Add(l.debounce))
	require.Equal(t, newer.Add(l.period), l.next, "the same addresses in another order are the same content")
}

// Changes arrive as a string rather than one at a time — a round brings back several machines and they
// are proved one verification tick apart — so the round is brought forward once for the burst, not once
// per arrival (§四 稳态). What holds that is the nudge being a MINIMUM: an arrival later in the string
// can only ever name a later moment than the one already scheduled.
func TestABurstOfChangesBringsOneRoundForward(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{gossipPeer(gossipA, lanAddr)}}
	l := quietLoop(side)
	at := time.Now()
	l.step(t.Context(), at)
	require.Equal(t, at.Add(l.period), l.next)

	// The first arrival of the string.
	side.entries = append(side.entries, gossipPeer(gossipB, lanAddr))
	l.step(t.Context(), at.Add(time.Minute))
	brought := l.next
	require.Equal(t, at.Add(time.Minute).Add(l.nudge), brought)

	// The ones behind it move nothing: the round is already coming, and each of them can only name a
	// later moment than the one already scheduled.
	for i, id := range []string{gossipC, gossipD, gossipE} {
		side.entries = append(side.entries, gossipPeer(id, lanAddr))
		l.step(t.Context(), at.Add(time.Minute+time.Duration(i+1)*discoveryTick))
		require.Equal(t, brought, l.next, "arrival %d moved nothing", i+1)
	}

	asked := len(side.asked)
	l.step(t.Context(), brought)
	require.Greater(t, len(side.asked), asked, "and the round does run when it comes")
}

// The floor between nudges is measured from the last nudge that was APPLIED, so it has to outlast the
// round that nudge brought forward: a machine proved just after that round (verification is one tick
// per machine, so this is the ordinary case rather than a corner) would otherwise pull the next round
// in again — and the cadence would be the fast one after all, which is the whole thing the floor is
// there to stop.
func TestTheFloorHoldsUntilTheRoundANudgeBroughtForwardHasRun(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{gossipPeer(gossipA, lanAddr)}}
	l := quietLoop(side)
	at := time.Now()
	l.step(t.Context(), at)
	require.Equal(t, at.Add(l.period), l.next)

	// The first arrival brings the round forward.
	side.entries = append(side.entries, gossipPeer(gossipB, lanAddr))
	l.step(t.Context(), at.Add(time.Minute))
	brought := at.Add(time.Minute).Add(l.nudge)
	require.Equal(t, brought, l.next)

	// That round happens, and leaves the cadence where it was.
	l.step(t.Context(), brought)
	require.Equal(t, brought.Add(l.period), l.next, "the round the nudge asked for did run")

	// One verification tick later a second machine is proved. It is inside the floor, so the cadence
	// does not move: without the floor this would pull the next round to now + nudge, which is sooner
	// than the round already scheduled and would be picked up by the next tick's round as well.
	side.entries = append(side.entries, gossipPeer(gossipC, lanAddr))
	l.step(t.Context(), brought.Add(discoveryTick))
	require.Equal(t, brought.Add(l.period), l.next, "the floor held the cadence")

	// Past the floor, the next arrival does bring it in again: a string that runs long still converges.
	side.entries = append(side.entries, gossipPeer(gossipD, lanAddr))
	later := at.Add(time.Minute).Add(l.debounce)
	l.step(t.Context(), later)
	require.Equal(t, later.Add(l.nudge), l.next, "and past the floor a nudge still counts")
}

// A round that failed mutes its neighbour for gossipMute, so that a machine that is switched off costs
// one timeout rather than one every time its turn comes round. Neither outcome is an error the loop
// reports: a round that fails writes nothing else (§四).
func TestAFailedRoundMutesTheNeighbourUntilItIsWorthTryingAgain(t *testing.T) {
	side := &fakeSide{
		entries:     []peerEntry{gossipPeer(gossipA, lanAddr), gossipPeer(gossipB, lanAddr)},
		exchangeErr: map[string]error{gossipA: errors.New("connection refused")},
	}
	l := quietLoop(side)
	l.fanout = 2
	at := time.Now()

	l.round(t.Context(), at, side.entries)
	require.Equal(t, []string{gossipA, gossipB}, side.asked, "the round asks both")

	l.round(t.Context(), at.Add(time.Minute), side.entries)
	require.Equal(t, []string{gossipB}, side.asked[2:], "and next time only the one that answered")

	l.round(t.Context(), at.Add(l.mute+time.Second), side.entries)
	require.Equal(t, []string{gossipA, gossipB}, side.asked[3:], "the mute expires on its own")
}

// What goes out is this machine's own account of where it is plus the machines it has proved itself —
// and never an entry about the peer being asked or about this machine. What comes back goes into the
// same queue an announcement goes into.
func TestAnExchangeCarriesSelfAndOnlyWhatThisMachineProved(t *testing.T) {
	side := &fakeSide{
		entries: []peerEntry{gossipPeer(gossipA, lanAddr)},
		said: []gossipEntry{
			{ID: claimedID, Addr: lanAddr2, Age: 30},
			{ID: gossipA, Addr: lanAddr2, Age: 3}, // about the peer being asked: it just reached us
			{ID: gossipMe, Addr: lanAddr2, Age: 3},
		},
		answer: map[string][]gossipEntry{gossipA: {{ID: discoveredID, Addr: lanAddr, Age: 12}}},
	}
	l := quietLoop(side)
	l.self = gossipMe
	l.selfAddrs = []string{lanAddr2}

	l.round(t.Context(), time.Now(), side.entries)
	require.Len(t, side.sent, 1)
	sent := side.sent[0]
	require.NotNil(t, sent.Self, "a machine that only calls out has to say where it is")
	require.Equal(t, []string{lanAddr2}, sent.Self.Addrs)
	require.Len(t, sent.Peers, 1, "the peer itself and this machine are not answers this one can give")
	require.Equal(t, claimedID, sent.Peers[0].ID)
	require.Equal(t, 30, sent.Peers[0].Age, "and how long ago it was proved, so the peer can judge")

	require.Len(t, side.received, 1, "the answer went into the candidate queue")
	require.Equal(t, discoveredID, side.received[0].Identity)
	require.Equal(t, []string{lanAddr}, side.received[0].Addrs)
}

// The half above is the loop over a fake process: it can say nothing about whether the two halves of
// this exchange agree on the wire — the path, the method, the body's shape, the content type. A real
// remoteClient asking a real node over the real mTLS surface is the only test that would notice, and
// every loop test above is blind to it, because a fake side never speaks HTTP. A 404, a 405 and a 400
// all arrive here as a failed round, which is a mute for that neighbour.
func TestARealExchangeWalksTheWireAndCountsAsAContact(t *testing.T) {
	h := tlsNode(t, t.TempDir(), func(o *Options) { o.Gossip = true })
	mesh := &fakeMesh{entries: []gossipEntry{{ID: claimedID, Addr: lanAddr}}}
	registerFakeMesh(t, mesh)

	// The three things OpenSender builds a client out of: this machine's identity and certificate, and
	// the TOFU record the handshake will pin the answerer into.
	dir := t.TempDir()
	identity, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	cert, err := loadOrCreateCertificate(dir, identity)
	require.NoError(t, err)
	known, err := openKnownPeers(t.TempDir()+"/peer-identities.json", nil)
	require.NoError(t, err)
	directory, err := newPeerDirectory(nil)
	require.NoError(t, err)
	directory.known = known
	store := newSightingsStore(nil)
	client := &remoteClient{
		peers:          directory,
		known:          known,
		ourCert:        cert,
		http:           &http.Client{},
		requestTimeout: defaultRequestTimeout,
		store:          store,
	}

	served := "https://" + dialAddr(t, h)
	e := peerEntry{Addrs: []string{served}}
	payload, err := json.Marshal(gossipPayload{
		Self:  &gossipSelf{Addrs: []string{lanAddr2}},
		Peers: []gossipEntry{{ID: discoveredID, Addr: lanAddr, Age: 3}},
	})
	require.NoError(t, err)

	raw, err := client.Exchange(t.Context(), e, payload, gossipTimeout)
	require.NoError(t, err, "one POST to %s, over the mTLS surface", gossipPath)

	var answer gossipPayload
	require.NoError(t, json.Unmarshal(raw, &answer), "the answer decodes in the shape the loop reads")
	require.Len(t, answer.Peers, 1)
	require.Equal(t, claimedID, answer.Peers[0].ID)

	// What went out arrived on the other side and became what an announcement becomes: a candidate, one
	// connection away from being believed.
	require.Len(t, mesh.got, 2, "the caller's own address, then the machine it named")
	require.Equal(t, identity.NodeID, mesh.got[0].Identity)
	require.Equal(t, []string{lanAddr2}, mesh.got[0].Addrs)
	require.Equal(t, discoveredID, mesh.got[1].Identity)

	// And the round is a CONTACT, recorded as one: the identity comes out of the pin the handshake just
	// filed, and the address that answered is the one on the row (node/sightings.go touch).
	pinned, ok := known.pinnedIdentityAt(served)
	require.True(t, ok, "the handshake pinned the machine that answered")
	recent := store.recent(store.contactWindow())
	require.Len(t, recent, 1)
	require.Equal(t, pinned, recent[0].Identity)
	require.Equal(t, served, recent[0].Served)

	// The budget belongs to the caller rather than to the delegation timeout: one that is already spent
	// makes the round fail — which the loop answers with a mute — instead of quietly borrowing a longer
	// one and holding the loop for it.
	_, err = client.Exchange(t.Context(), e, payload, time.Nanosecond)
	require.Error(t, err, "a budget of a nanosecond is a round that does not happen")
}

// Close is safe without Start and safe twice, and it never waits on anything another package owns —
// the same promise the store and the browser make, because an entry point calls it on its way out.
func TestClosingTheLoopReturnsWhetherOrNotItStarted(t *testing.T) {
	never := quietLoop(&fakeSide{})
	require.NotPanics(t, never.Close)
	require.NotPanics(t, never.Close, "and calling it twice does not wait for something already stopped")

	started := quietLoop(&fakeSide{})
	started.Start(t.Context())
	closed := make(chan struct{})
	go func() {
		started.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the loop does not leave on its cancel")
	}
}

// A machine that goes quiet loses its row, and what the loop remembered about it has to go with it:
// three tables keyed by every machine this process has ever asked would otherwise be the one thing here
// that only grows. Losing the MUTE with the row is deliberate — a machine that comes back is one
// somebody has just proved, so it should be asked rather than skipped on the strength of an old failure.
func TestTheLoopForgetsMachinesThatLeftThePool(t *testing.T) {
	side := &fakeSide{entries: []peerEntry{gossipPeer(gossipA, lanAddr)}}
	l := quietLoop(side)
	at := time.Now()
	l.asked[gossipA] = at
	l.contributed[gossipA] = 5
	l.mutedUntil[gossipA] = at.Add(l.mute)

	side.entries = nil
	l.round(t.Context(), at, side.entries)
	require.Empty(t, l.asked, "its place in the rotation went with its row")
	require.Empty(t, l.contributed)
	require.Empty(t, l.mutedUntil)
}
