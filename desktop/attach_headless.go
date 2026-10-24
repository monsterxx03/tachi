//go:build headless

package main

import "errors"

// The "open this with the desktop's default application" actions are desktop capabilities: there
// is no Finder, no launcher, no user sitting in front of this machine. attach.go is therefore not
// compiled here; these stand in for its bindings so the shared session code (sessiondir.go) keeps
// one call path.
//
// They report the truth rather than pretending: a caller that somehow reaches one from the phone
// gets "no desktop session", not a silent success.

func (s *AgentService) OpenPath(path string) string {
	return "unavailable: no desktop session on this host"
}

func (s *AgentService) OpenURL(rawURL string) string {
	return "unavailable: no desktop session on this host"
}

// errNoDesktopSession exists so the wording above has one source.
var errNoDesktopSession = errors.New("no desktop session on this host")
