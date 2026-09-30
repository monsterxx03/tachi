# Tachi — MCP Handbook (split out of `.tachi.md`)

Read this **before touching anything under `agent/mcp/`, the search tool (`agent/tools/mcp_tool_search.go`),
or the deferred-tools reminder (`agent/systemreminder/deferred_tool.go`)**. The CLIENT half — OAuth, the
authorize button, the panel, `roots` — lives in [desktop.md](desktop.md) (Desktop MCP) and is pointed at from
here rather than repeated.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New MCP lessons belong in THIS file, not back in
`.tachi.md`** — and the rules that ride with that file (freshness in the same turn, convention-not-history,
split + index when a subject grows) apply to this split too.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **The pool, and the reminder that advertises it** | you add/remove tools, toggle a server, or change the reminder's shapes |
| **The search** | you touch `MCPSearchTools`, either searcher, or the `server` narrowing |
| **The result the model reads** | you change what a search returns |
| **The schema the provider sees** | you touch how an MCP tool becomes an `llm.Tool` |
| **What loading costs, and the budget card** | you touch the cost gate (`agent/mcp_budget.go`), `mcp_tool_search.cache_budget`, or the confirm card's copy |

## The pool, and the reminder that advertises it

- **A tool in the pool is INVISIBLE to the model until the reminder says so: any path that adds deferred
  tools MUST also call `NotifyDeferredToolsAdded()`.** `DeferredToolReminder` fires at most once per session,
  so a mid-session addition (a server toggled on, a profile switch, a refresh) reaches the model only through
  the dirty mark — write the pool alone and the tools stay unreachable for the rest of the conversation,
  silently. `agent.AddDeferredMCPTools` is the entry point that does both; a frontend that adds pool entries
  by hand (as `desktop.SetMCPServerEnabled` did) is the bug. Everything that marks it dirty goes through
  `ensureDeferredReminder`, which also owns the once-only registration — `Collector.AddReminder` only
  appends, so registering per notification runs the same reminder N times per message.
- **Registration must not be gated on the pool being non-empty.** A shared pool (desktop, channel) is
  populated in the BACKGROUND, so an agent built first sees an empty one; the reminder is what copes with
  emptiness, so it is registered unconditionally and stays silent until it has something to hint about.
  `desktop/agent_mcp_test.go` (a stub stdio server run by the test binary itself) and
  `agent/agent_deferred_test.go` pin the enable, disable and late-arrival paths.
- **`min_tools_for_search` cannot be written as 0** — `defaults.Set` runs after the YAML and fills every
  ZERO value from the tag, so an explicit `0` comes back as the default `5`. The auto-load test is
  `len(pool) > threshold`, so the value meaning "never auto-load" is a NEGATIVE one; a fixture that needs a
  tool to STAY in the pool (the smoke suite's `perm-mcp-budget`, which is about the cost card in front of a
  search that could load something) sets `-1` for exactly that reason.
- **Toggling a server is a GLOBAL fact** — one pool and one connection serve every session — so the desktop
  notifies all live agents (`applyToLiveAgents()`), not just the displayed one; and disabling runs
  `UnregisterMCPServer` (registry + every session's discovered set), not a bare pool removal.
- **The reminder's shape follows the matcher** (`agent.mcpSearchUsesJev`): per-server summary lines while the
  Jev gate is healthy, the per-tool catalog (plus a server roster) under keywords. The catalog is not a
  fallback aesthetic — the NAMES are what a keyword search matches on, and a summary line would leave that
  matcher nothing to hit.
- **Both shapes open with `serverLegend`**, which says those lines are the values of the search tool's
  `server` parameter. The tool description points at this block for them ("named exactly as the deferred-tools
  block lists them"), and a pointer into a list that never labels itself is how `mcp__github — 30 个工具（…）`
  gets read as a TOOL rather than a server. Do not drop the legend as redundant.
  The roster line is rendered only when two or more servers still hold undiscovered tools: a roster of one is
  not a choice, and the prefix is already visible in every tool name.
- **Pieces are separated by one blank line** (`RenderPieces`), which is also why a piece must not carry its
  own leading blank. The block's bytes are a wire format — stored as sent, and replayed by
  `ConvertSessionToLLMMessages` — so the rule that a rendering change is NOT structural (only the block being
  built now changes, and that one is at the tail) lives in [prompt-cache.md](prompt-cache.md).

## The search

- **Two halves, and only one of them is narrowed.** A query that NAMES what it wants (`select:`, an exact or
  suffix tool name, an `mcp__server` prefix, the empty-query listing) is resolved against EVERY server: a name
  is an identity, and a mis-remembered server must not turn a hit into a miss. Prose is what `server` narrows.
- **`server` exists to keep the Jev gate affordable** — a candidate is one question, so a full pool is one
  request per hundred tools. `tools.NormalizeMCPServer` normalizes what the model wrote (`mcp__GITHUB`,
  ` github ` → `github`), and the searcher receives `tools.MCPSearchOptions`. An unknown `server` is an ERROR
  naming the servers that exist (`tools.MCPServerLister`, implemented by both real searchers), never a
  silently empty result that reads as 「没有这个工具」.
- **Do not try to talk the model into passing `server`.** It has not once passed it in practice, under two
  different wordings (a sentence in the tool description, then a legend in the reminder block), while `select:`
  is in routine use — because `server` only RESTRICTS the model's own search space and the payoff (the Jev
  bill) is not one it can perceive. A narrower search has to be inferred in CODE, not requested in prose.
- **Narrowing does not invalidate the swept cut-off** (it removes candidates, it does not reword a question):
  the reasoning is in [jev.md](jev.md)'s "Measuring a threshold" section, together with the cost of a full-pool
  search and the degradation rules (budget, failure, unanswered questions).
- **Every judgment request leaves one log line** (`jev_search.go`'s `logAnswers`): query, the model that
  answered, chunk i/n, the scope, threshold, tokens, then `answers="name=0.912, …"` in probability order — an
  unanswered question is written `name=-`, never `0.000` (not judged is not judged low).
- **A run with no session ID cannot record what it finds, and is a known gap rather than a supported path.**
  `Manager.TrackerFor("")` returns nil for exactly that reason (the empty-ID check avoids boxing a typed nil),
  and the same ID key builds the NEXT request's tool list (`filterActiveSchemas`) — so a side-channel run
  (dream, `/review`, an ambient turn) never gets a found tool into its tool list, while the executor would
  still register it from the pool on first use (`lazyRegisterMCPTool`), i.e. without a schema to check the
  arguments against. The comment on `TrackerFor` carries the same warning.

## The result the model reads

- **The result is a MENU: `{"matches": [{"name", "matched_by", "score"}]}` and nothing else.** No
  description, no parameter schema, no `tool_definitions` block, no echoed `query`, no count. None of it is
  needed to CALL the tool: a found tool reaches the NEXT request's tool list with its full schema from the
  pool (`filterActiveSchemas`), and a call registers it from that same pool (`lazyRegisterMCPTool`). The
  definitions were half the payload, and the payload lives in the transcript for the rest of the session —
  so `TestMCPSearchToolsTool_Execute_Success` pins the ABSENCE. Re-adding any of it looks harmless and
  doubles every search, permanently.
- **`matched_by` always rides with `score`** (`name` | `judgment` | `keywords`): a noul probability and a BM25
  score are different quantities, so a bare number invites a comparison across searches that means nothing. A
  name match carries no score at all (a lookup is not ranked). Scores are rounded to three decimals, the same
  precision the judgment's own log line uses.
- **The clause framing the score (`tools.ScoreAdvice`) is load-bearing and must not be trimmed with other
  schema words**: every result already passed the cut-off, so a model that reads a low score as a verdict
  drops exactly the marginal tool the threshold was swept to include, then spends a turn searching again.

## The schema the provider sees

- **An MCP tool's parameter type must reach the provider as ONE plain string, and a property that names none
  must arrive WITHOUT the keyword.** Servers declare a nullable parameter as a union (`["null","array"]`) or as
  a composition with no `type` at all (pydantic emits `anyOf:[{type:integer},{type:null}]` for `Optional[int]`),
  so `agent/mcp/tool.go`'s `propertyType` resolves both to the union's non-null member; a `$ref`-only property
  stays typeless, and `llm.ToolParameterProperty.Type` is `omitempty` because `"type": ""` is invalid JSON
  Schema — one such property makes the provider reject the ENTIRE request, so every tool in the list dies with
  it, not just the offending one (`TestBuildLLMTools_EmptyTypeNeverReachesTheProvider`,
  `TestMCPToolProperties_ComposedSchemaType`).

## What loading costs, and the budget card

- **Loading a tool is the one change that re-reads the WHOLE prompt once.** Tools are the first segment of
  the prefix (tools → system → messages), so a tool that was not in the previous request turns everything
  after its position into a miss — and the messages are behind the tools. `mcp_tool_search.cache_budget`
  (CNY per search, 0 = off) is what the user accepts: over it, the call is confirmed first, on the TUI, ACP
  and desktop paths (`agent/mcp_budget.go`, called from both executor paths).
- **The estimate is `tokens × (input + cache-creation − cache-read)` per 1M**, with `tokens` from
  `convState.contextEstimate()` — the REAL-anchored number, not the raw character estimate, because a budget
  decision is where that estimate's known bias would show up as a wrong yuan figure — and the price pinned
  with `ResolveModelPriceAt(now)` (time-of-use bands included). An unpriced model, or a delta ≤ 0 (cache read
  at least as expensive as a miss), raises NO card: there is no number to put on it, and inventing one is
  worse than not asking.
- **It asks only when a search could actually move the tool list**: zero undiscovered pool tools (the pool
  minus the session's discovered set, keyed by the SAME session ID that builds the next request's list,
  `tools.SessionIDFromCtx`) means a search can only return names already in it. Whether a SPECIFIC query
  would load anything is the search's own answer, so the card says 「可能加载」.
- **It is a COST gate, not a safety one, and three properties follow.** `PermissionModeSkip` never gates —
  nobody is at the console in channel / `tachi -p` / a subagent, and a spend question with no one to answer
  is a hang, not a question. A refusal returns a tool RESULT and the turn continues (the bash ask instead
  aborts with `errCancelled`), because the model is the reader that has to learn why the tools are gone. And
  the refusal is remembered for the session (`PermissionState.MCPSearchBlocked`): a refusal the next call
  could undo by asking again would be a button that cannot deliver. 「本会话允许」 is its counterpart — the
  TUI reaches the agent as `ConfirmAllowSession`, while the ACP/desktop handlers answer only a bool and call
  `AllowMCPSearchForSession()` themselves. **Both are cleared by `ClearSession` (`/new`), beside the bash
  policy's own session approvals**: an answer belongs to the conversation that gave it, and a refusal that
  outlived its conversation left a user who changed their mind with no way back short of a restart. The
  desktop builds one agent per session, so it gets a fresh gate without that call.
- **Asks are serialized per agent (`PermissionState.askMu`).** Bash never needed this (its tool is
  non-parallel, so its asks are already one at a time); MCPSearchTools IS parallel, so two searches in one
  group can be parked at once — and the TUI answers every card through ONE channel
  (`Channels.ConfirmResp`, capacity 1), where the second card would otherwise consume the first one's answer.
- **Every call leaves one log line, card or no card**: `Agent: MCP search cost gate` with
  `decision` (proceed/ask/refuse) + `reason` + `tokens` + `cost` + `budget` + `undiscovered` +
  `priced`, and an ask leaves a second one (`... gate answered`) carrying the user's choice. The
  facts it needs are therefore gathered BEFORE the early returns. This is what makes the gate
  auditable: "it didn't ask" and "nothing left to load" and "no price table for this model" are
  otherwise the same non-event from the outside, and the estimate — the number the whole feature
  is about — exists nowhere else.
- **The card's chrome is per kind, its body never is.** `desktop.PermissionEvent.Kind` (`bash` /
  `mcp_search`, derived from the tool name) selects the headline and button hints; the preview stays the
  agent's own rendering, shown verbatim — the same rule that keeps `bashAskPreview` the only writer of its
  format. The TUI branches on the tool name for the same two strings. The whole card (copy, the numbers on
  it, a refusal sticking across a second call) is pinned end-to-end by the desktop smoke scenario
  `perm-mcp-budget`.

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md) (MCP client, OAuth, the panel),
[jev.md](jev.md) (the gate and its threshold discipline), [prompt-cache.md](prompt-cache.md) (the reminder
block's seam).
