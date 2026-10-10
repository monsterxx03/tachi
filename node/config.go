package node

import (
	"context"

	"github.com/monsterxx03/tachi/config"
)

// StartOrAttachNode is the ONE entry point the frontends call. It reads the node's
// settings from the process config and the data directory from the resolved base
// dir, then hands everything explicitly to StartOrAttach — this file is the only
// place where the node meets process-global configuration.
//
// Disabled (the default) means nothing happens at all, and the returned handle is
// nil: no data directory, no lock, no listener. Callers depend on that to keep
// behaving exactly as they did before the node existed, so it is asserted by tests
// rather than promised in a comment.
//
// A non-nil error means this process has no usable node service right now. It is
// NOT fatal to local chat: the entry points log it and carry on (design §3.2).
func StartOrAttachNode(ctx context.Context, cfg *config.Config) (*Handle, error) {
	if cfg == nil || !cfg.Node.Enabled {
		return nil, nil
	}
	return StartOrAttach(ctx, Options{
		DataDir:     config.NodeDir(),
		LocalListen: DefaultLocalListen,
	})
}
