package systemreminder

import "fmt"

// DateReminder injects the current date and time (precise to seconds) on every
// user message. This keeps the model temporally aware without hard-coding a
// date in the system prompt (which would go stale across long sessions).
type DateReminder struct{}

func (DateReminder) Generate(ctx Context) []string {
	// DateReminder is only meaningful for real user messages — skip when
	// the reminder block is injected after tool results in the agent loop.
	if ctx.IsToolResult {
		return nil
	}
	line := fmt.Sprintf("Current date: %s", ctx.Now.Format("Monday, January 2, 2006 15:04:05 MST"))
	return []string{line}
}
