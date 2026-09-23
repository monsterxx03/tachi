package manager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The table's two subtle rules, both of which used to be properties of the container it replaced
// (a LockedMap) rather than of the code that used it — so nothing said what would break if they were
// lost when the mapping moved here.

// TestThreadTable_EndTurnOnlyClearsItsOwnTurn: a handler goroutine that finishes LATE must not drop
// the activation a newer turn installed. The sequence is the /stop one: stop takes the running turn
// out of the table, the next message starts a fresh one, and only then does the stopped goroutine
// reach its deferred bookkeeping.
func TestThreadTable_EndTurnOnlyClearsItsOwnTurn(t *testing.T) {
	table := newThreadTable()
	const threadID = "test:group:stale"

	stopped := table.beginTurn(threadID, func() *threadActivation { return &threadActivation{} })
	require.NotNil(t, stopped)

	taken, ok := table.takeTurn(threadID)
	require.True(t, ok)
	require.Same(t, stopped, taken)

	next := table.beginTurn(threadID, func() *threadActivation { return &threadActivation{} })
	require.NotSame(t, stopped, next, "a taken turn must not be handed out again")

	table.endTurn(threadID, stopped) // the stale goroutine's deferred cleanup

	live, ok := table.turn(threadID)
	require.True(t, ok, "the stale turn's cleanup dropped the LIVE turn")
	assert.Same(t, next, live)
}

// TestThreadTable_BeginTurnJoinsARunningTurn: a second message on a thread does NOT get a second
// activation — it gets the running one and reads the marker inside it to tell "I started this" from
// "I am steering it". Creating a rival activation would give one thread two cancels, two steer queues
// and two AskUser slots.
func TestThreadTable_BeginTurnJoinsARunningTurn(t *testing.T) {
	table := newThreadTable()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := table.beginTurn("test:group:join", func() *threadActivation {
		return &threadActivation{ctx: ctx, cancel: cancel}
	})
	second := table.beginTurn("test:group:join", func() *threadActivation {
		t.Error("make() ran while a turn was live: a rival activation would be leaked")
		return &threadActivation{}
	})
	assert.Same(t, first, second)
}

// TestThreadTable_LookupsDoNotCreateRecords: the read-only lookups (channelFor / defaults / turn) are
// what stateless commands and the ambient timer call on threads that may never have been seen — they
// must not grow the table. Only the writers (setChannel / setDefaults / beginTurn) create a record.
func TestThreadTable_LookupsDoNotCreateRecords(t *testing.T) {
	table := newThreadTable()

	_, ok := table.channelFor("never-seen")
	assert.False(t, ok)
	_, ok = table.defaults("never-seen")
	assert.False(t, ok)
	_, ok = table.turn("never-seen")
	assert.False(t, ok)
	assert.Empty(t, table.byID, "a lookup created a record")

	table.setChannel("seen", &mockChannel{name: "plain"})
	table.setDefaults("seen", threadDefaults{workDir: "/tmp/x", providerName: "main"})
	assert.Len(t, table.byID, 1)

	ch, ok := table.channelFor("seen")
	require.True(t, ok)
	assert.Equal(t, "plain", ch.Name())
	td, ok := table.defaults("seen")
	require.True(t, ok)
	assert.Equal(t, threadDefaults{workDir: "/tmp/x", providerName: "main"}, td)
}
