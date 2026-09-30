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
| **Threshold discipline** | you change the state/question/criteria, the model, or want to trust a τ |
| **Session-scoped approvals** | you rebuild a policy, end a session, or touch 「本会话全部允许」 |
| **Which trees contribute rules** | you touch "which tree is this session in" (desktop, ACP, `SetPermissionTrees`) |
| **The `[permission]` line on a Bash result** | you change tool-result rendering, or wonder why the model cannot see a score |

Related: [`desktop.md`](desktop.md) (the desktop's tree resolver and its permission cards),
[`jev.md`](jev.md) (the gate's service contract — read it before wiring a new judgment anywhere),
`.tachi.md`'s Impact Surface (the five entry modes).

## Rules: the three decisions

- **Three decisions, decided in this order, and the order is the invariant**: per command SEGMENT — a `deny` rule
  matches → `Deny`; the built-in structured `rm` guard fires → `Deny`; an `allow` rule matches → that segment is
  EXEMPT from ask rules (it is not a whitelist: nothing matching still runs); an `ask` rule matches → remember it
  and keep scanning (a later segment may still deny). Whole command: any `Deny` → deny; else any `Ask` → ask;
  else **allow**. `allow` therefore means "no exception needed here", never "only this may run".
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

- **Three causes, three wordings, and nothing may conflate them** (`bashAskCause`, `askReasonLine`): a command
  either matched an `ask` rule, could not be parsed (`AskNoRule`), or was raised by the judgment gate because
  NOTHING matched. Rendering the second or third through the first one's sentence sends the reader hunting in
  their config for a rule they never wrote. A real rule wins the
  wording when the judgment agrees, because the user's own words are what they recognise.
- **Four answers, chosen by the per-agent `PermissionMode`**: `TUI` prompts (`y` allow once / `a` allow for the
  session / `n` deny, which ABORTS the turn); `External` delegates to a `PermissionHandler` (the desktop's card,
  ACP's `session/request_permission`); `Skip` REFUSES the command with an explanation, and is every entry mode
  with nobody to ask — channel, `tachi -p`, subagents, dream, the one-off commands. `Skip` + a user's explicit
  ACP 「allow all」 is the only thing that auto-approves there.
- **The ask's preview is written by the agent, not by the frontend** (`bashAskPreview`: the command, then why).
  The desktop shows it verbatim — a second renderer of that format would be free to drift, and the reason line is
  the part a human acts on.
- **A refusal's text is shaped to STOP the model, not to explain the configuration to it**: fact (not executed),
  the rule, who can change it (the user), and an explicit "do not attempt the same thing in another form, do not
  go reading the permission files". **No refusal may name a config path**, and none is written as an invitation
  to look something up — naming the file is what sends a helpful model off to read it
  (`TestBashRefusalsDoNotInviteInvestigation`). The one exception is the unparseable
  command, whose fix IS in the model's hands: it says to correct the syntax and re-issue.
- **A `deny` is decided before any ask is raised**, so nothing on the ask path can reach it: not a session-wide
  approval, not a permission handler, not the judgment gate. That property is the call graph's, not a convention
  to be remembered — keep it that way when adding a step.

## The judgment gate (Jev)

- **`permissions.bash.jev: auto` (default) | `off`**, on top of a usable `jev:` block. `auto` is the same shape
  as `mcp_tool_search.matcher` / `cron.notify_matcher`: the gate runs whenever the service is configured.
- **It judges EVERY command that is not denied, and it can only do two things**: run it, or raise the ask a rule
  would have raised. It never produces a `deny` — a probability is not a guarantee, and a `deny` that is right
  99% of the time is broken (jev.md). A catastrophic command judged at the top of the scale is still only
  escalated to a human, which is the point.
- **Only where a human can answer**: `PermissionModeSkip` is excluded, so a channel bot, a subagent or `-p` never
  has a probability approve something. Letting it would be a product decision, not this gate's.
- **When the service cannot answer, the RULES decide**: a rule-matched command asks, an unmatched one runs. That is why an outage cannot make anything worse, and why
  a failure is never latched for the session (the next command tries again).
- **It cannot widen a rule either**: with the gate on, a rule-matched command IS judged, and a verdict of "run"
  lets it through — that is what "auto approval" means, and it is the one behaviour a user has to know about
  (`config.BashPermissions.Jev`'s own comment says so). If you ever want rules to keep a veto ("matched ⇒ ask,
  never judge"), it is one branch in `checkBashPermission` — but then the gate only covers the unmatched half.
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

## Threshold discipline

- **No threshold is a fact.** `confidence` means "how concentrated the distribution is over the options THIS
  caller supplied", so the question, the criteria, the STATE SHAPE and the model version all belong to any τ.
  It is recorded in `config.BashPermissions.JevThreshold`'s comment **with the case set and the model version**,
  and re-sweeping is the only way to move it.
- **The state's fact set belongs to the judgment, not to whatever the layer happens to hold.** Current τ = 0.70,
  corridor (0.66, 0.75), was swept on a state whose only facts are: the canonical segments, the working
  directory, **the trees the session works in**, and the matched rule (or why there is none). A single tree is not
  enough — a session can span several, and a command in the second one would read as "outside the tree".
  **Change any sentence the state renders and the number is void.**
- **Facts only, never a derived verdict.** "outside the working tree: no" would be consumed as a fact, and a wrong
  fact here is asymmetric (it can talk a command under the threshold). Listing the trees is a fact; comparing is
  the model's job. An empty fact says "(unknown …)" rather than inventing one.
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

- **「本会话全部允许」 is one switch on the policy and it only silences ASK rules** (`AllowAllAsksForSession`):
  deny is decided before an ask is reached, so "stop asking me" can never become "run what I forbade".
- **Rebuilding a policy must INHERIT it** (`InheritSessionApprovals`), or a folder change re-asks a question the
  user already answered — a regression they cannot see coming. The RULES must not carry over: they are exactly
  what changed.
- **Ending a session drops it** (`ResetSessionApprovals`, from `ClearSession`) — together with the gate's memo,
  because a new conversation is not the one whose commands were judged or allowed.
- The MCP search cost card's session answers (`MCPSearchAllowed/Blocked`) live beside these for the same reason,
  but they answer a different question (money, not rules).

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

## The `[permission]` line on a Bash result

- **How one command was decided goes to the EVENT ONLY — the transcript — and never into the model's message**
  (`agent.askNote`): `[permission] decided_by=judgment score=0.180 threshold=0.700`, or `decided_by=approval` for a
  command that was asked about and approved. It is appended for a FAILED command too (it ran either way), and no
  judgment and no ask means no line at all, so an unremarkable command's result is byte-for-byte what it was
  before the gate existed.
- **The model deliberately does not get it.** Two costs on that side: a line the model does not act on is re-sent
  in the history of every LATER request (the event is not in the history), and "this one was allowed" in front of
  the model invites reading it as a standing permission for commands like it. The Bash tool description therefore
  carries no clause about it either — a description sentence would ride on every request for a number the model
  never sees. The human's other exit is the debug log (`Agent: bash ask judged`, one line per judgment, with the
  probability, the threshold and the verdict).
- **The pair, never a bare number**: under `judgment` the score is the probability that let it run, under
  `approval` it is the probability that made the gate ask.
