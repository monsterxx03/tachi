package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// newTestApp builds a desktopApp with no runs: the @-file root then falls back
// to the process working directory, which each test pins with t.Chdir.
func newTestApp() *desktopApp {
	return &desktopApp{runs: make(map[string]*sessionRun), fileIndex: newFileIndex()}
}

// newSessionManagerForTest returns a session manager rooted in a temp dir
// (config.SetBaseDir is a process global — it must never point at the real
// ~/.tachi) holding one current session bound to dir.
func newSessionManagerForTest(t *testing.T, dir string) *session.Manager {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	sm, err := session.NewManager(nil)
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	if _, err := sm.New("", dir); err != nil {
		t.Fatalf("new session: %v", err)
	}
	return sm
}

func writeTestFile(t *testing.T, dir, rel string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed")
	}
}

// TestSearchFilesRootsAtWorkingDir covers the empty-query case: the picker
// opens with the immediate entries of the session's working directory, which
// for a session without a bound agent is the process working directory.
func TestSearchFilesRootsAtWorkingDir(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	writeTestFile(t, dir, "readme.md")
	writeTestFile(t, dir, "src/main.go")
	t.Chdir(dir)

	svc := &AgentService{desk: newTestApp()}
	matches := svc.SearchFiles("session-without-run", "", 10)

	if len(matches) != 2 {
		t.Fatalf("expected 2 immediate entries, got %v", matches)
	}
	if matches[0].Path != "src" || !matches[0].IsDir {
		t.Errorf("expected directories first, got %+v", matches[0])
	}
	if matches[1].Path != "readme.md" || matches[1].IsDir {
		t.Errorf("expected readme.md second, got %+v", matches[1])
	}

	// A query searches the whole tree, not just the first level.
	fuzzy := svc.SearchFiles("session-without-run", "main", 10)
	if len(fuzzy) == 0 || fuzzy[0].Path != "src/main.go" {
		t.Errorf("expected src/main.go to match %q, got %+v", "main", fuzzy)
	}
}

// TestResolveDroppedPathsMapsReferences covers a file drop: paths inside the
// working directory become relative references, paths outside it stay absolute,
// and vanished paths are skipped.
func TestResolveDroppedPathsMapsReferences(t *testing.T) {
	root := t.TempDir()
	inside := writeTestFile(t, root, "notes/a.txt")
	t.Chdir(root)

	outside := writeTestFile(t, t.TempDir(), "pic.png")

	svc := &AgentService{desk: newTestApp()}
	got := svc.ResolveDroppedPaths("session-without-run", []string{
		inside,
		outside,
		filepath.Join(root, "gone.txt"),
	})

	if len(got) != 2 {
		t.Fatalf("expected the missing path to be skipped, got %+v", got)
	}
	if got[0].Ref != "@notes/a.txt" || got[0].Kind != "text" || got[0].IsDir {
		t.Errorf("wrong reference for an in-tree drop: %+v", got[0])
	}
	if want := "@" + filepath.ToSlash(outside); got[1].Ref != want {
		t.Errorf("expected an absolute reference %q, got %q", want, got[1].Ref)
	}
	if got[1].Kind != "image" {
		t.Errorf("expected the png to classify as an image, got %+v", got[1])
	}
}

// TestAtFileRootFollowsSessionWorkingDir pins the root used for BOTH the picker
// and @-reference expansion to the session's working directory, so what the
// popup offers is exactly what the agent will read.
func TestAtFileRootFollowsSessionWorkingDir(t *testing.T) {
	dir := t.TempDir()
	d := newTestApp()

	// No runs → process working directory fallback.
	if got, want := d.atFileRoot("unknown"), processCWD(); got != want {
		t.Errorf("expected the process cwd %q, got %q", want, got)
	}

	// A run bound to a session exposes that session's working directory.
	r := d.getRun("sid")
	r.sm = newSessionManagerForTest(t, dir)
	if got := d.atFileRoot("sid"); got != dir {
		t.Errorf("expected the session working dir %q, got %q", dir, got)
	}
}
