//go:build !darwin && !headless

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// installApplicationMenu deliberately installs NO menu bar off macOS.
//
// On Linux Wails prepends the application menu to the window as a GTK menu bar
// (linux_cgo.go's windowNew), so a menu is a visible strip of chrome, not a global bar
// — and Wails' default is exactly the macOS menu: an App menu, Services, Hide, a Window
// menu, none of which means anything here. Nothing needs it either: WebKit answers
// copy/paste and the other editing keys itself, and quitting is the tray menu's
// 「退出 Tachi Desktop」.
//
// The menu must be left UNSET rather than set to nil: `SetApplicationMenu(nil)` makes
// Wails substitute its default (linuxApp.setApplicationMenu), which is the very bar this
// avoids. Never having set one leaves the window without a menu at all.
//
// A menu accelerator is handled BEFORE the webview sees the key, so this is also what
// keeps a default menu item from claiming a shortcut the page owns (the reason the macOS
// half trims the zoom trio in the first place).
func installApplicationMenu(_ *application.App) {}
