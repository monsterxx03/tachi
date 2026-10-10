# Tachi — Compaction Handbook (split out of `.tachi.md`)

Read this **before touching the compaction path**: `agent/agent_compact.go` (`doCompact` / the auto-compact
trigger), `agent/compact.go` (`CompleteCompact` / `FinalizeCompact`), `agent.CompactRunOptions`, **any
frontend's `/compact`** (tui, desktop, acp, channel), or an `AgentEventAutoCompact*` handler.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New compaction lessons belong in THIS file**, and
the rules that ride with `.tachi.md` (freshness in the same turn, convention-not-history, split + index when a
subject grows) apply to this split too.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Two triggers, one mechanism** | you touch `/compact` in any frontend, the auto-compact trigger, or a run that hides every tool |
| **The child's record** | you touch `FinalizeCompact`, or add a field to the session meta |
| **What a frontend owes the hand-off** | you touch a frontend's compact command, or the `AgentEventAutoCompact*` handlers |

## Two triggers, one mechanism

- **A typed `/compact` is an ordinary conversation turn** whose user message IS the compact instruction
  (`agent/commands.BuildCompactInstruction`) — the model has to SEE the whole history to summarise it, which a
  side-channel call cannot do. The loop's auto-compact (`maybeAutoCompact`, threshold in `compact.threshold`)
  is the other trigger. Both end at `FinalizeCompact`: the conversation moves into a CHILD session whose record
  opens with the summary and 「请基于以上摘要继续对话。」, the parent keeps the pre-compaction history, and the
  two link to each other (`compacted_child_id` / `compacted_parent_id`).
- **A compact run carries `agent.CompactRunOptions()` — both halves, and the second one is the load-bearing
  one.** Hidden tools keep the reply text; **switching the loop's own auto-compaction off** is what keeps the
  instruction in front of the model. The trap: a compact run's history is over the threshold BY DEFINITION
  (that is why the reader typed `/compact`), so the loop would otherwise compact at its own top, replace
  `rs.Messages` with the summary — and the request no longer carries the instruction at all. The model answers
  a continuation, and the frontend, which collects that reply as its summary, compacts a SECOND time: two new
  sessions, and the one the reader lands on has a conversation fragment as its summary. Pinned by the desktop
  smoke scenario `compact-race`.
- **A request with every tool hidden can come back with the model's own tool-call markup as TEXT** (a
  DeepSeek-family model emitting its native `｜｜DSML｜｜` block into ordinary content, which Tachi renders
  verbatim — there is no sanitiser). A no-tools run has nothing to parse it against, so a no-tools run must be
  one whose answer is text by construction; `WithNoTools()` alone is used for exactly one other purpose (the
  node's managed-session refusal), and it is not what marks a run as a compaction.

## The child's record

- **Every session-meta field `FinalizeCompact` does not copy is silently ZEROED** — not reported. Each one has
  its own failure: `AdditionalDirs`/`ProjectID` (tools, prompt and checkpoints agreeing with each other in the
  wrong tree), `Pinned` (invisible state on a folded-away row, plus a permanent exemption from session
  cleanup), `Kind`/`SkipDream`/`SourceNodeID` (a remote task coming back as the user's own local conversation —
  and, for the source, a record of somebody's work that no longer says whose). Adding a field to the meta means
  deciding whether it travels with the conversation; `agent/compact_test.go` pins the path fields and
  `agent/managed_test.go` the three that mark a session as a remote execution.
- **`ThreadID` and `Pinned` MOVE rather than being copied** — the parent releases them in the same step. One
  binding, held by the record it still means something on: a thread binding left on the parent makes
  `FindByThreadID` resolve to the pre-compaction history, which is how every turn after a compaction used to
  trigger another one.

## What a frontend owes the hand-off

- **A frontend's compact command must not complete a compaction whose session already moved.** All of them
  share one shape — run the compact turn, take the reply as the summary, call `CompleteCompact` — and each one
  has to refuse the second half when the loop compacted anyway:
  - the desktop watches the run's own stream for a successful `AgentEventAutoCompactDone` and returns without
    the hand-off; the record the event's handler already wrote is the one `endTurn`'s `followCompaction`
    honours (`desktop/commands.go`);
  - the TUI keeps `Model.autoCompacted` — set by that same event, cleared at turn start — and at `TurnComplete`
    drops the pre-compaction `savedHistory` instead of restoring it (`tui/model_events.go`). Restoring it would
    put the OLD history in front of a session that is the child on disk: the next request would send it and
    cross the threshold again, so the reader's `/compact` would look like it did nothing.
  With `CompactRunOptions` in place neither path can be reached; they are the floor under it, not the fix.
- **`compact`, `compact-race` and `auto-compact` are the smoke scenarios for this path** — run all three for
  any change here (`make desktop-smoke ARGS="-run compact,compact-race,auto-compact"`).
