package systemreminder

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// RunningTask is one background process the conversation still has going.
type RunningTask struct {
	Name    string
	Command string
}

// RunningTaskProvider reports what is still running for this conversation. The
// ProcessManager implements the adapter in agent_configure.go.
type RunningTaskProvider interface {
	// RunningTasks returns what is still running in ONE conversation.
	RunningTasks(conversation string) []RunningTask
}

// RunningTasksReminder tells the model what is still running, so an id it has
// forgotten is recoverable without a tool call.
//
// It is CHANGE-detected, like PeersReminder: the same list re-sent every turn is
// tokens spent saying nothing, and a list that carried elapsed time would change
// on every request. It speaks when the set changes — a process starts or ends.
//
// Only background processes are listed. A running sub-agent has no name-based
// action (there is no wait or stop for it), so listing it would give the model a
// handle it cannot use; its completion is reported like any other task's.
type RunningTasksReminder struct {
	Provider RunningTaskProvider

	mu sync.Mutex
	// last is what each conversation was last told, so an unchanged set stays
	// silent. Keyed by the run's logical conversation (empty is a scope of its own,
	// for the pipe entry).
	last map[string]string
}

func (r *RunningTasksReminder) Generate(_ context.Context, rctx Context) []string {
	if r.Provider == nil {
		return nil
	}
	tasks := r.Provider.RunningTasks(rctx.LogicalConversation)
	text := renderRunningTasks(tasks)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = make(map[string]string, 1)
	}
	if text == "" {
		// Nothing running: forget what was said, so the next start speaks again.
		delete(r.last, rctx.LogicalConversation)
		return nil
	}
	if r.last[rctx.LogicalConversation] == text {
		return nil
	}
	r.last[rctx.LogicalConversation] = text
	return []string{text}
}

func renderRunningTasks(tasks []RunningTask) string {
	if len(tasks) == 0 {
		return ""
	}
	sorted := append([]RunningTask(nil), tasks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	b.WriteString("Background processes still running in this conversation:")
	for _, t := range sorted {
		fmt.Fprintf(&b, "\n- %q: %s", t.Name, t.Command)
	}
	b.WriteString("\nUse wait_name to wait for one, stop_name to terminate it; a process that finishes on its own is reported to you automatically.")
	return b.String()
}
