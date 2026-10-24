//go:build !headless

package main

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/monsterxx03/tachi/desktop/link"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// windowHost carries the process's WINDOW-facing state, embedded into desktopApp.
//
// It exists so that a build without a window (`-tags headless`, see main_headless.go) can compile
// the very same runtime: the runtime files ask the app questions like "is there anyone looking?"
// (hasUI) and "tell the frontend" (emit), and those two answer differently here than they do in
// the windowed build. The fields keep their names, so every file that does have a window (tray.go,
// notify.go, remote.go, main.go) reads d.app / d.tray / d.window exactly as before.
//
// Nothing in the headless build may reach for these: the type there has no fields at all, which is
// what turns a stray d.app into a compile error instead of a nil panic on a machine with no screen.
type windowHost struct {
	app  *application.App
	tray *application.SystemTray
	// trayStatus is the tray menu's live readout item (state + today's spend + the displayed
	// session's context %). refreshTray updates it. On Linux it is the ONLY place those numbers
	// can be seen INSIDE Tachi's own chrome: a tray host there shows neither the bar label
	// (SetLabel) nor a tooltip. What a bar OUTSIDE this process can draw comes from refreshTray's
	// other copy, desktop_status.json (barstatus.go).
	trayStatus *application.MenuItem
	window     *application.WebviewWindow
	// quitting is set once the app is shutting down (see main.go's OnShutdown), and it is what
	// lets the main window's close hook tell a USER-initiated close (which hides the window)
	// from the one the shutdown path performs on its way out (which must go through). Without
	// it the hook would be the app's own exit being cancelled.
	quitting atomic.Bool

	// voice is the desktop's own microphone state (the composer's voice button). It lives with
	// the window because the microphone is a desktop capability: a headless host has no room to
	// record in. Its own lock lives inside the type — a recording runs for tens of seconds and a
	// transcription for hundreds of milliseconds, neither of which may wait behind the app lock.
	voice voiceInput

	// badgeLink is the link server of the connection that is up right now (nil when the
	// device is away). It exists for the one message that has to be written from
	// OUTSIDE the link's own goroutine: the voice receipt, which is produced after
	// transcription finishes. Guarded by mu.
	badgeLink *link.Server

	// alerts holds the device's pending notification sounds, oldest first. They wait here
	// until the link's Server takes them (see TakeAlerts) — a beep is an EVENT, not part of
	// the snapshot, so it must not be swallowed by the snapshot's debounce. Guarded by mu.
	alerts []link.Alert

	// linkMu guards the device link's subscriber list AND its lifecycle — the running
	// listener and the cancel that stops it (see badge.go). It is NOT mu: notifyLink is
	// called from under mu in several places, and the callbacks must never wait behind
	// the app-wide lock.
	linkMu     sync.Mutex
	linkSubs   map[int]func()
	linkSubSeq int
	linkStop   context.CancelFunc
	linkLn     link.Listener
	// linkGate serializes startBadge/stopBadge, so that "at most one link is live" holds
	// by OUR bookkeeping rather than by the operating system refusing a second open. BLE
	// will not refuse it — two centrals can connect to the same peripheral —
	// and two live links mean two ref ledgers, where the same waiting item gets registered
	// twice and neither copy can be answered. linkMu keeps guarding the fields; this one
	// keeps two whole switches from interleaving.
	linkGate sync.Mutex
}

// newWindowHost is the windowed process's zero state.
func newWindowHost() windowHost { return windowHost{} }

// newNotifierHost returns the notifier a process of THIS kind can offer. The windowed half hands
// back a typed nil for the notifier main.go installs later; every method tolerates nil, which is
// what lets a turn run in a test (or before the notification service starts) without one.
func newNotifierHost() notifierHost { return (*notifier)(nil) }

// hasUI reports whether a window exists to be told anything. Runtime files use it as the "is a
// frontend listening?" gate around pushes that only make sense with a webview.
func (d *desktopApp) hasUI() bool { return d.app != nil }

// emit pushes one event to the window. It is the ONLY way the runtime talks to a frontend, so the
// headless build can silence every one of them by redefining this single method.
//
// Some call sites still wrap it in `if d.hasUI()` — that is the shape they had when the field was
// d.app, and it is now redundant (this method answers the same question). Either form is correct;
// new code can just call emit.
func (d *desktopApp) emit(name string, data any) {
	if d.app != nil {
		d.app.Event.Emit(name, data)
	}
}
