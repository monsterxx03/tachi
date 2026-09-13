package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// stubOpenFile replaces the `open` launcher for one test and returns the argument lists it
// was called with. Real Finder windows in a test run are the thing this avoids: the fact
// worth pinning is WHICH path was handed over, not that Finder drew a window (see openFile
// in attach.go).
func stubOpenFile(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	prev := openFile
	t.Cleanup(func() { openFile = prev })
	openFile = func(args ...string) error {
		calls = append(calls, args)
		return nil
	}
	return &calls
}

// TestSessionDirPath pins the layout AND the untrusted-input rule: the id comes from the
// webview, so anything that is not a single directory name must be refused before it can be
// joined onto the store path.
func TestSessionDirPath(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	root, err := config.SessionDir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}

	const id = "2026-09-13-103926-92db8c33"
	if got, err := sessionDirPath(id); err != nil || got != filepath.Join(root, id) {
		t.Errorf("sessionDirPath(%q) = %q, %v; want %q", id, got, err, filepath.Join(root, id))
	}

	for _, bad := range []string{"", ".", "..", "../other-session", "a/b", `/abs`, `a\b`} {
		got, err := sessionDirPath(bad)
		if err == nil {
			t.Errorf("sessionDirPath(%q) = %q, want an error", bad, got)
		}
	}
}

// TestOpenSessionDirOpensTheSessionDirectory is the end-to-end half: the resolved path, the
// existence check and the launcher argument, on a real (temp) session directory.
func TestOpenSessionDirOpensTheSessionDirectory(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	root, err := config.SessionDir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	const id = "2026-09-13-103926-92db8c33"
	if err := os.MkdirAll(filepath.Join(root, id), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	calls := stubOpenFile(t)

	if got := (&AgentService{}).OpenSessionDir(id); got != "ok" {
		t.Fatalf("OpenSessionDir = %q, want ok", got)
	}
	if len(*calls) != 1 || len((*calls)[0]) != 1 || (*calls)[0][0] != filepath.Join(root, id) {
		t.Fatalf("open calls = %q, want one call with the session directory", *calls)
	}
}

// TestOpenSessionDirRefusesATraversal: the guard has to hold at the boundary that reaches the
// launcher, not only in the resolver — a refused id must not open anything at all.
func TestOpenSessionDirRefusesATraversal(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	calls := stubOpenFile(t)

	got := (&AgentService{}).OpenSessionDir("../../etc")
	if !strings.Contains(got, "非法会话名") {
		t.Errorf("OpenSessionDir = %q, want a rejected session name", got)
	}
	if len(*calls) != 0 {
		t.Errorf("open was called with %q, want nothing launched", *calls)
	}
}

// TestOpenSessionDirReportsAMissingDirectory: the directory is stat'ed through OpenPath, so a
// session whose folder is gone says so instead of opening a path that is not there.
func TestOpenSessionDirReportsAMissingDirectory(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	calls := stubOpenFile(t)

	if got := (&AgentService{}).OpenSessionDir("2026-09-13-103926-92db8c33"); got != "not found" {
		t.Errorf("OpenSessionDir = %q, want not found", got)
	}
	if len(*calls) != 0 {
		t.Errorf("open was called with %q, want nothing launched", *calls)
	}
}
