package shutil

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
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
