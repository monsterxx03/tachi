package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// The managed-session rule, tested where it is enforced. Three things matter and each has
// a test: a local turn is REFUSED, the refusal leaves the session untouched, and the run
// that owns the session (carrying the grant) is not refused. A fourth asserts the rule
// survives compaction, which is where a classification like this silently disappears.

func managedAgent(t *testing.T, kind string, opts ...testAgentOpt) (*AIAgent, *fakeSessionManager) {
	t.Helper()
	mgr := &fakeSessionManager{current: &session.Session{ID: "s-managed", Kind: kind}}
	all := append([]testAgentOpt{func(a *AIAgent) { a.SetSessionManager(mgr) }}, opts...)
	provider := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{textSeq("hello")}}
	return newTestAgent(t, provider, all...), mgr
}

func TestManagedSessionRefusesALocalTurn(t *testing.T) {
	a, mgr := managedAgent(t, session.KindA2AExecution)

	result, events := drainAgentEvents(a.RunConversationStream(
		context.Background(), nil, "carry on where the remote task left off", "sys", llm.ChatOptions{}))

	require.ErrorIs(t, result.Error, ErrManagedSession)
	require.Len(t, events, 1, "the refusal is one error event, not a run that stopped early")
	require.Contains(t, events[0].TextDelta, session.KindA2AExecution)
	require.Empty(t, mgr.messages,
		"a refused turn must not have written the user message it was refused for")
}

func TestManagedSessionRunsWithTheGrant(t *testing.T) {
	a, _ := managedAgent(t, session.KindA2AExecution)

	result, _ := drainAgentEvents(a.RunConversationStream(
		WithTrustedExecution(context.Background()), nil, "the node's own turn", "sys", llm.ChatOptions{}))

	require.NoError(t, result.Error, "the subsystem that owns the session runs it")
	require.Equal(t, "hello", result.Response)
}

func TestOrdinarySessionIsNotAffected(t *testing.T) {
	// An ordinary conversation has no kind: the check must be invisible for every local
	// frontend, or the rule would break the product it exists to protect.
	a, _ := managedAgent(t, "")

	result, _ := drainAgentEvents(a.RunConversationStream(
		context.Background(), nil, "hello", "sys", llm.ChatOptions{}))

	require.NoError(t, result.Error)
	require.Equal(t, "hello", result.Response)
}

func TestOneOffRunsAreRefusedToo(t *testing.T) {
	// The one-off path writes to the session as well (sidecar recorder, history), so it is
	// the other door into a managed conversation — and it takes a provider argument, so it
	// has its own entry point to guard.
	a, _ := managedAgent(t, session.KindA2AExecution)

	result, events := drainAgentEvents(a.RunOneOffStream(
		context.Background(), nil, "sys", "/commit", llm.ChatOptions{}))

	require.ErrorIs(t, result.Error, ErrManagedSession)
	require.Len(t, events, 1)
}

// Compaction is where a session-level classification goes to die: FinalizeCompact carries
// fields to the successor BY HAND, so anything not listed there is silently zeroed. A
// compacted remote execution that lost its kind would come back as an ordinary local
// conversation — drivable from a frontend, and fed to the next Dream pass.
func TestCompactionCarriesTheManagedKindAndTheDreamExemption(t *testing.T) {
	mgr := &fakeSessionManager{current: &session.Session{
		ID:        "s-remote",
		Kind:      session.KindA2AExecution,
		SkipDream: true,
	}}

	if _, err := FinalizeCompact(mgr, "sys", "summary of the remote work"); err != nil {
		t.Fatalf("FinalizeCompact: %v", err)
	}

	child := mgr.current
	require.NotNil(t, child)
	require.NotEqual(t, "s-remote", child.ID, "compaction continues in a NEW session")
	require.Equal(t, session.KindA2AExecution, child.Kind)
	require.True(t, child.SkipDream)
}
