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
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	channelmgr "github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/logger"

	// The IM plugins this host can serve. They are named HERE, in the headless build only, for the
	// same reason main.go names them for the channel entry: a channel is linked in by importing it,
	// and the windowed desktop build must not carry the bots it will never run. Without these, an
	// enabled channel is "no factory registered" — visibly missing — instead of silently absent.
	_ "github.com/monsterxx03/tachi/channel/discord"
	_ "github.com/monsterxx03/tachi/channel/mock"
	_ "github.com/monsterxx03/tachi/channel/weixin"
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

	// IM channels: the ones config switched on run HERE, beside the phone interface, in this one
	// process. That is the point of a host: the conversations a person has with the bot and with the
	// phone are the same sessions under the same ~/.tachi, the A2A node is unambiguous (it is this
	// process'), and there is one discovery browser instead of two.
	//
	// A channel that cannot start does not cost the user the phone: it is logged and the host serves
	// what is left — the same posture as the node service above. When the phone is off as well,
	// there is nothing left to serve and starting is pointless (below).
	var channels *channelmgr.Manager
	if len(cfg.Channel.ActiveChannels()) > 0 {
		mgr, err := startChannels(ctx, desk, cfg)
		if err != nil {
			logger.New("headless").Error(ctx, "channels: not started; the host serves what is left", err)
		} else {
			channels = mgr
			// The phone may SPEAK INTO a conversation a channel owns (and read its scheduled jobs),
			// but the turn is run by the channel runtime — see desktop/channel_mirror.go. This is
			// the one hand-off that makes that possible in a single process.
			desk.setChannels(mgr)
		}
	}

	// The phone interface: HTTP for commands, one WebSocket for the progress stream. It is off
	// unless the config's mobile block enables it.
	if !cfg.Mobile.Enabled && channels == nil {
		log.Fatal("tachi: nothing to serve — set mobile.enabled: true in config.yaml for the phone " +
			"interface, or enable at least one channel")
	}
	if cfg.Mobile.Enabled {
		desk.startMobile(cfg)
		srv := desk.mobileServer()
		if srv == nil || srv.listenError() != "" {
			reason := "no server"
			if srv != nil {
				reason = srv.listenError()
			}
			// The mirror of the rule above: a phone that cannot listen must not cost the user the
			// bots. With nothing else to serve there is no reason to keep running, and a port that
			// cannot be opened is the answer the reader needs.
			if channels == nil {
				log.Fatalf("tachi: cannot listen on %s: %s", cfg.Mobile.MobileAddress(), reason)
			}
			log.Printf("tachi: WARNING the phone interface cannot listen on %s: %s — this host "+
				"serves the IM channels only", cfg.Mobile.MobileAddress(), reason)
		} else {
			log.Printf("tachi: headless host up, phone interface on %s", srv.listenAddr())
			logMobilePairing(desk)
		}
	} else {
		log.Print("tachi: headless host up, IM channels only (mobile.enabled is off)")
	}
	// The one-host lock (host_lock.go) is already held: another entry point that tries this data
	// directory while this process runs is refused, so nothing else can write these conversations.
	log.Print("tachi: this process owns ~/.tachi while it runs; a second host (the desktop app or " +
		"another headless process) will be refused until it exits")

	<-ctx.Done()

	log.Print("tachi: shutting down")
	// Same order as the desktop's shutdown hooks: schedulers first (they own turn goroutines), then
	// the frontends — channels before the phone, because a channel turn may be mid-write to a
	// session the phone is reading — then the things holding background sockets.
	desk.stopCron()
	desk.stopDream()
	if channels != nil {
		channels.Close()
	}
	desk.stopMobile()
	if cache := desk.delegator; cache != nil {
		cache.Close()
	}
	desk.teardownAgent()
}

// startChannels builds and starts the IM side of this host, or returns an error explaining which
// part of it could not come up.
//
// It exists as a function so the two things this process already owns reach the channel manager
// explicitly (see channelmgr.Dependencies) rather than as a second copy:
//
//   - the sending side. The host built one for the phone's node picker and its own turns; a manager
//     that built another would open a second discovery browser and a second sighting ledger, and the
//     process-wide gossip route would answer from whichever registered last.
//   - the memory pass. The host's own loop (startDream, above) is the one owner; the channel
//     manager's scheduler registration is skipped.
func startChannels(ctx context.Context, desk *desktopApp, cfg *config.Config) (*channelmgr.Manager, error) {
	mgr, err := channelmgr.NewWith(cfg, channelmgr.Dependencies{
		Delegator: desk.delegatorCache(cfg),
		SkipDream: true,
	})
	if err != nil {
		return nil, err
	}
	registered, missing, err := mgr.AddEnabled(cfg)
	if err != nil {
		mgr.Close()
		return nil, err
	}
	for _, name := range missing {
		log.Printf("tachi: WARNING channel %q is enabled in config but this binary has no factory "+
			"for it (its package was not imported)", name)
	}
	if len(registered) == 0 {
		mgr.Close()
		return nil, fmt.Errorf("no channel factories registered for any enabled channel: %v", missing)
	}
	if err := mgr.Start(ctx); err != nil {
		mgr.Close()
		return nil, fmt.Errorf("channel manager start: %w", err)
	}
	log.Printf("tachi: IM channels running: %v", registered)
	return mgr, nil
}

// logMobilePairing prints what the phone needs, because there is no settings card here to show it.
//
// The windowed build has GetMobilePairing behind a card (QR + address + a masked token). A headless
// host has no window at all, so without this the pairing information exists only as a file the user
// has to know about (~/.tachi/mobile.json) plus an IP they have to look up — and the address the
// listener logs is the BIND address (0.0.0.0:8787), which is not something a phone can dial.
//
// GetMobilePairing is the same source the card renders: candidates are this machine's LAN addresses,
// ordered physical-first, and the token is created on first call. One source, two presentations.
func logMobilePairing(d *desktopApp) {
	vo := (&AgentService{desk: d}).GetMobilePairing()
	if vo.Error != "" {
		log.Printf("tachi: pairing information unavailable: %s", vo.Error)
		return
	}
	if len(vo.Candidates) == 0 {
		log.Print("tachi: this machine has no usable LAN address — a phone cannot reach it")
		return
	}
	if host, _, err := net.SplitHostPort(vo.Listen); err == nil && loopbackHost(host) {
		// The tokens and the card are right; the listener itself is the problem. Saying so here is
		// the difference between "the phone keeps saying it cannot connect" and one line the reader
		// can act on before they start pairing.
		log.Printf("tachi: WARNING mobile.listen is %s — only this machine can reach that; "+
			"set it to 0.0.0.0:8787 for the phone to connect", vo.Listen)
	}
	for i, c := range vo.Candidates {
		// The label, not the interface name: "utun10 · 100.98.239.75" tells a reader nothing about
		// which of these they can actually dial from where.
		if i == 0 {
			log.Printf("tachi: phone pairing — fill this address in the app, or scan the code below: %s (%s)",
				c.URL, c.Label)
			continue
		}
		log.Printf("tachi:                 fallback address: %s (%s)", c.URL, c.Label)
	}
	if len(vo.URLs) > 1 {
		log.Print("tachi: the code carries every address above; the phone tries them and uses whichever answers")
	}
	printPairingQRCode(vo.Payload)
	log.Printf("tachi: token: %s", vo.Token)
	log.Print("tachi: that line is a credential — delete it before pasting this log anywhere")
}

// loopbackHost reports whether a listen host only accepts connections from this machine.
func loopbackHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "::":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// printPairingQRCode shows the payload as a scannable code, but ONLY on a terminal.
//
// A QR code is the difference between copying a 43-character token by hand and pointing the phone at
// the screen. When stdout is not a terminal — systemd's journal, a redirected log, the itest
// harness — the block characters are unreadable noise in a file nobody can scan, so the two text
// lines above are the whole answer there.
func printPairingQRCode(payload string) {
	info, err := os.Stdout.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return
	}
	code, err := pairingQRCode(payload)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stdout, code)
}
