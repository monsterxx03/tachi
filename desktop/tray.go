//go:build !headless

package main

import (
	"embed"
	"fmt"
	"log"

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

// setupTray builds the tray item that reflects the agent state and offers quick
// actions — the menu bar on macOS, the StatusNotifier icon in the bar on Linux. It
// returns the tray so the caller can attach it to the desktopApp (for state updates).
//
// Two things differ per platform and live behind helpers (trayicon_darwin.go /
// trayicon_other.go): HOW the icon is drawn (a macOS template vs. a tinted colour) and
// whether a click has to open the menu itself.
func setupTray(d *desktopApp) *application.SystemTray {
	tray := d.app.SystemTray.New()
	applyTrayIcon(tray, StatusIdle)
	tray.SetLabel("空闲")
	tray.SetTooltip("Tachi")

	menu := d.app.NewMenu()
	menu.Add("打开主窗口").OnClick(func(ctx *application.Context) {
		d.revealWindow()
	})
	menu.Add("新会话").OnClick(func(ctx *application.Context) {
		d.newSessionFromMenuBar()
	})
	menu.AddSeparator()
	// Status readout — updated per state by refreshTray. Kept disabled so users
	// can't accidentally activate it; it is purely informational.
	//
	// It is a live MENU ITEM rather than only the bar label because the two platforms
	// differ in where text can appear: macOS shows SetLabel in the menu bar, while a
	// Linux tray host shows neither that label nor the tooltip — the menu is the only
	// surface left for the numbers in Tachi's own chrome (a bar widget outside this
	// process reads them instead from desktop_status.json, see barstatus.go).
	d.trayStatus = menu.Add(trayStatusPlaceholder).SetEnabled(false)
	menu.AddSeparator()
	menu.Add("退出 Tachi Desktop").OnClick(func(ctx *application.Context) {
		d.app.Quit()
	})

	tray.SetMenu(menu)
	// The LEFT button: on macOS a menu-bar item does not drop its menu on a click by
	// itself, so the app opens it. On Linux the HOST shows the menu on click and the
	// framework's OpenMenu is unimplemented — so this is deliberately skipped there
	// (trayClickOpensMenu), or the click would be swallowed and an error logged on
	// every press.
	if trayClickOpensMenu {
		tray.OnClick(func() {
			tray.OpenMenu()
		})
	}

	return tray
}

// trayStatusPlaceholder is the status item's label before any state has been reported
// (refreshTray replaces it). Mirrors the initial "空闲" the tray label starts from.
const trayStatusPlaceholder = "运行状态: 空闲"

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

// revealWindow brings the window back and gives it the keyboard: the way home from a hidden
// window (the close hook in main.go only hides it, and the tray is the route back). Used by the
// menu's 打开主窗口 and by a second launch of the app (singleInstanceOptions).
func (d *desktopApp) revealWindow() {
	if d.window == nil {
		// A second launch can land while this instance is still building its window; the window
		// it is asking for is about to appear on its own, so there is nothing to do.
		log.Printf("desktop: asked to reveal the window before one existed")
		return
	}
	d.window.Show()
	d.window.Focus()
}

// newSessionFromMenuBar is the menu bar's 「新会话」: bring the window forward and let the frontend
// create the session (see uiNewSessionEventName). The event reaches a HIDDEN window too — the
// webview stays alive across a hide, which is what makes this work with the window closed.
func (d *desktopApp) newSessionFromMenuBar() {
	if d.window == nil {
		return
	}
	d.revealWindow()
	if d.hasUI() {
		d.emit(uiNewSessionEventName, nil)
	}
}

// 菜单栏那一格的读数：上下文百分比与今日花费。
//
// 这两样（以及它们用到的 toFloat / contextPercent）原先住在 sessioninfo.go —— 那个文件是给
// 设备双击确定看会话账用的，而那条路已经没了（协议 v2，见 desktop/link/link.go 的 Proto）。
// 留下来的这几个是**菜单栏自己的**：refreshTray 拿它们拼 trayLabel。
//
// 为什么不复用（已经删掉的）设备那套排版：那串 "12.3k / 32k (42%)" 是给 240 像素的小屏写的，
// 菜单栏一格放不下（旁边还挨着今日花费）。量不出来就不画——同一个态度：不编一个百分比出来。
func trayContext(estimate, window int64) string {
	if percent := contextPercent(estimate, window); percent >= 0 {
		return fmt.Sprintf("%d%%", percent)
	}
	return ""
}

// contextPercent 是「这段上下文占模型窗口的百分之几」（四舍五入）；量不出来返回 -1。
func contextPercent(estimate, window int64) int {
	if estimate <= 0 || window <= 0 {
		return -1
	}
	return int(float64(estimate)/float64(window)*100 + 0.5)
}

// formatCost 排成 "¥0.420"；拿不到就不显示（空字符串），而不是画一个 "¥0"——那看起来像
// 「这次没花钱」，而真相是「还没算」。
//
// 单位和位数**跟着桌面端已有的那份来**（App.tsx 的 `¥{cost.toFixed(3)}`）：这个 cost 是
// 人民币，而单次调用可能只有几厘，所以是三位小数而不是两位。两处显示同一个数却长得不一样，
// 是比「尾随的零不好看」更糟的问题。
func formatCost(v any) string {
	f := toFloat(v)
	if f <= 0 {
		return ""
	}
	return fmt.Sprintf("¥%.3f", f)
}

// toFloat 是给 map[string]any 用的：费用数据装在 any 里，而 JSON 之外没人保证它是哪个宽度。
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float32:
		return float64(n)
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
