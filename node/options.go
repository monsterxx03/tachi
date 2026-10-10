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
//   - the REMOTE A2A interface — loopback only as well, and for a harder reason: it
//     authenticates nobody yet, so it must not be reachable from anywhere
//     (Options.normalized refuses a non-loopback address). Caller authentication is
//     the slice that opens it up.
package node

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

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

	// RemoteListen is the address of the A2A interface — the one other nodes talk
	// to. It must name a loopback host too, until caller authentication exists
	// (design §3.3, §10.1): a node that anyone can call must not be reachable from
	// anywhere. The address it actually binds is what the Card publishes. Ignored
	// when this process attaches. Empty means DefaultRemoteListen.
	RemoteListen string

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
	if err := checkLoopback("RemoteListen", o.RemoteListen); err != nil {
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
// Both of this node's listeners are on loopback for the same reason and it is not
// politeness: neither of them authenticates its callers yet. The local one has a
// private credential but is meant for THIS machine, and the remote one has nothing
// at all until the pairing slice lands. A non-loopback address would therefore be
// the difference between "a feature nobody enabled" and "an open agent endpoint",
// so it is refused rather than warned about.
func checkLoopback(name, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("node: %s %q: %w", name, addr, err)
	}
	if !netutil.IsLoopbackHost(host) {
		return fmt.Errorf("node: %s %q must name a loopback host: "+
			"this listener does not authenticate callers yet, so it must not be reachable from anywhere", name, addr)
	}
	return nil
}
