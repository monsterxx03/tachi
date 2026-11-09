package tools

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/container"
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

	stdoutBuf *container.RingBuf
	stderrBuf *container.RingBuf

	// pm is the manager that owns this process. It is what reports a natural exit
	// to the conversation's task queue, and what forgets the process afterwards.
	pm *ProcessManager

	// conversation is the logical conversation that started this process, captured
	// from the tool context. The TUI runs one agent across sessions, so the event
	// has to say which conversation it belongs to (tasks.Event.Conversation).
	conversation string

	// done is closed exactly once, by setTerminal, when the process reaches a
	// terminal state. Wait blocks on it — waiting for a process is a pure
	// observation, so it must not cost a process of its own.
	done chan struct{}
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
		info.RecentStdout = truncateRecent(mp.stdoutBuf)
	}
	if mp.stderrBuf != nil {
		info.RecentStderr = truncateRecent(mp.stderrBuf)
	}

	return info
}

// truncateRecent caps the recent-output snapshot (adopted foreground
// processes may carry a 1MB ring buffer; List must stay context-safe).
// Overwritten (wrapped) buffers are flagged so readers know the head is gone.
func truncateRecent(buf *container.RingBuf) string {
	s := buf.String()
	if len(s) <= recentOutputCap && !buf.Wrapped() {
		return s
	}
	head := ""
	if buf.Wrapped() {
		head = "[HEAD DROPPED]\n"
	}
	cut := recentOutputCap - len(head)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return head + s[:cut] + "\n... (truncated)"
}

// ProcessManager manages background processes started by the Bash tool.
// It is a singleton shared across all BashTool instances within the same agent.
//
// A process that ends on its own is announced through the sink (the
// conversation's task queue) and then forgotten. A process that was stopped on
// purpose is not: the caller asked for it, so there is nothing to report.
type ProcessManager struct {
	processes sync.Map // map[string]*ManagedProcess

	// sink receives one event per natural exit. It is bound once, by the agent
	// that owns this manager: a forked child shares the parent's manager, and a
	// late bind must not move the conversation's news to the child's queue.
	sinkMu sync.Mutex
	sink   func(tasks.Event)
}

// NewProcessManager creates a new ProcessManager.
func NewProcessManager() *ProcessManager {
	return &ProcessManager{}
}

// BindSink points the manager at the conversation's task queue. Only the first
// call takes effect — the manager is shared with forked agents, and the
// conversation that created it is the one that owns its news.
func (pm *ProcessManager) BindSink(sink func(tasks.Event)) {
	if pm == nil || sink == nil {
		return
	}
	pm.sinkMu.Lock()
	defer pm.sinkMu.Unlock()
	if pm.sink == nil {
		pm.sink = sink
	}
}

// report hands one finished process to the sink (when bound) and forgets it:
// the event is the snapshot, and the manager's job is to track what is still
// running. A finished process left in the map would make list_bg grow with
// things that are no longer running, and wait_name would keep finding corpses.
func (pm *ProcessManager) report(mp *ManagedProcess) {
	pm.sinkMu.Lock()
	sink := pm.sink
	pm.sinkMu.Unlock()

	if sink != nil {
		info := mp.toInfo()
		status := tasks.StatusExited
		if info.Status == ProcessError {
			status = tasks.StatusError
		}
		sink(tasks.Event{
			ID:           mp.Name,
			Conversation: mp.conversation,
			Kind:         tasks.KindBackground,
			Name:         mp.Name,
			Command:      mp.Command,
			Status:       status,
			ExitCode:     info.ExitCode,
			Error:        info.Error,
			Stdout:       info.RecentStdout,
			Stderr:       info.RecentStderr,
			At:           time.Now(),
		})
	}
	// CompareAndDelete, not Delete: a stopped process frees its name for reuse, and
	// the late exit goroutine of the OLD process must not remove the NEW one.
	pm.processes.CompareAndDelete(mp.Name, mp)
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
	// Same reason as the foreground path: a detached command with no controlling terminal can
	// only be asked for a password through the askpass helper (askpass.go).
	cmd.Env = bashEnv()

	// Process group isolation: kill -pgid terminates the entire tree.
	// Setsid detaches from the parent's controlling terminal, preventing
	// interactive commands from hanging on /dev/tty reads.
	// Setsid alone also makes the child a process group leader (PGID = PID),
	// so -PID process group kill works — no separate Setpgid needed.
	// macOS cannot use Setsid+Setpgid together (setsid() fails if PG leader).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	stdoutBuf := container.NewRingBuf(recentOutputCap)
	stderrBuf := container.NewRingBuf(recentOutputCap)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start background process '%s': %w", name, err)
	}

	mp := &ManagedProcess{
		Name:         name,
		Command:      command,
		Cmd:          cmd,
		PID:          cmd.Process.Pid,
		StartedAt:    time.Now(),
		stdoutBuf:    stdoutBuf,
		stderrBuf:    stderrBuf,
		pm:           pm,
		conversation: LogicalConversation(ctx),
		done:         make(chan struct{}),
	}
	if !pm.register(mp, true) {
		// Concurrent Start with the same name won the race — kill this one.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil, fmt.Errorf("background process '%s' already exists; stop it first or use a different name", name)
	}

	return mp.toInfo(), nil
}

// Adopt takes over an already-started foreground command and registers it as
// a background process. Used when a foreground command outlives its window
// (auto-background). The command must have been started with Setsid (process
// group leader) so Stop can kill the whole tree.
//
// The caller's own goroutine keeps waiting on cmd.Wait() (exec.Cmd forbids
// concurrent Wait calls), so no wait goroutine is started here; the caller
// must call mp.recordExit(err) once Wait returns.
func (pm *ProcessManager) Adopt(name, command string, cmd *exec.Cmd, startedAt time.Time, stdoutBuf, stderrBuf *container.RingBuf, conversation string) (*ManagedProcess, error) {
	mp := &ManagedProcess{
		Name:         name,
		Command:      command,
		Cmd:          cmd,
		PID:          cmd.Process.Pid,
		StartedAt:    startedAt,
		stdoutBuf:    stdoutBuf,
		stderrBuf:    stderrBuf,
		pm:           pm,
		conversation: conversation,
		done:         make(chan struct{}),
	}
	if !pm.register(mp, false) {
		return nil, fmt.Errorf("background process '%s' already exists; stop it first or use a different name", name)
	}
	return mp, nil
}

// register stores a running ManagedProcess under its name atomically and,
// when waitSelf is true, starts the goroutine that records its exit state
// (Start path). Adopted processes pass false — their waiter goroutine
// already exists. Returns false when a process with the same name is already
// registered (the caller must clean up the command).
func (pm *ProcessManager) register(mp *ManagedProcess, waitSelf bool) bool {
	mp.status.Store(_psRunning)
	if _, loaded := pm.processes.LoadOrStore(mp.Name, mp); loaded {
		return false
	}
	if waitSelf {
		go func() {
			err := mp.Cmd.Wait()
			mp.recordExit(err)
		}()
	}
	return true
}

// recordExit stores the exit state of a managed process. Safe to call more
// than once (rendezvous paths may race); the CAS guard lets exactly one call
// win, and only the natural exit announces anything — a stop already recorded
// its outcome, and the caller asked for that.
func (mp *ManagedProcess) recordExit(err error) {
	to := int32(_psExited)
	var ec int32
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			ec = int32(exitErr.ExitCode())
		} else {
			ec = -1
			mp.ExitErr = err // surfaced via toInfo when status is _psError
			to = int32(_psError)
		}
	}
	// Report BEFORE waking waiters: whoever observes "finished" then also finds
	// the event queued and the process gone from the map, instead of a half
	// transition that a test (or the next list_bg) could race.
	var beforeClose func()
	if mp.pm != nil {
		beforeClose = func() { mp.pm.report(mp) }
	}
	mp.setTerminal(to, ec, beforeClose)
}

// setTerminal moves the process from running to a terminal state and wakes any
// waiter. It returns false when a terminal state was already recorded (this is
// the CAS loser of a race, e.g. the exit goroutine reporting after Stop).
//
// Both terminal transitions — natural exit (recordExit) and kill (stopProcess)
// — go through here, which is what makes the single close(mp.done) safe: only
// the CAS winner reaches it. The exit code is stored BEFORE the close so a
// waiter that wakes on done already sees the final status and code; beforeClose
// (the natural exit's report) runs in the same window, for the same reason.
func (mp *ManagedProcess) setTerminal(to, exitCode int32, beforeClose func()) bool {
	if !mp.status.CompareAndSwap(_psRunning, to) {
		return false
	}
	mp.exitCode.Store(exitCode)
	if beforeClose != nil {
		beforeClose()
	}
	close(mp.done)
	return true
}

// Stop terminates a background process by name. Sends SIGTERM to the process
// group first, then SIGKILL after a 5-second grace period. Removes from the
// managed process map on completion.
func (pm *ProcessManager) Stop(name string) (*ManagedProcessInfo, error) {
	v, ok := pm.processes.LoadAndDelete(name)
	if !ok {
		return nil, fmt.Errorf("background process '%s' not found — it may have finished already (finished background tasks are reported to you automatically) or been stopped", name)
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

	if !mp.setTerminal(_psKilled, -1, nil) {
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

// Wait blocks until the named background process reaches a terminal state
// (exited / killed / error) or bound elapses, whichever comes first.
//
// Returns (info, stillRunning, err):
//   - info is the process snapshot at return time. It is nil — with a
//     "not found" error — only when no process of that name is tracked, which is
//     also the case after one finished on its own: a natural exit is reported
//     through the queue and the process leaves the map.
//   - stillRunning reports that the wait ended while the process was still
//     running: bound elapsed, or ctx was cancelled.
//   - err carries ctx.Err() on cancellation; the snapshot is still returned so
//     the caller can report what it saw.
//
// Waiting is a pure observation — it spawns nothing and holds no lock, so a
// caller that wants to wait for a slow command should block here instead of
// starting a sleeper of its own.
func (pm *ProcessManager) Wait(ctx context.Context, name string, bound time.Duration) (*ManagedProcessInfo, bool, error) {
	v, ok := pm.processes.Load(name)
	if !ok {
		return nil, false, fmt.Errorf("background process '%s' not found", name)
	}
	mp := v.(*ManagedProcess)

	// A process that is already terminal has its done channel closed, so this
	// select returns its final state immediately.
	timer := time.NewTimer(bound)
	defer timer.Stop()

	select {
	case <-mp.done:
		return mp.toInfo(), false, nil
	case <-timer.C:
		return mp.toInfo(), true, nil
	case <-ctx.Done():
		return mp.toInfo(), true, ctx.Err()
	}
}

// ListFor returns the running processes that belong to one conversation. The TUI
// hosts several conversations on one agent; a list that mixed them would offer
// wait_name/stop_name for a process the reader is not talking about.
func (pm *ProcessManager) ListFor(conversation string) []ManagedProcessInfo {
	var result []ManagedProcessInfo
	pm.processes.Range(func(key, value any) bool {
		mp := value.(*ManagedProcess)
		if mp.conversation != conversation {
			return true
		}
		result = append(result, *mp.toInfo())
		return true
	})
	return result
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
