package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
)

// The MCP search cost gate: one search may load a tool, and loading a tool
// invalidates the whole prompt cache once.
//
// Loading a tool touches the ONE part of a request that sits before everything
// else — the prefix is tools → system → messages (docs/agents/prompt-cache.md) —
// so a tool that was not in the previous request's list turns every token after
// its position into a cache miss. The messages are behind the tools, so that is
// the ENTIRE prompt, re-read once. It is paid in money, not in 积分 (credit is
// charged on total tokens either way), and mcp_tool_search.cache_budget is what
// the user set as acceptable per search.
//
// It is a COST question, not a safety one, and that is what shapes it:
//
//   - PermissionModeSkip does not gate at all. Nobody is at the console in
//     channel / `tachi -p` / subagents, and "would you like to spend ¥2" with no
//     one to answer is not a question — it is a hang. The pre-gate behaviour
//     (search proceeds) is the one that stays.
//   - A refusal feeds the model an error and lets the turn CONTINUE, unlike the
//     bash ask, whose refusal aborts the turn (errCancelled). The point of a
//     refusal here is to tell the model why the tools are gone, and a dead turn
//     cannot hear it.
//   - The refusal is remembered for the session: a refusal that the next model
//     turn could simply undo by calling again would be a button that cannot
//     deliver. 「本会话允许」 is its counterpart, and it is also per session.
//
// The gate runs BEFORE the search (it is the executor's pre-invoke check, next
// to the bash policy one), so it can only ask about a search that COULD load
// something: whether a specific query would actually do so is what the search
// itself decides, and the pool still holding undiscovered tools is the honest
// pre-check for it.

// mcpSearchBudgetKey is the config field the refusal points at, so the reader
// of a refusal knows exactly what to raise.
const mcpSearchBudgetKey = "mcp_tool_search.cache_budget"

// mcpCacheEstimate is what one cache invalidation would cost right now: the
// prompt size that would be re-read, the price snapshot it is priced with, and
// the resulting cost.
//
// Priced is separate from Cost because "the model has no price table" and "the
// prices say a cache miss is free" are different answers: the first means we
// cannot put a number on the card at all (no card is raised), the second means
// the number is genuinely zero.
type mcpCacheEstimate struct {
	Tokens int64
	Cost   float64
	Price  llm.ModelPrice
	Priced bool
}

// mcpCacheCost estimates what invalidating the prompt cache would cost right
// now. A zero Tokens (no conversation yet) or an unpriced model yields an
// estimate the gate treats as "cannot ask".
func (a *AIAgent) mcpCacheCost() mcpCacheEstimate {
	p := a.Provider()
	if p == nil {
		return mcpCacheEstimate{}
	}
	// contextEstimate (not tokens) is the number the frontends report: the last
	// call's REAL prompt size, moved by the estimate's own delta since then. The
	// raw character estimate is biased on mixed content (see convState), and a
	// budget decision is exactly where that bias would show up as a wrong yuan
	// figure on a card.
	tokens := a.conv.contextEstimate()
	if tokens <= 0 {
		return mcpCacheEstimate{}
	}
	rp := llm.ResolveModelPriceAt(a.Config.FullConfig, p.ProviderName(), p.Model(), time.Now())
	if !rp.HasPrice() {
		return mcpCacheEstimate{Tokens: tokens}
	}
	est := mcpCacheEstimate{Tokens: tokens, Price: rp.Price, Priced: true}
	est.Cost = cacheInvalidationCost(tokens, rp.Price)
	return est
}

// cacheInvalidationCost prices one full re-read of a prompt that was fully
// cached.
//
// The tokens are billed at the miss price AND written back into the cache,
// where a hit had cost the cache-read price — so the delta per token is
// input + creation − read. A number ≤ 0 means this model's cache read is at
// least as expensive as a miss (or nothing here is charged at all), i.e. a
// tool-list change costs the user nothing and there is nothing to ask about.
func cacheInvalidationCost(tokens int64, price llm.ModelPrice) float64 {
	perToken := price.InputPrice + price.CacheCreationInputPrice - price.CacheReadInputPrice
	if perToken <= 0 {
		return 0
	}
	return float64(tokens) / 1_000_000 * perToken
}

// mcpUndiscoveredTools counts the pool tools this session has not loaded —
// the tools a search could still add.
//
// sessionID is the tool-call context's (tools.SessionIDFromCtx), because that
// is the key the NEXT request's tool list is built from
// (filterActiveSchemas), so it is the set whose growth would move the prefix.
// Zero means every deferred tool is already in the tool list: a search can only
// return names it has already returned, the list does not change, and the cache
// is not broken — the one case where asking about the cost would be asking
// about nothing.
func (a *AIAgent) mcpUndiscoveredTools(sessionID string) int {
	pool := a.DeferredPool()
	if pool == nil {
		return 0
	}
	set := a.discoveredSetFor(sessionID)
	n := 0
	for _, dt := range pool.All() {
		if set == nil || !set.Contains(dt.Name) {
			n++
		}
	}
	return n
}

// AllowMCPSearchForSession records 「本会话允许」: the cost card is not raised
// again in this session. Called by the frontends that own the three-way answer
// (the TUI reaches the agent through ConfirmTool; the desktop and ACP handlers
// return only a bool, so they report this separately).
//
// Nil-tolerant like the other setters a frontend calls directly: a bare agent
// has no PermissionState, and a decision about a session it does not have is not
// an error worth panicking over.
func (a *AIAgent) AllowMCPSearchForSession() {
	if a == nil || a.PermState == nil {
		return
	}
	a.PermState.MCPSearchAllowed = true
	a.PermState.MCPSearchBlocked = false
}

// BlockMCPSearchForSession records a refusal, which is what makes it stick: no
// further card, and no further search either (see checkMCPSearchBudget).
func (a *AIAgent) BlockMCPSearchForSession() {
	if a == nil || a.PermState == nil {
		return
	}
	a.PermState.MCPSearchBlocked = true
	a.PermState.MCPSearchAllowed = false
}

// checkMCPSearchBudget is the pre-invoke gate for MCPSearchTools, shaped like
// checkBashPermission: it returns (zero, false, nil) to let the call proceed
// normally, or a tool result plus true when it has already decided the call's
// outcome. It never returns an error that aborts the turn — a refusal here is
// information for the model, not the end of the conversation.
func (a *AIAgent) checkMCPSearchBudget(ctx context.Context, tc llm.ToolCall, ch chan<- AgentEvent) (tools.ToolResult, bool, error) {
	if tc.Function.Name != tools.ToolNameMCPSearchTools {
		return tools.ToolResult{}, false, nil
	}
	// The facts are gathered FIRST, before any early return, so that every call ends in exactly
	// one trace line — including the calls that raise no card. A gate that only speaks when it
	// asks (or refuses) cannot be audited: "it didn't ask" and "it had nothing to ask about" and
	// "the price table is missing" all look the same from the outside, and the numbers are what
	// tell them apart. The estimate is cheap (a context read plus a price lookup) and is not
	// gated on any of the checks below for that reason.
	est := a.mcpCacheCost()
	budget := a.mcpCacheBudget()
	undiscovered := a.mcpUndiscoveredTools(tools.SessionIDFromCtx(ctx))

	// trace is the one line per call. decision is what the gate DID to the call
	// (proceed/ask/refuse) and reason is why; the numbers are what the card quotes when there
	// is one, so a reader can reconstruct any decision — including a refusal that was never
	// asked about.
	trace := func(decision, reason string) {
		a.Config.Logger.Info(ctx, "Agent: MCP search cost gate",
			"decision", decision, "reason", reason,
			"tokens", est.Tokens, "cost", roundCNY(est.Cost), "budget", budget,
			"undiscovered", undiscovered, "priced", est.Priced)
	}

	// Nobody to ask: channel, `tachi -p`, subagents, dream. See the file comment. The estimate
	// is still logged — that is the number this search WOULD have been charged for.
	if a.Config.PermissionMode == PermissionModeSkip {
		trace("proceed", "non-interactive mode")
		return tools.ToolResult{}, false, nil
	}
	if a.PermState.MCPSearchBlocked {
		trace("refuse", "declined earlier this session")
		return tools.ToolResult{
			Status: tools.ToolResultError,
			Err:    mcpSearchRefusal(est, budget),
		}, true, nil
	}
	if a.PermState.MCPSearchAllowed {
		trace("proceed", "allowed for this session")
		return tools.ToolResult{}, false, nil
	}
	if budget <= 0 {
		trace("proceed", "budget off")
		return tools.ToolResult{}, false, nil
	}
	if undiscovered == 0 {
		// Nothing left to load: a search cannot change the tool list, so the cache cannot break.
		trace("proceed", "no undiscovered tools")
		return tools.ToolResult{}, false, nil
	}
	if !est.Priced || est.Cost <= 0 {
		// No price table for this model, or a cache read as expensive as a miss: there is no
		// number to put on a card, and inventing one would be worse than not asking.
		trace("proceed", "no price estimate")
		return tools.ToolResult{}, false, nil
	}
	if est.Cost < budget {
		trace("proceed", "under budget")
		return tools.ToolResult{}, false, nil
	}

	preview := mcpSearchAskPreview(est, undiscovered, budget)
	trace("ask", "over budget")

	// Serialized: every card this agent raises goes through ONE answer channel
	// per frontend (see PermissionState.askMu).
	a.PermState.askMu.Lock()
	defer a.PermState.askMu.Unlock()

	// answer traces how the card came back. It is a second line rather than a rewrite of the
	// first one: the ask line says what the user was asked to spend, and this one says what
	// they decided — a log that only kept the second would lose the figure the answer was made
	// against.
	answer := func(decision string) {
		a.Config.Logger.Info(ctx, "Agent: MCP search cost gate answered",
			"answer", decision, "tokens", est.Tokens, "cost", roundCNY(est.Cost), "budget", budget)
	}

	switch a.Config.PermissionMode {
	case PermissionModeExternal:
		approved, err := a.PermState.PermissionHandler(ctx, tc.Function.Name, tc.ID, preview, tc.Function.Arguments)
		if err != nil {
			answer("error: " + err.Error())
			return tools.ToolResult{Status: tools.ToolResultError, Err: err}, true, nil
		}
		a.dispatchPermissionResult(ctx, tc, approved)
		if approved {
			answer(mcpSearchExternalAnswer(true, a.PermState))
			return tools.ToolResult{}, false, nil
		}
		answer(mcpSearchExternalAnswer(false, a.PermState))
		a.BlockMCPSearchForSession()

	default: // PermissionModeTUI
		a.dispatchEvent(ctx, hooks.EventPermissionRequest, hooks.Payload{
			ToolName: tc.Function.Name,
			ToolID:   tc.ID,
			ToolArgs: tc.Function.Arguments,
		})
		ch <- AgentEvent{
			Type:     AgentEventToolConfirmation,
			ToolName: tc.Function.Name,
			ToolID:   tc.ID,
			ToolArgs: tc.Function.Arguments,
			ToolDiff: preview,
		}

		select {
		case resp := <-a.Channels.ConfirmResp:
			a.dispatchPermissionResult(ctx, tc, resp != ConfirmDeny)
			switch resp {
			case ConfirmDeny:
				a.BlockMCPSearchForSession()
			case ConfirmAllowSession:
				a.AllowMCPSearchForSession()
			}
			answer(mcpSearchConfirmName(resp))
			if resp != ConfirmDeny {
				return tools.ToolResult{}, false, nil
			}
		case <-ctx.Done():
			answer("cancelled")
			return tools.ToolResult{Status: tools.ToolResultError, Err: ctx.Err()}, true, nil
		}
	}

	return tools.ToolResult{
		Status: tools.ToolResultError,
		Err:    mcpSearchRefusal(est, budget),
	}, true, nil
}

// mcpCacheBudget is the live budget (the settings object is mutated in place by
// a save, so reading it here is what makes a pushed change take effect — the
// same reason mcpSearchConfig reads it per call).
func (a *AIAgent) mcpCacheBudget() float64 {
	if cfg := a.Config.FullConfig; cfg != nil {
		return cfg.MCPToolSearch.CacheBudget
	}
	return 0
}

// roundCNY trims a cost to the precision money is discussed in — the same three decimals the
// card quotes. It is not cosmetic: a raw float64 puts `cost=2.6999999999999997` next to the
// ¥2.700 the reader just approved, and a log that disagrees with the card in the digits that
// are PRINTED is a log someone will go and "fix".
func roundCNY(v float64) float64 { return math.Round(v*1000) / 1000 }

// mcpSearchExternalAnswer names what an external handler's card decided.
//
// The handler answers with a bool — 「本会话允许」 reaches the agent out of band, because the
// desktop's card flips the switch itself (AllowMCPSearchForSession) — so the label is read back
// from the switch that was actually flipped rather than guessed from the bool. It only ever
// labels a log line, which is why the fallback to allow_once is acceptable here.
func mcpSearchExternalAnswer(approved bool, st *PermissionState) string {
	switch {
	case !approved:
		return "deny"
	case st != nil && st.MCPSearchAllowed:
		return "allow_session"
	default:
		return "allow_once"
	}
}

// mcpSearchConfirmName names the TUI's answer (y / a / n) for the same trace line.
func mcpSearchConfirmName(resp ConfirmResponse) string {
	switch resp {
	case ConfirmDeny:
		return "deny"
	case ConfirmAllowSession:
		return "allow_session"
	default:
		return "allow_once"
	}
}

// mcpSearchAskPreview is the card's body — what the TUI prints under its
// confirmation prompt and what the desktop shows verbatim (a second renderer
// would be free to drift from the numbers this one computed).
//
// It says what the cost IS before it asks: the reader is being asked for money,
// so "整段重读" and its size and unit price belong on the card, not behind a
// second question.
func mcpSearchAskPreview(est mcpCacheEstimate, undiscovered int, budget float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "执行 MCPSearchTools 可能加载新工具（池中还有 %d 个未加载的 MCP 工具）。\n", undiscovered)
	b.WriteString("MCP 工具的 schema 排在请求最前面，新工具一插进去，它之后的整段缓存前缀就全部失效，\n")
	b.WriteString("下一次请求要整段重读：\n\n")
	fmt.Fprintf(&b, "  当前上下文  %s tokens\n", cmds.FormatTokens(est.Tokens))
	fmt.Fprintf(&b, "  本次多花    ¥%.3f%s\n", est.Cost, mcpPriceNote(est.Price))
	fmt.Fprintf(&b, "  预算阈值    ¥%.3f\n\n", budget)
	b.WriteString("允许 → 本次搜索照常执行；拒绝 → 本会话不再加载 MCP 工具，也不再询问。")
	return b.String()
}

// mcpPriceNote renders the unit prices the estimate used, so a surprising number
// can be checked against the model's own table instead of trusted. The cache
// write price is named only when it is charged: a 「缓存写 ¥0.000/M」 reads like a
// broken number rather than the "not charged" that a zero means everywhere in
// the price tables.
func mcpPriceNote(price llm.ModelPrice) string {
	note := fmt.Sprintf("（输入 ¥%.3f/M、缓存读 ¥%.3f/M", price.InputPrice, price.CacheReadInputPrice)
	if price.CacheCreationInputPrice > 0 {
		note += fmt.Sprintf("、缓存写 ¥%.3f/M", price.CacheCreationInputPrice)
	}
	return note + "）"
}

// mcpSearchRefusal is the tool result a declined (or session-wide blocked)
// search returns. It carries the reason AND the instruction, because the model
// is the only reader: without the second half it retries the same search, and a
// retry is refused again with no progress.
func mcpSearchRefusal(est mcpCacheEstimate, budget float64) error {
	numbers := ""
	if est.Priced && est.Cost > 0 {
		numbers = fmt.Sprintf("（整段前缀约 %s tokens，重读一次约 ¥%.3f）", cmds.FormatTokens(est.Tokens), est.Cost)
	}
	return fmt.Errorf("用户拒绝了本次 MCP 工具加载：新工具会插到请求最前面，让整段 prompt 缓存失效%s，"+
		"预估超过预算 ¥%.3f。本会话已停止 MCP 工具加载 —— 不要再调用 MCPSearchTools；"+
		"请用当前已有的工具（含已加载的 MCP 工具）继续完成任务，或让用户调高 %s",
		numbers, budget, mcpSearchBudgetKey)
}
