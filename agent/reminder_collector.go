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

	// AddReminder appends a reminder to the collector.
	// The fake implementation (fakeReminderCollector) is a no-op, which is
	// safe because tests inject pre-configured collectors that produce the
	// exact output they want to test.
	AddReminder(r systemreminder.Reminder)
}
