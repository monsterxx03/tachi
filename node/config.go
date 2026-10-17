package node

import (
	"context"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent/skill"
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
	dataDir := config.A2ADir()
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
		DataDir: dataDir,
		// The local control interface stays where it was: this machine's own business, never a
		// config key (design §3.3).
		LocalListen: DefaultLocalListen,
		// The REMOTE surface is the operator's decision: the address it binds, what it calls
		// itself, and what it advertises. Nothing here decides WHO may call — the network does
		// (see checkRemoteListen), and every caller is still identified by its certificate.
		RemoteListen: cfg.Node.Listen,
		PublicURL:    cfg.Node.PublicURL,
		// Discovery is ONE switch, and it covers announcing too: a machine that looks for others on
		// this network may be looked for on it (config node.discovery.mdns.enabled). This is the
		// process that decides whether it happens at all, because it is the one hosting the surface
		// there would be something to find on.
		Advertise:           cfg.Node.Discovery.Mdns.Enabled,
		AdvertiseInterfaces: append([]string(nil), cfg.Node.Discovery.Mdns.Interfaces...),
		// Discovery by hearsay, and how old a claim it will act on. Both are the operator's: the
		// switch decides whether this node answers at all, the window decides when a peer's word about
		// a machine is stale enough to be worth one connection instead of a schedule.
		Gossip:      cfg.Node.Discovery.Gossip.Enabled,
		ContactTTL:  cfg.Node.Discovery.ContactTTL,
		Name:        cfg.Node.Name,
		Description: cfg.Node.Description,
		// What this machine's agent can do, published beside the node's own capability of running
		// a remote turn. Read here, once, because a Card is a description published at startup.
		Skills:   MachineSkills(),
		Executor: &nodeExecutor{runner: taskRunner},
		Logger:   log,
	})
}

// MachineSkills reads the skills THIS MACHINE's agent has, for the node's Agent Card to publish.
//
// It is read here — in the one file where the node meets process-global configuration — because the
// Card is built at startup, before any session exists: the local agent's store is built per session
// and is not reachable from here. What it reads is the same set (and the same precedence) the local
// agent will see, minus a project root: a remote task works in node.workspace, and a project's own
// skills belong to whoever opened that project.
//
// Disabled skills are left out: `enabled: false` means "not loaded, not activatable" everywhere
// else, and a Card that advertised one would be claiming something this machine will refuse to do.
func MachineSkills() []a2a.AgentSkill {
	metas := skill.NewStore("").List()
	out := make([]a2a.AgentSkill, 0, len(metas))
	for _, m := range metas {
		if !m.Enabled {
			continue
		}
		out = append(out, a2a.AgentSkill{
			ID:          m.Name,
			Name:        m.Name,
			Description: m.Description,
			Tags:        m.Tags,
		})
	}
	return out
}
