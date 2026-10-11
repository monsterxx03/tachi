# Tachi — Integration Test Handbook (split out of `.tachi.md`)

Read this **before adding or editing a spec under `itest/`, or when one fails only on a loaded runner** (a
flake here is usually a wait anchored on the wrong thing, not a product bug).

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New itest lessons belong in THIS file, not back in
`.tachi.md`** — and the rules that ride with that file (freshness in the same turn, convention-not-history,
split + index when a subject grows) apply to this split too.

## Suites and targets

- Suites: `itest/run`, `itest/tui`, `itest/acp`, `itest/mockllm` (run/tui/acp depend on mockllm).
- `make itest` (= `make -j4` + `ginkgo -p`); one suite: `make itest-run|itest-tui|itest-acp|itest-mockllm`.
- `itest/tui` drives the real `tui.Model` + `AIAgent` in-process under `tea.NewProgram` (keys via
  `p.Send(tea.KeyPressMsg)`); the ANSI diff stream is replayed through `itest/tui/vt.go`, so `Screen()` == a
  real terminal.

## What a spec must anchor on

- **A spec's waits anchor on the AGENT's turn state, never on the screen alone**: the driver's
  `WaitIdle`/`Stop` wait for `AIAgent.TurnInFlight()` (the run goroutine behind every session write) to clear.
  A rendered idle frame lags the state machine and the mock's request counter rises on the mock's goroutine,
  so "request counted + still-idle-looking screen" is reachable mid-turn — a wait on those two returns early,
  and a spec that then types (queued as pending instead of a new turn), quits the program, or lists sessions
  fails randomly on a loaded runner only.
- **`config.SetBaseDir` / `tools.SetWorkingDir` are process globals** — safe only because ginkgo parallel
  nodes are separate processes with serial specs; pin a fresh working dir per spec (a deleted `t.TempDir`
  makes Bash fork/exec fail).
- **Hermetic**: the harness config disables `herdr`; never touch the real `~/.tachi`.

## The harness

- **`itest/harness.BuildBinary` caches the binary by git HEAD hash** in `os.TempDir()/tachi-itest-<hash>`; a
  dirty tree falls back to per-call builds → keep the tree clean.
- **mockllm speaks 3 wire protocols** (openai / anthropic / openai-res); a new one needs route + normalize +
  render + contract tests.
- **`run.Binary` waits for the process; `run.Spawn` does not.** A HOSTING entry has to stay up (its node
  service dies with it), so a spec that talks to one uses `Spawn` and owns the process — `Stop` in a
  `DeferCleanup`, or a leftover host holding the node lock fails the NEXT spec of that worker for a reason
  that has nothing to do with it.

## A node with no terminal (two real processes)

- **Only three entries host a node** (`node.StartOrAttachNode`: the TUI, the desktop, the channel entry), and
  **only the channel entry runs headless** — but it refuses to start with no channel enabled. `channel/mock`
  is the inert channel that satisfies it: `Name`/`OnStart`/`Run`, where `Run` blocks until shutdown. It is not
  user-facing (only `main_channels.go` reads the registry, so nothing lists or offers it).
- **The fixture must NAME the A2A port.** Nothing publishes a hosting process's remote address — `local.json`
  carries the LOCAL control address, the node id, the instance id and the token, and nothing else — so a spec
  that keeps the peer in a separate process picks a free port (`0.0.0.0:<port>`, which also puts it off
  loopback and therefore behind mTLS) and puts it in the config.
- **Waiting for `local.json` is NOT waiting for a node.** The file outlives its writer by design (`Close`
  leaves it, because the attach path re-checks the instance id and the handshake, so a stale file is harmless
  there) — and an entry that dies right after publishing leaves it behind. Readiness is "the file appeared"
  AND "something answers on the port" (`net.Dial`; the handshake is EXPECTED to fail on a non-loopback
  surface, so a TCP connect is all that can be checked). When the process exits while waiting, report ITS
  output: "never became ready" sends a reader looking in the wrong place while the answer — a channel that is
  enabled but not registered, a taken port — sits right there.
- That pair is what makes the cross-process specs possible at all: two identities, two TOFU pin records, two
  data directories and two logs, each written by its own process (`itest/run/node_host_test.go`). **Keep the
  in-process peer for everything else**: it proves the config→surface path without a second process, and only
  the process boundary needs this machinery.
- **What the three specs in that file cover**, so they are not rewritten: (1) a real process hosts and another
  real process delegates to it over mTLS, with both sides' records checked on disk; (2) a second process on
  the SAME `--home` ATTACHES instead of starting a second service — asserted on its own `attached to local
  node service` line (pid-attributed, because two processes sharing a home share its log file) plus the lock
  being held by someone else and the published instance id being the first process's; (3) a host killed with
  `-9` while a task was parked on a question — the next start takes the freed lock (a flock dies with its
  holder) and resolves the task as interrupted, keeping the question. The retention COORDINATION lock's
  contention tests are still deferred (`node.md`); these are about the node lock.
- **Each of those three breaks in a way worth knowing**: without the inert channel the host exits right after
  publishing (the readiness trap above); without the kill, the restart attaches and the interrupted-task
  assertions never fire; with a different home, the "second" process hosts and the attach assertions never
  fire. All three were checked by breaking them, and each failure is reported with the process's own output
  rather than as a timeout with no reason.

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md) (the smoke suite, which is a different
harness), [prompt-cache.md](prompt-cache.md) (why a mock's request counter is not a turn boundary),
[node.md](node.md) (what the node service does with the process it lives in).
