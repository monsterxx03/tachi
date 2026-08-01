package syncx

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemaphore(t *testing.T) {
	s := NewSemaphore(2)
	ctx := context.Background()

	if !s.TryAcquire() {
		t.Fatal("TryAcquire failed with free slots")
	}
	if !s.TryAcquire() {
		t.Fatal("TryAcquire failed with free slots")
	}
	if s.TryAcquire() {
		t.Fatal("TryAcquire succeeded with full semaphore")
	}
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("TryAcquire failed after release")
	}
	s.Release()
	s.Release()

	// Acquire with canceled ctx on a full semaphore.
	s2 := NewSemaphore(1)
	s2.TryAcquire() // fill it
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s2.Acquire(cctx); err == nil {
		t.Fatal("Acquire with canceled ctx: want error")
	}
}

func TestSemaphoreWithBlocking(t *testing.T) {
	s := NewSemaphore(1)
	ctx := context.Background()
	if err := s.WithSemaphore(ctx, func() {}); err != nil {
		t.Fatalf("WithSemaphore: %v", err)
	}
	if err := s.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// Full — a second acquire must block until release.
	released := make(chan struct{})
	go func() {
		_ = s.WithSemaphore(ctx, func() {})
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("WithSemaphore ran despite full semaphore")
	case <-time.After(20 * time.Millisecond):
	}
	s.Release()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("WithSemaphore did not run after release")
	}
}

func TestNewSemaphoreNonPositive(t *testing.T) {
	s := NewSemaphore(0)
	if !s.TryAcquire() {
		t.Fatal("capacity-1 semaphore should allow one acquire")
	}
	if s.TryAcquire() {
		t.Fatal("capacity-1 semaphore should block a second acquire")
	}
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("after release should be acquirable")
	}
}

func TestGroup(t *testing.T) {
	g := NewGroup()
	var counter atomic.Int32
	for i := 0; i < 10; i++ {
		g.Go(func() { counter.Add(1) })
	}

	select {
	case <-g.Done():
		t.Fatal("Done closed before Wait")
	default:
	}

	g.Wait()
	if counter.Load() != 10 {
		t.Errorf("counter = %d, want 10", counter.Load())
	}
	select {
	case <-g.Done():
	default:
		t.Fatal("Done not closed after Wait")
	}

	// Wait is idempotent.
	g.Wait()
}

func TestSleepContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !SleepContext(ctx, time.Second) {
		t.Error("SleepContext with canceled ctx: want interrupted=true")
	}
	if SleepContext(context.Background(), 0) {
		t.Error("SleepContext(0): want interrupted=false")
	}
	if SleepContext(context.Background(), 5*time.Millisecond) {
		t.Error("SleepContext normal: want interrupted=false")
	}
}
