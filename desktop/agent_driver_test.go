package main

import (
	"strings"
	"testing"

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

// TestSystemPromptForWithoutSessionUsesProcessCwd covers sessions that never
// picked a folder: the advertised directory is the process cwd — the same root
// the tools (wdctx) and @-file references fall back to.
func TestSystemPromptForWithoutSessionUsesProcessCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	d := newTestApp()
	d.cfg = &config.Config{Language: "en"}

	if got, want := promptWorkingDir(d.systemPromptFor("unknown")), "- Working directory: "+dir; got != want {
		t.Errorf("expected the process cwd fallback %q, got %q", want, got)
	}

	// No config (bootstrap failed) → no prompt, like the simulated turn path.
	d.cfg = nil
	if got := d.systemPromptFor("unknown"); got != "" {
		t.Errorf("expected an empty prompt without config, got %q", got)
	}
}
