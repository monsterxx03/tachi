// Package syncx provides shared concurrency primitives used across tachi:
// semaphores, background goroutine groups with completion signaling, and
// cancel-aware sleeps.
package syncx

import (
	"context"
	"sync"
	"time"
)

// Semaphore is a counting semaphore backed by a buffered channel.
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore returns a semaphore allowing at most n concurrent holders.
// A non-positive n yields a semaphore of capacity 1.
func NewSemaphore(n int) *Semaphore {
	if n <= 0 {
		n = 1
	}
	return &Semaphore{ch: make(chan struct{}, n)}
}

// Acquire blocks until a slot is free or ctx is canceled.
func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire attempts to acquire without blocking. Returns false if all
// slots are taken.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns one slot. Acquire must have been called first.
func (s *Semaphore) Release() {
	<-s.ch
}

// Len returns the number of slots currently held.
func (s *Semaphore) Len() int {
	return len(s.ch)
}

// WithSemaphore runs fn holding one slot, releasing it on return.
// Returns ctx.Err() if acquisition failed.
func (s *Semaphore) WithSemaphore(ctx context.Context, fn func()) error {
	if err := s.Acquire(ctx); err != nil {
		return err
	}
	defer s.Release()
	fn()
	return nil
}

// Group manages a set of background goroutines and signals when all have
// finished. It embeds sync.WaitGroup — so Add/Done/Go come from the standard
// library (Go 1.21+) — and adds a one-shot "all done" channel closed by Wait.
type Group struct {
	sync.WaitGroup
	done chan struct{}
	once sync.Once
}

// NewGroup returns an empty Group.
func NewGroup() *Group {
	return &Group{done: make(chan struct{})}
}

// Wait blocks until all goroutines started via Go have finished, then closes
// the channel returned by Done. Calling Wait before any Go is fine.
func (g *Group) Wait() {
	g.WaitGroup.Wait()
	g.once.Do(func() { close(g.done) })
}

// Done returns a channel that is closed once Wait has completed (i.e. all
// goroutines have exited).
func (g *Group) Done() <-chan struct{} {
	return g.done
}

// SleepContext sleeps for d, returning true if the sleep was cut short by
// ctx cancellation. A non-positive d returns immediately.
func SleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
