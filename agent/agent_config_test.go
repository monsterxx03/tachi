package agent

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/llm"
)

// TestConvState_ConcurrentEstimateAndRead guards the data race that motivated
// convState. In channel mode one cached AIAgent is shared between the turn
// goroutine (writing the estimate via EstimateAndUpdateTokens) and slash-command
// handlers reading it: handleUsageCommand reaches the agent through
// getAgentEstimate / getAgentBreakdown, which release agentCacheMu after the
// map lookup and then read agent fields without holding the cachedAgent lock.
//
// Run with -race for this test to be meaningful.
func TestConvState_ConcurrentEstimateAndRead(t *testing.T) {
	a := NewAIAgent(&mockStreamProvider{name: "anthropic"}, 10)

	msgs := []llm.Message{
		{Role: "system", Content: "you are a helpful agent"},
		{Role: "user", Content: "hello world, padding to make the estimate nonzero"},
	}

	const iterations = 500
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // turn goroutine
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			a.EstimateAndUpdateTokens(nil, msgs)
		}
	}()

	wg.Add(1)
	go func() { // slash-command reader
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = a.LastInputEstimate()
			_ = a.LastTokenBreakdown()
		}
	}()

	wg.Wait()

	if got := a.LastInputEstimate(); got <= 0 {
		t.Errorf("expected a positive estimate after concurrent updates, got %d", got)
	}
}

// TestConvState_EstimateAndBreakdownStayConsistent verifies the invariant that
// motivated pairing the two fields under one lock: a reader must never observe
// a token total that disagrees with the breakdown it was computed from.
func TestConvState_EstimateAndBreakdownStayConsistent(t *testing.T) {
	a := NewAIAgent(&mockStreamProvider{name: "anthropic"}, 10)

	short := []llm.Message{{Role: "user", Content: "hi"}}
	long := []llm.Message{{Role: "user", Content: string(make([]byte, 40_000))}}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			if i%2 == 0 {
				a.EstimateAndUpdateTokens(nil, short)
			} else {
				a.EstimateAndUpdateTokens(nil, long)
			}
		}
		close(stop)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			total, tb := a.LastInputEstimateWithBreakdown()
			// Only meaningful once the first estimate has landed.
			if total != 0 && tb.Total != 0 && total != tb.Total {
				t.Errorf("torn read: estimate=%d but breakdown.Total=%d", total, tb.Total)
				return
			}
		}
	}()

	wg.Wait()
}

// TestConvState_CompactCooldown verifies the cross-turn cooldown semantics:
// the compact baseline lives in convState (agent lifetime), so a fresh turn
// still honours it until the estimate grows >= 20%.
func TestConvState_CompactCooldown(t *testing.T) {
	cs := newConvState()

	if cs.compactCooldown() {
		t.Error("no compaction recorded yet — cooldown must be false")
	}

	cs.setEstimate(1000, cs.snapshotBreakdown())
	cs.setCompactEstimate(1000)

	if !cs.compactCooldown() {
		t.Error("estimate unchanged since compaction — expected cooldown")
	}

	cs.setEstimate(1150, cs.snapshotBreakdown()) // +15%
	if !cs.compactCooldown() {
		t.Error("growth below 20% — expected cooldown to still hold")
	}

	cs.setEstimate(1300, cs.snapshotBreakdown()) // +30%
	if cs.compactCooldown() {
		t.Error("growth above 20% — expected cooldown to lift")
	}
}

// TestConvState_MessageDate verifies the date of the last user message is
// recorded and readable across turns (agent-level, not per-run).
func TestConvState_MessageDate(t *testing.T) {
	cs := newConvState()
	if got := cs.messageDate(); got != "" {
		t.Errorf("expected empty initial date, got %q", got)
	}
	cs.setMessageDate("2026-07-30")
	if got := cs.messageDate(); got != "2026-07-30" {
		t.Errorf("expected 2026-07-30, got %q", got)
	}
}

// TestRunState_ConcurrentMessagesSnapshot covers GetLastMessages, which channel
// mode calls to refresh its in-memory history cache while a turn may still be
// running. The returned slice is a copy, so appends by the writer must not be
// visible through it.
func TestRunState_ConcurrentMessagesSnapshot(t *testing.T) {
	rs := &RunState{}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rs.append(llm.Message{Role: "user", Content: "m"})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			snap := rs.snapshotMessages()
			// Mutating the snapshot must not affect the stored slice.
			for j := range snap {
				snap[j].Content = "mutated"
			}
		}
	}()

	wg.Wait()

	for i, m := range rs.snapshotMessages() {
		if m.Content != "m" {
			t.Fatalf("snapshot mutation leaked into run state at %d: %q", i, m.Content)
		}
	}
}

// TestSteerInjection verifies that a steer message sent on the run's steer
// channel is injected as a RoleSteer message at the tool-call boundary,
// after which the loop continues.
func TestSteerInjection(t *testing.T) {
	mp := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{
		toolCallSeq("Bash", "call_1", `{"command":"ls"}`), // first call: tool call
		textSeq("done"), // second call: stop
	}}
	a := newTestAgent(t, mp)
	a.RegisterTool(echoStub())

	steerCh := make(chan SteerInput, 1)
	ch := a.RunConversationStream(t.Context(), nil, "hi", "sys",
		llm.ChatOptions{MaxTokens: 4096}, WithSteerChannel(steerCh))

	steerCh <- SteerInput{Text: "继续写代码"}
	result, _ := drainAgentEvents(ch)

	require.NotNil(t, result)
	assert.Equal(t, ExitReasonStop, result.ExitReason)

	// The steer injection happens at the tool-call boundary and the loop
	// continues afterwards — assert a RoleSteer message exists in history
	// (not that it is the last one).
	msgs := a.GetLastMessages()
	assert.True(t, slices.ContainsFunc(msgs,
		func(m llm.Message) bool { return m.Role == llm.RoleSteer && m.Content == "继续写代码" }),
		"expected a RoleSteer message in history")
}

// TestRunStateConcurrentRead exercises concurrent reads of run/conversation
// state while the loop is running (channel mode /usage pattern).
// Run with -race for this test to be meaningful.
func TestRunStateConcurrentRead(t *testing.T) {
	mp := &mockStreamProvider{name: "mock", sequences: [][]llm.StreamEvent{
		toolCallSeq("Bash", "call_1", `{"command":"ls"}`),
		textSeq("done"),
	}}
	a := newTestAgent(t, mp)
	a.RegisterTool(echoStub())

	ch := a.RunConversationStream(t.Context(), nil, "hi", "sys", llm.ChatOptions{MaxTokens: 4096})

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // slash-command reader, racing the turn goroutine
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = a.LastInputEstimate()
			_, _ = a.LastInputEstimateWithBreakdown()
			_ = a.GetLastMessages()
		}
	}()

	for range ch { // drain to close
	}
	close(done)
	wg.Wait()

	require.NotEmpty(t, a.GetLastMessages())
	require.Greater(t, a.LastInputEstimate(), int64(0))
}

// TestOneOffDoesNotPublishCurrentRun is the regression test for the one-off
// RunState semantics: after a one-off run (/commit, /review), the main
// conversation's GetLastMessages and LastInputEstimate must be exactly what
// they were before — one-off runs neither publish currentRun nor touch the
// conversation token estimate.
func TestOneOffDoesNotPublishCurrentRun(t *testing.T) {
	mainProv := &mockStreamProvider{name: "main", sequences: [][]llm.StreamEvent{
		textSeq("main answer"),
	}}
	a := newTestAgent(t, mainProv)

	// 1. Run a main-conversation turn to establish baseline state.
	ch := a.RunConversationStream(t.Context(), nil, "hello main", "sys", llm.ChatOptions{MaxTokens: 4096})
	result, _ := drainAgentEvents(ch)
	require.NotNil(t, result)
	require.Equal(t, ExitReasonStop, result.ExitReason)

	baseMsgs := a.GetLastMessages()
	baseEstimate := a.LastInputEstimate()
	require.NotEmpty(t, baseMsgs)
	require.Greater(t, baseEstimate, int64(0))

	// 2. Run a one-off task (like /commit) on the same agent.
	oneoffProv := &mockStreamProvider{name: "oneoff", sequences: [][]llm.StreamEvent{
		textSeq("commit message"),
	}}
	oneoffCh := a.RunOneOffStream(t.Context(), oneoffProv, "sys", "make a commit",
		llm.ChatOptions{MaxTokens: 1024})
	oneoffResult, _ := drainAgentEvents(oneoffCh)
	require.NotNil(t, oneoffResult)
	require.Equal(t, ExitReasonStop, oneoffResult.ExitReason)

	// 3. Main conversation state must be untouched.
	assert.Equal(t, baseMsgs, a.GetLastMessages(),
		"one-off run must not replace the main conversation's messages")
	assert.Equal(t, baseEstimate, a.LastInputEstimate(),
		"one-off run must not change the main conversation's token estimate")
}
