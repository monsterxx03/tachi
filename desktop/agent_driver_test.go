package main

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
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
