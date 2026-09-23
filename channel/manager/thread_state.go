package manager

import (
	"sync"

	"github.com/monsterxx03/tachi/pkg/channel"
)

// threadDefaults holds per-thread default config applied when a fresh session is created for the
// thread — typically parsed from user-editable platform metadata such as a group announcement.
// Empty workDir/providerName leave that dimension on the usual fallback.
type threadDefaults struct {
	workDir      string
	providerName string
}

// threadState is everything the Manager keeps about one thread besides its cached agent: the channel
// it belongs to, the platform defaults for a fresh session, and the turn running right now.
//
// It exists because those three used to be THREE containers with THREE locks — the channel behind an
// RWMutex, the defaults behind a Mutex, the live turn in a LockedMap — reached from four different
// files, with the agent cache adding a fourth. Four containers and four lock styles for one subject
// is where "which lock am I holding" comes from. Now it is one record behind one lock, and the turn's
// INTERNALS keep their own lock (threadActivation.mu): that object is shared with a running turn
// goroutine, so it is not a field of the record.
//
// The lock is a LEAF: nothing is taken while holding it, and its methods never call out (the one
// callback, beginTurn's make, only allocates). A record is created at most once per thread and never
// removed — the same lifetime the channel/defaults maps had.
type threadState struct {
	ch       channel.Channel   // who owns this thread (interactivity, tools, reminders)
	defaults threadDefaults    // defaults for a session created from here on
	turn     *threadActivation // the live turn, nil when the thread is idle
}

// threadTable is threadState by threadID.
type threadTable struct {
	mu   sync.Mutex
	byID map[string]*threadState
}

func newThreadTable() *threadTable {
	return &threadTable{byID: make(map[string]*threadState)}
}

func (t *threadTable) recordLocked(threadID string) *threadState {
	if s, ok := t.byID[threadID]; ok {
		return s
	}
	s := &threadState{}
	t.byID[threadID] = s
	return s
}

// channelFor reports the channel owning a thread, or false when the thread is unknown.
func (t *threadTable) channelFor(threadID string) (channel.Channel, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byID[threadID]
	if !ok || s.ch == nil {
		return nil, false
	}
	return s.ch, true
}

// setChannel records the channel owning a thread. Written once per thread, on its first message.
func (t *threadTable) setChannel(threadID string, ch channel.Channel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordLocked(threadID).ch = ch
}

// defaults returns the thread's platform defaults, if it has any.
func (t *threadTable) defaults(threadID string) (threadDefaults, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byID[threadID]
	if !ok {
		return threadDefaults{}, false
	}
	return s.defaults, true
}

// setDefaults replaces the thread's platform defaults.
func (t *threadTable) setDefaults(threadID string, td threadDefaults) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordLocked(threadID).defaults = td
}

// beginTurn returns the thread's live turn, creating it with make() when the thread is idle. The
// caller MUST look at the returned activation's marker (steerRespCh) to tell "I started a turn" from
// "I joined one that is already running" — this method deliberately does not answer that.
//
// make runs under the lock, so it must not call back into the Manager (it only builds the context and
// the channels of a new activation).
func (t *threadTable) beginTurn(threadID string, make func() *threadActivation) *threadActivation {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.recordLocked(threadID)
	if s.turn != nil {
		return s.turn
	}
	s.turn = make()
	return s.turn
}

// endTurn clears the live turn, but only if the stored activation is still the caller's (pointer
// equality): a stale handler goroutine finishing late must not delete the activation a newer turn
// installed after /new or /stop.
func (t *threadTable) endTurn(threadID string, ta *threadActivation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.byID[threadID]; ok && s.turn == ta {
		s.turn = nil
	}
}

// takeTurn removes and returns the live turn, if any. The caller owns the cancelling: this returns
// the activation so the cancellation happens OUTSIDE the lock (cancel() runs the turn goroutine's
// unwinding, which must not happen under a table lock).
func (t *threadTable) takeTurn(threadID string) (*threadActivation, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byID[threadID]
	if !ok || s.turn == nil {
		return nil, false
	}
	ta := s.turn
	s.turn = nil
	return ta, true
}

// turn reports the live turn without removing it.
func (t *threadTable) turn(threadID string) (*threadActivation, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byID[threadID]
	if !ok || s.turn == nil {
		return nil, false
	}
	return s.turn, true
}
