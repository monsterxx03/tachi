package node

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
