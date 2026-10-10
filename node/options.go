// Package node owns this machine's side of multi-node collaboration: the node
// identity, the single-instance lock, the local control interface that the other
// Tachi processes on this machine talk to, and — in later slices — the A2A surface
// and the task store.
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
//   - the REMOTE A2A interface — not implemented yet; it arrives with the A2A slice.
//
// This slice implements the local one only.
package node

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

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
	host, _, err := net.SplitHostPort(o.LocalListen)
	if err != nil {
		return Options{}, fmt.Errorf("node: LocalListen %q: %w", o.LocalListen, err)
	}
	if !netutil.IsLoopbackHost(host) {
		return Options{}, fmt.Errorf("node: LocalListen %q must name a loopback host: "+
			"the local interface is guarded by a private credential, not by its address", o.LocalListen)
	}

	if o.AttachTimeout <= 0 {
		o.AttachTimeout = DefaultAttachTimeout
	}
	if o.Logger == nil {
		o.Logger = logger.New("node")
	}
	return o, nil
}
