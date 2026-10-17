// Package node owns this machine's side of multi-node collaboration: the node
// identity, the single-instance lock, the local control interface that the other
// Tachi processes on this machine talk to, the A2A surface a peer talks to, the task
// store that remembers what was accepted, and the runner that turns an accepted task
// into a real Tachi turn.
//
// Everything it needs is handed in. This package never reads the process-global
// config and never resolves paths from the user's data directory; the entry points
// do that and pass the result down (docs/2026-10-10-a2a-node-design.md §3). That is
// also what lets ONE test process run two independent nodes, which is where most of
// the multi-node behaviour gets verified.
//
// Two surfaces live on a node and they are NOT the same thing (design §3.3):
//
//   - the LOCAL control interface — loopback only, guarded by a private credential
//     read from the node's data dir. Other Tachi processes on this machine use it.
//   - the REMOTE A2A interface — the one other machines talk to. Off loopback it serves TLS and
//     requires a client certificate from the caller, so a request that arrives is always
//     identified and never in the clear; on loopback it serves plaintext and identifies nobody,
//     because there is no network to read it off and no caller to name. What that surface does NOT
//     do is decide who MAY call: it knows WHO did (Options.checkRemoteListen).
package node

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/netutil"
)

const (
	// APIVersion is the version of the local control interface. A process that
	// speaks a different one must not talk to this node: the caller reports
	// ErrVersionMismatch instead of guessing at the shape of the answers.
	APIVersion = 1

	// DefaultLocalListen binds a kernel-chosen port on loopback. Tests rely on
	// ":0" so two nodes never fight over a port.
	DefaultLocalListen = "127.0.0.1:0"

	// DefaultRemoteListen is the A2A interface's default address. Loopback, and it
	// will stay loopback until caller authentication exists: enabling a node must not
	// put an unauthenticated agent on the network.
	DefaultRemoteListen = "127.0.0.1:0"

	// DefaultAttachTimeout bounds how long StartOrAttach waits for a host that is
	// still coming up to publish its local info. Bounded, not infinite: a process
	// that never announces itself is indistinguishable from one that is wedged.
	DefaultAttachTimeout = 5 * time.Second

	// attachRetryInterval is how often the attach path re-reads the published
	// local info while waiting.
	attachRetryInterval = 25 * time.Millisecond

	// dataDirPerm is the node data dir: private, like everything in it.
	dataDirPerm = 0o700

	// lockFileName is the single-instance lock inside the data dir.
	lockFileName = "node.lock"

	// shutdownTimeout bounds the local interface's graceful shutdown on Close.
	shutdownTimeout = 3 * time.Second

	// localHTTPTimeout is the client-side budget for one local request: loopback
	// plus a small JSON answer, so anything slower is a wedged holder, not a slow
	// one.
	localHTTPTimeout = 5 * time.Second
)

// Options is the whole input of StartOrAttach: an explicit data directory and an
// explicit listen address, nothing read from process globals.
type Options struct {
	// DataDir is the node's own directory: identity, lock and published local
	// connection info live here, and the task store will. Required.
	DataDir string

	// LocalListen is the address of the local control interface. It must name a
	// LOOPBACK host — the interface is guarded by a private credential, not by
	// where it is reachable, and binding it anywhere else would hand that
	// credential's endpoint to the network. Ignored when this process attaches
	// instead of hosting. Empty means DefaultLocalListen.
	LocalListen string

	// RemoteListen is the address of the A2A interface — the one other nodes talk to.
	//
	// It decides MORE than where to bind: this address is the one switch that says whether the
	// surface serves TLS, and with it whether callers are identified at all (see isLoopbackListen,
	// and node/a2a.go where one expression drives encryption, the client certificate request and
	// the identity carried into the request).
	//
	// Loopback (the default) is plaintext and identifies nobody, which is right for what it is:
	// nothing can read a conversation off the machine, and there is no caller to name. Any other
	// address encrypts and REQUIRES a client certificate, so a node is never an open agent
	// endpoint — but it is also not a gatekeeper: whoever can reach the address gets work done on
	// this machine. THE NETWORK IS THE BOUNDARY, deliberately (checkRemoteListen).
	//
	// Ignored when this process attaches. Empty means DefaultRemoteListen.
	RemoteListen string

	// Name is what this node calls itself (config: node.name): published in its Agent Card and
	// sent to a peer with every call, so the peer's records can say who asked. Empty means the
	// default.
	//
	// It is a LABEL, and the receiving side records it as self-declared. There is no list of
	// accepted callers and no credential: any client certificate is accepted, because this node's
	// job here is to know WHO called (a node id it cannot be fooled about) rather than to decide
	// who MAY (that judgment belongs to the network — design §3.3, and a cluster that wants a
	// whitelist can add one back where this one was).
	Name string

	// Description is what this node says it is FOR (config: node.description): published in its
	// Agent Card and shown to the operator on the other side. Empty means the built-in line, which
	// describes what a Tachi node is rather than what this one is for.
	//
	// Self-declared, like Name, and read the same way.
	Description string

	// Skills are the skills THIS MACHINE's agent has, published in the Card beside the node's own
	// capability of running a remote turn. The host reads them at startup from the same store the
	// local agent reads (node.MachineSkills) and hands them in here: this package never reads the
	// process-global config or the user's data directory (see the package comment), and a Card is a
	// description published at startup anyway — a skill added later shows up on the next start,
	// exactly like a changed Name or Description.
	Skills []a2a.AgentSkill

	// PublicURL is what the Agent Card advertises as the way to reach this node, when that is
	// not simply the address the socket bound (a mesh address, a port forwarder, a real host
	// name). Empty means "derive it from the bound address" — and where nothing can be derived,
	// because RemoteListen is a wildcard, the start REFUSES rather than publishing a Card that
	// sends callers nowhere (node/a2a.go).
	PublicURL string

	// Advertise is whether this node announces itself on the local network (design §一.2). It only
	// ever happens in the process hosting the remote surface — that is the only process with
	// something to announce.
	Advertise bool

	// AdvertiseInterfaces names the interfaces to announce on. Empty means the ones this machine could
	// actually carry a LAN announcement on, which is a FILTER rather than "everything"
	// (node/discovery.go LANInterfaces: a tunnel, a VM bridge or a NIC with no usable address is left
	// out, because the library sends on the interfaces one after another and one that cannot take the
	// packet stalls the round in silence). Naming them is for a machine on several networks, where
	// announcing its name onto the wrong one is what the setting exists to prevent — so a name that is
	// not an interface here is an error rather than a fallback.
	AdvertiseInterfaces []string

	// Gossip is whether the remote surface answers a peer asking who else this machine can vouch for
	// (config `node.discovery.gossip.enabled`, design §五). The sending side is what actually holds
	// that answer, so this flag decides whether the ROUTE exists at all; whether there is anything to
	// say is a question about the sending side (node/gossip.go registeredMesh).
	Gossip bool

	// ContactTTL is how long a machine this node has proved stays worth talking about (config
	// `node.discovery.contact_ttl`). The gossip route uses it to decide when a peer's claim about a
	// machine is old enough to be worth ONE connection rather than a retry schedule
	// (node/gossip.go gossipCandidates). Zero means the built-in default.
	//
	// It is read at startup, like everything else this surface answers with.
	ContactTTL time.Duration

	// Executor is the receiving side's agent: it turns an accepted task into events.
	// Nil means the built-in placeholder that echoes the task text — the seam a test
	// injects through. The real one is the task runner (node/runner.go), wired by
	// StartOrAttachNode; it is not built here because it needs the config, which this
	// Options struct deliberately does not carry.
	Executor a2asrv.AgentExecutor

	// AttachTimeout bounds the wait for a starting host to publish its info.
	// Zero means DefaultAttachTimeout.
	AttachTimeout time.Duration

	// Identity overrides the identity loaded from DataDir. Tests inject one; a
	// production caller leaves it nil and the data dir is the single source.
	Identity *Identity

	// Logger receives lifecycle events. Nil means logger.New("node").
	Logger *logger.Logger
}

func (o Options) normalized() (Options, error) {
	if strings.TrimSpace(o.DataDir) == "" {
		return Options{}, errors.New("node: Options.DataDir is required")
	}
	abs, err := filepath.Abs(o.DataDir)
	if err != nil {
		return Options{}, fmt.Errorf("node: resolve data dir %s: %w", o.DataDir, err)
	}
	o.DataDir = abs

	if o.LocalListen == "" {
		o.LocalListen = DefaultLocalListen
	}
	if err := checkLoopback("LocalListen", o.LocalListen); err != nil {
		return Options{}, err
	}
	if o.RemoteListen == "" {
		o.RemoteListen = DefaultRemoteListen
	}
	if err := checkRemoteListen(o.RemoteListen); err != nil {
		return Options{}, err
	}

	if o.AttachTimeout <= 0 {
		o.AttachTimeout = DefaultAttachTimeout
	}
	if o.Logger == nil {
		o.Logger = logger.New("node")
	}
	return o, nil
}

// checkLoopback refuses an address that is not loopback.
//
// The LOCAL control interface lives under this rule forever, and not because it lacks a
// credential — it has one. That interface is a way for the OTHER Tachi processes on this machine
// to talk to the node holding the lock; it is this machine's own business, nobody else's, and
// binding it anywhere else would hand its credential's endpoint to the network for no gain.
func checkLoopback(name, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("node: %s %q: %w", name, addr, err)
	}
	if !netutil.IsLoopbackHost(host) {
		return fmt.Errorf("node: %s %q must name a loopback host: "+
			"this interface is this machine's own business, and it is not authenticated for the network", name, addr)
	}
	return nil
}

// checkRemoteListen used to allow a non-loopback address only when the node authenticated its
// callers. It no longer restricts the address at all, and that is a deliberate trade rather than a
// relaxation nobody noticed:
//
//   - what still holds: the surface serves TLS off loopback and REQUIRES a client certificate, so
//     a caller is always identified (node/tls.go) and nothing travels in the clear;
//   - what no longer holds: nobody is turned away. Any machine that can reach this address and
//     present a certificate gets work done on this one, so THE NETWORK IS THE BOUNDARY — a mesh
//     that authenticates devices, a firewall, or loopback.
//
// That is a decision this project made on purpose (a personal cluster where reachability IS
// permission), and it is the reason this comment sits where a reader looks before pointing a node
// at a public address.
func checkRemoteListen(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("node: RemoteListen %q: %w", addr, err)
	}
	return nil
}

// isLoopbackListen reports whether the remote surface is on loopback, which is what decides
// whether it serves TLS: leaving the machine is what makes encryption and a client certificate
// necessary, and staying on it is what makes them pointless.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && netutil.IsLoopbackHost(host)
}
