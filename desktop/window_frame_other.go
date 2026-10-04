//go:build !darwin

package main

// framelessWindow: off macOS the window draws NO native frame, so the app's own bar IS the
// window's chrome.
//
// Two reasons, and the second is the one that decides it:
//
//   - Wails draws GTK's default title bar — a headerbar carrying the app name and a close button
//     — ABOVE the webview, and the app draws its own titlebar directly below it. Two stacked
//     bars, the upper one saying nothing the lower one does not.
//   - The frame is also where the window's controls lived, and both replacements are the
//     runtime's own: dragging is the `--wails-draggable` region on the app's bar (the frontend
//     puts one on every full-height column's top strip, which is the room macOS paints its
//     traffic lights into), and the window's EDGE RESIZE only exists while frameless — the
//     runtime gates that path on `flags.frameless` (drag.ts), which is set from this option.
//
// Hiding the window is then the titlebar's own button: the window is tray-resident, and a close
// that hides rather than quits is the bargain macOS's red light already makes (main.go's close
// hook), so the button is that same gesture, spelled out.
const framelessWindow = true
