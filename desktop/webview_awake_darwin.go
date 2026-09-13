//go:build darwin

package main

/*
#cgo CFLAGS: -mmacosx-version-min=10.13 -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

// A scripted run is driven while the machine is in use, so its window spends most of the
// run COVERED by whatever the user is working in. WebKit reads that state as "the page is
// not visible": PageClientImpl::isViewVisible() asks only that the window be on screen and
// not occluded — it never looks at whether the app is frontmost — and a page it calls
// invisible loses both its animation frames and its timers (Page::updateTimerThrottlingState
// keys off ActivityState::IsVisible plus the visually-idle flag, which a background app
// always carries: PageClientImpl::isVisuallyIdle() is true whenever the app has no active
// window on screen). A driver that polls would then crawl and time out.
//
// Two switches turn that inference off. Both are SPI — the headers are private — so they are
// declared here and called only after a respondsToSelector: check.
@interface WKWebView (TachiDemoAwake)
- (void)_setWindowOcclusionDetectionEnabled:(BOOL)enabled;
@end

@interface WKPreferences (TachiDemoAwake)
- (void)_setHiddenPageDOMTimerThrottlingEnabled:(BOOL)enabled;
@end

// Wails' window carries its WKWebView in a property of the same name; the header belongs to
// the Wails module, so the shape is declared here rather than imported.
@interface NSWindow (TachiDemoAwakeWindow)
@property (readonly) WKWebView *webView;
@end

// Fallback for a window that does not expose the property: the webview is the first
// WKWebView anywhere in the hierarchy (Wails reparents it when the window has a glass or
// notch effect view).
static WKWebView *tachiFindWebView(NSView *view)
{
    if ([view isKindOfClass:[WKWebView class]])
        return (WKWebView *)view;
    for (NSView *subview in [view subviews]) {
        WKWebView *found = tachiFindWebView(subview);
        if (found)
            return found;
    }
    return nil;
}

// tachiApplyAwakeSwitches returns why the page could NOT be unthrottled — 0 means it worked.
//
// Called on the main thread only: it walks AppKit's view hierarchy and changes WebKit's page
// policy, and off-main the webview looks missing (measured: a call from a goroutine answered
// "no webview" while the same call from main found it).
//
// A window that was never ordered in is put on screen here, at the BOTTOM of the window stack
// and without becoming key: a no-activate run is created hidden (see demoNoActivate), so this
// is the only thing that shows it — `orderWindow:NSWindowBelow relativeTo:0` makes it visible
// (all the page needs; the occlusion switch above covers being covered) while neither covering
// anyone's work nor making the app active, which `makeKeyAndOrderFront:` (what Wails itself
// uses) would.
static int tachiApplyAwakeSwitches(void *nativeWindow)
{
    NSWindow *window = (NSWindow *)nativeWindow;
    if (!window)
        return 1;
    if (!window.isVisible)
        [window orderWindow:NSWindowBelow relativeTo:0];

    WKWebView *webView = nil;
    if ([window respondsToSelector:@selector(webView)])
        webView = window.webView;
    if (!webView) {
        NSView *content = [window contentView];
        if (content)
            webView = tachiFindWebView(content);
    }
    if (!webView)
        return 2;

    bool unthrottled = false;
    if ([webView respondsToSelector:@selector(_setWindowOcclusionDetectionEnabled:)]) {
        [webView _setWindowOcclusionDetectionEnabled:NO];
        unthrottled = true;
    }
    WKPreferences *preferences = webView.configuration.preferences;
    if ([preferences respondsToSelector:@selector(_setHiddenPageDOMTimerThrottlingEnabled:)]) {
        [preferences _setHiddenPageDOMTimerThrottlingEnabled:NO];
        unthrottled = true;
    }
    return unthrottled ? 0 : 3;
}
*/
import "C"

import (
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The C function's return codes.
const (
	awakeOK        = 0
	awakeNoWindow  = 1
	awakeNoWebView = 2
	awakeNoSwitch  = 3
)

// What keepPageAwake reports when the throttling could not be lifted. The first two are
// TRANSIENT: the NSWindow and its WKWebView are created on the main thread from the Wails
// window's own run(), i.e. after NewWithOptions has already returned, so a call that lands
// too early legitimately finds no window yet.
const (
	reasonNoWindow  = "no native window"
	reasonNoWebView = "webview not found in the window"
	reasonNoSwitch  = "neither the occlusion-detection nor the timer-throttling switch answered"
)

// keepPageAwake lifts WebKit's occlusion throttling so the page keeps running while the
// window is covered by another app's window — which is where a scripted run happens (see the
// preamble for what WebKit does by default and why). It returns "" on success, otherwise the
// reason; demo.go hands that to the driver, which asserts on it, so a run that lost the
// switch fails naming its cause instead of as a pile of timeouts.
//
// Demo mode only: a real session must follow the machine's power behaviour, and its window is
// in front anyway. Two things it must get right, both measured:
//   - WAIT for the window and its webview, retrying the transient answers, because they are
//     created on the main thread after NewWithOptions returns;
//   - do the work ON THE MAIN THREAD — it talks to AppKit and to WebKit, and off-main the
//     webview looks missing.
func keepPageAwake(window *application.WebviewWindow) string {
	deadline := time.Now().Add(2 * time.Second)
	for {
		reason := application.InvokeSyncWithResult(func() string { return applyAwakeSwitches(window) })
		if reason != reasonNoWindow && reason != reasonNoWebView {
			return reason
		}
		if time.Now().After(deadline) {
			return reason
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// applyAwakeSwitches is ONE attempt, on the current (main) thread.
func applyAwakeSwitches(window *application.WebviewWindow) string {
	if window == nil {
		return reasonNoWindow
	}
	native := window.NativeWindow()
	if native == nil {
		return reasonNoWindow
	}
	switch int(C.tachiApplyAwakeSwitches(native)) {
	case awakeOK:
		return ""
	case awakeNoWebView:
		return reasonNoWebView
	case awakeNoSwitch:
		return reasonNoSwitch
	default:
		return "unexpected failure"
	}
}
