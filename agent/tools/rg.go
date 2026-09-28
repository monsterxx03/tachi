package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func checkRipgrep() error {
	if _, err := exec.LookPath("rg"); err != nil {
		return fmt.Errorf("ripgrep (rg) not found in PATH: %w", err)
	}
	return nil
}

// resolveSearchPath turns the search path argument into an absolute path: the
// shared resolution rule (ResolvePath) plus the two things a search needs — an
// empty path means the working directory itself, and the result must be absolute
// for ripgrep.
func resolveSearchPath(ctx context.Context, path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(ResolvePath(ctx, path))
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}
	return abs, nil
}

func toRelativePath(absFilePath, basePath string) string {
	rel, err := filepath.Rel(basePath, absFilePath)
	if err != nil {
		return absFilePath
	}
	return filepath.ToSlash(rel)
}

// marshalResult serializes a tool's result struct — the text the MODEL reads.
//
// What belongs in one: what the model needs to act on, and nothing else. Two
// kinds of field have been removed from results already, and both looked
// harmless when they were added:
//
//   - **Timing.** A duration is for the human and the UI, and the UI does not
//     read it from here — the tool call's wall clock rides on the event and on
//     `session.Message.DurationMs`. `durationMs` was carried in the Bash, Glob,
//     WebSearch and WebFetch results for no reader at all.
//   - **An echo of what the caller already sent.** The tool_use arguments are
//     re-sent with every request, so a result that repeats them is a second copy
//     of the model's own text (see edit.go's generateDiffSnippet, which folds
//     exactly that).
//
// A result also lives in the transcript for the rest of the session, so a field
// that is merely tidy to emit is paid for on every later request until it
// compacts away.
func marshalResult(result any) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseArgs(args string, dest any) error {
	if args == "" {
		args = "{}"
	}
	if err := json.Unmarshal([]byte(args), dest); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func isRgNoMatch(err error) bool {
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return true
	}
	return false
}

func rgErrorMessage(err error) (string, bool) {
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
		return strings.TrimSpace(string(exitErr.Stderr)), true
	}
	return "", false
}
