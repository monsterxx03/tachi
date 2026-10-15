package node

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
)

const (
	discoveredID = "dddd4444dddd4444dddd4444dddd4444"
	claimedID    = "eeee5555eeee5555eeee5555eeee5555"
)

// testBrowser is a browser with no interfaces and no socket at all: these tests drive its decisions
// directly (observe/decide/Sightings), so what this machine's network looks like must not enter into
// them — and newMDNSBrowser would refuse to build without a LAN interface, rightly (browserOver).
func testBrowser(t *testing.T) *mdnsBrowser {
	t.Helper()
	return browserOver(nil, nasID, nil)
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
	require.Empty(t, b.Sightings(), "an announcement is not a sighting")

	// The machine at that address proves a DIFFERENT identity than the one announced.
	b.setVerifier(func(context.Context, []string) (string, error) { return claimedID, nil })
	require.True(t, b.verifyDue(context.Background()))
	require.Empty(t, b.Sightings(), "a claim that the handshake refutes is not a machine this one may list")
	require.False(t, b.verifyDue(context.Background()), "and it is dropped rather than retried forever")

	// The honest announcement, on the other hand, becomes one.
	b.observe(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7081"))
	b.setVerifier(func(context.Context, []string) (string, error) { return discoveredID, nil })
	require.True(t, b.verifyDue(context.Background()))
	sightings := b.Sightings()
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
	b.setVerifier(func(context.Context, []string) (string, error) {
		attempts++
		return "", errors.New("connection refused")
	})

	require.True(t, b.verifyDue(context.Background()))
	require.Equal(t, 1, attempts)
	require.False(t, b.verifyDue(context.Background()), "not immediately: the retry has a delay")
	require.Empty(t, b.Sightings())

	b.now = func() time.Time { return time.Now().Add(discoveryRetry) }
	require.True(t, b.verifyDue(context.Background()))
	require.Equal(t, 2, attempts, "the machine was tried again after the delay")
}

// A candidate nobody can verify (a plaintext peer, something else entirely) is dropped rather than
// retried: there is no identity there to find, and asking again is a connection spent on a known
// answer.
func TestACandidateWithNoIdentityToProveIsDropped(t *testing.T) {
	b := testBrowser(t)
	b.observe(txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080"))
	b.setVerifier(func(context.Context, []string) (string, error) { return "", nil })

	require.True(t, b.verifyDue(context.Background()))
	require.False(t, b.verifyDue(context.Background()))
}

// A machine that went quiet stops being a sighting — an hour after it was last seen — without
// anything having to sweep it.
func TestASightingExpiresAnHourAfterItWasLastSeen(t *testing.T) {
	b := testBrowser(t)
	at := time.Now()
	b.now = func() time.Time { return at }
	// The announcement is stamped by the same clock the browser reads, so "when it was seen" and
	// "when to try it" are the same instant rather than two.
	a := txtAnnouncement("nas-dddd4444", discoveredID, "https://192.168.1.9:7080")
	a.Seen = at
	b.observe(a)
	b.setVerifier(func(context.Context, []string) (string, error) { return discoveredID, nil })
	require.True(t, b.verifyDue(context.Background()))
	require.Len(t, b.Sightings(), 1)

	b.now = func() time.Time { return at.Add(discoveryTTL - time.Minute) }
	require.Len(t, b.Sightings(), 1, "still inside the window")

	b.now = func() time.Time { return at.Add(discoveryTTL + time.Minute) }
	require.Empty(t, b.Sightings(), "outside it, the machine is no longer an entry")
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
	b.setVerifier(func(_ context.Context, addrs []string) (string, error) {
		asked = append([]string(nil), addrs...)
		return discoveredID, nil
	})

	require.True(t, b.verifyDue(context.Background()))
	require.Equal(t, []string{"https://192.168.1.9:7080", "https://100.64.0.3:7080"}, asked,
		"one machine, one pass, its addresses in the order the announcement gave them")
	require.False(t, b.verifyDue(context.Background()), "and nothing is left queued")

	sightings := b.Sightings()
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
			Seen:     time.Now(),
		}}
	}

	entries := dir.Entries()
	require.Len(t, entries, 1, "the same machine is one entry, however many ways this one knows it")
	require.Equal(t, "nas", entries[0].Name(), "and the config's label is the one a person reads")
	require.Equal(t, discoveredID, entries[0].Identity)
	require.Equal(t, []string{"https://192.168.1.9:7080", "https://100.64.0.3:7080"}, entries[0].Addrs,
		"what the machine says about itself is tried first, the configured address last")
	require.True(t, entries[0].Configured && entries[0].Discovered)
}

// A machine only discovery found expires; a configured one does not, however long it has been quiet
// — a person put that line there, and "it did not answer" is not a reason to hide it (design §三).
func TestOnlyDiscoveredMachinesExpire(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "nas", URL: "https://100.64.0.3:7080"}})
	require.NoError(t, err)
	dir.known = emptyPins(t)
	dir.discover = func() []discoveredPeer {
		return []discoveredPeer{
			{Identity: discoveredID, Addrs: []string{"https://192.168.1.9:7080"}, Seen: time.Now()},
		}
	}

	found, ok := dir.byIdentity(discoveredID)
	require.True(t, ok, "a machine discovery found is an entry, keyed by the identity it proved")
	require.Equal(t, "dddd4444", found.Name(), "it has no label and no card, so its row reads as its short id")
	require.True(t, found.Discovered && !found.Configured)

	// The source stops reporting it — the browser is what expires a sighting (node/discovery.go), and
	// the directory follows it: the configured line stays, the found machine goes.
	dir.discover = func() []discoveredPeer { return nil }
	entries := dir.Entries()
	require.Len(t, entries, 1)
	require.Equal(t, "nas", entries[0].Name())
}

// The discovered half is capped, because anyone on the network can multicast: a table that grew with
// a stranger's packet rate would have no bound at all. Configured entries are a person's, and are
// never counted against it.
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
	require.True(t, entries[0].Configured, "the configured machine is still there")
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
