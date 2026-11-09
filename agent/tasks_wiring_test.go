package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/agent/tools"
)

// The wiring the desktop relies on: a background process that ends on its own
// lands in THIS conversation's queue, with the output the model needs. The queue
// is what both the in-turn reminder and the idle wake read from.
func TestBackgroundTaskExitLandsInTheConversationQueue(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{})
	q := a.Config.TaskQueue
	if q == nil {
		t.Fatal("an agent must always have a task queue")
	}

	if _, err := a.Config.ProcessManager.Start(context.Background(), "job", "echo hi; exit 0"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-q.Signals():
	case <-time.After(5 * time.Second):
		t.Fatal("the queue was never signalled")
	}

	events := q.Pending()
	if len(events) != 1 {
		t.Fatalf("Pending = %d events, want 1", len(events))
	}
	e := events[0]
	if e.Name != "job" || e.Kind != tasks.KindBackground {
		t.Errorf("event = %+v, want the background job", e)
	}
	if e.Status != tasks.StatusExited || e.ExitCode != 0 {
		t.Errorf("status/exit = %s/%d, want exited/0", e.Status, e.ExitCode)
	}
	if e.Stdout == "" {
		t.Error("the event must carry the process output")
	}
}

// A one-off agent that is never given a queue still gets one, and its process
// manager reports into it: the reminder path must work even where no entry point
// can wake an idle conversation.
func TestForkedAgentSharesTheParentsQueue(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{})
	fork := a.Fork(ForkConfig{Provider: &mockStreamProvider{}, MaxIterations: 1})
	defer fork.Close()

	if fork.Agent().Config.TaskQueue == a.Config.TaskQueue {
		t.Fatal("a fork gets its own queue; only the ProcessManager is shared")
	}
	if fork.Agent().Config.ProcessManager != a.Config.ProcessManager {
		t.Fatal("a fork must share the parent's ProcessManager")
	}

	// The shared manager still reports to the PARENT's queue: the fork is
	// transient, and the process belongs to the conversation that started it.
	if _, err := fork.Agent().Config.ProcessManager.Start(context.Background(), "fork-job", "true"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-a.Config.TaskQueue.Signals():
	case <-time.After(5 * time.Second):
		t.Fatal("the parent's queue was never signalled")
	}
	events := a.Config.TaskQueue.Pending()
	if len(events) != 1 || events[0].Name != "fork-job" {
		t.Fatalf("parent queue = %+v, want the fork's job", events)
	}
}

// A stopped process is not news: the caller asked for it to stop, and a wake
// turn for it would be noise.
func TestStoppedBackgroundTaskIsNotReported(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{})
	if _, err := a.Config.ProcessManager.Start(context.Background(), "server", "sleep 30"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := a.Config.ProcessManager.Stop("server"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-a.Config.TaskQueue.Signals():
		t.Fatalf("a stopped process must not be reported: %+v", a.Config.TaskQueue.Pending())
	case <-time.After(200 * time.Millisecond):
	}
}

// stubSubagentRunner is the minimum a SubagentTool needs to exist.
type stubSubagentRunner struct{}

func (stubSubagentRunner) RunSubagent(context.Context, tools.SubagentArgs) (string, string, *tools.SubagentResult, error) {
	return "", "", nil, nil
}
func (stubSubagentRunner) AvailableToolNames() []string { return nil }
func (stubSubagentRunner) MaxOutputChars() int          { return 0 }

// Async sub-agents are enabled by the ENTRY POINT's ability to wake an idle
// conversation: the tool is told, not assumed, because a background result on an
// entry that cannot wake would be read by nobody (and `-p` exits outright).
func TestSubagentToolIsAsyncOnlyWhereTheEntryCanWake(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{})

	a.Config.WakeCapable = false
	syncTool := a.newSubagentTool(stubSubagentRunner{}, "")
	if strings.Contains(syncTool.Description(), "BACKGROUND") {
		t.Error("without WakeCapable the tool must describe the blocking behavior")
	}

	a.Config.WakeCapable = true
	asyncTool := a.newSubagentTool(stubSubagentRunner{}, "")
	if !strings.Contains(asyncTool.Description(), "BACKGROUND") {
		t.Error("with WakeCapable the tool must describe the background behavior")
	}
}
