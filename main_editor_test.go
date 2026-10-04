package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEditor writes an executable script that records its arguments (one per
// line) into the file named by $EDITOR_RECORD.
func fakeEditor(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-editor")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$EDITOR_RECORD\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLaunchEditorPassesEditorArgsAndPathIntact(t *testing.T) {
	editor := fakeEditor(t)
	record := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("EDITOR_RECORD", record)
	// $EDITOR may carry arguments; the path may contain spaces.
	t.Setenv("EDITOR", editor+" --inline")

	target := filepath.Join(t.TempDir(), "config with spaces.yaml")
	if err := launchEditor(target); err != nil {
		t.Fatalf("launchEditor: %v", err)
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("editor was not run: %v", err)
	}
	args := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	want := []string{"--inline", target}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Fatalf("editor args = %q, want %q", args, want)
	}
}

func TestLaunchEditorNoEditorFound(t *testing.T) {
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", t.TempDir())
	err := launchEditor("/tmp/whatever.yaml")
	if err == nil || !strings.Contains(err.Error(), "no editor found") {
		t.Fatalf("err = %v, want no-editor error", err)
	}
}
