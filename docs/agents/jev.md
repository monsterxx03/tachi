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

**There are four production callers.** The MCP tool search (`agent/mcp/jev_search.go`, matcher
`mcp_tool_search.matcher: auto` with a usable `jev` block) turns a prose request into one `noul`
question per candidate tool and offers the ones over the cut-off; the question text lives in
`agent/mcp/jevsearch` because the eval case file that produced the cut-off is generated from it, so
the measurement cannot describe a question the gate no longer asks. The cron notify gate
(`channel/manager/notify_gate.go`, matcher `cron.notify_matcher: auto` with the same block) judges a
finished `notify=when_relevant` run, with the question and state in `channel/manager/notifygate`; its
counterpart to that file is `cron.jev_threshold`. The Bash judgment gate
(`agent/agent_permission_jev.go`, `permissions.bash.jev: auto`) asks whether a command a rule did not
deny should be shown to the human first — with the question and state in `agent/permission/jevask` and
the threshold in `permissions.bash.jev_threshold`. The FILE gate
(`agent/agent_permission_files.go`, `permissions.files.jev: auto`) asks the same question about one
`ReadFile` / `WriteFile` / `EditFile` call, with its question and state in `agent/permission/jevfiles`,
its threshold in `permissions.files.jev_threshold`, and its own case set
(`pkg/jev/testdata/files-ask.json`). Those two are the callers whose answer can only escalate to a
human and never block — a deny is not theirs to make — and the two question packages are SIBLINGS
rather than one shared package on purpose: a shared renderer would let a reworded sentence void both
thresholds at once. Which facts belong in either state, and why, is [`permission.md`](permission.md).
**No cut-off is a fact, and none belongs to its gate**: the tool search's τ = 0.30 belongs to a POOL,
the notify gate's, the bash gate's and the file gate's to their own case sets — and the bash and file
numbers are independent even though both were swept against `jev-1.13.0`: they answer different
questions.
Read [Measuring a threshold](#measuring-a-threshold) before copying any of them anywhere, and re-sweep
when the thing it was measured on changes materially. Everything else in the repo still asks a
generative model and parses the reply.

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
  (`channel/github/security.go`) and the channel's `[SILENT]` silence marker for ambient turns — cron
  notifications, the other `[SILENT]` site, moved to the notify gate below (the marker survives there
  as the fallback, which is not the same thing as being the decision). Every one of these is a
  generative call wearing a judgment's clothes — the reason to move it is that the marker can be faked
  by the input, mis-typed by the model, or land in the text that gets published.
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

Five things every gate needs, in the order they matter:

1. **A degraded path.** Jev unreachable, over its limit, or answering an error must fall back to the
   call site's existing behaviour, and the fallback must err toward the conservative side of that
   gate's purpose — not toward "silently drop what the user should have seen". Config gates the whole
   thing: `config.JevConfig.Configured()` is enabled-AND-key, so a half-filled section keeps the old
   path everywhere instead of failing per call. The desktop settings page already offers the block
   (`desktop/settings.go`'s single `jev` row, under 模型). **Where a gate reads the config is what
   decides that row's mode**: read it when it judges and the row stays `applyLazy`; build and keep a
   client from it and the row is `applyPush` + an `Apply`, in the same commit — a cached client makes
   the key a push-shaped fact, and the page would otherwise save a value that reaches nothing until
   the agent is rebuilt. The tool-search gate is that second case on both rows it touches: its client
   and its searcher are built per agent (`agent.mcpSearcher`, `agent/agent_jev.go`), so the `jev` row
   and the `mcp_tool_search` row each call `a.RefreshMCPSearch()` on every save — without it,
   `matcher` / `jev_threshold` / `jev_budget` / a key would read as live settings that only a new
   agent ever sees. The ledger half is the exception in the other direction: a row's price and credit
   rate are read where the row is written, so pricing needs no push. The bound
   reaches the client through `WithHTTPClient(httpx.NewClient(cfg.Jev.Timeout, cfg.Jev.Proxy))`,
   the way `web_search` does it — `pkg/jev` has no `WithTimeout` option.
2. **A pinned model.** `pkg/jev`'s `DefaultModel` is a concrete version; leave a blank config field
   blank so the package default applies. The service's `jev-latest` alias moves, and a threshold
   swept against the old weights silently stops applying when it does.
   **An endpoint that exposes only an alias cannot satisfy this, and that has to be said out loud
   where the number is recorded.** A gateway fronting the service accepts one model name and rejects
   a pinned version (`typesafe/jev` answers, `typesafe/jev-1.13.0` answers 400 `unsupported_model`),
   and the response echoes the alias rather than what answered it — so a config pointed at one has an
   alias in `model` and its τ values are hypotheses again, with the move invisible from here. Prefer
   an endpoint that takes the version; where none will, the thresholds are re-swept, not trusted.
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
   The config-side halves of the same seam are `config.JevConfig.Pricing` / `CreditRate` and
   `llm.ResolveModelPriceFrom(schedule, source, model, at)`: `Record(UsageRow)` takes a plain row
   (no `Provider` needed) and a raw `*config.PricingConfig` prices it, because a price schedule is
   not always a provider's. That is what tool search resolves through — the block's override when it
   sets one, the built-in table otherwise, the order a provider's own row resolves in — and its credit
   is `ComputeCredit` over the call's tokens at the configured rate, written even when 0 (nil Credit
   means "pre-credit row", not "cost nothing").
   **Do not add a fake provider entry to carry Jev's pricing.** `ProviderConfig` is not inert —
   `BuildProvider` iterates it, so a `typesafe` entry becomes a real LLM provider that title, vision
   and fallback resolution can select.
5. **A way to see what it decided.** The answers are the only record of why a gate took or missed
   something — a probability exists nowhere else, and after the fact it cannot be reconstructed. One
   **Info** line per request, metadata first and the scores last (a handler printing a 100-entry map
   per field would interleave them): query, the model that ANSWERED, chunk i/n, threshold, tokens,
   then `answers="name=0.912, name=0.044"` in probability order. A question the service left
   unanswered is written `name=-`, never `0.000`: it was not judged, which is not the same as judged
   low. `agent/mcp/jev_search.go`'s `logAnswers` is the shape to copy;
   `TestJevSearcher_LogsTheAnswers` pins the line, `TestAnswerList_OrderAndUnanswered` the ordering.
   **The probability now also reaches the MODEL**, because `MCPSearchTools` renders it on each
   result as `score` + `matched_by` (`judgment` / `keywords` / `name`) — the pair, never a bare
   number, since a noul probability and a BM25 score are different quantities. That makes the
   framing load-bearing rather than cosmetic: a result is only ever returned because it cleared
   the cut-off, so a low score means a weak match and not a wrong tool, and the tool description
   says so in one clause (`tools.ScoreAdvice`) — a model that reads it as a verdict drops exactly
   the marginal tool τ was swept to include, then spends a turn searching again.

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

**Two rules for using it:**

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
τ, the pool (or case file) it was swept on, and the model version, because none of the three can move
without invalidating the number. The tool search's is `mcp_tool_search.jev_threshold`, and that gate
is swept in the shape it RUNS in rather than through the case-file harness:

```sh
# 1. dump what this machine actually has (production path: whitelists, search hints, summaries)
TOOL_SEARCH_POOL_OUT=$PWD/pkg/jev/testdata/tool-search/local/corpus.json \
  go test ./agent/mcp/ -run TestDumpDeferredPool -v
# 2. author the intents beside it (local/queries.json), then sweep every candidate for every request
JEV_API_KEY=... TOOL_SEARCH_CORPUS=$PWD/pkg/jev/testdata/tool-search/local/corpus.json \
  TOOL_SEARCH_QUERIES=$PWD/pkg/jev/testdata/tool-search/local/queries.json \
  TOOL_SEARCH_SWEEP_RAW=/tmp/raw.json \
  go test ./pkg/jev/ -run TestToolSearchPoolSweep -v
# 3. argue with the stored run for free — no calls, any cut-off, any corrected label
TOOL_SEARCH_SWEEP_RAW=/tmp/raw.json go test ./pkg/jev/ -run TestToolSearchRawAnalysis -v
```

`testdata/tool-search/local/` is gitignored: a pool names internal servers and their descriptions, so
the corpus is local and only the cut-off it produced is recorded. A question whose text is per-case
goes through the case-level `instructions` override, and the generator must build that text by calling
the production function — otherwise the file measures a prompt the gate never sends.

**The channel's cron notify gate: its case files are GENERATED, and its cut-off is `cron.jev_threshold`.**
The question, the state and the two shapes the state was allowed to take live in
`channel/manager/notifygate`; the decision rules (what a failure of the service means, what a report
that is only the marker means, what a failed run means) live in
`channel/manager/notify_gate.go`. The case files are generated because a hand-written one measures a
state nobody sends: one authored scenario in `pkg/jev/testdata/notify/source.json` renders three times,
through those production assemblers, so all three share ids, labels and header —

- `notify-gate-prompt.json` — the **candidate**: the job's own words ride in the state (任务原话), asked
  with `Question()`. The criterion is per-job ("开市了就总结" vs "有到期单就说"), which is the point: no
  single generic reading of "worth interrupting" can serve both.
- `notify-gate.json` — the **control**: outcome + report as prose, asked with `GenericQuestion()`, i.e.
  what a judge that never saw the job's words answers. It prices the criterion block.
- `notify-gate-template.json` — a **variant**: the report cut into two pinned fields, for the case where
  free prose alone turns out unsweepable. A follow-on, not a peer.

The candidate and the control differ in the state AND the question at once, deliberately: a criterion
the question does not tell the model to read is dead weight, so the pair answers "is the design better",
not "is the block worth 30 tokens":

```sh
NOTIFY_CASES_SRC=$PWD/pkg/jev/testdata/notify/source.json \
NOTIFY_CASES_OUT=$PWD/pkg/jev/testdata \
go test ./channel/manager/notifygate/ -run TestGenerateNotifyCases -v
```

Editing the generated files is lost work. Question, labels and state text are Chinese because the
deployment's jobs are — a translated gate needs its own sweep, not this one's threshold. Its
`existence-*` groups are the deployment's own jobs (扫 A 股 / 查菜单 / 查到期单 / 盯 blog 更新) and are
a different judgment from the rest: "did anything come back at all", where a fetch failure and a
genuinely empty result must not be told apart by prose alone. A deployment that reaches the service
through a gateway must set `JEV_BASE_URL` (and `JEV_PROXY` if it needs one) or every case fails
against a host it cannot see.

**The job's own words in the state are what make this gate work**: a job that says "开市了就总结" and a
job that says "有到期单就说" want different things from the same report, so a state without the criterion
leaves the judgment no rule to apply and the corridor disappears (the control arm — outcome plus report,
generic question — has none). The deployed cut-off is **`cron.jev_threshold` = 0.45**, the middle of the
corridor measured on `pkg/jev/testdata/notify-gate-prompt.json`; the case set's labels are one person's
judgement, so re-sweep before moving it.

**A tool-search cut-off is a property of the POOL's tool TEXTS — τ = 0.30 on a 184-tool pool, and
neither of the two things it is tempting to blame instead:**

- **Not the request shape.** The same (query, tool) asked solo, against its own server, in one
  184-question request, and in production's chunks scored within 0.02 of one another
  (`TestToolSearchShapeProbe`). A fixture-swept threshold is shape-portable; the gate's own batching
  is never why a number moved. **Narrowing a search to one server is that same case, not a
  re-sweep**: the search tool's `server` argument removes candidates instead of rewording a
  question — 「against its own server」 above is literally one of the four forms compared — so it is
  a cost lever (one candidate is one question, and a full pool is one request per hundred of them)
  rather than a different question. That is also why the narrowed search logs its scope
- **Not the request shape.** The same (query, tool) asked solo, against its own server, in one
  184-question request, and in production's chunks scored within 0.02 of one another
  (`TestToolSearchShapeProbe`). A fixture-swept threshold is shape-portable; the gate's own batching
  is never why a number moved. **Narrowing a search to one server is that same case, not a
  re-sweep**: the search tool's `server` argument removes candidates instead of rewording a
  question — 「against its own server」 above is literally one of the four forms compared — so it is
  a cost lever (one candidate is one question, and a full pool is one request per hundred of them)
  rather than a different question. That is also why the narrowed search logs its scope
  (`logAnswers` carries `server`) and answers nothing from outside it.
  How many questions ride in one request is the same kind of lever, and it belongs to the ENDPOINT
  rather than to any gate: a service — or a gateway in front of it — rejects a request that carries too
  many questions (measured on one gateway: HTTP 400 `at most 20 questions per call`, one over the cap,
  and the status is not retried, so the whole search degrades to the keyword path). `jev.max_questions_per_request`
  sets the page size and `config.JevConfig.QuestionsPerRequest` resolves an unset one from `base_url`:
  a known gateway takes its own measured cap (a table in `config`), everything else the reference
  service's 100. Changing it moves the number of round trips (and the wall time `jev_budget` has to
  cover), not a probability: a search's state is the query itself and the question texts are the same
  either way.
- **It IS the tool texts.** The same intents separate cleanly against a small hand-written fixture
  and do not against a real pool: real descriptions are written for someone who already knows the
  product — preconditions ("使用前提：先调用 …"), required server modes, product jargon — and the `No`
  criterion's "needs input the request does not provide" is read against exactly that. A pool whose descriptions are rewritten is a pool whose cut-off is a
  hypothesis again.

Measuring per REQUEST is what picks the number, not per (query, tool) pair — an intent that names two
or three legitimate answers is served by any one of them, so pair-level hit rates understate a search
that worked. What the cut-off trades, on that 184-tool pool (jev-1.13.0, ~15k input tokens per
search):

| τ | recall | tools offered / search | precision | no-tool leak |
| --- | --- | --- | --- | --- |
| 0.15 | 0.89 | 3.3 | 0.29 | 3/10 |
| 0.20 | 0.81 | 2.2 | 0.40 | 3/10 |
| 0.25 | 0.70 | 1.6 | 0.47 | 2/10 |
| **0.30** | **0.60** | **1.1** | **0.55** | **0/10** |
| 0.35 | 0.51 | 0.9 | 0.63 | 0/10 |

*recall* = share of searches where any expectation cleared τ; *offers* is the cost side (every offered
tool is a schema registered and a tool-list change, which invalidates the prompt prefix);
*no-tool leak* = requests nothing in the pool can serve that were answered with something anyway. The
repo default sits at **0.30**: about one tool offered per search and no unlabelled answer leaking
through, paid for with two searches in five coming back empty. One row down buys 21 points of recall
for twice the offers and 3/10 leaks; one row up trades another 9 points of recall for a fifth of the
cost in tools.


## Traps

**Non-text state fails silently and confidently.** An image sent as base64 returns HTTP 200 with a
plausible answer and a *higher* confidence than the same request carrying noise — the answer was read
off nothing. Convert images and audio to text with an OCR/CV pipeline first. Only 4xx/5xx are
trustworthy signals; a 200 with a high confidence is not evidence of a correct answer.

**The tighter of the two token limits is the one that binds.** The service documents a per-request
budget and a smaller `state`-plus-longest-question budget, and it is the smaller one a large request
hits first — well before the total is anywhere near its cap. Keep `state` plus the LONGEST single
question under it, and prefer many short questions over one enormous one; read `Response.Usage` for
what a request actually cost rather than estimating. Tachi's gates send hundreds of tokens per request, so this binds only
when `state` grows into real document text.

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
