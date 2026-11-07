package main

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// The rules that decide whether a remembered window frame is still usable. Every case here is one
// the reader would otherwise meet as "my window opened somewhere I cannot reach it".

func oneScreen() []screenArea {
	// The laptop screen this suite runs on: 1512x982, work area under the menu bar.
	return []screenArea{{X: 0, Y: 25, Width: 1512, Height: 930}}
}

func TestUsableFrameKeepsAGoodFrame(t *testing.T) {
	f := windowFrame{X: 100, Y: 100, Width: 1200, Height: 780}
	got, positionOK, ok := usableFrame(f, oneScreen())
	if !ok || !positionOK {
		t.Fatalf("a frame inside the display must be restored as it is: ok=%v positionOK=%v", ok, positionOK)
	}
	if got != f {
		t.Fatalf("frame changed: got %+v, want %+v", got, f)
	}
}

func TestUsableFrameDropsAPositionOnAnUnpluggedDisplay(t *testing.T) {
	// Written while a second display sat to the right; only the laptop is here now.
	f := windowFrame{X: 2400, Y: 200, Width: 1200, Height: 780}
	got, positionOK, ok := usableFrame(f, oneScreen())
	if !ok {
		t.Fatal("the SIZE is still a real choice and must survive; only the position expired")
	}
	if positionOK {
		t.Fatal("a position on a display that is gone must not be reported as usable")
	}
	if got.Width != f.Width || got.Height != f.Height {
		t.Fatalf("the reader's size must be kept: got %dx%d, want %dx%d", got.Width, got.Height, f.Width, f.Height)
	}
}

func TestUsableFrameRejectsAFrameWiderThanTheWindowAllows(t *testing.T) {
	// A hand edit (or a truncated file) that the window itself would refuse.
	for _, f := range []windowFrame{
		{Width: minWindowWidth - 1, Height: 800},
		{Width: 1200, Height: minWindowHeight - 1},
		{Width: maxWindowWidth + 1, Height: 800},
		{Width: 1200, Height: maxWindowHeight + 1},
	} {
		if _, _, ok := usableFrame(f, oneScreen()); ok {
			t.Fatalf("frame %+v is outside the window's own bounds and must be refused", f)
		}
	}
}

func TestUsableFrameNeedsEnoughOfTheTitleBarOnScreen(t *testing.T) {
	// Nudged so only a sliver of the LEFT edge is on screen: the drag handle is off, so it cannot be
	// grabbed — that is the case the minimum exists for.
	f := windowFrame{X: -(1200 - minimumVisibleWidth + 1), Y: 100, Width: 1200, Height: 780}
	if _, positionOK, _ := usableFrame(f, oneScreen()); positionOK {
		t.Fatal("less than a grabbable strip on screen must not count as a usable position")
	}

	// Exactly the minimum is enough.
	f.X = -(1200 - minimumVisibleWidth)
	if _, positionOK, _ := usableFrame(f, oneScreen()); !positionOK {
		t.Fatal("a grabbable strip on screen must count as usable")
	}
}

func TestUsableFrameRejectsAPositionAboveEveryScreen(t *testing.T) {
	// The title bar hangs above the top of every display: the body may be visible, the handle is not.
	f := windowFrame{X: 100, Y: 25 - 780 + 100, Width: 1200, Height: 780}
	if _, positionOK, _ := usableFrame(f, oneScreen()); positionOK {
		t.Fatal("a window whose title bar is above the screen cannot be grabbed and must be re-placed")
	}
}

func TestUsableFrameWithNoScreenInformationKeepsTheSizeOnly(t *testing.T) {
	// "Cannot tell" must not be read as "the position is fine": a platform that reports no screens
	// would otherwise leave the window wherever the file said, unverified.
	f := windowFrame{X: 100, Y: 100, Width: 1200, Height: 780}
	got, positionOK, ok := usableFrame(f, nil)
	if !ok {
		t.Fatal("an unverifiable position must not throw the size away")
	}
	if positionOK {
		t.Fatal("an unverifiable position must not be reported as usable")
	}
	if got.Width != f.Width || got.Height != f.Height {
		t.Fatalf("size lost: %+v", got)
	}
}

func TestUsableFrameOnASecondDisplay(t *testing.T) {
	// A display to the right and BELOW the primary's origin: the frame has to be judged against the
	// display it is on, not against the primary.
	screens := []screenArea{
		{X: 0, Y: 25, Width: 1512, Height: 930},
		{X: 1512, Y: 300, Width: 1920, Height: 1080},
	}
	f := windowFrame{X: 1600, Y: 400, Width: 1200, Height: 780}
	if _, positionOK, _ := usableFrame(f, screens); !positionOK {
		t.Fatal("a frame fully inside the second display must be usable")
	}
	// Vertically above the second display's top edge and past the primary's right edge: nowhere.
	f.Y = 100
	if _, positionOK, _ := usableFrame(f, screens); positionOK {
		t.Fatal("a frame that overlaps no display must not be usable")
	}
}

func TestWindowFrameSurvivesARoundTripThroughTheFile(t *testing.T) {
	// The stored frame is read back at launch (startupFrame) and written by the tracker, so the two
	// have to agree about what "no frame" means: an absent field is not a zero frame.
	config.SetBaseDir(t.TempDir())

	if _, ok := startupFrame(); ok {
		t.Fatal("a fresh preferences file must not offer a frame")
	}

	stored := windowFrame{X: 40, Y: 60, Width: 1400, Height: 900}
	(&desktopApp{}).SetWindowFrame(stored)
	got, ok := startupFrame()
	if !ok {
		t.Fatal("a frame just stored must be offered back")
	}
	if got != stored {
		t.Fatalf("round trip changed the frame: got %+v, want %+v", got, stored)
	}

	// A frame the window could never have had is refused at the door rather than stored.
	(&desktopApp{}).SetWindowFrame(windowFrame{X: 0, Y: 0, Width: 10, Height: 10})
	if kept, _ := startupFrame(); kept != stored {
		t.Fatalf("a nonsense frame must not replace the stored one: now %+v", kept)
	}
}
