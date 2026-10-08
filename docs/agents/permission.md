# Tachi — Permission Handbook (Bash policy, ask, judgment gate, tree scope)

Read this **before touching `agent/permission/`, `agent/agent_permission.go`, `agent/agent_permission_jev.go`,
`agent/permission/jevask/`, or `permissions.bash`**, before adding a tool that raises an ask, and before
believing any threshold. It is the authority for rule semantics, the ask path, the judgment gate, the
session-scope of approvals, and which trees contribute rules; the narratives behind a number live in the dated
design doc (`docs/2026-09-30-bash-ask-jev-design.md`, allowed to go stale — never copy a fact FROM it).

`.tachi.md` is injected WHOLE into every session, so it keeps one line per subject and points here for the rest.
**New permission lessons belong in THIS file, not back in `.tachi.md`** — and the rules that ride with that file
(freshness in the same turn, convention-not-history, split + index when a subject grows) apply to this split too.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Rules: the three decisions** | you write/edit rules, or change `agent/permission/` |
| **The ask path: three causes, four answers** | you touch the ask, its wording, or add a tool that raises one |
| **The judgment gate (Jev)** | you touch `permissions.bash.jev`, `jevask`, or anything on the ask path |
| **The file tools: path rules + their gate** | you touch `permissions.files`, `agent/agent_permission_files.go`, `agent/permission/paths.go`, `agent/permission/jevfiles/`, or add a tool that touches a path |
| **Threshold discipline** | you change the state/question/criteria, the model, or want to trust a τ |
| **Session-scoped approvals** | you rebuild a policy, end a session, or touch 「本会话全部允许」 |
| **Which trees contribute rules** | you touch "which tree is this session in" (desktop, ACP, `SetPermissionTrees`) |
| **The `[permission]` line on a result** | you change tool-result rendering, or wonder why the model cannot see a score |

Related: [`desktop.md`](desktop.md) (the desktop's tree resolver and its permission cards),
[`jev.md`](jev.md) (the gate's service contract — read it before wiring a new judgment anywhere),
`.tachi.md`'s Impact Surface (the five entry modes).

## Rules: the three decisions

- **Three decisions, decided in this order, and the order is the invariant**: per command SEGMENT — a `deny` rule
  matches → `Deny`; the built-in structured `rm` guard fires → `Deny`; an `allow` rule matches → that segment is
  EXEMPT from ask rules (it is not a whitelist: nothing matching still runs) **and the whole command comes back as
  `DecisionAllowExempt`**, the matched pattern riding back with the decision like a deny's does; an `ask` rule
  matches → remember it and keep scanning (a later segment may still deny). Whole command: any `Deny` → deny;
  else any `Ask` → ask; else `DecisionAllowExempt` when an allow rule matched, else plain **allow**. `allow`
  therefore means "no exception needed here", never "only this may run".
- **`DecisionAllow` and `DecisionAllowExempt` are two values because a caller acts on the difference**: a bare
  `Allow` is "nothing covered this, so it runs by default" — the population the judgment gate exists for — while the
  exempt form is the USER's own answer about exactly this shape, and it is what switches the gate off for that call.
  One segment's allow rule never overrides another segment's deny, and an ask rule on a segment no allow rule covers
  still stands.
- **Segments come from a real parser** (`mvdan.cc/sh`, `permission.Segments`): pipelines, `&&`/`||`/`;`,
  command substitutions and redirects each become their own segment, quoting is respected (`echo "a;b"` is one),
  a heredoc's BODY is data and never a segment, and a leading `sudo ` is stripped per segment so `sudo rm -rf /`
  matches rules written for `rm`. A rule only ever sees the canonical printed form — that is why the same output
  feeds the judgment gate's state (see below), and why a second parser anywhere is a bug.
- **A command that cannot be parsed is an `ask` with the sentinel `permission.AskNoRule`**, not a rule match —
  and whatever a caller does with this population, it must not claim a rule matched. The shape to keep in mind is
  the **unclosed heredoc**: bash RUNS it (ending the body at end-of-input) while our parser sees nothing of what
  it acts on. A trailing `&&` or an unclosed quote — the two most common — are refused by bash too, so `ask`
  there is only noise.
- **Built-in protection** is two different mechanisms and `disable_builtin_deny` (GLOBAL config only — a project
  file cannot weaken it) turns off both: `permission.BuiltinDenyRules` (disk overwrite, shutdown/reboot globs,
  prepended to the user's deny list by the caller) and the **structured `rm` guard** inside the Policy, which
  targets the root directory, its direct children and `$HOME` — deliberately NOT a glob, because `rm -rf /*`
  would also swallow `rm -rf /tmp/x` (deeper absolute paths are user policy, by design).

## The ask path: three causes, four answers

- **Three causes, three wordings, and nothing may conflate them** (`askCause`, `askReasonLine`): a command
  either matched an `ask` rule, could not be parsed (`AskNoRule`), or was raised by the judgment gate because
  NOTHING matched. Rendering the second or third through the first one's sentence sends the reader hunting in
  their config for a rule they never wrote. A real rule wins the
  wording when the judgment agrees, because the user's own words are what they recognise.
  **A file tool's ask has the first and the third of those and never the second** — a path always parses — so
  `AskNoRule` belongs to the command half alone and must never be written into a path cause.
- **Four answers, chosen by the per-agent `PermissionMode`**: `TUI` prompts (`y` allow once / `a` allow for the
  session / `n` deny, which ABORTS the turn — a FILE tool's refusal refuses that call and the turn continues,
  see its section); `External` delegates to a `PermissionHandler` (the desktop's card,
  ACP's `session/request_permission`); `Skip` APPROVES the ask, and is every entry mode with nobody to ask —
  channel, `tachi -p`, subagents, dream, the one-off commands. A matched `ask` rule is the user asking to be
  consulted before the command runs; with nobody to consult, refusing would stall a run that no one can
  unblock, so those entries run it and `deny` (decided before the ask branch) is what still says no there.
  `Skip` also never judges (see the gate section) and, approving without asking anyone, writes no
  `[permission]` note.
- **The ask's preview is written by the agent, not by the frontend** (`askPreview`: the subject, then why — the subject is the command for a Bash ask and the tool plus the path a caller wrote for a file ask).
  The desktop shows it verbatim — a second renderer of that format would be free to drift, and the reason line is
  the part a human acts on.
- **A refusal's text is shaped to STOP the model, not to explain the configuration to it**: fact (not executed),
  the rule, who can change it (the user), and an explicit "do not attempt the same thing in another form, do not
  go reading the permission files". **No refusal may name a config path**, and none is written as an invitation
  to look something up — naming the file is what sends a helpful model off to read it
  (`TestBashRefusalsDoNotInviteInvestigation`). A refusal can only ever come from a `deny` now: an `ask` has no
  refusal left in it (an attached frontend puts it to a human, an unattended mode approves it).
- **A `deny` is decided before any ask is raised**, so nothing on the ask path can reach it: not a session-wide
  approval, not a permission handler, not the judgment gate. That property is the call graph's, not a convention
  to be remembered — keep it that way when adding a step.

## The judgment gate (Jev)

- **`permissions.bash.jev: auto` (default) | `off`**, on top of a usable `jev:` block. `auto` is the same shape
  as `mcp_tool_search.matcher` / `cron.notify_matcher`: the gate runs whenever the service is configured.
- **It judges EVERY command that is not denied, and it can only do two things**: run it, or raise the ask a rule
  would have raised. It never produces a `deny` — a probability is not a guarantee, and a `deny` that is right
  99% of the time is broken (jev.md). A catastrophic command judged at the top of the scale is still only
  escalated to a human, which is the point. Two things switch it off: **an `allow` rule the user wrote** (below),
  and 「本会话全部允许」 per SESSION (see Session-scoped approvals below) — a verdict nobody can act on is not worth
  the call.
- **An `allow` rule is the deterministic way to stop a whole class of command being judged** — and it is the only
  lever that is per-shape rather than per-session, which matters because the gate's answers are what a user meets
  constantly (measured on real traffic: nearly every ask was one `ssh <host> '<script>'` class of work, and no
  threshold could separate it from the commands that should ask). `CheckBash` reports the matched pattern as
  `DecisionAllowExempt` and `checkBashPermission` returns before the gate — nothing is judged, so no
  `[permission]` note is written either, exactly as if the gate did not exist for that command. It sits AFTER the
  deny branch, so an allow rule can never widen into running what a deny forbids. Passing the rule to the judgment
  as another STATE FACT instead was measured and buys ~0.02 (see
  `docs/2026-09-30-bash-ask-jev-design.md`) — the rule has to decide, not advise.
- **Only where a human can answer**: `PermissionModeSkip` is excluded, so a channel bot, a subagent or `-p` never
  has a probability approve something. Letting it would be a product decision, not this gate's.
- **When the service cannot answer, the RULES decide**: a rule-matched command asks, an unmatched one runs. That is why an outage cannot make anything worse, and why
  a failure is never latched for the session (the next command tries again).
- **It cannot widen an ASK rule either**: with the gate on, an ask-rule-matched command IS judged, and a verdict of
  "run" lets it through — that is what "auto approval" means, and it is the one behaviour a user has to know about
  (`config.BashPermissions.Jev`'s own comment says so). If you ever want ask rules to keep a veto ("matched ⇒ ask,
  never judge"), it is one branch in `checkBashPermission` — but then the gate only covers the unmatched half. The
  allow rule above is the opposite case and deliberately so: a rule the user wrote to EXEMPT a shape decides, while a
  rule that only says "ask first" is judged.
- **A session memo (per agent) stores the PROBABILITY, and the threshold is applied when it is read**
  (`PermissionState.askVerdicts`): editing `jev_threshold` re-decides commands already judged, and the key is
  (canonical command, matched rule) — nothing is inferred about a command that was not itself judged, and
  `ClearSession` drops the map.
- **The client is built lazily but KEPT** (`agent_permission_jev.go`): rebuilding it per command would drop
  connection pooling and pay a fresh handshake every time, which is most of the latency this gate costs. The cache is
  keyed by the block it was built from, so a settings save rebuilds it without any push plumbing.
- **A judgment is not a provider call**, so the billing decorator never sees it: the gate writes its own ledger
  row (`llm.RecordJevUsage`, `UsageKindJev`, credit snapshot included). Skipping it makes these calls look free
  in `/usage`.

## The file tools: path rules + their gate

- **The three file tools (`ReadFile` / `WriteFile` / `EditFile`) have their own rule sets and share ONE
  gate.** `permissions.files.read|write|edit` are path globs (`deny`/`ask`/`allow`, same precedence as
  the command rules); `permissions.files.jev` is one `auto|off` switch over all three, with one τ — the
  tool name is a FACT in the state, and a threshold belongs to the question, so three switches over one
  measured number would be three numbers where there is one.
- **A path rule's `allow` means the same as the command side's — and it is read the same way**: `CheckPath`
  returns `DecisionAllowExempt` with the matched pattern, `checkFilePermission` returns before the judgment, and
  nothing is asked or noted. It is a GLOBAL-only list (`allow` never comes from a project file), so what it can do is
  let a user exempt a directory outside their trees — e.g. a scratch directory the agent keeps writing exports
  into — from being judged at all.
- **A rule has two forms, and both are load-bearing** (`permission.PathPattern`): `Text` is the
  pattern VERBATIM as the user wrote it — `~/.ssh/*` stays `~/.ssh/*`, a project's `secrets/*` stays
  `secrets/*` — and it is what an ask preview, a refusal and the judgment's state quote back;
  `Canonical` is the anchored form with its literal prefix resolved, and it is what the matcher
  compares. Matching only the text silently never fires on a machine that reaches a directory through a
  symlink (macOS `/etc` → `/private/etc`); naming only the canonical form quotes a rule the user cannot
  find in their own config — issue #40's failure again, which is why `Text` is not the expansion either.
  Both sides go through `permission.CanonicalizePath`, which is why there is one function for it.
- **`canonicalPattern` builds its result from strings, never with `filepath.Join`** — and that is a
  TRAP, not a style choice: `Join` cleans, so `Join("/a", "")+"*"` comes out `/a*`, a rule that no
  longer covers the directory but every SIBLING whose name starts the same way (`/a-backup`, `/a2`). A
  matcher-only test cannot see it (a `*` spans `/`, so the widened pattern still matches the file the
  rule was written for) — the direction differs by list: a deny over-blocks, an `allow` over-exempts.
  `TestCanonicalPatternKeepsTheSeparator` asserts both the string and the sibling behaviour.
- **Anchoring, and the layering rule**: `~` (and any relative global pattern) is home-anchored for the
  global config, and a relative pattern in `<tree>/.tachi/permissions.yaml` is anchored to THAT tree;
  `allow` is global-only, so a project can only tighten. The path half lives on the SAME `Policy` as the
  command half (`WithFileRules`): one rebuild, one session switch, one reset.
- **Reading is judged too.** The criteria are not "does this write anything" — `~/.ssh/id_rsa` and a
  project's `.env` are the clearest needs-a-look cases, and they are reads. So is the one boundary the
  state can state as a fact: whether the target is inside the trees the session works in.
- **The state describes the TARGET and never the payload.** The path (canonical), what a stat says about
  it, the session's working directory and trees, and the matched rule — never the content a write would
  put in the file, nor an edit's two halves. A payload is model-generated free text, which is the register
  trap `jev.md` warns about, and it made the state grow with whatever was being written. The cost is
  measured and named in `config.FilesPermissions.JevThreshold`'s own comment: with the content in the
  state a write to `.tachi/permissions.yaml` scored 0.87, without it 0.43 — so **the rules, not the
  judgment, are what stops an agent from rewriting the rules it is judged by**.
- **A refusal refuses the CALL and never the turn** (`resolveFileAsk`), unlike the bash ask's
  `errCancelled`; a `deny` is still decided before the gate is reached. Under `PermissionModeSkip` the gate is
  off entirely and a matched ask rule is approved by that mode's branch — the same reading the command half
  has, and the reason no unattended file call is refused for an `ask` — no stock config has file rules, so the
  default behaviour is unchanged.
- **`Read` and `Edit` are PARALLEL tools** (`Write` is not): the gate runs on both executors, and a
  parked ask serializes on `PermissionState.askMu` — one `ConfirmResp` channel serves the whole agent.
  The `[permission]` note travels out of a parallel group through the `notes[i]` slice, one index per
  goroutine.
- **Its τ belongs to `pkg/jev/testdata/files-ask.json`** and to nothing else — not to
  `permissions.bash.jev_threshold`, which answers a different question. Shipped τ = 0.60 — the TOP of
  the corridor (0.49, 0.62), so it reads every routine call the same way but sits 0.02 under the
  nearest needs-a-look case — swept on that case set (64 calls × 5, `jev-1.13.0`); what it deliberately misses and
  when to re-sweep live in `config.FilesPermissions.JevThreshold`'s own comment, and the sweep history
  is in `docs/2026-09-30-files-ask-jev-design.md` (allowed to go stale). The few renderers it shares in
  SHAPE with `jevask` (the trees line, the unknown-value placeholder) are duplicated on purpose: one
  shared function would let a reworded sentence void both thresholds at once. Re-generate the case file
  with `agent/permission/jevfiles/generate_cases_test.go`.



- **No threshold is a fact.** `confidence` means "how concentrated the distribution is over the options THIS
  caller supplied", so the question, the criteria, the STATE SHAPE and the model version all belong to any τ.
  It is recorded in `config.BashPermissions.JevThreshold`'s comment **with the case set and the model version**,
  and re-sweeping is the only way to move it. Where the endpoint serves an ALIAS rather than a version, say that
  in the same comment: the number is then a hypothesis about weights nobody pinned (jev.md, "A pinned model").
- **The state's fact set belongs to the judgment, not to whatever the layer happens to hold.** Shipped τ = 0.66 —
  the middle of the measured window (0.62, 0.70) — was swept on a state whose only facts are: the canonical
  segments, the working directory, and the matched rule (or why there is none). It used to carry a fourth, **the
  trees the session works in**, for a criterion ("touches data outside the working tree") that has since been
  removed: the axis was measured, and it was the whole reason the gate asked about real work (see
  agent/permission/jevask's package doc). **Change any sentence the state renders and the number is void.**
- **A criterion is the lever; a fact the criteria do not read is dead weight.** The same measurement is what says
  so: deleting the trees line from the state alone moved the routine commands by 0.036, while deleting the
  outside-the-tree clause from the CRITERIA moved them by 0.225 and moved the intrinsically dangerous ones by
  0.009. Do not try to quiet a gate by taking context away from it — take the clause out, or it will keep judging
  by it.
- **What the bash gate asks about, after that removal**: what the command DOES — reads credentials, changes or
  publishes shared state, rewrites history, runs code fetched from the network, deletes unreproducible work, or
  walks a whole filesystem or an unbounded tree (that last one is the one read-only shape on the Yes side: a
  DURATION reason, kept on purpose and deliberately not tied to scope).
- **Facts only, never a derived verdict.** "outside the working tree: no" would be consumed as a fact, and a wrong
  fact here is asymmetric (it can talk a command under the threshold). An empty fact says "(unknown …)" rather than
  inventing one.
- **A label has to be decidable from what the gate sees** — the command as WRITTEN, never what would happen if
  the shell rejected it. (A case filed the other way empties the corridor.)
- **Do not put the user's turn message in the state.** The judgment answers what the COMMAND is; a request is
  context rather than permission, and free text is the register variable jev.md warns about. It would also break
  the session memo's key. (`bash-ask-context.json` is the paired measurement that settled this — the deltas are
  within noise.)
- **What a re-sweep is**: regenerate the case file from `agent/permission/jevask` (its generator; the state text
  must come from the production assembler, never be hand-written), then
  `JEV_EVAL_FILE=bash-ask.json go test ./pkg/jev/ -run TestJevEval -v`. The corpus is
  `pkg/jev/testdata/bash-ask*/`; `bash-ask-context.json` is the paired experiment above, kept so the conclusion
  stays reproducible.
- **A high τ misses asks; a low τ only interrupts.** Sit on the safe side of the corridor, and re-read the
  per-case table rather than the summary when something looks off.

## Session-scoped approvals

- **「本会话全部允许」 is ONE answer with TWO readers — and flipping only one of them is the bug the shape
  exists to prevent.** The policy's copy (`Policy.AllowAllAsksForSession`) silences a matched ASK rule; the
  agent's copy (`PermState.AllowAllAsks`) silences the two JUDGMENT GATES, which both read it after their deny
  branch and BEFORE judging — so the gate is not consulted at all (a verdict nobody can act on is not worth a
  call, and the card it would raise is the one the user just dismissed). Two readers are not redundancy: the
  policy's copy is what a forked child shares with its parent (`agent_fork.go`), and the PermState copy is the
  only one that exists when a session has no policy at all (rules absent AND built-ins disabled — its gates
  still ask, and its button used to do nothing). One setter flips both: `AIAgent.AllowAllAsksForSession`.
- **deny is decided before EITHER reader is reached**, so "stop asking me" can never become "run what I
  forbade". The one ask it does widen into is the UNPARSEABLE command (`AskNoRule`): nothing was parsed, so no
  rule — deny included — saw any part of it, and the command runs on the blanket answer alone. That is
  deliberate and matches every unattended frontend (PermissionModeSkip approves a policy ask of either kind on
  its own, having nobody to consult), and the population is benign: an unclosed quote, or a trailing `&&`.
- **Rebuilding a policy must INHERIT the policy's half** (`InheritSessionApprovals`), or a folder change re-asks
  a question the user already answered — a regression they cannot see coming. The PermState half needs no
  inheritance (a tree change rebuilds the policy, not the agent). The RULES must not carry over: they are
  exactly what changed.
- **Ending a session drops both halves** (`ResetSessionApprovals` + `PermState.AllowAllAsks`, from
  `ClearSession`) — together with the gate's memo, because a new conversation is not the one whose commands were
  judged or allowed.
- The MCP search cost card's session answers (`MCPSearchAllowed/Blocked`) live beside these for the same reason,
  but they answer a different question (money, not rules) — and its own switch is what made THAT card's
  「本会话全部允许」 work in the first place, which is where this shape was copied from: the permission card was
  the inconsistent one, not the search card.

## Which trees contribute rules

- **Rules are per tree and EVERY tree the session works in contributes**: `AIAgent.SetPermissionTrees(primary,
  additional)` → `config.ProjectRootsFrom` (each directory walked up to its nearest `.git`, de-duplicated in
  order) → every `<tree>/.tachi/permissions.yaml` read and unioned. `allow` stays global-only, so a project can
  only tighten. Reading only the primary's file is a hole in the WORST direction: a rule the user wrote in their
  second repository silently does not fire.
- **`PermissionRoot` names the primary's tree; `PermissionTrees` is the whole set** — the latter is what the
  judgment gate's state reports, because "outside the tree" is meaningless without all of them.
- **The frontends own the resolver, and it must be the SAME one their prompt uses**: the desktop's
  `sessionRootsFrom` (three call sites: agent construction, `beginTurn`'s one stale flag, and the settings row's
  push), ACP's `roots.Cwd()/Dirs()` (four: the three session constructors and the in-memory resume that changes
  `additionalDirectories`). A new call site that resolves a tree by itself is the bug desktop.md's Session Tree
  section describes.
- **`configure` builds from the PROCESS cwd**, which for a GUI app is `/` — so a frontend that knows better
  re-points it before the first turn (the desktop and ACP both do).

## The `[permission]` line on a result

- **Both gates write the same line, and it goes to the EVENT ONLY — the transcript — and never into the model's message**
  (`agent.askNote`, written by `checkBashPermission` and `checkFilePermission` alike):
  `[permission] decided_by=judgment score=0.180 threshold=0.660`, or `decided_by=approval` for a call that was
  asked about and approved (the unattended branch approves without asking anyone, so it writes no note). It is
  appended for a FAILED call too (the call ran, or was refused, either way), and no judgment and no ask means no
  line at all, so an unremarkable call's result is byte-for-byte what it was before the gate existed.
- **The model deliberately does not get it.** Two costs on that side: a line the model does not act on is re-sent
  in the history of every LATER request (the event is not in the history), and "this one was allowed" in front of
  the model invites reading it as a standing permission for commands like it. The Bash tool description therefore
  carries no clause about it either — a description sentence would ride on every request for a number the model
  never sees, and the file tools' descriptions carry none either. The human's other exit is the debug log
  (`Agent: bash ask judged` / `Agent: file ask judged`, one line per judgment, with the probability, the threshold
  and the verdict).
- **The pair, never a bare number**: under `judgment` the score is the probability that let it run, under
  `approval` it is the probability that made the gate ask.
