package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/monsterxx03/tachi/agent/wdctx"
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
	Command    string `json:"command"`
	Timeout    *int   `json:"timeout,omitempty"`
	Background bool   `json:"background,omitempty"`
	BgName     string `json:"bg_name,omitempty"`
	StopName   string `json:"stop_name,omitempty"`
	ListBg     bool   `json:"list_bg,omitempty"`
}

type BashTool struct {
	processManager *ProcessManager
}

// NewBashTool creates a BashTool with an optional ProcessManager for
// background process support. Pass nil to disable background operations.
func NewBashTool(pm *ProcessManager) *BashTool {
	return &BashTool{processManager: pm}
}

func (t BashTool) Name() string { return ToolNameBash }
func (t BashTool) Description() string {
	return "Executes a shell command and returns its output. " +
		"The working directory persists between commands. " +
		"Use for running build commands, tests, git operations, and other shell tasks. " +
		"Run long-lived commands in background with background=true and stop with stop_name. " +
		"Use list_bg to list all background processes."
}
func (t BashTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"command":    {Type: "string", Description: "The bash command to execute"},
		"timeout":    {Type: "integer", Description: "Optional timeout in milliseconds (max 600000, default 120000)"},
		"background": {Type: "boolean", Description: "Set true to run this command in the background. Requires bg_name."},
		"bg_name":    {Type: "string", Description: "A unique name for this background process. Required when background=true. Use this name with stop_name to stop it later."},
		"stop_name":  {Type: "string", Description: "Stop a background process by its bg_name."},
		"list_bg":    {Type: "boolean", Description: "Set true to list all running background processes."},
	}
}
func (t BashTool) Required() []string { return []string{"command"} }
func (t BashTool) Parallel() bool     { return false }

func (t BashTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var a bashArgs
	if err := parseArgs(args, &a); err != nil {
		return "", err
	}

	pm := t.processManager

	// list_bg: list all background processes
	if a.ListBg {
		if pm == nil {
			return "", fmt.Errorf("background process management is not available (no ProcessManager configured)")
		}
		list := pm.List()
		return marshalResult(list)
	}

	// stop_name: stop a background process by name
	if a.StopName != "" {
		if pm == nil {
			return "", fmt.Errorf("background process management is not available (no ProcessManager configured)")
		}
		info, err := pm.Stop(a.StopName)
		if err != nil {
			return "", err
		}
		return marshalResult(info)
	}

	// background: start a background process
	if a.Background {
		if pm == nil {
			return "", fmt.Errorf("background process management is not available (no ProcessManager configured)")
		}
		if a.Command == "" {
			return "", fmt.Errorf("command is required for background mode")
		}
		if a.BgName == "" {
			return "", fmt.Errorf("bg_name is required for background mode")
		}
		info, err := pm.Start(ctx, a.BgName, a.Command)
		if err != nil {
			return "", err
		}
		return marshalResult(info)
	}

	// Default: foreground execution (original behavior)
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
	cmd.Dir = wdctx.Dir(ctx)

	// Process group isolation: kill -pgid terminates the entire process tree
	// (bash + all children like python/wget/curl). Without this, exec.CommandContext
	// only kills the immediate bash process, leaving orphan children running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative PID = process group kill. The process group ID equals the
		// bash process PID because Setpgid assigns the child's PID as its PGID.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

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

	if err := ctx.Err(); err != nil {
		result.Interrupted = true
		result.Stdout, _ = trimAndTruncate(stdout.Bytes())
		if errors.Is(err, context.DeadlineExceeded) {
			result.Stderr = fmt.Sprintf("Command timed out after %s", timeout)
		} else {
			result.Stderr = fmt.Sprintf("Command interrupted: %v", err)
		}
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

var (
	workingDir   string
	workingDirMu sync.RWMutex
)

func getWorkingDir() string {
	workingDirMu.RLock()
	dir := workingDir
	workingDirMu.RUnlock()
	if dir != "" {
		return dir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func SetWorkingDir(dir string) {
	workingDirMu.Lock()
	workingDir = dir
	workingDirMu.Unlock()
}

func init() {
	wdctx.SetFallbackDir(getWorkingDir)
}
