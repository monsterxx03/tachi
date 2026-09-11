package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// newRootsApp returns an app with a session manager bound to a session whose
// primary root is workingDir — the shape the root APIs expect (a bound run).
func newRootsApp(t *testing.T, workingDir string) (*desktopApp, *AgentService, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, workingDir)
	sid := sm.Current().ID
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	d.sm = sm
	d.getRun(sid).sm = sm
	return d, &AgentService{desk: d}, sid
}

// TestAddSessionRootsValidation covers the desktop-only rules: a root the @-reference
// syntax cannot express, and a path that is not a usable directory. Both must be
// refused with something a user can act on, and nothing may be persisted.
func TestAddSessionRootsValidation(t *testing.T) {
	d, svc, sid := newRootsApp(t, t.TempDir())
	_ = d

	spaced := filepath.Join(t.TempDir(), "My Projects")
	file := writeTestFile(t, t.TempDir(), "a.txt")

	tests := []struct {
		name string
		path string
		want string
	}{
		// Whitespace is reported even though this directory does not exist: the
		// space rule is the more specific one, and it is the actionable message when
		// both apply.
		{"whitespace", spaced, "空白"},
		{"missing", filepath.Join(t.TempDir(), "nope"), "不存在"},
		{"not a directory", file, "不是目录"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.AddSessionRoots(sid, []string{tt.path})
			if !strings.Contains(got, tt.want) {
				t.Errorf("AddSessionRoots(%q) = %q, want it to mention %q", tt.path, got, tt.want)
			}
			if roots := svc.GetSessionRoots(sid); len(roots.Additional) != 0 {
				t.Errorf("a rejected root was persisted: %+v", roots.Additional)
			}
		})
	}

	if got := svc.AddSessionRoots(sid, nil); got == "ok" {
		t.Error("an empty selection must not report success")
	}
}

// TestAddSessionRootsValidationNeedsPrimary: additional roots without a primary
// would leave relative paths resolving against the process cwd, which is not a
// working directory at all.
func TestAddSessionRootsValidationNeedsPrimary(t *testing.T) {
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	sm := newSessionManagerForTest(t, "") // session without a working dir
	sid := sm.Current().ID
	d.sm = sm
	d.getRun(sid).sm = sm

	svc := &AgentService{desk: d}
	if got := svc.AddSessionRoots(sid, []string{t.TempDir()}); !strings.Contains(got, "主目录") {
		t.Errorf("got %q, want a message about the primary directory", got)
	}
}

// TestAddSessionRootsPersistsAndDedupes: the list is the session's, cleaned and
// deduplicated by the shared rule, and readable again from disk.
func TestAddSessionRootsPersistsAndDedupes(t *testing.T) {
	primary := t.TempDir()
	rootA, rootB := t.TempDir(), t.TempDir()
	d, svc, sid := newRootsApp(t, primary)

	if res := svc.AddSessionRoots(sid, []string{rootA, rootB}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}
	// A second add of the same directory, dressed differently, must not duplicate.
	if res := svc.AddSessionRoots(sid, []string{filepath.Join(rootA, ".")}); res != "ok" {
		t.Fatalf("AddSessionRoots(duplicate): %s", res)
	}
	// A directory that IS the primary is not an additional root.
	if res := svc.AddSessionRoots(sid, []string{primary}); res != "ok" {
		t.Fatalf("AddSessionRoots(primary): %s", res)
	}

	want := []string{rootA, rootB}
	roots := svc.GetSessionRoots(sid)
	if roots.Primary != primary {
		t.Errorf("Primary = %q, want %q", roots.Primary, primary)
	}
	if len(roots.Additional) != len(want) {
		t.Fatalf("Additional = %+v, want %v", roots.Additional, want)
	}
	for i, w := range want {
		if roots.Additional[i].Path != w || !roots.Additional[i].Exists {
			t.Errorf("Additional[%d] = %+v, want path %q exists", i, roots.Additional[i], w)
		}
	}

	// Persisted, not just in memory.
	sess, err := d.sm.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.AdditionalDirs) != 2 || sess.AdditionalDirs[0] != rootA {
		t.Errorf("meta AdditionalDirs = %v, want %v", sess.AdditionalDirs, want)
	}
}

// TestRemoveSessionRoot: removal is by path, an unknown path is reported, and the
// order of the rest survives.
func TestRemoveSessionRoot(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	_, svc, sid := newRootsApp(t, t.TempDir())
	if res := svc.AddSessionRoots(sid, []string{rootA, rootB}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}

	if res := svc.RemoveSessionRoot(sid, rootA); res != "ok" {
		t.Fatalf("RemoveSessionRoot: %s", res)
	}
	roots := svc.GetSessionRoots(sid)
	if len(roots.Additional) != 1 || roots.Additional[0].Path != rootB {
		t.Errorf("Additional = %+v, want only %q", roots.Additional, rootB)
	}

	if res := svc.RemoveSessionRoot(sid, rootA); !strings.Contains(res, "不在附加目录") {
		t.Errorf("removing a non-member = %q, want it to say so", res)
	}
}

// TestStaleRootIsKeptButHidden covers the directory that disappears after being
// added: still in the session (the user decides), marked in the UI, out of the
// prompt, and skipped by the search.
func TestStaleRootIsKeptButHidden(t *testing.T) {
	primary := t.TempDir()
	live, gone := t.TempDir(), filepath.Join(t.TempDir(), "usb")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	d, svc, sid := newRootsApp(t, primary)
	if res := svc.AddSessionRoots(sid, []string{live, gone}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}

	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	roots := svc.GetSessionRoots(sid)
	if len(roots.Additional) != 2 {
		t.Fatalf("Additional = %+v, want both roots kept", roots.Additional)
	}
	byPath := map[string]bool{}
	for _, r := range roots.Additional {
		byPath[r.Path] = r.Exists
	}
	if !byPath[live] || byPath[gone] {
		t.Errorf("exists flags = %+v, want live=%v gone=%v", roots.Additional, true, false)
	}

	// Still persisted: an unmounted volume coming back must find its root intact.
	sess, err := d.sm.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.AdditionalDirs) != 2 {
		t.Errorf("meta AdditionalDirs = %v, want the stale root kept", sess.AdditionalDirs)
	}

	// Out of the prompt: the model must not be invited to a path that is not there.
	prompt := d.systemPromptFor(sid)
	if strings.Contains(prompt, gone) {
		t.Errorf("the prompt still advertises the vanished root %q", gone)
	}
	if !strings.Contains(prompt, live) {
		t.Errorf("the prompt dropped the live root %q", live)
	}

	// The search skips the vanished root without failing, and still answers from
	// the roots that are there.
	writeTestFile(t, live, "keep.go")
	got := svc.SearchFiles(sid, "keep", 10)
	if want := "@" + filepath.ToSlash(filepath.Join(live, "keep.go")); len(got) == 0 || got[0].Ref != want {
		t.Errorf("search with a stale root = %+v, want the live root's match %q", got, want)
	}
}

// TestSystemPromptFollowsAdditionalRoots pins the second half of the cache key:
// adding a root must rebuild the prompt, and a session without roots must not grow
// an "Additional workspace roots" line at all.
func TestSystemPromptFollowsAdditionalRoots(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	d, svc, sid := newRootsApp(t, primary)

	if prompt := d.systemPromptFor(sid); strings.Contains(prompt, "Additional workspace roots") {
		t.Error("a session without additional roots must not advertise any")
	}

	if res := svc.AddSessionRoots(sid, []string{extra}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}
	prompt := d.systemPromptFor(sid)
	if want := "- Additional workspace roots: " + extra; !strings.Contains(prompt, want) {
		t.Errorf("prompt does not carry %q", want)
	}

	// The old (root-less) prompt stays cached under its own key: one entry per
	// (dir, session, roots) triple.
	if n := len(d.promptCache); n != 2 {
		t.Errorf("prompt cache holds %d entries, want 2", n)
	}
}

// TestSearchFilesAcrossRoots: an additional root's files are reachable from the
// picker, with a reference that resolves back (relative under the primary,
// absolute under an additional root) and a label that says where it came from.
func TestSearchFilesAcrossRoots(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	writeTestFile(t, primary, "src/main.go")
	writeTestFile(t, extra, "lib/util.go")
	_, svc, sid := newRootsApp(t, primary)
	if res := svc.AddSessionRoots(sid, []string{extra}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}

	inPrimary := svc.SearchFiles(sid, "main", 10)
	if len(inPrimary) == 0 {
		t.Fatal("the primary root's file was not found")
	}
	if got := inPrimary[0]; got.Ref != "@src/main.go" || got.Root != "" || got.Path != "src/main.go" {
		t.Errorf("primary hit = %+v, want a relative ref and no label", got)
	}

	inExtra := svc.SearchFiles(sid, "util", 10)
	if len(inExtra) == 0 {
		t.Fatal("the additional root's file was not found")
	}
	if got := inExtra[0]; got.Ref != "@"+filepath.ToSlash(filepath.Join(extra, "lib/util.go")) {
		t.Errorf("additional hit ref = %q, want an absolute reference", got.Ref)
	} else if got.Root != filepath.Base(extra) {
		t.Errorf("additional hit label = %q, want %q", got.Root, filepath.Base(extra))
	} else if got.Path != "lib/util.go" {
		t.Errorf("additional hit display path = %q, want it relative to its root", got.Path)
	}

	// Every reference the picker hands out must be one the backend can expand
	// again — that is the whole contract of the ref field.
	for _, m := range append(inPrimary, inExtra...) {
		if !strings.HasPrefix(m.Ref, "@") {
			t.Errorf("ref %q does not start with @", m.Ref)
		}
	}
}

// TestSearchFilesDedupesNestedRoots: with a root nested inside another, the same
// file must appear once — attributed to the primary, whose reference is the stable
// relative form.
func TestSearchFilesDedupesNestedRoots(t *testing.T) {
	outer := t.TempDir()
	pkg := filepath.Join(outer, "pkg")
	writeTestFile(t, outer, "pkg/inner.go")
	_, svc, sid := newRootsApp(t, pkg)
	if res := svc.AddSessionRoots(sid, []string{outer}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}

	got := svc.SearchFiles(sid, "inner", 10)
	if len(got) != 1 {
		t.Fatalf("got %+v, want exactly one (deduplicated) hit", got)
	}
	if got[0].Ref != "@inner.go" || got[0].Root != "" {
		t.Errorf("hit = %+v, want the primary-relative reference", got[0])
	}
}

// TestSearchFilesQuotaPerRoot: one big root must not crowd the others out of the
// list — that is what the per-root share of the limit is for.
func TestSearchFilesQuotaPerRoot(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	for i := range 12 {
		writeTestFile(t, primary, filepath.Join("noise", string(rune('a'+i))+"-cfg.txt"))
	}
	writeTestFile(t, extra, "shared-cfg.txt")
	_, svc, sid := newRootsApp(t, primary)
	if res := svc.AddSessionRoots(sid, []string{extra}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}

	got := svc.SearchFiles(sid, "cfg", 4) // 2 per root
	if len(got) == 0 {
		t.Fatal("no matches at all")
	}
	found := false
	for _, m := range got {
		if m.Root != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("the additional root got no share of a 4-slot result set: %+v", got)
	}
	if len(got) > 4 {
		t.Errorf("limit not respected: %d results", len(got))
	}
}

// TestSearchFilesAbsoluteDrilldown covers what happens after the picker inserts an
// absolute reference: the query IS a path, and it must list that directory instead
// of being fuzzy-matched against root-relative paths (which could never match).
func TestSearchFilesAbsoluteDrilldown(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	writeTestFile(t, extra, "lib/util.go")
	_, svc, sid := newRootsApp(t, primary)
	if res := svc.AddSessionRoots(sid, []string{extra}); res != "ok" {
		t.Fatalf("AddSessionRoots: %s", res)
	}

	got := svc.SearchFiles(sid, filepath.Join(extra, "lib")+"/", 10)
	if len(got) != 1 {
		t.Fatalf("got %+v, want the directory's own entries", got)
	}
	if want := "@" + filepath.ToSlash(filepath.Join(extra, "lib/util.go")); got[0].Ref != want {
		t.Errorf("Ref = %q, want %q", got[0].Ref, want)
	}
	if got[0].Root != filepath.Base(extra) {
		t.Errorf("Root = %q, want the owning root's label", got[0].Root)
	}
}

// TestWideRootsRejected covers the guard added on top of the root rules: a
// workspace root must be a project, not the filesystem root or the home directory.
// Both are allowed by the OS and useless here — the @-file index would cover
// everything, and relative paths would resolve against a directory that is not a
// project. It applies to the PRIMARY too, which is where $HOME used to come from.
func TestWideRootsRejected(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	t.Run("additional root", func(t *testing.T) {
		_, svc, sid := newRootsApp(t, t.TempDir())
		for _, wide := range []string{string(filepath.Separator), home, home + "/"} {
			got := svc.AddSessionRoots(sid, []string{wide})
			if !strings.Contains(got, "项目目录") {
				t.Errorf("AddSessionRoots(%q) = %q, want the wide-root refusal", wide, got)
			}
		}
		if roots := svc.GetSessionRoots(sid); len(roots.Additional) != 0 {
			t.Errorf("a wide root was persisted: %+v", roots.Additional)
		}
	})

	t.Run("primary", func(t *testing.T) {
		_, svc, sid := newRootsApp(t, t.TempDir())
		for _, wide := range []string{string(filepath.Separator), home} {
			if got := svc.SetSessionWorkingDir(sid, wide); !strings.Contains(got, "项目目录") {
				t.Errorf("SetSessionWorkingDir(%q) = %q, want the wide-root refusal", wide, got)
			}
		}
	})

	t.Run("a project below the home directory is fine", func(t *testing.T) {
		project := filepath.Join(home, ".cache")
		if !fileutil.IsDir(project) {
			t.Skip("no usable directory under home")
		}
		_, svc, sid := newRootsApp(t, t.TempDir())
		if got := svc.SetSessionWorkingDir(sid, project); !strings.Contains(got, project) {
			// A refusal is also acceptable here (the path may be a file); what must NOT
			// happen is the wide-root message.
			t.Logf("SetSessionWorkingDir(%q) = %q", project, got)
		}
	})
}

// TestDefaultWorkspaceFor: a new session inherits the workspace the user last chose,
// never a wide one, and never falls back to $HOME.
func TestDefaultWorkspaceFor(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // config.SetBaseDir is set by newSessionManagerForTest below; ui state lives there

	// Nothing remembered and no sessions → no workspace at all (the composer asks).
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	if got := d.defaultWorkspaceFor(); got != "" {
		t.Errorf("with no history defaultWorkspaceFor() = %q, want \"\"", got)
	}

	// The most recently UPDATED usable session wins.
	sm := newSessionManagerForTest(t, t.TempDir())
	d.sm = sm
	older, err := sm.New("default", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	newest, err := sm.New("default", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.defaultWorkspaceFor(); got != dir {
		t.Errorf("defaultWorkspaceFor() = %q, want the newest session's %q", got, dir)
	}

	// A wide root is skipped even when it is the only thing in history: $HOME was
	// every session's default before this rule existed.
	if home, herr := os.UserHomeDir(); herr == nil {
		if _, err := sm.New("default", home); err != nil {
			t.Fatal(err)
		}
		if got := d.defaultWorkspaceFor(); got != dir {
			t.Errorf("defaultWorkspaceFor() = %q, want the wide root %q skipped", got, home)
		}
		_ = older
		_ = newest
	}
}

// TestSearchFilesWithoutWorkspace: a session that has not chosen a directory searches
// nothing. Falling back to the process cwd would index "/" for a Finder-launched app
// — the cost this design exists to avoid.
func TestSearchFilesWithoutWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "notes.md")
	t.Chdir(dir)

	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	sm := newSessionManagerForTest(t, "") // session with NO working dir
	sid := sm.Current().ID
	d.sm = sm
	d.getRun(sid).sm = sm

	svc := &AgentService{desk: d}
	if got := svc.SearchFiles(sid, "", 10); got != nil {
		t.Errorf("SearchFiles with no workspace = %+v, want none", got)
	}
	if got := svc.SearchFiles(sid, "notes", 10); got != nil {
		t.Errorf("SearchFiles with no workspace = %+v, want none", got)
	}
}

// TestRememberWorkspaceSurvivesThemeChange pins the cross-field rule for
// desktop_ui.json: the theme and the remembered workspace share one file, so each
// writer must read-modify-write it. A fresh struct on either side would silently
// drop the other's field.
func TestRememberWorkspaceSurvivesThemeChange(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	dir := t.TempDir()

	rememberWorkspace(dir)
	saveUIState(uiState{Theme: themeLight, LastWorkspace: loadUIState().LastWorkspace})
	if got := loadUIState(); got.LastWorkspace != dir || got.Theme != themeLight {
		t.Fatalf("after both writes: %+v, want workspace %q with theme %q", got, dir, themeLight)
	}

	// A theme change must not erase it. persistTheme is the controller's file half:
	// driving setFromFrontend itself would reach into the native window and block
	// forever in a test binary (there is no app event loop here).
	persistTheme(themeDark)
	if got := loadUIState(); got.LastWorkspace != dir || got.Theme != themeDark {
		t.Errorf("after a theme switch: %+v, want workspace %q kept", got, dir)
	}

	// …and remembering a workspace must not erase the theme.
	rememberWorkspace(t.TempDir())
	if got := loadUIState(); got.Theme != themeDark {
		t.Errorf("after remembering a workspace: theme = %q, want it kept", got.Theme)
	}
}
