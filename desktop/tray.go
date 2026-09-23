package main

import (
	"embed"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed menuicon/*
var trayIconFS embed.FS

var (
	iconIdle     = loadTrayIcon("tray-idle.png")
	iconThinking = loadTrayIcon("tray-thinking.png")
	iconTool     = loadTrayIcon("tray-tool.png")
	iconBusy     = loadTrayIcon("tray-busy.png")
	iconError    = loadTrayIcon("tray-error.png")
)

func loadTrayIcon(name string) []byte {
	b, err := trayIconFS.ReadFile("menuicon/" + name)
	if err != nil {
		return nil
	}
	return b
}

// setupTray builds the macOS menu-bar item that reflects the agent state and
// offers quick actions. It returns the tray so the caller can attach it to the
// desktopApp (for state updates).
func setupTray(d *desktopApp) *application.SystemTray {
	tray := d.app.SystemTray.New()
	tray.SetTemplateIcon(iconIdle)
	tray.SetLabel("空闲")
	tray.SetTooltip("Tachi")

	menu := d.app.NewMenu()
	menu.Add("打开主窗口").OnClick(func(ctx *application.Context) {
		d.window.Show()
		d.window.Focus()
	})
	menu.Add("新会话").OnClick(func(ctx *application.Context) {
		d.newSessionFromMenuBar()
	})
	menu.AddSeparator()
	// Status readout — updated per state. Kept disabled so users can't
	// accidentally activate it; it is purely informational.
	menu.Add("运行状态: 空闲").SetEnabled(false)
	menu.AddSeparator()
	menu.Add("退出 Tachi Desktop").OnClick(func(ctx *application.Context) {
		d.app.Quit()
	})

	tray.SetMenu(menu)
	// The LEFT button opens the menu too — it no longer shows the window, so
	// 「打开主窗口」 is the item that brings the window back. The right button
	// keeps the framework's default for a menu-bearing tray (same menu).
	tray.OnClick(func() {
		tray.OpenMenu()
	})

	return tray
}

// uiNewSessionEventName is the shell asking the UI for a new conversation. The menu bar cannot
// make one itself: creation is the frontend's `newChat`, which also reveals the group the session
// lands in, resets the usage readouts and puts the caret in the composer — and it is the same call
// the sidebar's ＋ and ⌘N make, so all three entry points behave alike. The name is mirrored by
// App.tsx's listener (the two languages share no constant).
//
// It is NOT one of main.go's init registrations: a registered name makes Emit validate the payload
// against the registered type, and this event carries none, so the emit would be cancelled and the
// menu item would quietly do nothing.
const uiNewSessionEventName = "ui:new_session"

// newSessionFromMenuBar is the menu bar's 「新会话」: bring the window forward and let the frontend
// create the session (see uiNewSessionEventName). The event reaches a HIDDEN window too — the
// webview stays alive across a hide, which is what makes this work with the window closed.
func (d *desktopApp) newSessionFromMenuBar() {
	if d.window == nil {
		return
	}
	d.window.Show()
	d.window.Focus()
	if d.app != nil {
		d.app.Event.Emit(uiNewSessionEventName)
	}
}
