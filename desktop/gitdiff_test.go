package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// TestGetTurnDiffOutsideRepo: a path that is in NO workspace root of this session gets a
// sentence, not a silent omission. (A path in an ADDITIONAL root is a different story — it is
// diffed, see TestGetTurnDiffDiffsAdditionalRoots.)
func TestGetTurnDiffOutsideRepo(t *testing.T) {
	repo := gitRepo(t)
	outside := writeRepoFile(t, t.TempDir(), "elsewhere.go", "package x\n")
	_, svc, sid := newRootsApp(t, repo)

	vo := svc.GetTurnDiff(sid, []string{outside})
	if len(vo.Files) != 0 {
		t.Errorf("files = %+v, want none", vo.Files)
	}
	if !strings.Contains(vo.Note, "不在任何工作目录内") {
		t.Errorf("Note = %q, want it to say the path is in no working directory", vo.Note)
	}
}

// TestGetTurnDiffDiffsAdditionalRoots is the multi-root fix: a path under a DECLARED additional
// root is in a different repository, not "outside the repository", so it is diffed like any
// other — against ITS OWN git, with the root it came from carried on every file.
//
// The label is what keeps the panel honest: without it a file would be resolved against the
// primary root, and a same-named file there would be what 预览/打开 showed.
func TestGetTurnDiffDiffsAdditionalRoots(t *testing.T) {
	primary := gitRepo(t)
	second := gitRepo(t)
	// Same RELATIVE path in both roots: the case that makes a root-blind diff wrong rather
	// than merely imprecise.
	writeRepoFile(t, primary, "shared/notes.md", "primary\n")
	writeRepoFile(t, second, "shared/notes.md", "second\n")

	_, svc, sid := newRootsApp(t, primary)
	if res := svc.AddSessionRoots(sid, []string{second}); res != "ok" {
		t.Fatalf("AddSessionRoots(%q) = %q, want ok", second, res)
	}

	vo := svc.GetTurnDiff(sid, []string{
		filepath.Join(primary, "shared/notes.md"),
		filepath.Join(second, "shared/notes.md"),
	})
	if vo.Note != "" {
		t.Errorf("Note = %q, want none: both paths are in the session's roots", vo.Note)
	}
	if len(vo.Files) != 2 {
		t.Fatalf("files = %+v, want both roots' files", vo.Files)
	}
	byRoot := map[string]FileDiffVO{}
	for _, f := range vo.Files {
		if f.Path != "shared/notes.md" {
			t.Errorf("path = %q, want the path relative to its own root", f.Path)
		}
		byRoot[f.Root] = f
	}
	if _, ok := byRoot[primary]; !ok {
		t.Errorf("no file carried the primary root; roots seen: %v", keysOf(byRoot))
	}
	secondFile, ok := byRoot[second]
	if !ok {
		t.Fatalf("no file carried the additional root; roots seen: %v", keysOf(byRoot))
	}
	if secondFile.RootLabel != filepath.Base(second) {
		t.Errorf("RootLabel = %q, want the root's base name %q", secondFile.RootLabel, filepath.Base(second))
	}
	if byRoot[primary].RootLabel != "" {
		t.Errorf("RootLabel = %q for the primary root, want empty", byRoot[primary].RootLabel)
	}
}

// keysOf lists a map's keys, for a failure message that says what actually came back.
func keysOf(m map[string]FileDiffVO) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
// TestGetTurnDiffIgnoredPath is the case that made the panel lie: a path git ignores appears in
// neither `git diff HEAD` nor `ls-files --others --exclude-standard`, so the panel showed an
// empty diff and explained it as 「没有未提交的改动（可能已经提交）」 — for a file that was
// written seconds earlier. The ignore rule is the honest explanation, and 评审本轮改动 has to
// refuse with it rather than with "nothing to review".
func TestGetTurnDiffIgnoredPath(t *testing.T) {
	repo := gitRepo(t)
	writeRepoFile(t, repo, ".gitignore", "scratch/\n")
	ignored := writeRepoFile(t, repo, "scratch/report.md", "# a report\n")
	_, svc, sid := newRootsApp(t, repo)

	vo := svc.GetTurnDiff(sid, []string{ignored})
	if len(vo.Files) != 0 {
		t.Fatalf("files = %+v, want none (git never diffs an ignored path)", vo.Files)
	}
	if vo.Ignored != 1 {
		t.Errorf("Ignored = %d, want 1", vo.Ignored)
	}
	if !strings.Contains(vo.Note, "被 git 忽略") {
		t.Errorf("Note = %q, want the ignore rule named", vo.Note)
	}
	// The rule's source is what a reader can act on, so it has to be in the sentence.
	if !strings.Contains(vo.Note, ".gitignore") {
		t.Errorf("Note = %q, want the .gitignore source", vo.Note)
	}
	// And the review entry must refuse with THAT reason, not with "already committed".
	notice := svc.nothingToReview(sid, reviewScope{Paths: []string{ignored}})
	if !strings.Contains(notice, "被 git 忽略") {
		t.Errorf("nothingToReview = %q, want the ignore reason", notice)
	}
	if strings.Contains(notice, "可能已经提交") {
		t.Errorf("nothingToReview = %q, must not blame a commit", notice)
	}

	// A path that is neither ignored nor changed keeps the existing wording.
	clean := filepath.Join(repo, "src/main.go")
	if note := svc.GetTurnDiff(sid, []string{clean}).Note; strings.Contains(note, "被 git 忽略") {
		t.Errorf("a tracked, clean path must not be reported as ignored: %q", note)
	}
}

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
	got := svc.ReviewChanges(sid, 0, []string{tracked}, "")
	if got == "" {
		t.Fatal("a review of committed changes must not start")
	}
	if !strings.Contains(got, "已经提交") {
		t.Errorf("the notice must name the likely cause: %q", got)
	}

	// An actual working-tree change is reviewable again.
	writeRepoFile(t, repo, "src/main.go", "package main\n\nfunc main() {\n\tnewOne()\n}\n")
	if notice := svc.nothingToReview(sid, reviewScope{Paths: []string{tracked}}); notice != "" {
		t.Errorf("an uncommitted change must be reviewable, got %q", notice)
	}

	// So is a file git does not track yet (GetTurnDiff synthesizes it as all-added).
	writeRepoFile(t, repo, "src/brand-new.go", "package main\n")
	if notice := svc.nothingToReview(sid, reviewScope{Paths: []string{filepath.Join(repo, "src/brand-new.go")}}); notice != "" {
		t.Errorf("a brand-new file must be reviewable, got %q", notice)
	}
}
