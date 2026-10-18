package main

// A conversation's own directory in the session store: <root>/<id>.
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

	"github.com/monsterxx03/tachi/session"
)

// sessionDirPath resolves the directory a session is stored in, in the wording this frontend shows.
//
// The layout and the untrusted-id rule belong to session.DirFor (the same directory is where the
// TUI's pasted images land, so it is stated once, in the store); what stays here is the message and
// the ROOT — the store this desktop reads and writes sessions through, so the question is answered
// about the conversation that actually exists rather than about a path under a global default. The
// id arrives from the webview, and a refusal is something the reader sees.
func (s *AgentService) sessionDirPath(sessionID string) (string, error) {
	if s.desk == nil {
		// A binding with no app behind it (a zero value in a test): there is no store to ask, and
		// saying so is the same answer as an app whose store is not up yet.
		return "", errors.New("会话存储不可用")
	}
	store := s.desk.sessionStore()
	if store == nil {
		return "", errors.New("会话存储不可用")
	}
	dir, err := session.DirFor(store.BaseDir(), sessionID)
	switch {
	case err == nil:
		return dir, nil
	case errors.Is(err, session.ErrNoSessionID):
		return "", errors.New("没有会话")
	case errors.Is(err, session.ErrNoSessionDir):
		return "", errors.New("会话存储不可用")
	default:
		return "", fmt.Errorf("非法会话名：%q", sessionID)
	}
}

// OpenSessionDir opens a session's directory in the Finder — the sidebar row's right-click
// action. Returns "ok", or the reason it was not opened: the frontend ignores it, like every
// other open/reveal binding (there is nowhere honest to put the message when the very
// mechanism that would show it is the thing that failed).
func (s *AgentService) OpenSessionDir(id string) string {
	dir, err := s.sessionDirPath(id)
	if err != nil {
		return err.Error()
	}
	// Opening a directory is the app's existing "open this thing" action, so the stat, the
	// launcher and the error wording stay ONE implementation rather than a second copy.
	return s.OpenPath(dir)
}
