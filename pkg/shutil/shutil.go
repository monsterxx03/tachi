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
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

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

// --- Raw shell execution (/sh slash command) ---

// DefaultShellTimeout bounds /sh slash-command execution so a runaway
// command cannot pin a thread's handler (or the TUI) indefinitely.
const DefaultShellTimeout = 60 * time.Second

// maxShellOutput caps the combined output returned by Shell (in runes),
// keeping the echoed reply within IM message size limits.
const maxShellOutput = 4 * 1024

// Shell runs command via "sh -c" in dir ("" = the process's current
// directory) and returns its combined stdout+stderr (trimmed, capped at
// maxShellOutput runes) together with the process exit code. Non-zero
// exits are reported as an "exit status N" error; a cancelled or
// timed-out context kills the child process and surfaces the context
// error (callers distinguish via ctx.Err()).
func Shell(ctx context.Context, dir, command string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	runErr := cmd.Run()
	out := strutil.Truncate(strings.TrimSpace(combined.String()), maxShellOutput)

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return out, exitErr.ExitCode(), fmt.Errorf("exit status %d", exitErr.ExitCode())
	}
	return out, 0, runErr
}

// FormatShellResult renders a Shell() outcome for direct echo in /sh
// replies (markdown-capable IMs and the TUI chat view):
//
//	$ <command>
//	```
//	<output>
//	```
//	(exit N)        — only when the exit code is non-zero
//	⏱️ timed out    — only when timedOut (context deadline / cancel)
//	(no output)     — only when the command produced no output
func FormatShellResult(command, output string, exitCode int, timedOut bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", command)
	if output != "" {
		b.WriteString("```\n")
		b.WriteString(output)
		b.WriteString("\n```\n")
	}
	switch {
	case timedOut:
		b.WriteString("⏱️ timed out, process killed")
	case exitCode != 0:
		fmt.Fprintf(&b, "(exit %d)", exitCode)
	case output == "":
		b.WriteString("(no output)")
	}
	return b.String()
}
