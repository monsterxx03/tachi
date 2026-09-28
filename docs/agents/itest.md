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

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md) (the smoke suite, which is a different
harness), [prompt-cache.md](prompt-cache.md) (why a mock's request counter is not a turn boundary).
