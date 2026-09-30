# Prompt cache & hit rate (handbook)

Read this **when a cache hit rate drops** — the user's or your own — or when a restart looks
expensive, and **before changing anything that shapes a request's prefix**.

## Contents — read the section you need

| Section | Read it when |
| --- | --- |
| **What actually caches** | you are about to change the system prompt, the tool list, or how a stored message is rendered |
| **Where the numbers are** | you need the hit-rate data (or you are about to trust the desktop's ring) |
| **Reading a drop** | a drop has been reported and you need to locate it |
| **Baseline** | you want to know whether a number is normal |
| **Practical notes** | you are about to run the analysis, or to write it up |

Related: [`.tachi.md`](../../.tachi.md) (the Session History & Reminders invariant).

## What actually caches

The hit that matters is the **provider's own prefix cache**, not the client's markers: `llm/anthropic.go`
puts `cache_control` on the system prompt only (with a standing `FIXME`), yet long prefixes hit at ~99.9%
and break exactly where the request's prefix diverges. The prefix is **tools → system → messages**, in that
order.

Two consequences drive everything below:

- **Any tool-list change invalidates the whole message history.** Tools come first, so inserting, removing
  or reordering one (MCP connect/disconnect, tool enable/disable, a new tool from an upgrade) makes
  everything after it a miss. Moving dynamic tools to the end does not help — the messages are still behind
  them. This one is structural: expect one full re-read, and say so rather than hunting for a bug.
- **The rebuilt history must equal the history that was SENT, byte for byte.** Anything that renders a
  stored message differently from how it went out (an extra newline at a reminder seam, a block in the
  wrong position) shifts the prefix from that point on. `agent/session_convert_test.go` pins it.
  The reminder block is where this is easiest to get wrong, and it has two shapes, told apart only by
  POSITION (both are `MessageTypeReminder`): a wrapper for the NEXT user message — its next non-reminder
  record IS a user message (artifacts, the date block, continuation prompts) — and a block the loop injected
  mid-turn, which is its own message and must stay where it was sent. Mid-turn injection renders the fresh
  pieces once and both appends that text and records it (`agent_loop.go`), so the two paths go through one
  render.
  **A block is stored as the text that was SENT** (`RenderPieces` output, recorded verbatim, and
  `CollectPieces` renders each piece alone as the dedup key). That is what makes a change to `RenderPieces`
  itself — its separators, its layout — NOT structural: existing records keep their bytes, and only the block
  being built now renders differently, at the tail. It is also what makes a piece's own leading blank line a
  bug rather than a style choice: the renderer already separates the pieces.
- **An image part survives a reload: the record keeps the parts, the session's `images/` directory keeps the
  bytes.** Both halves are single-write-path — the record sites attach the parts the request actually
  carried (`agent.RecordParts`), and `session.FileStore.AppendMessage`, the only writer of a session record,
  moves the bytes into a content-addressed blob and leaves the reference — so a rebuilt history carries the
  images the live turn sent instead of a miss from the first image on. The read side is the `ImageResolver`
  parameter of `ConvertSessionToLLMMessages`; nil there is a deliberate text-only conversion (token
  estimates), and an unreadable blob drops that one part with a warning rather than failing the conversion.
  Records written before this existed carry no parts and are NOT backfilled. Verify a session by converting
  it (`agent.ConvertSessionToLLMMessages(store.LoadMessages(id), "anthropic", store)`) and counting the parts
  against the `[Image: …]` / `[图片: …]` placeholders; `itest`'s `image-reload` scenario pins the route.

## Where the numbers are

A session's directory (`~/.tachi/session/<id>/`) holds two files worth reading together:

| File | Answers |
|---|---|
| `api_requests.jsonl` | one record per API call: `timestamp` / `iteration` / `seq` / `system_prompt` / `user_prompt` / `tools` — **what changed in the prefix** |
| `messages.jsonl` | the conversation; `type=assistant` records carry `usage` — **the per-call hit numbers** |

`usage`, under the anthropic protocol: `input_tokens` = the **uncached** input, `cache_read_input_tokens` =
the **cached** part; hit rate = `read / (read + input)`. Output tokens are not part of it.

Two more places, and one display trap:

- `~/.tachi/usage/<date>.jsonl` is the ledger: per-call credit plus the price snapshot it was billed with.
- **Credit ignores the cache** — it is charged on total tokens, so a cache collapse costs money (the ledger's
  own prices are the truth) but barely moves 积分.
- The desktop's cache ring (in the usage row under the composer, beside ¥cost and 积分) shows the **last
  call**, not the session — `desktop/agent_usage.go` takes the final ledger row. Check the session total
  before believing the ring.

## Reading a drop (three steps)

1. **Get the per-call sequence.** Walk `messages.jsonl`'s assistant records in time order and find where the
   rate fell. Note the time of the bad call.
2. **Compare `tools` and `system_prompt` byte for byte** between the last call before it and the bad one.
   Names-changed → tool-set change (above). `system_prompt` differs → the prompt changed (working
   directory, session id, capability declarations). Identical → the difference is in the messages:
   a rebuild rendered one differently, or a record's image part could not be resolved (a session directory
   whose `images/` blob is gone, or a record written before images were persisted — see above).
3. **Read the breakpoint level.** The bad call's `cache_read_input_tokens` is how far the prefix matched.
   Against the yardstick of *system + 16 tool schemas ≈ 7k tokens*:

| Cached | Where the prefix broke | Meaning |
|---|---|---|
| ≈ 0 | the very start | the whole request differs — model, provider, system prompt |
| ≈ 3.5k | mid tool array | a tool was inserted or removed in the middle |
| ≈ 7k | the first message | a tool at the end of the array changed, or the **first message** did |
| far above 7k, below the previous total | inside the messages | **turns that ran since the last reload** were rebuilt differently (this family's signature) |
| the same, with the break at a message that carried an image | the FIRST such message | that record's image part could not be rebuilt (the blob is gone, or the record predates image persistence — see above); the whole tail from that image re-reads |
| the previous total | nowhere | nothing changed — the rate should be ~100% |

A **single** low call followed by recovery is a re-warm (normal). **Consecutive** low calls mean every turn
is drifting — that is a new defect, not a re-warm.

## Baseline — compare against this instead of guessing

- **Restart with an unchanged tool set**: the first call should hit almost everything; only this turn's own
  reminder + message is new. An image in the conversation does not change that any more — its parts are
  rebuilt from the session's `images/` blobs. A record whose image part cannot be rebuilt (pre-dates this,
  or its blob is gone) is the one case that still re-reads the tail from there, and it says so in the log.
- **The restart right after a prefix-shaping fix**: one full re-read is the migration cost, not a regression
  — it recovers on the next call.
- Cheap arithmetic for cost: `uncached × input_price` vs `cached × cache_read_price` from the ledger row
  (the configured ratio is large, so the miss is where the money is).

## Practical notes

- **Write the analysis to a file and run it** (`python3 /tmp/probe.py`) rather than stacking nested quotes on
  one command line. The unattended entry modes (`channel`, `-p`, subagent) treat a policy `ask` as a refusal,
  and a command whose syntax is incomplete is conservatively an `ask` — there it is simply refused.
- Diagnose in the order above: a drop's face does not identify its cause. A tool-set change, a seam drift
  and a whole-history rebuild are indistinguishable at a glance — they all read as "1.x%".
- When the drop is explained by a prefix change you just made, say so and quantify it — "this costs N tokens
  once" is a very different report from "the cache is broken".
