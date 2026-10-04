package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// editorFallbacks are tried in order when $EDITOR is unset.
var editorFallbacks = []string{"nvim", "vim", "vi", "nano", "code"}

// launchEditor opens path in the user's editor ($EDITOR, else the first
// available fallback) and waits for it to exit.
//
// $EDITOR is allowed to carry arguments ("code --wait",
// "omarchy-launch-editor --inline"), so it is a shell snippet rather than a
// program name: running it through sh keeps that contract the same as git's,
// and the path is handed over as a positional argument so a path containing
// spaces needs no quoting by whoever set $EDITOR.
func launchEditor(path string) error {
	editor := strings.TrimSpace(os.Getenv("EDITOR"))
	if editor == "" {
		for _, candidate := range editorFallbacks {
			if p, err := exec.LookPath(candidate); err == nil {
				editor = p
				break
			}
		}
	}
	if editor == "" {
		return fmt.Errorf("no editor found; set $EDITOR or install one of: %s", strings.Join(editorFallbacks, ", "))
	}

	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor failed: %w", err)
	}
	return nil
}
