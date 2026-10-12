package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixture's price table: ¥100/M in, ¥10/M cached, ¥0 to write — a miss costs
// 90 units per million, which keeps every expected cost in this file in whole or
// obvious numbers (and keeps the built-in table, which is time-of-use dependent,
// out of the assertions).
const (
	testInputPrice = 100.0
	testCacheRead  = 10.0
)

// newMCPBudgetAgent builds a TUI-mode agent whose prompt is `tokens` big, priced
// by an explicit (band-free) schedule, with `undiscovered` MCP tools sitting in
// the deferred pool that no session has loaded yet.
func newMCPBudgetAgent(t *testing.T, tokens int64, budget float64, undiscovered int) *AIAgent {
	t.Helper()
	return newMCPBudgetAgentWith(t, &mockStreamProvider{name: "mock", providerName: "mock"}, tokens, budget, undiscovered)
}

func newMCPBudgetAgentWith(t *testing.T, provider llm.Provider, tokens int64, budget float64, undiscovered int) *AIAgent {
	t.Helper()
	// The discovered-set path is keyed on files under the base dir; a test must
	// not read or write the user's own ~/.tachi.
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	a := newTestAgent(t, provider)
	a.Config.Logger = logger.Default()
	a.SetPermissionMode(PermissionModeTUI)
	// Auto-compaction off: these fixtures build FullConfig by hand, and the
	// zero-value threshold (a loaded config gets 0.8 from its default tag) would
	// otherwise make every turn compact, which is a different test.
	autoOff := false
	a.Config.FullConfig = &config.Config{
		Compact: config.CompactConfig{Auto: &autoOff},
		Providers: []config.ProviderConfig{{
			Name: "mock",
			Spec: config.ModelSpec{Pricing: &config.PricingConfig{
				InputPrice:          f64ptr(testInputPrice),
				CacheReadInputPrice: f64ptr(testCacheRead),
			}},
		}},
		MCPToolSearch: config.MCPToolSearchConfig{CacheBudget: budget},
	}
	mgr := mcp.NewManager(t.Context(), nil, logger.Default())
	for i := range undiscovered {
		name := fmt.Sprintf("mcp__alpha__tool%d", i)
		mgr.Pool().Add(&mcp.DeferredTool{Name: name, ServerName: "alpha", Description: "d"})
	}
	a.Config.MCPManager = mgr
	a.conv.setEstimate(tokens, tokenbreakdown.Breakdown{})
	return a
}

// searchCall is one MCPSearchTools call as the executor sees it.
func searchCall() llm.ToolCall {
	return llm.ToolCall{ID: "call-1", Function: llm.ToolCallFunction{
		Name:      tools.ToolNameMCPSearchTools,
		Arguments: `{"query":"query the database"}`,
	}}
}

// gate runs the pre-invoke gate on a channel that is never read, for the cases
// where no card is expected. A card raised here would block the gate forever, so
// callers that DO expect one use gateWithAnswer.
func gate(t *testing.T, a *AIAgent, tc llm.ToolCall) (tools.ToolResult, bool) {
	t.Helper()
	return gateWithSession(t, a, "", tc)
}

// gateWithSession is gate with a session ID in the context — the ID the executor passes
// (tools.WithSessionID) and therefore the one the discovered-set lookup is keyed on. An empty
// ID means "no session": the set is nil and every pooled tool counts as undiscovered.
func gateWithSession(t *testing.T, a *AIAgent, sessionID string, tc llm.ToolCall) (tools.ToolResult, bool) {
	t.Helper()
	ctx := context.Background()
	if sessionID != "" {
		ctx = tools.WithSessionID(ctx, sessionID)
	}
	tr, handled, err := a.checkMCPSearchBudget(ctx, tc, make(chan AgentEvent, 8))
	require.NoError(t, err, "the gate must never abort the turn")
	return tr, handled
}

func TestCacheInvalidationCost(t *testing.T) {
	// A miss is billed at the input price and written back to the cache, where a
	// hit had cost the cache-read price: input + creation − read.
	assert.Equal(t, 90.0, cacheInvalidationCost(1_000_000, llm.ModelPrice{
		InputPrice: 100, CacheReadInputPrice: 10,
	}))
	assert.Equal(t, 92.5, cacheInvalidationCost(1_000_000, llm.ModelPrice{
		InputPrice: 100, CacheReadInputPrice: 10, CacheCreationInputPrice: 2.5,
	}))
	assert.Equal(t, 0.0, cacheInvalidationCost(1_000_000, llm.ModelPrice{
		InputPrice: 100, CacheReadInputPrice: 100,
	}), "a cache read as expensive as a miss means nothing to charge for")
	assert.Equal(t, 0.0, cacheInvalidationCost(0, llm.ModelPrice{InputPrice: 100}))
}

func TestMCPSearchBudget_UnderBudgetDoesNotAsk(t *testing.T) {
	// 100k tokens × 90/1M = ¥9 of re-read; the budget is ¥10.
	a := newMCPBudgetAgent(t, 100_000, 10, 3)
	_, handled := gate(t, a, searchCall())
	assert.False(t, handled, "under budget the search must run unasked")
}

// The budget is read from the config object on EVERY call, not captured when the agent or the
// tool was built — which is what makes a settings save take effect on the very next search, with
// no restart and nothing rebuilt. The test mutates the leaf the way a save does (see
// desktop/agent.go's withConfig: the ONE object, in place); a cached copy would pass every other
// test in this file.
//
// The three calls are the proof, and the log is where they are visible: proceed (¥9 < ¥10), ask
// (¥9 ≥ ¥1), proceed (¥10 restored). The middle call is answered allow-ONCE on purpose — a
// refusal would block the session and the third call would be decided by that flag instead.
func TestMCPSearchBudget_PicksUpAChangedBudget(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, logger.Init(dir, *logger.DefaultConfig()))

	a := newMCPBudgetAgent(t, 100_000, 10, 3)
	a.Config.Logger = logger.New("agent-budget-test")

	if _, handled := gate(t, a, searchCall()); handled {
		t.Fatal("the fixture is supposed to be under budget")
	}

	a.Config.FullConfig.MCPToolSearch.CacheBudget = 1 // the save: the same object, in place

	if _, handled := gateWithAnswer(t, a, searchCall(), ConfirmAllowOnce); handled {
		t.Fatal("the lowered budget must be in force on the very next call")
	}

	a.Config.FullConfig.MCPToolSearch.CacheBudget = 10
	if _, handled := gate(t, a, searchCall()); handled {
		t.Error("a raised budget must stop the asking too")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "debug.log"))
	require.NoError(t, err)
	var decisions []string
	for _, l := range strings.Split(string(raw), "\n") {
		// The answer line shares the message prefix; the trace line is the one carrying a decision.
		if strings.Contains(l, "Agent: MCP search cost gate") && strings.Contains(l, "decision=") {
			decisions = append(decisions, logValue(l, "decision"))
		}
	}
	assert.Equal(t, []string{"proceed", "ask", "proceed"}, decisions,
		"the same agent, the same tool instance, three budgets")
}

// logValue reads one key=... field out of a log line ("key=" until the next space).
func logValue(line, key string) string {
	_, rest, ok := strings.Cut(line, key+"=")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, " ")
	return v
}

func TestMCPSearchBudget_OverBudgetRefusalBlocksTheSession(t *testing.T) {
	// 100k × 90/1M = ¥9, budget ¥1.
	a := newMCPBudgetAgent(t, 100_000, 1, 3)

	tr, handled := gateWithAnswer(t, a, searchCall(), ConfirmDeny)
	require.True(t, handled, "a refusal settles the call")
	require.Equal(t, tools.ToolResultError, tr.Status)
	msg := tr.Err.Error()
	assert.Contains(t, msg, "拒绝", "the model must be told WHY")
	assert.Contains(t, msg, "不要再调用 MCPSearchTools", "and told not to retry")
	assert.Contains(t, msg, mcpSearchBudgetKey, "and what to raise")

	// The second call is refused WITHOUT a card: a refusal the next call could
	// undo by asking again would be a button that cannot deliver.
	tr2, handled2 := gate(t, a, searchCall())
	assert.True(t, handled2)
	assert.Equal(t, tools.ToolResultError, tr2.Status)
}

func TestMCPSearchBudget_AllowOnceAsksAgainNextTime(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 1, 3)

	_, handled := gateWithAnswer(t, a, searchCall(), ConfirmAllowOnce)
	assert.False(t, handled, "an approved search proceeds")

	// allow-ONCE is not the session switch: the next search asks again.
	_, handled2 := gateWithAnswer(t, a, searchCall(), ConfirmAllowOnce)
	assert.False(t, handled2)
}

func TestMCPSearchBudget_SessionAllowStopsAsking(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 1, 3)

	_, handled := gateWithAnswer(t, a, searchCall(), ConfirmAllowSession)
	assert.False(t, handled, "the approved search proceeds")

	// No card, no wait: the gate runs straight through on a channel nobody reads.
	_, handled2 := gate(t, a, searchCall())
	assert.False(t, handled2)
}

func TestMCPSearchBudget_SkipModeNeverAsks(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 1, 3)
	a.SetPermissionMode(PermissionModeSkip) // channel / tachi -p / subagent

	_, handled := gate(t, a, searchCall())
	assert.False(t, handled, "no one is at the console: the search proceeds")
}

func TestMCPSearchBudget_AllToolsLoadedDoesNotAsk(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 1, 3)
	// Every pool tool is already in this session's list, so a search cannot move
	// the tool list — and an unmoved tool list cannot break the cache.
	set := a.Config.MCPManager.SetFor("s1")
	for _, dt := range a.DeferredPool().All() {
		set.Add(dt.Name)
	}

	ctx := tools.WithSessionID(context.Background(), "s1")
	tr, handled, err := a.checkMCPSearchBudget(ctx, searchCall(), make(chan AgentEvent, 8))
	require.NoError(t, err)
	assert.False(t, handled, "nothing to load means nothing to charge for")
	_ = tr
}

func TestMCPSearchBudget_UnpricedModelDoesNotAsk(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 1, 3)
	a.Config.FullConfig.Providers = nil // no schedule, and "mock-model" has no built-in price

	_, handled := gate(t, a, searchCall())
	assert.False(t, handled, "an unpriced model has no number to put on a card")
}

func TestMCPSearchBudget_BudgetOffNeverAsks(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 0, 3)

	_, handled := gate(t, a, searchCall())
	assert.False(t, handled, "cache_budget: 0 turns the confirmation off")
}

func TestMCPSearchBudget_OtherToolsAreUntouched(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 1, 3)
	tr, handled := gate(t, a, llm.ToolCall{ID: "c", Function: llm.ToolCallFunction{Name: "Bash"}})
	assert.False(t, handled)
	assert.Equal(t, tools.ToolResultSuccess, tr.Status)
}

func TestMCPSearchAskPreviewCarriesTheNumbers(t *testing.T) {
	a := newMCPBudgetAgent(t, 100_000, 1, 3)
	est := a.mcpCacheCost()
	require.True(t, est.Priced)
	require.InDelta(t, 9.0, est.Cost, 0.001)

	preview := mcpSearchAskPreview(est, 3, 1)
	for _, want := range []string{"100.0k tokens", "¥9.000", "¥1.000", "3 个未加载"} {
		assert.Contains(t, preview, want)
	}
	// The write price is not charged here (0 = free), so naming it would read as
	// a broken number rather than an omitted fee.
	assert.NotContains(t, preview, "缓存写")
}

// Every call leaves one line carrying the number the card would have quoted — including the
// calls that raise NO card, and including a refusal that was never asked about. "It didn't ask"
// and "it had nothing to ask about" and "this model has no price table" are otherwise the same
// event from the outside, and the estimate exists nowhere else.
func TestMCPSearchBudget_LogsTheCostOnEveryCall(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, logger.Init(dir, *logger.DefaultConfig()))

	a := newMCPBudgetAgent(t, 100_000, 1, 3)
	// Built AFTER Init and injected: a *Logger resolves its file at construction.
	a.Config.Logger = logger.New("agent-budget-test")

	// 1) Over budget → the card, answered with a refusal.
	_, handled := gateWithAnswer(t, a, searchCall(), ConfirmDeny)
	require.True(t, handled)
	// 2) The next call is refused on the session flag — no card this time, and it must still say
	//    what the search would have cost.
	_, handled2 := gate(t, a, searchCall())
	require.True(t, handled2)

	raw, err := os.ReadFile(filepath.Join(dir, "debug.log"))
	require.NoError(t, err)
	var traced, answered []string
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case strings.Contains(l, "Agent: MCP search cost gate answered"):
			answered = append(answered, l)
		case strings.Contains(l, "Agent: MCP search cost gate"):
			traced = append(traced, l)
		}
	}

	require.Len(t, traced, 2, "one line per call, whether or not a card was raised")
	require.Len(t, answered, 1, "the card's answer has its own line")
	assert.Contains(t, traced[0], "decision=ask")
	assert.Contains(t, traced[0], "reason=\"over budget\"")
	assert.Contains(t, traced[0], "tokens=100000", "the size the number was computed from")
	assert.Contains(t, traced[0], "cost=9", "100k tokens × ¥90/M")
	assert.Contains(t, traced[0], "budget=1")
	assert.Contains(t, traced[0], "undiscovered=3")
	assert.Contains(t, answered[0], "answer=deny")
	assert.Contains(t, answered[0], "cost=9", "the figure the answer was made against")

	assert.Contains(t, traced[1], "decision=refuse")
	assert.Contains(t, traced[1], "reason=\"declined earlier this session\"")
	assert.Contains(t, traced[1], "cost=9", "a refusal nobody was asked about still carries the number")
}

// The decisions that leave the call alone are distinguishable in the log — which is the whole
// point of writing the reason down: without it, each of these used to look like the same
// non-event.
func TestMCPSearchBudget_LogsWhyItDidNotAsk(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		setup  func(a *AIAgent)
	}{
		{"under budget", "under budget", nil},
		{"budget off", "budget off", func(a *AIAgent) { a.Config.FullConfig.MCPToolSearch.CacheBudget = 0 }},
		{"no undiscovered tools", "no undiscovered tools", func(a *AIAgent) {
			set := a.Config.MCPManager.SetFor("s1")
			for _, dt := range a.DeferredPool().All() {
				set.Add(dt.Name)
			}
		}},
		{"allowed for the session", "allowed for this session", func(a *AIAgent) {
			a.AllowMCPSearchForSession()
		}},
		{"non-interactive", "non-interactive mode", func(a *AIAgent) {
			a.SetPermissionMode(PermissionModeSkip)
		}},
		// The second shape of "nobody to ask": an agent whose asks DO reach a handler, which
		// refuses them all (an a2a remote turn's denyRemoteAsk). Asking there would silently
		// become "an operator with a budget never lets a caller load a tool", decided by a
		// handler whose job is the SAFETY asks.
		{"no one attached", "no one attached to answer a cost question", func(a *AIAgent) {
			a.Config.NoCostAsks = true
		}},
		{"no price estimate", "no price estimate", func(a *AIAgent) {
			a.Config.FullConfig.Providers = nil
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, logger.Init(dir, *logger.DefaultConfig()))
			// A budget nothing here can exceed: every case must reach its OWN reason rather than
			// the ask (and the small prompt keeps the estimate out of the way of that).
			a := newMCPBudgetAgent(t, 100, 10_000, 3)
			a.Config.Logger = logger.New("agent-budget-test")
			if c.setup != nil {
				c.setup(a)
			}

			if _, handled := gateWithSession(t, a, "s1", searchCall()); handled {
				t.Fatalf("%s: the gate settled the call instead of leaving it alone", c.name)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "debug.log"))
			require.NoError(t, err)
			assert.Contains(t, string(raw), "decision=proceed")
			assert.Contains(t, string(raw), "reason=\""+c.reason+"\"")
			assert.Contains(t, string(raw), "tokens=", "the estimate is logged even when nothing is asked")
		})
	}
}

// gateWithAnswer drives the TUI card the way the TUI does: watch the event
// channel, answer the confirmation, then read the gate's outcome.
func gateWithAnswer(t *testing.T, a *AIAgent, tc llm.ToolCall, resp ConfirmResponse) (tools.ToolResult, bool) {
	t.Helper()
	ch := make(chan AgentEvent, 8)
	type outcome struct {
		tr      tools.ToolResult
		handled bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		tr, handled, err := a.checkMCPSearchBudget(context.Background(), tc, ch)
		done <- outcome{tr, handled, err}
	}()

	select {
	case e := <-ch:
		require.Equal(t, AgentEventToolConfirmation, e.Type)
		assert.Equal(t, tools.ToolNameMCPSearchTools, e.ToolName)
		assert.Contains(t, e.ToolDiff, "¥", "the card body is the agent's cost estimate")
		a.ConfirmTool(resp)
	case o := <-done:
		// Returned without asking — legitimate for the cases that expect no card.
		require.NoError(t, o.err)
		return o.tr, o.handled
	case <-time.After(3 * time.Second):
		t.Fatal("the gate neither asked nor returned")
	}

	select {
	case o := <-done:
		require.NoError(t, o.err)
		return o.tr, o.handled
	case <-time.After(3 * time.Second):
		t.Fatal("the gate never returned after the card was answered")
	}
	return tools.ToolResult{}, false
}

// The wiring, end to end: an over-budget search is refused BEFORE the tool runs,
// the refusal reaches the model as a tool error, and the turn continues — the
// property that separates this gate from the bash ask's errCancelled.
//
// It runs on BOTH executor paths, because the search tool is Parallel(): alone
// in a message the executor routes it through the sequential path, while beside
// another parallel call it runs in a group — and a gate that only guarded one of
// them would be a gate that sometimes does not run.
func TestAgentLoop_MCPSearchBudgetRefusalContinuesTurn(t *testing.T) {
	const sibling = "StubParallel"
	cases := []struct {
		name   string
		calls  []llm.StreamEvent
		shared bool // the search shares its group with the sibling
	}{
		{
			name:  "alone (sequential path)",
			calls: toolCallSeq(tools.ToolNameMCPSearchTools, "call-1", `{"query":"anything"}`),
		},
		{
			name: "beside another call (parallel path)",
			calls: multiToolCallSeq(
				struct{ Name, ID, Args string }{tools.ToolNameMCPSearchTools, "call-1", `{"query":"anything"}`},
				struct{ Name, ID, Args string }{sibling, "call-2", `{}`},
			),
			shared: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mp := &mockStreamProvider{
				name:         "mock",
				providerName: "mock", // the fixture's price schedule is keyed on this name
				sequences: [][]llm.StreamEvent{
					c.calls,
					textSeq("carrying on without it"),
				},
			}
			// A ten-thousandth of a yuan: this test is about the WIRING, and the
			// loop recomputes the estimate from the (tiny) fixture conversation
			// before the call, so the budget has to be small enough that any
			// prompt at all exceeds it (¥0.0001 at ¥90/M is ~1 token).
			a := newMCPBudgetAgentWith(t, mp, 20_000, 0.0001, 3)
			searched, ranSibling := false, false
			a.RegisterTool(&stubTool{
				name:     tools.ToolNameMCPSearchTools,
				parallel: true,
				executeFn: func(context.Context, string) (string, error) {
					searched = true
					return `{"matches":[]}`, nil
				},
			})
			a.RegisterTool(&stubTool{
				name:     sibling,
				parallel: true,
				executeFn: func(context.Context, string) (string, error) {
					ranSibling = true
					return "sibling ok", nil
				},
			})

			ch := a.RunConversationStream(t.Context(), nil, "find a tool", "", llm.ChatOptions{MaxTokens: 4096})
			var result *RunResult
			var events []AgentEvent
			for e := range ch {
				events = append(events, e)
				if e.Type == AgentEventToolConfirmation {
					a.ConfirmTool(ConfirmDeny)
				}
				if e.Type == AgentEventTurnComplete || e.Type == AgentEventError {
					result = e.Result
				}
			}

			require.NotNil(t, result)
			assert.Equal(t, 1, countConfirmations(events), "the cost card must be raised")
			assert.Equal(t, "carrying on without it", result.Response)
			assert.False(t, searched, "the refused search must not reach the tool")
			assert.True(t, toolResultContains(events, true, "不要再调用 MCPSearchTools"))
			assert.Equal(t, ExitReasonStop, result.ExitReason, "a refusal is not a cancelled turn")
			if c.shared {
				assert.True(t, ranSibling, "the refusal must not take its group down with it")
			}
		})
	}
}
