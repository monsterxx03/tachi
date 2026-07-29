package agent

import (
	"context"
	"sync"

	"github.com/monsterxx03/tachi/agent/systemreminder"
)

// fakeReminderCollector is an in-memory ReminderCollector for testing.
// It returns a preset block and records every Collect call for assertion.
type fakeReminderCollector struct {
	mu          sync.Mutex
	block       string                  // preset output for Collect()
	callCount   int                     // how many times Collect() was called
	lastRctx    systemreminder.Context  // the most recent context passed to Collect()
}

func (f *fakeReminderCollector) Collect(ctx context.Context, rctx systemreminder.Context) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.lastRctx = rctx
	return f.block
}

// AddReminder is a no-op for the fake. Tests inject a pre-configured collector
// that produces the exact output they test against.
func (f *fakeReminderCollector) AddReminder(_ systemreminder.Reminder) {}

// CallCount returns how many times Collect was called.
func (f *fakeReminderCollector) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// LastContext returns the most recent context passed to Collect.
func (f *fakeReminderCollector) LastContext() systemreminder.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRctx
}
