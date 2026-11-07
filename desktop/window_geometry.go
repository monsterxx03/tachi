package main

// Where the window was last time, and whether that place still exists.
//
// A remembered frame is the one piece of layout state that can become WRONG without anyone
// touching it: the display it was on gets unplugged, the machine changes resolution, a laptop
// comes home to a different desk. Restoring a frame that no longer fits means a window half off
// the screen — or entirely off it, which is worse than a wrong default because there is nothing
// to drag back. So what is STORED is exactly what the reader had, and what is USED is checked
// against the displays that are actually here.
//
// The check runs in two places, because a window exists before the platform has told Wails which
// screens are attached (main.go creates the window, the screen cache is filled on the same launch
// event that ApplicationStarted maps to). So the window is CREATED at the stored frame — the common
// case, a display that is still plugged in, must not flicker — and the frame is verified once the
// screens are known; a position that does not survive that check is replaced by the default.
//
// Everything here is plain geometry, so the headless build (which shares ui_state_file.go and has
// no window at all) compiles it too.

// windowFrame is a remembered window rectangle, in the same DIP coordinates Wails reports from
// Window.Bounds() (origin at the top-left of the primary display's top-left corner).
type windowFrame struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// The window's own size bounds — the ones Wails is given at creation (main.go) and the ones a
// remembered frame is checked against, because a frame the window itself would refuse is not
// something a reader can have chosen. The default is what a reader who has never dragged one gets,
// and it is also demoWindowSize's fallback (demo.go).
const (
	minWindowWidth  = 900
	minWindowHeight = 600
	// The maxima are sanity bounds for a hand-edited file rather than a real limit (nothing else
	// in the app caps the window): the largest display in use is well under 8K, and a bigger frame
	// is a typo, not a preference.
	maxWindowWidth  = 8192
	maxWindowHeight = 8192

	defaultWindowWidth  = 1200
	defaultWindowHeight = 780
)

// screenArea is one display's usable rectangle (its work area: no menu bar, no dock). A private
// copy of the fields of Wails' Screen.WorkArea, so the rules below can be tested without a window
// server.
type screenArea struct {
	X      int
	Y      int
	Width  int
	Height int
}

// minimumVisibleWidth is how much of the window must land on SOME display for a remembered position
// to count as usable. The point is not tidiness: a window whose title bar is off every screen cannot
// be grabbed any more, so the reader's only escape is editing the preferences file by hand. A narrow
// strip being reachable is enough to drag it back.
const minimumVisibleWidth = 240

// usableFrame reports whether a remembered frame can be restored as it is, and why not when it
// cannot. The size and the position are judged separately:
//
//   - a size outside the window's own bounds is not a real choice → not usable at all;
//   - a position on no attached display loses the position but keeps the size (the unplugged-monitor
//     case: what is above the reader's desk changed, their preferred window size did not).
//
// screens is the work areas attached NOW; an empty slice means "cannot tell", which is treated as
// "position unusable" — the safe direction, since the window then opens where the OS puts it.
func usableFrame(frame windowFrame, screens []screenArea) (w windowFrame, positionOK bool, ok bool) {
	if frame.Width < minWindowWidth || frame.Height < minWindowHeight ||
		frame.Width > maxWindowWidth || frame.Height > maxWindowHeight {
		return windowFrame{}, false, false
	}
	if !frameOnAnyScreen(frame, screens) {
		return windowFrame{Width: frame.Width, Height: frame.Height}, false, true
	}
	return frame, true, true
}

// frameOnAnyScreen reports whether at least minimumVisibleWidth of the frame's top edge lands inside
// a display. The TOP edge is what matters, not the centre: the title bar (or, off macOS, the toolbar
// that doubles as the drag handle) is the only part of a window the reader can grab, so a frame whose
// bar hangs above every screen is unreachable however much of its body is visible.
func frameOnAnyScreen(frame windowFrame, screens []screenArea) bool {
	for _, s := range screens {
		if s.Width <= 0 || s.Height <= 0 {
			continue
		}
		if frame.Y < s.Y || frame.Y >= s.Y+s.Height {
			continue
		}
		left := maxInt(frame.X, s.X)
		right := minInt(frame.X+frame.Width, s.X+s.Width)
		if right-left >= minimumVisibleWidth {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
