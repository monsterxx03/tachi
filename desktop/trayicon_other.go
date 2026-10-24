//go:build !darwin && !headless

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// trayClickOpensMenu: on Linux the StatusNotifier host (omarchy's bar, waybar, …)
// shows the menu itself when the icon is clicked, so the app must NOT also try —
// OpenMenu is unimplemented on Linux, so a click handler would only swallow the
// click and log an error on every press (measured: "openMenu not implemented on
// Linux" on each click).
const trayClickOpensMenu = false

// applyTrayIcon paints the state icon onto the tray item.
//
// Not the template icon: the shapes in menuicon/ are BLACK, and only macOS recolours
// a template image. A tray host here draws the PNG as-is, so a black glyph on a dark
// panel is invisible — which is exactly what the first Linux run showed. So the
// monochrome shape is kept and TINTED:
//
//   - the resting state follows the panel (light glyph on a dark desktop, dark glyph
//     on a light one), so the icon is always visible at rest;
//   - the working states get a saturated colour each, which reads on either panel and
//     is what now carries the state, since the menu-bar TEXT is gone here (SetLabel
//     does not reach a Linux tray host — see setupTray and refreshTray).
func applyTrayIcon(tray *application.SystemTray, status AgentStatus) {
	raw := trayIcon(status)
	if raw == nil {
		return
	}
	darkPanel := true
	if app := application.Get(); app != nil {
		darkPanel = app.Env.IsDarkMode()
	}
	tray.SetIcon(tintTrayIcon(raw, trayIconColour(status, darkPanel)))
}

// trayIconColour is the tint for one state. The working colours are picked to hold up
// on a light AND a dark panel rather than to match the app's own theme: a tray icon
// has to survive whatever bar the user runs.
func trayIconColour(status AgentStatus, darkPanel bool) color.NRGBA {
	switch status {
	case StatusThinking:
		return color.NRGBA{R: 0x4c, G: 0x9a, B: 0xf0, A: 0xff} // blue
	case StatusToolRunning:
		return color.NRGBA{R: 0xe0, G: 0xa3, B: 0x2e, A: 0xff} // amber
	case StatusBusy:
		return color.NRGBA{R: 0xe8, G: 0x7c, B: 0x3f, A: 0xff} // orange
	case StatusError:
		return color.NRGBA{R: 0xe0, G: 0x5c, B: 0x5c, A: 0xff} // red
	default: // idle
		if darkPanel {
			return color.NRGBA{R: 0xd8, G: 0xde, B: 0xe9, A: 0xff} // light glyph
		}
		return color.NRGBA{R: 0x3b, G: 0x42, B: 0x52, A: 0xff} // dark glyph
	}
}

// tintTrayIcon recolours every pixel of a monochrome PNG to c, keeping the alpha
// channel (i.e. the shape). A decode/encode failure falls back to the original bytes:
// a black icon still beats no icon at all.
func tintTrayIcon(data []byte, c color.NRGBA) []byte {
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	bounds := src.Bounds()
	dst := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := src.At(x, y).RGBA()
			dst.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: uint8(a >> 8)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return data
	}
	return buf.Bytes()
}
