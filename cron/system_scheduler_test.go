package cron

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSystemScheduler_RegisterAndFire(t *testing.T) {
	ss := NewSystemScheduler(SystemSchedulerConfig{})

	var fired atomic.Int32

	// Use @every 200ms for test.
	err := ss.Register("test-job", "@every 200ms", 5*time.Second, func(ctx context.Context) error {
		fired.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ss.Start(ctx)
	defer ss.Stop()

	// Poll until at least one fire, with generous deadline.
	deadline := time.After(2 * time.Second)
	for {
		if fired.Load() >= 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for fire; got %d", fired.Load())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestSystemScheduler_RegisterAfterStart(t *testing.T) {
	ss := NewSystemScheduler(SystemSchedulerConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ss.Start(ctx)
	defer ss.Stop()

	err := ss.Register("late-job", "@every 1s", 5*time.Second, func(ctx context.Context) error {
		return nil
	})
	if err != ErrAlreadyStarted {
		t.Errorf("expected ErrAlreadyStarted, got %v", err)
	}
}

func TestSystemScheduler_ContextCancellation(t *testing.T) {
	ss := NewSystemScheduler(SystemSchedulerConfig{})

	var ctxErr atomic.Value

	err := ss.Register("ctx-job", "@every 50ms", 100*time.Millisecond, func(ctx context.Context) error {
		// This job just records whether its context is done.
		<-ctx.Done()
		ctxErr.Store(ctx.Err())
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ss.Start(ctx)

	// Let the job fire.
	time.Sleep(80 * time.Millisecond)

	// Cancel the parent context — should propagate to jobs.
	cancel()
	ss.Stop()

	// Verify context cancellation propagated.
	if v := ctxErr.Load(); v != nil {
		if v.(error) != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", v)
		}
	}
}

func TestSystemScheduler_InvalidSchedule(t *testing.T) {
	ss := NewSystemScheduler(SystemSchedulerConfig{})

	err := ss.Register("bad-job", "not a valid cron", 5*time.Second, func(ctx context.Context) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for invalid schedule")
	}
}
