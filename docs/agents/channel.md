# Tachi — Channel Mode Handbook (split out of `.tachi.md`)

Read this **before changing anything under `channel/`**. Channel mode is the entry mode where the agent
runs somewhere nobody is watching it: a message arrives from an IM backend, one `*Manager` decides what
it means, and the answer goes back out. Most of its invariants are about *state that outlives a turn*
(a per-thread agent, buffered ambient messages, a parked AskUser question) — which is exactly the state a
careless change breaks in a way no single-turn test notices.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions
that apply anywhere in the repo and points here for the rest. **New channel lessons belong in THIS file**,
not back in `.tachi.md` — and the rules that ride with that file (freshness in the same turn,
convention-not-history, split + index when a subject grows) apply to this split too.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Wiring: who builds what** | you touch startup, channel registration, or `MessageHandler` |
| **The per-thread agent pool** | you touch the agent cache, eviction, `/new`, `/model`, `/cd`, or anything about lock order |
| **Per-thread state besides the agent** | you add state that lives per thread, or touch whisper / AskUser / steer |
| **Ambient (whisper) routing** | you touch group-chat handling, silence, or the batch window |
| **Slash commands** | you add a command, change dispatch, or touch `/compact` |
| **One-off commands (`/commit`, `/review`)** | you touch the side-channel runs or their cancellation |

## Wiring: who builds what

- **One `Manager` serves every channel.** `main_channels.go` builds it (`channelmgr.New(cfg)`), adds each
  channel the config enables through the registry (`channel.Register` in `channel/discord`, `weixin`,
  `github`), and calls `mgr.Start(ctx)`. The manager hands each channel ONE handler — `buildHandler()`
  (`agent_turn.go`) — so every routing decision in the product lives behind that closure.
- **The handler is synchronous per channel.** `channel.Channel.Run` calls it and waits for the
  `HandlerResult`; a channel that wants concurrency spawns its own goroutines, but the manager already
  runs the agent turn on a goroutine and blocks the handler on a result channel, so a slow turn does not
  block the channel's read loop forever — it blocks only that thread's next message.
- **`PermissionModeSkip` all the way down**: a channel agent REFUSES a bash `ask` rather than parking one
  (`.tachi.md`'s entry-mode table). The two exceptions both come from the *channel's* own capability, not
  from the manager's choice:
  - `AskUserQuestion` is unregistered for a non-interactive channel and **re-registered** in `buildAgent`
    for one whose channel implements `InteractiveChannel` with `Interactive() == true`. `NewAIAgentWithConfig`
    unregisters it unconditionally under `PermissionModeSkip`, so the re-registration is what makes the
    tool exist — not a formality.
  - A channel implementing `ToolProvider` / `ThreadReminderChannel` injects its own tools and a per-thread
    reminder into the cached agent at build time (`buildAgent`), so both are per-thread by construction.

## The per-thread agent pool

**One long-lived `*AIAgent` per ThreadID** (`agent_pool.go`), so the state worth keeping across turns —
skill activation, the MCP discovered set, reminder cadence — survives them, while threads stay isolated.
The agent is built lazily on a thread's first message and rebuilt when the provider changes or the thread
is evicted (`/new`, `/model`, a compact's finalize).

**Three guards, and NONE of them is nested.** This is the invariant to keep:

| Guard | Protects | Held for |
| --- | --- | --- |
| `agentPool.mu` | the map only | microseconds: map read/write, never across a call into an agent |
| `cachedAgent.mu` | that thread's turns (serializes them; `release` ends the turn) | a whole turn |
| `cachedAgent.evicted` (atomic) | "this slot is dead" | set before the agent is closed, read by a waiter |

An earlier version nested the first two — eviction took the map lock and then the slot lock, while
`acquireAgent` took the slot lock and then went BACK for the map lock to re-check its slot — and that is
an ABBA deadlock: a thread acquiring thread T while another evicts T hangs both. It was latent for as long
as the cache existed, because every test ran acquire and evict one after the other instead of against each
other. The atomic `evicted` flag is what lets a waiter refuse a doomed slot without touching the map, so
neither direction is needed. `TestAgentPool_ConcurrentAcquireEvictDoesNotDeadlock` pins it: it fails within
seconds if the nesting comes back (verified by restoring the old shape).

**The rest follows from that:**

- **A borrow must be released before an eviction on the same goroutine.** `acquireAgent` holds the slot
  lock, and `evictAgent` waits for it — correct across goroutines (a running turn is not yanked), a
  self-deadlock within one. The `/compact` finalize path is the one caller that must borrow AND evict, so
  it releases inside a scope before evicting; `compact_deadlock_test.go` is that regression test.
- **Nothing outside `agent_pool.go` touches the pool's guards.** Callers use `slot` (bookkeeping read, never
  creates; a dead entry reads as absent), `ensure` (create-if-absent-or-dead; its closure must not call back
  into the Manager — building the value first is what keeps the short lock from becoming an ordering
  problem), `acquire`/`release`, `evict`/`evictAll`, `each` (snapshots, then calls out — a slow
  `ReloadSkills` must not hold the map), `count`, and `withAgent` (runs a callback on a thread's agent under
  the slot lock — how a slash command reaches an agent, instead of taking `ca.mu` itself and inventing an
  order of its own).
- **A failed build leaves a tombstone**: the slot is marked dead and stays in the map until the next caller
  for that thread replaces it (removing it would mean taking the map lock with the slot lock held). It stays
  INVISIBLE meanwhile — `slot` and `ensure` treat an evicted entry as absent, so `/cd` creates a live slot
  instead of writing a directory into a dead one, and `getThreadWorkDir` falls back to the session rather
  than answering with a value nothing will ever use (`TestAgentPool_TombstonesAreInvisible`). A slot that
  already has an agent never becomes a tombstone in place: detach, `evictAll` and `acquire`'s phase 1 all
  delete it in the same critical section that marks it, and the one that removes it owns the close — even
  though it left the map, so `evict` can no longer find it (`TestAgentPool_ReplacedSlotClosesItsAgent`).
- **`workDir` is the one field two lifecycles write**, so it carries its own LEAF lock (`cachedAgent.WorkDir`/
  `SetWorkDir`) rather than the turn lock: a turn reads it at the start and may write it back from the
  session's persisted value, while `/cd` — a stateless command that must not wait out a turn — writes it
  directly. Guarding it with the turn lock would make `/cd` block for a whole turn; guarding it with the map
  lock was the old code's accident (that lock protects the map).

## Per-thread state besides the agent

**ONE table, one lock** (`thread_state.go`): `threadTable.byID[threadID] → *threadState`, holding

- **`ch`** — the channel owning the thread, so `buildAgent` can ask the right channel about
  interactivity, tools and reminders. Written once, by the per-channel handler wrapper on the first
  message.
- **`defaults`** — workdir/provider defaults from platform metadata (a group announcement), applied when
  a FRESH session is created. Empty fields fall back to the usual defaults.
- **`turn`** — the LIVE turn: steer queue + steer response channel, the result channel the handler blocks
  on, cancel func, `isCompact`, Ask User state, and the whisper ambient state. Created by `beginTurn`,
  dropped by `endTurn`.

Three containers with three lock styles used to stand here (an RWMutex map, a Mutex map, a
`container.LockedMap`), reached from four files — the largest source of "which lock am I holding" in the
package. What matters now:

- **The table lock is a LEAF**: nothing is taken while holding it, and its methods never call out.
  `beginTurn`'s `make` callback only allocates for that reason — a provider lookup inside it would nest
  the thread-defaults lock.
- **A second message JOINS the running turn** (`beginTurn` returns the live activation); the caller reads
  the activation's own marker to tell "I started it" from "I am steering it". A rival activation would
  mean two cancels, two steer queues and two AskUser slots for one thread.
- **`endTurn` clears only its OWN activation** (pointer equality). A handler goroutine that finishes after
  `/stop` took the turn must not drop the activation the next message installed —
  `TestThreadTable_EndTurnOnlyClearsItsOwnTurn`. `takeTurn` hands the activation back so the CANCELLATION
  happens outside the table lock: `cancel()` unwinds the turn goroutine.
- **Read-only lookups never create a record**: `channelFor` / `defaults` / `turn` are what stateless
  commands and the ambient timer call on threads that may never have been seen.
- **The turn's internals keep their own lock** (`threadActivation.mu`) — that object is shared with a
  running goroutine, so it is not a field of the record. `cancelled` is an atomic: `/stop` writes it from
  the command goroutine while the turn goroutine reads it.
- **`oneoffCancels`** (Mutex) + **`oneoffSem`** (Semaphore) stay separate: one-off runs have no activation
  and no agent slot, so they are not part of a thread record.

A new per-thread need goes INTO `threadState`; a fifth map is the moment to stop and ask why.

## Ambient (whisper) routing

One feature, THREE entry points — a change to any of them has to consider the other two:

1. **The guard in `buildHandler`** decides whether a message is ambient at all: non-directed + group chat +
   whisper enabled → `handleAmbientMessage`; non-directed + group chat + whisper OFF → dropped silently
   (never an agent turn).
2. **`handleAmbientMessage`** (same file) buffers the message on the thread activation and (re)arms the
   batch timer; a directed message instead cancels the pending ambient turn, transfers the buffered messages
   as steer context, and resets the silence backoff.
3. **`ambient.go`** runs the batch: `flushAmbientBatch` → `runAmbientTurn` → the `[SILENT]` decision, whose
   consecutive count drives the exponential batch window (`ambientBatchWindow`).

The reply itself is the primary product of that turn, so a change that moves the silence decision (a gate
before generation, say) also changes where the backoff counter is fed.

## Slash commands

Three places, and they must agree:

- **`agent/commands` is the registry** (name, description, modes, input hint) — metadata only. It is what
  the unknown-command help lists and what a frontend builds its autocomplete from.
- **`executeSlashCommand` (`commands.go`) is the dispatch** — a switch, one case per command, all returning
  `(text, error)` through `textHandlerResult`.
- **`/compact` is special**: `buildHandler` intercepts it and runs it as an AGENT TURN (it needs the live
  session context to summarize), so it never reaches the switch. Skill activation takes the same road.

`TestEveryChannelCommandIsHandled` (`agent_pool_test.go`) holds the three together: a command declared for
channel mode with neither a case nor a documented special path fails the build. It is a source check —
invoking every handler would need a live session per command and would run the destructive ones
(`/restart` calls systemctl, `/new` ends a conversation).

## One-off commands (`/commit`, `/review`)

They are not turns: they do not create a `threadActivation`, do not touch the agent cache, and are tracked
in `oneoffCancels` so `/stop` and `/new` can cancel them. `oneoffSem` caps how many run at once globally
(a burst of `/review N` would otherwise saturate provider quotas — a full fork per run), and a command that
cannot get a slot is REFUSED with a hint rather than queued silently.

## Tests

```sh
go test -race ./channel/manager/     # the package is written for -race; the field-level races it covers are real
```

| Test | Pins |
| --- | --- |
| `agent_pool_test.go` `TestAgentPool_ConcurrentAcquireEvictDoesNotDeadlock` | the pool nests no guards (fails in seconds on the old shape) |
| `agent_pool_test.go` `TestAgentPool_EachDoesNotHoldThePoolLock` | `each` calls out with the map lock released |
| `agent_pool_test.go` `TestAgentPool_TombstonesAreInvisible` | a dead slot reads as absent, so `/cd` lands on a live one |
| `agent_pool_test.go` `TestAgentPool_ReplacedSlotClosesItsAgent` | a slot acquire replaces is still closed (it left the map) |
| `agent_pool_test.go` `TestEveryChannelCommandIsHandled` | registry ↔ dispatch ↔ special paths |
| `compact_deadlock_test.go` | borrow-then-evict must release first; evict waits for a held agent |
| `usage_race_test.go` | `/usage` reading an agent while its turn runs |
| `agent_cache_test.go` | workDir precedence (cached agent wins over the persisted session) |
