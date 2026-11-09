package systemreminder

import (
	"context"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

const maxOutputSnippet = 10 * 1024 // 10KB

// BackgroundTaskReminder injects notifications when background processes that
// were started by the Bash tool finish on their own (a stopped one is not
// reported: the caller asked for it).
//
// It is one of the queue's two readers. This one serves a turn that is already
// in flight — the collector runs before every request — while the entry point
// wakes an idle conversation from the same queue. Taking is destructive, so the
// two never tell the model the same thing twice, and whichever gets there first
// owns the telling.
//
// Fires on every Collect() call except the first message of a session.
type BackgroundTaskReminder struct {
	Queue *tasks.Queue
}

func (r *BackgroundTaskReminder) Generate(ctx context.Context, rctx Context) []string {
	// No background tasks can exist before any message, so skip.
	if rctx.IsFirstMessage {
		return nil
	}
	if r.Queue == nil {
		return nil
	}

	// Filtered by the run's own conversation: one agent may host several (the
	// TUI keeps one across sessions), and a task from another conversation is not
	// this turn's news.
	completed := r.Queue.PendingFor(rctx.LogicalConversation)
	if len(completed) == 0 {
		return nil
	}
	return TaskLines(completed)
}

// TaskLines renders finished tasks for the model. It is shared by the in-turn
// reminder and the wake path, so a result reads the same whether it arrived in
// the middle of a turn or started one of its own.
func TaskLines(events []tasks.Event) []string {
	lines := make([]string, 0, len(events))
	for _, p := range events {
		var b strings.Builder
		if p.Status == tasks.StatusWaiting {
			fmt.Fprintf(&b, "%s %q is waiting for an answer", taskSubject(p.Kind), p.Name)
			if p.QuestionID != "" {
				fmt.Fprintf(&b, " (question_id=%s)", p.QuestionID)
			}
			if p.Kind == tasks.KindRemote {
				fmt.Fprintf(&b, " — answer with RemoteTask(action=\"reply\", task_id=%q, question_id=%q, answer=…)", p.Name, p.QuestionID)
			}
		} else {
			fmt.Fprintf(&b, "%s %q finished", taskSubject(p.Kind), p.Name)
			if p.Status == tasks.StatusExited {
				if p.ExitCode == 0 {
					b.WriteString(" successfully")
				} else {
					fmt.Fprintf(&b, " with exit code %d", p.ExitCode)
				}
			} else if p.Error != "" {
				fmt.Fprintf(&b, ": %s", p.Error)
			}
		}

		// Show tail of recent output — stdout on success, stderr on failure. A
		// sub-agent has no stderr of its own: its text IS the result (including the
		// partial result of a failed one), so that is what gets shown.
		var snippet string
		switch {
		case p.Kind == tasks.KindBackground && p.ExitCode == 0 && p.Stdout != "":
			snippet = tailSnippet(p.Stdout, maxOutputSnippet)
		case p.Kind == tasks.KindBackground && p.ExitCode != 0 && p.Stderr != "":
			snippet = tailSnippet(p.Stderr, maxOutputSnippet)
		case p.Kind != tasks.KindBackground && p.Stdout != "":
			snippet = tailSnippet(p.Stdout, maxOutputSnippet)
		}
		if snippet != "" {
			fmt.Fprintf(&b, "\n  Output:\n%s", indent(snippet, "    "))
		}

		if p.Node != "" {
			fmt.Fprintf(&b, "\n  Node: %s", p.Node)
		}

		// Keep what the task was asked to do, so the LLM knows what finished.
		if p.Command != "" {
			label := "Command"
			if p.Kind != tasks.KindBackground {
				label = "Task"
			}
			fmt.Fprintf(&b, "\n  %s: %s", label, p.Command)
		}
		lines = append(lines, b.String())
	}
	return lines
}

// taskSubject is the noun a task's sentence starts with.
func taskSubject(k tasks.Kind) string {
	switch k {
	case tasks.KindSubagent:
		return "Sub-agent"
	case tasks.KindRemote:
		return "Remote task"
	default:
		return "Background task"
	}
}

// tailSnippet returns the last maxLen bytes of s, prefixed with a note if
// the content was truncated.
func tailSnippet(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Find a newline near the truncation point for cleaner cuts.
	start := len(s) - maxLen
	if i := strings.IndexByte(s[start:], '\n'); i >= 0 && i < maxLen/2 {
		start += i + 1
	}
	return "(truncated, showing tail " + strutil.HumanBytes(int64(maxLen)) + ")\n" + s[start:]
}

func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
