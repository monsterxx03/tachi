package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// demoEnabled reports whether the self-test driver should run. It is triggered
// by the TACHI_DEMO=1 env var, or by a marker file at demoFlagPath (which makes
// it usable with `open`, since `open` cannot pass environment variables).
func demoEnabled() bool {
	if os.Getenv("TACHI_DEMO") == "1" {
		return true
	}
	if _, err := os.Stat(demoFlagPath); err == nil {
		return true
	}
	return false
}

const demoFlagPath = "/tmp/tachi-demo.flag"

// The demo bootstrap's two delays, in order: enough for the app's main loop to start (a
// main-thread dispatch that lands before it never runs), then the page's own time to load
// before the driver is injected. Together they are the ~2s the driver has always waited for.
const (
	demoBootstrapDelay = 500 * time.Millisecond
	demoLoadDelay      = 1500 * time.Millisecond
)

// demoNoActivateEnv keeps a scripted run from taking the foreground. The smoke suite sets it,
// because it launches the app once per scenario and each launch used to steal the foreground
// from whoever was working — whatever they were typing at that instant landed in the smoke's
// own composer. Three things have to hold together, and each was measured on its own: the
// runner also launches with `open -g` (LaunchServices must not ask the app to activate), the
// app starts as an ACCESSORY app (Wails activates a Regular one itself from
// ApplicationDidFinishLaunching — with a Regular policy `open -g` alone still brought the app
// to the front ~1.4s in), and its window is created hidden and put on screen by the demo
// bootstrap without becoming key (see webview_awake_darwin.go). A hand-run (a screenshot of a
// specific screen) leaves it unset, so its window still comes to the front as before.
const demoNoActivateEnv = "TACHI_DEMO_NO_ACTIVATE"

// activationPolicy is Regular, except for a scripted run that asks not to be activated.
func activationPolicy() application.ActivationPolicy {
	if demoNoActivate() {
		return application.ActivationPolicyAccessory
	}
	return application.ActivationPolicyRegular
}

// demoNoActivate reports whether this run must keep its hands off the foreground.
func demoNoActivate() bool { return os.Getenv(demoNoActivateEnv) == "1" }

// runJsDemo drives the WebView DOM via Wails ExecJS — a reliable way to
// "type into the input and send" without depending on system-level mouse and
// keyboard coordinates. The WebView content is not exposed to the Accessibility
// tree, so ExecJS (running directly in the page) is the robust automation path.
//
// It is gated behind the TACHI_DEMO=1 env var so normal launches are unaffected.
// TACHI_DEMO_JS=<file> runs that file's JS instead of the canned messages, which
// is how a SPECIFIC screen gets driven (open a session, expand an attachment
// preview, open the lightbox …) while screenshots are taken from outside.
// unthrottleReason is what keepPageAwake reported: "" when the page can no longer be
// throttled, otherwise why it still can — handed to the driver, which asserts on it.
func runJsDemo(window *application.WebviewWindow) {
	if window == nil {
		return
	}
	// First let the app's main loop start: the two steps below are dispatched onto the main
	// thread, and a dispatch that lands before the loop runs is never executed at all — the
	// driver goroutine then waits forever and the run reports nothing (measured: a probe app
	// launched with the bootstrap moved to t=0 never reached its own driver).
	time.Sleep(demoBootstrapDelay)

	// Get the window on screen and lift WebKit's occlusion throttling BEFORE the driver runs.
	// A scripted run happens on a machine that is in use, so its window WILL be covered by
	// whatever the user is working in, and WebKit reads a covered window as a hidden page —
	// frames stop, timers clamp to 1Hz. This also shows the window (a no-activate run is
	// created hidden, see demoNoActivate) with `orderFront:`, i.e. visible but never key.
	//
	// It has to happen BEFORE the wait below rather than after it: the page needs that time to
	// load, and a driver injected into a document that is still loading is simply lost
	// (measured: `compact`, the fixture slowest to load, timed out with nothing reported while
	// the other scenarios passed).
	//
	// A run that lost the switch must fail on one readable line instead of as a pile of
	// timeouts (see webview_awake_darwin.go and itest/desktop/drivers/harness.js). The verdict
	// rides in the SAME ExecJS call as the driver: a separate call is dispatched on its own,
	// and Wails sends a script queued while the runtime is still loading in a different order
	// than the next one — which loses the flag.
	reason := keepPageAwake(window)

	// Give the webview a moment to load the React app.
	time.Sleep(demoLoadDelay)

	if script, ok := demoScriptFromFile(); ok {
		window.ExecJS(fmt.Sprintf(
			"window.__tachiDemoUnthrottled = %t; window.__tachiDemoUnthrottleError = %q;\n%s",
			reason == "", reason, script))
		return
	}

	messages := []string{
		"帮我测试一下桌面端的输入与状态流转",
		"再发一条，观察执行工具的流程",
		"最后一条，回归空闲",
	}
	for _, msg := range messages {
		window.ExecJS(demoScript(msg))
		time.Sleep(7 * time.Second)
	}
}

// demoScriptFromFile reads the TACHI_DEMO_JS script, if one is configured. The
// script owns its own timing (setTimeout/setInterval): it runs as soon as the app
// has had its two seconds to mount.
func demoScriptFromFile() (string, bool) {
	path := os.Getenv("TACHI_DEMO_JS")
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("TACHI_DEMO_JS: %v", err)
		return "", false
	}
	return string(data), true
}

// demoScript returns a JS snippet that fills the composer textarea the way a
// real user would (native value setter + input event so React's onChange fires),
// then clicks the send button once the React state has settled.
func demoScript(msg string) string {
	quoted := strconv.Quote(msg)
	return fmt.Sprintf(`(function(){
		var attempts = 0;
		var timer = setInterval(function(){
			var ta = document.querySelector('.composer-input');
			if (ta || attempts > 40) {
				clearInterval(timer);
				if (!ta) return;
				var setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
				setter.call(ta, %s);
				ta.dispatchEvent(new Event('input', { bubbles: true }));
				setTimeout(function(){
					var btn = document.querySelector('.send-btn');
					if (btn) btn.click();
				}, 250);
			}
			attempts++;
		}, 100);
	})();`, quoted)
}
