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

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// workDir is the directory a contextual reminder describes: the TURN's working
// directory (wdctx), never the process's.
//
// It has to come from the context because a reminder is generated per turn inside a
// process that may host many sessions in different trees — and because a GUI's own
// working directory is meaningless (macOS hands a Finder-launched app "/", so a
// process-cwd reminder would describe the filesystem root). Single-session
// frontends are unaffected: wdctx falls back to the process working directory, which
// for the CLI/TUI is the directory it was started in.
//
// Any reminder that reads a file or runs a command relative to a project (project
// context, git status, plan tracking) must go through this instead of os.Getwd.
func workDir(ctx context.Context) string {
	return wdctx.Dir(ctx)
}

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

// Piece is one reminder's contribution to a block, kept separate from the rest so a
// caller can tell what actually changed since the last injection. The agent loop
// re-injects its reminder block after every tool round; without this, an unchanged
// reminder is repeated verbatim into a growing context (tokens spent, nothing learned).
type Piece struct {
	// Name is the reminder's type name — stable within a build, which is all the
	// dedup needs.
	Name string
	// Lines are what the reminder generated, in registration order.
	Lines []string
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

// CollectPieces queries every registered reminder and returns one Piece per reminder
// that fired, in registration order. Empty when none did (or c is nil).
func (c *Collector) CollectPieces(ctx context.Context, rctx Context) []Piece {
	if c == nil {
		return nil
	}

	var pieces []Piece
	var firedName string

	for _, r := range c.reminders {
		generated := r.Generate(ctx, rctx)
		if len(generated) == 0 {
			continue
		}
		name := fmt.Sprintf("%T", r)
		pieces = append(pieces, Piece{Name: name, Lines: generated})
		if firedName == "" {
			firedName = name
		} else {
			firedName += ", " + name
		}
	}
	if len(pieces) == 0 {
		return nil
	}

	rctx.Info(ctx, "systemreminder: firing reminder(s)", "names", firedName)
	return pieces
}

// RenderPieces wraps pieces into the single <system-reminder> block every consumer
// expects. Empty input renders "", so "nothing to say" and "say nothing" stay the same
// thing.
func RenderPieces(pieces []Piece) string {
	if len(pieces) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	for _, p := range pieces {
		for _, line := range p.Lines {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	sb.WriteString("</system-reminder>\n")
	return sb.String()
}

// Collect returns the block as one string: CollectPieces plus RenderPieces. Callers
// with nothing to compare against (a turn's first injection, a one-off run) want this
// shape; the loop uses the pieces.
func (c *Collector) Collect(ctx context.Context, rctx Context) string {
	return RenderPieces(c.CollectPieces(ctx, rctx))
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
