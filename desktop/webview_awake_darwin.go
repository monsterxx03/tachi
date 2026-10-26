//go:build darwin && !headless

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

// What keepPageAwake reports when the throttling could not be lifted. All three are
// TRANSIENT: the NSWindow and its WKWebView are created on the main thread from the Wails
// window's own run(), i.e. after NewWithOptions has already returned, and the runtime the
// dispatch itself needs (Wails' platform impl) does not exist until App.Run — so a call that
// lands too early legitimately finds nothing to talk to yet.
const (
	reasonNoWindow  = "no native window"
	reasonNoWebView = "webview not found in the window"
	reasonNoSwitch  = "neither the occlusion-detection nor the timer-throttling switch answered"
	// reasonNotReady is the answer for a dispatch issued before App.Run() has built the
	// runtime — see awakeOnce, which is what keeps that from being fatal.
	reasonNotReady = "app runtime not up yet"
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
		reason := awakeOnce(window)
		if reason != reasonNoWindow && reason != reasonNoWebView && reason != reasonNotReady {
			return reason
		}
		if time.Now().After(deadline) {
			return reason
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awakeOnce is one dispatch, and it never panics.
//
// The dispatch dereferences Wails' platform impl, which is nil until App.Run() creates it, so
// a call that lands before the runtime exists is a nil-pointer panic INSIDE the Wails call —
// and a panic in any goroutine takes the whole process down. From outside that is invisible
// and indistinguishable from "the app did not start": the window never loads, the driver
// never runs, the runner reports a silent launch. It is therefore converted into the
// retryable answer above, which is a line the driver's own report can name.
//
// It is a safety net, not the plan: main.go now starts the demo from
// ApplicationDidFinishLaunching, which is the first moment the runtime is up. This is what
// stands between a future early caller and a dead app.
func awakeOnce(window *application.WebviewWindow) (reason string) {
	defer func() {
		if r := recover(); r != nil {
			reason = reasonNotReady
		}
	}()
	return application.InvokeSyncWithResult(func() string { return applyAwakeSwitches(window) })
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
