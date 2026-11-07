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
