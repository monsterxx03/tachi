# Prompt cache & hit rate (handbook)

Read this **when a cache hit rate drops** — the user's or your own — or when a restart looks
expensive, and **before changing anything that shapes a request's prefix**.

## Contents — read the section you need

| Section | Read it when |
| --- | --- |
| **What actually caches** | you are about to change the system prompt, the tool list, or how a stored message is rendered |
| **Where the numbers are** | you need the hit-rate data (or you are about to trust the desktop's ring) |
| **Reading a drop** | a drop has been reported and you need to locate it |
| **Baseline** | you want to know whether a number is normal, and what it cost |
| **Practical notes** | you are about to run the analysis, or to write it up |

Related: [`.tachi.md`](../../.tachi.md) (the Session History & Reminders invariant),
[api request recording](../2026-08-15-api-request-recording.md) (what `api_requests.jsonl` records per
request), [plan-panel design §7](../2026-09-12-desktop-plan-panel-design.md) (the reminder-reconstruction
family and its measurements).

## What actually caches

The hit that matters is the **provider's own prefix cache**, not the client's markers: `llm/anthropic.go`
puts `cache_control` on the system prompt only (with a standing `FIXME`), yet 290k-token prefixes hit at
~99.9% and break exactly where the request's prefix diverges. The prefix is **tools → system → messages**,
in that order.

Two consequences drive everything below:

- **Any tool-list change invalidates the whole message history.** Tools come first, so inserting, removing
  or reordering one (MCP connect/disconnect, tool enable/disable, a new tool from an upgrade) makes
  everything after it a miss. Moving dynamic tools to the end does not help — the messages are still behind
  them. This one is structural: expect one full re-read, and say so rather than hunting for a bug.
- **The rebuilt history must equal the history that was SENT, byte for byte.** Anything that renders a
  stored message differently from how it went out (an extra newline at a reminder seam, a block in the
  wrong position) shifts the prefix from that point on. The invariant and its two shapes are in `.tachi.md`;
  `agent/session_convert_test.go` pins them.

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
- **Credit ignores the cache** — it is charged on total tokens, so a cache collapse costs money (the price
  ratio between a miss and a hit is large; the ledger's own prices are the truth) but barely moves 积分.
- The desktop's cache ring (in the usage row under the composer, beside ¥cost and 积分) shows the **last
  call**, not the session — `desktop/agent_usage.go` takes the final ledger row. One bad call therefore
  reads as 1.x% while the session cumulative is still ~99%; check the session total before believing the ring.

## Reading a drop (three steps)

1. **Get the per-call sequence.** Walk `messages.jsonl`'s assistant records in time order and find where the
   rate fell. Note the time of the bad call.
2. **Compare `tools` and `system_prompt` byte for byte** between the last call before it and the bad one.
   Names-changed → tool-set change (above). `system_prompt` differs → the prompt changed (working
   directory, session id, mode, capability declarations). Identical → the difference is in the messages.
3. **Read the breakpoint level.** The bad call's `cache_read_input_tokens` is how far the prefix matched.
   Against the yardstick of *system + 16 tool schemas ≈ 7k tokens*:

| Cached | Where the prefix broke | Meaning |
|---|---|---|
| ≈ 0 | the very start | the whole request differs — model, provider, system prompt |
| ≈ 3.5k | mid tool array | a tool was inserted or removed in the middle |
| ≈ 7k | the first message | a tool at the end of the array changed, or the **first message** did |
| far above 7k, below the previous total | inside the messages | **turns that ran since the last reload** were rebuilt differently (this family's signature) |
| the previous total | nowhere | nothing changed — the rate should be ~100% |

A **single** low call followed by recovery is a re-warm (normal). **Consecutive** low calls mean every turn
is drifting — that is a new defect, not a re-warm.

## Baseline — compare against this instead of guessing

- **Restart with an unchanged tool set**: the first call should hit almost everything; only this turn's own
  reminder + message is new (measured on a ~485k-token prefix: 342 uncached, 99.9%).
- **The restart right after a prefix-shaping fix**: one full re-read is the migration cost, not a regression
  (measured: 467k uncached on a ~467k-token prefix, ≈ ¥0.47) — it recovers on the next call.
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
