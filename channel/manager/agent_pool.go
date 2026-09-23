package manager

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

// cachedAgent wraps a per-thread AIAgent with serialization (so a single
// thread never runs two turns concurrently against the same agent state)
// and bookkeeping fields used to detect when the cached agent must be
// rebuilt (e.g. after /model switches the active provider).
type cachedAgent struct {
	mu      sync.Mutex
	agent   *agent.AIAgent
	evicted atomic.Bool // set BEFORE the agent is closed; a waiter on mu must not adopt a doomed slot

	providerName string // currentProviderName when the agent was built

	// workDir is the working directory for this thread. All tools (Bash, Read, Write, Edit, Glob,
	// etc.) resolve relative paths against it. Initialized to os.Getwd() on first agent creation;
	// updated by /cd.
	//
	// It is the ONE field two different lifecycles write: a turn (which holds ca.mu for its whole
	// duration) reads it at the start and may write it back from the session's persisted value,
	// while /cd — a stateless command that must NOT block on a running turn — writes it directly.
	// Guarding it with ca.mu would make /cd wait out the turn; guarding it with the pool's map lock
	// would be a lie (the map lock protects the MAP). So it carries its own lock, and that lock is a
	// LEAF: nothing is taken while holding it, and nothing is held while taking it — pool.mu is
	// released before it (pool.workDir takes the map lock, then this one) and ca.mu may be held
	// above it on the turn path, which is fine because this lock is a field lock and the other two
	// are structure locks.
	wdMu    sync.Mutex
	workDir string

	// history caches the full LLM message slice (system prompt + all prior
	// turns) so each new turn can pass it directly to RunConversationStream
	// without reloading from disk. Updated after every completed turn via
	// agent.GetLastMessages(). Nil until the first turn completes (or after
	// an eviction), at which point prepareThreadSession loads from disk once.
	history []llm.Message
}

// WorkDir reports this thread's working directory.
func (ca *cachedAgent) WorkDir() string {
	ca.wdMu.Lock()
	defer ca.wdMu.Unlock()
	return ca.workDir
}

// SetWorkDir replaces this thread's working directory. See the field's comment for why this is a
// lock of its own rather than the turn lock.
func (ca *cachedAgent) SetWorkDir(dir string) {
	ca.wdMu.Lock()
	defer ca.wdMu.Unlock()
	ca.workDir = dir
}

// agentPool is the per-thread agent cache: ONE long-lived *AIAgent per ThreadID, so the state that
// is worth accumulating across a thread's turns (skill activation, the MCP discovered set, reminder
// cadence) survives between them, while threads stay isolated from each other.
//
// The concurrency model belongs to this type. It used to be a comment on one method while three
// other files reached into the map and its lock by hand, each carrying its own note ("Fetch provider
// name outside the lock to avoid lock ordering", "Lock ordering: agentCacheMu → ca.mu").
//
// Three guards, and EVERY one of them is taken on its own — there is no nesting order to get wrong:
//
//   - pool.mu protects the MAP, and nothing else. Never held across a call into an agent.
//   - cachedAgent.mu serializes the turns of one thread. Held for a whole turn.
//   - cachedAgent.evicted is an atomic flag: the slot is dead. evict sets it BEFORE it closes the
//     agent, which is what lets a waiter that already holds mu refuse the slot instead of adopting
//     one that is about to be shut down.
//
// The earlier design did nest them (evict took the map lock then the slot lock; acquire took the
// slot lock and then went BACK for the map lock to re-check that its slot was still current), which
// is an ABBA deadlock: a thread acquiring T while another evicts T hangs both. It was latent for as
// long as the cache existed — the tests reached it in seconds once they ran acquire and evict
// concurrently instead of one after the other (TestAgentPool_ConcurrentAcquireEvictDoesNotDeadlock).
//
// A dead slot is a TOMBSTONE: still in the map when a build failed, with the flag set — and it never
// has an agent (a slot that DOES have one leaves the map when it is marked dead: detach, evictAll and
// phase 1 all delete it in the same critical section that marks it). slot() and ensure() therefore
// treat an evicted entry as absent, which is what keeps a tombstone out of every reader's way: /cd
// creates a LIVE slot instead of writing a directory into a dead one, and getThreadWorkDir falls back
// to the session instead of answering with a value nothing will ever use. Phase 1 of acquire replaces
// the entry, so tombstones are cleared by the next caller for that thread; count() and each() may
// still see one for a moment (each() hands it to a callback, which goes through withAgent and does
// nothing).
//
// Nothing outside this file holds any of the three; every caller goes through a method here. In
// particular, commands do NOT take ca.mu themselves — they reach an agent through withAgent, which is
// what keeps "no guard is ever held outside this file" true rather than aspirational.
type agentPool struct {
	mu    sync.Mutex
	slots map[string]*cachedAgent
}

func newAgentPool() *agentPool {
	return &agentPool{slots: make(map[string]*cachedAgent)}
}

// slot returns the entry for a thread, or nil when it has none or the one it has is dead (a
// tombstone). It never creates one: callers that only read bookkeeping (a workDir, an estimate) must
// not turn a stateless command into a cache entry. The returned entry may be evicted a moment later —
// that is safe: the pointer stays valid, and evict only closes an agent whose slot lock it could take.
func (p *agentPool) slot(threadID string) *cachedAgent {
	p.mu.Lock()
	defer p.mu.Unlock()
	ca, ok := p.slots[threadID]
	if !ok || ca.evicted.Load() {
		return nil
	}
	return ca
}

// ensure returns the entry for a thread, creating it with make() when absent or dead. make runs UNDER
// the lock, so it must not call back into the Manager (a provider lookup takes another lock, and
// pool.mu → anything else is an order nothing here defines): build the value first and let the
// closure only allocate.
//
// A tombstone is replaced rather than returned: otherwise a /cd would write this thread's working
// directory into a slot that acquire is going to throw away, and the command would report success
// while the pool silently kept the old answer.
func (p *agentPool) ensure(threadID string, make func() *cachedAgent) *cachedAgent {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ca, ok := p.slots[threadID]; ok && !ca.evicted.Load() {
		return ca
	}
	ca := make()
	p.slots[threadID] = ca
	return ca
}

// acquire returns the thread's entry with its LONG lock held, building the agent when the entry has
// none. The caller MUST hand it back with release.
//
// The retry loop is the price of not nesting: the entry is looked up under the short lock, the long
// lock is taken outside it, and the entry is re-checked — it may have been evicted (by /new) while
// this caller waited for a turn to finish. build() runs with the long lock held, so a second caller
// arriving mid-build waits instead of building a rival agent.
func (p *agentPool) acquire(ctx context.Context, threadID, providerName string, build func() (*agent.AIAgent, error)) (*cachedAgent, error) {
	for {
		// Phase 1, map only: adopt the current slot, replacing one that is dead (evicted, or built
		// for a provider this thread no longer uses). The doomed slot may still be in use by a turn
		// that has not released its lock, and this phase holds no agent lock of its own — so its
		// shutdown is handed to a goroutine that waits for that turn to finish. It has to happen
		// HERE: the slot leaves the map in the lines below, and evict/evictAll can only close what
		// is still in it, so nobody else will ever close this agent (its Close is what kills the
		// processes a turn left behind).
		p.mu.Lock()
		if p.slots == nil {
			p.slots = make(map[string]*cachedAgent)
		}
		ca, ok := p.slots[threadID]
		if ok && (ca.evicted.Load() || ca.providerName != providerName) {
			ca.evicted.Store(true) // set BEFORE the close: a waiter must refuse this slot
			delete(p.slots, threadID)
			go ca.close()
			ok = false
		}
		if !ok {
			ca = &cachedAgent{providerName: providerName, workDir: initialWorkDir()}
			p.slots[threadID] = ca
		}
		p.mu.Unlock()

		// Phase 2, this slot only. Nothing is taken while mu is held, and mu is never taken while
		// holding pool.mu, so a concurrent evict of this very thread can only wait — never cycle.
		ca.mu.Lock()
		if ca.evicted.Load() {
			// Evicted while this caller waited for the turn lock. Let go and let phase 1 make a
			// fresh slot rather than hand out one whose agent is being closed.
			ca.mu.Unlock()
			continue
		}

		if ca.agent == nil {
			built, err := build()
			if err != nil {
				// Roll back: mark the slot dead so the next attempt starts fresh. It stays in the
				// map as a tombstone until then (phase 1) — removing it here would mean taking
				// pool.mu with mu held, which is the pairing this design exists to avoid.
				ca.evicted.Store(true)
				ca.mu.Unlock()
				return nil, err
			}
			ca.agent = built
		}
		return ca, nil
	}
}

// release unlocks an entry taken by acquire. A nil entry is a no-op, so callers can defer it
// unconditionally.
func (p *agentPool) release(ca *cachedAgent) {
	if ca == nil {
		return
	}
	ca.mu.Unlock()
}

// evict drops one thread's agent, and reports whether there was one. Called by /new (the thread's
// session is ending) and by callers forcing a rebuild on the next turn.
//
// The map is touched first and the agent closed after, with NO lock held in between: marking the
// slot dead (the atomic flag) is what makes the slot refusable to a waiter, so the close can be
// slow — it kills processes — without blocking any other thread.
func (p *agentPool) evict(threadID string) bool {
	ca := p.detach(threadID)
	if ca == nil {
		return false
	}
	ca.close()
	return true
}

// evictAll drops every cached agent, returning how many there were. Called when the active provider
// changes via /model, so the next message rebuilds against the new config.
func (p *agentPool) evictAll() int {
	p.mu.Lock()
	detached := make([]*cachedAgent, 0, len(p.slots))
	for threadID, ca := range p.slots {
		ca.evicted.Store(true)
		detached = append(detached, ca)
		delete(p.slots, threadID)
	}
	p.mu.Unlock()

	for _, ca := range detached {
		ca.close()
	}
	return len(detached)
}

// detach removes one thread's slot and marks it dead, or returns nil when it had none. The caller
// owns the closing.
func (p *agentPool) detach(threadID string) *cachedAgent {
	p.mu.Lock()
	defer p.mu.Unlock()
	ca, ok := p.slots[threadID]
	if !ok {
		return nil
	}
	ca.evicted.Store(true)
	delete(p.slots, threadID)
	return ca
}

// close shuts an agent down, waiting for a turn in flight to finish first: the slot is already
// marked dead, so nobody new picks it up.
func (ca *cachedAgent) close() {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.agent != nil {
		ca.agent.Close()
	}
}

// withAgent runs fn on this slot's agent with the slot lock held — the same lock a turn holds — and
// reports false when the slot has no agent yet (a placeholder from /cd, or a tombstone). It is how a
// command touches an agent without holding one of the pool's guards itself: a slash command must not
// define a lock order, it must inherit the one this file keeps.
//
// fn must not call back into the pool (it runs under ca.mu) and should be short: taking ca.mu means
// waiting out a turn that is already running on this thread.
func (ca *cachedAgent) withAgent(fn func(*agent.AIAgent)) bool {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.agent == nil {
		return false
	}
	fn(ca.agent)
	return true
}

// withAgent runs fn on a thread's agent, reporting false when it has no live slot or no agent yet.
func (p *agentPool) withAgent(threadID string, fn func(*agent.AIAgent)) bool {
	ca := p.slot(threadID)
	if ca == nil {
		return false
	}
	return ca.withAgent(fn)
}

// each calls fn for every entry, one at a time. The list is SNAPSHOT under the short lock and fn
// runs without it: a slow call (reloading skills into every agent) must not hold up another
// thread's acquire, and an entry evicted while fn runs on it is harmless — the pointer stays valid,
// and evict only closes an agent whose slot lock it could take. A tombstone may be in the snapshot
// (a build that failed): a callback that reaches the agent through ca.withAgent skips it.
func (p *agentPool) each(fn func(threadID string, ca *cachedAgent)) {
	p.mu.Lock()
	snapshot := make(map[string]*cachedAgent, len(p.slots))
	for threadID, ca := range p.slots {
		snapshot[threadID] = ca
	}
	p.mu.Unlock()

	for threadID, ca := range snapshot {
		fn(threadID, ca)
	}
}

// workDir reads a thread's working directory, reporting false when the thread has no cache entry.
func (p *agentPool) workDir(threadID string) (string, bool) {
	if ca := p.slot(threadID); ca != nil {
		return ca.WorkDir(), true
	}
	return "", false
}

// setWorkDir updates a thread's working directory, reporting false when the thread has no cache
// entry (a /cd on a thread whose agent was just evicted still persists to the session; the next
// turn picks it up from there).
func (p *agentPool) setWorkDir(threadID, dir string) bool {
	ca := p.slot(threadID)
	if ca == nil {
		return false
	}
	ca.SetWorkDir(dir)
	return true
}

// count reports how many agents are cached.
func (p *agentPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots)
}
