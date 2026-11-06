package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/shutil"
	"github.com/monsterxx03/tachi/session"
)

// gitInit makes dir a repository with one commit, so a worktree of it has something to check out.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	steps := [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"add", "-A"},
		{"commit", "-q", "-m", "init"},
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("gitInit: write file: %v", err)
	}
	// `add -A` has to see the file, so it is written before the steps run.
	for _, args := range steps {
		if err := shutil.Run(ctx, dir, "git", args...); err != nil {
			t.Skipf("git is not usable here: %v", err)
		}
	}
}

// realPath resolves macOS's /var → /private/var so a git-reported path can be compared with a
// t.TempDir() one.
func realPath(t *testing.T, p string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return out
}

// TestCreateSessionWorktree: the session moves into its own checkout, the record says WHOSE it is,
// and the main checkout is untouched.
func TestCreateSessionWorktree(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	repo = realPath(t, repo)
	d, svc, sid := newAppWithSession(t, repo)

	vo := svc.GetWorktreeBranches(sid)
	if vo.Err != "" {
		t.Fatalf("GetWorktreeBranches: %s", vo.Err)
	}
	eq(t, "repo", vo.Repo, repo)
	eq(t, "current branch", vo.Current, "main")
	eq(t, "main is checked out in the repo", vo.CheckedOut["main"], repo)
	if vo.SuggestedBranch == "" || vo.SuggestedDir == "" {
		t.Errorf("the form must be prefilled: %+v", vo)
	}

	dir := filepath.Join(t.TempDir(), "checkout")
	eq(t, "create", svc.CreateSessionWorktree(sid, "main", "tachi/test", dir), "ok")

	rec := d.sessionRecord(sid)
	eq(t, "the session moved", rec.WorkingDir, dir)
	eq(t, "worktree repo", rec.WorktreeOf, repo)
	eq(t, "worktree branch", rec.WorktreeBranch, "tachi/test")
	if !fileutil.Exists(filepath.Join(dir, "a.txt")) {
		t.Error("the checkout must come from the base branch")
	}
	// The main checkout is still on main: the worktree is an addition, never a move of the repo.
	if branch, _, _ := shutil.GitBranch(context.Background(), repo); branch != "main" {
		t.Errorf("the main checkout moved to %q", branch)
	}
	// And the branch really exists now.
	branches, _ := shutil.GitLocalBranches(context.Background(), repo)
	if !contains(branches, "tachi/test") {
		t.Errorf("branches = %v, want tachi/test", branches)
	}

	// One worktree per conversation: a second one is refused by name.
	if got := svc.CreateSessionWorktree(sid, "main", "tachi/other", filepath.Join(t.TempDir(), "x")); !strings.Contains(got, "已经在 worktree") {
		t.Errorf("second worktree = %q, want the refusal", got)
	}
}

// TestWorktreeBranchRefusals: the two picks that cannot work say why, before anything is created.
func TestWorktreeBranchRefusals(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	_, svc, sid := newAppWithSession(t, repo)

	// Checking out a branch that the main checkout holds: git would refuse, so we name the holder.
	if got := svc.CreateSessionWorktree(sid, "main", "", filepath.Join(t.TempDir(), "a")); !strings.Contains(got, "已经被") {
		t.Errorf("checked-out branch = %q, want it to name the holder", got)
	}
	// A directory that already exists is refused rather than reused.
	if got := svc.CreateSessionWorktree(sid, "main", "tachi/new", repo); !strings.Contains(got, "已经存在") {
		t.Errorf("existing directory = %q, want the refusal", got)
	}
	// A branch name git would not accept.
	if got := svc.CreateSessionWorktree(sid, "main", "bad branch", filepath.Join(t.TempDir(), "b")); !strings.Contains(got, "不合法") {
		t.Errorf("invalid branch = %q, want the refusal", got)
	}
	// Nothing was created and the session did not move.
	if rec := sessionRecordFor(t, svc, sid); rec.WorkingDir != repo {
		t.Errorf("the session moved on a refusal: %s", rec.WorkingDir)
	}
}

// TestDeleteSessionWithWorktree walks the three outcomes: the flag off keeps the directory, a dirty
// checkout refuses the whole delete, and a clean one removes the directory.
func TestDeleteSessionWithWorktree(t *testing.T) {
	t.Run("flag off keeps the directory", func(t *testing.T) {
		repo := t.TempDir()
		gitInit(t, repo)
		repo = realPath(t, repo)
		_, svc, sid := newAppWithSession(t, repo)
		dir := filepath.Join(t.TempDir(), "checkout")
		eq(t, "create", svc.CreateSessionWorktree(sid, "main", "tachi/keep", dir), "ok")

		eq(t, "delete", svc.DeleteSession(sid, false), "ok")
		if !fileutil.IsDir(dir) {
			t.Error("a delete that did not ask for the worktree must leave it alone")
		}
	})

	t.Run("dirty checkout refuses", func(t *testing.T) {
		repo := t.TempDir()
		gitInit(t, repo)
		repo = realPath(t, repo)
		d, svc, sid := newAppWithSession(t, repo)
		dir := filepath.Join(t.TempDir(), "checkout")
		eq(t, "create", svc.CreateSessionWorktree(sid, "main", "tachi/dirty", dir), "ok")
		if err := os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("work\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		got := svc.DeleteSession(sid, true)
		if !strings.Contains(got, "未提交") {
			t.Errorf("delete with a dirty worktree = %q, want a refusal naming the changes", got)
		}
		// Nothing was deleted: not the directory, not the session.
		if !fileutil.IsDir(dir) {
			t.Error("the checkout must survive a refused delete")
		}
		if d.sessionRecord(sid) == nil {
			t.Error("the session must survive a refused delete")
		}
	})

	t.Run("clean checkout is removed, branch kept", func(t *testing.T) {
		repo := t.TempDir()
		gitInit(t, repo)
		repo = realPath(t, repo)
		d, svc, sid := newAppWithSession(t, repo)
		dir := filepath.Join(t.TempDir(), "checkout")
		eq(t, "create", svc.CreateSessionWorktree(sid, "main", "tachi/gone", dir), "ok")

		eq(t, "delete", svc.DeleteSession(sid, true), "ok")
		if fileutil.Exists(dir) {
			t.Error("the checkout must be gone")
		}
		if d.sessionRecord(sid) != nil {
			t.Error("the session must be gone")
		}
		if trees, ok := shutil.GitWorktrees(context.Background(), repo); ok {
			for _, tr := range trees {
				if tr.Path == dir {
					t.Error("git still lists the removed worktree")
				}
			}
		}
		// The branch survives: deleting a conversation must never delete commits.
		branches, _ := shutil.GitLocalBranches(context.Background(), repo)
		if !contains(branches, "tachi/gone") {
			t.Errorf("branches = %v, want the branch kept", branches)
		}
	})

	t.Run("checkout that is already gone only prunes", func(t *testing.T) {
		repo := t.TempDir()
		gitInit(t, repo)
		repo = realPath(t, repo)
		_, svc, sid := newAppWithSession(t, repo)
		dir := filepath.Join(t.TempDir(), "checkout")
		eq(t, "create", svc.CreateSessionWorktree(sid, "main", "tachi/vanish", dir), "ok")
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("rm: %v", err)
		}
		eq(t, "delete", svc.DeleteSession(sid, true), "ok")
	})
}

// TestMigrateProjectWorkspaces: projects.json without a version describes sessions whose workspace
// lived on the project. The migration gives each member the project's CURRENT roots once, stamps the
// file, and then never runs again — so a later project edit cannot reach those sessions.
func TestMigrateProjectWorkspaces(t *testing.T) {
	projectDir, movedDir := t.TempDir(), t.TempDir()
	snapshot := t.TempDir()
	sm := newSessionManagerForTest(t, snapshot)
	sid := sm.Current().ID

	d := writeProjectsFile(t, projectJSON(t, "p1", "legacy", projectDir))
	d.sm = sm
	d.getRun(sid).sm = sm
	// A v1 member: bound to the project, snapshot only (normally stale by construction).
	must(t, d.updateSessionMeta(sid, func(s *session.Session) { s.ProjectID = "p1" }), "bind")

	d.migrateProjectWorkspaces()

	rec := d.sessionRecord(sid)
	eq(t, "the member got the project's roots", rec.WorkingDir, projectDir)
	eq(t, "binding kept", rec.ProjectID, "p1")
	if d.projects.needsWorkspaceMigration() {
		t.Fatal("the migration must stamp projects.json")
	}
	// The version is on disk, so the next process does not migrate again.
	fresh := newTestApp()
	if fresh.projects.needsWorkspaceMigration() {
		t.Error("a fresh read must see the stamped version")
	}

	// From here on, a project edit moves nobody.
	svc := &AgentService{desk: d}
	eq(t, "SetProjectRoots", svc.SetProjectRoots("p1", movedDir, nil), "ok")
	eq(t, "the migrated session stays", d.sessionRecord(sid).WorkingDir, projectDir)
	d.migrateProjectWorkspaces()
	eq(t, "re-running the migration changes nothing", d.sessionRecord(sid).WorkingDir, projectDir)
}

// TestMigrationSkipsUnusableProjects: a project whose primary is gone has no roots worth copying —
// the migration leaves its members on the last known-good workspace and still stamps the file, so a
// directory that comes back later does not trigger a second, surprising move.
func TestMigrationSkipsUnusableProjects(t *testing.T) {
	snapshot := t.TempDir()
	sm := newSessionManagerForTest(t, snapshot)
	sid := sm.Current().ID
	dead := filepath.Join(t.TempDir(), "gone")

	d := writeProjectsFile(t, projectJSON(t, "p1", "dead", dead))
	d.sm = sm
	d.getRun(sid).sm = sm
	must(t, d.updateSessionMeta(sid, func(s *session.Session) { s.ProjectID = "p1" }), "bind")

	d.migrateProjectWorkspaces()
	eq(t, "the session keeps its workspace", d.sessionRecord(sid).WorkingDir, snapshot)
	if d.projects.needsWorkspaceMigration() {
		t.Error("an unusable project must not keep the migration pending forever")
	}
	// The base dir the migration must have written into is the test's, never the user's.
	if b := config.BaseDir(); !strings.HasPrefix(filepath.Join(b, ""), os.TempDir()) {
		t.Errorf("config.BaseDir leaked outside the temp dir: %s", b)
	}
}

// sessionRecordFor is the record through the service, for tests that only hold the AgentService.
func sessionRecordFor(t *testing.T, svc *AgentService, id string) *session.Session {
	t.Helper()
	rec := svc.desk.sessionRecord(id)
	if rec == nil {
		t.Fatalf("no session %s", id)
	}
	return rec
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
