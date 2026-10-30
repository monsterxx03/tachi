package manager

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
)

// The phone drives a conversation a channel owns — through the CHANNEL's agent, its cached history and
// its thread lock. These pin the two halves of that contract: the turn really runs there, and a
// command whose answer would go out through the channel is refused instead of run.

func TestSubmitTurn_RunsOnTheThreadsOwnAgent(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	mgr.defaultResolvedProvider.Provider = &mockProvider{name: "mock", responses: []string{"好的，我在。"}}

	var mu sync.Mutex
	var deltas strings.Builder
	var sawComplete bool
	obs := func(ev agent.AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Type {
		case agent.AgentEventTextDelta:
			deltas.WriteString(ev.TextDelta)
		case agent.AgentEventTurnComplete:
			sawComplete = true
		}
	}

	reply, err := mgr.SubmitTurn(t.Context(), uniqueThreadID(t), "在吗", obs)
	require.NoError(t, err)
	assert.Contains(t, reply, "好的，我在。")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "好的，我在。", deltas.String(), "the observer sees the turn stream, not just its end")
	assert.True(t, sawComplete, "the observer must see the turn end too — that is how the phone stops waiting")
}

// TestSubmitTurn_RefusesACommandAnsweredInTheChannel: /model (and /stop, and every other synchronous
// slash command) answers through the conversation's own channel. Run from the phone it would reply
// where the reader is not looking and leave the phone with nothing — so it is refused BEFORE anything
// runs, and the caller can turn that into one clear HTTP refusal.
func TestSubmitTurn_RefusesACommandAnsweredInTheChannel(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	_, err := mgr.SubmitTurn(t.Context(), threadID, "/model", nil)
	require.ErrorIs(t, err, ErrExternalCommand)
	assert.ErrorIs(t, mgr.ExternalRefusal("/model"), ErrExternalCommand)

	// The two slash commands that ARE turn-shaped stay available: they need the live session context
	// and answer as the turn does.
	assert.NoError(t, mgr.ExternalRefusal("/compact"))
	assert.NoError(t, mgr.ExternalRefusal("随便说点什么"))

	// Nothing was activated by the refusal: the thread has no turn in flight.
	_, running := mgr.threads.takeTurn(threadID)
	assert.False(t, running, "a refused command must not claim the thread")
}

// TestSubmitTurn_JoinsTheRunningTurn: a message that lands mid-turn is the channel's own semantics —
// it is queued as steer input, not refused with "turn running".
func TestSubmitTurn_JoinsTheRunningTurn(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)

	// A live activation, exactly what a running channel turn leaves behind.
	ta := &threadActivation{
		steerRespCh: make(chan agent.SteerInput, 1),
		resultCh:    make(chan handlerResult, 1),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ta.ctx, ta.cancel = ctx, cancel
	mgr.threads.beginTurn(threadID, func() *threadActivation { return ta })

	_, err := mgr.SubmitTurn(t.Context(), threadID, "再加一句", nil)
	require.NoError(t, err)

	ta.mu.Lock()
	defer ta.mu.Unlock()
	require.Len(t, ta.pending, 1, "mid-turn text is queued for steer, so the turn sees it")
	assert.Equal(t, "再加一句", ta.pending[0].content)
}

// TestPrepareThreadSession_RecordsTheOwningChannel: the process's thread→channel mapping is built by
// inbound messages, so it is empty right after a restart. The conversation's record carries the name
// instead — which is what lets a phone name the channel of a conversation that has not spoken yet.
func TestPrepareThreadSession_RecordsTheOwningChannel(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	threadID := uniqueThreadID(t)
	mgr.setThreadChannel(threadID, &mockChannel{name: "discord"})

	resolved := mgr.defaultResolvedProvider
	sm, _ := mgr.prepareThreadSession(threadID, resolved)
	require.NotNil(t, sm)
	require.NotNil(t, sm.Current())
	assert.Equal(t, "discord", sm.Current().ChannelName, "the owning channel goes onto the record")

	// A second pass does not rewrite it (and a thread with no known channel writes nothing at all).
	before := sm.Current().UpdatedAt
	sm2, _ := mgr.prepareThreadSession(threadID, resolved)
	require.NotNil(t, sm2)
	assert.Equal(t, "discord", sm2.Current().ChannelName)
	assert.True(t, before.Equal(sm2.Current().UpdatedAt), "an unchanged name is not rewritten")

	other := uniqueThreadID(t)
	sm3, _ := mgr.prepareThreadSession(other, resolved)
	require.NotNil(t, sm3)
	assert.Empty(t, sm3.Current().ChannelName, "no known channel, no guess")
}

// TestThreadPlatform_NamesTheChannelThatOwnsTheThread is what the phone's scheduled-jobs panel shows
// beside a channel job. An unknown thread has no name — a thread id does not encode its platform.
func TestThreadPlatform_NamesTheChannelThatOwnsTheThread(t *testing.T) {
	mgr := newTestManagerWithProvider(t)
	assert.Equal(t, "", mgr.ThreadPlatform("never-seen"))

	mgr.setThreadChannel("discord-thread", &mockChannel{name: "discord"})
	assert.Equal(t, "discord", mgr.ThreadPlatform("discord-thread"))
}

// Compile-time guard: the phone's side of this process talks to the manager through this signature,
// so a change must break here rather than at run time.
var _ func(context.Context, string, string, func(agent.AgentEvent)) (string, error) = (&Manager{}).SubmitTurn
