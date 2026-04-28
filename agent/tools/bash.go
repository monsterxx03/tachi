package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashTimeout     = 600 * time.Second
	maxOutputSize      = 1 << 20 // 1MB
)

type BashResult struct {
	Stdout      string `json:"stdout,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	ExitCode    int    `json:"exitCode"`
	DurationMs  int64  `json:"durationMs"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

type bashArgs struct {
	Command string `json:"command"`
	Timeout *int   `json:"timeout"`
}

type BashTool struct{}

func (t BashTool) Name() string { return "Bash" }
func (t BashTool) Description() string {
	return "Executes a shell command and returns its output. " +
		"The working directory persists between commands. " +
		"Use for running build commands, tests, git operations, and other shell tasks."
}
func (t BashTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"command": {Type: "string", Description: "The bash command to execute"},
		"timeout": {Type: "integer", Description: "Optional timeout in milliseconds (max 600000, default 120000)"},
	}
}
func (t BashTool) Required() []string { return []string{"command"} }
func (t BashTool) Parallel() bool     { return false }

func (t BashTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var a bashArgs
	if err := parseArgs(args, &a); err != nil {
		return "", err
	}

	if a.Command == "" {
		return "", fmt.Errorf("command is required")
	}

	timeout := defaultBashTimeout
	if a.Timeout != nil {
		d := min(time.Duration(*a.Timeout)*time.Millisecond, maxBashTimeout)
		if d > 0 {
			timeout = d
		}
	}

	// Create a new context with timeout if none provided
	var cancelFn context.CancelFunc
	if ctx == nil {
		ctx, cancelFn = context.WithTimeout(context.Background(), timeout)
	} else {
		ctx, cancelFn = context.WithTimeout(ctx, timeout)
	}
	defer cancelFn()

	cmd := exec.CommandContext(ctx, "bash", "-c", a.Command)
	cmd.Dir = getWorkingDir()

	var stdout, stderr limitedBuffer
	stdout.maxSize = maxOutputSize + 256
	stderr.maxSize = maxOutputSize + 256
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start).Milliseconds()

	result := BashResult{
		DurationMs: duration,
	}

	if ctx.Err() == context.DeadlineExceeded {
		result.Interrupted = true
		result.Stdout, _ = trimAndTruncate(stdout.Bytes())
		result.Stderr = fmt.Sprintf("Command timed out after %s", timeout)
		result.ExitCode = -1
		return marshalResult(result)
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return "", fmt.Errorf("failed to execute command: %w", err)
		}
	}

	result.Stdout, result.Truncated = trimAndTruncate(stdout.Bytes())
	stderrStr, _ := trimAndTruncate(stderr.Bytes())
	result.Stderr = stderrStr

	return marshalResult(result)
}

func trimAndTruncate(b []byte) (string, bool) {
	b = bytes.TrimSpace(b)
	if len(b) > maxOutputSize {
		return string(b[:maxOutputSize]) + "\n... (output truncated)", true
	}
	return string(b), false
}

type limitedBuffer struct {
	buf     bytes.Buffer
	maxSize int
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	remaining := lb.maxSize - lb.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return lb.buf.Write(p)
}

func (lb *limitedBuffer) Bytes() []byte {
	return lb.buf.Bytes()
}

var workingDir string

func getWorkingDir() string {
	if workingDir != "" {
		return workingDir
	}
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

func SetWorkingDir(dir string) {
	workingDir = dir
}
