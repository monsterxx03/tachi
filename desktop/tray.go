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
	tray.SetTooltip("Tachi Desktop")

	menu := d.app.NewMenu()
	menu.Add("打开主窗口").OnClick(func(ctx *application.Context) {
		d.window.Show()
		d.window.Focus()
	})
	menu.Add("新会话").OnClick(func(ctx *application.Context) {
		d.window.Show()
		d.window.Focus()
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
	// Left click shows the window, right click opens the menu (native macOS
	// behaviour for a status-bar item).
	tray.OnClick(func() {
		d.window.Show()
		d.window.Focus()
	})
	tray.OnRightClick(func() {
		tray.OpenMenu()
	})

	return tray
}
