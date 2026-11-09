package tools

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tasks"
)

// captureExits binds a sink to pm and returns the events it reports. A process
// that ends on its own is forgotten by the manager, so the event is the only
// place its final state can be observed.
func captureExits(pm *ProcessManager) <-chan tasks.Event {
	ch := make(chan tasks.Event, 16)
	pm.BindSink(func(e tasks.Event) { ch <- e })
	return ch
}

// waitForExit waits for the event of one named process.
func waitForExit(t *testing.T, ch <-chan tasks.Event, name string, timeout time.Duration) tasks.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-ch:
			if e.Name == name {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for process %q to exit", name)
			return tasks.Event{}
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

	exits := captureExits(pm)
	pm.Start(t.Context(), "quick", "true") // exits immediately

	e := waitForExit(t, exits, "quick", 5*time.Second)
	if e.Status != tasks.StatusExited {
		t.Errorf("expected status exited, got %s", e.Status)
	}
	if e.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", e.ExitCode)
	}

	// The event IS the snapshot: a process that ended on its own leaves the map,
	// so list_bg never grows with things that are no longer running.
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

// A process that ends on its own is reported ONCE and forgotten. The event
// carries the snapshot (output included), so nothing is lost by removing it.
func TestProcessManager_NaturalExitIsReportedOnce(t *testing.T) {
	pm := NewProcessManager()
	exits := captureExits(pm)

	if _, err := pm.Start(t.Context(), "done1", "echo hello"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := pm.Start(t.Context(), "running", "sleep 60"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	e := waitForExit(t, exits, "done1", 5*time.Second)
	if e.Name != "done1" {
		t.Errorf("expected 'done1', got %q", e.Name)
	}
	if !strings.Contains(e.Stdout, "hello") {
		t.Errorf("the event must carry the process's output, got %q", e.Stdout)
	}
	if pm.Get("running") == nil {
		t.Error("running process should still be in the map")
	}

	select {
	case e := <-exits:
		t.Errorf("a natural exit must be reported exactly once, got another: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}

	pm.Stop("running")
}

// A process stopped on purpose is not news: the caller asked for it, and an
// "it finished" wake right after a stop would be noise.
func TestProcessManager_StoppedProcessIsNotReported(t *testing.T) {
	pm := NewProcessManager()
	exits := captureExits(pm)

	if _, err := pm.Start(t.Context(), "server", "sleep 60"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := pm.Stop("server"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	select {
	case e := <-exits:
		t.Errorf("a stopped process must not be reported, got %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestProcessManager_OutputCapture(t *testing.T) {
	pm := NewProcessManager()

	if runtime.GOOS == "windows" {
		t.Skip("background output capture test not supported on Windows")
	}

	exits := captureExits(pm)
	_, err := pm.Start(t.Context(), "output-test", "echo hello && echo world >&2 && sleep 1 && echo done")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// The background goroutine in Start() handles cmd.Wait(); its event carries
	// the retained output, so there is nothing left to poll in the map.
	e := waitForExit(t, exits, "output-test", 5*time.Second)
	if e.Status != tasks.StatusExited {
		t.Errorf("expected status exited, got %s", e.Status)
	}
	if e.Stdout == "" {
		t.Error("expected stdout output, got empty")
	}
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

// Wait is the alternative to polling with a shell sleep (issue #45): it blocks
// on the process's own terminal-state signal, so waiting costs no process.
func TestProcessManager_Wait(t *testing.T) {
	t.Run("finished within the bound", func(t *testing.T) {
		pm := NewProcessManager()
		if _, err := pm.Start(t.Context(), "quick", "echo done; exit 7"); err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		info, stillRunning, err := pm.Wait(t.Context(), "quick", 10*time.Second)
		if err != nil {
			t.Fatalf("Wait failed: %v", err)
		}
		if stillRunning {
			t.Errorf("expected the wait to end on exit, got stillRunning")
		}
		if info.Status != ProcessExited {
			t.Errorf("expected status exited, got %s", info.Status)
		}
		// The exit code is stored before the waiter is woken — a wake-up must
		// never observe a half-written terminal state.
		if info.ExitCode != 7 {
			t.Errorf("expected exit code 7, got %d", info.ExitCode)
		}
		if !strings.Contains(info.RecentStdout, "done") {
			t.Errorf("expected recent stdout to carry the output, got %q", info.RecentStdout)
		}

		// A finished process leaves the map: its snapshot is the event, and the
		// event is reported exactly once. A second wait finds nothing to observe
		// and says so instead of blocking — the tool turns this into "it may have
		// finished already (finished background tasks are reported automatically)".
		again, stillRunning, err := pm.Wait(t.Context(), "quick", 10*time.Second)
		if again != nil || stillRunning || err == nil {
			t.Errorf("second Wait = (%v, %v, %v), want not-found after the exit was reported", again, stillRunning, err)
		}
	})

	t.Run("bound elapses while running", func(t *testing.T) {
		pm := NewProcessManager()
		if _, err := pm.Start(t.Context(), "slow", "sleep 30"); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer pm.Stop("slow")

		start := time.Now()
		info, stillRunning, err := pm.Wait(t.Context(), "slow", 100*time.Millisecond)
		if err != nil {
			t.Fatalf("Wait failed: %v", err)
		}
		if !stillRunning {
			t.Errorf("expected stillRunning after the bound elapsed, got %+v", info)
		}
		if info.Status != ProcessRunning {
			t.Errorf("expected status running, got %s", info.Status)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Wait overran its bound: %s", elapsed)
		}
	})

	t.Run("killed is terminal", func(t *testing.T) {
		pm := NewProcessManager()
		if _, err := pm.Start(t.Context(), "killme", "sleep 30"); err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		done := make(chan *ManagedProcessInfo, 1)
		go func() {
			info, _, _ := pm.Wait(t.Context(), "killme", 10*time.Second)
			done <- info
		}()

		// Give the waiter a moment to actually block, then stop the process.
		time.Sleep(50 * time.Millisecond)
		if _, err := pm.Stop("killme"); err != nil {
			t.Fatalf("Stop failed: %v", err)
		}

		select {
		case info := <-done:
			if info == nil || info.Status != ProcessKilled {
				t.Errorf("expected the waiter to wake on the kill, got %+v", info)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("waiter did not wake when the process was stopped")
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		pm := NewProcessManager()
		if _, err := pm.Start(t.Context(), "cancelme", "sleep 30"); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer pm.Stop("cancelme")

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		info, stillRunning, err := pm.Wait(ctx, "cancelme", 10*time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		// The snapshot still comes back so the caller can report what it saw.
		if info == nil || !stillRunning {
			t.Errorf("expected a running snapshot alongside the error, got %+v (stillRunning=%v)", info, stillRunning)
		}
	})

	t.Run("unknown name", func(t *testing.T) {
		pm := NewProcessManager()
		info, _, err := pm.Wait(t.Context(), "no-such-process", time.Second)
		if err == nil {
			t.Fatal("expected an error for an untracked process")
		}
		if info != nil {
			t.Errorf("expected no snapshot for an untracked process, got %+v", info)
		}
	})
}
