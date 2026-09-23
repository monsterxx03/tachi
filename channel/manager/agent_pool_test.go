package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/tools"
)

// The agent pool's locking has two properties worth a test rather than a comment, and both used to
// be held only by prose spread over four files ("Lock ordering: agentCacheMu → ca.mu", "Fetch
// provider name outside the lock to avoid lock ordering"):
//
//  1. acquire/evict must not deadlock when they run AGAINST each other on many threads. That is a
//     real bug this test found: the cache used to re-check its slot by taking the MAP lock while
//     holding the SLOT lock, while eviction did the reverse — an ABBA hang, latent for as long as the
//     cache existed, because every test ran acquire and evict one after the other instead of against
//     each other. The pool now nests nothing; this test is what keeps it that way.
//
//  2. each() must call OUT without holding the pool lock. It is used to reload skills into every
//     cached agent, and holding the map lock across that would block every other thread's acquire
//     for the length of the reload — the reason the old code had to nest its two locks.
//
// Run with -race: the same tests cover the field-level races (workDir, agent pointers).

func TestAgentPool_ConcurrentAcquireEvictDoesNotDeadlock(t *testing.T) {
	// The POOL, not Manager.acquireAgent: building a real agent is tens of milliseconds of MCP/tool
	// wiring, and what is under test here is the lock choreography, not agent construction. A bare
	// *agent.AIAgent is enough — evict only calls Close on it, which is a no-op without a hook
	// dispatcher.
	pool := newAgentPool()
	build := func() (*agent.AIAgent, error) { return &agent.AIAgent{}, nil }

	const (
		threads    = 8
		iterations = 200
	)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < threads; i++ {
		threadID := fmt.Sprintf("thread-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				ca, err := pool.acquire(context.Background(), threadID, "test-model", build)
				if err != nil {
					return
				}
				_ = ca.WorkDir()
				pool.release(ca)
			}
		}()
	}

	// The evictors take the same two locks in the NESTED order, so an inconsistency between the two
	// orders shows up as a hang rather than as a wrong answer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for j := 0; j < iterations; j++ {
			pool.evict(fmt.Sprintf("thread-%d", j%threads))
			if j%10 == 0 {
				pool.evictAll()
			}
		}
	}()

	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("acquire/release racing evict/evictAll deadlocked. The pool holds THREE guards and " +
			"nests NONE of them (pool.mu = the map, cachedAgent.mu = one thread's turns, an atomic " +
			"evicted flag = \"this slot is dead\"); introducing any acquisition of one while " +
			"holding another — the shape the cache originally had, and the reason this test " +
			"exists — turns into a hang here; see agent_pool.go")
	}
}

func TestAgentPool_EachDoesNotHoldThePoolLock(t *testing.T) {
	mgr := newTestManagerWithProvider(t)

	// Two threads with entries, so each() has real work to do.
	slowThread := uniqueThreadID(t)
	mgr.agents.ensure(slowThread, func() *cachedAgent { return &cachedAgent{} })
	otherThread := uniqueThreadID(t)
	mgr.agents.ensure(otherThread, func() *cachedAgent { return &cachedAgent{} })

	inside := make(chan struct{})
	release := make(chan struct{})
	eachDone := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(eachDone)
		mgr.agents.each(func(_ string, _ *cachedAgent) {
			once.Do(func() { close(inside) }) // each() visits EVERY entry; signal on the first
			<-release                         // stand in for a slow ReloadSkills
		})
	}()
	<-inside

	// While the callback is stuck, another thread's lookup — the thing every turn and every slash
	// command does — must go straight through. If each() held the pool lock, this would block.
	lookupDone := make(chan struct{})
	go func() {
		defer close(lookupDone)
		_ = mgr.agents.slot(otherThread)
		mgr.agents.ensure(uniqueThreadID(t), func() *cachedAgent { return &cachedAgent{} })
	}()
	select {
	case <-lookupDone:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("each() held the pool lock while calling out: every other thread's acquire/slot " +
			"blocks for the length of the callback (agent_pool.go snapshots to prevent exactly this)")
	}
	close(release)
	<-eachDone
}

// TestAgentPool_TombstonesAreInvisible pins the other half of "a failed build leaves a tombstone": the
// slot STAYS in the map (removing it would take the map lock with the slot lock held), but no reader
// may SEE it. A tombstone slot() hands out is one /cd writes this thread's directory into — a value
// the next acquire throws away, so the command reports success while the pool keeps the old answer —
// and one getThreadWorkDir answers from instead of falling back to the session.
func TestAgentPool_TombstonesAreInvisible(t *testing.T) {
	pool := newAgentPool()
	buildErr := errors.New("provider unavailable")
	fail := func() (*agent.AIAgent, error) { return nil, buildErr }

	_, err := pool.acquire(context.Background(), "thread-tomb", "provider-a", fail)
	require.ErrorIs(t, err, buildErr)
	require.Equal(t, 1, pool.count(), "the tombstone is meant to stay in the map until the next caller")

	assert.Nil(t, pool.slot("thread-tomb"), "slot() handed out a dead entry")
	if wd, ok := pool.workDir("thread-tomb"); ok {
		t.Fatalf("workDir() answered %q from a tombstone; the session fallback never gets a chance", wd)
	}

	// The /cd sequence — slot, ensure, setWorkDir — must land on a LIVE slot. /cd resolves the
	// provider name before creating the placeholder, so the slot it makes is the one the next turn's
	// acquire will adopt rather than replace.
	if ca := pool.slot("thread-tomb"); ca == nil {
		pool.ensure("thread-tomb", func() *cachedAgent {
			return &cachedAgent{providerName: "provider-a", workDir: initialWorkDir()}
		})
	}
	require.True(t, pool.setWorkDir("thread-tomb", "/target/dir"))
	wd, ok := pool.workDir("thread-tomb")
	require.True(t, ok, "the slot created after the failed build was discarded")
	assert.Equal(t, "/target/dir", wd)

	// ... and the next successful build reuses that slot, so the /cd survives it.
	build := func() (*agent.AIAgent, error) { return &agent.AIAgent{}, nil }
	ca, err := pool.acquire(context.Background(), "thread-tomb", "provider-a", build)
	require.NoError(t, err)
	defer pool.release(ca)
	assert.Equal(t, "/target/dir", ca.WorkDir(), "the live slot's workDir did not survive the build")
}

// TestAgentPool_ReplacedSlotClosesItsAgent: a slot acquire replaces (the thread's provider changed) is
// dropped from the map, so evict/evictAll will never find it again — acquire owns its shutdown. The
// failure mode is silent: an agent's Close is what kills the processes a turn left behind, so a
// missing Close leaks them for the life of the process. The test starts a real background process and
// waits for the replaced agent's Close to take it down.
func TestAgentPool_ReplacedSlotClosesItsAgent(t *testing.T) {
	pool := newAgentPool()
	pm := tools.NewProcessManager()
	t.Cleanup(pm.KillAll)

	build := func() (*agent.AIAgent, error) {
		return &agent.AIAgent{Config: agent.AgentConfig{ProcessManager: pm}}, nil
	}

	ca, err := pool.acquire(context.Background(), "thread-switch", "provider-a", build)
	require.NoError(t, err)
	pool.release(ca)

	_, err = pm.Start(context.Background(), "sleeper", "sleep 30")
	require.NoError(t, err)

	// The provider this thread resolves to changed: the slot is dead, not merely stale.
	other, err := pool.acquire(context.Background(), "thread-switch", "provider-b", build)
	require.NoError(t, err)
	pool.release(other)

	deadline := time.Now().Add(5 * time.Second)
	for pm.Get("sleeper") != nil {
		if time.Now().After(deadline) {
			t.Fatal("the replaced slot's agent was never closed: its background process outlived the " +
				"slot — acquire's phase 1 dropped it from the map, so evict/evictAll can no longer reach it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEveryChannelCommandIsHandled closes the gap between "a command is declared for channel mode"
// and "something handles it". The registry (agent/commands) is the metadata, `executeSlashCommand`
// is the dispatch, and ONE command takes a third path: /compact is intercepted by buildHandler and
// runs as an agent turn, because summarizing needs the live session context rather than a
// synchronous handler.
//
// Nothing checked that the three agree. A command declared for channel mode with no handler
// anywhere answers "Unknown command: /x" and lists /x as available — the reader is told the command
// exists by the very message denying it does. Adding a channel command now means either handling it
// or naming its special path below.
//
// It is a SOURCE check, not a behavioural one: invoking every handler would need a live session per
// command and would run the destructive ones (/restart calls systemctl, /new ends a conversation).
func TestEveryChannelCommandIsHandled(t *testing.T) {
	// Commands handled outside executeSlashCommand, each naming where and why.
	handledElsewhere := map[string]string{
		"compact": "agent_turn.go buildHandler: /compact is rewritten into an instruction for the agent turn",
	}

	src, err := os.ReadFile("commands.go")
	require.NoError(t, err, "the dispatch source must be readable from the package dir")

	// The cases of executeSlashCommand only: a case in another function is not a dispatch.
	body := string(src)
	at := strings.Index(body, "func (m *Manager) executeSlashCommand(")
	require.Greater(t, at, 0, "executeSlashCommand moved: update this guard")
	body = body[at:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	dispatched := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\tcase "([a-z_]+)":`).FindAllStringSubmatch(body, -1) {
		dispatched[m[1]] = true
	}
	require.NotEmpty(t, dispatched, "no cases parsed — the pattern and the code have drifted apart")

	for _, def := range cmds.ForMode(cmds.ModeChannel) {
		if dispatched[def.Name] {
			continue
		}
		if where, ok := handledElsewhere[def.Name]; ok {
			assert.NotEmpty(t, where, "%s: the exception must name the code path that handles it", def.Name)
			continue
		}
		assert.Failf(t, "channel command with no handler",
			"/%s is declared for channel mode (agent/commands) but neither dispatched by "+
				"executeSlashCommand nor listed in handledElsewhere — a user typing it gets "+
				"\"Unknown command\" while the same message lists it as available", def.Name)
	}
}
