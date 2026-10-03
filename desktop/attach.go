package main

import "os"

// Attachment actions for files the agent hands over with the SendFile tool.
//
// There is deliberately no attachment *event*: the transcript renders the file
// card from the recorded SendFile tool call itself (see FileCard in the
// frontend), which means the live turn and reloaded history use one rendering
// path. Emitting a separate event as well made every file show up twice.

// openFile and revealFile are the two launcher actions, defaulting to this
// platform's own spellings (attach_darwin.go / attach_other.go — `open` / `open -R`
// on macOS, xdg-open / FileManager1 elsewhere). They are variables so a test can
// stand in for them: the interesting assertion is WHICH path was handed over, and
// proving it with the real launcher costs a Finder window per run (and, in the
// smoke suite, the app's foreground — see docs/agents/desktop.md).
//
// openFile stays variadic because that is the shape the stub in sessiondir_test.go
// fills in. revealFile takes the single path instead of a shared argument list: the
// two platforms spell "reveal" differently (`open -R` vs. a FileManager1 call), so
// there is no common flag to put in one.
var (
	openFile   = platformOpenFile
	revealFile = platformRevealFile
)

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

// RevealPath shows the file in the platform's file manager (Finder on macOS, the
// desktop's own on Linux).
func (s *AgentService) RevealPath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "not found"
	}
	if err := revealFile(path); err != nil {
		return err.Error()
	}
	return "ok"
}
