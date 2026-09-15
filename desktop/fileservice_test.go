package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// newTestApp builds a desktopApp with no runs: the @-file root then falls back
// to the process working directory, which each test pins with t.Chdir.
func newTestApp() *desktopApp {
	return &desktopApp{runs: make(map[string]*sessionRun), fileIndex: newFileIndex(), projects: &projectTable{}}
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

// TestSavePastedImageStoresUnderTheSession pins the paste path end to end, as far as this layer can
// see it: the bytes land INSIDE the session's own directory (so a screenshot dies with the
// conversation it was sent in and never appears in the user's workspace, where their diff would
// show it), the answer is an @-reference — the same thing a dropped file gets — and it points at a
// file @-file expansion classifies as an image, which is how it becomes a multi-modal part for the
// model. Every refusal is a REASON rather than an empty answer: a paste that silently does nothing
// is the same class of bug as a message that silently goes nowhere.
func TestSavePastedImageStoresUnderTheSession(t *testing.T) {
	_, svc, sid, dir := newDeleteApp(t)

	// A real 1x1 PNG, base64 — the shape the webview sends (a data URL's payload).
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

	got := svc.SavePastedImage(sid, "image/png", pngB64)
	if got.Error != "" {
		t.Fatalf("SavePastedImage: %s", got.Error)
	}
	path := strings.TrimPrefix(got.Ref, "@")
	if path == got.Ref {
		t.Fatalf("ref = %q, want an @-reference", got.Ref)
	}
	if want := filepath.Join(dir, pastedDirName) + string(filepath.Separator); !strings.HasPrefix(path, want) {
		t.Errorf("pasted file %q is not under the session's directory (%q)", path, want)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pasted file: %v", err)
	}
	want, err := base64.StdEncoding.DecodeString(pngB64)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if !bytes.Equal(raw, want) {
		t.Errorf("stored %d bytes, want the %d that were pasted", len(raw), len(want))
	}
	if kind, _ := atfile.Classify(path); kind != atfile.KindImage {
		t.Errorf("atfile classifies the pasted file as %v, want an image — the expansion would not attach it", kind)
	}

	// A second paste must not overwrite the first: one file per paste, whatever the name.
	first := path
	if again := svc.SavePastedImage(sid, "image/png", pngB64); again.Error != "" || strings.TrimPrefix(again.Ref, "@") == first {
		t.Errorf("second paste = %+v, want its own file", again)
	}

	// Refusals, each naming what was wrong.
	for _, tc := range []struct{ name, session, media, data string }{
		{"not an image", sid, "application/pdf", pngB64},
		{"no session", "", "image/png", pngB64},
		{"undecodable", sid, "image/png", "not base64 at all!!"},
	} {
		if out := svc.SavePastedImage(tc.session, tc.media, tc.data); out.Error == "" {
			t.Errorf("%s: got %+v, want a reason", tc.name, out)
		}
	}
}
