package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/container"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const (
	maxOutputSize = 1 << 20 // 1MB

	// defaultForegroundWindow is how long a foreground command may run before
	// it is automatically moved to the background (see BashTool).
	defaultForegroundWindow = 15 * time.Second

	// legacyBashTimeout applies when no ProcessManager is available: the
	// command cannot be backgrounded, so the pre-auto-background hard-kill
	// window is kept.
	legacyBashTimeout = 120 * time.Second

	// drainTimeout bounds waiting for a killed process to exit. SIGKILL
	// cannot terminate D-state (uninterruptible sleep) processes, so an
	// unbounded wait could hang the tool call forever.
	drainTimeout = 5 * time.Second
)

type BashResult struct {
	Stdout       string `json:"stdout,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
	ExitCode     int    `json:"exitCode"`
	DurationMs   int64  `json:"durationMs"`
	Interrupted  bool   `json:"interrupted,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
	Backgrounded bool   `json:"backgrounded,omitempty"`
	BgName       string `json:"bgName,omitempty"`
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

	// foregroundWindow is how long a foreground command may run before being
	// automatically moved to the background. 0 = defaultForegroundWindow.
	// Exposed as a field for tests.
	foregroundWindow time.Duration
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
func (t *BashTool) Description() string {
	desc := "Executes a shell command and returns its output. " +
		"The working directory persists between commands. " +
		"Use for running build commands, tests, git operations, and other shell tasks. "
	if t.processManager != nil {
		desc += "Commands still running after the foreground window (~15s, or the timeout value if shorter) " +
			"are automatically moved to the background — check progress with list_bg and stop with stop_name. " +
			"Run long-lived commands in background with background=true and stop with stop_name. " +
			"Use list_bg to list all background processes."
	}
	return desc
}
func (t *BashTool) Properties() map[string]PropertySchema {
	timeoutDesc := "Foreground window in milliseconds — commands still running after it are moved to the background (default 15000, max 600000)"
	if t.processManager == nil {
		timeoutDesc = "Optional timeout in milliseconds before the command is killed (max 600000)"
	}
	return map[string]PropertySchema{
		"command":    {Type: "string", Description: "The bash command to execute"},
		"timeout":    {Type: "integer", Description: timeoutDesc, Minimum: new(1.0), Maximum: new(600000.0), Default: 15000},
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

	// Default: foreground execution.
	if a.Command == "" {
		return "", fmt.Errorf("command is required")
	}

	// Foreground window: a command still running after this is automatically
	// moved to the background (managed via list_bg/stop_name) instead of
	// being killed — long builds/tests keep producing output the model can
	// inspect. The optional timeout narrows the window; it is no longer a
	// hard kill.
	window := t.foregroundWindow
	if window <= 0 {
		window = defaultForegroundWindow
	}
	if a.Timeout != nil {
		if d := time.Duration(*a.Timeout) * time.Millisecond; d > 0 && d < window {
			window = d
		}
	}

	if ctx == nil {
		ctx = context.Background()
	}

	// Command runs detached from any tool-call deadline: cancellation is
	// handled via the select below (window / ctx.Done), and an adopted
	// background process must survive the turn.
	cmd := exec.Command("bash", "-c", a.Command)
	cmd.Dir = wdctx.Dir(ctx)

	// Process group isolation: kill -pgid terminates the entire process tree
	// (bash + all children like python/wget/curl). Without this, killing
	// only the immediate bash process leaves orphan children running.
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

	// RingBuf captures output for both the foreground return and (on
	// auto-background) the managed process's recent-output view. Capacity
	// mirrors the previous 1MB buffer cap.
	stdoutBuf := container.NewRingBuf(maxOutputSize + 256)
	stderrBuf := container.NewRingBuf(maxOutputSize + 256)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to execute command: %w", err)
	}

	// adopted/adoptCh: handoff to the background. The wait goroutine stays
	// the sole caller of cmd.Wait() (exec.Cmd forbids concurrent Wait); on
	// exit it reports the state to the adopted process via recordExit.
	var adopted atomic.Pointer[ManagedProcess]
	adoptCh := make(chan *ManagedProcess, 1)
	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if mp := adopted.Load(); mp != nil {
			mp.recordExit(err)
		} else {
			select {
			case mp := <-adoptCh: // main already adopted (process exited right after): record here
				mp.recordExit(err)
			default: // main not there yet: normal waitCh path
			}
		}
		waitCh <- err
	}()

	// finish renders the completed-command result (shared by the exit branch
	// and the boundary drains in the window/cancel branches).
	finish := func(err error) (string, error) {
		duration := time.Since(start).Milliseconds()
		result := BashResult{DurationMs: duration}
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exitErr.ExitCode()
			} else {
				return "", fmt.Errorf("failed to execute command: %w", err)
			}
		}
		result.Stdout, result.Truncated = formatRingOutput(stdoutBuf)
		stderrStr, stderrTruncated := formatRingOutput(stderrBuf)
		result.Stderr = stderrStr
		result.Truncated = result.Truncated || stderrTruncated
		t.spillOversized(&result)
		return marshalResult(result)
	}

	// killAndDrain kills the process group and waits (bounded) for exit.
	killAndDrain := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-waitCh:
		case <-time.After(drainTimeout):
		}
	}

	// interrupted renders the killed-command result (shared by the legacy
	// timeout and cancel branches).
	interrupted := func(reason string) (string, error) {
		duration := time.Since(start).Milliseconds()
		stdout, _ := formatRingOutput(stdoutBuf)
		result := BashResult{
			Stdout:      stdout,
			Stderr:      reason,
			ExitCode:    -1,
			DurationMs:  duration,
			Interrupted: true,
		}
		t.spillOversized(&result)
		return marshalResult(result)
	}

	select {
	case err := <-waitCh:
		return finish(err)

	case <-time.After(window):
		// Process may have exited right at the window boundary — drain first
		// so a successful command is not misreported as backgrounded.
		select {
		case err := <-waitCh:
			return finish(err)
		default:
		}
		// A cancelled turn must not background the command.
		if ctx.Err() != nil {
			killAndDrain()
			return interrupted(fmt.Sprintf("Command interrupted: %v", ctx.Err()))
		}
		if t.processManager != nil {
			bgName := fmt.Sprintf("bash-%d", time.Now().UnixNano())
			mp, err := t.processManager.Adopt(bgName, a.Command, cmd, start, stdoutBuf, stderrBuf)
			if err != nil {
				// Registration failed (name collision etc.) — kill to avoid
				// leaking a detached process.
				killAndDrain()
				return "", fmt.Errorf("failed to background command: %w", err)
			}
			// Rendezvous: exactly one of these paths records the exit state
			// (the process may have exited between the drain above and now).
			select {
			case adoptCh <- mp: // wait goroutine is at the receive point: it records
			case err := <-waitCh: // goroutine passed the receive point: record here
				mp.recordExit(err)
			default: // goroutine still in cmd.Wait(): normal path
				adopted.Store(mp)
			}
			stdout, _ := formatRingOutput(stdoutBuf)
			duration := time.Since(start).Milliseconds()
			result := BashResult{
				Stdout: stdout,
				Stderr: fmt.Sprintf("Command still running after %s — continuing in background as %q. "+
					"Use list_bg to check progress or stop_name to terminate.", window, bgName),
				// Backgrounded commands have no exit code yet — 0 avoids
				// -1 being read as a crash/failure.
				ExitCode:     0,
				DurationMs:   duration,
				Backgrounded: true,
				BgName:       bgName,
			}
			t.spillOversized(&result)
			return marshalResult(result)
		}

		// No ProcessManager — legacy behavior: kill and report the timeout.
		killAndDrain()
		return interrupted(fmt.Sprintf("Command timed out after %s", window))

	case <-ctx.Done():
		// Process may have exited right at the cancel boundary — drain first.
		select {
		case err := <-waitCh:
			return finish(err)
		default:
		}
		killAndDrain()
		return interrupted(fmt.Sprintf("Command interrupted: %v", ctx.Err()))
	}
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
// instead of the full oversized output. Note the saved file holds the ring
// buffer's retained tail (the head may have been dropped).
func formatBashSpill(stream string, chars, limit int, path string) string {
	return fmt.Sprintf(
		"[BASH OUTPUT TOO LARGE] %s: %d chars exceeds limit (%d chars).\n"+
			"Buffered output saved to:\n  %s\n"+
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

// formatRingOutput renders a ring buffer for the LLM: truncated to the 1MB
// cap, with a head-dropped flag when the buffer wrapped (the oldest output
// is gone — the retained content is the tail).
func formatRingOutput(buf *container.RingBuf) (string, bool) {
	s, truncated := trimAndTruncate([]byte(buf.String()))
	if buf.Wrapped() {
		s = "[HEAD DROPPED — output exceeded buffer]\n" + s
		truncated = true
	}
	return s, truncated
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
