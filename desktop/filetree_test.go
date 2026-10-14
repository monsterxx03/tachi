package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/monsterxx03/tachi/session"
)

// requireGit skips a test that needs `git check-ignore` when git is not installed — the same
// bargain gitdiff_test.go makes for the panels that shell out to git.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// newTreeApp is the shape the listing APIs expect: an app with one session whose primary root is
// dir.
func newTreeApp(t *testing.T, dir string) (*desktopApp, *AgentService, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, dir)
	sid := sm.Current().ID
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	d.sm = sm
	d.getRun(sid).sm = sm
	return d, &AgentService{desk: d}, sid
}

// newWorkspacelessApp is the other end of the same fixture: a session with no working directory at
// all (a launch, a session created before a folder was picked).
func newWorkspacelessApp(t *testing.T) (*desktopApp, *AgentService, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, "")
	sid := sm.Current().ID
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	d.sm = sm
	d.getRun(sid).sm = sm
	return d, &AgentService{desk: d}, sid
}

// namesOf is the row order as the tree draws it, which is what the ordering assertions compare.
func namesOf(vo DirListingVO) []string {
	out := make([]string, 0, len(vo.Entries))
	for _, e := range vo.Entries {
		out = append(out, e.Name)
	}
	return out
}

// TestListWorkspaceDirOrdersLikeFileindex: the tree's column and the @-picker's column hold the same
// files, so one directory must not read in two different orders — directories first, then names
// compared case-insensitively. The expectation is not written out by hand: it is read from
// fileindex.Immediate, which is what the picker uses.
func TestListWorkspaceDirOrdersLikeFileindex(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "Alpha-dir"), 0o755), "mkdir Alpha-dir")
	must(t, os.MkdirAll(filepath.Join(dir, "beta"), 0o755), "mkdir beta")
	for _, name := range []string{"README.md", "zeta.go", "main.go"} {
		writeTestFile(t, dir, name)
	}
	_, svc, sid := newTreeApp(t, dir)

	got := namesOf(svc.ListWorkspaceDir(sid, dir))
	matches, err := fileindex.New(nil).Immediate(dir, 100)
	must(t, err, "fileindex.Immediate")
	want := make([]string, 0, len(matches))
	for _, m := range matches {
		want = append(want, m.Path)
	}
	eqSlices(t, "row order", got, want)
	if len(got) < 2 || !strings.HasPrefix(got[0], "Alpha") {
		t.Errorf("directories must come first, sorted case-insensitively: %v", got)
	}
}

// TestListWorkspaceDirShowsDotfilesButNotGit: the tree answers "what is on this disk", so a dotfile
// is a row like any other — and .git is the one name the column hides (design §6.1).
func TestListWorkspaceDirShowsDotfilesButNotGit(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".env")
	writeTestFile(t, dir, ".gitignore")
	must(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755), "mkdir .git")
	must(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755), "mkdir src")

	_, svc, sid := newTreeApp(t, dir)
	eqSlices(t, "rows", namesOf(svc.ListWorkspaceDir(sid, dir)), []string{"src", ".env", ".gitignore"})
}

// TestListWorkspaceDirRefusesOutsideRoots: containment is literal (no symlink resolution on the way
// in), and a refusal is a SENTENCE the reader can act on rather than an empty directory — silence is
// the only symptom a resolution bug ever has (design §3.2).
func TestListWorkspaceDirRefusesOutsideRoots(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755), "mkdir sub")
	_, svc, sid := newTreeApp(t, root)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"parent escape", filepath.Join(root, "sub", "..", ".."), "不在本会话的工作目录内"},
		{"absolute elsewhere", other, "不在本会话的工作目录内"},
		{"unrelated absolute path", "/etc", "不在本会话的工作目录内"},
		{"relative", "sub", "绝对路径"},
		{"empty", "", "绝对路径"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vo := svc.ListWorkspaceDir(sid, tc.path)
			if !strings.Contains(vo.Error, tc.want) {
				t.Errorf("Error = %q, want it to mention %q", vo.Error, tc.want)
			}
			if len(vo.Entries) != 0 {
				t.Errorf("a refused listing carried rows: %+v", vo.Entries)
			}
		})
	}

	// The two forms that must work: the root itself (how its top level is listed) and a layer
	// inside it.
	if vo := svc.ListWorkspaceDir(sid, root); vo.Error != "" || len(vo.Entries) != 1 {
		t.Errorf("listing the root itself failed: err=%q rows=%v", vo.Error, namesOf(vo))
	}
	if vo := svc.ListWorkspaceDir(sid, filepath.Join(root, "sub")); vo.Error != "" {
		t.Errorf("listing a subdirectory failed: %q", vo.Error)
	}
}

// TestListWorkspaceDirNeedsWorkspace: a session with no root has nothing to list, and says so.
func TestListWorkspaceDirNeedsWorkspace(t *testing.T) {
	_, svc, sid := newWorkspacelessApp(t)
	vo := svc.ListWorkspaceDir(sid, t.TempDir())
	if !strings.Contains(vo.Error, "还没有工作目录") {
		t.Errorf("Error = %q, want the no-workspace sentence", vo.Error)
	}
	// An id no session was ever created for answers the same way rather than panicking.
	if vo := svc.ListWorkspaceDir("no-such-session", "/tmp"); vo.Error == "" {
		t.Error("an unknown session must say why it has nothing to list")
	}
}

// TestListWorkspaceDirTruncatesAndCountsTotal: the cap is on what travels to the webview, never on
// what exists — "另有 N 项未显示" has to be a true number.
func TestListWorkspaceDirTruncatesAndCountsTotal(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < fileTreeMaxEntries+3; i++ {
		writeTestFile(t, dir, filepath.Join("d", fmt.Sprintf("f%05d.txt", i)))
	}
	_, svc, sid := newTreeApp(t, dir)

	vo := svc.ListWorkspaceDir(sid, filepath.Join(dir, "d"))
	if !vo.Truncated {
		t.Error("a directory past the cap must be reported as truncated")
	}
	eq(t, "rows handed over", len(vo.Entries), fileTreeMaxEntries)
	eq(t, "Total (the real entry count)", vo.Total, fileTreeMaxEntries+3)
}

// TestListWorkspaceDirMarksIgnored: an ignored entry is still LISTED (that is the point of this
// column) and carries the rule that hides it, so the tooltip can name the file to go and open.
func TestListWorkspaceDirMarksIgnored(t *testing.T) {
	requireGit(t)
	dir := gitRepo(t)
	writeRepoFile(t, dir, ".gitignore", "dist/\n*.log\n")
	writeRepoFile(t, dir, "dist/bundle.js", "x\n")
	writeRepoFile(t, dir, "run.log", "x\n")
	writeRepoFile(t, dir, "notes.md", "x\n")

	_, svc, sid := newTreeApp(t, dir)
	vo := svc.ListWorkspaceDir(sid, dir)
	byName := map[string]FileNodeVO{}
	for _, e := range vo.Entries {
		byName[e.Name] = e
	}
	if got := byName["dist"]; !strings.Contains(got.Ignored, ".gitignore") {
		t.Errorf("an ignored directory carries no rule: %+v", got)
	}
	if got := byName["run.log"]; !strings.Contains(got.Ignored, "*.log") {
		t.Errorf("an ignored file carries no rule: %+v", got)
	}
	if got := byName["notes.md"]; got.Ignored != "" {
		t.Errorf("a tracked file must not be marked ignored: %+v", got)
	}
	// The tracked directory committed by the fixture is in the listing too: nothing was hidden.
	if _, ok := byName["src"]; !ok {
		t.Errorf("the listing lost an ordinary directory: %v", namesOf(vo))
	}
}

// TestListWorkspaceDirOutsideARepoMarksNothing: a workspace that is not a git repository simply has
// no marks — the absence of git is not an error to report on every row.
func TestListWorkspaceDirOutsideARepoMarksNothing(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "notes.md")
	_, svc, sid := newTreeApp(t, dir)

	vo := svc.ListWorkspaceDir(sid, dir)
	if vo.Error != "" || len(vo.Entries) != 1 {
		t.Fatalf("listing outside a repo failed: err=%q rows=%v", vo.Error, namesOf(vo))
	}
	if vo.Entries[0].Ignored != "" {
		t.Errorf("a non-repository workspace must mark nothing: %+v", vo.Entries[0])
	}
}

// TestListWorkspaceDirFollowsDirectoryLinks: a link to a directory is a directory here (the row has
// to expand), the row says it IS a link, and a broken link keeps its row with a reason.
func TestListWorkspaceDirFollowsDirectoryLinks(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	writeTestFile(t, target, "inside.txt")
	must(t, os.Symlink(target, filepath.Join(root, "link-dir")), "symlink to dir")
	writeTestFile(t, root, "real.txt")
	must(t, os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "dangling")), "dangling symlink")

	_, svc, sid := newTreeApp(t, root)
	vo := svc.ListWorkspaceDir(sid, root)
	byName := map[string]FileNodeVO{}
	for _, e := range vo.Entries {
		byName[e.Name] = e
	}
	link, ok := byName["link-dir"]
	if !ok {
		t.Fatalf("the link row is missing: %v", namesOf(vo))
	}
	if !link.IsDir || !link.Link || link.Kind != kindDir {
		t.Errorf("a link to a directory must read as a linked directory: %+v", link)
	}
	dangling, ok := byName["dangling"]
	if !ok {
		t.Fatalf("a broken link must keep its row: %v", namesOf(vo))
	}
	if dangling.Error == "" || !dangling.Link {
		t.Errorf("a broken link must carry its reason: %+v", dangling)
	}
	// The link expands: its target's entries are what opening it gives.
	if sub := svc.ListWorkspaceDir(sid, filepath.Join(root, "link-dir")); len(sub.Entries) != 1 {
		t.Errorf("expanding the link gave %v (err=%q)", namesOf(sub), sub.Error)
	}
}

// TestListWorkspaceDirReportsGoneDirectory: opening a directory that was deleted a moment ago (an
// agent is working in this tree) is an ordinary event with a sentence, never a panic and never an
// empty listing.
func TestListWorkspaceDirReportsGoneDirectory(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "gone")
	must(t, os.MkdirAll(gone, 0o755), "mkdir gone")
	_, svc, sid := newTreeApp(t, root)
	must(t, os.RemoveAll(gone), "rm gone")

	vo := svc.ListWorkspaceDir(sid, gone)
	if vo.Error == "" {
		t.Error("a deleted directory must report why it could not be read")
	}
	if len(vo.Entries) != 0 {
		t.Errorf("a failed listing carried rows: %+v", vo.Entries)
	}
}

// TestListWorkspaceDirNamesTheRoot: every layer says which root it belongs to, labelled the way the
// @-picker and the diff panel label it — one rule, three readers.
func TestListWorkspaceDirNamesTheRoot(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(extra, "deep"), 0o755), "mkdir extra/deep")
	sibling := extra + "-sibling"
	must(t, os.MkdirAll(sibling, 0o755), "mkdir sibling")
	_, svc, sid := newTreeApp(t, primary)
	eq(t, "adding the extra root", svc.AddSessionRoots(sid, []string{extra}), "ok")

	vo := svc.ListWorkspaceDir(sid, filepath.Join(extra, "deep"))
	eq(t, "Root", vo.Root, extra)
	eq(t, "Label (an additional root is named by its base)", vo.Label, filepath.Base(extra))

	top := svc.ListWorkspaceDir(sid, primary)
	eq(t, "Root", top.Root, primary)
	if top.Label != "" {
		t.Errorf("the primary root's label must be empty, got %q", top.Label)
	}
	// A directory whose NAME merely starts with a root's path is not inside it: containment is a
	// path comparison, not a string prefix.
	if got := svc.ListWorkspaceDir(sid, sibling); got.Error == "" {
		t.Errorf("a sibling of a root with the same prefix must be refused, got %+v", got.Entries)
	}
}

// TestTreeKindHint: the hint comes from the name alone (an 8KB content probe per row is not
// acceptable for several hundred rows), it reuses preview.go's own tables, and an extension nobody
// knows is UNKNOWN — not "binary", which would deny a preview the reader can actually have.
func TestTreeKindHint(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"README.md", kindMarkdown},
		{"main.go", kindText},
		{"Makefile", kindText},
		{".bashrc", kindText},
		{"logo.PNG", kindImage},
		{"report.pdf", kindPDF},
		{"data.csv", kindCSV},
		{"index.html", kindHTML},
		{"archive.zip", kindBinary},
		{"NOTES", kindUnknown},
		{"x.weirdext", kindUnknown},
	}
	for _, tc := range cases {
		got := treeKindHint(tc.name, false)
		if got != tc.want {
			t.Errorf("treeKindHint(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	eq(t, "a directory", treeKindHint("src", true), kindDir)

	// Every kind the hint answers "text" for is one the previewer can actually highlight — the tree
	// and the previewer share one table, so a file the tree calls source cannot open as plain text
	// for lack of a grammar.
	for _, tc := range cases {
		if treeKindHint(tc.name, false) != kindText {
			continue
		}
		ext := strings.ToLower(filepath.Ext(tc.name))
		if langByExt[ext] == "" && langByBase[tc.name] == "" {
			t.Errorf("%q came back as text but has no grammar and no base-name entry", tc.name)
		}
	}
	// And the two tables really are preview.go's: a kind it classifies as text must be text here.
	if got := treeKindHint("main.go", false); got != kindText || langByExt[".go"] == "" {
		t.Errorf("the tree and the previewer disagree about a .go file: %q", got)
	}
}

// TestPollWorkspacePaths: the poll reports what is, and never what it means — reading a change (a
// directory's entries moved, a file's content moved) belongs to the half that shows it.
func TestPollWorkspacePaths(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	must(t, os.MkdirAll(sub, 0o755), "mkdir sub")
	file := filepath.Join(root, "a.txt")
	must(t, os.WriteFile(file, []byte("one\n"), 0o644), "write a.txt")
	_, svc, sid := newTreeApp(t, root)

	before := svc.PollWorkspacePaths(sid, []string{sub, file, filepath.Join(root, "gone")})
	if len(before) != 3 {
		t.Fatalf("got %d states, want 3", len(before))
	}
	if !before[0].Exists || !before[0].IsDir {
		t.Errorf("the directory state is wrong: %+v", before[0])
	}
	if !before[1].Exists || before[1].IsDir || before[1].Size == 0 {
		t.Errorf("the file state is wrong: %+v", before[1])
	}
	if before[2].Exists {
		t.Errorf("a missing path must report Exists=false: %+v", before[2])
	}

	// A file appearing in the directory moves its mtime…
	time.Sleep(1100 * time.Millisecond) // mtime has one-second resolution on some filesystems
	writeTestFile(t, sub, "new.txt")
	// …and rewriting a file moves its size.
	must(t, os.WriteFile(file, []byte("one\ntwo\nthree\n"), 0o644), "rewrite a.txt")

	after := svc.PollWorkspacePaths(sid, []string{sub, file})
	if after[0].MTimeMs <= before[0].MTimeMs {
		t.Errorf("a new entry must move the directory's mtime: %d -> %d", before[0].MTimeMs, after[0].MTimeMs)
	}
	if after[1].Size == before[1].Size {
		t.Errorf("rewriting a file must move its size: %d -> %d", before[1].Size, after[1].Size)
	}
}

// TestPollWorkspacePathsNeedsWorkspace: nothing to poll without a workspace, and the batch cap is a
// floor under a bug (the frontend sends dozens of paths, never hundreds).
func TestPollWorkspacePathsNeedsWorkspace(t *testing.T) {
	_, svc, sid := newWorkspacelessApp(t)
	if got := svc.PollWorkspacePaths(sid, []string{"/tmp"}); len(got) != 0 {
		t.Errorf("a session with no workspace must poll nothing, got %+v", got)
	}

	dir := t.TempDir()
	_, svc2, sid2 := newTreeApp(t, dir)
	many := make([]string, 0, fileTreePollMaxPaths+10)
	for i := 0; i < fileTreePollMaxPaths+10; i++ {
		many = append(many, filepath.Join(dir, fmt.Sprintf("f%05d", i)))
	}
	if got := svc2.PollWorkspacePaths(sid2, many); len(got) != fileTreePollMaxPaths {
		t.Errorf("got %d states, want the cap %d", len(got), fileTreePollMaxPaths)
	}
}

// TestFileTreeKeyIsTheWorkspace: the key is derived here so "what counts as the same workspace" has
// one definition — a project drives its members (so they share one expansion), anything else is its
// primary directory, and a session with no workspace remembers nothing at all.
func TestFileTreeKeyIsTheWorkspace(t *testing.T) {
	// A project-driven session, and a second member of the SAME project: one key for both.
	d, _, sid, pid := newProjectApp(t, "tachi", t.TempDir())
	eq(t, "project key", d.fileTreeKey(sid), "p:"+pid)
	second, err := d.sm.New("", t.TempDir())
	must(t, err, "second session")
	must(t, d.updateSessionMeta(second.ID, func(s *session.Session) { s.ProjectID = pid }), "bind second session")
	eq(t, "the other member of the same project", d.fileTreeKey(second.ID), "p:"+pid)

	// Ordinary sessions: their primary directory, and two unrelated ones are two keys.
	d1, _, sid1 := newTreeApp(t, t.TempDir())
	d2, _, sid2 := newTreeApp(t, t.TempDir())
	key1, key2 := d1.fileTreeKey(sid1), d2.fileTreeKey(sid2)
	if !strings.HasPrefix(key1, "d:") {
		t.Errorf("an ordinary session's key = %q, want a d: key", key1)
	}
	if key1 == key2 {
		t.Errorf("two unrelated workspaces share the key %q", key1)
	}

	// No primary directory: nothing to remember, so no key to remember it under.
	d3, _, sid3 := newWorkspacelessApp(t)
	eq(t, "no workspace", d3.fileTreeKey(sid3), "")
	eq(t, "an unknown session", d3.fileTreeKey("no-such-session"), "")
}

// TestFileTreeMemoFiltersOnRead: the memo stores ABSOLUTE paths (with several roots a relative one
// cannot say which tree it belongs to) and hands back only the ones still inside a root and still a
// directory. The filtered result is not written back: that would cost a file write every time the
// panel opens, and what was stored was only ever valid when it was stored.
func TestFileTreeMemoFiltersOnRead(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "src")
	must(t, os.MkdirAll(inside, 0o755), "mkdir inside")
	outside := t.TempDir()
	gone := filepath.Join(root, "deleted")
	must(t, os.MkdirAll(gone, 0o755), "mkdir gone")
	file := writeTestFile(t, root, "a.txt")
	must(t, os.RemoveAll(gone), "rm gone")

	_, svc, sid := newTreeApp(t, root)
	svc.SetFileTreeMemo(sid, []string{inside, outside, gone, file}, file)
	key := svc.desk.fileTreeKey(sid)
	if key == "" {
		t.Fatal("fixture: no workspace key")
	}
	// The stored record keeps everything it was given — the filter is the read side's.
	if stored := loadUIState().FileTreeMemo[key]; len(stored.Dirs) != 4 {
		t.Errorf("the setter filtered its input: %v", stored.Dirs)
	}

	memo := svc.GetFileTreeMemo(sid)
	eqSlices(t, "kept directories", memo.Dirs, []string{inside})
	eq(t, "the file on screen", memo.File, file)
	// The filtered result is not written back.
	if stored := loadUIState().FileTreeMemo[key]; len(stored.Dirs) != 4 {
		t.Errorf("the read filter wrote back: %v", stored.Dirs)
	}

	// A file outside every root may not be reopened at all — the same rule the directories obey.
	svc.SetFileTreeMemo(sid, []string{inside}, outside)
	eq(t, "a file outside the roots is not remembered", svc.GetFileTreeMemo(sid).File, "")

	// A file that no longer EXISTS is still remembered: reopening it says 文件已不存在, which is the
	// honest answer, where forgetting it would silently open the default view instead.
	svc.SetFileTreeMemo(sid, []string{inside}, file)
	must(t, os.Remove(file), "rm the remembered file")
	eq(t, "a deleted file stays remembered", svc.GetFileTreeMemo(sid).File, file)
}

// TestFileTreeMemoIsClamped: desktop_ui.json is hand-editable, so the setter clamps without trusting
// the reader.
func TestFileTreeMemoIsClamped(t *testing.T) {
	root := t.TempDir()
	_, svc, sid := newTreeApp(t, root)

	many := make([]string, 0, fileTreeMemoMaxDirs+20)
	for i := 0; i < fileTreeMemoMaxDirs+20; i++ {
		many = append(many, filepath.Join(root, "d", fmt.Sprintf("d%04d", i)))
	}
	svc.SetFileTreeMemo(sid, many, "")
	key := svc.desk.fileTreeKey(sid)
	eq(t, "stored directories", len(loadUIState().FileTreeMemo[key].Dirs), fileTreeMemoMaxDirs)

	// A hand-written file could repeat a path or carry an empty one; neither is stored twice.
	svc.SetFileTreeMemo(sid, []string{root, root, ""}, "")
	eq(t, "deduplicated directories", len(loadUIState().FileTreeMemo[key].Dirs), 1)
}

// TestFileTreeMemoSkipsAWorkspacelessSession: a session with no working directory has no tree to
// expand, and inventing a key from its id would remember one that nothing else can ever name.
func TestFileTreeMemoSkipsAWorkspacelessSession(t *testing.T) {
	_, svc, sid := newWorkspacelessApp(t)
	svc.SetFileTreeMemo(sid, []string{t.TempDir()}, "")
	if n := len(loadUIState().FileTreeMemo); n != 0 {
		t.Errorf("a workspaceless session wrote %d memo(s)", n)
	}
	if vo := svc.GetFileTreeMemo(sid); vo.Key != "" || len(vo.Dirs) != 0 {
		t.Errorf("a workspaceless session answered with a memo: %+v", vo)
	}
}

// TestFileTreeMemoEvictsOldestWorkspaces: the table is bounded by write time, so a reader who has
// visited many projects keeps the ones they actually use.
func TestFileTreeMemoEvictsOldestWorkspaces(t *testing.T) {
	memo := map[string]fileTreeMemo{}
	base := time.Now()
	for i := 0; i < fileTreeMemoMaxWorkspaces+1; i++ {
		memo[fmt.Sprintf("w%04d", i)] = fileTreeMemo{At: base.Add(time.Duration(i) * time.Second)}
	}
	evictFileTreeMemo(memo)
	eq(t, "kept workspaces", len(memo), fileTreeMemoMaxWorkspaces)
	if _, gone := memo["w0000"]; gone {
		t.Error("the least recently written workspace must be the one dropped")
	}
	if _, kept := memo[fmt.Sprintf("w%04d", fileTreeMemoMaxWorkspaces)]; !kept {
		t.Error("the workspace just written must survive")
	}
}

// TestFileTreeWidthBounds: the stored width is a preference, and a hand-edited value outside the
// bounds the layout can honour is refused instead of stored — mirroring the other two columns.
func TestFileTreeWidthBounds(t *testing.T) {
	_, svc, _ := newTreeApp(t, t.TempDir())
	svc.SetFileTreeWidth(fileTreeMaxWidth + 200)
	eq(t, "an out-of-bounds width", loadUIState().FileTreeWidth, 0)
	svc.SetFileTreeWidth(360)
	eq(t, "a width inside the bounds", loadUIState().FileTreeWidth, 360)
	svc.SetFileTreeOpen(true)
	if !loadUIState().FileTreeOpen {
		t.Error("the open flag must persist")
	}
}
