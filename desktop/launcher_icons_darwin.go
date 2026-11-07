//go:build darwin && !headless

package main

/*
#cgo CFLAGS: -x objective-c -mmacosx-version-min=10.13
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

// writeIconPNG draws a bundle's icon into a square PNG at `size` pixels and returns 0 on success.
//
// The icon comes from LaunchServices through NSWorkspace, not from a file in the bundle: modern
// apps ship their artwork in an asset catalog (Assets.car) rather than a .icns, so looking for
// Contents/Resources/*.icns finds nothing for a good half of them. The system has already resolved
// the right image for this display — this only asks for it and writes it down.
//
// Drawing is into a fresh bitmap of our own rather than through a view: there is no window here,
// and the panel is a webview that can only show a file.
static int writeIconPNG(const char *appPath, const char *outPath, int size) {
    @autoreleasepool {
        NSString *app = [NSString stringWithUTF8String:appPath];
        NSImage *icon = [[NSWorkspace sharedWorkspace] iconForFile:app];
        if (icon == nil) {
            return 1;
        }
        [icon setSize:NSMakeSize(size, size)];

        NSBitmapImageRep *rep = [[NSBitmapImageRep alloc]
            initWithBitmapDataPlanes:NULL
                          pixelsWide:size
                          pixelsHigh:size
                       bitsPerSample:8
                     samplesPerPixel:4
                            hasAlpha:YES
                            isPlanar:NO
                      colorSpaceName:NSDeviceRGBColorSpace
                         bytesPerRow:0
                        bitsPerPixel:0];
        if (rep == nil) {
            return 2;
        }
        NSGraphicsContext *ctx = [NSGraphicsContext graphicsContextWithBitmapImageRep:rep];
        if (ctx == nil) {
            return 3;
        }

        [NSGraphicsContext saveGraphicsState];
        [NSGraphicsContext setCurrentContext:ctx];
        [icon drawInRect:NSMakeRect(0, 0, size, size)
                fromRect:NSZeroRect
               operation:NSCompositingOperationSourceOver
                fraction:1.0];
        [NSGraphicsContext restoreGraphicsState];

        NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
        if (png == nil) {
            [rep release];
            return 4;
        }
        // Manual release: this file is compiled WITHOUT ARC, because cgo's flags are package-wide
        // and the package's other cgo file is written for manual retain/release.
        BOOL written = [png writeToFile:[NSString stringWithUTF8String:outPath] atomically:YES];
        [rep release];
        return written ? 0 : 5;
    }
}
*/
import "C"

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// writeAppIcon renders one bundle's icon to outPath. Every AppKit call runs on the main thread —
// NSWorkspace and NSImage are not ours to call from just any goroutine, and the panel is drawn by
// the same thread.
//
// The file is checked for after the dispatch: a call that lands before the runtime is up is
// swallowed (InvokeSync recovers the panic and returns), and "the drawing reported success" must
// therefore never be taken on trust — an icon recorded but not written is a row that draws a
// broken picture until the next run.
func writeAppIcon(appPath, outPath string, size int) error {
	cApp := C.CString(appPath)
	cOut := C.CString(outPath)
	defer C.free(unsafe.Pointer(cApp))
	defer C.free(unsafe.Pointer(cOut))

	var code C.int
	application.InvokeSync(func() {
		code = C.writeIconPNG(cApp, cOut, C.int(size))
	})
	if code != 0 {
		return fmt.Errorf("launcher: writing the icon for %s failed (step %d)", appPath, int(code))
	}
	if _, err := os.Stat(outPath); err != nil {
		return fmt.Errorf("launcher: the icon for %s was not written: %w", appPath, err)
	}
	return nil
}
