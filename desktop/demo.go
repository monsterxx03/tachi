package main

import (
	"fmt"
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

// runJsDemo drives the WebView DOM via Wails ExecJS — a reliable way to
// "type into the input and send" without depending on system-level mouse and
// keyboard coordinates. The WebView content is not exposed to the Accessibility
// tree, so ExecJS (running directly in the page) is the robust automation path.
//
// It is gated behind the TACHI_DEMO=1 env var so normal launches are unaffected.
func runJsDemo(window *application.WebviewWindow) {
	// Give the webview a moment to load the React app.
	time.Sleep(2 * time.Second)

	messages := []string{
		"帮我测试一下桌面端的输入与状态流转",
		"再发一条，观察执行工具的流程",
		"最后一条，回归空闲",
	}
	for _, msg := range messages {
		if window == nil {
			return
		}
		window.ExecJS(demoScript(msg))
		time.Sleep(7 * time.Second)
	}
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
