package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusesFromPorcelain covers the parts of the -z format this parser has to get right: the
// XY pair means the same thing as one letter per row, a rename's ORIGINAL path travels as a second
// field, and a path with a space in it is not split.
func TestStatusesFromPorcelain(t *testing.T) {
	root := t.TempDir()
	out := strings.Join([]string{
		" M edited.go",
		"?? new.go",
		"A  added.go",
		"R  moved.go", "gone.go", // rename: the second field is the old path
		"UU conflict.go",
		" M dir/file with space.go",
		"!! ignored.go", // never asked for, but must not become a mark
		"",
	}, "\x00")

	got := map[string]GitStatusVO{}
	for _, st := range statusesFromPorcelain(out, root, "") {
		got[st.Path] = st
	}
	want := map[string]string{
		"edited.go":              "M",
		"new.go":                 "U",
		"added.go":               "A",
		"moved.go":               "R",
		"conflict.go":            "!",
		"dir/file with space.go": "M",
	}
	for rel, code := range want {
		st, ok := got[filepath.Join(root, rel)]
		if !ok {
			t.Errorf("%s: no status (got %v)", rel, got)
			continue
		}
		if st.Code != code || st.Label == "" {
			t.Errorf("%s: got %q/%q, want %q", rel, st.Code, st.Label, code)
		}
	}
	if _, ok := got[filepath.Join(root, "gone.go")]; ok {
		t.Errorf("a rename's old path must not become a row status of its own: %v", got)
	}
	if _, ok := got[filepath.Join(root, "ignored.go")]; ok {
		t.Errorf("an ignored entry is not a change: %v", got)
	}
}

// TestStatusesFromPorcelainSkipsPathsAboveTheRoot: `git status` answers for the whole repository,
// so a root that is a SUBDIRECTORY has to drop everything outside it — otherwise a row in another
// tree would inherit a change from a file that shares a name.
func TestStatusesFromPorcelainSkipsPathsAboveTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sub")
	out := " M sub/inside.go\x00 M outside.go\x00?? elsewhere/deep.go\x00"
	got := statusesFromPorcelain(out, root, "sub/")
	if len(got) != 1 || got[0].Path != filepath.Join(root, "inside.go") {
		t.Fatalf("got %+v, want only sub/inside.go", got)
	}
	if got[0].Code != "M" {
		t.Errorf("code = %q, want M", got[0].Code)
	}
}

// TestWorkingTreeStatusResolvesSubdirectoryRoots: the paths git prints are relative to the
// repository root, and the app's rows are absolute — with a root below the top level, and a
// toplevel that git reports with symlinks resolved (macOS /tmp vs /private/tmp), both conversions
// have to happen or every row loses its mark.
func TestWorkingTreeStatusResolvesSubdirectoryRoots(t *testing.T) {
	requireGit(t)
	repo := gitRepo(t)
	sub := filepath.Join(repo, "src")
	writeRepoFile(t, repo, "src/main.go", "package main // edited\n")
	writeRepoFile(t, repo, "src/fresh.go", "package main // new\n")

	got := map[string]GitStatusVO{}
	for _, st := range workingTreeStatus(sub) {
		got[st.Path] = st
	}
	if c := got[filepath.Join(sub, "main.go")].Code; c != "M" {
		t.Errorf("main.go = %q, want M (paths %v)", c, got)
	}
	if c := got[filepath.Join(sub, "fresh.go")].Code; c != "U" {
		t.Errorf("fresh.go = %q, want U (paths %v)", c, got)
	}
}

// TestGetWorkspaceGitStatus is the binding's half: the tree asks with a session, and gets back the
// uncommitted paths under THAT session's roots — an ignored file excluded, a clean one absent.
func TestGetWorkspaceGitStatus(t *testing.T) {
	requireGit(t)
	repo := gitRepo(t)
	writeRepoFile(t, repo, ".gitignore", "dist/\n")
	writeRepoFile(t, repo, "src/main.go", "package main // edited\n")
	writeRepoFile(t, repo, "notes.md", "new\n")
	writeRepoFile(t, repo, "dist/bundle.js", "x\n")

	_, svc, sid := newTreeApp(t, repo)
	got := map[string]GitStatusVO{}
	for _, st := range svc.GetWorkspaceGitStatus(sid) {
		got[st.Path] = st
	}
	if c := got[filepath.Join(repo, "src", "main.go")].Code; c != "M" {
		t.Errorf("modified file = %q, want M (%v)", c, got)
	}
	if c := got[filepath.Join(repo, "notes.md")].Code; c != "U" {
		t.Errorf("untracked file = %q, want U (%v)", c, got)
	}
	if _, ok := got[filepath.Join(repo, "dist", "bundle.js")]; ok {
		t.Errorf("an ignored file is not an uncommitted change: %v", got)
	}
}

// TestGetWorkspaceGitStatusOutsideARepo: no repository is not an error — the tree draws the same
// rows, they just have no marks.
func TestGetWorkspaceGitStatusOutsideARepo(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "notes.md")
	_, svc, sid := newTreeApp(t, dir)
	if got := svc.GetWorkspaceGitStatus(sid); len(got) != 0 {
		t.Errorf("a non-repository workspace reported %+v", got)
	}
}

// TestGetWorkspaceGitStatusNeedsWorkspace: a session with no folder is answered, not crashed into.
func TestGetWorkspaceGitStatusNeedsWorkspace(t *testing.T) {
	_, svc, sid := newWorkspacelessApp(t)
	if got := svc.GetWorkspaceGitStatus(sid); len(got) != 0 {
		t.Errorf("a workspaceless session reported %+v", got)
	}
}
