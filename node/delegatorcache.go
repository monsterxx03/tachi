package node

import (
	"context"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// DelegatorCache is this machine's sending side, kept up to date against the live config.
//
// Every entry point needs exactly the same thing, and none of the three obvious ways of getting
// it is right on its own:
//
//   - building it at startup freezes the peer list, which is a config leaf the reader can edit
//     while the process runs (the `peers` settings row is applyLazy — it takes effect on the
//     next turn);
//   - building it per turn re-scans the delegated-task records (a directory read) on every
//     turn of every conversation, including the ones that never delegate;
//   - keeping it in a process global makes tests fight each other and hides the dependency.
//
// So: one cache per entry point, keyed by the peer list itself. The caller holds it as a field
// and asks for the delegator on the turn it is about to run.
//
// The key is the CONFIGURED peers — name, address and note — because that is everything that
// decides which nodes exist and where they are. A note change rebuilds too: it is what the
// reminder says about an unreachable node, and a stale description is worse than a rebuild.
type DelegatorCache struct {
	mu      sync.Mutex
	current tools.RemoteDelegator
	key     string
	log     *logger.Logger
	// ownName is what this machine calls itself, sent with every delegation so a peer's records
	// can say who asked (node.name).
	ownName  string
	openFunc func(peers []config.PeerConfig, ownName string, log *logger.Logger) (*Sender, error)
}

// NewDelegatorCache returns a cache that builds senders with OpenSender.
func NewDelegatorCache(ownName string, log *logger.Logger) *DelegatorCache {
	return &DelegatorCache{ownName: strings.TrimSpace(ownName), log: log, openFunc: OpenSender}
}

// For returns the sending side for this peer list, rebuilding it when the list changed.
//
// It returns nil — meaning "this process cannot delegate" — in two cases, and both are states
// the entry points should simply live with rather than report per turn:
//
//   - no peers are configured: there is nothing to delegate to, and `RemoteTask` on its own
//     could only show records this machine can no longer reach;
//   - the records directory cannot be opened: a broken disk must not cost the user their local
//     chat (the entry points log it), and the next turn tries again rather than caching the
//     failure, so a transient problem heals itself.
func (c *DelegatorCache) For(peers []config.PeerConfig) tools.RemoteDelegator {
	if c == nil || len(peers) == 0 {
		return nil
	}
	key := peerSignature(peers)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil && c.key == key {
		return c.current
	}

	open := c.openFunc
	if open == nil {
		open = OpenSender
	}
	sender, err := open(peers, c.ownName, c.log)
	if err != nil {
		if c.log != nil {
			c.log.Warn(context.Background(), "delegation is unavailable this turn", "cause", err)
		}
		return nil
	}
	c.current, c.key = sender, key
	return sender
}

// peerSignature is what a rebuild is keyed by.
func peerSignature(peers []config.PeerConfig) string {
	parts := make([]string, 0, len(peers))
	for _, p := range peers {
		// A separator that cannot occur in a name or a URL, so two different lists cannot
		// produce one signature by running into each other.
		parts = append(parts, strings.Join([]string{p.Name, p.URL, p.Note}, "\x00"))
	}
	return strings.Join(parts, "\x01")
}
