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

// The demo bootstrap's two waits, in order: enough for the app's main loop to start (a
// main-thread dispatch that lands before it never runs), then the page's own time to load
// before the driver's FIRST injection attempt.
//
// A too-small BOOTSTRAP is not slow, it is SILENT: a dispatch that lands before the main loop
// runs is never executed at all — the driver goroutine then waits forever and the run reports
// nothing (measured: a probe app launched with the bootstrap moved to t=0 never reached its own
// driver). It is a floor, and the shipped 250ms keeps ~3x its measured need; turn it down only
// with the grid to hand (`TACHI_DEMO_*_MS` lets the same binary be re-measured).
//
// The LOAD wait no longer has to be right on its own: an injection that lands while the document
// is still loading is silently dropped, so the driver is injected repeatedly for a window instead
// of once (see demoInjectWindow). This one is now only how much of a head start the first attempt
// gets — the loading page is covered by the retries, not by this number.
//
// They are NOT free time, and their sum is the floor on EVERY smoke scenario — the suite
// launches the app once per scenario, so this is paid 28 times a run. Both used to be several
// times larger and were trimmed against measurement: with `TACHI_DEMO_*_MS` set, the same binary
// was run at a grid of values and the whole suite re-run at each viable point. What the grid says
// is that they trade against a SINGLE budget — the moment of injection, ~250ms after launch — so
// shrinking one is not compensated by the other: the app is launched with `open`, whose `--env`
// inherits this process's environment, which is what makes the knobs reachable at all.
var (
	demoBootstrapDelay = envDuration("TACHI_DEMO_BOOTSTRAP_MS", 250)
	demoLoadDelay      = envDuration("TACHI_DEMO_LOAD_MS", 500)
)

// demoInjectWindow is how long the driver keeps being injected to, and demoInjectStep how often.
//
// A script injected into a document that is still loading is simply lost, and Wails drops an
// ExecJS issued before the window's impl exists — so a single shot after a fixed sleep is a race
// against the machine, and losing it is SILENT: that run reports no assertions at all after
// burning its whole per-scenario budget (measured twice on a machine that was busy compiling —
// the app was up, its session directory created, and nothing ever arrived).
//
// What is being waited for inside this window is the React app's mount, whose length depends on
// the machine and on what else is running, so the window is generous and the repeats are
// idempotent (the payload guards on `window.__tachiDriverStarted`, see runJsDemo): the first
// attempt that lands runs the driver and every later one returns immediately. The window is
// therefore the app's own goroutine time, not a per-attempt cost — the calls are no-ops once the
// driver is running.
var demoInjectWindow = envDuration("TACHI_DEMO_INJECT_MS", 10000)

// demoInjectStep is how often the driver is injected during that window: the distance between
// "the page just became ready" and "the driver started", which is the whole point of the retries.
const demoInjectStep = 200 * time.Millisecond

// envDuration reads a millisecond override for one of the waits above, or returns the
// default. It exists so the numbers can be re-measured (see the comment above) instead of
// being argued about, and so a slow or loaded machine has a way to widen them without a
// rebuild.
func envDuration(key string, defMS int) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Millisecond
		}
	}
	return time.Duration(defMS) * time.Millisecond
}

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

	// Give the webview a moment to load the React app — the first injection attempt's head
	// start, not a deadline: the attempts keep coming for demoInjectWindow (see demoInject).
	time.Sleep(demoLoadDelay)

	if script, ok := demoScriptFromFile(); ok {
		demoInject(window, reason, script)
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

// demoInject drives the file's script into the page, re-issuing it until the driver is running.
//
// The unthrottle verdict rides in the SAME ExecJS payload as the driver: a separate call is
// dispatched on its own, and Wails sends a script queued while the runtime is still loading in a
// different order than the next one — which loses the flag.
//
// The guard is what makes the repeats safe. A page that has not loaded yet drops the script, but
// one that lands twice would otherwise run the whole driver twice: two harnesses, every assertion
// posted twice, and a scenario whose result is whatever the race decided.
func demoInject(window *application.WebviewWindow, reason, script string) {
	payload := fmt.Sprintf(
		"(function(){\n"+
			"if (window.__tachiDriverStarted) return;\n"+
			"window.__tachiDriverStarted = true;\n"+
			"window.__tachiDemoUnthrottled = %t; window.__tachiDemoUnthrottleError = %q;\n"+
			"%s\n})()",
		reason == "", reason, script)

	deadline := time.Now().Add(demoInjectWindow)
	for attempt := 0; ; attempt++ {
		window.ExecJS(payload)
		// Worth a line the first time: it means the first attempt did NOT land (the page was
		// still loading), which is the flake this loop exists for — and how many attempts it
		// took is the measurement that says whether demoLoadDelay is still in the right place.
		if attempt == 1 {
			log.Printf("demo: driver injection retried (the first attempt did not take)")
		}
		if !time.Now().Add(demoInjectStep).Before(deadline) {
			return
		}
		time.Sleep(demoInjectStep)
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
