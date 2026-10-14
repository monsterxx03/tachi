package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/libp2p/zeroconf/v2"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Announcing this machine, so that other nodes can find it without being told about it.
//
// This is the other half of node/discovery.go, and the two are one arrangement read from two ends:
// what a browser there treats as a CLAIM is what a machine here publishes — three small facts, about
// the identity (which is what a reader checks against the certificate the node proves) and about the
// protocol (the version and the scheme, so a reader that could not talk to this node does not try).
//
// It happens in the process that HOLDS the node service and nowhere else, and that is not a
// preference: only that process has a remote surface, so only it has anything to announce, and a
// process announcing one it is not serving would be sending peers to a door with nobody behind it.
//
// What it deliberately does NOT announce is the address. In DNS-SD that comes from the records
// themselves — this machine's interfaces and the port it bound — and whatever a reader ends up with
// is checked the same way every other candidate is: by connecting and reading a certificate. A
// machine behind a port forwarder is reachable at an address its local network never sees, and the
// audience of a multicast announcement is the local network, which is exactly where the local
// addresses are right.
type mdnsAdvertiser struct {
	server   *zeroconf.Server
	log      *logger.Logger
	instance string
}

// startAdvertising announces this node on the local network, and returns the handle that withdraws it.
//
// boundAddr is the address the remote surface actually bound, which is where the port comes from: the
// announcement has to name the port this machine answers on, not the one somebody wished it answered
// on.
func startAdvertising(o Options, boundAddr string, identity *Identity) (*mdnsAdvertiser, error) {
	if identity == nil || identity.NodeID == "" {
		return nil, errors.New("node: cannot announce a node that has no identity")
	}
	_, portStr, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return nil, fmt.Errorf("node: cannot announce this node: %q is not an address it is bound to: %w",
			boundAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("node: cannot announce this node: %q names no port to announce", boundAddr)
	}
	// Resolved BEFORE anything is announced, so a mistyped interface refuses the start rather than
	// silently announcing on every network this machine is on — which is the one outcome the setting
	// exists to prevent.
	ifaces, err := resolveInterfaces(o.AdvertiseInterfaces)
	if err != nil {
		return nil, err
	}
	log := o.Logger
	if log == nil {
		log = logger.New("node")
	}

	instance := instanceName(o.Name, identity.NodeID)
	server, err := zeroconf.Register(instance, mdnsServiceType, mdnsDomain, port,
		advertiseTXT(identity.NodeID), ifaces, zeroconf.TTL(uint32(announceTTL.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("node: cannot announce this node: %w", err)
	}
	log.Info(context.Background(), "node: announcing this node on the local network",
		"instance", instance, "port", port, "node_id", identity.NodeID)
	return &mdnsAdvertiser{server: server, log: log, instance: instance}, nil
}

// Close withdraws the announcement.
//
// Withdrawal is a goodbye: the library re-sends the records with a zero TTL, so a machine that is
// going away stops being a row on someone else's list at that moment instead of when its records
// would have expired. It is called BEFORE the listeners are closed — the order the design fixes —
// because a peer that believed the announcement would otherwise find a door shut, with nobody having
// said it was going to be.
func (a *mdnsAdvertiser) Close() {
	if a == nil || a.server == nil {
		return
	}
	a.server.Shutdown()
	a.server = nil
	if a.log != nil {
		a.log.Info(context.Background(), "node: stopped announcing this node", "instance", a.instance)
	}
}

// instanceName is what a person sees in a discovery browser: the name this machine calls itself, and
// the front of its identity.
//
// The identity is there because the name is not a fact — every machine's default is the same word,
// and anyone may announce any name at all. It is the READER's job to check what a machine proves
// before believing anything about it; the suffix is a courtesy to whoever is looking down a list of
// machines, and no part of any check.
//
// The name is made SAFE rather than trusted, and what that means is narrow: a DNS-SD instance name
// may hold anything except a dot and a backslash (the two characters the DNS encoding has to escape),
// and this library does not escape them — it joins instance, service and domain with dots and trims a
// trailing one. So a dot in a configured name would cut it in half and a backslash would make the
// record unreadable, and those two become dashes. Everything else is KEPT, control characters aside:
// a machine called 家里的机器 is a machine somebody named in the language they think in, and a browser
// shows it correctly.
func instanceName(label, nodeID string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r == '.', r == '\\':
			return '-'
		case unicode.IsSpace(r):
			// Whitespace is a separator a person typed, so it becomes the one character that reads
			// like a separator in a name; anything else a control character might be is dropped.
			return '-'
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, strings.TrimSpace(label))

	// Bounded so a long configured name cannot push the identity off the end of what a browser shows
	// — bounded in RUNES and with no marker: this is a name, and "…" in a name is noise.
	if runes := []rune(name); len(runes) > instanceNameMaxRunes {
		name = string(runes[:instanceNameMaxRunes])
	}
	name = strings.Trim(strings.TrimSpace(name), "-")
	if name == "" {
		name = "tachi"
	}
	return name + "-" + agentref.ShortID(nodeID)
}

// instanceNameMaxRunes bounds the configured half of an instance name: enough for a name a person
// recognises, short enough that the identity beside it stays visible in a list.
const instanceNameMaxRunes = 40

// announceTTL is how long a reader may cache this machine's records without hearing from it again.
// The library re-announces well inside it, so the number only matters for the case where this machine
// stops talking without closing: two minutes is the conventional DNS-SD choice, and it is the
// reader's own sighting TTL (node/discovery.go) that decides how long a machine stays on a list.
const announceTTL = 2 * time.Minute

// advertiseTXT is what this machine says about itself in the announcement: the identity, the API
// version, and the scheme.
//
// Three facts, and each is there for a reason of its own: the identity is what a reader compares
// against the certificate the node proves (the only one of the three that is not a statement about
// software); the version is so a node of another vintage is not dialled; the scheme is so nothing
// tries this port expecting to speak to it in the clear. No name, no description, no capability list
// — those live in the Agent Card, which a reader only gets AFTER it has connected and proved who is
// answering (design §4.3: 身份提示仍须连接后验证).
//
// The spelling (`key=value`) is the DNS-SD convention this library's own reader parses back, and the
// two halves live in one file each (node/discovery.go reads them).
func advertiseTXT(nodeID string) []string {
	return []string{
		mdnsTXTIdentity + "=" + nodeID,
		mdnsTXTAPIVer + "=" + strconv.Itoa(APIVersion),
		mdnsTXTScheme + "=" + mdnsScheme,
	}
}
