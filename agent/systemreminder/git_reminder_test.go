package systemreminder

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// TestGitReminderSwitchIsAReadNotARegistration: the reminder is registered whether or not the
// config wants it and asks Enabled per generation — that is what makes `system_reminder.
// git_reminder` hot without rebuilding the collector (a rebuild would cost every other
// reminder its runtime state). Two properties follow, and both are pinned here: off must not
// even run git, and on must produce exactly what the nil default produces.
func TestGitReminderSwitchIsAReadNotARegistration(t *testing.T) {
	consulted := false
	off := GitReminder{Enabled: func() bool { consulted = true; return false }}
	if got := off.Generate(t.Context(), Context{IsFirstMessage: true}); got != nil {
		t.Errorf("a disabled git reminder still generated %q", got)
	}
	if !consulted {
		t.Error("the switch was not consulted: the reminder is not reading the config")
	}

	dir := t.TempDir()
	gitRepo(t, dir)
	ctx := wdctx.WithDir(t.Context(), dir)

	explicit := GitReminder{Enabled: func() bool { return true }}.Generate(ctx, Context{IsFirstMessage: true})
	if len(explicit) == 0 {
		t.Fatal("no output in a real repository — the fixture is wrong, not the switch")
	}
	deflt := GitReminder{}.Generate(ctx, Context{IsFirstMessage: true})
	if strings.Join(explicit, "\n") != strings.Join(deflt, "\n") {
		t.Errorf("Enabled=true and no switch at all disagree:\n%v\n%v", explicit, deflt)
	}
	// And off is still off in a real repository — the branch line is exactly what must not leak.
	stillOff := GitReminder{Enabled: func() bool { return false }}.Generate(ctx, Context{IsFirstMessage: true})
	if stillOff != nil {
		t.Errorf("disabled reminder reported %q from a real repository", stillOff)
	}
}

// gitRepo makes dir a repository with one commit, so the reminder has a branch and a clean
// status to report. Skipped (not failed) when git is unavailable: the reminder is a thin layer
// over git, and the package's other tests do not need it.
func gitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	identity := []string{"-c", "user.email=test@example.invalid", "-c", "user.name=test"}
	steps := [][]string{
		{"init", "--quiet", "-b", "main", "."},
		append(identity, "commit", "--quiet", "--allow-empty", "-m", "init"),
	}
	for _, args := range steps {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v (%s)", args, err, out)
		}
	}
}
