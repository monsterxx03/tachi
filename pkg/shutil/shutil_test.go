package shutil

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutputTrimmed(t *testing.T) {
	out, err := Output(context.Background(), "", "sh", "-c", "printf '  hello world  \\n'")
	if err != nil {
		t.Fatalf("Output failed: %v", err)
	}
	if out != "hello world" {
		t.Errorf("Output = %q, want %q", out, "hello world")
	}
}

func TestOutputErrorIncludesStderr(t *testing.T) {
	_, err := Output(context.Background(), "", "sh", "-c", "echo boom >&2; exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q missing stderr context", err)
	}
}

func TestRunDiscardsOutput(t *testing.T) {
	if err := Run(context.Background(), "", "sh", "-c", "echo noise; exit 0"); err != nil {
		t.Errorf("Run failed: %v", err)
	}
	err := Run(context.Background(), "", "sh", "-c", "exit 3")
	if err == nil {
		t.Fatal("expected error for exit 3")
	}
}

func TestSuccess(t *testing.T) {
	if !Success(context.Background(), "", "true") {
		t.Error("true should succeed")
	}
	if Success(context.Background(), "", "false") {
		t.Error("false should not succeed")
	}
	if Success(context.Background(), "", "no-such-binary-xyz") {
		t.Error("missing binary should not succeed")
	}
}

func TestDirIsUsed(t *testing.T) {
	// Run in a directory that has no "git" wrapper; verify cmd.Dir is honored
	// by comparing the child's pwd against the requested directory. Both sides
	// are symlink-resolved (macOS resolves /var → /private/var in pwd).
	dir := t.TempDir()
	out, err := Output(context.Background(), dir, "sh", "-c", "pwd")
	if err != nil {
		t.Fatalf("Output failed: %v", err)
	}
	got, err := filepath.EvalSymlinks(out)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", out, err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	if got != want {
		t.Errorf("pwd = %q, want %q (cmd.Dir not honored)", got, want)
	}
}

func TestStderrCapped(t *testing.T) {
	big := strings.Repeat("x", maxErrOutput*2)
	_, err := Output(context.Background(), "", "sh", "-c", "printf %s '"+big+"' >&2; exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > maxErrOutput+256 {
		t.Errorf("error too long: %d bytes", len(err.Error()))
	}
}

func TestShellCapturesCombinedOutputAndExitCode(t *testing.T) {
	out, code, err := Shell(context.Background(), "", "echo out; echo err >&2; exit 7")
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("err = %v, want exit status 7", err)
	}
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("output %q missing stdout/stderr", out)
	}
}

func TestShellRunsInDir(t *testing.T) {
	dir := t.TempDir()
	out, code, err := Shell(context.Background(), dir, "pwd")
	if err != nil || code != 0 {
		t.Fatalf("Shell failed: %v (code %d)", err, code)
	}
	if out != dir {
		t.Errorf("pwd = %q, want %q", out, dir)
	}
}

func TestShellTimeoutKillsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, _ = Shell(ctx, "", "sleep 5")
	if ctx.Err() == nil {
		t.Fatal("expected context deadline exceeded")
	}
}

func TestShellOutputCapped(t *testing.T) {
	out, _, err := Shell(context.Background(), "", "seq 1 100000")
	if err != nil {
		t.Fatalf("Shell failed: %v", err)
	}
	if len(out) > maxShellOutput+16 {
		t.Errorf("output len %d exceeds cap %d", len(out), maxShellOutput)
	}
}

func TestFormatShellResult(t *testing.T) {
	got := FormatShellResult("ls", "file1\nfile2", 0, false)
	want := "$ ls\n```\nfile1\nfile2\n```\n"
	if got != want {
		t.Errorf("FormatShellResult = %q, want %q", got, want)
	}

	got = FormatShellResult("false", "", 1, false)
	want = "$ false\n(exit 1)"
	if got != want {
		t.Errorf("FormatShellResult = %q, want %q", got, want)
	}

	got = FormatShellResult("sleep", "", 0, true)
	want = "$ sleep\n⏱️ timed out, process killed"
	if got != want {
		t.Errorf("FormatShellResult = %q, want %q", got, want)
	}

	got = FormatShellResult("true", "", 0, false)
	want = "$ true\n(no output)"
	if got != want {
		t.Errorf("FormatShellResult = %q, want %q", got, want)
	}
}
