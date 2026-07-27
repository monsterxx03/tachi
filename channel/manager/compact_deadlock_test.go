package manager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
)

// The /compact finalize path has to borrow the thread's cached agent to supply
// the memory backend for the pre-compaction write, and then evict that agent so
// the next turn reloads history from the newly-created compacted session.
//
// Those two steps do not compose naively: acquireAgent holds cachedAgent.mu for
// as long as the caller keeps it, and evictAgent takes the same lock to shut the
// agent down safely. Releasing via `defer` in the enclosing function — the
// obvious way to write it — keeps the lock held until the handler returns, so
// evictAgent blocks forever on a lock its own goroutine owns.
//
// These tests pin the ordering that makes the sequence safe.

// TestAcquireEvict_SequenceDoesNotDeadlock is the direct regression test:
// acquire, release, then evict must complete. Before the fix the evict step
// hung, taking the channel's message-handling goroutine with it.
func TestAcquireEvict_SequenceDoesNotDeadlock(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)

		// Mirror the finalize path: borrow the agent inside a scope that
		// releases the lock before eviction.
		func() {
			ca, err := mgr.acquireAgent(ctx, threadID)
			require.NoError(t, err)
			require.NotNil(t, ca.agent)
			defer mgr.releaseAgent(ca)
		}()

		mgr.evictAgent(threadID)
	}()

	select {
	case <-done:
		// Success: the sequence completed.
	case <-time.After(5 * time.Second):
		t.Fatal("acquire -> release -> evict deadlocked; evictAgent takes cachedAgent.mu and cannot proceed while the borrow still holds it")
	}

	mgr.agentCacheMu.Lock()
	_, stillCached := mgr.agentCache[threadID]
	mgr.agentCacheMu.Unlock()
	assert.False(t, stillCached, "evictAgent should have dropped the cache entry")
}

// TestEvictWhileAgentHeld_BlocksUntilReleased documents *why* the scope matters:
// evictAgent genuinely waits on cachedAgent.mu. This is the desired behaviour
// across goroutines (it prevents yanking an agent out from under a running
// turn) and is precisely what deadlocks when both happen on one goroutine.
func TestEvictWhileAgentHeld_BlocksUntilReleased(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)
	ctx := context.Background()

	ca, err := mgr.acquireAgent(ctx, threadID)
	require.NoError(t, err)

	evicted := make(chan struct{})
	go func() {
		mgr.evictAgent(threadID)
		close(evicted)
	}()

	// While the agent is held, eviction must not complete.
	select {
	case <-evicted:
		t.Fatal("evictAgent returned while the agent was still held — a running turn could have its agent closed underneath it")
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocked.
	}

	mgr.releaseAgent(ca)

	select {
	case <-evicted:
		// Expected: eviction proceeds once the lock is free.
	case <-time.After(5 * time.Second):
		t.Fatal("evictAgent did not complete after the agent was released")
	}
}

// TestFinalizeCompactResult_NilAgentStillFinalizes covers the degraded path.
// When the cached agent cannot be acquired the memory write is skipped, but the
// compact itself must still finalize — losing a memory write is acceptable,
// losing the compacted session is not.
func TestFinalizeCompactResult_NilAgentStillFinalizes(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	// Seed a session for the thread so finalize has something to compact.
	sm, _, err := mgr.loadThreadSession(threadID, mgr.resolvedConfig)
	require.NoError(t, err)
	require.True(t, sm.HasCurrent())
	oldSessionID := sm.Current().ID

	reply, err := mgr.finalizeCompactResult(threadID, "a summary of the conversation so far", nil)
	require.NoError(t, err, "finalize must succeed even without an agent for the memory write")
	assert.NotEmpty(t, reply)

	// A new session should now own the thread, linked back to the old one.
	verify := mgr.newSessionManager()
	require.NotNil(t, verify)
	_, err = verify.FindByThreadID(threadID)
	require.NoError(t, err)
	cur := verify.Current()
	require.NotNil(t, cur)
	assert.NotEqual(t, oldSessionID, cur.ID, "thread should point at the new compacted session")
	assert.Equal(t, oldSessionID, cur.CompactedParentID, "new session should link back to its parent")
}

// ---- helpers ----

func newTestManagerWithProvider(t *testing.T) *Manager {
	t.Helper()
	mgr := New(Config{
		Cfg:          config.DefaultConfig(),
		SessionStore: newTempSessionStore(t),
	})
	mgr.resolvedConfig = &config.ResolvedConfig{
		Provider: config.ResolvedProvider{
			Type:          "openai",
			Model:         "test-model",
			ContextWindow: 128_000,
		},
		MaxTokens: 4096,
	}
	mgr.provider = &mockProvider{name: "mock"}
	return mgr
}

func uniqueThreadID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("thread-%s-%d", t.Name(), time.Now().UnixNano())
}
