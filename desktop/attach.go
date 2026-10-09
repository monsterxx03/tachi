package main

import (
	"net/url"
	"os"
	"slices"
	"strings"
)

// Attachment actions for files the agent hands over with the SendFile tool, plus the one action
// that leaves the app for the OUTSIDE world: opening a link from a reply in the system browser
// (OpenURL).
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
// smoke suite, the app's foreground — see docs/agents/desktop.md). OpenURL uses
// openFile too: handing over a URL is the same platform action as handing over a
// path (`open` and xdg-open take both), so it costs no third var — and it inherits
// the same seam for a test.
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

// openableURLSchemes are the schemes OpenURL hands to the system: the two a reply can point at
// (a source, a doc) plus mailto, which `open` and xdg-open both know how to route. The list is an
// ALLOWLIST because the value arrives from a click on model-written markdown, and the launcher
// happily opens anything its handler table knows — `file:` would turn a link in a reply into "open
// this path on your machine", which is what the app's own file actions (and their permission
// checks) are for, and a `data:`/`javascript:` URL is a script waiting for the wrong consumer.
var openableURLSchemes = []string{"http", "https", "mailto"}

// OpenURL opens an http/https/mailto URL in the SYSTEM BROWSER.
//
// It exists because the transcript's links have nowhere else to go: the reply is markdown rendered
// inside the webview, so an <a> that is allowed to follow its href takes the whole app off itself,
// with no back button — the window IS the app. The frontend therefore hands the URL here instead
// (see MdLink in frontend/src/markdown.tsx), and the app is left where it was.
//
// It goes through the same openFile var as OpenPath: the launcher is one action per platform
// (`open` / xdg-open both take a URL), and a test can stand in for it rather than opening a real
// browser window.
func (s *AgentService) OpenURL(rawURL string) string {
	target := strings.TrimSpace(rawURL)
	u, err := url.Parse(target)
	if err != nil {
		return err.Error()
	}
	if !slices.Contains(openableURLSchemes, strings.ToLower(u.Scheme)) {
		return "unsupported scheme: " + u.Scheme
	}
	if err := openFile(target); err != nil {
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
