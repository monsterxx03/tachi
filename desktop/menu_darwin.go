//go:build darwin && !headless

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// installApplicationMenu installs Tachi's menu bar (applicationMenu below).
//
// It is a platform PAIR (menu_other.go is the other half) because the application menu is a
// macOS-only surface here: on Linux Wails draws the same menu as a GTK menu bar INSIDE the
// window — File / Edit / View / Window / Help — with nothing behind it, since the roles it
// carries (App, Services, Hide, a Window menu) are macOS ideas. See menu_other.go.
func installApplicationMenu(app *application.App) {
	app.Menu.SetApplicationMenu(applicationMenu())
}

// applicationMenu returns the app's menu bar: Wails' default macOS menu with the
// three WebKit zoom items REMOVED.
//
// Those items (Zoom In / Zoom Out / Actual Size, ⌘= / ⌘- / ⌘0) answer the keys with
// `webView.setMagnification` — the WHOLE window, sidebar and chrome included. Tachi's
// reading surfaces have a size of their own and a step of the reader's own (⌘= / ⌘- /
// ⌘0, see frontend/src/fontsize.ts), and a key equivalent claimed by a menu item is
// handled BEFORE the webview ever sees the event: leaving the items in place makes the
// shortcut dead in a way no frontend test can observe (a synthetic key dispatched into
// the page bypasses the menu entirely).
//
// Everything else the default carries is kept exactly as it is — Reload / Force Reload /
// DevTools / Toggle Full Screen and the App, File, Edit, Window and Help menus. Wails
// only substitutes this menu for its default when one was set at all, which is why
// installApplicationMenu above installs it explicitly.
func applicationMenu() *application.Menu {
	menu := application.DefaultApplicationMenu()

	// The default's View menu is assembled by NewViewMenu, which adds the zoom ROLES as
	// plain items — their role field is never set, so they can only be found by the labels
	// Wails gives them. A Wails upgrade that renames one makes the removal a no-op, and
	// TestApplicationMenuFreesTheZoomKeys (which asserts on the ACCELERATORS) is what fails.
	for _, label := range []string{"Zoom In", "Zoom Out", "Actual Size"} {
		if item := menu.FindByLabel(label); item != nil {
			menu.RemoveMenuItem(item)
		}
	}
	return menu
}
