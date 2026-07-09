package systemreminder

import (
	"fmt"
	"strings"
)

const maxOutputSnippet = 10 * 1024 // 10KB

// BackgroundTaskProvider is the interface that ProcessManager must implement
// for DrainCompleted. Defined here to avoid importing the tools package.
type BackgroundTaskProvider interface {
	DrainCompleted() []BackgroundTaskInfo
}

// BackgroundTaskInfo is a minimal representation of a completed background
// process, returned by BackgroundTaskProvider.DrainCompleted().
type BackgroundTaskInfo struct {
	Name         string
	Command      string
	ExitCode     int
	Status       string
	Error        string
	RecentStdout string
	RecentStderr string
}

// BackgroundTaskReminder injects notifications when background processes that
// were started by the Bash tool naturally exit (not killed via stop_name).
// Fires on every Collect() call except the first message of a session.
type BackgroundTaskReminder struct {
	Provider BackgroundTaskProvider
}

func (r *BackgroundTaskReminder) Generate(ctx Context) []string {
	// No background tasks can exist before any message, so skip.
	if ctx.IsFirstMessage {
		return nil
	}
	if r.Provider == nil {
		return nil
	}

	completed := r.Provider.DrainCompleted()
	if len(completed) == 0 {
		return nil
	}

	lines := make([]string, 0, len(completed))
	for _, p := range completed {
		var b strings.Builder
		fmt.Fprintf(&b, "Background task %q finished", p.Name)
		if p.Status == "exited" {
			if p.ExitCode == 0 {
				b.WriteString(" successfully")
			} else {
				fmt.Fprintf(&b, " with exit code %d", p.ExitCode)
			}
		} else if p.Error != "" {
			fmt.Fprintf(&b, ": %s", p.Error)
		}

		// Show tail of recent output — stdout on success, stderr on failure.
		var snippet string
		if p.ExitCode == 0 && p.RecentStdout != "" {
			snippet = tailSnippet(p.RecentStdout, maxOutputSnippet)
		} else if p.ExitCode != 0 && p.RecentStderr != "" {
			snippet = tailSnippet(p.RecentStderr, maxOutputSnippet)
		}
		if snippet != "" {
			fmt.Fprintf(&b, "\n  Output:\n%s", indent(snippet, "    "))
		}

		// Keep the command so the LLM knows what finished.
		fmt.Fprintf(&b, "\n  Command: %s", p.Command)
		lines = append(lines, b.String())
	}
	return lines
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
	return "(truncated, showing tail " + formatBytes(maxLen) + ")\n" + s[start:]
}

func formatBytes(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%dKB", n/1024)
	}
	return fmt.Sprintf("%dB", n)
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
