// Package retry provides shared exponential-backoff and cancel-aware sleep
// helpers used by provider wrappers, LSP calls, and channel retry loops.
package retry

import (
	"context"
	"time"
)

// Backoff implements exponential backoff: attempt 1 yields BaseDelay, and
// each further attempt doubles the delay, capped at MaxDelay.
type Backoff struct {
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// Delay returns the backoff delay for the given 1-based attempt.
func (b Backoff) Delay(attempt int) time.Duration {
	d := b.BaseDelay
	if attempt > 1 {
		d = b.BaseDelay << (attempt - 1)
		if d <= 0 { // shift overflow — clamp to max
			return b.MaxDelay
		}
	}
	if d > b.MaxDelay {
		return b.MaxDelay
	}
	return d
}

// Sleep sleeps for d, returning early with ctx.Err() if ctx is canceled.
// A non-positive d returns immediately.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
