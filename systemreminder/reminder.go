// Package systemreminder provides a mechanism for injecting dynamic
// <system-reminder> tags at the top of user messages. These carry
// transient contextual information (current date, token usage warnings,
// iteration budget) that's not suitable for the static system prompt.
package systemreminder

import (
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// Context carries the dynamic state available when generating reminders.
// All fields are zero-valued when not applicable; individual reminders
// decide which fields to examine.
type Context struct {
	// IsFirstMessage is true when this is the first user message in a
	// brand-new conversation (no prior messages except the system prompt).
	IsFirstMessage bool

	// IterationsLeft is the number of agent-loop iterations remaining.
	// Zero means the budget is exactly exhausted.
	IterationsLeft int

	// MaxIterations is the configured iteration budget ceiling.
	MaxIterations int

	// InputTokens is the cumulative input token count from the most recent
	// API response (includes all prior messages in the context window).
	InputTokens int64

	// ContextWindow is the model's maximum context window size.
	ContextWindow int64

	// Now is the current time, provided by the caller so tests can
	// inject a deterministic clock.
	Now time.Time
}

// Reminder generates one or more reminder lines given the current context.
// It returns nil or an empty slice when no reminder is needed.
type Reminder interface {
	Generate(ctx Context) []string
}

// Collector aggregates a set of Reminders and formats active ones into a
// single <system-reminder>...</system-reminder> block.
type Collector struct {
	reminders []Reminder
}

// NewCollector creates a Collector that consults the given reminders.
// When called with no arguments the collector always produces empty output.
func NewCollector(reminders ...Reminder) *Collector {
	return &Collector{reminders: reminders}
}

// Collect queries every registered reminder and, if any produce output,
// wraps the combined lines in <system-reminder> tags.
// Returns an empty string when no reminders are active.
func (c *Collector) Collect(ctx Context) string {
	var parts []string
	for _, r := range c.reminders {
		parts = append(parts, r.Generate(ctx)...)
	}
	if len(parts) == 0 {
		return ""
	}
	block := "<system-reminder>\n" + strings.Join(parts, "\n") + "\n</system-reminder>"
	debuglog.Log("systemreminder: injecting %d reminder(s):\n%s", len(parts), block)
	return block
}

// WrapUserMessage prepends the <system-reminder> block (if any) to the
// given user message content. This is a convenience helper so callers
// don't need to manually check emptiness.
func (c *Collector) WrapUserMessage(userMessage string, ctx Context) string {
	block := c.Collect(ctx)
	if block == "" {
		return userMessage
	}
	return block + "\n" + userMessage
}

// ---- Built-in reminders -----------------------------------------------------

// DateReminder injects the current date on the first message of a brand-new
// conversation. This gives the model temporal awareness without hard-coding
// a date in the system prompt (which would go stale across long sessions).
type DateReminder struct{}

func (DateReminder) Generate(ctx Context) []string {
	if !ctx.IsFirstMessage {
		return nil
	}
	line := fmt.Sprintf("Current date: %s", ctx.Now.Format("Monday, January 2, 2006"))
	debuglog.Log("systemreminder: DateReminder firing: %q", line)
	return []string{line}
}

// IterationWarningReminder warns when the agent loop is running low on
// iterations so the model knows to finish its work efficiently.
type IterationWarningReminder struct{}

func (IterationWarningReminder) Generate(ctx Context) []string {
	if ctx.MaxIterations <= 0 || ctx.IterationsLeft <= 0 {
		return nil
	}
	// Only warn when there are 2 or fewer iterations left.
	if ctx.IterationsLeft > 2 {
		return nil
	}
	line := fmt.Sprintf(
		"Iteration budget: %d of %d iterations remaining. Complete your work as efficiently as possible.",
		ctx.IterationsLeft, ctx.MaxIterations,
	)
	debuglog.Log("systemreminder: IterationWarningReminder firing: %q", line)
	return []string{line}
}

// TokenWarningReminder warns when the input token count exceeds 60% of the
// context window, so the model can adjust its output verbosity.
type TokenWarningReminder struct{}

func (TokenWarningReminder) Generate(ctx Context) []string {
	if ctx.ContextWindow <= 0 || ctx.InputTokens <= 0 {
		return nil
	}
	pct := float64(ctx.InputTokens) / float64(ctx.ContextWindow)
	if pct < 0.6 {
		return nil
	}
	line := fmt.Sprintf(
		"Context window usage: %.0f%% (%d / %d input tokens). Be concise and minimize unnecessary output.",
		pct*100, ctx.InputTokens, ctx.ContextWindow,
	)
	debuglog.Log("systemreminder: TokenWarningReminder firing: %q", line)
	return []string{line}
}
