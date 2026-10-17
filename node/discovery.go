package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/netutil"
)

// Discovery over multicast DNS: finding other Tachi nodes on the local network instead of being told
// about them.
//
// This file is the mDNS HALF of it — browsing (`_tachi._tcp`), and the checks an announcement gets
// before it costs a connection. What the browse produces is a stream of CLAIMS: anyone on the same
// network can send a multicast packet, and the instance name, the TXT records and the addresses in it
// are all text a stranger wrote. So a claim is filtered on nothing but its SHAPE (does it claim to be
// a Tachi node of this API version, at an address that could be on this network), and a shape check
// is not trust: it exists so that a stray `_tachi._tcp` from something else does not cost a
// connection attempt.
//
// What happens to a claim after that is not this file's business. It goes to the sightings store
// (node/sightings.go), which connects ONCE, learns the identity the handshake PROVES — the packet's
// claim about it is only what the connection is checked against, and a disagreement is a lie worth a
// line in the log — and then publishes the machine as a sighting. The store is also where the second
// kind of claim arrives (a peer's word, with gossip): one queue, one verification, one answer to
// "which machines has this process proved". Design §5.
//
// Nothing about correctness depends on the multicast ever working — a network where it does not is a
// network where `peers:` is how nodes are introduced.

const (
	// The service type this program advertises and browses. The `_tcp` suffix is what says "we speak a
	// stream protocol here", and the domain ends in a DOT because everything built from it has to be a
	// fully-qualified name: the library joins these two with dots to make the record names, and a name
	// without the final dot is refused by the DNS library underneath it ("domain must be fully
	// qualified"). The library's own default is `local` and it fails the same way — which is how this
	// was found: the smoke test in node/advertise_test.go browses for real.
	mdnsServiceType = "_tachi._tcp"
	mdnsDomain      = "local."

	// The TXT records, and they are deliberately three tiny facts rather than a description of the
	// node. A description would be free text on a network where anyone can send one — and it would be
	// a second source of the same claim the Agent Card already makes, over a channel with no way to
	// check it (design §4.3: 身份提示仍须连接后验证).
	mdnsTXTIdentity = "id"     // 32 hex characters: the node id this machine claims to have
	mdnsTXTAPIVer   = "v"      // the API version, so a node of another vintage is not dialled
	mdnsTXTScheme   = "scheme" // always https; a peer that could serve plaintext is not one
	mdnsScheme      = "https"  // this program's only scheme for a peer (node/peers.go checkPeerURL)

)

// The numbers that pace and expire discovery (discoveryTTL, discoveryRetry, discoveryTick,
// contactTTL) live with the store that spends them: node/sightings.go.

// announcement is one DNS-SD announcement as it ARRIVED. Every field is untrusted text from whoever
// sent the packet, and the shape of this type says so: nothing here is used without another check.
type announcement struct {
	// Instance is the announced instance name (`<machine>-<short id>`, by construction on the sending
	// side). It is for a log line and nothing else: it is a string anyone can choose, right down to
	// imitating another machine's.
	Instance string
	TXT      map[string]string
	// Addrs are the addresses the announcement resolved to (SRV + A/AAAA), already spelled as URLs.
	Addrs []string
	Seen  time.Time
}

// mdnsBrowser finds other Tachi nodes by browsing `_tachi._tcp` on the local network, and hands what
// it sees to the store that decides what is real (node/sightings.go).
//
// One browser per process, owned by whoever owns the sending side (node/delegatorcache.go): it holds
// UDP sockets and goroutines, and the sending side is rebuilt every time the peer list changes
// while the network underneath it is the same network.
//
// It is a PRODUCER and nothing else. Everything it can see is a claim — a multicast packet anyone on
// the network could have sent — so its whole job is to decide "this is worth one connection" and hand
// the candidate over. The connection, and the only write to the confirmed set, happen in the store.
type mdnsBrowser struct {
	// self is this machine's own node id, and the one announcement that is never a peer.
	self string
	// ifaces is which interfaces to browse on, already resolved (resolveInterfaces: the configured
	// names, or the LAN set LANInterfaces picks when the operator named none). It is empty only in a
	// test that drives the browser's decisions directly — newMDNSBrowser refuses to build over an
	// empty set, because the library would read that as "every multicast-capable interface".
	ifaces []net.Interface
	// traffic is which traffic types the library is asked to carry, and it is settled from the
	// addresses these interfaces actually hold (trafficFor) rather than picked: asking for a family
	// they cannot carry does not skip that family, it stops the browse altogether.
	traffic zeroconf.IPType
	log     *logger.Logger

	// store is what this browser feeds, and the only thing it does with what it sees: a candidate goes
	// in there, and is believed only after a connection has proved who is answering at one of its
	// addresses. The store is NOT the browser's — it outlives it and has other sources
	// (node/delegatorcache.go) — and it carries the discovery side's one clock, which is why
	// browseLoop asks IT what time it is.
	store *sightingsStore

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// newMDNSBrowser builds a browser over the interfaces the config names — or, when it names none, over
// the ones LANInterfaces picks. Nothing starts until Start is called.
//
// It refuses to build over an empty set, and that is the one thing that must not be quietly ignored:
// the library reads an empty list as "every multicast-capable interface", which is exactly the set
// LANInterfaces exists to keep away from it.
func newMDNSBrowser(self string, cfg config.MdnsDiscoveryConfig, log *logger.Logger, store *sightingsStore) (*mdnsBrowser, error) {
	ifaces, err := resolveInterfaces(cfg.Interfaces)
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, errNoLANInterface
	}
	// What to ask the library for is settled HERE, from the interfaces themselves, and it can refuse:
	// an interface carrying no address of either family is one nothing can be browsed over (trafficFor).
	traffic, err := trafficFor(ifaces)
	if err != nil {
		return nil, err
	}
	b := browserOver(ifaces, self, log, store)
	b.traffic = traffic
	return b, nil
}

// browserOver builds the browser over interfaces that are already resolved: what newMDNSBrowser does
// once the answer is in hand, and what a test that drives the browser's decisions directly wants (no
// network, and no opinion about the machine it runs on).
//
// The traffic types default to the library's own, and that is only because such a test never starts
// the browser: newMDNSBrowser replaces the value with the one the interfaces justify (trafficFor).
func browserOver(ifaces []net.Interface, self string, log *logger.Logger, store *sightingsStore) *mdnsBrowser {
	if log == nil {
		log = logger.New("node")
	}
	return &mdnsBrowser{
		self:    self,
		ifaces:  ifaces,
		traffic: zeroconf.IPv4AndIPv6,
		log:     log,
		store:   store,
		done:    make(chan struct{}),
	}
}

// errNoLANInterface is the answer when this machine has no interface that could carry a multicast
// packet to another machine. It is an error rather than an empty list because the library expands an
// empty list BACK to "every multicast-capable interface" — the very set that cannot work here.
var errNoLANInterface = errors.New("node: this machine has no interface on a local network (only " +
	"loopback, tunnels, VM bridges, or addresses that are link-local); name one in " +
	"node.discovery.mdns.interfaces")

// errNoBrowseTraffic is the answer when the interfaces discovery was given carry no address of either
// family, which is a browse with nothing to send it over.
var errNoBrowseTraffic = errors.New("node: none of the interfaces discovery may browse on carries an " +
	"IPv4 or an IPv6 address")

// trafficFor is which traffic types a browse over these interfaces may ask the library for.
//
// It is not a preference, and the reason is in the library's own client: it joins the multicast group
// of EVERY type it is asked for, on the interfaces it is given, and gives up on the first type it
// cannot join anywhere — returning before it ever fills the entries channel it was handed (a channel
// whose other end browseLoop has to survive: see the drain there). So asking for IPv6 on interfaces
// that hold no IPv6 address does not skip IPv6, it buys NO browse at all — a browse that never runs,
// and whose reason only survives if browseLoop says it before it waits. An IPv4-only Wi-Fi lease is
// exactly that case, and it is worth naming because the answer is not "give up on discovery": it is
// to ask for what the interfaces actually carry.
//
// Nothing is a preference the other way either: an interface with an address of both families gets
// both, and one with only an IPv6 address (a ULA LAN) gets IPv6 alone rather than a doomed IPv4 join.
func trafficFor(ifaces []net.Interface) (zeroconf.IPType, error) {
	var addrs []net.Addr
	for _, iface := range ifaces {
		// An interface whose addresses cannot be read is one nothing can be browsed over, the same
		// answer LANInterfaces gives it.
		if a, err := iface.Addrs(); err == nil {
			addrs = append(addrs, a...)
		}
	}
	return trafficForAddrs(addrs)
}

// trafficForAddrs is the half of trafficFor that holds the rule, and it is separate so that the rule
// can be told about a machine's addresses without running on a machine (the default set is a fact
// about the network the test happens to sit on).
func trafficForAddrs(addrs []net.Addr) (zeroconf.IPType, error) {
	var traffic zeroconf.IPType
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.To4() != nil {
			traffic |= zeroconf.IPv4
		} else {
			traffic |= zeroconf.IPv6
		}
	}
	if traffic == 0 {
		return 0, errNoBrowseTraffic
	}
	return traffic, nil
}

// nonLANInterfacePrefixes are the name prefixes of interfaces that are never the LAN this machine is
// on: L2 bridges and the links virtual machines hang off. They are excluded BY NAME for one reason —
// the operating system hands them to us as ordinary ethernet-shaped interfaces, with the same flags
// and private addresses as a real NIC (macOS: the VM framework's `bridgeN`/`vmenetN`; Linux: the
// container and VM bridges below), so there is no fact about them to test. The alternative is
// LANInterfaces' own comment.
var nonLANInterfacePrefixes = []string{
	"bridge", "vmenet", "vmnet", // macOS VM networking
	"docker", "br-", "virbr", "veth", "vmbr", "cni", "flannel", // Linux containers and VMs
}

// resolveInterfaces turns configured names into interfaces, refusing a name that is not a network
// interface on this machine.
//
// Refusing rather than ignoring is the point: a name that does not resolve means the operator
// believes discovery is running somewhere it is not, and a browser that silently fell back to every
// interface would publish this machine's name onto a network the config says to keep it off.
//
// An EMPTY list is a different thing from a chosen one: nobody picked, so this function picks — and
// what it picks is the subset that could carry a LAN packet (LANInterfaces), never "everything".
func resolveInterfaces(names []string) ([]net.Interface, error) {
	var out []net.Interface
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return nil, fmt.Errorf("node: discovery.mdns.interfaces names %q, which is not an interface "+
				"on this machine: %w", name, err)
		}
		out = append(out, *iface)
	}
	if len(out) == 0 {
		return LANInterfaces()
	}
	return out, nil
}

// LANInterfaces is the set discovery uses when the operator named none: the interfaces that could
// carry a multicast packet between two machines on a LAN.
//
// It is exported for the one caller that has to ask the SAME question this set is about: the
// integration spec probes multicast over these interfaces and skips when the machine cannot carry it,
// and a probe over a set this machine would never browse on answers a question nobody asked
// (itest/run/discovery_test.go).
//
// It is deliberately NOT "every multicast-capable interface", which is what the library picks for
// itself from an empty list — and on a machine with VPN tunnels and virtual machines that set is a
// trap, because the library sends on it ONE INTERFACE AFTER ANOTHER (zeroconf's per-interface
// `WriteTo` loop). A single interface that cannot take the packet — a tunnel, a VM bridge, a NIC
// that never got a lease — stalls that round for good: the announcement never goes out, the query is
// never sent, and nothing anywhere says so. Three of the four rules are about the interface itself:
//
//   - up, multicast-capable, and not loopback. A down link carries nothing, an interface without the
//     multicast flag cannot carry this at all, and loopback is this machine talking to itself (two
//     Tachis on one machine coordinate through the node lock, not through mDNS);
//   - not point-to-point: that is what a VPN tunnel is, and announcing on one publishes this
//     machine's name onto somebody else's network;
//   - it has an address a peer could actually be handed (hasLANAddress). An interface with nothing but
//     link-local addresses — an IPv4 one that never got a lease, or an IPv6 one that means nothing
//     without a zone — has nothing to put in an announcement.
//
// L2 bridges are the fourth case and the only one that needs a name (nonLANInterfacePrefixes): the OS
// gives them no flag to test, and they are both unable to carry this and wrong to publish onto. Asking
// each interface whether it can actually take a multicast packet — a fact rather than a name — would
// catch anything these rules miss, and it costs a packet and a timeout on every machine, including the
// ones where the default is already right. Worth revisiting the day a broken interface shows up that
// these rules miss.
func LANInterfaces() ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("node: cannot list this machine's interfaces: %w", err)
	}
	out := make([]net.Interface, 0, len(all))
	for _, iface := range all {
		if !lanShape(iface) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			// An interface whose addresses cannot be read is one nothing can be announced on.
			continue
		}
		if !hasLANAddress(addrs) {
			continue
		}
		out = append(out, iface)
	}
	if len(out) == 0 {
		return nil, errNoLANInterface
	}
	return out, nil
}

// lanShape reports whether an interface has the shape of one that could carry a LAN multicast packet:
// the flags, and the name kinds the OS does not give a flag for. See LANInterfaces.
func lanShape(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 ||
		iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
		return false
	}
	for _, prefix := range nonLANInterfacePrefixes {
		if strings.HasPrefix(iface.Name, prefix) {
			return false
		}
	}
	return true
}

// hasLANAddress reports whether an interface carries at least one address a peer could actually be
// handed. The test is the same pair the BROWSING side applies to an address it is given — is it on a
// local network (netutil.IsReservedIP) and could it be dialled (isDialableIP) — read from the other
// end, because an announcement only makes sense if the address in it is one the reader will accept.
//
// It is what keeps an interface with nothing but link-local addresses out of the default set: an IPv6
// link-local address means nothing without a zone nothing can name, and an IPv4 one (169.254/16) is a
// machine that never got a lease.
func hasLANAddress(addrs []net.Addr) bool {
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if netutil.IsReservedIP(ipnet.IP) && isDialableIP(ipnet.IP) {
			return true
		}
	}
	return false
}

// Start runs the browser until the context is canceled or Close is called.
//
// It never fails the caller: a network where mDNS cannot be used at all (no multicast-capable
// interface, a port taken by the system responder) is a real deployment, and the answer to it is
// that this machine has to be told about peers — which is exactly the state before discovery
// existed. The reason is logged once.
func (b *mdnsBrowser) Start(parent context.Context) {
	if b == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	b.cancel = cancel
	go b.browseLoop(ctx)
}

// Close stops browsing: the socket and the goroutine go away.
//
// What it does NOT do is drop the sightings — they are the store's, and the store outlives this
// browser (it has other sources). Whoever brought both up closes both (node/delegatorcache.go).
func (b *mdnsBrowser) Close() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		<-b.done
	})
}

// browseLoop is the mDNS half: the library's Browse blocks until the context is done, and reports a
// browse that never got going as an error instead.
//
// The one thing to know about it is that Browse closes the entries channel ONLY on its way out of a
// browse that ran: a failure in its own setup returns before anything was ever handed over, leaving
// the channel open for good. So the drain below leaves on the CONTEXT as well as on the channel —
// without that, a browser whose multicast setup failed would leave Close waiting for a channel nobody
// will ever close, and Close is what an entry point calls on its way out. That is a process that
// cannot quit, which is what this shape cost once.
func (b *mdnsBrowser) browseLoop(ctx context.Context) {
	defer close(b.done)
	entries := make(chan *zeroconf.ServiceEntry)
	browseDone := make(chan struct{})
	go func() {
		defer close(browseDone)
		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-entries:
				if !ok {
					return
				}
				// The STORE's clock, not one of the browser's: a sighting's age is read against that
				// one, and two clocks would be two answers to "how old is this" (node/sightings.go).
				b.observe(announcementOf(entry, b.store.clock()))
			}
		}
	}()

	opts := []zeroconf.ClientOption{zeroconf.SelectIPTraffic(b.traffic)}
	if len(b.ifaces) > 0 {
		opts = append(opts, zeroconf.SelectIfaces(b.ifaces))
	}
	err := zeroconf.Browse(ctx, mdnsServiceType, mdnsDomain, entries, opts...)
	// Said BEFORE the wait, because the failure that makes that wait long is this one — its reason is
	// known here and nowhere else. And said once, not retried: the reasons for it are all about the
	// network (multicast blocked, the port taken, no interface), and none is fixed by asking again.
	if err != nil && ctx.Err() == nil {
		b.log.Warn(ctx, "node: mDNS browsing stopped; this machine has to be told about peers in config",
			"error", err)
	}
	<-browseDone
}

// observe applies the checks that need no connection to one announcement, and hands what survives to
// the store as a candidate.
//
// It decides one thing and no more: that this is worth ONE connection. What comes back from that
// connection is what says whether the claim was true (node/sightings.go verifyDue), and this file
// never learns the answer — a producer here has no opinion about what is real.
func (b *mdnsBrowser) observe(a announcement) {
	cand, why := b.decide(a)
	if cand == nil {
		if why != "" {
			b.log.Debug(context.Background(), "node: ignoring a discovery announcement", "why", why,
				"instance", a.Instance)
		}
		return
	}
	b.store.feed(*cand)
}

// decide applies every check that needs no connection, and answers with a candidate or the reason
// there is none.
//
// The checks, and none of them is trust:
//
//  1. the TXT records have the shape this program publishes (an identity, this API version, https).
//     A stray `_tachi._tcp` from something else is dropped here without a connection attempt;
//  2. it is not this machine (by identity — the one field a liar cannot fake, since a lie only gets
//     as far as the handshake that refutes it);
//  3. it resolved to an address that could be on a local network. A public address in a multicast
//     announcement is a machine asking to be dialled from wherever, and this is not that: the design
//     drops it (design §5 预筛).
func (b *mdnsBrowser) decide(a announcement) (*candidate, string) {
	id := strings.TrimSpace(a.TXT[mdnsTXTIdentity])
	if !IsNodeID(id) {
		return nil, "it does not carry a node id"
	}
	if v := strings.TrimSpace(a.TXT[mdnsTXTAPIVer]); v != strconv.Itoa(APIVersion) {
		return nil, fmt.Sprintf("it speaks API version %q, this machine speaks %d", v, APIVersion)
	}
	if got := strings.TrimSpace(a.TXT[mdnsTXTScheme]); got != mdnsScheme {
		return nil, fmt.Sprintf("it announces scheme %q, and a peer must be %s", got, mdnsScheme)
	}
	if id == b.self {
		return nil, ""
	}
	local := make([]string, 0, len(a.Addrs))
	for _, addr := range a.Addrs {
		if isLocalAddr(addr) {
			local = append(local, addr)
			continue
		}
		b.log.Debug(context.Background(), "node: ignoring a discovery address that is not on a local "+
			"network", "addr", addr)
	}
	if len(local) == 0 {
		return nil, "it resolved to no address on a local network"
	}
	// Source is what makes this candidate different from one a peer named, and the difference is
	// evidence: this machine heard the machine itself, so it is preferred when two candidates are due
	// at once (node/sightings.go dueFirst) and the row says mDNS rather than gossip.
	return &candidate{Identity: id, Addrs: local, Next: a.Seen, Source: agentref.SourceMDNS}, ""
}

// announcementOf turns one library entry into the shape this file works in.
//
// The addresses are spelled here rather than taken from the entry as-is, and that is where a missing
// `scheme` matters: this program's peers are https (node/peers.go checkPeerURL), so an announcement
// that says otherwise was already filtered out and what is built here is a URL this machine could
// actually dial.
func announcementOf(entry *zeroconf.ServiceEntry, now time.Time) announcement {
	if entry == nil {
		return announcement{Seen: now}
	}
	txt := make(map[string]string, len(entry.Text))
	for _, kv := range entry.Text {
		if key, value, ok := strings.Cut(kv, "="); ok {
			txt[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	ips := make([]net.IP, 0, len(entry.AddrIPv4)+len(entry.AddrIPv6))
	ips = append(ips, entry.AddrIPv4...)
	ips = append(ips, entry.AddrIPv6...)
	return announcement{
		Instance: entry.Instance,
		TXT:      txt,
		Addrs:    addressesFor(entry.Port, ips),
		Seen:     now,
	}
}

// addressesFor spells the addresses an announcement resolved to, as URLs.
//
// It is deliberately the RESOLVED addresses (the SRV target's A/AAAA records) and never the hostname
// the announcement carried: Go's resolver does not speak mDNS, so a `<name>.local` would resolve to
// nothing or, worse, to something else entirely (design §5).
//
// Two rules about what is worth spelling out, and both come from a machine that answered this and
// took half a minute to be found:
//
//   - IPv6 LINK-LOCAL addresses are left out. They need a zone (`fe80::1%en0`) to mean anything, and
//     nothing here can name one — so dialling them can only ever fail, and on a machine with a dozen
//     interfaces they are a dozen failures standing in front of the address that works.
//   - IPv4 comes first, because it is the one that usually works: the same machine here had thirteen
//     IPv6 addresses and one IPv4 one, and the order decides what is tried first.
func addressesFor(port int, ips []net.IP) []string {
	if port <= 0 {
		return nil
	}
	out := make([]string, 0, len(ips))
	for _, wantV4 := range []bool{true, false} {
		for _, ip := range ips {
			if !isDialableIP(ip) {
				continue
			}
			if (ip.To4() != nil) != wantV4 {
				continue
			}
			out = append(out, mdnsScheme+"://"+net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		}
	}
	return out
}

// isDialableIP reports whether this machine could ever reach a machine at this address, which is the
// question BOTH ends of this file ask: the announcement's records are turned into candidates through
// it, and an address that arrived some other way (a hand-built announcement, a record that changed
// under us) is checked against it again before anything is dialled. One rule, one place — the version
// of this that had the two halves filtering differently dialled the addresses the other half had
// already decided were useless.
func isDialableIP(ip net.IP) bool {
	switch {
	case ip == nil, ip.IsUnspecified():
		return false
	case ip.IsLinkLocalUnicast():
		// An IPv6 link-local address only means something with a zone (`fe80::1%en0`), and there is no
		// way to name one from here: every dial would fail. An IPv4 link-local address (169.254/16) is
		// a machine that could not get a lease, and is not one to go looking for either.
		return false
	}
	return true
}

// isLocalAddr reports whether an address is one this machine will consider dialling, which is the
// same question as "could this machine be on a local network".
//
// The test is on the IP and not on the name: netutil.IsReservedIP covers private ranges, link-local
// and loopback, so a public address in a multicast announcement — a machine asking to be dialled from
// wherever, or an announcement built to make this one connect outwards — is not considered. And an
// address that is reserved but unreachable (a link-local IPv6 without a zone) is not one either,
// which is the other half of the same rule (isDialableIP).
func isLocalAddr(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return netutil.IsReservedIP(ip) && isDialableIP(ip)
}
