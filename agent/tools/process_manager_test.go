package tools

import (
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// waitForProcess polls pm.Get(name) at a short interval until the process
// exits (or is killed/errored), or until the timeout elapses. It returns the
// final process info on success, or calls t.Fatalf on timeout.
//
// Using this instead of time.Sleep makes tests faster (quick-exiting processes
// are detected within milliseconds) and more reliable (no flaky fixed sleeps).
func waitForProcess(t *testing.T, pm *ProcessManager, name string, timeout time.Duration) *ManagedProcessInfo {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for {
		info := pm.Get(name)
		if info != nil && info.Status != ProcessRunning {
			return info
		}
		select {
		case <-deadline:
			info := pm.Get(name)
			if info != nil {
				t.Fatalf("timed out waiting for process %q to exit (status=%s)", name, info.Status)
			}
			t.Fatalf("timed out waiting for process %q to exit (not found)", name)
			return nil
		case <-tick.C:
		}
	}
}

func TestProcessManager_StartStop(t *testing.T) {
	pm := NewProcessManager()

	// Start a simple background process
	info, err := pm.Start(t.Context(), "test-sleep", "sleep 10")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if info.Name != "test-sleep" {
		t.Errorf("expected name 'test-sleep', got %q", info.Name)
	}
	if info.Status != ProcessRunning {
		t.Errorf("expected status running, got %s", info.Status)
	}
	if info.PID <= 0 {
		t.Errorf("expected positive PID, got %d", info.PID)
	}

	// Verify it appears in List
	list := pm.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 process in list, got %d", len(list))
	}
	if list[0].Name != "test-sleep" {
		t.Errorf("expected name 'test-sleep', got %q", list[0].Name)
	}

	// Get by name
	g := pm.Get("test-sleep")
	if g == nil {
		t.Fatal("Get returned nil for existing process")
	}
	if g.Name != "test-sleep" {
		t.Errorf("expected name 'test-sleep', got %q", g.Name)
	}

	// Get non-existent
	if pm.Get("nonexistent") != nil {
		t.Error("Get should return nil for non-existent process")
	}

	// Stop it
	info, err = pm.Stop("test-sleep")
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if info.Status != ProcessKilled {
		t.Errorf("expected status killed, got %s", info.Status)
	}

	// Verify it's gone from list
	list = pm.List()
	if len(list) != 0 {
		t.Errorf("expected empty list after stop, got %d entries", len(list))
	}

	// Stop non-existent should error
	_, err = pm.Stop("test-sleep")
	if err == nil {
		t.Error("expected error stopping non-existent process")
	}
}

func TestProcessManager_StartDuplicate(t *testing.T) {
	pm := NewProcessManager()

	_, err := pm.Start(t.Context(), "dupe", "sleep 1")
	if err != nil {
		t.Fatalf("first Start failed: %v", err)
	}

	_, err = pm.Start(t.Context(), "dupe", "sleep 1")
	if err == nil {
		t.Error("expected error for duplicate name")
	}

	pm.Stop("dupe") //nolint:errcheck
}

func TestProcessManager_KillAll(t *testing.T) {
	pm := NewProcessManager()

	pm.Start(t.Context(), "a", "sleep 60")
	pm.Start(t.Context(), "b", "sleep 60")
	pm.Start(t.Context(), "c", "sleep 60")

	pm.KillAll()

	// All should be gone
	list := pm.List()
	if len(list) != 0 {
		t.Errorf("expected empty list after KillAll, got %d entries", len(list))
	}
}

func TestProcessManager_StopAlreadyExited(t *testing.T) {
	pm := NewProcessManager()

	pm.Start(t.Context(), "quick", "true") // exits immediately

	// Wait for it to exit (polling is faster and more reliable than fixed sleep)
	waitForProcess(t, pm, "quick", 5*time.Second)

	info, err := pm.Stop("quick")
	if err != nil {
		t.Fatalf("Stop of already-exited process failed: %v", err)
	}
	if info.Status != ProcessExited {
		t.Errorf("expected status exited, got %s", info.Status)
	}
	if info.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", info.ExitCode)
	}

	// Should be cleaned up from map
	if pm.Get("quick") != nil {
		t.Error("already-exited process should be removed from map")
	}
}

func TestProcessManager_ListEmpty(t *testing.T) {
	pm := NewProcessManager()
	list := pm.List()
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d entries", len(list))
	}
}

func TestProcessManager_DrainCompleted(t *testing.T) {
	pm := NewProcessManager()

	// Start a process that exits quickly
	_, err := pm.Start(t.Context(), "done1", "echo hello")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Start a long-running process
	_, err = pm.Start(t.Context(), "running", "sleep 60")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for the quick one to exit
	waitForProcess(t, pm, "done1", 5*time.Second)

	completed := pm.DrainCompleted()
	if len(completed) != 1 {
		t.Fatalf("expected 1 completed process, got %d", len(completed))
	}
	if completed[0].Name != "done1" {
		t.Errorf("expected 'done1', got %q", completed[0].Name)
	}

	// "running" should still be there
	if pm.Get("running") == nil {
		t.Error("running process should still be in the map")
	}

	// Clean up
	pm.Stop("running")
}

func TestProcessManager_OutputCapture(t *testing.T) {
	pm := NewProcessManager()

	if runtime.GOOS == "windows" {
		t.Skip("background output capture test not supported on Windows")
	}

	_, err := pm.Start(t.Context(), "output-test", "echo hello && echo world >&2 && sleep 1 && echo done")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for the process to complete — the background goroutine in Start()
	// will handle cmd.Wait(). Polling is faster and more reliable than fixed sleep.
	info := waitForProcess(t, pm, "output-test", 5*time.Second)

	if info.Status != ProcessExited {
		t.Errorf("expected status exited, got %s", info.Status)
	}
	if info.RecentStdout == "" {
		t.Error("expected stdout output, got empty")
	}

	// Clean up
	pm.DrainCompleted()
}

func TestProcessManager_JSONRoundtrip(t *testing.T) {
	pm := NewProcessManager()

	info, err := pm.Start(t.Context(), "json-test", "sleep 10")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var parsed ManagedProcessInfo
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if parsed.Name != "json-test" {
		t.Errorf("expected name 'json-test', got %q", parsed.Name)
	}
	if parsed.PID != info.PID {
		t.Errorf("PID mismatch: %d vs %d", parsed.PID, info.PID)
	}
	if parsed.Status != "running" {
		t.Errorf("expected status running, got %s", parsed.Status)
	}

	pm.Stop("json-test")
}

func TestProcessManager_StopNonExistent(t *testing.T) {
	pm := NewProcessManager()
	_, err := pm.Stop("no-such-process")
	if err == nil {
		t.Error("expected error for non-existent process")
	}
}

func TestProcessManager_CloseCalledFromAgent(t *testing.T) {
	// Simulates the AIAgent.Close() path: ProcessManager is agent-owned
	// and KillAll() cleans up tracked background processes.
	pm := NewProcessManager()
	pm.KillAll() // should not panic when no processes are tracked
}
