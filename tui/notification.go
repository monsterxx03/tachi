package tui

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

const maxNotifyBodyLen = 200
const maxNotifyBodyRunes = maxNotifyBodyLen - 3 // reserve for "..."

var (
	terminalNotifierOnce sync.Once
	terminalNotifierPath string
)

// terminalNotifierAvailable returns the path to terminal-notifier if it's
// installed and on PATH. Result is cached after the first lookup.
func terminalNotifierAvailable() string {
	terminalNotifierOnce.Do(func() {
		if p, err := exec.LookPath("terminal-notifier"); err == nil {
			terminalNotifierPath = p
		}
	})
	return terminalNotifierPath
}

// notifyTerminal sends a desktop notification. Runs asynchronously in
// a goroutine — does not block the caller.
//
// macOS:
//   - Prefers terminal-notifier (brew install terminal-notifier) when
//     available.
//   - Falls back to osascript display notification when terminal-notifier
//     is not installed.
//
// Other platforms: uses OSC 9 terminal escape sequence, supported by
// iTerm2, Kitty, WezTerm, Ghostty, Warp. Terminals that don't support
// OSC 9 silently ignore the sequence.
func notifyTerminal(title, body string) {
	// Truncate body to a reasonable character (rune) count to avoid
	// splitting multi-byte UTF-8 characters.
	if len([]rune(body)) > maxNotifyBodyLen {
		runes := []rune(body)
		body = string(runes[:maxNotifyBodyRunes]) + "..."
	}

	go func() {
		if runtime.GOOS == "darwin" {
			if tn := terminalNotifierAvailable(); tn != "" {
				_ = exec.Command(tn, "-title", title, "-message", body).Run()
				return
			}

			// Fallback: osascript display notification.
			escapedTitle := strings.ReplaceAll(title, `\`, `\\`)
			escapedTitle = strings.ReplaceAll(escapedTitle, `"`, `\"`)
			escapedBody := strings.ReplaceAll(body, `\`, `\\`)
			escapedBody = strings.ReplaceAll(escapedBody, `"`, `\"`)

			script := `display notification "` + escapedBody +
				`" with title "` + escapedTitle + `"`
			_ = exec.Command("osascript", "-e", script).Run()
			return
		}

		// OSC 9 fallback: direct concatenation avoids fmt.Sprintf issues
		// when body contains literal '%' characters.
		osc9 := "\x1b]9;" + title + "\n" + body + "\x07"
		_ = os.WriteFile("/dev/tty", []byte(osc9), 0o666)
	}()
}

