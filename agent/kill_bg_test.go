package agent

import (
	"syscall"
	"testing"
	"time"
)

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// TestKillBackgroundProcesses verifies that interrupting a turn (Ctrl+C)
// terminates background processes started via the ProcessManager. Background
// processes deliberately use context.Background() so they survive the tool
// call — the turn-cancel path must kill them explicitly, otherwise a
// long-running task (e.g. an http server) keeps running after Ctrl+C.
func TestKillBackgroundProcesses(t *testing.T) {
	a := NewAIAgent(nil, 10)
	defer a.Close()

	info, err := a.Config.ProcessManager.Start(t.Context(), "bg-test", "sleep 30")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	pid := info.PID

	a.KillBackgroundProcesses()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return // killed as expected
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("background process %d still alive after KillBackgroundProcesses", pid)
}
