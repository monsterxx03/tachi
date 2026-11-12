//go:build darwin && !headless

package main

/*
#cgo CFLAGS: -x objective-c -mmacosx-version-min=10.13
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

// cursorPoint reports where the pointer is, in the coordinate space the window manager uses:
// points, origin at the TOP-LEFT of the main display.
//
// AppKit measures from the bottom-left of the main display instead, so the y is flipped against that
// display's height. Points (not pixels) is also what the window's placement wants: two displays with
// different scale factors still share one point grid, which is exactly why a mixed-retina desk does
// not need special handling here.
static int cursorPoint(double *x, double *y) {
    @autoreleasepool {
        NSArray<NSScreen *> *screens = [NSScreen screens];
        if (screens.count == 0) {
            return 1;
        }
        NSPoint p = [NSEvent mouseLocation];
        CGFloat mainHeight = screens[0].frame.size.height;
        *x = p.x;
        *y = mainHeight - p.y;
        return 0;
    }
}

// TachiLauncherScreen is one display, in the same top-left point space as cursorPoint: `b*` is the
// whole screen, the rest is its work area (the screen minus the menu bar and the Dock).
typedef struct {
    int bx, by, bw, bh;
    int x, y, w, h;
} TachiLauncherScreen;

// screenAreas fills `out` with the displays attached RIGHT NOW and returns how many it wrote.
//
// This asks AppKit instead of the window manager's screen list on purpose. That list is assembled by
// the framework and refreshed on display-change events; when it goes stale the work areas keep the
// geometry of a monitor that is no longer attached, and a panel placed from it lands somewhere the
// reader cannot see (a 4K display's points and pixels differ by the scale factor, so the error is
// thousands of points, not a few). Asking NSScreen every time cannot go stale.
static int screenAreas(TachiLauncherScreen *out, int max) {
    @autoreleasepool {
        NSArray<NSScreen *> *screens = [NSScreen screens];
        if (screens.count == 0) {
            return 0;
        }
        CGFloat mainHeight = screens[0].frame.size.height;
        int n = 0;
        for (NSScreen *screen in screens) {
            if (n >= max) {
                break;
            }
            NSRect bounds = screen.frame;
            NSRect work = screen.visibleFrame;
            out[n].bx = (int)bounds.origin.x;
            out[n].by = (int)(mainHeight - bounds.origin.y - bounds.size.height);
            out[n].bw = (int)bounds.size.width;
            out[n].bh = (int)bounds.size.height;
            out[n].x = (int)work.origin.x;
            out[n].y = (int)(mainHeight - work.origin.y - work.size.height);
            out[n].w = (int)work.size.width;
            out[n].h = (int)work.size.height;
            n++;
        }
        return n;
    }
}
*/
import "C"

// launcherCursor returns the pointer's position in the space Screen.Bounds uses, or false when the
// platform cannot say (no screen, no AppKit).
func launcherCursor() (int, int, bool) {
	var x, y C.double
	if C.cursorPoint(&x, &y) != 0 {
		return 0, 0, false
	}
	return int(x), int(y), true
}

// launcherScreens reports every display attached right now, in the same point space as
// launcherCursor. Empty means the platform could not say.
func launcherScreens() []launcherScreenArea {
	const maxScreens = 16
	var raw [maxScreens]C.TachiLauncherScreen
	count := int(C.screenAreas(&raw[0], maxScreens))
	out := make([]launcherScreenArea, 0, count)
	for i := 0; i < count; i++ {
		s := raw[i]
		out = append(out, launcherScreenArea{
			Bounds: launcherArea{X: int(s.bx), Y: int(s.by), Width: int(s.bw), Height: int(s.bh)},
			Work:   launcherArea{X: int(s.x), Y: int(s.y), Width: int(s.w), Height: int(s.h)},
		})
	}
	return out
}
