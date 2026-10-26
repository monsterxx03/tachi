//go:build !headless

package main

import "strings"

// The menu-bar readout. It lives apart from agent_turn.go because a headless process has no menu
// bar at all: its half is tray_readout_headless.go, and the runtime code that calls refreshTray
// never learns the difference.

// trayLabel is the menu bar's text: today's spend and how full the displayed session's context is,
// joined with " · ".
//
// Either half can be missing (nothing spent yet, nothing measured), and when BOTH are the label
// falls back to the session's short state — the bar must keep saying something rather than flipping
// to an empty box next to the icon.
func trayLabel(st AgentState, cost, context string) string {
	parts := make([]string, 0, 2)
	if cost != "" {
		parts = append(parts, cost)
	}
	if context != "" {
		parts = append(parts, context)
	}
	if len(parts) == 0 {
		return st.Label
	}
	return strings.Join(parts, " · ")
}

// refreshTray repaints the tray for the DISPLAYED session — the only surface where a state is
// not per session. The icon carries the state; the two numbers are what the window cannot tell you
// while it is hidden.
//
// The numbers go to THREE places, and each covers a surface the others cannot reach: SetLabel is
// the macOS menu bar's text (a Linux tray host ignores it), the status MENU ITEM is where a Linux
// user reads them (a macOS menu shows it too, which is a bonus), and desktop_status.json is what a
// bar widget outside this process draws (barstatus.go). The three are set from the same strings
// and numbers so they can never disagree.
func (d *desktopApp) refreshTray() {
	if d.tray == nil {
		return
	}
	st := d.currentState()
	estimate, _, window := d.contextUsage(st.SessionID)
	spent := d.todayCost()
	cost := formatCost(spent)
	context := trayContext(estimate, window)
	d.tray.SetLabel(trayLabel(st, cost, context))
	if d.trayStatus != nil {
		d.trayStatus.SetLabel(trayStatusItemText(st, cost, context))
	}
	applyTrayIcon(d.tray, st.Status)
	writeDesktopStatus(st, spent, contextPercent(estimate, window))
}

// trayStatusItemText is the label of the tray menu's status item: the state word plus the same two
// numbers the bar label carries.
//
// The state word is kept even when the numbers are present: on Linux this item is what a reader
// looks at to know what the app is doing, and "¥0.386 · 42%" alone would not say whether it is
// working or idle.
func trayStatusItemText(st AgentState, cost, context string) string {
	readout := trayLabel(st, cost, context) // the numbers, or st.Label when there are none
	if readout == st.Label {
		return "运行状态: " + st.Label
	}
	return "运行状态: " + st.Label + " · " + readout
}

// trayIcon returns the menu-bar (template) icon bytes for a given status.
func trayIcon(status AgentStatus) []byte {
	switch status {
	case StatusThinking:
		return iconThinking
	case StatusToolRunning:
		return iconTool
	case StatusBusy:
		return iconBusy
	case StatusError:
		return iconError
	default:
		return iconIdle
	}
}
