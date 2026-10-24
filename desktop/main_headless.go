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

	log.Printf("tachi: headless host up, phone interface on %s", srv.listenAddr())
	logMobilePairing(desk)
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
		if i == 0 {
			log.Printf("tachi: phone pairing — fill this address in the app, or scan the code below: %s", c.URL)
			continue
		}
		log.Printf("tachi:                 another interface: %s (%s)", c.URL, c.Interface)
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
