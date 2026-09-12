# Desktop smoke suite

`make desktop-smoke` drives the **real** desktop app: it launches the built bundle, a
scripted model answers it, and a driver does what a user would — typing, clicking, reading
what appears. The verdict is an exit code.

```sh
make desktop-smoke                        # everything
make desktop-smoke ARGS="-run send-now"   # one scenario
make desktop-smoke ARGS="-run send-now,sessions -v"   # a few, with every assertion printed
make desktop-smoke ARGS="-keep"           # keep the sandbox after a pass (artifacts)
```

## Why not unit tests

Unit tests and the headless itest suites (`itest/run`, `itest/tui`, `itest/acp`) cannot see
whether a Wails binding's result reaches a card, whether a queue interruption lands its
reply, or whether a plan's step statuses move on screen. Every desktop bug that shipped
lived in exactly that gap. This suite exists to watch the window instead.

## How a run works

```
 itest/desktop/
   main.go        the runner: sandbox, launch, verdict, report
   scenarios.go   one conversation per scenario (mock steps + Go-side assertions)
   sandbox.go     bundle prep, isolated HOME, fixtures, launch/kill/capture
   sink.go        the loopback endpoint drivers report to
   drivers/       harness.js + one script per scenario
```

Per scenario the runner builds a throwaway sandbox:

```
<sandbox>/
  TachiSmoke.app/   a private copy of the app, executable renamed so no pkill can reach
                    the user's own Tachi (same reasoning as docs/agents/desktop.md's bundle rules)
  home/             $HOME for the run: config.yaml pointing at the mock, session store
  work/             the session's working directory (what tools and @-references see)
  driver.js         harness.js + the scenario, with the sink URL filled in
  dom.html          the page as the verdict found it (always available)
  mock-requests.txt what the model was actually asked
```

The driver asserts what the UI **shows**; the runner asserts what actually **happened**
(the prompt the model received, the files written). A regression usually shows up in one
of the two, so both are checked.

## Adding a scenario

1. `drivers/<name>.js` — the driver. Use the harness: `smoke.waitFor` (never a fixed
   sleep), `smoke.check(label, ok, detail)`, `smoke.finish()`. Stop early when a wait
   fails; the failure is already recorded and later assertions only cascade.
2. `scenarios.go` — append to `scenarios()`: the mock's `steps`, the work-dir `files`, and
   an `after` func for the Go-side assertions (`c.requestSeen(...)`: did the model get
   told; `c.planFiles()`: what landed on disk).

Keep both halves small: one behaviour per scenario, and assert what the user would notice.

## Constraints and gotchas

- **Run one smoke at a time.** Two runs fight over the screen: the runner's preflight may refuse,
  and — worse — an app window that ends up behind another stops painting (measured: 0 rAF frames
  while another window was in front), and a resize observer is delivered by the browser's
  rendering steps, so an occluded window starves that probe too. `switch-scroll` falls back to a
  timer rule when no delivery arrives, and prints which rule judged; do not read its PASS/FAIL
  without looking at that line.
- **A probe must judge what was painted — and WHERE the read happens is what decides that.** A pin
  runs in a ResizeObserver callback (after layout, before paint), so a read taken BEFORE the pin
  only reports the intermediate state "content grew, pin not run yet" — which is never drawn.
  Reading `scrollHeight` forces layout, and layout comes before the pin, so a rAF probe is exactly
  such a read: it reported a lone 298px excursion with 0 on both sides. The honest read is in an
  observer of your OWN — observers are called in registration order, so one registered after the
  app's runs after the pin and still before the paint. Verify a new judge BOTH ways: it must read
  0 with the fix in place, and catch the drift with the fix disabled. (Disabling the pin in this
  very scenario is what turned a 298px figure into a DRAWN excursion — the same number the rAF
  artefact had reported, which is precisely why the second reading is the one that counts.)

- **There is no screenshot.** Capturing the window needs it on the visible Space, and
  macOS 15+ has closed the window-capture APIs (`CGWindowListCreateImage` is obsoleted,
  `screencapture -R` fails, `-l <windowid>` is blank for a WebKit window) — a "capture"
  that silently returns the desktop is worse than none. The evidence is `dom.html` plus the
  assertion lines; for a picture, take one by hand (the recipe in docs/agents/desktop.md).
- **The driver runs once, at launch.** It is handed to the app through `TACHI_DEMO_JS`
  (see `desktop/demo.go`), so a scenario cannot be re-run in a live app.
- **Only one instance.** The runner refuses to start while a `TachiSmoke` is alive: `open`
  on a live instance merely activates it and passes NO environment, which would silently
  drive the wrong app (or the user's).
- **`bin/Tachi.app` must exist** (`make -C desktop build`); the executable inside it is
  refreshed from `bin/Tachi` on every run, so the app under test is always the current tree.
- **The suite never touches the real `~/.tachi`**: HOME is the sandbox's, and the session
  fixture is written with the `session` package (not hand-rolled JSON, so it cannot drift).
