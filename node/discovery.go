package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/netutil"
)

// Discovery: finding other Tachi nodes on the local network instead of being told about them.
//
// The browse side is DNS-SD over multicast DNS (`_tachi._tcp`), and what it produces is a stream of
// CLAIMS — anyone on the same network can send a multicast packet, and the instance name, the TXT
// records and the addresses in it are all text a stranger wrote. So nothing here decides anything a
// later step relies on:
//
//   - an announcement is filtered on nothing but its SHAPE (does it claim to be a Tachi node of this
//     API version, at an address that could be on this network), and a shape check is not trust: it
//     exists so that a stray `_tachi._tcp` from something else does not cost a connection attempt;
//   - a candidate becomes a SIGHTING only after this machine has connected to it over TLS and read a
//     card — which is where an identity comes from, because it is that handshake, not the packet,
//     that proves which key is answering there;
//   - the identity the handshake proves must be the one the packet claimed, or the packet was a lie
//     and the candidate is dropped with a line in the log.
//
// Design §5. What discovery therefore produces is one thing: a machine this machine may now TRY,
// with an identity it has already verified. Nothing about correctness depends on the multicast ever
// working — a network where it does not is a network where `peers:` is how nodes are introduced.

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

	// discoveryTTL is how long a machine that discovery alone found stays in the directory after the
	// last time it was seen. An hour is this machine's own choice rather than the announcement's TTL:
	// the announcement says how long its own records may be cached, not how long a person should keep
	// seeing a row for a node that went quiet (design §一.8).
	discoveryTTL = time.Hour

	// discoveryRetry is how long a candidate that could not be verified waits before it is tried
	// again. A node that is still starting up announces itself before it can answer, so a failure is
	// the ordinary first result rather than a reason to forget it.
	discoveryRetry = 30 * time.Second

	// discoveryTick paces the verification loop. Verification is a connection per candidate, and a
	// candidate is re-verified on a retry schedule rather than on every packet: a busy network would
	// otherwise turn announcements into connections at whatever rate they arrive.
	discoveryTick = 2 * time.Second
)

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

// candidate is an announcement that passed the checks which need no connection, waiting to be
// verified by one.
//
// It is keyed by IDENTITY and carries every address the announcement resolved to, because that is
// what verifying means: a machine has several addresses, one of them answers, and the others are
// not separate machines to be discovered. Treating each address as its own candidate is what made
// the first version of this take half a minute on a machine with a dozen IPv6 addresses — one dial
// per two-second tick, in map order, and the address that worked could be the twelfth.
type candidate struct {
	Identity string
	Addrs    []string
	// Next is when to try verifying it: a failure schedules a retry rather than a removal, because a
	// node announcing itself before it can answer is the ordinary case for a machine starting up.
	Next time.Time
}

// mdnsBrowser finds other Tachi nodes by browsing `_tachi._tcp` on the local network, and publishes
// the ones it has VERIFIED.
//
// One browser per process, owned by whoever owns the sending side (node/delegatorcache.go): it holds
// UDP sockets and goroutines, and the sending side is rebuilt every time the peer list changes
// while the network underneath it is the same network.
type mdnsBrowser struct {
	// self is this machine's own node id, and the one announcement that is never a peer.
	self string
	// ifaces is which interfaces to browse on, already resolved (resolveInterfaces: the configured
	// names, or the LAN set lanInterfaces picks when the operator named none). It is empty only in a
	// test that drives the browser's decisions directly — newMDNSBrowser refuses to build over an
	// empty set, because the library would read that as "every multicast-capable interface".
	ifaces []net.Interface
	log    *logger.Logger

	// now is the clock, injectable so a test can age a sighting without waiting an hour.
	now func() time.Time

	// verify connects to a candidate's addresses and answers with the identity the peer PROVED there,
	// which is the only thing that turns an announcement into a sighting. It is set by the sending
	// side as it is built (it needs that side's client and certificate), and a browser without one
	// publishes nothing: a candidate nobody can verify is a candidate nobody should believe.
	//
	// It takes the WHOLE address list, so that trying one machine is one pass with one deadline per
	// address (the client's own rule: short for the ones with a fallback behind them) rather than a
	// dial per tick in whatever order a map hands them over.
	verify func(ctx context.Context, addrs []string) (string, error)

	mu      sync.Mutex
	pending map[string]candidate      // identity → candidate, awaiting verification
	seen    map[string]discoveredPeer // identity → verified sighting, with when it was last seen

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// newMDNSBrowser builds a browser over the interfaces the config names — or, when it names none, over
// the ones lanInterfaces picks. Nothing starts until Start is called.
//
// It refuses to build over an empty set, and that is the one thing that must not be quietly ignored:
// the library reads an empty list as "every multicast-capable interface", which is exactly the set
// lanInterfaces exists to keep away from it.
func newMDNSBrowser(self string, cfg config.MdnsDiscoveryConfig, log *logger.Logger) (*mdnsBrowser, error) {
	ifaces, err := resolveInterfaces(cfg.Interfaces)
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, errNoLANInterface
	}
	return browserOver(ifaces, self, log), nil
}

// browserOver builds the browser over interfaces that are already resolved: what newMDNSBrowser does
// once the answer is in hand, and what a test that drives the browser's decisions directly wants (no
// network, and no opinion about the machine it runs on).
func browserOver(ifaces []net.Interface, self string, log *logger.Logger) *mdnsBrowser {
	if log == nil {
		log = logger.New("node")
	}
	return &mdnsBrowser{
		self:    self,
		ifaces:  ifaces,
		log:     log,
		now:     time.Now,
		pending: make(map[string]candidate),
		seen:    make(map[string]discoveredPeer),
		done:    make(chan struct{}),
	}
}

// errNoLANInterface is the answer when this machine has no interface that could carry a multicast
// packet to another machine. It is an error rather than an empty list because the library expands an
// empty list BACK to "every multicast-capable interface" — the very set that cannot work here.
var errNoLANInterface = errors.New("node: this machine has no interface on a local network (only " +
	"loopback, tunnels, VM bridges, or addresses that are link-local); name one in " +
	"node.discovery.mdns.interfaces")

// nonLANInterfacePrefixes are the name prefixes of interfaces that are never the LAN this machine is
// on: L2 bridges and the links virtual machines hang off. They are excluded BY NAME for one reason —
// the operating system hands them to us as ordinary ethernet-shaped interfaces, with the same flags
// and private addresses as a real NIC (macOS: the VM framework's `bridgeN`/`vmenetN`; Linux: the
// container and VM bridges below), so there is no fact about them to test. The alternative is
// lanInterfaces' own comment.
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
// what it picks is the subset that could carry a LAN packet (lanInterfaces), never "everything".
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
		return lanInterfaces()
	}
	return out, nil
}

// lanInterfaces is the set discovery uses when the operator named none: the interfaces that could
// carry a multicast packet between two machines on a LAN.
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
func lanInterfaces() ([]net.Interface, error) {
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
// the flags, and the name kinds the OS does not give a flag for. See lanInterfaces.
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

// setVerifier is how the sending side teaches the browser to check a candidate. Called with the
// newest client each time the sending side is rebuilt.
func (b *mdnsBrowser) setVerifier(verify func(ctx context.Context, addrs []string) (string, error)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.verify = verify
	b.mu.Unlock()
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
	go b.verifyLoop(ctx)
}

// Close stops browsing. The sightings go with it, and so do the rows: a machine this process found
// and can no longer see is not an entry it may keep listing.
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

// Sightings is the browser's answer for the directory: the machines it has VERIFIED and seen
// recently, in a stable order.
//
// It reads memory and nothing else — the directory's cheap path calls it on every keystroke — and it
// applies the TTL on read, so a machine that went quiet stops being an entry without anything having
// to sweep it.
func (b *mdnsBrowser) Sightings() []discoveredPeer {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := make([]discoveredPeer, 0, len(b.seen))
	for _, s := range b.seen {
		if now.Sub(s.Seen) > discoveryTTL {
			delete(b.seen, s.Identity)
			continue
		}
		out = append(out, s)
	}
	// Sorted so a rebuild of the directory produces the same list twice: Go's map order is random,
	// and rows that shuffle between keystrokes are unusable (node/peerdir.go).
	slices.SortFunc(out, func(a, b discoveredPeer) int { return strings.Compare(a.Identity, b.Identity) })
	return out
}

// browseLoop is the mDNS half: the library's Browse blocks until the context is done, and the
// channel it fills is closed when it returns.
func (b *mdnsBrowser) browseLoop(ctx context.Context) {
	defer close(b.done)
	entries := make(chan *zeroconf.ServiceEntry)
	browseDone := make(chan struct{})
	go func() {
		defer close(browseDone)
		for entry := range entries {
			b.observe(announcementOf(entry, b.now()))
		}
	}()

	opts := []zeroconf.ClientOption{zeroconf.SelectIPTraffic(zeroconf.IPv4AndIPv6)}
	if len(b.ifaces) > 0 {
		opts = append(opts, zeroconf.SelectIfaces(b.ifaces))
	}
	err := zeroconf.Browse(ctx, mdnsServiceType, mdnsDomain, entries, opts...)
	<-browseDone
	if err != nil && ctx.Err() == nil {
		// Said once, and not retried: the reasons for this are all about the network (multicast
		// blocked, the port taken, no interface), and none of them is fixed by asking again.
		b.log.Warn(ctx, "node: mDNS browsing stopped; this machine has to be told about peers in config",
			"error", err)
	}
	b.mu.Lock()
	clear(b.pending)
	clear(b.seen)
	b.mu.Unlock()
}

// verifyLoop turns candidates into sightings, one at a time.
//
// One at a time on purpose: each verification is a TLS connection to a machine that may be a
// stranger's, and the rate at which announcements arrive is chosen by whoever is on the network. A
// loop that took the next due candidate each tick bounds that rate at one connection per tick, per
// machine, whatever the network does.
func (b *mdnsBrowser) verifyLoop(ctx context.Context) {
	ticker := time.NewTicker(discoveryTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.verifyDue(ctx)
		}
	}
}

// verifyDue verifies the earliest candidate whose retry time has come, and reports whether it did.
func (b *mdnsBrowser) verifyDue(ctx context.Context) bool {
	b.mu.Lock()
	verify := b.verify
	now := b.now()
	id, cand, found := "", candidate{}, false
	for machine, c := range b.pending {
		if c.Next.After(now) {
			continue
		}
		if !found || c.Next.Before(cand.Next) {
			id, cand, found = machine, c, true
		}
	}
	b.mu.Unlock()
	if !found || verify == nil {
		return false
	}

	// The connection happens with no lock held: it is a network round trip, and the rule this
	// codebase follows is one lock at a time.
	proved, err := verify(ctx, cand.Addrs)
	switch {
	case err != nil:
		b.reschedule(id, cand, err)
	case proved == "":
		// No identity to prove: nothing at those addresses is serving a TLS surface this machine can
		// read (it may be plaintext, or something else entirely). Not an error worth a line per
		// attempt — it is a candidate that will never verify, so it is dropped rather than retried for
		// ever.
		b.forget(id)
	case proved != cand.Identity:
		// The packet named one identity and the machine at those addresses proved another: the
		// announcement lied. Said out loud, because it is the one case where something on the network
		// deliberately misled this machine.
		b.log.Warn(ctx, "node: a discovery announcement named an identity the machine at that address "+
			"did not prove; dropping the candidate", "claimed", cand.Identity, "proved", proved,
			"addrs", cand.Addrs)
		b.forget(id)
	default:
		b.confirm(cand)
	}
	return true
}

// observe decides what to do with one announcement, without connecting to anything.
func (b *mdnsBrowser) observe(a announcement) {
	cand, why := b.decide(a)
	if cand == nil {
		if why != "" {
			b.log.Debug(context.Background(), "node: ignoring a discovery announcement", "why", why,
				"instance", a.Instance)
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	next := candidate{Identity: cand.Identity, Addrs: cand.Addrs, Next: a.Seen}
	if prev, ok := b.pending[cand.Identity]; ok {
		// Already waiting: keep the earliest retry (so a machine that announces once a second does not
		// keep pushing its own verification out of reach) and the addresses learned since, because an
		// announcement is a claim about where a machine IS and the next one may be the one that works.
		if prev.Next.Before(next.Next) {
			next.Next = prev.Next
		}
		next.Addrs = prependAddrs(next.Addrs, prev.Addrs)
	}
	b.pending[cand.Identity] = next
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
	return &candidate{Identity: id, Addrs: local, Next: a.Seen}, ""
}

// reschedule keeps a candidate that could not be verified, for another attempt later.
func (b *mdnsBrowser) reschedule(id string, cand candidate, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.pending[id]; !ok {
		return // forgotten while the attempt was in flight
	}
	cand.Next = b.now().Add(discoveryRetry)
	b.pending[id] = cand
	b.log.Debug(context.Background(), "node: a discovered node did not answer yet; will try again",
		"node_id", id, "addrs", cand.Addrs, "error", err.Error())
}

// forget drops one candidate for good: nothing at those addresses will ever verify, so asking again
// is a connection spent on a known answer.
func (b *mdnsBrowser) forget(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// confirm records a verified machine: it entered the directory, and every address its announcement
// resolved to travels with it, because they are all places this machine may try (design §三).
func (b *mdnsBrowser) confirm(cand candidate) {
	b.mu.Lock()
	next := discoveredPeer{Identity: cand.Identity, Addrs: append([]string(nil), cand.Addrs...), Seen: b.now()}
	if prev, ok := b.seen[cand.Identity]; ok {
		// An address once learned is not dropped, only reordered: what said "I am here" most recently
		// goes first (the same rule the directory applies to a card, node/peerdir.go).
		next.Addrs = prependAddrs(next.Addrs, prev.Addrs)
	}
	b.seen[cand.Identity] = next
	delete(b.pending, cand.Identity)
	b.mu.Unlock()

	b.log.Info(context.Background(), "node: discovered a node and proved its identity",
		"node_id", cand.Identity, "addrs", cand.Addrs)
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
