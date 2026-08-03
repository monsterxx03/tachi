// Package shutil provides small helpers for running external commands
// (git, systemctl, …) with uniform error context. The three functions are
// thin wrappers over os/exec that normalize the most common patterns:
// capture-and-trim stdout, capture stderr for error messages, and probe
// whether a command exits successfully. Each takes a working directory
// ("" = the caller's current directory) so commands can be run against a
// specific repo or checkout without chdir.
package shutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// maxErrOutput caps stderr captured into error messages so a noisy command
// cannot blow up a log line (counted in runes so multi-byte characters are
// never split mid-sequence).
const maxErrOutput = 4 * 1024

// Output runs name with args in dir and returns stdout with surrounding
// whitespace trimmed. On error the returned error includes trimmed stderr
// for context (capped at maxErrOutput bytes).
func Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w (stderr: %s)", err, clip(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Run runs name with args in dir, discarding output. On error the returned
// error includes trimmed stderr for context (capped at maxErrOutput bytes).
func Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, clip(stderr.String()))
	}
	return nil
}

// Success reports whether the command exits with status 0 (output discarded).
// Used for cheap capability probes like "is this a git repo".
func Success(ctx context.Context, dir, name string, args ...string) bool {
	return Run(ctx, dir, name, args...) == nil
}

// clip trims s and caps it at maxErrOutput runes for embedding in errors.
// Truncate keeps multi-byte characters intact (never splits mid-sequence).
func clip(s string) string {
	s = strings.TrimSpace(s)
	return strutil.Truncate(s, maxErrOutput)
}
