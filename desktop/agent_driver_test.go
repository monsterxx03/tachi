package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// promptWorkingDir extracts the Working directory line of a system prompt ("" if
// absent) so failure messages stay readable.
func promptWorkingDir(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "- Working directory:") {
			return line
		}
	}
	return ""
}

// TestSystemPromptForFollowsSessionWorkingDir pins the desktop's per-session
// prompt: the Working directory line must come from the session's CURRENT
// directory, and picking another folder must take effect on the next build with
// no invalidation step (the cache key is the directory itself). A GUI process
// hosts several sessions and its cwd is meaningless, so a startup-built prompt
// would advertise the wrong tree while the tools used the right one.
func TestSystemPromptForFollowsSessionWorkingDir(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	sm := newSessionManagerForTest(t, dirA)
	sid := sm.Current().ID

	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}
	d.sm = sm
	r := d.getRun(sid)
	r.sm = sm

	wantLine := func(dir string) string { return "- Working directory: " + dir }
	if got := promptWorkingDir(d.systemPromptFor(sid)); got != wantLine(dirA) {
		t.Errorf("expected the session's working dir, got %q", got)
	}
	// The prompt carries the session identity it was built for.
	if !strings.Contains(d.systemPromptFor(sid), "- Session ID: "+sid) {
		t.Errorf("expected the session ID in the prompt, got %q", d.systemPromptFor(sid))
	}

	// An unchanged session is served from the memo (same text, no rebuild).
	if first, second := d.systemPromptFor(sid), d.systemPromptFor(sid); first != second {
		t.Error("expected an unchanged session to reuse its cached prompt")
	}

	// Switching the folder through the real user path: the next build must
	// follow, purely because it is a new cache key.
	if res := (&AgentService{desk: d}).SetSessionWorkingDir(sid, dirB); res != "ok" {
		t.Fatalf("SetSessionWorkingDir: %s", res)
	}
	if got := promptWorkingDir(d.systemPromptFor(sid)); got != wantLine(dirB) {
		t.Errorf("expected the prompt to follow the new working dir, got %q", got)
	}

	// One entry per (directory, session) pair — the old directory's prompt stays
	// cached under its own key rather than being invalidated.
	if n := len(d.promptCache); n != 2 {
		t.Errorf("expected one cache entry per (dir, session) pair, got %d", n)
	}
}

// TestSystemPromptForWithoutWorkspace covers sessions that never picked a folder:
// the prompt SAYS SO instead of substituting the process cwd. For a
// Finder-launched GUI app that cwd is "/", so the substitution advertised the
// filesystem root as the workspace and invited absolute paths there — the original
// "Working directory: /" bug. The tools still fall back to the process cwd (wdctx),
// which is why the composer asks the user to pick a directory rather than leaving
// the session in this state.
func TestSystemPromptForWithoutWorkspace(t *testing.T) {
	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}

	want := "- Working directory: (not set yet — ask the user which directory to work in before using relative paths)"
	if got := promptWorkingDir(d.systemPromptFor("unknown")); got != want {
		t.Errorf("expected an explicit unset line\n got %q\nwant %q", got, want)
	}

	// No config (bootstrap failed) → no prompt, like the simulated turn path.
	d.cfg = nil
	if got := d.systemPromptFor("unknown"); got != "" {
		t.Errorf("expected an empty prompt without config, got %q", got)
	}
}

// TestSystemPromptForPlanMode pins the P2 prompt rule: plan mode appends the plan-mode
// rules to the turn's prompt, and — because the prompt is memoized — the mode has to be
// part of the cache key. Without that, the second call below would hand a plan-mode turn
// the CACHED auto-mode prompt, i.e. it would be told it may edit files right after being
// put on a leash. (Same failure shape as the working-directory key this test file already
// covers.)
func TestSystemPromptForPlanMode(t *testing.T) {
	dir := t.TempDir()
	d, _, sid := newRootsApp(t, dir)

	auto := d.systemPromptFor(sid)
	if strings.Contains(auto, "Plan Mode (ACTIVE)") {
		t.Errorf("auto mode must not carry the plan-mode rules")
	}

	// Persist plan mode the way AIAgent.SetMode does (session meta), which is also how a
	// session left in plan mode by an editor arrives here.
	cur := d.getRun(sid).sm.Current()
	cur.Mode = agent.ModePlan
	if err := d.getRun(sid).sm.UpdateMeta(cur); err != nil {
		t.Fatalf("update meta: %v", err)
	}

	plan := d.systemPromptFor(sid)
	if !strings.Contains(plan, "Plan Mode (ACTIVE)") {
		t.Fatalf("the plan-mode turn's prompt is missing the plan-mode rules (mode is not reaching the builder)")
	}
	if plan == auto {
		t.Fatal("the prompt memo returned the auto-mode prompt for a plan-mode turn: mode must be part of the cache key")
	}
	if got := d.sessionMode(sid); got != agent.ModePlan {
		t.Errorf("sessionMode() = %q, want %q", got, agent.ModePlan)
	}

	// Switching back must be just as immediate.
	cur = d.getRun(sid).sm.Current()
	cur.Mode = agent.ModeAuto
	if err := d.getRun(sid).sm.UpdateMeta(cur); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	if back := d.systemPromptFor(sid); back != auto {
		t.Errorf("switching back to auto should return the auto prompt again")
	}
}

// writeSkillFixture drops one valid SKILL.md at dir/<name>/SKILL.md. The frontmatter
// name is what the store validates and the description is what the catalog reminder
// shows, so both are real rather than placeholders.
func writeSkillFixture(t *testing.T, dir, name, description string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	md := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nbody of " + name + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// writeProjectSkillFixture is writeSkillFixture for a workspace tree, i.e. at
// <tree>/.tachi/skills/<name> — the directory shape a project's own skills live in.
// (The GLOBAL scope is <base>/skills instead; see config.GlobalSkillsDir.)
func writeProjectSkillFixture(t *testing.T, tree, name, description string) {
	t.Helper()
	writeSkillFixture(t, filepath.Join(tree, ".tachi", "skills"), name, description)
}

// storeSkillNames lists the names a skill store currently offers.
func storeSkillNames(store *skill.Store) []string {
	var names []string
	for _, m := range store.List() {
		names = append(names, m.Name)
	}
	return names
}

// TestSessionSkillStoreFollowsTheSessionTree pins WHICH tree a session's skills come
// from. A store's scan roots are fixed when it is built (skill.Store) and the desktop
// process cwd is meaningless — macOS hands a Finder-launched app "/" — so the desktop
// builds it from the session's working directory. A cwd-built store would scan
// "/.tachi/skills" and offer no project skills at all, which is why DisableSkills is
// not simply flipped on.
func TestSessionSkillStoreFollowsTheSessionTree(t *testing.T) {
	treeA, treeB := t.TempDir(), t.TempDir()
	writeProjectSkillFixture(t, treeA, "skill-a", "from tree A")
	writeProjectSkillFixture(t, treeB, "skill-b", "from tree B")

	sm := newSessionManagerForTest(t, treeA)
	// A global skill too, so the layering is pinned: always in scope, and behind the
	// session's own tree (which is what lets a project shadow it).
	writeSkillFixture(t, config.GlobalSkillsDir(), "global-skill", "from the global scope")

	store := newTestApp().sessionSkillStore(sm)
	names := storeSkillNames(store)

	if !slices.Contains(names, "skill-a") {
		t.Errorf("the session's own tree must be scanned, got %v", names)
	}
	if !slices.Contains(names, "global-skill") {
		t.Errorf("global skills are always in scope, got %v", names)
	}
	if slices.Contains(names, "skill-b") {
		t.Errorf("another session's tree must not leak in, got %v", names)
	}
	if want := filepath.Join(treeA, ".tachi", "skills"); store.Dirs()[0] != want {
		t.Errorf("project skills must be scanned first (to shadow global ones), got %q, want %q",
			store.Dirs()[0], want)
	}
}

// TestSessionSkillStoreWithoutWorkspace covers a session that has not picked a folder:
// global skills alone, and never the process cwd (skill.NewStore("")). Without an
// existing session the same rule applies.
func TestSessionSkillStoreWithoutWorkspace(t *testing.T) {
	sm := newSessionManagerForTest(t, "")
	sm.EndCurrent()
	d := newTestApp()

	want := []string{config.GlobalSkillsDir()}
	if got := d.sessionSkillStore(sm).Dirs(); !slices.Equal(got, want) {
		t.Errorf("an empty working directory must mean the global scope only\ngot  %v\nwant %v", got, want)
	}
	if got := d.sessionSkillStore(nil).Dirs(); !slices.Equal(got, want) {
		t.Errorf("a nil session manager must not fall back to the process cwd\ngot  %v\nwant %v", got, want)
	}
}

// TestSetSessionWorkingDirRepointsSkills covers the other half of "skills follow the
// session": scan roots are fixed when the store is built, so a session that MOVES has
// to be re-pointed. Without it the session keeps serving the OLD tree's project skills
// — and sends Skill create's "project" target there — until it is reloaded.
func TestSetSessionWorkingDirRepointsSkills(t *testing.T) {
	treeA, treeB := t.TempDir(), t.TempDir()
	writeProjectSkillFixture(t, treeA, "skill-a", "from tree A")
	writeProjectSkillFixture(t, treeB, "skill-b", "from tree B")

	d, svc, sid := newRootsApp(t, treeA)

	// The state buildAgentForSession leaves behind: an agent whose store is rooted at
	// the session's tree. A bare agent suffices — reload only needs the registry.
	a := &agent.AIAgent{Config: agent.AgentConfig{
		ToolRegistry: tools.NewRegistry(),
		Logger:       logger.Default(),
	}}
	a.ReloadSkillsIn(treeA)
	if !slices.Contains(storeSkillNames(a.SkillStore()), "skill-a") {
		t.Fatalf("fixture: the store did not start on tree A")
	}
	d.getRun(sid).agent = a

	if res := svc.SetSessionWorkingDir(sid, treeB); res != "ok" {
		t.Fatalf("SetSessionWorkingDir: %s", res)
	}

	names := storeSkillNames(a.SkillStore())
	if !slices.Contains(names, "skill-b") {
		t.Errorf("the store must follow the session to its new tree, got %v", names)
	}
	if slices.Contains(names, "skill-a") {
		t.Errorf("the old tree's skills must be gone, got %v", names)
	}
}

// TestSetSessionWorkingDirWithoutAgentIsSafe pins the desktop's one-off path: a session
// whose agent has not been built yet (or whose bootstrap failed and left it agent-less)
// must still change folder — the agent is built later, from the persisted directory.
func TestSetSessionWorkingDirWithoutAgentIsSafe(t *testing.T) {
	d, svc, sid := newRootsApp(t, t.TempDir())
	_ = d

	if res := svc.SetSessionWorkingDir(sid, t.TempDir()); res != "ok" {
		t.Fatalf("SetSessionWorkingDir without an agent: %s", res)
	}
}
