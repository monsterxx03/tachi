package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// Plain helpers, not an assertion library: the desktop module deliberately has no test
// dependency of its own (every other _test.go here is written the same way).
func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func eqSlices(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// newProjectApp returns an app holding one project rooted at projectDir and ONE session that
// was started from it. The session's roots are COPIED from the project (copyProjectRoots), which
// is exactly what NewSession does, so the fixture is a normal session of the new model rather than
// a hand-built half-state.
//
// The project is created through the API — it is the only writer — and the copy is written through
// the session's own update path, so nothing here is reading a structure it also built by hand.
func newProjectApp(t *testing.T, name, projectDir string) (d *desktopApp, svc *AgentService, sessionID, projectID string) {
	t.Helper()
	snapshot := t.TempDir() // the session's record: stale by construction
	sm := newSessionManagerForTest(t, snapshot)
	sid := sm.Current().ID

	d = newTestApp()
	d.cfg = &config.Config{Language: "en"}
	d.sm = sm
	d.getRun(sid).sm = sm
	svc = &AgentService{desk: d}

	if res := svc.CreateProject(name, projectDir, nil); res != "ok" {
		t.Fatalf("CreateProject: %s", res)
	}
	projects := svc.ListProjects()
	if len(projects) != 1 {
		t.Fatalf("fixture: %d projects, want 1", len(projects))
	}
	pid := projects[0].ID

	proj, primary, ok := d.projects.drives(pid)
	if !ok {
		t.Fatal("fixture: the project does not drive")
	}
	must(t, d.updateSessionMeta(sid, func(s *session.Session) { copyProjectRoots(s, proj, primary) }), "bind session")
	return d, svc, sid, pid
}

// newAppWithSession builds a bare app around one session manager whose current session is
// rooted at dir — the shape every root API expects.
func newAppWithSession(t *testing.T, dir string) (*desktopApp, *AgentService, string) {
	t.Helper()
	sm := newSessionManagerForTest(t, dir)
	sid := sm.Current().ID
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	d.sm = sm
	d.getRun(sid).sm = sm
	return d, &AgentService{desk: d}, sid
}

// writeProjectsFile puts a hand-written table in place and returns a FRESH app, so the
// lazy load actually reads it (an app that already loaded keeps its own copy).
func writeProjectsFile(t *testing.T, body string) *desktopApp {
	t.Helper()
	path := filepath.Join(config.BaseDir(), projectsFileName)
	must(t, os.WriteFile(path, []byte(body), 0o644), "write projects.json")
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	return d
}

// restoreBase points the process-global config.BaseDir at dir for the rest of ONE test and
// puts the old value back when it ends. SetBaseDir is not test-scoped, and t.TempDir() is
// deleted at cleanup — a leaked value is a live path pointing at nothing for whichever test
// runs next.
func restoreBase(t *testing.T, dir string) {
	t.Helper()
	old := config.BaseDir()
	config.SetBaseDir(dir)
	t.Cleanup(func() { config.SetBaseDir(old) })
}

// projectJSON renders a one-project table for a fixture, quoting the path properly.
func projectJSON(t *testing.T, id, name, primary string) string {
	t.Helper()
	body, err := json.Marshal(projectFile{Projects: []*project{
		{ID: id, Name: name, WorkingDir: primary},
	}})
	must(t, err, "marshal project fixture")
	return string(body)
}

// TestProjectStoreRoundTrip: every project write lands on disk, and a fresh app (a new
// process, in effect) reads back exactly what was written.
func TestProjectStoreRoundTrip(t *testing.T) {
	primary, extra := t.TempDir(), t.TempDir()
	_, svc, _, pid := newProjectApp(t, "tachi", primary)

	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, primary, []string{extra}), "ok")
	eq(t, "RenameProject", svc.RenameProject(pid, "tachi-renamed"), "ok")

	fresh := &AgentService{desk: newTestApp()}
	projects := fresh.ListProjects()
	if len(projects) != 1 {
		t.Fatalf("got %d projects, want 1", len(projects))
	}
	eq(t, "name", projects[0].Name, "tachi-renamed")
	eq(t, "workingDir", projects[0].WorkingDir, primary)
	if len(projects[0].AdditionalDirs) != 1 || projects[0].AdditionalDirs[0].Path != extra {
		t.Errorf("additional dirs = %+v, want [%s]", projects[0].AdditionalDirs, extra)
	}
	if !projects[0].RootsUsable {
		t.Error("a fresh project must report usable roots")
	}

	t.Run("missing file", func(t *testing.T) {
		restoreBase(t, t.TempDir())
		if got := (&AgentService{desk: newTestApp()}).ListProjects(); len(got) != 0 {
			t.Errorf("a missing projects.json must read as no projects, got %+v", got)
		}
		if _, err := os.Stat(filepath.Join(config.BaseDir(), projectsFileName)); !os.IsNotExist(err) {
			t.Error("merely reading must not create the file")
		}
	})

	// A file we cannot parse is a file the user can still repair (it is state, and hand
	// editable). Reads must degrade — the desktop works without projects — but a write must
	// NOT happen: saving the new project would replace their whole list with that one entry.
	t.Run("corrupt file", func(t *testing.T) {
		restoreBase(t, t.TempDir())
		corrupt := "{ this is not json"
		svc := &AgentService{desk: writeProjectsFile(t, corrupt)}
		if got := svc.ListProjects(); len(got) != 0 {
			t.Errorf("a corrupt projects.json must degrade to no projects, got %+v", got)
		}
		res := svc.CreateProject("", t.TempDir(), nil)
		if !strings.Contains(res, "无法读取或解析") {
			t.Errorf("create over a corrupt file = %q, want a refusal naming the file", res)
		}
		onDisk, err := os.ReadFile(filepath.Join(config.BaseDir(), projectsFileName))
		must(t, err, "read back projects.json")
		if string(onDisk) != corrupt {
			t.Errorf("the corrupt file must be left untouched, got %q", onDisk)
		}
		// Repairing it brings the feature back in the same process — no restart.
		must(t, os.WriteFile(filepath.Join(config.BaseDir(), projectsFileName), []byte(`{"projects":[]}`), 0o644), "repair projects.json")
		if res := svc.CreateProject("", t.TempDir(), nil); res != "ok" {
			t.Errorf("creating a project after a repair: %s", res)
		}
	})
}

// TestCreateProjectDefaults: the name comes from the directory, and a collision gets -2
// rather than two projects that read the same.
func TestCreateProjectDefaults(t *testing.T) {
	base := t.TempDir()
	foo, bar := filepath.Join(base, "foo"), filepath.Join(base, "bar")
	must(t, os.MkdirAll(foo, 0o755), "mkdir foo")
	must(t, os.MkdirAll(bar, 0o755), "mkdir bar")

	_, svc, _ := newAppWithSession(t, "")
	for _, dir := range []string{foo, foo, bar} {
		if res := svc.CreateProject("", dir, nil); res != "ok" {
			t.Fatalf("CreateProject(%s): %s", dir, res)
		}
	}

	names := map[string]bool{}
	for _, p := range svc.ListProjects() {
		names[p.Name] = true
	}
	if !names["foo"] || !names["foo-2"] || !names["bar"] {
		t.Errorf("want foo / foo-2 / bar, got %v", names)
	}
}

// TestCreateProjectValidation: the project layer applies the session rules and no fewer —
// a bad root here would be inherited by every member session.
func TestCreateProjectValidation(t *testing.T) {
	_, svc, _ := newAppWithSession(t, "")
	cases := []struct {
		name    string
		primary string
		extra   []string
		want    string
	}{
		{name: "empty", primary: "   ", want: "主目录"},
		{name: "wide", primary: t.TempDir() + "/..", want: "项目目录"},
		{name: "missing", primary: filepath.Join(t.TempDir(), "gone"), want: "不存在"},
		{name: "spaced additional root", primary: t.TempDir(),
			extra: []string{filepath.Join(t.TempDir(), "My Projects")}, want: "空白"},
		{name: "wide additional root", primary: t.TempDir(), extra: []string{"/"}, want: "项目目录"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := svc.CreateProject("", tc.primary, tc.extra)
			if !strings.Contains(got, tc.want) {
				t.Errorf("CreateProject(%q, %v) = %q, want it to mention %q", tc.primary, tc.extra, got, tc.want)
			}
		})
	}
	if got := svc.ListProjects(); len(got) != 0 {
		t.Errorf("a refused project was persisted: %+v", got)
	}
}

// TestProjectEditLeavesMembersAlone is the model in one test: a project is a STARTING POINT.
// Editing it moves the next session to be created and leaves every existing one exactly where it
// was — same primary, same additional roots, same everything the tools read.
func TestProjectEditLeavesMembersAlone(t *testing.T) {
	projectDir, movedDir, extra := t.TempDir(), t.TempDir(), t.TempDir()
	d, svc, sid, pid := newProjectApp(t, "p", projectDir)

	eq(t, "member primary", svc.GetSessionWorkingDir(sid), projectDir)

	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, movedDir, []string{extra}), "ok")

	// The existing session did not move — not on disk, not in the resolution the tools read.
	primary, additional := d.sessionRoots(sid)
	eq(t, "primary after the move", primary, projectDir)
	if len(additional) != 0 {
		t.Errorf("additional after the move = %v, want none", additional)
	}
	eq(t, "the record did not move", d.sessionRecord(sid).WorkingDir, projectDir)

	// …and every consumer still agrees, because they all read that one exit.
	r := d.getRun(sid)
	eq(t, "expansionRoot", d.expansionRoot(r), projectDir)
	ctx, cancel, _, _, ok := d.beginTurn(sid, r)
	if !ok {
		t.Fatal("beginTurn refused")
	}
	t.Cleanup(cancel)
	eq(t, "turn wdctx", wdctx.Dir(ctx), projectDir)
	if prompt := d.systemPromptFor(sid); !strings.Contains(prompt, projectDir) {
		t.Error("the prompt must still name the session's own directory")
	}

	// The panel can say the session no longer matches the project…
	roots := svc.GetSessionRoots(sid)
	eq(t, "primary in the panel", roots.Primary, projectDir)
	if !roots.Changed {
		t.Error("the panel must be able to say the session no longer matches its project")
	}
	if roots.ProjectGone {
		t.Error("a usable project is not gone")
	}
	if !d.workspaceChanged(d.sessionRecord(sid)) {
		t.Error("the sidebar's dot reads the same comparison and must agree")
	}

	// …and the NEXT session starts where the project now points.
	info := svc.NewSession(pid)
	if info.ID == "" {
		t.Fatal("NewSession returned no session")
	}
	rec := d.sessionRecord(info.ID)
	eq(t, "the new session starts in the project's current directory", rec.WorkingDir, movedDir)
	eqSlices(t, "the new session's roots", rec.AdditionalDirs, []string{extra})
	if d.workspaceChanged(rec) {
		t.Error("a freshly created session matches its project by definition")
	}
}

// TestSessionOwnsItsWorkspace: the session-level writers are open to every session, including one
// started from a project, and they move the session WITHOUT touching the project.
func TestSessionOwnsItsWorkspace(t *testing.T) {
	projectDir := t.TempDir()
	d, svc, sid, pid := newProjectApp(t, "shared-lib", projectDir)

	newDir := t.TempDir()
	eq(t, "SetSessionWorkingDir", svc.SetSessionWorkingDir(sid, newDir), "ok")
	eq(t, "the session moved", d.sessionRecord(sid).WorkingDir, newDir)

	extra := t.TempDir()
	eq(t, "AddSessionRoots", svc.AddSessionRoots(sid, []string{extra}), "ok")
	eqSlices(t, "roots", d.sessionRecord(sid).AdditionalDirs, []string{extra})
	eq(t, "RemoveSessionRoot", svc.RemoveSessionRoot(sid, extra), "ok")
	eqSlices(t, "roots after remove", d.sessionRecord(sid).AdditionalDirs, []string{})

	// The project is where it always was: one session changing its own directory is not an edit.
	projects := svc.ListProjects()
	if len(projects) != 1 || projects[0].ID != pid {
		t.Fatalf("projects = %+v, want the one project", projects)
	}
	eq(t, "project primary", projects[0].WorkingDir, projectDir)
	eq(t, "project name", projects[0].Name, "shared-lib")

	roots := svc.GetSessionRoots(sid)
	eq(t, "project name is still provenance", roots.ProjectName, "shared-lib")
	if !roots.Changed {
		t.Error("a session moved away from its project must report the difference")
	}
}

// TestDanglingProjectIsOnlyProvenance: a project_id that no longer resolves (deleted, or
// projects.json lost) changes nothing about the session's workspace — it was the session's own all
// along — and the panel says the reference is gone.
func TestDanglingProjectDegradesToSnapshot(t *testing.T) {
	snapshotDir := t.TempDir()
	d, svc, sid := newAppWithSession(t, snapshotDir)
	must(t, d.updateSessionMeta(sid, func(s *session.Session) { s.ProjectID = "no-such-project" }), "bind")

	primary, additional := d.sessionRoots(sid)
	eq(t, "primary", primary, snapshotDir)
	if len(additional) != 0 {
		t.Errorf("additional = %v, want none", additional)
	}

	roots := svc.GetSessionRoots(sid)
	eq(t, "projectId", roots.ProjectID, "no-such-project")
	if !roots.ProjectGone {
		t.Error("the UI must be able to say the project is gone")
	}
	if roots.Changed {
		t.Error("a gone project is not something a session can differ from")
	}
	eq(t, "projectName", roots.ProjectName, "")

	eq(t, "still editable", svc.SetSessionWorkingDir(sid, t.TempDir()), "ok")
}

// TestProjectReadSideValidation: projects.json is a file anyone can edit, so an entry whose
// roots stopped validating must never be copied into a session. A session already bound to such a
// project keeps its own roots and the panel reports the reference as gone.
func TestProjectReadSideValidation(t *testing.T) {
	cases := []struct {
		name    string
		primary string
	}{
		{name: "filesystem root", primary: "/"},
		{name: "state directory", primary: config.BaseDir()},
		{name: "vanished directory", primary: filepath.Join(os.TempDir(), "tachi-tests-gone")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := t.TempDir()
			sm := newSessionManagerForTest(t, snapshot)
			sid := sm.Current().ID

			d := writeProjectsFile(t, projectJSON(t, "p1", "bad", tc.primary))
			d.sm = sm
			d.getRun(sid).sm = sm
			must(t, d.updateSessionMeta(sid, func(s *session.Session) { s.ProjectID = "p1" }), "bind")
			svc := &AgentService{desk: d}

			primary, _ := d.sessionRoots(sid)
			eq(t, "primary", primary, snapshot)

			roots := svc.GetSessionRoots(sid)
			if !roots.ProjectGone {
				t.Error("an unusable project must be reported as gone")
			}
			eq(t, "projectName", roots.ProjectName, "bad")
			eq(t, "still editable", svc.SetSessionWorkingDir(sid, t.TempDir()), "ok")
			// And a session created here is an ordinary one: a broken project cannot hand out roots.
			if info := svc.NewSession("p1"); info.ProjectID == "p1" {
				t.Error("a project that cannot hand out roots must not bind a new session")
			}
		})
	}
}

// TestDirectoryChangeMarksTreeStale: skills are the one thing a directory change does NOT move by
// itself — a store's scan roots are fixed when it is built — so the session's own directory change
// has to mark the agent. A PROJECT edit does not: no member's workspace moved with it.
func TestDirectoryChangeMarksTreeStale(t *testing.T) {
	treeA, treeB := t.TempDir(), t.TempDir()
	writeProjectSkillFixture(t, treeA, "skill-a", "from tree A")
	writeProjectSkillFixture(t, treeB, "skill-b", "from tree B")

	d, svc, sid, pid := newProjectApp(t, "p", treeA)

	// The state buildAgentForSession leaves behind: an agent whose store is rooted at the
	// session's tree. A bare agent suffices — reload only needs the registry.
	a := &agent.AIAgent{Config: agent.AgentConfig{
		ToolRegistry: tools.NewRegistry(),
		Logger:       logger.Default(),
	}}
	a.ReloadSkillsIn(treeA)
	if !slices.Contains(storeSkillNames(a.SkillStore()), "skill-a") {
		t.Fatal("fixture: the store did not start on tree A")
	}
	r := d.getRun(sid)
	r.agent = a

	// Editing the project moves nothing, so it must not invalidate a live member.
	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, treeB, nil), "ok")
	if r.treeStale {
		t.Error("a project edit must not mark a member's tree stale — it did not move")
	}

	// Moving the SESSION does, and the next turn applies it.
	eq(t, "SetSessionWorkingDir", svc.SetSessionWorkingDir(sid, treeB), "ok")
	if !r.treeStale {
		t.Fatal("a directory change must mark the session as needing a skill-store reload")
	}

	ctx, cancel, _, _, ok := d.beginTurn(sid, r)
	if !ok {
		t.Fatal("beginTurn refused")
	}
	t.Cleanup(cancel)
	eq(t, "turn wdctx", wdctx.Dir(ctx), treeB)

	names := storeSkillNames(a.SkillStore())
	if !slices.Contains(names, "skill-b") {
		t.Errorf("the store must follow the session, got %v", names)
	}
	if slices.Contains(names, "skill-a") {
		t.Errorf("the old tree's skills must be gone, got %v", names)
	}
	if r.treeStale {
		t.Error("the reload must clear the flag")
	}
}

// TestCheckpointRootsComeFromTheSession: the agent resolves its checkpoint roots through the same
// exit, and that exit now reads the SESSION only — so a project edit cannot move what a turn
// snapshots. A project-less session keeps the agent's own default shape exactly (the empty primary).
func TestCheckpointRootsComeFromTheSession(t *testing.T) {
	projectDir, extra, added := t.TempDir(), t.TempDir(), t.TempDir()
	d, svc, sid, pid := newProjectApp(t, "p", projectDir)
	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, projectDir, []string{extra}), "ok")

	// The project's extra root did not come along: this session was created before it.
	eqSlices(t, "checkpoint roots", d.checkpointRootsFor(d.sessionRecord(sid)), []string{projectDir})

	// The session's OWN root does.
	eq(t, "AddSessionRoots", svc.AddSessionRoots(sid, []string{added}), "ok")
	eqSlices(t, "checkpoint roots after adding", d.checkpointRootsFor(d.sessionRecord(sid)),
		[]string{projectDir, added})

	plainApp, _, plainSID := newAppWithSession(t, "")
	eqSlices(t, "project-less checkpoint roots",
		plainApp.checkpointRootsFor(plainApp.sessionRecord(plainSID)), []string{""})
}

// TestProjectNameUniqueness: the name is the sidebar's group label, so two projects must
// never read the same — and that has to hold for the names a user TYPES, not only for the
// -2/-3 suffixes projectNameFor derives.
func TestProjectNameUniqueness(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	_, svc, _ := newAppWithSession(t, "")

	eq(t, "first create", svc.CreateProject("shared", dirA, nil), "ok")
	if res := svc.CreateProject("shared", dirB, nil); !strings.Contains(res, "同名") {
		t.Errorf("a duplicate name must be refused, got %q", res)
	}
	if got := svc.ListProjects(); len(got) != 1 {
		t.Errorf("a refused create must not persist, got %d projects", len(got))
	}

	eq(t, "second create", svc.CreateProject("other", dirB, nil), "ok")
	var other, shared string
	for _, p := range svc.ListProjects() {
		if p.Name == "shared" {
			shared = p.ID
		} else {
			other = p.ID
		}
	}
	if other == "" || shared == "" {
		t.Fatalf("fixture: expected the two projects, got %s / %s", other, shared)
	}
	if res := svc.RenameProject(other, "shared"); !strings.Contains(res, "同名") {
		t.Errorf("renaming onto a taken name = %q, want a refusal", res)
	}
	// Its own name is not a collision: that rename stays the no-op success it was.
	eq(t, "rename to its own name", svc.RenameProject(other, "other"), "ok")
	names := map[string]bool{}
	for _, p := range svc.ListProjects() {
		names[p.Name] = true
	}
	if !names["shared"] || !names["other"] || len(names) != 2 {
		t.Errorf("names after the refused rename = %v, want exactly shared / other", names)
	}
}

// TestDeadAdditionalRootSurvivesInTheSession: an additional root that disappeared (an unmounted
// volume) is a fact about that ONE directory: it stays in the session's set, is reported as
// exists=false, and the prompt/@-file search skip it. Nothing else degrades with it.
func TestDeadAdditionalRootSurvivesInTheSession(t *testing.T) {
	projectDir, dead := t.TempDir(), filepath.Join(t.TempDir(), "unmounted")
	must(t, os.MkdirAll(dead, 0o755), "mkdir the extra root")

	d, svc, _, pid := newProjectApp(t, "p", projectDir)
	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, projectDir, []string{dead}), "ok")
	// Created AFTER the edit, so the dead root is copied in — a mounted-then-unmounted volume.
	info := svc.NewSession(pid)
	if info.ID == "" {
		t.Fatal("NewSession returned no session")
	}
	sid := info.ID

	must(t, os.RemoveAll(dead), "unmount the extra root")

	primary, additional := d.sessionRoots(sid)
	eq(t, "primary", primary, projectDir)
	eqSlices(t, "additional", additional, []string{dead})

	projects := svc.ListProjects()
	if len(projects) != 1 || !projects[0].RootsUsable {
		t.Errorf("a dead ADDITIONAL root must not make the project unusable: %+v", projects)
	}

	roots := svc.GetSessionRoots(sid)
	if len(roots.Additional) != 1 || roots.Additional[0].Exists {
		t.Errorf("the dead root must be reported with exists=false: %+v", roots.Additional)
	}
	// And the session is editable like any other.
	eq(t, "editable", svc.SetSessionWorkingDir(sid, t.TempDir()), "ok")
}

// TestNewSessionInsideProject: creating a session inside a project copies the project's roots as
// they are at that moment — a copy, not a link. From then on the session is on its own, so a later
// project edit moves the NEXT session and not this one.
func TestNewSessionInsideProject(t *testing.T) {
	projectDir, extra := t.TempDir(), t.TempDir()
	d, svc, _, pid := newProjectApp(t, "p", projectDir)
	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, projectDir, []string{extra}), "ok")

	info := svc.NewSession(pid)
	if info.ID == "" {
		t.Fatal("NewSession returned no session")
	}
	eq(t, "provenance", info.ProjectID, pid)

	// The record carries the roots themselves, so every reader (desktop or not) agrees.
	rec := d.sessionRecord(info.ID)
	eq(t, "primary", rec.WorkingDir, projectDir)
	eqSlices(t, "additional", rec.AdditionalDirs, []string{extra})
	eq(t, "GetSessionWorkingDir", svc.GetSessionWorkingDir(info.ID), projectDir)

	// A later project edit does not reach it…
	moved := t.TempDir()
	eq(t, "move the project", svc.SetProjectRoots(pid, moved, nil), "ok")
	eq(t, "the session stays", svc.GetSessionWorkingDir(info.ID), projectDir)
	eq(t, "it differs from the project now", svc.GetSessionRoots(info.ID).Changed, true)

	// …and it is editable like any other session.
	own := t.TempDir()
	eq(t, "SetSessionWorkingDir", svc.SetSessionWorkingDir(info.ID, own), "ok")
	eq(t, "the project did not move", svc.ListProjects()[0].WorkingDir, moved)
}

// TestNewSessionWithUnusableProjectIsUnbound: asking for a session in a project that cannot
// drive one (deleted, or its primary gone) yields an ORDINARY session. Binding it would create
// a member that is degraded from birth; §8.6's fallback is for a project that disappears under
// a session that already exists.
func TestNewSessionWithUnusableProjectIsUnbound(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "gone")

	// The manager first: it is what repoints config.BaseDir, and projects.json must land in the
	// directory the app will read (same ordering as TestProjectReadSideValidation).
	sm := newSessionManagerForTest(t, t.TempDir())
	d := writeProjectsFile(t, projectJSON(t, "p1", "dead", dead))
	d.sm = sm
	svc := &AgentService{desk: d}

	info := svc.NewSession("p1")
	if info.ID == "" {
		t.Fatal("NewSession returned no session")
	}
	eq(t, "unbound", info.ProjectID, "")
	rec := d.sessionRecord(info.ID)
	eq(t, "the record is unbound too", rec.ProjectID, "")
	if rec.WorkingDir == dead {
		t.Error("a session must not be started in a directory that does not exist")
	}
	// The project itself is untouched: refusing to bind is not a delete.
	eq(t, "the project is still listed", len(svc.ListProjects()), 1)
}

// TestCreateProjectDoesNotRememberWorkspace is design §6.1's second half: a project's directory
// is not the answer to "where should the next PROJECT-LESS session start", so creating a project
// must not move the remembered workspace the way SetSessionWorkingDir does.
func TestCreateProjectDoesNotRememberWorkspace(t *testing.T) {
	_, svc, _ := newAppWithSession(t, "")
	before := loadUIState().LastWorkspace
	eq(t, "CreateProject", svc.CreateProject("p", t.TempDir(), nil), "ok")
	eq(t, "lastWorkspace unchanged", loadUIState().LastWorkspace, before)
}

// TestDeleteProjectDetachesMembers: deleting a project removes the GROUP and nothing else. Every
// member keeps the directory it was working in (it owns it already), loses only project_id, and the
// delete is refused outright while a member is mid-turn.
func TestDeleteProjectDetachesMembers(t *testing.T) {
	projectDir := t.TempDir()
	d, svc, sid, pid := newProjectApp(t, "p", projectDir)

	before := d.sessionRecord(sid).WorkingDir
	eq(t, "fixture: the session works in the project's directory", before, projectDir)

	// A running member refuses the delete, and nothing is written.
	r := d.getRun(sid)
	r.running = true
	if got := svc.DeleteProject(pid); !strings.Contains(got, "正在运行") {
		t.Fatalf("DeleteProject with a member running = %q, want the refusal", got)
	}
	eq(t, "still listed", len(svc.ListProjects()), 1)
	eq(t, "still bound", d.sessionRecord(sid).ProjectID, pid)
	r.running = false

	eq(t, "DeleteProject", svc.DeleteProject(pid), "ok")

	if got := svc.ListProjects(); len(got) != 0 {
		t.Errorf("the project must be gone from the table, got %+v", got)
	}

	// The member is an ordinary session on the same directory — deleting the container must not
	// move anyone's workspace.
	rec := d.sessionRecord(sid)
	eq(t, "primary unchanged", rec.WorkingDir, before)
	eq(t, "unbound", rec.ProjectID, "")
	// The bound manager was updated too, not just a loaded copy: otherwise the next UpdateMeta
	// writes the binding back from the in-memory struct (see UpdateMeta's doc).
	eq(t, "the live copy is unbound", d.getRun(sid).sm.Current().ProjectID, "")
	// On disk as well — the next process must read the same thing.
	loaded, err := d.sm.Load(sid)
	must(t, err, "reload the detached session")
	eq(t, "on disk", loaded.ProjectID, "")
	eq(t, "on disk primary", loaded.WorkingDir, before)

	roots := svc.GetSessionRoots(sid)
	eq(t, "no project marks", roots.ProjectID, "")
	if roots.ProjectGone || roots.Changed {
		t.Error("an unbound session has no project to be gone or different from")
	}
	eq(t, "editable", svc.SetSessionWorkingDir(sid, t.TempDir()), "ok")

	// Deleting it twice is a plain "no such project", not a second detach.
	if got := svc.DeleteProject(pid); got != "项目不存在" {
		t.Errorf("DeleteProject twice = %q, want 项目不存在", got)
	}
}

// TestDeleteProjectWithDeadPrimaryKeepsTheSnapshot: a project whose own primary directory is
// gone has no roots worth copying. The members are already on their last known-good snapshot,
// and detaching must leave them there rather than point them at a path that does not exist.
func TestDeleteProjectWithDeadPrimaryKeepsTheSnapshot(t *testing.T) {
	snapshot := t.TempDir()
	sm := newSessionManagerForTest(t, snapshot)
	sid := sm.Current().ID
	dead := filepath.Join(t.TempDir(), "gone")

	d := writeProjectsFile(t, projectJSON(t, "p1", "dead", dead))
	d.sm = sm
	d.getRun(sid).sm = sm
	must(t, d.updateSessionMeta(sid, func(s *session.Session) { s.ProjectID = "p1" }), "bind")
	svc := &AgentService{desk: d}

	eq(t, "DeleteProject", svc.DeleteProject("p1"), "ok")
	rec := d.sessionRecord(sid)
	eq(t, "the snapshot is the last known-good workspace", rec.WorkingDir, snapshot)
	eq(t, "unbound", rec.ProjectID, "")
	if got := svc.ListProjects(); len(got) != 0 {
		t.Errorf("the project must be gone, got %+v", got)
	}
}
