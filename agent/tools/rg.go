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
