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
// conversation — drivable from a frontend, and fed to the next Dream pass — and one that lost
// its source node would be a record that says somebody's work is in here without saying whose.
func TestCompactionCarriesTheManagedKindTheDreamExemptionAndTheSource(t *testing.T) {
	mgr := &fakeSessionManager{current: &session.Session{
		ID:           "s-remote",
		Kind:         session.KindA2AExecution,
		SkipDream:    true,
		SourceNodeID: "448ed5358a38add359c08cf734f05240",
	}}

	if _, err := FinalizeCompact(mgr, "sys", "summary of the remote work"); err != nil {
		t.Fatalf("FinalizeCompact: %v", err)
	}

	child := mgr.current
	require.NotNil(t, child)
	require.NotEqual(t, "s-remote", child.ID, "compaction continues in a NEW session")
	require.Equal(t, session.KindA2AExecution, child.Kind)
	require.True(t, child.SkipDream)
	require.Equal(t, "448ed5358a38add359c08cf734f05240", child.SourceNodeID,
		"the child is the session a list shows, so it is the one that has to say whose work it holds")
}

// The other half of the same rule: a session a subsystem drives must not be MODIFIED from a
// local frontend either, and the write that reaches it without a run is the rewind — it cuts the
// conversation and restores workspace files (design §2.1).
//
// The assertion that matters is not "returned an error" but "the history is byte-identical
// afterwards": a refusal that still truncated something would be worse than none.
func TestManagedSessionRefusesALocalRewind(t *testing.T) {
	a, mgr := managedAgent(t, session.KindA2AExecution)
	mgr.messages = []session.Message{{Type: session.MessageTypeUser, Content: "远端的一轮"}}

	// The chain surface the desktop builds must say why up front, not offer rows that fail.
	require.Contains(t, a.RewindChainBlocked(), session.KindA2AExecution)
	preview, err := a.PreviewRewind(context.Background(), 1)
	require.NoError(t, err)
	require.Contains(t, preview.Blocked, "subsystem", "the card has to explain itself")

	_, err = a.Rewind(context.Background(), 1)
	require.ErrorIs(t, err, ErrManagedSession)
	require.Len(t, mgr.messages, 1, "a refused rewind must not have cut the conversation")
	require.Equal(t, "远端的一轮", mgr.messages[0].Content)
}

func TestAnOrdinarySessionIsNotBlockedByTheManagedRule(t *testing.T) {
	// The rule must be invisible for an ordinary conversation: it blocks by KIND, not by "this
	// agent has a session manager". The rewind still fails in this fixture — checkpoints are off
	// — and that is the point: it fails for ITS OWN reason, unchanged by the new check.
	a, _ := managedAgent(t, "")
	require.Empty(t, a.RewindChainBlocked(), "an ordinary conversation has no chain-wide refusal")

	_, err := a.Rewind(context.Background(), 1)
	require.NotErrorIs(t, err, ErrManagedSession)
	require.NotContains(t, err.Error(), "subsystem")
}

// A FORK of a managed session cannot write into it, and the reason is structure rather than a
// check: the child is built without a session manager (agent/agent_fork.go). The design leans on
// exactly that (§7.1.3: "fork 出来的子 agent 没有 SessionManager（写不进任何会话）"), so it is
// asserted here — a later change that handed the child the parent's manager would otherwise turn
// a fork of a remote execution into a writer of it, silently.
func TestAForkOfAManagedSessionHasNoSessionToWriteInto(t *testing.T) {
	a, mgr := managedAgent(t, session.KindA2AExecution)
	require.NotNil(t, a.Config.SessionManager, "the parent owns its managed session")

	child := a.Fork(ForkConfig{Provider: &mockStreamProvider{name: "mock"}})
	t.Cleanup(child.Close)

	require.Nil(t, child.Agent().Config.SessionManager,
		"a fork must not inherit the parent's session — it has nothing to write into")
	// And the refusal the parent carries by KIND cannot even be reached from the child: it has no
	// session to judge, so it is not a second spelling of the same rule.
	require.NoError(t, child.Agent().managedSessionRefusal(context.Background()))
	require.Equal(t, "s-managed", mgr.current.ID, "forking must change nothing about the parent")
}
