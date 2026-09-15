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

// newProjectApp returns an app holding one project rooted at projectDir and ONE session
// that belongs to it. The session's own WorkingDir is deliberately a different directory
// (the snapshot it was created with), so a test can tell "resolved from the project" apart
// from "read the record".
//
// The project is created through the API — it is the only writer — and the binding is
// written through the session's own update path, so nothing here is reading a structure it
// also built by hand.
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

	must(t, d.updateSessionMeta(sid, func(s *session.Session) { s.ProjectID = pid }), "bind session")
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

// TestMemberSessionResolvesThroughItsProject is the core of the design: the session's own
// record is a stale snapshot and the PROJECT's roots win on every read, without anything
// writing to session meta.
func TestMemberSessionResolvesThroughItsProject(t *testing.T) {
	projectDir, movedDir, extra := t.TempDir(), t.TempDir(), t.TempDir()
	d, svc, sid, pid := newProjectApp(t, "p", projectDir)

	primary, _ := d.sessionRoots(sid)
	eq(t, "member primary", primary, projectDir)
	eq(t, "GetSessionWorkingDir", svc.GetSessionWorkingDir(sid), projectDir)

	snapshot := d.sessionRecord(sid).WorkingDir

	// Move the project: the next read follows, and nothing touched the session.
	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, movedDir, []string{extra}), "ok")
	primary, additional := d.sessionRoots(sid)
	eq(t, "primary after the move", primary, movedDir)
	eqSlices(t, "additional after the move", additional, []string{extra})
	eq(t, "session meta is a snapshot", d.sessionRecord(sid).WorkingDir, snapshot)

	// Every other consumer reads the same exit.
	r := d.getRun(sid)
	eq(t, "expansionRoot", d.expansionRoot(r), movedDir)
	ctx, cancel, _, _, ok := d.beginTurn(sid, r)
	if !ok {
		t.Fatal("beginTurn refused")
	}
	t.Cleanup(cancel)
	eq(t, "turn wdctx", wdctx.Dir(ctx), movedDir)
	if prompt := d.systemPromptFor(sid); !strings.Contains(prompt, movedDir) {
		t.Error("the prompt must name the project's directory")
	}
}

// TestProjectWriteGuards: while a project owns the workspace the three session-level
// writers refuse — in the BACKEND, not by hiding buttons — and nothing is persisted.
func TestProjectWriteGuards(t *testing.T) {
	projectDir := t.TempDir()
	d, svc, sid, _ := newProjectApp(t, "shared-lib", projectDir)
	snapshotDir := d.sessionRecord(sid).WorkingDir

	for name, call := range map[string]func() string{
		"SetSessionWorkingDir": func() string { return svc.SetSessionWorkingDir(sid, t.TempDir()) },
		"AddSessionRoots":      func() string { return svc.AddSessionRoots(sid, []string{t.TempDir()}) },
		"RemoveSessionRoot":    func() string { return svc.RemoveSessionRoot(sid, t.TempDir()) },
	} {
		if got := call(); !strings.Contains(got, "shared-lib") {
			t.Errorf("%s = %q, want a refusal naming the project", name, got)
		}
	}

	roots := svc.GetSessionRoots(sid)
	eq(t, "primary", roots.Primary, projectDir)
	if len(roots.Additional) != 0 {
		t.Errorf("a refused write was persisted: %+v", roots.Additional)
	}
	eq(t, "project name", roots.ProjectName, "shared-lib")
	if roots.ProjectMissing {
		t.Error("a usable project must not be reported as missing")
	}
	eq(t, "the snapshot is untouched", d.sessionRecord(sid).WorkingDir, snapshotDir)
}

// TestDanglingProjectDegradesToSnapshot: a project_id that no longer resolves (deleted, or
// projects.json lost) must not LOCK the session — roots fall back to the snapshot, the
// writers work again, and the panel is told what happened.
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
	if !roots.ProjectMissing {
		t.Error("the UI must be able to say the project is gone")
	}
	eq(t, "projectName", roots.ProjectName, "")

	eq(t, "editable again", svc.SetSessionWorkingDir(sid, t.TempDir()), "ok")
}

// TestProjectReadSideValidation: projects.json is a file anyone can edit, so an entry whose
// roots stopped validating must not reach the tools. It degrades exactly like a missing
// project — snapshot, editable — while staying nameable in the UI.
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
			if !roots.ProjectMissing {
				t.Error("an unusable project must be reported as not driving the session")
			}
			eq(t, "projectName", roots.ProjectName, "bad")
			eq(t, "editable again", svc.SetSessionWorkingDir(sid, t.TempDir()), "ok")
		})
	}
}

// TestProjectSkillStoreFollowsTheProject: skills are the one thing a project edit does NOT
// move by itself — a store's scan roots are fixed when it is built — so every live member
// has to be re-pointed. The edit only MARKS them (invalidateMemberSkills): it runs on the
// UI goroutine while members may be mid-turn, and the reload rewrites the store and the
// tool registry. Each member's own next turn applies it.
func TestProjectSkillStoreFollowsTheProject(t *testing.T) {
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

	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, treeB, nil), "ok")

	// The edit itself must not touch a live agent: the member can be mid-turn.
	if !slices.Contains(storeSkillNames(a.SkillStore()), "skill-a") {
		t.Error("a project edit must not rewrite a live agent's skill store")
	}
	if !r.skillsStale {
		t.Fatal("the member must be marked as needing a skill-store reload")
	}

	ctx, cancel, _, _, ok := d.beginTurn(sid, r)
	if !ok {
		t.Fatal("beginTurn refused")
	}
	t.Cleanup(cancel)
	eq(t, "turn wdctx", wdctx.Dir(ctx), treeB)

	names := storeSkillNames(a.SkillStore())
	if !slices.Contains(names, "skill-b") {
		t.Errorf("the store must follow the project, got %v", names)
	}
	if slices.Contains(names, "skill-a") {
		t.Errorf("the old tree's skills must be gone, got %v", names)
	}
	if r.skillsStale {
		t.Error("the reload must clear the flag")
	}
}

// TestCheckpointRootsFollowTheProject: the agent resolves its checkpoint roots through the
// same exit, so a turn inside a project snapshots the tree the tools actually wrote in
// rather than the snapshot in the session record. A project-less session keeps the agent's
// own default shape exactly — including the empty primary — so nobody else's snapshots
// change (the agent package pins the hook itself in TestCheckpointRootsUsesRootsFunc).
func TestCheckpointRootsFollowTheProject(t *testing.T) {
	projectDir, extra := t.TempDir(), t.TempDir()
	d, svc, sid, pid := newProjectApp(t, "p", projectDir)
	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, projectDir, []string{extra}), "ok")

	eqSlices(t, "member checkpoint roots", d.checkpointRootsFor(d.sessionRecord(sid)),
		[]string{projectDir, extra})

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

// TestDeadAdditionalRootDoesNotUndoTheProject: an additional root that disappeared (an
// unmounted volume) is a fact about that ONE directory. It is reported the way a plain
// session reports it — kept in the set, exists=false, skipped by the prompt and the
// @-file search — and must not take the whole project out of the picture: that would
// relocate every member session to its stale snapshot because a drive is unplugged.
func TestDeadAdditionalRootDoesNotUndoTheProject(t *testing.T) {
	projectDir, dead := t.TempDir(), filepath.Join(t.TempDir(), "unmounted")
	must(t, os.MkdirAll(dead, 0o755), "mkdir the extra root")

	d, svc, sid, pid := newProjectApp(t, "p", projectDir)
	eq(t, "SetProjectRoots", svc.SetProjectRoots(pid, projectDir, []string{dead}), "ok")

	must(t, os.RemoveAll(dead), "unmount the extra root")

	// The roots still resolve THROUGH THE PROJECT, not through the session's snapshot.
	primary, additional := d.sessionRoots(sid)
	eq(t, "primary", primary, projectDir)
	eqSlices(t, "additional", additional, []string{dead})

	projects := svc.ListProjects()
	if len(projects) != 1 || !projects[0].RootsUsable {
		t.Errorf("a dead ADDITIONAL root must not make the project unusable: %+v", projects)
	}

	roots := svc.GetSessionRoots(sid)
	if roots.ProjectMissing {
		t.Error("the project still drives this session")
	}
	if len(roots.Additional) != 1 || roots.Additional[0].Exists {
		t.Errorf("the dead root must be reported with exists=false: %+v", roots.Additional)
	}
	// Still a member, so the session-level writers still refuse.
	if res := svc.SetSessionWorkingDir(sid, t.TempDir()); !strings.Contains(res, "管理该会话的工作区") {
		t.Errorf("SetSessionWorkingDir = %q, want the project refusal", res)
	}
}
