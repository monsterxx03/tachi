//go:build !darwin && !headless

package main

import (
	"log"
	"os/exec"
)

// reportHostLocked tells the user why no window appeared.
//
// Wails' dialog cannot be used here: MessageDialog.Show dispatches onto the app's main thread,
// which does not exist until App.Run, and this failure happens before the app is built. stderr
// covers the terminal launch (the normal way this binary is started on Linux); zenity covers a
// launcher double-click on a desktop that has it. Neither is required — a missing zenity just means
// the log line is the whole message.
func reportHostLocked(err error) {
	log.Print(err)
	if _, lookErr := exec.LookPath("zenity"); lookErr == nil {
		_ = exec.Command("zenity", "--error", "--title=Tachi 已经在运行", "--text="+err.Error()).Run()
	}
}
