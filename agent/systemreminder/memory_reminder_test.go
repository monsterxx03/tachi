package systemreminder

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
)

// stubMemoryBackend is a memory.Backend that returns one entry, so Generate exercises the
// whole path (recall → reinforce → render) without any storage.
type stubMemoryBackend struct{}

func (stubMemoryBackend) Store(context.Context, string) error { return nil }

func (stubMemoryBackend) Recall(context.Context, string, int) ([]memory.Entry, error) {
	return []memory.Entry{{ID: "fact-1", Content: "a remembered thing", Timestamp: time.Now().Unix()}}, nil
}

func (stubMemoryBackend) ReinforceFact(context.Context, string) error { return nil }

// TestMemoryRecallReminder_SwapBackendIsSafeUnderConcurrentGenerate is the race the reload
// path creates: the settings goroutine swaps what the reminder points at (memory.* saved)
// while the turn goroutine generates. A torn interface read — or a Generate that mixed a new
// backend with the old policy — is what the lock exists to prevent; run this with -race.
func TestMemoryRecallReminder_SwapBackendIsSafeUnderConcurrentGenerate(t *testing.T) {
	r := &MemoryRecallReminder{}
	r.SetPolicy(5, 0)
	r.SetBackend(stubMemoryBackend{})

	rctx := Context{CurrentPrompt: "what did we talk about?"}
	const rounds = 300

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range rounds {
			if i%2 == 0 {
				r.SetBackend(nil) // memory turned off mid-conversation
				continue
			}
			r.SetPolicy(i, time.Millisecond)
			r.SetBackend(stubMemoryBackend{})
		}
	}()

	for range rounds {
		// nil and non-nil backends alternate, so both the early return and the full
		// path are exercised against the concurrent writes.
		_ = r.Generate(t.Context(), rctx)
	}
	wg.Wait()

	// The reminder must still be usable, and the policy it reports must be the last one set.
	r.SetBackend(stubMemoryBackend{})
	if got := r.Generate(t.Context(), rctx); len(got) == 0 {
		t.Error("no output after the concurrent swaps — the reminder is unusable")
	}
}

// TestMemoryRecallReminder_BackendAccessorAndPolicy pins the small read/set API the reload
// path relies on (ReloadMemory sets both; the reminder reports what is in effect).
func TestMemoryRecallReminder_BackendAccessorAndPolicy(t *testing.T) {
	r := &MemoryRecallReminder{}
	if r.Backend() != nil {
		t.Error("a fresh reminder must report no backend")
	}
	b := stubMemoryBackend{}
	r.SetBackend(b)
	if r.Backend() == nil {
		t.Error("SetBackend did not take")
	}
	r.SetPolicy(3, 2*time.Second)
	if _, limit, timeout := r.snapshot(); limit != 3 || timeout != 2*time.Second {
		t.Errorf("snapshot() = (%d, %v), want (3, 2s)", limit, timeout)
	}
}
