//go:build !headless

package main

import (
	"log"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// Restoring and remembering the window's frame.
//
// The reader's window is theirs: where it sits and how big it is should survive a restart, the way
// every other app on the machine does it. Three moments, and they are different problems:
//
//   - BEFORE the window exists, the stored frame is the starting point (startupFrame). The common
//     case — the reader's display is still plugged in — must not flicker, so the window is simply
//     created where they left it.
//   - ONCE the screens are known, that frame is a CLAIM to verify: the display it named may be gone
//     (window_geometry.go), and the window is moved back to a place it can be grabbed from.
//   - WHILE RUNNING, moves and resizes arrive continuously during a drag, so the frame is written
//     only after the window has been STILL for a moment (a write per frame would be hundreds of
//     file writes per drag), plus once when the window is closed.
//
// A scripted run is excluded entirely: its window size is a fixture (TACHI_DEMO_WINDOW), and a
// remembered frame would both override that fixture and write the fixture into the reader's real
// preferences file.

// frameSaveDelay is how long the window must be still before its frame is written. Long enough to
// swallow a drag, short enough that a reader who moves the window and quits gets what they chose.
const frameSaveDelay = 400 * time.Millisecond

// startupFrame is the frame to CREATE the window at, from the preferences file. The size is used
// whenever it is a real choice; the position too, unless a hand edit made it nonsense. Validation
// against the attached displays happens later (verifyFrame), because they are not known yet.
func startupFrame() (windowFrame, bool) {
	st := loadUIState()
	if st.WindowFrame == nil {
		return windowFrame{}, false
	}
	f := *st.WindowFrame
	if f.Width < minWindowWidth || f.Height < minWindowHeight ||
		f.Width > maxWindowWidth || f.Height > maxWindowHeight {
		return windowFrame{}, false
	}
	return f, true
}

// currentScreenAreas is the work area of every attached display, in the same DIP coordinates the
// window's frame uses. It answers nil while Wails has not filled its screen cache yet — which is the
// normal state DURING the first launch event, and the reason verifyFrame waits for the second.
func currentScreenAreas(app *application.App) []screenArea {
	if app == nil {
		return nil
	}
	screens := app.Screen.GetAll()
	areas := make([]screenArea, 0, len(screens))
	for _, s := range screens {
		areas = append(areas, screenArea{
			X:      s.WorkArea.X,
			Y:      s.WorkArea.Y,
			Width:  s.WorkArea.Width,
			Height: s.WorkArea.Height,
		})
	}
	return areas
}

// verifyFrame checks the frame the window was created with against the displays that exist, and
// moves the window if its position does not hold up. It keeps the reader's size either way: what
// changed under them is the desk, not their preference.
//
// screens being empty means "this platform has not told us" (a headless screen manager, an event
// that arrived before the cache was filled) and is not a reason to move anything — the window is
// already where the reader had it, and moving it on a guess would be worse than trusting it.
func verifyFrame(window *application.WebviewWindow, screens []screenArea) {
	if len(screens) == 0 {
		return
	}
	cur := window.Bounds()
	curFrame := windowFrame{X: cur.X, Y: cur.Y, Width: cur.Width, Height: cur.Height}
	fixed, positionOK, ok := usableFrame(curFrame, screens)
	if !ok {
		// The stored size was not a real one after all; fall back to the default entirely.
		fixed = windowFrame{Width: defaultWindowWidth, Height: defaultWindowHeight}
	}
	if positionOK {
		return
	}
	window.SetSize(fixed.Width, fixed.Height)
	window.Center()
	log.Printf("desktop window: remembered position was off every display; using the default one")
}

// frameTracker remembers the window's position after it settles.
type frameTracker struct {
	window *application.WebviewWindow
	desk   *desktopApp

	mu      sync.Mutex
	pending *time.Timer
}

// watchFrame starts remembering the window's frame. A scripted run never calls it (see the file
// comment).
func (d *desktopApp) watchFrame(window *application.WebviewWindow) {
	if window == nil {
		return
	}
	t := &frameTracker{window: window, desk: d}
	settle := func(*application.WindowEvent) { t.schedule() }
	window.OnWindowEvent(events.Common.WindowDidMove, settle)
	window.OnWindowEvent(events.Common.WindowDidResize, settle)
	// A close HIDES this window rather than destroying it (main.go's close hook), so a reader who
	// moves the window and then closes it would otherwise never have the new frame written. The
	// event fires before the hide, while the frame on screen is still the one to remember.
	window.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) { t.saveNow() })
}

// schedule (re)arms the settle timer: the write happens once the window has been still for
// frameSaveDelay, so a drag costs one write rather than one per frame.
func (t *frameTracker) schedule() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending != nil {
		t.pending.Stop()
	}
	t.pending = time.AfterFunc(frameSaveDelay, t.saveNow)
}

// saveNow writes the window's current frame, if it is one worth restoring.
func (t *frameTracker) saveNow() {
	t.mu.Lock()
	if t.pending != nil {
		t.pending.Stop()
		t.pending = nil
	}
	t.mu.Unlock()

	rect := t.window.Bounds()
	// Bounds() answers an empty rectangle for a window that is gone; storing that would erase the
	// reader's real frame on the next launch, so this guard and the bounds check are one test.
	if rect.Width < minWindowWidth || rect.Height < minWindowHeight {
		return
	}
	t.desk.SetWindowFrame(windowFrame{X: rect.X, Y: rect.Y, Width: rect.Width, Height: rect.Height})
}
