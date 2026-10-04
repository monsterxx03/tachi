package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// withBaseDir points the process-global config base at dir for one test and restores it after.
// SetBaseDir is not test-scoped, and t.TempDir() is deleted at cleanup, so a leaked value would
// leave the next test writing into a path that no longer exists.
func withBaseDir(t *testing.T, dir string) {
	t.Helper()
	old := config.BaseDir()
	config.SetBaseDir(dir)
	t.Cleanup(func() { config.SetBaseDir(old) })
}

// installedAskpass installs the helper through the code path under test and then asserts it is
// there and runnable.
func installedAskpass(t *testing.T) string {
	t.Helper()
	path := askpassHelper()
	if path == "" {
		t.Fatalf("askpassHelper() returned no path (base dir %s)", config.BaseDir())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("helper not installed at %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v) — sudo would refuse it", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != sudoAskpassScript {
		t.Error("installed helper differs from the embedded master copy")
	}
	return path
}

// The helper is the whole reason sudo can ask for a password at all, so it has to land
// executable, and it has to be REPLACED when Tachi ships a different one — otherwise a machine
// keeps running the first version it ever installed.
func TestAskpassHelperInstallsExecutableAndRefreshes(t *testing.T) {
	withBaseDir(t, t.TempDir())

	path := installedAskpass(t)

	// A second call is a no-op rather than a rewrite (it compares the content).
	if again := askpassHelper(); again != path {
		t.Errorf("askpassHelper() = %q on the second call, want %q", again, path)
	}

	// An older helper on disk (an upgrade) is replaced.
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatalf("clobber helper: %v", err)
	}
	if got := askpassHelper(); got != path {
		t.Fatalf("askpassHelper() = %q after a clobbered helper, want %q", got, path)
	}
	installedAskpass(t)
}

// Nothing everywhere: a TACHI_HOME that cannot be written (a read-only home, a path that is a
// file) must leave the command's environment untouched rather than fail the command.
func TestAskpassHelperUnwritableBaseDir(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("prepare blocker: %v", err)
	}
	withBaseDir(t, blocked)
	// Cleared explicitly: the ambient environment may already carry one (a developer whose
	// session sets SUDO_ASKPASS would otherwise see it here and read it as this code's doing).
	t.Setenv("SUDO_ASKPASS", "")

	if path := askpassHelper(); path != "" {
		t.Errorf("askpassHelper() = %q with an unwritable base dir, want \"\"", path)
	}
	// The assertion is about what THIS code names, not about the ambient entry: an inherited
	// empty SUDO_ASKPASS= stays in the environment (it is not ours to remove), and it must not
	// hide a helper we could not install.
	for _, kv := range bashEnv() {
		if value, ok := strings.CutPrefix(kv, "SUDO_ASKPASS="); ok && value != "" {
			t.Errorf("bashEnv() named an askpass helper it could not install: %s", kv)
		}
	}
}

// A SUDO_ASKPASS that is already there wins: the session may have set one on purpose (a
// dotfile, a desktop-wide dialog), and Tachi's default must not silently outrank it.
func TestBashEnvKeepsAnEnvironmentProvidedAskpass(t *testing.T) {
	withBaseDir(t, t.TempDir())
	t.Setenv("SUDO_ASKPASS", "/somewhere/else/askpass")

	named := ""
	for _, kv := range bashEnv() {
		if strings.HasPrefix(kv, "SUDO_ASKPASS=") {
			named = strings.TrimPrefix(kv, "SUDO_ASKPASS=")
		}
	}
	if named != "/somewhere/else/askpass" {
		t.Errorf("SUDO_ASKPASS = %q, want the environment's own value", named)
	}
}

// The point of all of it: a command the agent runs sees a working helper in its environment.
func TestBashCommandSeesTheAskpassHelper(t *testing.T) {
	withBaseDir(t, t.TempDir())
	t.Setenv("SUDO_ASKPASS", "")
	want := installedAskpass(t)

	tool := NewBashTool(BashToolConfig{})
	result, err := tool.ExecuteContext(context.TODO(), `{"command": "test -x \"$SUDO_ASKPASS\" && echo \"$SUDO_ASKPASS\""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r BashResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if got := strings.TrimSpace(r.Stdout); got != want {
		t.Errorf("command saw SUDO_ASKPASS = %q (exit %d), want %q", got, r.ExitCode, want)
	}
}

// The helper has to fail LOUDLY when it has no dialog to draw (a machine without pinentry):
// printing an empty line would hand sudo an empty password and look like a wrong password,
// while a non-zero exit reads as "cancelled", which is the truth.
func TestAskpassScriptWithoutADialogFailsLoudly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the script's dialog branch is Linux-only")
	}
	withBaseDir(t, t.TempDir())
	path := installedAskpass(t)

	cmd := exec.Command("sh", path, "[sudo] password for someone: ")
	cmd.Env = append(os.Environ(), "PINENTRY=/nonexistent-pinentry")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper exited 0 with no dialog available; output: %q", out)
	}
	if !strings.Contains(string(out), "pinentry") {
		t.Errorf("helper's failure says nothing about pinentry: %q", out)
	}
}

// A cancelled dialog (or a front end that dies) must exit non-zero with NOTHING on stdout:
// sudo reads an empty password as a wrong one and asks twice more, while a non-zero exit means
// "no password was provided" — the truth, and one attempt instead of three.
func TestAskpassScriptStaysSilentWhenCancelled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the script's dialog branch is Linux-only")
	}
	withBaseDir(t, t.TempDir())
	path := installedAskpass(t)

	// A pinentry stand-in that answers like a cancelled dialog: an ERR line, no "D " line.
	stub := filepath.Join(t.TempDir(), "pinentry-fake")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat >/dev/null\necho 'ERR 83886179 cancelled'\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	cmd := exec.Command("sh", path, "Password:")
	cmd.Env = append(os.Environ(), "PINENTRY="+stub)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper exited 0 on a cancelled dialog; output: %q", out)
	}
	if len(out) != 0 {
		t.Errorf("helper printed %q on a cancelled dialog, want nothing", out)
	}
}

// An empty ambient SUDO_ASKPASS (what a wrapper or a conditional export leaves behind) must be
// REPLACED, not appended to: two entries for one variable are read differently by the two sides —
// sudo is a C program and glibc's getenv takes the first — so a duplicate could hand sudo the
// empty one and no dialog would ever appear.
func TestBashEnvReplacesAnEmptyAskpass(t *testing.T) {
	withBaseDir(t, t.TempDir())
	t.Setenv("SUDO_ASKPASS", "")
	want := installedAskpass(t)

	var found []string
	for _, kv := range bashEnv() {
		if strings.HasPrefix(kv, "SUDO_ASKPASS=") {
			found = append(found, kv)
		}
	}
	if len(found) != 1 {
		t.Fatalf("bashEnv() carries %d SUDO_ASKPASS entries (%v), want exactly one", len(found), found)
	}
	if found[0] != "SUDO_ASKPASS="+want {
		t.Errorf("bashEnv() = %q, want ours (%q)", found[0], want)
	}
}
