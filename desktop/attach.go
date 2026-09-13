package main

import (
	"os"
	"os/exec"
)

// Attachment actions for files the agent hands over with the SendFile tool.
//
// There is deliberately no attachment *event*: the transcript renders the file
// card from the recorded SendFile tool call itself (see FileCard in the
// frontend), which means the live turn and reloaded history use one rendering
// path. Emitting a separate event as well made every file show up twice.

// openFile hands a path to macOS `open`. It is a variable so a test can stand in for it:
// the interesting assertion is WHICH path was handed over, and proving it with the real
// launcher costs a Finder window per run (and, in the smoke suite, the app's foreground —
// see the suspension trap in docs/agents/desktop.md).
var openFile = func(args ...string) error {
	return exec.Command("open", args...).Start()
}

// OpenPath opens a file (or directory) with the system default application.
func (s *AgentService) OpenPath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "not found"
	}
	if err := openFile(path); err != nil {
		return err.Error()
	}
	return "ok"
}

// RevealPath shows the file in Finder.
func (s *AgentService) RevealPath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "not found"
	}
	if err := openFile("-R", path); err != nil {
		return err.Error()
	}
	return "ok"
}
