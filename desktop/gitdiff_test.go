package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a temporary repository with one committed file and returns its root.
// Identity and signing are passed per command (-c …), so the test never reads or
// writes the user's git configuration.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		base := []string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}
		cmd := exec.Command("git", append(base, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	git("init")
	writeRepoFile(t, dir, "src/main.go", "package main\n\nfunc main() {\n\told()\n}\n")
	git("add", ".")
	git("commit", "-m", "init")
	return dir
}

func writeRepoFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGetTurnDiffModifiedFile: the panel's core case — real file coordinates for a
// file the turn edited.
func TestGetTurnDiffModifiedFile(t *testing.T) {
	repo := gitRepo(t)
	path := writeRepoFile(t, repo, "src/main.go", "package main\n\nfunc main() {\n\tnewWith(30 * time.Second)\n}\n")
	_, svc, sid := newRootsApp(t, repo)

	vo := svc.GetTurnDiff(sid, []string{path})
	if vo.Note != "" {
		t.Errorf("Note = %q, want none", vo.Note)
	}
	if len(vo.Files) != 1 {
		t.Fatalf("files = %+v, want one", vo.Files)
	}
	f := vo.Files[0]
	if f.Path != "src/main.go" {
		t.Errorf("Path = %q, want the repo-relative path", f.Path)
	}
	if f.Added != 1 || f.Removed != 1 || f.Created || f.Binary {
		t.Errorf("file = %+v, want 1 added / 1 removed on a tracked file", f)
	}
	// The changed line is the FOURTH line of the file — the parsed numbers are real.
	var del, add *int
	for i, h := range f.Hunks {
		if h.Kind == "del" {
			del = &f.Hunks[i].OldLine
		}
		if h.Kind == "add" {
			add = &f.Hunks[i].NewLine
		}
	}
	if del == nil || *del != 4 {
		t.Errorf("deleted line number = %v, want 4", del)
	}
	if add == nil || *add != 4 {
		t.Errorf("added line number = %v, want 4", add)
	}
}

// TestGetTurnDiffUntracked: a file the turn created is new content end to end, and
// git will not report it — the desktop has to synthesize it.
func TestGetTurnDiffUntracked(t *testing.T) {
	repo := gitRepo(t)
	path := writeRepoFile(t, repo, "CHANGELOG.md", "# Changelog\n\n- one\n")
	_, svc, sid := newRootsApp(t, repo)

	vo := svc.GetTurnDiff(sid, []string{path})
	if len(vo.Files) != 1 {
		t.Fatalf("files = %+v, want the untracked file", vo.Files)
	}
	f := vo.Files[0]
	if !f.Created || f.Path != "CHANGELOG.md" || f.Added != 3 || f.Removed != 0 {
		t.Errorf("file = %+v, want a created CHANGELOG.md with 3 added lines", f)
	}
	if f.Hunks[0].NewLine != 1 || f.Hunks[0].OldLine != 0 {
		t.Errorf("first hunk = %+v, want line 1 on the new side only", f.Hunks[0])
	}
}

// TestGetTurnDiffScopedToPaths: the panel asks about the files the turn touched, not
// the whole tree.
func TestGetTurnDiffScopedToPaths(t *testing.T) {
	repo := gitRepo(t)
	wanted := writeRepoFile(t, repo, "src/main.go", "package main\n\nfunc main() {\n\tchanged()\n}\n")
	writeRepoFile(t, repo, "other.go", "package main\n\n// changed too\n")

	_, svc, sid := newRootsApp(t, repo)
	vo := svc.GetTurnDiff(sid, []string{wanted})
	if len(vo.Files) != 1 || vo.Files[0].Path != "src/main.go" {
		t.Errorf("files = %+v, want only the requested path", vo.Files)
	}
}

// TestGetTurnDiffRelativePath: the model may hand EditFile a relative path, so the
// lookup has to resolve it against the workspace.
func TestGetTurnDiffRelativePath(t *testing.T) {
	repo := gitRepo(t)
	writeRepoFile(t, repo, "src/main.go", "package main\n\nfunc main() {\n\tchanged()\n}\n")
	_, svc, sid := newRootsApp(t, repo)

	vo := svc.GetTurnDiff(sid, []string{"src/main.go"})
	if len(vo.Files) != 1 {
		t.Fatalf("files = %+v, want the relative path resolved", vo.Files)
	}
}

// TestGetTurnDiffOutsideRepo: a path from an additional root that is not in this
// repository gets a sentence, not a silent omission.
func TestGetTurnDiffOutsideRepo(t *testing.T) {
	repo := gitRepo(t)
	outside := writeRepoFile(t, t.TempDir(), "elsewhere.go", "package x\n")
	_, svc, sid := newRootsApp(t, repo)

	vo := svc.GetTurnDiff(sid, []string{outside})
	if len(vo.Files) != 0 {
		t.Errorf("files = %+v, want none", vo.Files)
	}
	if !strings.Contains(vo.Note, "不在该 git 仓库内") {
		t.Errorf("Note = %q, want it to say the path is outside the repository", vo.Note)
	}
}

// TestGetTurnDiffNotARepo and TestGetTurnDiffNoWorkspace cover the two honest
// degradations: no git, no workspace.
func TestGetTurnDiffNotARepo(t *testing.T) {
	_, svc, sid := newRootsApp(t, t.TempDir())
	vo := svc.GetTurnDiff(sid, nil)
	if !strings.Contains(vo.Note, "git") {
		t.Errorf("Note = %q, want it to mention git", vo.Note)
	}
}

func TestGetTurnDiffNoWorkspace(t *testing.T) {
	sm := newSessionManagerForTest(t, "")
	sid := sm.Current().ID
	d := newTestApp()
	d.cfg = nil
	d.sm = sm
	d.getRun(sid).sm = sm

	vo := (&AgentService{desk: d}).GetTurnDiff(sid, nil)
	if !strings.Contains(vo.Note, "工作目录") {
		t.Errorf("Note = %q, want it to mention the missing workspace", vo.Note)
	}
}

// TestGetTurnDiffCleanTree: nothing changed means nothing to show — and no note,
// because there is nothing to explain.
func TestGetTurnDiffCleanTree(t *testing.T) {
	repo := gitRepo(t)
	_, svc, sid := newRootsApp(t, repo)

	vo := svc.GetTurnDiff(sid, nil)
	if len(vo.Files) != 0 || vo.Note != "" {
		t.Errorf("vo = %+v, want empty and silent", vo)
	}
	if vo.Root != repo {
		t.Errorf("Root = %q, want the workspace %q", vo.Root, repo)
	}
}

// A review reads the working tree, and the file list is all that survives a turn — so once
// the changes are committed there is nothing for it to see. Reporting that instead of
// running a review that finds "no changes" is what keeps the panel's 「没有报告问题」 from
// meaning "the reviewer was blind".
func TestNothingToReviewDetectsCommittedChanges(t *testing.T) {
	repo := gitRepo(t)
	d, svc, sid := newRootsApp(t, repo)
	// ReviewChanges acts on the ACTIVE session; the test helper binds the run but does not
	// make it active (no UI selected it).
	d.mu.Lock()
	d.activeID = sid
	d.mu.Unlock()
	tracked := filepath.Join(repo, "src/main.go")

	// Committed and untouched: the turn's file is gone from the working tree.
	got := svc.ReviewChanges(sid, []string{tracked})
	if got == "" {
		t.Fatal("a review of committed changes must not start")
	}
	if !strings.Contains(got, "已经提交") {
		t.Errorf("the notice must name the likely cause: %q", got)
	}

	// An actual working-tree change is reviewable again.
	writeRepoFile(t, repo, "src/main.go", "package main\n\nfunc main() {\n\tnewOne()\n}\n")
	if notice := svc.nothingToReview(sid, []string{tracked}); notice != "" {
		t.Errorf("an uncommitted change must be reviewable, got %q", notice)
	}

	// So is a file git does not track yet (GetTurnDiff synthesizes it as all-added).
	writeRepoFile(t, repo, "src/brand-new.go", "package main\n")
	if notice := svc.nothingToReview(sid, []string{filepath.Join(repo, "src/brand-new.go")}); notice != "" {
		t.Errorf("a brand-new file must be reviewable, got %q", notice)
	}
}
