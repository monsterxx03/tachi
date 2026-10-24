//go:build headless

package main

// The no-window entry point: the same session runtime as the desktop app, with the phone as its
// only frontend.
//
// Everything here mirrors main.go's startup order minus the window, the tray, menus, theming,
// notifications, the badge link and voice — the parts that need a desktop session. What is left is
// what a conversation actually needs: config, the session store, the per-session agent pool, the
// cron scheduler, the memory pass, the node service, and the phone interface.
//
// It is compiled ONLY with `-tags headless`, which is also what makes the window framework absent
// from the dependency graph: `CGO_ENABLED=0 GOOS=linux go build -tags headless` produces a binary
// that needs no GTK, no WebKit, no display. That is the point — it runs on a NAS.

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/logger"
)

func main() {
	// One host per base directory — see host_lock.go. Taken before the config is read, so a second
	// launch leaves no listener, no scheduler and no stray state behind: it says why and exits.
	hostLock, lockErr := acquireHostLock()
	if lockErr != nil {
		log.Fatal(lockErr)
	}
	defer hostLock.Release()

	desk := newDesktopApp()

	// Boot the tachi agent. There is no simulated-turn fallback here on purpose: a headless host
	// with no provider can only mislead the phone, and the protocol already has a code for it
	// (409 agent_unavailable), so a failed bootstrap is reported by the phone's own error path
	// rather than by invented activity.
	if err := desk.initAgent(context.Background()); err != nil {
		log.Fatalf("tachi: agent unavailable: %v", err)
	}
	cfg := desk.config()
	if cfg == nil {
		log.Fatal("tachi: no configuration")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Scheduled tasks live in this process, exactly as they do in the desktop app: a job runs while
	// the host runs, and a stop stops them (no catch-up).
	desk.startCron(ctx)
	desk.startDream(ctx)

	// The node service (multi-node collaboration) is optional and OFF by default; a failure must
	// not cost the user their conversations, so it is logged and the rest starts anyway.
	if cfg.Node.Enabled {
		nodeHandle, err := node.StartOrAttachNode(ctx, cfg)
		if err != nil {
			logger.New("headless").Error(ctx, "node: node service unavailable; continuing", err)
		} else if nodeHandle != nil {
			defer nodeHandle.Close()
		}
	}
	desk.warmPeers()

	// The phone interface: HTTP for commands, one WebSocket for the progress stream. It is off
	// unless the config's mobile block enables it.
	if !cfg.Mobile.Enabled {
		log.Fatal("tachi: the phone interface is disabled; set mobile.enabled: true in config.yaml")
	}
	desk.startMobile(cfg)
	srv := desk.mobileServer()
	if srv == nil || srv.listenError() != "" {
		reason := "no server"
		if srv != nil {
			reason = srv.listenError()
		}
		log.Fatalf("tachi: cannot listen on %s: %s", cfg.Mobile.MobileAddress(), reason)
	}

	log.Printf("tachi: headless host up, phone interface on %s", cfg.Mobile.MobileAddress())
	// The one-host lock (host_lock.go) is already held: another entry point that tries this data
	// directory while this process runs is refused, so nothing else can write these conversations.
	log.Print("tachi: this process owns ~/.tachi while it runs; a second host (the desktop app or " +
		"another headless process) will be refused until it exits")

	<-ctx.Done()

	log.Print("tachi: shutting down")
	// Same order as the desktop's shutdown hooks: schedulers first (they own turn goroutines),
	// then the phone interface, then the things holding background sockets.
	desk.stopCron()
	desk.stopDream()
	desk.stopMobile()
	if cache := desk.delegator; cache != nil {
		cache.Close()
	}
	desk.teardownAgent()
}
