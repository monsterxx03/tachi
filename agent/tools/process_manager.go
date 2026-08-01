package tools

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/ringbuf"
)

// ProcessStatus represents the current state of a managed background process.
type ProcessStatus string

const (
	ProcessRunning ProcessStatus = "running"
	ProcessExited  ProcessStatus = "exited"
	ProcessKilled  ProcessStatus = "killed"
	ProcessError   ProcessStatus = "error" // failed to start
)

// Internal state enum for atomic operations.
const (
	_psRunning int32 = iota
	_psExited
	_psKilled
	_psError
)

func psString(s int32) ProcessStatus {
	switch s {
	case _psRunning:
		return ProcessRunning
	case _psExited:
		return ProcessExited
	case _psKilled:
		return ProcessKilled
	case _psError:
		return ProcessError
	default:
		return "unknown"
	}
}

// recentOutputCap is the maximum size of the ring buffer for recent stdout/stderr.
const recentOutputCap = 64 * 1024 // 64KB

// ManagedProcessInfo is the JSON-serializable summary returned to the LLM.
type ManagedProcessInfo struct {
	Name         string        `json:"name"`
	PID          int           `json:"pid"`
	Command      string        `json:"command"`
	Status       ProcessStatus `json:"status"`
	ExitCode     int           `json:"exitCode,omitempty"`
	StartedAt    string        `json:"startedAt"`
	Uptime       string        `json:"uptime,omitempty"`
	Error        string        `json:"error,omitempty"`
	RecentStdout string        `json:"recentStdout,omitempty"`
	RecentStderr string        `json:"recentStderr,omitempty"`
}

// ManagedProcess represents a single background process tracked by ProcessManager.
type ManagedProcess struct {
	Name      string
	Command   string
	Cmd       *exec.Cmd
	PID       int
	StartedAt time.Time
	ExitErr   error // non-nil only when Status==ProcessError; set before status store

	status   atomic.Int32 // _psRunning / _psExited / _psKilled / _psError
	exitCode atomic.Int32

	stdoutBuf *ringbuf.Buffer
	stderrBuf *ringbuf.Buffer
}

// toInfo returns a snapshot of the process for external consumption. Lock-free.
func (mp *ManagedProcess) toInfo() *ManagedProcessInfo {
	s := mp.status.Load()

	info := &ManagedProcessInfo{
		Name:      mp.Name,
		PID:       mp.PID,
		Command:   mp.Command,
		Status:    psString(s),
		ExitCode:  int(mp.exitCode.Load()),
		StartedAt: mp.StartedAt.Format(time.RFC3339),
	}

	if s == _psRunning {
		info.Uptime = time.Since(mp.StartedAt).Truncate(time.Second).String()
	}

	if s == _psError && mp.ExitErr != nil {
		info.Error = mp.ExitErr.Error()
	}

	if mp.stdoutBuf != nil {
		info.RecentStdout = mp.stdoutBuf.String()
	}
	if mp.stderrBuf != nil {
		info.RecentStderr = mp.stderrBuf.String()
	}

	return info
}

// ProcessManager manages background processes started by the Bash tool.
// It is a singleton shared across all BashTool instances within the same agent.
type ProcessManager struct {
	processes sync.Map // map[string]*ManagedProcess
}

// NewProcessManager creates a new ProcessManager.
func NewProcessManager() *ProcessManager {
	return &ProcessManager{}
}

// Start launches a command in the background. If a process with the same name
// already exists, an error is returned.
func (pm *ProcessManager) Start(ctx context.Context, name, command string) (*ManagedProcessInfo, error) {
	if _, loaded := pm.processes.Load(name); loaded {
		return nil, fmt.Errorf("background process '%s' already exists; stop it first or use a different name", name)
	}

	// Use context.Background() so the process survives the tool call's ctx.
	cmd := exec.CommandContext(context.Background(), "bash", "-c", command)
	cmd.Dir = wdctx.Dir(ctx)

	// Process group isolation: kill -pgid terminates the entire tree.
	// Setsid detaches from the parent's controlling terminal, preventing
	// interactive commands from hanging on /dev/tty reads.
	// Setsid alone also makes the child a process group leader (PGID = PID),
	// so -PID process group kill works — no separate Setpgid needed.
	// macOS cannot use Setsid+Setpgid together (setsid() fails if PG leader).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	stdoutBuf := ringbuf.New(recentOutputCap)
	stderrBuf := ringbuf.New(recentOutputCap)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start background process '%s': %w", name, err)
	}

	mp := &ManagedProcess{
		Name:      name,
		Command:   command,
		Cmd:       cmd,
		PID:       cmd.Process.Pid,
		StartedAt: time.Now(),
		stdoutBuf: stdoutBuf,
		stderrBuf: stderrBuf,
	}
	mp.status.Store(_psRunning)

	// Background goroutine: waits for process exit.
	go func() {
		err := cmd.Wait()

		var ec int32
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				ec = int32(exitErr.ExitCode())
			} else {
				ec = -1
				mp.ExitErr = err // set before status store for toInfo visibility
			}
		}
		mp.exitCode.Store(ec)

		// CAS ensures stopProcess and this goroutine don't race.
		if !mp.status.CompareAndSwap(_psRunning, _psExited) {
			return // stopProcess already set status to _psKilled
		}
	}()

	pm.processes.Store(name, mp)

	return mp.toInfo(), nil
}

// Stop terminates a background process by name. Sends SIGTERM to the process
// group first, then SIGKILL after a 5-second grace period. Removes from the
// managed process map on completion.
func (pm *ProcessManager) Stop(name string) (*ManagedProcessInfo, error) {
	v, ok := pm.processes.LoadAndDelete(name)
	if !ok {
		return nil, fmt.Errorf("background process '%s' not found", name)
	}
	mp := v.(*ManagedProcess)

	if mp.status.Load() != _psRunning {
		return mp.toInfo(), nil // already stopped by Wait goroutine or earlier stop
	}

	stopProcess(mp)
	return mp.toInfo(), nil
}

// stopProcess kills the process group. Uses CAS to avoid racing with the
// Wait goroutine from Start. Sets status to ProcessKilled if the process
// was still running; otherwise the process has already exited naturally.
func stopProcess(mp *ManagedProcess) {
	pgid := mp.Cmd.Process.Pid

	mp.exitCode.Store(-1)
	if !mp.status.CompareAndSwap(_psRunning, _psKilled) {
		return // already exited naturally — don't send signals
	}

	// Graceful termination — SIGTERM first, then SIGKILL after a grace period.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	// Force-kill after 5s if the process hasn't exited yet.
	go func() {
		time.Sleep(5 * time.Second)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()
}

// List returns a snapshot of all tracked background processes.
func (pm *ProcessManager) List() []ManagedProcessInfo {
	var result []ManagedProcessInfo
	pm.processes.Range(func(key, value any) bool {
		mp := value.(*ManagedProcess)
		info := mp.toInfo()
		if info != nil {
			result = append(result, *info)
		}
		return true
	})
	return result
}

// Get returns info for a specific process, or nil if not found.
func (pm *ProcessManager) Get(name string) *ManagedProcessInfo {
	v, ok := pm.processes.Load(name)
	if !ok {
		return nil
	}
	return v.(*ManagedProcess).toInfo()
}

// KillAll stops all tracked processes. Called on agent shutdown.
// Best-effort cleanup; errors are silently ignored.
func (pm *ProcessManager) KillAll() {
	pm.processes.Range(func(key, value any) bool {
		pm.processes.Delete(key)
		mp := value.(*ManagedProcess)
		if mp.status.Load() == _psRunning {
			stopProcess(mp)
		}
		return true
	})
}

// DrainCompleted collects all processes that have exited (not killed),
// removes them from the tracked map, and returns their info. Used by
// BackgroundTaskReminder (Phase 2) to notify the LLM about completed tasks.
func (pm *ProcessManager) DrainCompleted() []ManagedProcessInfo {
	var completed []ManagedProcessInfo
	pm.processes.Range(func(key, value any) bool {
		mp := value.(*ManagedProcess)
		if mp.status.Load() == _psExited {
			info := mp.toInfo()
			if info != nil {
				completed = append(completed, *info)
			}
			pm.processes.Delete(key)
		}
		return true
	})
	return completed
}
