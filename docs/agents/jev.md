# Tachi — Jev Handbook (TypeSafe System One)

Read this **before you call `pkg/jev`**, before you add a semantic gate that a generative model
currently answers by emitting a marker, and **before you trust a threshold anyone — including a
previous session — already swept**.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the
conventions that apply anywhere in the repo and points here for the rest. **New Jev lessons belong in
THIS file, not back in `.tachi.md`** — and the rules that ride with that file (freshness in the same
turn, convention-not-history, split + index when a subject grows) apply to this split too.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **What it is, and where it sits** | you are deciding whether a call site can move to it |
| **When to use it — and when not to** | you are about to reach for it — **start here** |
| **Wiring a call site** | you have picked a call site and are about to write the gate |
| **Measuring a threshold** | you need a number: a cut-off, a limit, a cost. **No number is a fact** |
| **Traps** | something behaves strangely, or you are reviewing a Jev integration |

Related: [`.tachi.md`](../../.tachi.md), and `pkg/jev`'s package doc — the constraint list lives
there, one screen from the code that has to obey it.

## What it is, and where it sits

Jev is an **evaluation** endpoint, not a chat completion. One request carries a `state` and a map of
typed `questions`; the response carries one typed answer per question — a probability (`noul`), a
chosen option plus its full distribution (`choice`), or a position on ordered levels (`score`). **It
never generates text**, and it never explains itself.

That is the whole boundary, and it is why `pkg/jev` deliberately does **not** implement
`llm.Provider`: no messages, no tools, no streaming — folding it into the provider interface would
put two unrelated contracts behind one type and invite call sites that need one to reach for the
other.

**There is one production caller: the MCP tool search** (`agent/mcp/jev_search.go`, matcher
`mcp_tool_search.matcher: auto` with a usable `jev` block). It turns a prose request into one `noul`
question per candidate tool and offers the ones over the cut-off; the question text lives in
`agent/mcp/jevsearch` because the eval case file that produced the cut-off is generated from it, so
the measurement cannot describe a question the gate no longer asks. Its calibration — τ = 0.35, the
corridor it came from, and the case file it was swept on — is recorded in the design doc
(`docs/2026-09-20-jev-tool-search-design.md` §六) and re-measurable with one command. Everything else
in the repo still asks a generative model and parses the reply.

## When to use it — and when not to

**One question decides it: does the caller read the model's output, or show it?**

Ask what happens to the answer. If the very next thing is an `if`, a threshold comparison, or a
`switch` — the model is being used as a **judgment**, and that is Jev's shape. If the answer gets
stored, rendered, logged for a human, or streamed to a UI — it is **generation**, and it stays on
`llm.Provider`, no matter how short or boring the text is.

A second, weaker test removes most of what is left: **can you enumerate the answers in advance?**
Jev picks from options you supply, or answers a yes/no you can state. It cannot invent a label, a
keyword, a file path or a summary — so anything open-ended is generation even when the output is tiny.

**Jev-shaped** (a judgment the code consumes):

- A marker protocol: the caller asks a model for short free text and then string-matches it. In Tachi
  today that is the GitHub channel's `[NO_REPLY]` / `[READY_FOR_PR]` control markers
  (`channel/github/security.go`) and the channel's `[SILENT]` silence marker for ambient turns and
  cron notifications. Every one of these is a generative call wearing a judgment's clothes — the
  reason to move it is that the marker can be faked by the input, mis-typed by the model, or land in
  the text that gets published.
- A binary or small-set gate the prompt already describes in prose: "is this worth interrupting the
  user for", "does this comment need a reply", "which team owns this".
- Reranking a candidate list that **code** produced — the MCP deferred-tool search (BM25 over tool
  metadata) is the standing example. Code guarantees candidate coverage; the judgment picks one.
  `agent/extractor.go`'s search-keyword extractor is the same shape with the missing half: keywords
  are open-ended, so Jev cannot produce them, and it only becomes a candidate once code supplies a
  candidate pool to select from. Until then it stays generation.

**Not Jev-shaped** (leave it alone):

- **Anything whose output is shown or stored.** Session titles, compaction summaries, commit
  messages, review prose, dream's memory consolidation, deep-research reports. All generation.
- **Image and audio description.** `agent/vision_fallback.go` must stay on a multimodal model — see
  the non-text trap below; this is the one place where reaching for Jev produces confident garbage
  rather than an error.
- **A hard safety decision.** `agent/permission/policy.go`'s Bash `deny` is deterministic, and it has
  to stay that way. A probability is not a guarantee, and a `deny` that is right 99% of the time is a
  broken `deny`. Weighted scores and veto rules are different things and must not share a number.
- **Picking something when the caller already has the full context.** Automatic skill selection, for
  instance, is done by the main model reading the catalog; routing that through Jev would discard
  context and gain nothing.

## Wiring a call site

Four things every gate needs, in the order they matter:

1. **A degraded path.** Jev unreachable, over its limit, or answering an error must fall back to the
   call site's existing behaviour, and the fallback must err toward the conservative side of that
   gate's purpose — not toward "silently drop what the user should have seen". Config gates the whole
   thing: `config.JevConfig.Configured()` is enabled-AND-key, so a half-filled section keeps the old
   path everywhere instead of failing per call. The desktop settings page already offers the block
   (`desktop/settings.go`'s single `jev` row, `applyLazy`, under 模型): a gate that reads the config
   when it judges keeps that row honest, while one that builds and caches a client has to turn it into
   `applyPush` + an `Apply` in the same commit — a cached client makes the key a push-shaped fact, and
   the page would otherwise save a value that reaches nothing until the agent is rebuilt. The bound
   reaches the client through `WithHTTPClient(httpx.NewClient(cfg.Jev.Timeout, cfg.Jev.Proxy))`,
   the way `web_search` does it — `pkg/jev` has no `WithTimeout` option.
   The tool-search gate is the worked example of the second half: its searcher is built per agent
   (`agent.mcpSearcher`, `agent/agent_jev.go`), so the desktop's `mcp_tool_search` row calls
   `a.RefreshMCPSearch()` on every save — without it, `matcher` / `jev_threshold` / `jev_budget`
   would read as live settings that only a new agent ever sees.
2. **A pinned model.** `pkg/jev`'s `DefaultModel` is a concrete version; leave a blank config field
   blank so the package default applies. The service's `jev-latest` alias moves, and a threshold
   swept against the old weights silently stops applying when it does.
3. **A catch-all option on every `choice`.** Not a safety net for inputs you forgot — it is what
   calibrates the distribution. Without one the model is forced to pick something and raises its
   confidence to do it, so the threshold you sweep afterwards comes out systematically too high and
   the gate then misses exactly the cases it exists to catch.
4. **Usage accounting.** The `llm` ledger is a `Provider` decorator, and Jev is not a provider, so
   its calls do not appear in it. Deciding how they do is part of wiring the first gate, not
   something that can be bolted on later by copying an existing pattern. How tool search settled it:
   the gate writes the row itself under `llm.UsageKindJev` (session, model, input/output tokens, an
   explicit credit of 0 — Jev earns no 积分 but is not a pre-credit row either), and the price comes
   from the built-in model table, where Jev has a record like any other model
   (`llm/builtin_models.go`: the vendor publishes USD, the table's unit is CNY, so the record carries
   a documented snapshot rate). A gate that skipped the price would log its calls as free, which is
   how a side channel quietly becomes invisible in /usage.

**The input's shape belongs to code, not to the model.** Write the state as the fields the judgment
actually needs, from named sources, rather than handing over whatever prose happens to be lying
around — see the register trap for why this is a correctness requirement and not a tidiness
preference.

## Measuring a threshold

**No threshold in this repo is a fact, and none may be copied from anywhere — not from these docs,
not from the local `jev` skill, not from another gate.** A confidence number means "how concentrated
the distribution is over the options THIS caller supplied"; adding, removing or rewording an option
rescales it, and so does a model version change. Every cut-off has to be swept against the caller's
own options and its own cases.

`pkg/jev` ships the harness for that:

```sh
# one case set (the usual way — start with the file you are working on)
JEV_API_KEY=... JEV_EVAL_FILE=notify-gate.json go test ./pkg/jev/ -run TestJevEval -v

# everything under pkg/jev/testdata/
JEV_API_KEY=... go test ./pkg/jev/ -run TestJevEval -v

# without a key this test skips, so `go test ./...` stays offline
JEV_EVAL_REPEATS=10        # default 5; below 5 the stability numbers mean nothing
JEV_EVAL_MODEL=jev-x.y.z   # re-measure a calibration against a new version
```

It reports, per case, what was answered, how sure the model was of the right answer, and the
**range** of the action probability across repeats; then the threshold sweep, the cases whose range
straddles a candidate cut-off, and a comparison across the forms of a `pair`. It asserts nothing
about the numbers — a harness that fails on poor calibration teaches people to ignore it — and fails
only on request errors and malformed case files.

**Two rules for using it, both learned the expensive way:**

- **To separate N dimensions you need the full grid, and repeats are not a substitute.** Two arms
  that share a confounded variable will agree, and they will keep agreeing no matter how many times
  you re-run — reproducibility measures the condition you held fixed, not the one you meant to vary.
  A two-arm comparison of a confounded design produces a tight, confident, wrong number.
- **Stability is a within-run property; a gate is exposed to the between-run one.** Repeats of the
  same request can be identical while a re-run of the whole file moves an aggregate by more than the
  drift tolerance. Prefer a cut-off from the MIDDLE of the separating run, not its edge — the edge is
  where the next run walks a case across.

`TestEvalCaseFilesAreWellFormed` runs offline and checks the case files before anyone spends calls on
them: a typo in a label otherwise shows up as "the gate never separated anything", which reads
exactly like a model failure.

**Where a swept threshold is recorded**: with the gate, as a comment on the config field it became —
τ, the case file it was swept on, and the model version, because none of the three can move without
invalidating the number. The tool search's is `mcp_tool_search.jev_threshold`, and its corridor,
per-case table and cost are in `docs/2026-09-20-jev-tool-search-design.md` §六 (one command re-runs it:
`JEV_EVAL_FILE=tool-search-noul.json`). A question whose text is per-case goes through the case-level
`instructions` override, and the generator must build that text by calling the production function —
otherwise the file measures a prompt the gate never sends.

## Traps

**Non-text state fails silently and confidently.** An image sent as base64 returns HTTP 200 with a
plausible answer and a *higher* confidence than the same request carrying noise — the answer was read
off nothing. Convert images and audio to text with an OCR/CV pipeline first. Only 4xx/5xx are
trustworthy signals; a 200 with a high confidence is not evidence of a correct answer.

**The tighter of the two token limits is the one that binds.** The service documents a per-request
budget and a smaller `state`-plus-longest-question budget, and it is the smaller one a large request
hits first — well before the total is anywhere near its cap. Keep `state` plus the LONGEST single
question under it, and prefer many short questions over one enormous one; read `Response.Usage` for
what a request actually cost rather than estimating. For the gates Tachi has measured, requests run
to hundreds of tokens, so this binds only when `state` grows into real document text.

**Register beats language, by a lot.** The same judgment written as a job report and as a colleague's
note moves the probability far more than translating it does — the language gap stays inside the
drift budget while the register gap flips decisions, in both languages symmetrically. So a gate fed
uncontrolled prose is not thresholable at all: its input is a variable nobody is holding fixed. This
is the reason "the input's shape belongs to code": pass a fixed summary assembled from named fields,
never raw model prose whose tone is whatever the last turn felt like. The evidence and the case set
live in `pkg/jev/testdata/notify-phrasing.json`, which is a 2×2 of language and register — the plain
arms are casual *and* lightly minimizing, so read it as register-plus-tone, not register alone.

**An empty option list is not the failure mode you will notice.** The gate that mislabels an input
and the gate that mislabels everything look the same in a summary; look at the per-case table, and
check that the catch-all option is actually absorbing the junk rather than the model forcing those
inputs into a real category.

**A verdict about a pair is about the argmax.** Two forms of one judgment sitting on different labels
means they disagree by default, not that no gate can serve them: a cut-off above the higher form
still decides both the same way. Check the sweep before concluding a pair is unservable.
