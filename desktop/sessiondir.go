package main

// A conversation's own directory in the session store: <SessionDir>/<id>.
//
// The desktop is the only surface that can offer this — the TUI has a working directory and
// an ACP peer has its own file manager — so the sidebar row's context menu is where the
// reader asks "where does this conversation actually live?". Everything under that directory
// is the conversation as stored: meta.json, messages.jsonl, api_requests.jsonl, oneoff/,
// subagent/. Opening the folder is the honest answer, and it is why the path is resolved
// here instead of being assembled in the frontend: the store's layout belongs to the backend,
// and the frontend never learns a path it could turn into a request of its own.

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/config"
)

// sessionDirPath resolves the directory a session is stored in.
//
// sessionID arrives from the webview, so it is treated as untrusted: only a single path
// element is accepted. Without that check a name like "../../other-session" would point
// somewhere else entirely — and the caller is one `open` away from showing it.
func sessionDirPath(sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("没有会话")
	}
	if !isSinglePathElement(sessionID) {
		return "", fmt.Errorf("非法会话名：%q", sessionID)
	}
	dir, err := config.SessionDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionID), nil
}

// OpenSessionDir opens a session's directory in the Finder — the sidebar row's right-click
// action. Returns "ok", or the reason it was not opened: the frontend ignores it, like every
// other open/reveal binding (there is nowhere honest to put the message when the very
// mechanism that would show it is the thing that failed).
func (s *AgentService) OpenSessionDir(id string) string {
	dir, err := sessionDirPath(id)
	if err != nil {
		return err.Error()
	}
	// Opening a directory is the app's existing "open this thing" action, so the stat, the
	// launcher and the error wording stay ONE implementation rather than a second copy.
	return s.OpenPath(dir)
}

// isSinglePathElement reports whether s names one entry in one directory: no separator, no
// traversal, and not the current/parent directory itself.
func isSinglePathElement(s string) bool {
	return s != "" && s != "." && s != ".." &&
		s == filepath.Base(s) && !strings.ContainsAny(s, `/\\`)
}
