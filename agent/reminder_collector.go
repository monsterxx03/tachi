package agent

import (
	"context"

	"github.com/monsterxx03/tachi/agent/systemreminder"
)

// ReminderCollector collects system reminders for the agent loop.
//
// Separating this from the concrete *systemreminder.Collector lets tests
// inject a lightweight fake that returns a controlled reminder block and
// tracks collection calls for assertion.
type ReminderCollector interface {
	// Collect gathers active reminders and formats them into a single
	// <system-reminder>... block. Returns an empty string when no
	// reminders are active.
	Collect(ctx context.Context, rctx systemreminder.Context) string

	// CollectPieces is the same collection, one Piece per reminder that fired.
	// The agent loop uses it to append only what changed since its last
	// injection instead of repeating the whole block every iteration.
	CollectPieces(ctx context.Context, rctx systemreminder.Context) []systemreminder.Piece

	// AddReminder appends a reminder to the collector.
	AddReminder(r systemreminder.Reminder)
}

// disabledReminderCollector is a no-op ReminderCollector used by
// non-interactive modes (e.g. `tachi -p`) that want zero system reminders.
// Collect always returns an empty string, and AddReminder is a no-op so
// later registrations (LSP diagnostics, deferred MCP tools, background
// tasks) stay inert without panicking on a nil collector.
type disabledReminderCollector struct{}

func (disabledReminderCollector) Collect(context.Context, systemreminder.Context) string {
	return ""
}

func (disabledReminderCollector) CollectPieces(context.Context, systemreminder.Context) []systemreminder.Piece {
	return nil
}

func (disabledReminderCollector) AddReminder(systemreminder.Reminder) {}
