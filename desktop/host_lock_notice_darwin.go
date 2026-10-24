//go:build darwin && !headless

package main

import (
	"log"
	"os/exec"
	"strings"
)

// reportHostLocked tells the user why no window appeared.
//
// It cannot use Wails' own dialog: MessageDialog.Show dispatches onto the app's main thread, which
// does not exist until App.Run, and this failure happens before the app is built. So the platform's
// own alert does the talking — on macOS `osascript` needs no window, no app, and no permission.
// Without this the double-clicked app would simply vanish, which reads as "Tachi is broken".
func reportHostLocked(err error) {
	log.Print(err)
	// The message travels inside an AppleScript string literal: quotes and backslashes would end it
	// early, so they are replaced rather than escaped (the wording is ours and contains neither).
	msg := strings.NewReplacer(`"`, `'`, `\`, "/").Replace(err.Error())
	script := `display alert "Tachi 已经在运行" message "` + msg + `" as critical`
	_ = exec.Command("osascript", "-e", script).Run()
}
