package node

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/libp2p/zeroconf/v2"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/config"
)

const (
	discoveredID = "dddd4444dddd4444dddd4444dddd4444"
	claimedID    = "eeee5555eeee5555eeee5555eeee5555"
	// gossipID is a machine this machine was only ever TOLD about, as opposed to discoveredID, which
	// these tests use for one it saw for itself.
	gossipID = "cccc3333cccc3333cccc3333cccc3333"
)

// testBrowser is a browser with no interfaces and no socket at all: these tests drive its decisions
// directly (observe/decide/Sightings), so what this machine's network looks like must not enter into
// them — and newMDNSBrowser would refuse to build without a LAN interface, rightly (browserOver).
func testBrowser(t *testing.T) *mdnsBrowser {
	t.Helper()
	return browserOver(nil, nasID, nil, newSightingsStore(nil))
}

// txtAnnouncement is one announcement the way a well-behaved Tachi node sends it.
func txtAnnouncement(instance, id string, addrs ...string) announcement {
	return announcement{
		Instance: instance,
		TXT: map[string]string{
			mdnsTXTIdentity: id,
			mdnsTXTAPIVer:   "1",
			mdnsTXTScheme:   mdnsScheme,
		},
		Addrs: addrs,
		Seen:  time.Now(),
	}
}

// An announcement is only ever a CLAIM, and the checks that need no connection are about its SHAPE.
// Nothing is dialled for a packet that does not claim to be a node this machine could talk to: the
// network is shared with everything else on it, and a `_tachi._tcp` from something else must cost
// nothing (design §5 预筛).
func TestAnAnnouncementIsNotDialledUnlessItClaimsThisProgram(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(announcement) announcement
		why  string
	}{
		{"no identity", func(a announcement) announcement { delete(a.TXT, mdnsTXTIdentity); return a }, "id"},
		{"an identity that is not a node id", func(a announcement) announcement {
			a.TXT[mdnsTXTIdentity] = "nas"
			return a
		}, "id"},
		{"another API version", func(a announcement) announcement { a.TXT[mdnsTXTAPIVer] = "2"; return a }, "version"},
		{"no version at all", func(a announcement) announcement { delete(a.TXT, mdnsTXTAPIVer); return a }, "version"},
		{"a scheme that is not https", func(a announcement) announcement { a.TXT[mdnsTXTScheme] = "http"; return a }, "scheme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBrowser(t)
			cand, why := b.decide(tc.mut(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080")))
			require.Nil(t, cand, "nothing may be dialled on this announcement")
			require.Contains(t, why, tc.why, "and the reason names the field that was wrong")
		})
	}
}

// This machine's own announcement is not a peer, and it is not worth a line in the log either: every
// node on the network hears itself.
func TestAnAnnouncementOfThisVeryMachineIsNotAPeer(t *testing.T) {
	b := testBrowser(t)
	cand, why := b.decide(txtAnnouncement("this-machine", nasID, "https://192.168.1.9:7080"))
	require.Nil(t, cand)
	require.Empty(t, why, "silence: it is us")
}

// What an announcement claims is not trust, but WHERE the claim came from is a fact about this
// machine's own reading — it heard this itself — and it is what decides the queue's order when two
// candidates are due at once (node/sightings.go dueFirst) and what the panel says about the row.
func TestAnAnnouncementIsMarkedAsSomethingThisMachineHeardItself(t *testing.T) {
	b := testBrowser(t)
	cand, why := b.decide(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080"))
	require.Empty(t, why)
	require.NotNil(t, cand)
	require.Equal(t, agentref.SourceMDNS, cand.Source, "heard on the local network, not told by a peer")
}

// A multicast announcement that resolves to a public address is a machine asking to be dialled from
// wherever — and dialling outwards on a stranger's word is not a thing discovery does.
func TestAnAnnouncementWithOnlyPublicAddressesIsNotDialled(t *testing.T) {
	b := testBrowser(t)
	cand, why := b.decide(txtAnnouncement("nas-dddd4444", discoveredID, "https://8.8.8.8:7080"))
	require.Nil(t, cand)
	require.Contains(t, why, "local network")
}

// A machine that announces several addresses keeps all of them: one identity, more than one place it
// can be reached, which is the ordinary shape of a machine on a LAN and a mesh at once.
func TestEveryLocalAddressOfAnAnnouncementBecomesACandidate(t *testing.T) {
	b := testBrowser(t)
	cand, why := b.decide(txtAnnouncement("nas-dddd4444", discoveredID,
		"https://192.168.1.9:7080", "https://8.8.8.8:7080", "https://100.64.0.3:7080"))
	require.Empty(t, why)
	require.NotNil(t, cand)
	require.Equal(t, []string{"https://192.168.1.9:7080", "https://100.64.0.3:7080"}, cand.Addrs,
		"the public one is dropped, the local ones are kept in the order they came in")
}

// THE rule of this file: an announcement becomes a sighting only after this machine has connected and
// the identity the peer PROVED is the one the packet claimed. A packet that named one identity while
// another answers at that address is a lie, and the candidate goes.
func TestASightingNeedsTheProvenIdentityToMatchTheClaim(t *testing.T) {
	b := testBrowser(t)
	b.observe(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080"))
	require.Empty(t, b.store.sightings(), "an announcement is not a sighting")

	// The machine at that address proves a DIFFERENT identity than the one announced.
	b.store.setVerifier(func(context.Context, []string) (string, string, error) {
		return claimedID, "https://192.168.1.9:7080", nil
	})
	require.True(t, b.store.verifyDue(context.Background()))
	require.Empty(t, b.store.sightings(), "a claim that the handshake refutes is not a machine this one may list")
	require.False(t, b.store.verifyDue(context.Background()), "and it is dropped rather than retried forever")

	// The honest announcement, on the other hand, becomes one.
	b.observe(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7081"))
	b.store.setVerifier(func(context.Context, []string) (string, string, error) {
		return discoveredID, "https://192.168.1.9:7080", nil
	})
	require.True(t, b.store.verifyDue(context.Background()))
	sightings := b.store.sightings()
	require.Len(t, sightings, 1)
	require.Equal(t, discoveredID, sightings[0].Identity)
	require.Equal(t, []string{"https://192.168.1.9:7081"}, sightings[0].Addrs)
}

// A candidate that does not answer yet is ordinary — a node announces itself before it can serve —
// so it is kept for another try rather than forgotten, and the retry is not immediate.
func TestACandidateThatDoesNotAnswerYetIsRetried(t *testing.T) {
	b := testBrowser(t)
	b.observe(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080"))
	attempts := 0
	b.store.setVerifier(func(context.Context, []string) (string, string, error) {
		attempts++
		return "", "", errors.New("connection refused")
	})

	require.True(t, b.store.verifyDue(context.Background()))
	require.Equal(t, 1, attempts)
	require.False(t, b.store.verifyDue(context.Background()), "not immediately: the retry has a delay")
	require.Empty(t, b.store.sightings())

	b.store.now = func() time.Time { return time.Now().Add(discoveryRetry) }
	require.True(t, b.store.verifyDue(context.Background()))
	require.Equal(t, 2, attempts, "the machine was tried again after the delay")
}

// A candidate nobody can verify (a plaintext peer, something else entirely) is dropped rather than
// retried: there is no identity there to find, and asking again is a connection spent on a known
// answer.
func TestACandidateWithNoIdentityToProveIsDropped(t *testing.T) {
	b := testBrowser(t)
	b.observe(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080"))
	b.store.setVerifier(func(context.Context, []string) (string, string, error) { return "", "", nil })

	require.True(t, b.store.verifyDue(context.Background()))
	require.False(t, b.store.verifyDue(context.Background()))
}

// A machine that went quiet stops being a sighting — an hour after it was last seen — without
// anything having to sweep it.
func TestASightingExpiresAnHourAfterItWasLastSeen(t *testing.T) {
	b := testBrowser(t)
	at := time.Now()
	b.store.now = func() time.Time { return at }
	// The announcement is stamped by the same clock the browser reads, so "when it was seen" and
	// "when to try it" are the same instant rather than two.
	a := txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080")
	a.Seen = at
	b.observe(a)
	b.store.setVerifier(func(context.Context, []string) (string, string, error) {
		return discoveredID, "https://192.168.1.9:7080", nil
	})
	require.True(t, b.store.verifyDue(context.Background()))
	require.Len(t, b.store.sightings(), 1)

	b.store.now = func() time.Time { return at.Add(discoveryTTL - time.Minute) }
	require.Len(t, b.store.sightings(), 1, "still inside the window")

	b.store.now = func() time.Time { return at.Add(discoveryTTL + time.Minute) }
	require.Empty(t, b.store.sightings(), "outside it, the machine is no longer an entry")
}

// A machine that announces two addresses keeps both once it is confirmed, no matter which one
// answered: the announcement resolved to both, and both are places this machine may try. ONE pass
// verifies the whole machine — its addresses are tried in order inside that pass, not as separate
// machines queued behind each other.
func TestAConfirmedMachineKeepsEveryAddressItAnnounced(t *testing.T) {
	b := testBrowser(t)
	b.observe(txtAnnouncement("nas-dddd4444", discoveredID,
		"https://192.168.1.9:7080", "https://100.64.0.3:7080"))

	var asked []string
	b.store.setVerifier(func(_ context.Context, addrs []string) (string, string, error) {
		asked = append([]string(nil), addrs...)
		// The SECOND address is the one that answered, which is the case the return value exists
		// for: a peer with several addresses proves itself at one of them.
		return discoveredID, addrs[len(addrs)-1], nil
	})

	require.True(t, b.store.verifyDue(context.Background()))
	require.Equal(t, []string{"https://192.168.1.9:7080", "https://100.64.0.3:7080"}, asked,
		"one machine, one pass, its addresses in the order the announcement gave them")
	require.False(t, b.store.verifyDue(context.Background()), "and nothing is left queued")

	sightings := b.store.sightings()
	require.Len(t, sightings, 1)
	require.ElementsMatch(t, []string{"https://192.168.1.9:7080", "https://100.64.0.3:7080"}, sightings[0].Addrs)
}

// An announcement that resolves to an IPv6 LINK-LOCAL address only is not dialled: those addresses
// need a zone to mean anything, and nothing here can name one — so a dial can only ever fail, and on
// a machine with a dozen interfaces they used to stand in front of the address that works.
func TestAnAnnouncementWithOnlyUnreachableAddressesIsNotDialled(t *testing.T) {
	b := testBrowser(t)
	parsed := []net.IP{
		net.ParseIP("fe80::1"),
		net.ParseIP("fe80::e469:b3ff:fe28:8117"),
	}
	require.Empty(t, addressesFor(7080, parsed),
		"a link-local address without a zone is not an address this machine can dial")
	require.Empty(t, addressesFor(0, []net.IP{net.ParseIP("192.168.1.9")}),
		"and a port the kernel has not given yet names nothing either")

	// IPv4 first, then IPv6: the same machine here had thirteen IPv6 addresses and one IPv4 one, and
	// the order decides what is tried first.
	require.Equal(t, []string{"https://192.168.1.9:7080", "https://100.64.0.3:7080", "https://[fd7a:115c:a1e0::482c:ef4c]:7080"},
		addressesFor(7080, []net.IP{
			net.ParseIP("fe80::1"),
			net.ParseIP("fd7a:115c:a1e0::482c:ef4c"),
			net.ParseIP("192.168.1.9"),
			net.ParseIP("100.64.0.3"),
		}))

	// And an announcement whose surviving addresses are none becomes no candidate at all.
	cand, why := b.decide(txtAnnouncement("nas-dddd4444", discoveredID, "https://[fe80::1]:7080"))
	require.Nil(t, cand)
	require.NotEmpty(t, why)
}

// ── the directory over a discovery source ────────────────────────────────────

// One machine with a config line AND an announcement is ONE row, and the config's label is the one
// shown: the identity is the key, which is the whole reason the directory is keyed by it.
func TestAConfiguredMachineThatAnnouncesItselfIsOneEntry(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "nas", URL: "https://100.64.0.3:7080"}})
	require.NoError(t, err)
	dir.known = emptyPins(t)
	require.NoError(t, dir.known.confirm("nas", "https://100.64.0.3:7080", discoveredID))

	dir.discover = func() []discoveredPeer {
		return []discoveredPeer{{
			Identity: discoveredID,
			Addrs:    []string{"https://192.168.1.9:7080"},
			Source:   agentref.SourceMDNS,
			Seen:     time.Now(),
		}}
	}

	entries := dir.Entries()
	require.Len(t, entries, 1, "the same machine is one entry, however many ways this one knows it")
	require.Equal(t, "nas", entries[0].Name(), "and the config's label is the one a person reads")
	require.Equal(t, discoveredID, entries[0].Identity)
	require.Equal(t, []string{"https://192.168.1.9:7080", "https://100.64.0.3:7080"}, entries[0].Addrs,
		"what the machine says about itself is tried first, the configured address last")
	require.Equal(t, agentref.SourceConfigured|agentref.SourceMDNS, entries[0].Source,
		"both ways this machine knows it, because the row came by both")
	require.Equal(t, "配置 · mDNS", agentref.SourceWord(entries[0].Source),
		"and the panel says so in words a reader sees")
}

// A machine only discovery found expires; a configured one does not, however long it has been quiet
// — a person put that line there, and "it did not answer" is not a reason to hide it (design §三).
func TestOnlyDiscoveredMachinesExpire(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "nas", URL: "https://100.64.0.3:7080"}})
	require.NoError(t, err)
	dir.known = emptyPins(t)
	dir.discover = func() []discoveredPeer {
		return []discoveredPeer{
			{Identity: discoveredID, Addrs: []string{"https://192.168.1.9:7080"}, Source: agentref.SourceMDNS, Seen: time.Now()},
		}
	}

	found, ok := dir.byIdentity(discoveredID)
	require.True(t, ok, "a machine discovery found is an entry, keyed by the identity it proved")
	require.Equal(t, "dddd4444", found.Name(), "it has no label and no card, so its row reads as its short id")
	require.Equal(t, agentref.SourceMDNS, found.Source, "and the row says where it came from")

	// The source stops reporting it — the browser is what expires a sighting (node/discovery.go), and
	// the directory follows it: the configured line stays, the found machine goes.
	dir.discover = func() []discoveredPeer { return nil }
	entries := dir.Entries()
	require.Len(t, entries, 1)
	require.Equal(t, "nas", entries[0].Name())
}

// The address this machine has itself GOT THROUGH ON is tried before the rest of what a sighting came
// with, and all of it is behind what the peer says about itself and ahead of the person's line
// (design §三). It is the difference between evidence and a claim: a machine with a dozen announced
// addresses used to be tried in announcement order, so the one that answered last time could sit
// third and every dial spent two attempts rediscovering that.
func TestTheAddressThisMachineGotThroughOnIsTriedBeforeTheOthers(t *testing.T) {
	const third = "https://192.168.71.169:7080"
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "nas", URL: "https://100.64.0.3:7080"}})
	require.NoError(t, err)
	dir.known = emptyPins(t)
	require.NoError(t, dir.known.confirm("nas", "https://100.64.0.3:7080", discoveredID))

	dir.discover = func() []discoveredPeer {
		return []discoveredPeer{{
			Identity: discoveredID,
			// Announced in this order; the THIRD one is the address that answered the handshake.
			Addrs:  []string{lanAddr, lanAddr2, third},
			Served: third,
			Source: agentref.SourceMDNS,
			Seen:   time.Now(),
		}}
	}

	entries := dir.Entries()
	require.Len(t, entries, 1)
	require.Equal(t, []string{third, lanAddr, lanAddr2, "https://100.64.0.3:7080"}, entries[0].Addrs,
		"the address that answered first among what discovery brought, and the person's line last")
}

// A sighting that names no served address keeps the order it came with, and one that names one it did
// not list still gets it — both are states the store can hand over (a contact this machine made, and a
// candidate whose answering address was not the one first tried).
func TestASightingWithoutAServedAddressKeepsItsOrder(t *testing.T) {
	dir, err := newPeerDirectory(nil)
	require.NoError(t, err)
	dir.known = emptyPins(t)
	dir.discover = func() []discoveredPeer {
		return []discoveredPeer{
			{Identity: discoveredID, Addrs: []string{lanAddr, lanAddr2}, Source: agentref.SourceMDNS},
			{Identity: claimedID, Addrs: []string{lanAddr}, Served: lanAddr2},
		}
	}

	byID := make(map[string][]string, 2)
	for _, e := range dir.Entries() {
		byID[e.Identity] = e.Addrs
	}
	require.Equal(t, []string{lanAddr, lanAddr2}, byID[discoveredID], "nothing to reorder, so nothing moved")
	require.Equal(t, []string{lanAddr2, lanAddr}, byID[claimedID],
		"and an address it got through on is carried even when the sighting did not list it")
}

func TestTheDiscoveredHalfIsCapped(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "nas", URL: "https://100.64.0.3:7080"}})
	require.NoError(t, err)
	dir.known = emptyPins(t)

	now := time.Now()
	found := make([]discoveredPeer, 0, maxDiscoveredPeers+8)
	for i := range maxDiscoveredPeers + 8 {
		// distinct identities, oldest first, so the cap has an order to go by
		id := strings.Repeat("a", 30) + fmtTwoDigits(i)
		found = append(found, discoveredPeer{
			Identity: id,
			Addrs:    []string{"https://192.168.1.9:7080"},
			Seen:     now.Add(-time.Duration(maxDiscoveredPeers+8-i) * time.Minute),
		})
	}
	dir.discover = func() []discoveredPeer { return found }

	entries := dir.Entries()
	require.Len(t, entries, maxDiscoveredPeers+1, "the cap plus the configured line")
	require.True(t, entries[0].Source&agentref.SourceConfigured != 0, "the configured machine is still there")
	oldest := found[0].Identity
	for _, e := range entries {
		require.NotEqual(t, oldest, e.Identity, "the machine that announced longest ago is the one to lose")
	}
}

// fmtTwoDigits spells i as exactly two decimal digits, so the ids above are all 32 hex characters
// long (IsNodeID's shape) and differ from one another.
func fmtTwoDigits(i int) string {
	const digits = "0123456789"
	return string([]byte{digits[(i/10)%10], digits[i%10]})
}

// The default set: which interfaces discovery uses when the operator named none.
//
// What makes this worth a test rather than a line of code is what the library does with an empty
// list — it expands it back to "every multicast-capable interface" and then sends on them ONE AFTER
// ANOTHER, so one interface that cannot take the packet stalls the whole round in silence. So the
// rules that keep an interface out are the feature, and each one is pinned here on its own.
func TestLANShapeRefusesAnythingThatCannotCarryALANPacket(t *testing.T) {
	live := net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning
	cases := []struct {
		name  string
		iface net.Interface
		want  bool
	}{
		{"a NIC on a live network", net.Interface{Name: "en0", Flags: live}, true},
		{"a wireless NIC", net.Interface{Name: "wlan0", Flags: live}, true},
		// A NIC with only a link-local address is dropped by the ADDRESS rule, not by shape: shape is
		// about what the interface is, the address rule is about what it could be announced as.
		{"a NIC that got no lease", net.Interface{Name: "en8", Flags: live}, true},
		{"a link that is down", net.Interface{Name: "en1", Flags: net.FlagBroadcast | net.FlagMulticast}, false},
		{"an interface that cannot multicast", net.Interface{Name: "ppp0", Flags: net.FlagUp}, false},
		{"loopback", net.Interface{Name: "lo0", Flags: live | net.FlagLoopback}, false},
		{"a VPN tunnel", net.Interface{Name: "utun3", Flags: net.FlagUp | net.FlagPointToPoint | net.FlagMulticast}, false},
		{"a tun device", net.Interface{Name: "tun0", Flags: net.FlagUp | net.FlagPointToPoint | net.FlagMulticast}, false},
		// The bridges: ethernet-shaped, private addresses, and no flag to tell them from a NIC — which
		// is why the name list exists.
		{"a macOS VM bridge", net.Interface{Name: "bridge100", Flags: live}, false},
		{"a macOS VM link", net.Interface{Name: "vmenet0", Flags: live}, false},
		{"a docker bridge", net.Interface{Name: "docker0", Flags: live}, false},
		{"a libvirt bridge", net.Interface{Name: "virbr0", Flags: live}, false},
		{"a veth pair", net.Interface{Name: "veth1a2b3c", Flags: live}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, lanShape(tc.iface))
		})
	}
}

// hasLANAddress is the other half of the filter, and it is the same question the BROWSING side asks
// about an address it is handed: one a peer could be given and would accept.
func TestHasLANAddressAsksWhatAPeerCouldBeHanded(t *testing.T) {
	ipnet := func(s string) net.Addr {
		ip, network, err := net.ParseCIDR(s)
		require.NoError(t, err)
		network.IP = ip
		return network
	}
	cases := []struct {
		what string
		addr net.Addr
		want bool
	}{
		{"a private IPv4 address", ipnet("10.5.178.101/21"), true},
		{"a home-network IPv4 address", ipnet("192.168.1.5/24"), true},
		// Skipped by SHAPE rather than here: lo0 is refused as an interface, so what its address would
		// have scored never comes up. Reading it as diallable is honest — it is, from this machine.
		{"loopback", ipnet("127.0.0.1/8"), true},
		// Reserved, and still not handed to anybody: an IPv4 link-local address is a machine that never
		// got a lease, and an IPv6 one means nothing without a zone.
		{"an IPv4 address that never got a lease", ipnet("169.254.60.225/16"), false},
		{"an IPv6 link-local address", ipnet("fe80::1cee:e68:70f3:fd1/64"), false},
		// What an IPv6-only LAN looks like, and it counts: ULA is private, so a peer accepts it.
		{"a unique-local IPv6 address", ipnet("fd07:b51a:cc66::1/64"), true},
		{"a public IPv6 address", ipnet("2001:4860:4860::8888/128"), false},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			require.Equal(t, tc.want, hasLANAddress([]net.Addr{tc.addr}))
		})
	}
	require.False(t, hasLANAddress(nil), "an interface with no addresses has nothing to announce")
}

// The default set on the machine the test runs on: whatever it is, it is a FILTER. The assertions are
// deliberately about the property rather than about a name — a test that named en0 would be a test
// about the developer's laptop — and a machine with no LAN interface at all is a real answer, not a
// failure (it is the one that makes the browser refuse to start instead of sending into a void).
func TestTheDefaultInterfaceSetIsFilteredRatherThanEveryInterface(t *testing.T) {
	ifaces, err := resolveInterfaces(nil)
	if errors.Is(err, errNoLANInterface) {
		return
	}
	require.NoError(t, err)
	require.NotEmpty(t, ifaces)
	for _, iface := range ifaces {
		require.True(t, lanShape(iface), "%s is in the default set and must have the shape", iface.Name)
		addrs, err := iface.Addrs()
		require.NoError(t, err)
		require.True(t, hasLANAddress(addrs), "%s is in the default set and must carry an address a peer could be handed", iface.Name)
	}
}

// What a browse asks the library to carry is settled from the interfaces rather than chosen, and the
// reason is not tidiness: a type the interfaces cannot carry does not skip that type, it stops the
// browse altogether (see trafficFor). Each family is pinned on its own, and so is the refusal.
func TestTheTrafficTypesFollowWhatTheInterfacesCarry(t *testing.T) {
	ipnet := func(s string) net.Addr {
		ip, network, err := net.ParseCIDR(s)
		require.NoError(t, err)
		network.IP = ip
		return network
	}
	cases := []struct {
		what    string
		addrs   []net.Addr
		want    zeroconf.IPType
		wantErr error
	}{
		{"a Wi-Fi lease with IPv4 only", []net.Addr{ipnet("192.168.71.173/24")}, zeroconf.IPv4, nil},
		{"a LAN carrying both", []net.Addr{ipnet("192.168.71.173/24"), ipnet("fd07:b51a:cc66::1/64")},
			zeroconf.IPv4AndIPv6, nil},
		// The other shape of one family: an IPv6-only LAN must not be handed a doomed IPv4 join.
		{"a ULA-only LAN", []net.Addr{ipnet("fd07:b51a:cc66::1/64")}, zeroconf.IPv6, nil},
		{"an interface that never got a lease", []net.Addr{ipnet("169.254.60.225/16")}, zeroconf.IPv4, nil},
		{"an interface with no address at all", nil, 0, errNoBrowseTraffic},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			got, err := trafficForAddrs(tc.addrs)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// The whole-machine half of the same rule: whatever interfaces this machine has, the browse asks for
// exactly the families those interfaces hold — no more (a family nothing can carry kills the browse)
// and no less (discovery would be silently blind to one family).
func TestTheDefaultSetAsksOnlyForTrafficItCanCarry(t *testing.T) {
	ifaces, err := resolveInterfaces(nil)
	if errors.Is(err, errNoLANInterface) {
		return
	}
	require.NoError(t, err)

	traffic, err := trafficFor(ifaces)
	require.NoError(t, err)

	have4, have6 := false, false
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		require.NoError(t, err)
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.To4() != nil {
				have4 = true
			} else {
				have6 = true
			}
		}
	}
	require.Equal(t, have4, traffic&zeroconf.IPv4 != 0, "%v holds IPv4 addresses: %v", ifaces, addrsOf(ifaces))
	require.Equal(t, have6, traffic&zeroconf.IPv6 != 0, "%v holds IPv6 addresses: %v", ifaces, addrsOf(ifaces))
}

// Close must not depend on the library having closed the channel it was handed. A browse that fails
// in its OWN SETUP returns before a single entry was handed over and never closes that channel, so a
// browser that waited for the channel alone would make Close wait for ever — and Close is what an
// entry point calls on its way out, so that is a process that cannot quit (an IPv4-only Wi-Fi lease
// is enough to get there: see trafficFor).
//
// The interface named here exists on no machine, so the library cannot join a multicast group on it
// and both families fail — the same shape as the real one, without touching the real network.
func TestCloseReturnsEvenWhenBrowsingNeverStarted(t *testing.T) {
	nowhere := net.Interface{
		Name:  "tachi-test-nowhere",
		Index: 1 << 20,
		Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning,
	}
	b := browserOver([]net.Interface{nowhere}, nasID, nil, newSightingsStore(nil))
	b.Start(context.Background())

	closed := make(chan struct{})
	go func() {
		b.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close() waited for an entries channel that will never be closed")
	}
}

// addrsOf is what a failure message needs: the addresses behind a set of interfaces, so that "this
// machine has no IPv6 address" can be checked by eye against the assertion that just failed.
func addrsOf(ifaces []net.Interface) map[string][]string {
	out := make(map[string][]string, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			out[iface.Name] = append(out[iface.Name], addr.String())
		}
	}
	return out
}

// A machine ENTERING the directory is the one change nothing else would ever report: a view drawn
// from the directory has no way to notice, so the browser is the only one who can say it.
//
// And it says it once per machine, not once per announcement: announcements repeat every couple of
// minutes, the listener re-reads the directory (and asks the entry point for a probe), so reporting
// a repeat would turn discovery into a heartbeat. A machine that MOVED — one announcing an address
// this process had not learned — is a change again, because there is somewhere new to dial.
func TestAMachineIsReportedOnlyWhenItEntersTheDirectory(t *testing.T) {
	s := newSightingsStore(nil)
	entered := make(chan string, 8)
	s.onSighting = func() { entered <- "sighting" }

	cand := candidate{Identity: discoveredID, Addrs: []string{"https://192.168.71.168:9990"}}
	s.confirm(cand, "https://192.168.71.168:9990")
	assertSighting(t, entered, "the first sighting of a machine")
	require.Equal(t, "https://192.168.71.168:9990", s.recent(time.Hour)[0].Served,
		"and the address that answered is kept: it is what a vouching answer quotes (node/gossip.go)")

	// The same machine, announcing the same address: already listed, nothing to say.
	s.confirm(cand, "https://192.168.71.168:9990")
	assertNoSighting(t, entered)

	// The same machine at an address it had not named before: a new place to dial, so it is news.
	s.confirm(candidate{Identity: discoveredID, Addrs: []string{"https://192.168.71.169:9990"}},
		"https://192.168.71.169:9990")
	assertSighting(t, entered, "a machine that moved to an address this process had not learned")

	// And an announcement that brings back the FIRST address alongside the new one reports nothing
	// new either: the address is one it already knows, whatever order it arrives in.
	s.confirm(candidate{Identity: discoveredID, Addrs: []string{"https://192.168.71.169:9990",
		"https://192.168.71.168:9990"}}, "https://192.168.71.169:9990")
	assertNoSighting(t, entered)

	// A different machine is a different entry, whatever it announces.
	s.confirm(candidate{Identity: claimedID, Addrs: []string{"https://192.168.71.168:9990"}},
		"https://192.168.71.168:9990")
	assertSighting(t, entered, "a second machine")
}

func assertSighting(t *testing.T, seen chan string, what string) {
	t.Helper()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatalf("%s: the browser did not report it", what)
	}
}

func assertNoSighting(t *testing.T, seen chan string) {
	t.Helper()
	select {
	case <-seen:
		t.Fatal("the browser reported a machine that was already in the directory")
	case <-time.After(100 * time.Millisecond):
	}
}
