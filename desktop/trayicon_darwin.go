//go:build darwin && !headless

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// trayClickOpensMenu: a macOS menu-bar item does not drop its menu on a left click by
// itself, so the app has to ask for it (see setupTray).
const trayClickOpensMenu = true

// applyTrayIcon paints the state icon onto the menu-bar item.
//
// macOS gets the TEMPLATE icon: the shapes in menuicon/ are monochrome on purpose,
// and the menu bar recolours a template image to match its own appearance — and
// inverts it while the menu is open, which is what keeps the icon legible in both
// light and dark. That recolouring is macOS-only (see trayicon_other.go).
func applyTrayIcon(tray *application.SystemTray, status AgentStatus) {
	if icon := trayIcon(status); icon != nil {
		tray.SetTemplateIcon(icon)
	}
}
