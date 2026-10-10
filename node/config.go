package node

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
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

	workspace, err := cfg.Node.WorkspaceDir()
	if err != nil {
		return nil, err
	}
	dataDir := config.NodeDir()
	// The retention record lives beside the sessions, because that is what it is about: the
	// node protects ITS conversations from a cleanup that runs in any other process, and the
	// sweep reads the same directory (see session/retention.go).
	sessionsDir, err := config.SessionDir()
	if err != nil {
		return nil, fmt.Errorf("node: session dir: %w", err)
	}
	log := logger.New("node")

	// The remote tasks' executor: a real Tachi turn in a session on this node's workspace.
	// Building it here (rather than passing the config down) is what keeps StartOrAttach's
	// inputs explicit — see Options.Executor.
	taskRunner, err := newRunner(cfg, workspace, dataDir, sessionsDir, log)
	if err != nil {
		return nil, err
	}

	return StartOrAttach(ctx, Options{
		DataDir:     dataDir,
		LocalListen: DefaultLocalListen,
		Executor:    &nodeExecutor{runner: taskRunner},
		Logger:      log,
	})
}
