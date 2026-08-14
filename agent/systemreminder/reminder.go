// Package systemreminder provides a mechanism for injecting dynamic
// <system-reminder> tags at the top of user messages. These carry
// transient contextual information (current date, token usage warnings,
// iteration budget) that's not suitable for the static system prompt.
package systemreminder

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// Context carries the dynamic state available when generating reminders.
// All fields are zero-valued when not applicable; individual reminders
// decide which fields to examine.
type Context struct {
	// IsFirstMessage is true when this is the first user message in a
	// brand-new conversation (no prior messages except the system prompt).
	IsFirstMessage bool

	// Now is the current time, provided by the caller so tests can
	// inject a deterministic clock.
	Now time.Time

	// LastMessageDate is the calendar date (YYYY-MM-DD) of the most recent
	// user message that was processed. It's empty for brand-new conversations.
	// Used by reminders that need to know when a new day has started.
	LastMessageDate string

	// IsToolResult is true when the reminder block is being injected after
	// tool results in the agent loop (not attached to a real user message).
	// Reminders that are only meaningful for user-facing messages (e.g.,
	// DateReminder) can skip when this is set.
	IsToolResult bool

	// CurrentPrompt is the current user input. Set by WrapUserMessage before
	// calling Collect so MemoryRecallReminder can use it as a search query.
	CurrentPrompt string

	// SkipRecall prevents memory recall (e.g. "tachi run" non-interactive mode).
	SkipRecall bool

	// ToolNames lists the names of tools that were executed in the current
	// turn (in order of execution). Empty when the reminder is not being
	// injected at a tool-result boundary. Reminders that only apply to
	// specific tool invocations can inspect this field.
	ToolNames []string

	// SessionID is the current session's ID. Used by PlanTrackingReminder
	// to filter plan files belonging to the current session.
	SessionID string

	// Logger is the logger for this reminder generation cycle.
	// When nil, uses logger.Default().
	Logger *logger.Logger
}

// Reminder generates one or more reminder lines given the current context.
// It returns nil or an empty slice when no reminder is needed.
type Reminder interface {
	Generate(ctx context.Context, rctx Context) []string
}

// Collector aggregates a set of Reminders and formats active ones into a
// single <system-reminder>...</system-reminder> block. All reminders share
// the same wrapper tag so downstream consumers (message stripping, session
// parsing, model prompts) only ever need to handle one tag shape.
type Collector struct {
	reminders []Reminder
}

// NewCollector creates a Collector that consults the given reminders.
// When called with no arguments the collector always produces empty output.
func NewCollector(reminders ...Reminder) *Collector {
	return &Collector{reminders: reminders}
}

// AddReminder appends a reminder to the collector without rebuilding.
func (c *Collector) AddReminder(r Reminder) {
	c.reminders = append(c.reminders, r)
}

// Collect queries every registered reminder, concatenates their output in
// registration order, and wraps it in a single <system-reminder> block.
// Returns an empty string when no reminders are active or c is nil.
func (c *Collector) Collect(ctx context.Context, rctx Context) string {
	if c == nil {
		return ""
	}

	var sb strings.Builder
	var lines []string
	var firedName string

	for _, r := range c.reminders {
		generated := r.Generate(ctx, rctx)
		if len(generated) == 0 {
			continue
		}
		lines = append(lines, generated...)
		if firedName == "" {
			firedName = fmt.Sprintf("%T", r)
		} else {
			firedName += ", " + fmt.Sprintf("%T", r)
		}
	}
	if len(lines) == 0 {
		return ""
	}

	rctx.Info(ctx, "systemreminder: firing reminder(s)", "names", firedName)
	sb.WriteString("<system-reminder>\n")
	for _, line := range lines {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

// WrapUserMessage prepends the <system-reminder> block (if any) to the
// given user message content. This is a convenience helper so callers
// don't need to manually check emptiness. Safe to call on a nil Collector.
func (c *Collector) WrapUserMessage(ctx context.Context, userMessage string, rctx Context) string {
	if c == nil {
		return userMessage
	}
	// Inject current prompt so MemoryRecallReminder can use it as search query
	rctx.CurrentPrompt = userMessage
	block := c.Collect(ctx, rctx)
	if block == "" {
		return userMessage
	}
	return block + userMessage
}

// Info logs at INFO level through the context's logger, falling back to Default().
func (c Context) Info(ctx context.Context, msg string, attrs ...any) {
	l := c.Logger
	if l == nil {
		l = logger.Default()
	}
	l.Info(ctx, msg, attrs...)
}

// Error logs at ERROR level through the context's logger, falling back to Default().
func (c Context) Error(ctx context.Context, msg string, err error, attrs ...any) {
	l := c.Logger
	if l == nil {
		l = logger.Default()
	}
	l.Error(ctx, msg, err, attrs...)
}

// Warn logs at WARN level through the context's logger, falling back to Default().
func (c Context) Warn(ctx context.Context, msg string, attrs ...any) {
	l := c.Logger
	if l == nil {
		l = logger.Default()
	}
	l.Warn(ctx, msg, attrs...)
}
