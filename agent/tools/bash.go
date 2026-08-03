package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/fileutil"
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
	resultBaseDir  string // dir for oversized output files (default: ~/.tachi/tool_results)
	maxResultChars int    // max runes of output returned to the LLM; 0 = no spill (1MB buffer cap still applies)
}

// BashToolConfig carries the tool's construction options.
type BashToolConfig struct {
	ProcessManager *ProcessManager
	ResultBaseDir  string // Dir for oversized output files (e.g. config tool_result.file_dir)
	MaxResultChars int    // Max runes of output passed to the LLM; 0 = no limit
}

// NewBashTool creates a BashTool with an optional ProcessManager for
// background process support. Pass nil to disable background operations.
// ResultBaseDir/MaxResultChars enable the oversized-output spill (same
// policy as WebFetch); when MaxResultChars <= 0 or ResultBaseDir is empty,
// outputs are only bounded by the 1MB buffer cap.
func NewBashTool(cfg BashToolConfig) *BashTool {
	return &BashTool{
		processManager: cfg.ProcessManager,
		resultBaseDir:  cfg.ResultBaseDir,
		maxResultChars: cfg.MaxResultChars,
	}
}

func (t BashTool) Name() string        { return ToolNameBash }
func (t BashTool) IsDestructive() bool { return true }
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
		"timeout":    {Type: "integer", Description: "Optional timeout in milliseconds (max 600000, default 120000)", Minimum: new(1.0), Maximum: new(600000.0), Default: 120000},
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
	//
	// Setsid creates a new session, detaching from the parent's controlling
	// terminal (/dev/tty). This prevents interactive commands (sudo, pass,
	// ssh -t, etc.) from hanging by reading the TUI's terminal directly.
	// Without Setsid, the child inherits the parent's controlling terminal
	// even when stdin is /dev/null, causing hangs when programs open /dev/tty.
	//
	// Setsid alone also makes the child a process group leader (PGID = PID),
	// so -PID process group kill still works — no separate Setpgid needed.
	// macOS cannot use both Setsid+Setpgid (setsid() fails if already PG leader).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		// Negative PID = process group kill. The process group ID equals the
		// bash process PID because Setsid makes the child a process group leader.
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
		t.spillOversized(&result)
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
	stderrStr, stderrTruncated := trimAndTruncate(stderr.Bytes())
	result.Stderr = stderrStr
	result.Truncated = result.Truncated || stderrTruncated

	t.spillOversized(&result)

	return marshalResult(result)
}

// spillOversized replaces oversized stdout/stderr with a compact pointer to a
// spill file on disk, instead of dumping hundreds of thousands of tokens into
// the context window. Same policy as WebFetch: the 1MB buffer cap bounds
// memory, maxResultChars bounds what the LLM receives. ReadFile can page
// through the spill file with the limit/offset parameters.
//
// No-op when spill is disabled (no ResultBaseDir or maxResultChars <= 0).
// Spill failures degrade silently to the already-truncated output.
func (t *BashTool) spillOversized(r *BashResult) {
	if t.maxResultChars <= 0 || t.resultBaseDir == "" {
		return
	}
	if rc := utf8.RuneCountInString(r.Stdout); rc > t.maxResultChars {
		if p, err := t.spillBashOutput(r.Stdout, "out"); err == nil {
			r.Stdout = formatBashSpill("stdout", rc, t.maxResultChars, p)
			r.Truncated = true
		}
	}
	if rc := utf8.RuneCountInString(r.Stderr); rc > t.maxResultChars {
		if p, err := t.spillBashOutput(r.Stderr, "err"); err == nil {
			r.Stderr = formatBashSpill("stderr", rc, t.maxResultChars, p)
			r.Truncated = true
		}
	}
}

// spillBashOutput writes content to a timestamped file under resultBaseDir.
func (t *BashTool) spillBashOutput(content, kind string) (string, error) {
	filename := fmt.Sprintf("bash_%s_%d.txt", kind, time.Now().UnixNano())
	fp := filepath.Join(t.resultBaseDir, filename)
	if err := fileutil.WriteFilePrivate(fp, []byte(content)); err != nil {
		return "", err
	}
	return fp, nil
}

// formatBashSpill builds the compact pointer message returned to the LLM
// instead of the full oversized output.
func formatBashSpill(stream string, chars, limit int, path string) string {
	return fmt.Sprintf(
		"[BASH OUTPUT TOO LARGE] %s: %d chars exceeds limit (%d chars).\n"+
			"Full output saved to:\n  %s\n"+
			"Use ReadFile with limit=500 (and offset to page) to read the full output.",
		stream, chars, limit, path)
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
