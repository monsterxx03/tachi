# Tachi — Desktop Handbook (split out of .tachi.md)

Read this **before changing anything under `desktop/`**.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New desktop lessons belong in THIS file, not back
in `.tachi.md`** — and the rules that ride with that file (freshness in the same turn, convention-not-history,
split + index when a subject grows) apply to every split of it, this one included.

**What this file is: the OUTLINE — each bullet is one invariant, plus the bug it prevents.** The mechanism
behind it belongs in the code it is about (this repo's source comments carry that load) or in a dated design
doc under `docs/`; a bullet here says WHAT must stay true and WHY, and a subject that outgrows a paragraph
gets its own handbook beside this one rather than growing here.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Desktop Smoke Verification** | you changed anything a driver can see (UI, events, a binding, an end-to-end flow) — **start with its routing table** |
| **Desktop Build & Signing** | you build/package the app, or debug notifications/TCC |
| **Badge (device link & voice)** | anything under `desktop/link/`, `desktop/badge.go` or `desktop/stt/` — **read `docs/agents/badge.md`, not this file** |
| **Desktop Voice Input** | you touch the composer's microphone button or the `voice:*` events (`desktop/voice.go`, the voice half of `composer.tsx`) |
| **Desktop Backend (bindings & paths)** | you touch `AgentService` — a method, a binding, a session path, an open/reveal action |
| **Desktop MCP** | you touch MCP authorization, the MCP panel, or the deferred-tools pool/reminder path |
| **Desktop Session Tree** | you touch skill discovery, permissions, MCP profile scope, or anything else that resolves "which tree does this session work in" |
| **Desktop Turn History** | you touch how a turn's history is assembled, merged or continued |
| **Desktop Compaction** | you touch the compaction path (`agent/compact.go`, `desktop/compaction.go`, the `AgentEventAutoCompactDone` case), `RunningSessions`, or anything that resolves **which run owns a session** |
| **Desktop UI State** | you change the frontend: which module owns what, and the rule each convention keeps |
| **Desktop Themes** | you touch colours |

## Desktop Smoke Verification (demo + mockllm)

Desktop-only paths (Wails wiring, rendered UI, end-to-end flows) need the real window: unit tests and itest
cannot see whether a binding's result reaches a card. **Use the in-repo suite** for anything touching desktop
state, events or UI, and add a scenario when a fix deserves a regression test.

```sh
make desktop-smoke ARGS="-run send-now,perm-allow -v"   # the scenarios your change touches (see below)
make desktop-smoke                                      # everything (~50 scenarios, minutes)
make desktop-smoke ARGS="-keep"                         # keep the sandbox after a pass (artifacts)
```

### Which scenarios to run — run the touched ones, not the suite

**Pick from this table; a full run is for a change that reshapes the transcript or the event plumbing
itself.** When two rows apply, run both. `-run` takes a comma-separated list.

| You changed | Run |
| --- | --- |
| transcript rendering / turn folding / process strip | `transcript`, `transcript-fold`, `transcript-live` |
| tool cards (their own args and result, several calls of one tool in a turn) | `parallel-tools`, `transcript-fold` |
| the live status line, its roll, or a row's geometry | `transcript-live`, `live-roll` |
| a message bubble's own layout (long links, wrapping, the reader's own line breaks) | `bubble-wrap`, `bubble-lines` |
| the reading size (⌘= / ⌘- / ⌘0, and the two stores it lands in) | `font-size`, `bubble-lines` |
| composer, send/queue/steer routing | `send-now`, `steer-fold`, `composer-height` |
| the microphone button / the `voice:*` events (states, where the text lands) | `voice-input` |
| scroll pinning, session switch, load, paged history (a turn across a page seam) | `switch-load`, `switch-scroll`, `turn-paging` |
| sidebar rows, grouping, titles, type scale, carets, a compaction chain (folded, and with its other end gone) | `sessions`, `sidebar-text`, `switch-load`, `chain-orphan` |
| the sidebar's width (drag, clamp, the choice surviving a restart) | `sidebar-width` |
| pinning a session (order inside its group, the mark, the menu item) | `sessions`, `projects` |
| the stop control / which session a status belongs to | `session-running` |
| projects (groups, binding, panel, palette, forms) | `projects`, `project-palette`, `form-text` |
| 会话搜索 ⌘K (which rows, what filters, IME composition, where Enter lands, a folded group) | `session-palette` |
| the menu bar's 「新会话」 (the shell's `ui:new_session` request) | `tray-new-session` |
| permission cards / bash `ask` / session-wide allow | `perm-allow`, `perm-deny`, `perm-session`, `perm-unparseable` |
| the MCP search cost card (its copy, and a refusal sticking for the session) | `perm-mcp-budget` |
| the AskUserQuestion form (options, the free-text field, the key that submits it) | `ask-form` |
| a turn's *changes* (footer chip, diff pane, review scope) | `bash-diff`, `frozen-panel`, `multi-root` |
| the diff overlay (file index, sections, drag, current row) | `file-index`, `multi-root`, `oneoff-footer` |
| the diff's 上下/左右 layout (pairing, blank cells, column scroll, the choice surviving) | `diff-layout`, `review-all` |
| review / commit side panel, report, findings | `oneoff-panel`, `oneoff-footer`, `oneoff-report`, `review-all` |
| rewind / checkpoints | `rewind`, `compact` |
| context meter, usage numbers, auto-compaction (the loop compacts on its own, mid-turn) | `ctx-ring`, `compact`, `auto-compact` |
| the compaction WINDOW (the child exists while the turn is still running: which row shows 运行中, what a send in the child does) | `compact-window` |
| MCP panel, OAuth, deferred-tools reminder | `mcp-auth` |
| skills discovery / the catalog | `skill-catalog`, `project-context` |
| workspace roots (add/remove, git branch, @-files) | `roots-branch`, `multi-root`, `at-file-reload` |
| a previewed file / the viewer / lightbox / find | `file-find`, `zoom-fit`, `mermaid-zoom` |
| ⌘F over the transcript (what it searches, where it lands) | `chat-find` |
| a turn whose stream dies mid-flight (what it keeps, what the next turn sends) | `stream-fail` |
| plans, `SavePlan`, the plan panel | `plan-writeback` |
| the settings page (groups, a config list's card, a dropdown, a save, its type scale) | `settings` |
| sending/receiving files or images | `paste-image`, `image-reload` |
| deleting sessions, running-session guards | `delete-running` |
| whatever else — the transcript itself, session rows | `transcript`, `sessions` |

The suite does not need its window in front, and does not take the front: demo mode keeps the page awake
while another window covers it, and a run launches without activating itself — so a run can be left going
while you work.

### Shape of the suite

It is `itest/desktop/`: `main.go` (runner: sandbox, launch, verdict, report), `scenarios.go` (one
conversation per scenario — the mock's steps plus the Go-side `after` assertions), `sandbox.go` (bundle prep,
isolated HOME, launch/kill), `sink.go` (the loopback endpoint drivers report to), `drivers/` (harness.js +
one script per scenario). Per run it builds a throwaway sandbox holding a private copy of the app (executable
renamed `TachiSmoke` so no `pkill` can reach the user's own Tachi), an isolated HOME with a `config.yaml`
pointing at the scripted model, the work dir, `driver.js`, `dom.html` (the page as the verdict found it) and
`mock-requests.txt` (what the model was actually asked). **`desktop/bin/Tachi.app` must exist**
(`make -C desktop build`); the runner refreshes the executable inside it from `bin/Tachi` on every run.

A driver asserts what the UI **shows** (transcript shape, chips, buttons); the scenario's Go half asserts what
actually **happened** (the prompt the model received, the files written) — a regression usually shows up in
exactly one of the two, so write both. A new scenario is a `drivers/<name>.js` against the harness
(`smoke.waitFor` — never a fixed sleep — `smoke.check(label, ok, detail)`, `smoke.finish()`, plus
`q`/`qa`/`text`/`allText`/`type`/`pick`/`click`/`key`/`sleep`) plus its entry in `scenarios.go` (the mock's
`steps`, the work-dir `files`, an `after` func), added to the routing table above. Keep both halves small: one
behaviour per scenario, asserting what the user would notice.

Three traps in the driver half, each of which cost a debugging session:

- **Read a computed style only for the frame AFTER the one that changed it.** A scripted key changes
  `--fs-scale` synchronously, but `getComputedStyle` keeps answering with the PREVIOUS frame's value until
  WebKit's style recalc runs — so measuring right after the key reads the size from before it, and the next
  `waitFor` compares against a baseline that is already the target (the `font-size` scenario did exactly this:
  `⌘0`, measure, then "⌘= made it bigger" waited forever). Wait for what you want to SEE — `waitFor` polling
  the computed size — never for a sleep.
- **A scenario whose LAST step is fire-and-forget must let the write land** (`smoke.sleep` before `finish()`).
  The runner kills the app as soon as the driver reports, so a bound call still in flight — `⌘=`, a panel width,
  a theme — is a file write that never happens, and the scenario's Go half reads the value from one step back.
  This is the one legitimate fixed wait in a driver, because there is no state to wait for: the UI is already
  correct, and what is owed is a side effect (`font-size` needed 500 ms after a 200 ms debounce).
- **The smoke bundle is signed with the LOCAL CERTIFICATE, not ad-hoc** (`smokeSignID` in `sandbox.go`), and it
  has to be: desktop links the BLE package, whose adapter builds a `CBCentralManager` at package INIT, so this
  bundle asks for Bluetooth permission whether or not a scenario goes near the badge. An ad-hoc signature's
  identity IS the content hash, so every rebuild is a NEW app to macOS and the prompt returns on every run;
  with the certificate macOS records the answer once. No certificate on the machine → ad-hoc, and the prompt
  is back (the suite still runs).

Fixtures exist where the real path needs something a driver cannot do:
- `extraRoots` — additional workspace roots; seeded into the session, because the UI's only way in is a NATIVE
  directory picker no driver can click. Reach a seeded root from the work dir as `../<name>/…`.
- `config` — a YAML block appended to the generated `config.yaml` (how a parked-permission scenario gets its
  `permissions.bash.ask` rule).
- `pricing` — the mock provider's own price table, for a scenario that asserts on MONEY (`perm-mcp-budget`'s
  cost card). It cannot ride in `config`: prices live INSIDE the provider entry, and a second `providers:` key
  would not load. Nil everywhere else — an unpriced model bills nothing, which keeps the other scenarios'
  ¥/积分 figures at zero and out of their assertions.
- `project` — a desktop project: `projects.json` and the session's `project_id` are seeded, same reason.
- `seedMessages` / `seedSecond` — a transcript written before the app launches. A driver cannot produce a
  reload by itself (switching sessions serves the in-memory copy), so this is how a restart-shaped assertion
  is staged.
- `orphanChain` — a session whose `compacted_parent_id` names a session that does **not** exist. A driver can
  neither write a session record nor delete one, so this is the only way to stage "a compaction chain whose
  other end is gone" — what a parent deleted by hand, or swept by session cleanup's `max_keep`, leaves behind.
- **A fixture has no "current session" — the app opens the NEWEST one** (`ListSessions` sorts by `CreatedAt`),
  so the session a driver asserts on has to be seeded LAST; seeding an unbound session after it made the
  driver assert on the wrong one, which reads exactly like "the project does not drive its member".

### Rules and traps

- **Only one instance may run.** `open` on a live instance only hands the launch to the running process and
  passes NO environment (the driver never runs and the previous scenario's app answers), and
  `pkill -f "Tachi.app/Contents/MacOS/Tachi"` matches the user's own app. The unique executable name is what
  makes `pkill -f TachiSmoke` safe. That pattern also matches the SANDBOX's own build (`codesign` carries it
  on its command line), so a stray `pkill -f TachiSmoke` while a run is preparing kills the ad-hoc signing and
  the scenario reports `codesign …: signal: terminated` before its app ever launches. Never kill a run in
  flight: every sandbox app shares one bundle id, so the instance left behind is what the next `open` activates.
- **Run one invocation at a time.** Each driver has a 1m30s budget and its assertions wait ~3s for a transient
  state, so a starved webview misses both. An EMPTY `mock-requests.txt` in a failing scenario's sandbox is the
  signature of a starved run rather than a broken app. Before believing a failure, check
  `pgrep -fl "itest/desktop|TachiSmoke"` and re-run the scenario on its own.
- **A driver that does not PARSE fails as "the app did not start".** The payload is one `(function(){…})()`, so
  a syntax error anywhere in it — the easy one is a `const` declared twice in the same IIFE — throws on EVERY
  retry, and the app's own log stays clean (session dir created, no demo error). `node --check
  itest/desktop/drivers/<name>.js` costs a second; a failing run's kept sandbox holds the copy that ran.
- **A driver must find what it clicks by NAME, never by position.** The page draws one ＋ per declared list and
  several `.set-elem` cards per block, so declaring a new list (or adding an item) MOVES the indices under an
  existing driver: `smoke.qa('.set-add .btn')[0]` silently became the ＋ of `jev.pricing.bands` when that list
  was declared, and the scenario then clicked a button belonging to another list — every later "the new card
  appeared" assertion failed, on a page that looked fine. Scope by the label the element CARRIES (the ＋ that
  says `＋ 添加 providers`, the card's own head, a row INSIDE that card) and by nothing else; the same rule
  says a leaf's TAIL is not unique on a tab either (`jev.api_key` and `providers.0.api_key` are both
  `api_key`, and only one of them has a value).
- **The harness can read the DOM, not the PAINT.** Every assertion goes through `getComputedStyle`, geometry
  or text, so a bug where WebKit simply does not paint something is INVISIBLE to a green run — the elements are
  there, their box is right, their styles are right. Do not read "no spec fails" as "it renders correctly"; a
  driver's job is to pin the MECHANISM that causes such a bug (see the `settings` scenario's
  `.app-body[inert]` + sidebar-`visibility` pair).
- **A driver dispatches a synthetic key INTO THE PAGE, so it can never say whether the menu bar claims that
  key** (`smoke.key` fires a `KeyboardEvent` on an element; the native menu is not in that path at all). A key
  equivalent a menu ITEM owns is handled before the webview sees the event — which is why the reading size's
  ⌘= / ⌘- / ⌘0 needed Wails' `View`→zoom items removed (`desktop/menu.go`) and why that half is pinned by
  `desktop/menu_test.go` (asserting on ACCELERATORS, not on labels). `font-size` asserts what the keys DO.
- **A driver must not click an action that hands the screen to another app**: 打开会话目录 launches the real
  Finder. Assert the control on screen and pin the argument off screen (`TestOpenSessionDirOpensTheSessionDirectory`
  stubs `openFile` and reads the argv). Any "open in …" action has this shape.
- **A control used as a TRIGGER for another behaviour pins neither** — a button and a page switch must not ride
  on one click. And **an assertion belongs to the scenario that produces the fact**: a check that sat in
  `oneoff-footer`'s `after` while the driver producing the fact is `oneoff-panel`'s read a default forever.
- **Wait for the fact the action reads, not for a neighbour that correlates with it.** The 评审本轮改动 chip
  exists after its FIRST changed file, not its last, so a click on 「完整 diff」 that waits only for the chip
  captures a one-file set.
- **A transient state belongs to the trail; a steady state belongs to a direct read.** A UI state whose length
  is the WORK's length (the 评审中… chip) makes `waitFor` a coin flip — sample on a ~10ms timer, dedupe into a
  trail, assert the trail CONTAINS the state. But read the END state off the element the wait returned.
- **With Reduce motion ON, EVERY property change is a real transition** (`transition-duration: 0.01ms
  !important` in base.css). 0.01ms is not zero: a driver that writes a style and reads
  `getBoundingClientRect()` in the same task gets the OLD value, however correct the code is.
- **A stale element swallows a gesture**: re-query the handle for EVERY synthetic drag, or the drag lands on a
  detached node and passes against the previous value without dragging at all.
- **A control run proves nothing until the build is PROVEN to carry it** (`make frontend && go build -o bin/Tachi .`):
  `npm run build` is `tsc && vite build`, so an edit that does not type-check skips the Go build via `&&` and
  the run measures the PREVIOUS binary. Check `[ bin/Tachi -nt <the edited source> ]`.
- **A capture is not part of the suite**: the window-capture APIs are gone on this OS (`-R` fails on macOS 26)
  and the window would have to be on the visible Space anyway. The suite keeps `dom.html` + the assertion
  lines; a "screenshot" that silently returns the desktop is worse than none. To SEE a palette change, render
  the tokens into an HTML page instead (see Themes).
- **The demo bootstrap and the page-awake switch are load-bearing; leave them alone unless you are fixing
  them.** `TACHI_DEMO_LOAD_MS` (`demo.go`) is the driver's head start, the payload is re-issued for a 10s
  window because a script injected into a loading document is dropped, and the repeats are safe because the
  payload guards on `window.__tachiDriverStarted` (a scenario reporting TWICE means that guard is gone). The
  demo starts from `ApplicationDidFinishLaunching`, never a sleep: a main-thread dispatch before `App.Run()`
  builds the platform impl PANICS the process, which from outside is indistinguishable from "the app did not
  start".
- **A covered window is not a paused run — demo mode keeps the page awake, deliberately.** WebKit reads a
  page's visibility off the WINDOW alone (on screen, not miniaturized, not occluded) and never off which app is
  frontmost, so an ordinary covered window is a *hidden* page: frames stop, DOM timers clamp to 1Hz, and a run
  can post NOTHING before the timeout. Read `等了 90s 没收到结果` as *the page was throttled*, not *the driver
  hung*; the run's `页面节流已解除` line says whether the switch was lifted (`keepPageAwake`,
  `webview_awake_darwin.go`). The harness asserts the outcome on EVERY scenario, so a Wails/WebKit update that
  drops the SPI fails on one readable line.
- **The console is part of the report**: the driver's `console.error` lines ride in `Result.Console`, printed
  for passing runs too.

## Desktop Build & Signing (macOS)

- **`make build` leaves the app ad-hoc signed; run `make sign-local` after it.** macOS ties TCC grants
  (notification permission included) to the bundle's code identity, and an ad-hoc identity is a content hash —
  it changes on every rebuild, so a granted permission is forgotten and the native notification path can end up
  refused for good (`Notifications are not allowed for this application`). `sign-local` re-signs with the local
  self-signed identity `Tachi Local Code Signing` (login keychain, trusted for code signing), which is stable
  across rebuilds. Build that cert once — `security find-identity -v -p codesigning` says whether one exists —
  with `security` + OpenSSL, exporting the PKCS#12 with **`-legacy`** (macOS cannot verify OpenSSL 3's default
  algorithms), and trust it for code signing.
- **Notifications only fire while the window is NOT focused** (`desktop/notify.go`): testing with the app in
  front proves nothing. The TUI's notifications are a different mechanism (`terminal-notifier` / `osascript`),
  **but they can still be the desktop's fault**: the app is often launched from a herdr pane, inherits
  `HERDR_ENV`/`HERDR_SOCKET_PATH`/`HERDR_PANE_ID`, and then auto-enables the herdr hook — so herdr, not Tachi,
  raises a terminal notification for a window that has no pane. `DetectHerdr` therefore also requires stdout to
  be a terminal. `log show --last 10m --predicate 'eventMessage CONTAINS "terminal-notifier"'` prints the TCC
  attribution with the **responsible** process. `notifyTurnDone` is for the transcript lane only: a side-channel
  run (/review, /commit) has no turn on screen, so it gets its own copy through `notifyOneOffDone`, raised from
  `commands.go`, where the outcome is known.
- **A signature change STRANDS the TCC rows it replaces: reset, then re-add — per service, by hand.** TCC
  stores the requirement it matched at grant time (an ad-hoc build's is `cdhash H"…"`, which is why sign-local
  exists), so re-signing leaves an entry that still reads as granted in the pane and fails every request —
  `Failed to match existing code requirement for subject <bundle-id>`, in
  `log show --last 10m --predicate 'eventMessage CONTAINS "kTCCServiceScreenCapture"'`, which prints both the
  stored requirement and the app's current one. `tccutil reset <service> <bundle-id>` followed by re-adding the
  app (系统设置 → 隐私与安全性 → 屏幕录制 → `+`, Finder's ⌘⇧G for the path) is the fix; screen recording
  **never prompts** (`does not allow prompting; returning denied`), so a missed pane visit looks like a broken
  build. Grants are judged against the **responsible** process, which for `open bin/Tachi.app` (PPID 1) is
  Tachi.app itself: a screenshot the agent takes by shelling out `screencapture` needs the APP granted, never the
  terminal it was launched from.

## Badge (device link & voice)

- **Split out — read `docs/agents/badge.md`**: the BLE transport traps (framing, the liveness poll, the write
  cap, TCC), the voice/STT path, and how the device side is built and flashed.

## Desktop Voice Input (the composer's microphone button)

- **It reuses the badge path's transcription HALF and none of its delivery.** The transcription is the same
  call (`stt.Transcribe`, same zh-CN/en-US modules, same `Choose`, same "why it failed" sentences — `voiceFailureText`
  serves both callers); what differs is where the text goes: the device has no keyboard, so its voice becomes a
  message (`badge:voice` → the frontend's `submit`), while the desktop has one, so this voice becomes a DRAFT
  spliced into the composer at the caret (`voice:text`). Two events, on purpose — collapsing them into one
  would make "send" and "fill the box" the same word.
- **The BACKEND owns the state machine, including the 60s watchdog** (`desktop/voice.go`: idle → preparing →
  recording → transcribing → idle). The button draws `voice:state` and the click only asks for a transition —
  a local boolean diverges from the microphone at exactly the wrong moment (the first-ever authorization
  window, where a second click would "stop" a recording that has not started). The watchdog is what keeps an
  abandoned recording from holding the microphone open with nothing on screen saying so.
- **The button and the ⌘⇧M shortcut are ONE call** (`ToggleVoiceInput`, not start/stop): the direction
  depends on whether the microphone is open, which only Go knows, so the decision cannot live in two callers
  that read a lagging event. The state the button reads is for DRAWING (the red pulse, the disabled wait) —
  never for deciding. **It is an app-local key on purpose**: a system-wide one (`app.GlobalShortcut`, Carbon
  hotkeys) takes the combination away from every other app and only pays off once the window can be recalled —
  a separate step, not a bigger version of this one. Its key is chosen to be unclaimed by the menu bar
  (`TestApplicationMenuFreesTheVoiceKey`): a menu accelerator is handled BEFORE the page sees the event, so a
  collision is a dead key that no frontend test can observe.
- **The microphone is a SECOND TCC permission, with its own plist key**, beside the speech one: a bundle
  without `NSMicrophoneUsageDescription` does not get an error when it opens the mic — it is killed (the
  Bluetooth trap in `docs/agents/badge.md`, same class of failure). It is also granted to the APP BUNDLE, so
  the button can only be exercised in one (`make -C desktop run`); a bare binary, or `go test`, cannot be
  granted it — that is why `desktop/stt`'s own test only reads the authorization STATUS and never records.
- **A smoke run must never touch the microphone**: `TACHI_DEMO_VOICE_TEXT` stands in for capture and
  recognition (the env var name is duplicated in `itest/desktop/sandbox.go`, which cannot import the app's
  package). A real recording would raise a TCC prompt on the machine running the suite and capture the room —
  the same reason the MCP OAuth flow gets a headless fetch. The stand-in replaces the audio, never the states
  or the events, so what `voice-input` asserts is the half that has nothing to do with where audio comes from.

## Desktop Backend (bindings & paths)

- **The Wails bindings are GENERATED and COMMITTED**
  (`frontend/bindings/github.com/monsterxx03/tachi/desktop/agentservice.ts`). Adding, renaming or deleting an
  exported method on `AgentService` means re-running
  `cd desktop && GOWORK=off wails3 generate bindings -clean=true -ts -i` (that is `build/Taskfile.yml`'s
  `generate:bindings`, which `make build` runs for you). Never hand-edit one: the numeric `$Call.ByID(...)` is
  derived from the method, and a frontend calling a method the generator has not seen fails `tsc` — the only
  thing that type-checks the call at all.
- **Only `desktop/` is a Go module of its own** (it is NOT listed in the repo-root `go.work`), so every Go
  command for it runs with `GOWORK=off`. **The root `go test ./...` does NOT compile `desktop/`'s test
  package**, and the root `make lint` does not cover it either: after touching `desktop/`, run
  `cd desktop && GOWORK=off go test ./...` too.
- **The packaged app must be signed with the LOCAL CERTIFICATE, not ad-hoc.** `wails3 package`
  leaves `bin/Tachi.app` ad-hoc signed, and an ad-hoc identity is a content hash — it changes on
  every rebuild, so macOS re-asks every TCC permission (bluetooth, speech recognition) and may
  deny notifications permanently. `desktop/Makefile`'s `build` target re-signs automatically
  **when the identity exists** (`Tachi Local Code Signing`, `LOCAL_SIGN_ID`); without it the build
  prints a warning and leaves the ad-hoc signature, which is exactly the state that causes the
  repeated prompts. Create it once with `desktop/scripts/dev-cert.sh` (⚠ that script must use
  `/usr/bin/openssl` and a NON-empty p12 password — OpenSSL 3's default p12 encryption and an
  empty password each produce `MAC verification failed during PKCS12 import`). Verify with
  `codesign -dv --verbose=2 bin/Tachi.app | grep -E "Authority|flags"`: `flags=0x0` plus an
  `Authority=` line means it worked; `flags=0x2(adhoc)` means it did not.
- **A new direct dependency of the ROOT module must be added to `desktop/go.mod` too.** `desktop/` is a
  separate module that `replace`s the root one, so it carries its own `require`/`go.sum` for everything the
  root code imports: a package the root starts importing builds fine there while `desktop/` fails with
  `missing go.sum entry for module providing package ...`. Fix: `cd desktop && GOWORK=off go mod tidy`. The
  reverse holds too — bumping such a dependency at the root must be re-tidied here.
- **`d.cfg` is read under `cfgMu` and written through ONE entry point** (`desktopApp.config()` /
  `withConfig()`, in `agent.go`). Plain-field access is a data race here, not merely a style choice: the
  settings page writes the object while turns read it.
- **⚠️ Never swap the `d.cfg` POINTER; mutate the existing object.** Every provider is wrapped by
  `wrapForUsage`, whose price/credit resolvers CLOSE OVER the `*config.Config` and dereference it per LLM call,
  each session's agent holds it as `AgentConfig.FullConfig`, and the shared MCP manager and its refresher hold
  it too. Those holders read LAZILY, which is what makes an in-place edit take effect everywhere at once — and
  what makes a pointer swap fail everywhere at once, silently (the usage ledger bills at the old prices). So
  `atomic.Pointer[config.Config]` is the wrong tool here, however tempting it looks.
- **One SAVE is one locked batch** (`withConfig` runs under the write lock): `SaveSettings` passes the whole
  batch to `config.ApplyUpdates` inside ONE call, all-or-nothing, so no reader can observe a half-applied
  batch. (`mcpServerByName` is still the one documented exception: a single bool written in place, outside the
  lock.)
- **A config key's "what does it take to make this live" is declared in `desktop/settings.go`, one row per key
  in `settingsTable`** (`applyLazy` = readers already re-read it / `applyPush` = something must be rebuilt,
  `Apply` carries that / `applyRestart` / `applyUnused` = another entry mode owns it). Two tests in
  `desktop/settings_test.go` hold it to the whole of `config.Config` by reflection, so **adding a config field
  fails the build until it gets a row**; `settingApply.needsRestart()` is DERIVED (`Apply == nil` and not
  lazy/unused), never a flag to remember. `TestTheLikelyEditsStayHot` fences the keys users edit most: if a
  refactor makes one of them need a restart again, that test — not a confused user — is what fails.
  - A row's `Apply` does only the PUSH half — the value itself is written into `d.cfg` by the caller — and
    anything that has to reach every session goes through `d.applyToLiveAgents(fn)` (`agent.go`), which returns
    failures labelled with the session's title (or its id). A push is a reload method from
    `agent/agent_reload.go` or a call into an existing one (`SetResolvedProvider`, `SetPermissionRoot`,
    `Setup*Provider`): idempotent, push half only, and it never replaces something a FRONTEND injected
    (`AgentConfig.HookDispatcher` / `.SubagentRunner`).
  - **PUSHES RUN ONLY AFTER `config.yaml` is written** (one save is serialized by `d.settingsMu`): a failed
    save pushes nothing, which is what makes the rollback message true — a push has no undo. A push is also
    never applied UNDER a running turn: the fan-out queues it for a session whose `r.running` is set and
    `endTurn` runs the queue (`desktop/deferred_push.go`), which is why a save made mid-turn answers
    `K 项等这一轮结束生效（…）` instead of claiming the key is live. Process-wide and shared-layer rows (log
    level, prompt-cache clear, the manager's own limits, a profile switch, the poller) are NOT deferred.
  - Two invariants the whole family shares: a REMINDER cannot be replaced (the collector only appends), so the
    agent holds `MemoryRecallReminder` itself and re-points it (`SetBackend`); and a rebuild that owns an
    external resource tears the old one down FIRST (`ReloadMemory` unhooks the old backend before building the
    new one — a half-installed pair is worse than none). A rebuilt subsystem is not identical to a constructed
    one: `ReloadMemory` must
    re-apply the wirings construction adds AFTER configure plus `MemoryState.SkipRecall` — losing them is
    silent degradation, not an error.
  - The two bound methods are `GetSettingsSchema` (the table filtered through the CONFIG's leaves —
    `config.PathsFor` — one field per editable leaf) and `SaveSettings` (refuse the whole batch, snapshot +
    `config.ApplyUpdates` all-or-nothing, write the WHOLE config with `config.Save`, push LAST: a failed write
    restores the exact snapshot). The file is that object marshalled, so a key this page does not own rides
    along untouched and the file stops being a hand-written document (comments and layout do not survive;
    `config.Save` keeps one copy as `config.yaml.bak`). Values are read and written by the SAME dot path as the
    file, via `config.ReadPath`/`SetPath`.
  - One row is the exception to the whole-object write: `active_mcp_profile` sets `OwnsWrite`, because its push
    IS the writer (the batch skips the leaf, and `SaveSettings` lays it over the object for that one marshal
    and lifts it again) — so a failed switch can never leave the field naming a profile whose servers are not
    loaded.
- **The settings page is a pure renderer of that declaration** (`frontend/src/settings.tsx` holds no config
  key), and everything it renders is a rule about PATHS rather than about keys: a `Category` puts a leaf in the
  left column, a top-level key's leaves go under one heading (the longest prefix they share, and the rows below
  drop it), and a config list's item collapses into a card. Three more declarations ride with it, each pinned
  by `desktop/settings_test.go`: `settingChoices` (a SECOND table for leaves with a closed value set, keyed by
  the structural path — `providers.*.type` — because the decision belongs to the list, not to item 0),
  `settingFoldedLists` (a declaration about a LIST, for the ones whose items are drawn folded whatever their
  size), and `Optional`/`IsSet` (`config.DescribeLeaf`: what an unset pointer cannot say about itself, and what
  lets the page draw 自动 instead of guessing from a value). A form REFUSING a value forces its card open over
  every fold rule — an error behind a fold is one the reader cannot act on. The order the page draws in is a
  rendering rule, so `itest/desktop/drivers/settings.js` is what pins it.
- **A credential leaf is drawn masked, and the PAGE does not decide which leaves those are** (`config`'s
  `secretLeafNames` → `LeafInfo.Secret` → `SettingFieldVO`/`SettingElementFieldVO.Secret` → `SecretInput`):
  the rule is the leaf's own yaml name (`api_key`, a web_search/web_fetch provider's `key`), written out
  rather than pattern-matched — a substring rule would also swallow `keyword_provider` and hide a value the
  reader came to read. The flag rides on the element TEMPLATE too, or a card ＋ made would draw one plain box
  for a key typed in from scratch. Masking is DISPLAY and nothing else: the reveal is the box's own state,
  never a draft — a reveal is not an edit.
- **「未设置」 is a state with its own value** (`config.Unset` → the key is simply NOT written, since every
  optional leaf's yaml tag is `omitempty`; a bare `nil` stays REFUSED, because that is what a forgotten
  pointer dereference looks like). It is the only way back to a field's auto/default behaviour, it
  round-trips, and `Optional` is true only for a pointer with NO `default:` tag.
- **A list is grown and shrunk by the SAVE, and the page owns the pending item**: ＋ and 删除 are THIS PAGE's
  state, exactly like a draft — there is no staging area on the server — so `SaveSettings(updates, appends,
  removes)` carries the whole unsaved state in one call. Three consequences: the backend hands over the two
  facts an item that does not exist needs (`SettingListVO.Length`, where a new item lands, and `.Element`, the
  item template, so `GetSettingsSchema` stays a PURE function of the config); the appends are ELEMENT paths and
  the append refuses an index that is not the list's current length (`validateRemovals` refuses the same kind
  of staleness, including **the same element twice** — removals run one by one, so a duplicate silently drops
  two items); and **the order inside one save is the mechanism**: appends, then the leaf updates, then the
  removals — a removal shifts every later index, so an update applied after it addresses the wrong element.
  删除 itself is ONE click whose undo is the point (nothing is written until the save), and the one call it
  makes, `SettingsRemovalWarning`, has both its answers read while the card is still on screen.
- **保存前的预览 is the SAME batch put through the SAME pre-flight, and then marshalled instead of written**
  (`PreviewSettings` / `resolveBatch`, which `SaveSettings` calls too), so a preview cannot promise something
  the save refuses. The diff is generated-vs-generated — never the bytes on disk, whose differences from the
  marshalled form include things the batch did not do — and `Rewrites` carries what that excludes (the save
  rewrites the whole document; the page shows it only when true). Two rules ride with it: the preview never
  writes and says so when the form moves on (「这份预览是旧的」), and it prints a credential VERBATIM on
  purpose — a masked field is a display choice about a FORM, while this panel answers "what will be in the
  file", and `api_key: ***` would make its one answer untrue.
- **While the settings page is up the app body is `inert`, and that is NOT the same as hidden**: the page is
  fixed and opaque, so what is behind it only has to leave the tab order and the accessibility tree — but it
  must keep being PAINTED (a `visibility: hidden` subtree lost its ellipsised sidebar titles for good, issue
  #27) and it must stay MOUNTED (that is what keeps the transcript's scroll offset and the composer's draft).
  The `settings` smoke pins the mechanism in both directions.
- **The settings page is a VIEW, so it owns its own two ways in and out**: `⌘,` opens it and Esc returns to
  the conversation, exactly what the rail's 「返回对话」 does — a dirty form is NOT a reason to stay or to ask.
  The Esc listener CLAIMS the key (`preventDefault` + `stopPropagation`): the chat view is inert rather than
  unmounted, so its own Esc listeners are alive and an unclaimed Escape would also dismiss whatever panel was
  left open behind the opaque page.
- **Closing the main window HIDES it; only 退出 Tachi Desktop quits the app** (`desktop/main.go`'s
  `WindowClosing` hook). On darwin a user-initiated close would DESTROY the window and leave every handle
  pointing at nothing while the app kept running — all three ways back (the tray menu's 「打开主窗口」,
  a notification click, a Dock click) going silent together. Hooks run synchronously before the
  framework's own listeners and `WindowEvent.Cancel`
  stops the rest, which is the only lever that turns the close into a hide; the shutdown path passes through
  (`desktopApp.quitting`). The hide is load-bearing, not cosmetic: the notifier, the theme controller and the
  tray all hold the window pointer, which is only safe while nothing destroys it.
- **A session's directory is `config.SessionDir()` + ONE path element**: `sessionDirPath(id)`
  (`sessiondir.go`) is the single resolver and it REFUSES anything a webview could send that is not a single
  name (`../..`, `a/b`, `.`, empty). `oneOffDir` shares it.
- **A GUI process's working directory is `/` — never derive a workspace root from it.** Workspace roots come
  from the SESSION (`sess.WorkingDir` + `AdditionalDirs`) and go through ONE exit, `desktop/roots.go`'s
  `sessionRoots` / `sessionRootsFrom`: the turn's `wdctx`, the prompt, the `@`-file root, git diff and the
  skill store all read that, so they cannot drift apart after a directory change. A root that is `/`, `$HOME`,
  or one that contains `config.BaseDir()` is refused outright (Tachi's own state must never sit inside a
  workspace root); `NewSession` applies the same rule.
- **A desktop project OWNS its members' workspaces, and the session record becomes a snapshot.**
  `projects.json` (next to `desktop_ui.json`) holds `{名称, 主目录, 附加目录}`; a member stores only
  `project_id`, and every read resolves through the same `sessionRootsFrom` — the project's roots win while the
  project DRIVES the session, otherwise the session's own fields are the answer. It is ONE predicate
  (`projectTable.drives`) shared by the resolver, the write guards and the panel, so a session can never be
  project-owned for a read and editable for a write. It judges the PRIMARY only: a vanished ADDITIONAL root is
  a fact about one directory (kept, reported `exists=false`, skipped by the prompt and the @-file search). A
  `projects.json` that cannot be parsed reads as "no projects" but is NEVER written over. Editing a project
  writes ONE file — no fan-out into session meta — so nothing can clobber a running turn's snapshot; the skill
  store is the exception, since its scan roots are fixed when it is built, so a project edit INVALIDATES every
  live member and each re-points itself at its own next turn.
- **A binding is made in exactly two places.** `NewSession(projectID)` is the only SETTER (`""` is an ordinary
  session, otherwise the project's roots are copied in as the snapshot — a project that cannot drive a session
  yields an ordinary one, never one bound to nothing). `DeleteProject` is the only un-setter, and it DETACHES:
  members keep their workspace with the snapshot refreshed, and it is refused while any member is mid-turn
  (the running check and the meta writes share one critical section).
- **Handing a path to the system is `attach.go`**: `OpenPath` (default app; a directory opens its Finder
  window) and `RevealPath`, both `stat`-ing first and returning `"ok"` or the reason. New open/reveal actions
  reuse them, and a test replaces the single `openFile` var to read the argv.
- **Every root in the workspace panel carries its git BRANCH** (`SessionRootVO.Branch`/`Detached`;
  `shutil.GitBranch`): two checkouts of one repository are otherwise the same row. It is probed PER ROOT when
  the panel opens (never stored: a checkout moves under you), skipped for a missing root, and EMPTY for one
  that is not a work tree — "not a repository" is never dressed up as a branch name. One probe serves the
  system reminder's `Git branch` line too.
- **A pasted image is stored, then attached by reference — the same route a dropped file takes**
  (`SavePastedImage`): a screenshot is BYTES with no path, so it is written under the SESSION's own directory
  (`pasted/`, so the file dies with the conversation and never appears in the user's workspace diff) and the
  binding answers with an ABSOLUTE `@`-reference. The answer is a VO, not "ok"/reason — a paste that silently
  does nothing is the same class of bug as a message that silently goes nowhere. **The `@`-reference is
  expanded ONCE, for the turn it is sent with**, and what a later turn sees comes from the record instead:
  the image part is persisted with its bytes (`session.Message.Parts` + the session's `images/` blob — see
  session-store.md), so a reloaded history rebuilds the image rather than the placeholder. `paste-image`
  asserts the pasted BYTES at the LLM boundary; `image-reload` asserts them again after a seeded record is
  loaded from disk, which is what a restart does.
- **A send NAMES its session, and a send that does not happen SAYS SO** (`SendMessage` / `StopAndSend`). The
  id comes from the CALLER, and every refusal is a returned string — never a bare `"ok"` for a message that
  started no turn, because the frontend draws the user's bubble and a running placeholder BEFORE the call: a
  dropped message would leave the reader looking at their own text under a 正在执行 nothing could clear. Two
  traps: `sessionIsKnown` is NOT a usable guard (it reads the RUN map, so it is false for a session that
  exists on disk and has not been loaded — every first send), and the busy check has to come BEFORE the agent
  build.
- **Every desktop manager is built on ONE `FileStore` (`d.sessionStore()`).** `FileStore.ListSessions`
  memoizes the session list, and a manager built any other way (`session.NewManager(nil)`) holds a store of
  its own, whose snapshot nothing can drop: a turn's write runs on the per-run manager, so the listing manager
  would keep serving the pre-turn title (`TestEveryDesktopManagerSharesTheSessionStore`). `LoadMeta` stays
  un-memoized on purpose — it feeds the live record and the read half of a read-modify-write, where a stale
  read is a lost write.
- **A bash `ask` rule is a question for the USER, and the desktop answers it the way ACP does — never with a
  fourth `PermissionMode`**: every session's agent is built with `agent.PermissionModeExternal` plus a
  `PermissionHandler`, so the turn parks on the window and the decision comes back through
  `AnswerPermission`. `PermissionModeSkip` is the wrong posture: its ask branch is the *unattended* one
  (channel / subagent / `tachi -p`) — it refuses the command and says "add an allow rule", to the user who
  wrote that rule. `deny` behaves identically either way; only `ask` differs. Four properties: an answer is
  addressed by **session + tool call id**, because two conversations can generate the same id; the pending
  entry is dropped by the waiting goroutine itself, so Stop cannot leave an approvable orphan; a side-channel
  run has no turn on screen, so `commands.go` marks its ctx and the handler refuses instead of parking a run
  nobody could answer; and 「本会话全部允许」 is a **session** decision, not a command memory. The switch sits
  BELOW the deny checks on purpose — "stop asking me" must never become "run what I forbade".
- **The device link (AI Passport, `desktop/link/`) is a SECOND READER of that pending table, never a second
  answer path**: every "a turn is parked on the user" record lives in ONE map (`d.asks`, `asks.go`) — a bash
  ask and an AskUserQuestion alike — and the window, the device and the TUI all answer through the same two
  functions (`answerPermission` / `answerAskUser`). Four things that buys, and that a second path would lose:
  the row carries what to SHOW (the permission preview, the questions), because the waiting channel alone
  could not say what was being waited for; the AskUser half has NO channel here (its wait lives on the agent's
  own `AskUserResp`), so nothing unwinds it on a stopped turn and `endTurn` sweeps it by kind; the link
  addresses an answer by a per-connection `ref` rather than the tool-call id, and refuses a stale one exactly
  as the card path refuses a stale card; and the transport (serial / TCP today, BLE later) is chosen by the
  `badge` config block, so the protocol and the answer path are transport-blind. It is OFF by default — the
  serial port is a shared resource and `idf.py monitor` has as much claim to it as we do — and `TACHI_BADGE`
  OVERRIDES the block rather than standing in for it: a debug channel that points the app at a fake device
  must not be blocked by the file. Both routes land in ONE `startBadge`, which stops the old listener first —
  two links in one process are two Servers racing for one serial port. **The session LIST is ordered, not
  arbitrary**: the device takes the first entry as "the one on my screen", so the DISPLAYED session
  (`activeID`) is sorted first while the rest keep their stable id order (the link's de-bounce compares
  snapshots byte for byte, so the order must stay deterministic). **An answer that arrives from the
  device must also be broadcast back to the frontend** (`agent:ask_answered`, emitted by the answer
  path itself): the window's card is cleared by the FRONTEND when IT answers, so a device answer
  leaves that card standing — still clickable, and clicking it now refuses with "no such pending
  request". Any state two frontends share needs the same treatment: whoever changes it tells the
  other one, or the second one keeps rendering a fact that expired.

## Desktop MCP

- **`enableMCPServer` is the ONE enable path** (the panel's toggle and the end of an authorization both go
  through it), so an authorized server ends up exactly as connected as a toggled one: tools into the shared
  pool AND `NotifyDeferredToolsAdded` for every live agent. That reminder fires at most once per session, so a
  server connected mid-session is invisible to the model without the dirty mark — a pool write alone is the
  bug. Toggling is a GLOBAL fact (one pool, one connection for every session), so all live agents are notified
  (`applyToLiveAgents()`), and disabling runs `UnregisterMCPServer` (registry + every session's discovered
  set), not a bare pool removal. `.tachi.md` carries the general rule.
- **The panel's own state comes from the BACKEND, never from a guess**: `MCPServerVO.NeedsAuth` is set wherever
  a connect attempt came back `*mcp.OAuthRequiredError`, because a server that wants a token otherwise looks
  exactly like one that is down; the panel re-reads on every open (nothing else would correct a stale
  snapshot) and `agent:mcp_ready` is what a panel that was ALREADY open needs, since the shared pool is filled
  only after every server has answered — "已连接 + 池子里还没有工具" is a real, seconds-wide window. A tool row
  opens its own contract on demand (`GetMCPToolSchema`), never as part of the row list, and tools in the pool
  are LISTED whether or not this session has loaded them — under MCP tool search "not loaded" is the normal
  state, and a row that simply disappears reads as a missing tool. The server row counts loaded out of offered
  (`n/m 工具`) and the panel head sums the rows it is rendering, never a separate backend number. **Loaded
  rows come FIRST, and the BACKEND does the sorting** (`ListMCPServers` sorts the VO loaded-first, stable, so
  each half keeps the pool's alphabetical order): the row a reader opens the panel for is a loaded one, and a
  stable sort is what stops a toggle from reshuffling the rows around their eyes. The panel renders the order
  it is given — `TestListMCPServersOrdersLoadedToolsFirst` pins it, with the stub's two tools arranged so the
  LOADED one is the one that sorts last (where "loaded first" cannot pass by accident).
- **Tachi answers MCP's `roots` capability with the workspaces of the frontend it is serving**
  (`Manager.SetRootsFunc`; the desktop wires `desktopApp.mcpRoots` on its shared manager, so the answer is the
  UNION of its live sessions' workspaces, while ACP wires it per session). The shape itself (file:// URIs, only
  directories that exist, deduplicated, sorted) is ONE rule, `mcp.RootsForDirs`. It is not optional in
  practice: a roots-aware server CONFINes file access to what it is told, and the official chrome-devtools
  server, told nothing, allows only the OS temp dir. `client.Start` is what installs the handler for requests
  the SERVER sends (`roots/list`, sampling, elicitation) — not `transport.Start`, and not the SDK's convenience
  constructors; skipping it is silent on Tachi's side.
- **A panel toggle is written back into the mcp.json the server came from** (`MCPServerConfig.SourceFile`; the
  rule is "the file it came from", never "the active profile", or the change is lost on the next profile
  switch). The edit is SURGICAL (one value's bytes; indentation, key order and unknown fields survive), because
  mcp.json is hand-written. The **profile SWITCH stays runtime-only** — nothing writes `active_mcp_profile`
  into config.yaml, so a dropdown cannot silently make one profile the default for every future launch.
- **`AuthorizeMCPServer(name)` starts the flow and returns "ok"; the progress arrives as `agent:mcp_auth`.** It
  has to: the flow needs a browser and often a human. `CompleteMCPServerAuth(name, url)` is the paste-back
  path, and it CANCELS an in-flight flow first — both ways out spend the same single-use authorization code.
  Three facts of the flow itself: the browser is a variable (`mcp.BrowserOpener`) so a scripted run replaces it
  (`TACHI_DEMO_BROWSER_FETCH=1` — a smoke run must never open a window on the machine it runs on, while a demo
  RECORDING must); the callback address is fixed BEFORE anything is registered or exchanged, because DCR sends
  the redirect_uri and the token exchange sends it again; and the state + PKCE verifier are persisted on the
  browser path too, so pasting the URL out of the address bar works while the callback is still pending. A 401
  is one fact however it arrives (`mcp.isAuthRequired` maps both transport sentinels at BOTH the `Initialize`
  and the `ListTools` step — mapping only the later one left such a server reporting a plain connect error).
- **The fixture is `itest/mcpauth/`, and both halves of the feature run against it** (401 + `WWW-Authenticate`
  → PRM → AS metadata → DCR → `/authorize` → `/token` → a bearer-only MCP endpoint). The desktop's unit test
  drives it in-process; `mcp-auth` starts it in the runner and points the sandbox's `mcp.json` at it. `desktop/`
  reaches it through its module `replace`: one fake, not two.

## Desktop Session Tree (every path that is per-tree must come from the SESSION)

- **The process cwd is `/` and is never the answer.** A Finder/LaunchServices-launched app gets the filesystem
  root as its working directory, and the desktop hosts several sessions in different trees anyway — so
  `config.FindProjectRoot()` describes nothing useful. Anything that must resolve "which tree does this
  conversation work in" goes through `sessionRoots`/`sessionPrimaryDir`. The failure mode is always the same
  and always SILENT: a lookup under `/.tachi/<file>` finds nothing, the global scope still works, so nothing
  looks broken. `config.FindProjectRootFrom(dir)` is the session-scoped exit, and `FindProjectRootFrom("")`
  returns "" ("no project") — the correct answer for a session with no folder yet.
- **A new call site that resolves a tree is a bug of this shape unless it goes through the session**: skills
  (`sessionSkillStore`, injected as `AgentConfig.SkillStore` — the DEFAULT path is the trap, since
  `skill.NewStore(config.FindProjectRoot())` scans `/.tachi/skills`), permissions
  (`AIAgent.SetPermissionRoot`), MCP profile scope (`desktopApp.mcpProjectRoot`), and MCP server LOAD
  (`reloadMCPForActiveTree`, called from every place the DISPLAYED tree can move).
- **Skills are ON.** `DisableSkills` is the knob for the NON-interactive modes; this frontend has a human in
  front of it. The scope is the session's git root plus `config.GlobalSkillsDir()` (note NOT
  `<base>/.tachi/skills` — a fixture written for one is invisible to the other), and **additional roots are
  NOT scanned**: they are extra places to read and write, not a second configuration home. There is no `/skill`
  command and no skill UI here — the shared registry's `Modes` exclude `ModeDesktop`, so the catalog reminder
  is what tells the model what exists. The `skill-catalog` scenario pins the scan by asserting on the REQUEST.

### Tree-derived state: ONE stale flag, re-pointed in beginTurn

- **Inputs fixed at build time must be re-pointed when the session MOVES, and that happens in `beginTurn` and
  NOWHERE ELSE.** A folder change and a project edit only set `sessionRun.treeStale`: both run on the UI
  goroutine, which cannot prove the session is not mid-turn, and re-pointing rewrites state under whatever is
  reading it. `beginTurn` is the one place that can prove no turn of this session is in flight.
- **The flag is ONE, and its members move together**: `ReloadSkillsIn(sessionTree)` and
  `SetPermissionRoot(ctx, sessionTree)`, fed from the same resolved `sessionRootsFrom` value in the same block.
  A second, parallel "stale" flag is how one member ends up describing the tree the other just left; a third
  tree-derived member goes THERE, not into a new flag. `SetPermissionRoot` exists because `configure` resolves
  its project from the process cwd, so the project half of `permissions.bash` (`<tree>/.tachi/permissions.yaml`)
  was never found while global rules kept working — invisible loss.
- **A rebuilt policy inherits the session's approvals** (`Policy.InheritSessionApprovals`): 「本会话全部允许」 is
  a user answer and MUST survive a re-point, or a folder change re-asks a question they already answered. The
  RULES do not carry over — that is the point — and deny is decided before any ask, so an inherited approval
  can never resurrect a forbidden command (`TestSetPermissionRoot_*`, `TestSessionTreeMove*`).
- **A bash `ask` has TWO causes, and nothing may conflate them** (`AskNoRule`): a command either matched an
  `ask` rule, or the policy could not PARSE it (an unclosed quote, a trailing `&&`) — and in the second case no
  rule was consulted at all. Rendering the second through the first one's sentence sends the reader hunting for
  a rule they never wrote, and makes a MALFORMED command look like a TRUNCATED one (issue #40). The rule slot
  therefore carries a machine value (`permission.AskNoRule`) that each surface words for its own reader: the
  prompt says what is wrong and what fixes it, the non-interactive refusal says the same in the LLM's language,
  and the card's subtitle states only the CONSEQUENCE. Smoke: `perm-unparseable`.

## Desktop Turn History (the interrupted-turn merge)

- **A turn's history is the CONVERTED form, reminder blocks included** (`runHistory` holds `[]llm.Message` as
  `agent.ConvertSessionToLLMMessages` builds it, with the `<system-reminder>` block re-attached to the user
  message it was stored with), **so anything that MOVES a message between turns must unwrap it first.** The one
  such site is `mergeTrailingUserMessage`: merging the content verbatim would carry the PREVIOUS turn's block
  along — a stale plan id, diagnostics and branch read as current, walking forward turn after turn. It strips
  via `systemreminder.UnwrapUserMessage`, the single implementation of that inverse.
- **A trailing message that is nothing but a block is left alone — deliberately.** Two shapes look identical
  there: a reminder the loop injected mid-run (stale scaffolding) and a trailing artifact reminder, which the
  conversion appends as its own user message ON PURPOSE so a reload cannot drop it. No marker separates them,
  so dropping the message would break the second to tidy up the first.
- **That state is reached by RELOAD or by a well-timed Stop, not by an ordinary Stop** (a stop mid-stream has
  already recorded the partial assistant text). What produces a trailing user message with real text is a
  session reopened after the reply was never recorded — which is why no smoke scenario covers it, and why
  `desktop/agent_turn_test.go` builds its fixture through the REAL conversion.

## Desktop Compaction (the window between the child's creation and the turn's end)

- **A run's session KEY moves at the end of the turn, but the CONVERSATION moves at the
  compaction** — `agent.FinalizeCompact` runs in the middle of the loop, so until `endTurn`
  reaches `followCompaction` a turn is appending to the child while the bookkeeping still calls
  it the parent's. ONE resolver owns that fact: `ownerOfLocked` (reached through `getRun` /
  `runOf`) answers "which run is this session's?", and for that child it answers the run
  producing into it. **Anything addressed by a session id goes through the resolver, never
  through `d.runs[id]`** — `startTurn`, `RunCommand`/`startCommand`, `Steer`, `StopAndSend`,
  `stopTurn`, `prepareSession`, `sessionRunning`, the delete/project guards, `sessionTitle`,
  `updateSessionMetaLocked`. Reading the map by the child's id is what made a message sent there
  start a SECOND turn on the same conversation instead of meeting `refuseTurnRunning`: two turns
  appending to one `messages.jsonl` (interleaved records, a rebuilt history whose tool calls are
  unpaired), a steer answering "not running" while the loop's `steer_check` timed out, and two
  sidebar rows spinning.
- **`RunningSessions` names BOTH rows of a pending compaction** — the key the run is filed under
  (the turn's stream lands in that session's transcript, which is the row the reader is watching)
  and the child it is producing into. Both, because the sidebar renders whichever of the two its
  own session list holds and **nothing refreshes that list mid-turn**: reporting the child alone
  leaves the only row on screen unmarked while a turn is running. They are two rows of ONE
  conversation (the parent hangs under the child as 「压缩前」, collapsed), which is why a marker
  on the chain is not the same thing as two sessions running.
- **`followCompaction` refuses to take the child's key from a run that is USING it** (a live
  turn of its own — the double-write above). It logs and leaves both runs alone, rather than
  dropping the run whose turn is in flight (its state, steer channel and busy guard all live on
  that entry); an IDLE entry on that key is stale bookkeeping from a load that raced the
  compaction, and is replaced with its agent closed.
- **The alias needs no clearing and no second table**: it is read off `sessionRun.compaction`,
  which `followCompaction` consumes — a map of its own would be a copy of one fact, and the copy
  is what goes stale. The manual `/compact` path sets that same record, so it needs no extra
  wiring: it only differs in who decided to compact.
- **The window's frontend half is pinned by `compact-window`**, and it is not only about the
  spinner: the message the reader sends in the child queues, and the switch that ends the turn
  re-reads that child from disk WHILE the flush is in the air — so the transcript store has to
  keep the bubbles the composer placed (see the `holdLocal` bullet under Desktop UI State). The
  order of the two requests is the Go half's assertion: the line has to reach the model in the
  request that already carries the reply it was queued behind.

## Desktop UI State

Each bullet is a rule a change must keep. The mechanism behind one lives in the module it names.

**Ownership — where new code goes**

- **The transcript is `useTranscript.ts`** (messages per session, running flags, the loaded window + its cursor,
  the frame-batched delta queue). Every mutator takes the session it is for — `updateSession` is the one funnel
  — so "this belongs to a conversation" is enforced by the signature. App.tsx must not declare its own
  per-session state; a new per-session need is a new op in that hook.
- **A page that lands from disk must not swallow a message the frontend placed on its own**
  (`holdLocal`/`releaseLocal`, called by whichever code puts a bubble in optimistically — the composer's send,
  `/command` and 立即发送 paths). A reload REPLACES the window (a rewind depends on that), but the page may have
  been READ before those bubbles existed: an auto-compaction switches the conversation into its child while the
  queue it is about to flush is still in the air, so the switch's own re-read of the child is a snapshot without
  the message the reader just sent — replacing the window with it left a transcript that answered a question it
  never showed (the reply arrives over the live stream, so nothing else is visibly missing). The held messages
  are re-attached by ID and the hold is consumed by that first landing; `releaseLocal` (on the session's
  `agent:idle`) is the backstop for a hold no page ever lands on. `compact-window` pins it.
- **The composer is `composer.tsx`** (`useComposer` + the `Composer` element): the input box, the @-picker, the
  "/" palette, the queue of messages typed while a turn runs, and the parked-question form. All four share one
  decision — `route`: send now, queue for the next steer point, or dispatch as a slash command. The microphone
  button lives here too, but its STATE does not: it draws `voice:state` (the backend owns the microphone and the
  watchdog, see Desktop Voice Input), and the click only asks for a transition.
- **Backend events are subscribed in `agentEvents.ts`.** Each payload names its session; compare it against
  `currentId` only where the UI genuinely means "on screen", never to address storage.
- **A parked permission is composer state too, matched by tool CALL ID** (keyed by session, NOT cleared on
  session switch — the agent is still parked — moved on auto-compaction, cleared on `agent:idle`). Matching by
  tool NAME is the trap: a model emits a whole batch of calls before any of them runs.

**Rules that hold across every surface**

- **Match by ID, never by name** — a tool result is matched to its card by the tool call's id (`parallel-tools`
  pins it; a name match lands every arg on the NEWEST unfinished card).
- **Never run a side effect inside a state updater**: React may invoke an updater more than once, so a fetch
  there fires twice and an `if (prev[k]) return prev` guard cannot help. Keep in-flight bookkeeping in a ref.
- **A state flag must be ended by the fact that ends it**: `ReviewChanges` only STARTS the fork, so clearing a
  "pending" flag on its return wipes the middle state before it can be seen; the run's own end event is the
  fact.
- **Anything cached per run or per session must be KEYED by it** (a record's name is a timestamp WITHIN its
  session). The key says what is IN HAND, never "we asked once" — store only content, and keep the reason a pane
  is empty in a second, equally keyed state, because an empty pane cannot tell "missing" from "unreadable".
- **A system reminder is a HISTORY artifact, not a live one** (`Message.reminder` is set by `buildTurns`, from
  the store's own rows): assert injected context at the LLM boundary or through a rebuild, not by waiting for
  the affordance on a live message.
- **A rebuilt transcript shows the user's OWN words, not what was SENT** (`displayContent` holds what was
  typed, while `Content` holds the `@`-file expansion) — and **`Content` stays authoritative for the model**,
  because the next request is rebuilt from it: writing the raw text there would send a prefix the model never
  saw and retroactively rewrite what the history says it was told.
- **A surface that shows RAW text owns its line breaks**: the user bubble renders the message as plain text
  (`UserBubble`), so the newlines the composer's Shift+Enter and a pasted multi-line block produce get a line
  box from `.msg-user .msg-content`'s own `white-space` and from nothing else — under the default `normal`
  every one of them collapses into a space, and the message reads as one paragraph (`bubble-lines`).

**A turn on screen**

- **A turn is folded by the CONVERSATION, not by the part renderer** (`turnView()`, pure, shared by the live
  view and a rebuilt transcript): one strip stands in for its thinking, tool cards and intermediate messages,
  the turn's LAST prose, plus the parts that must never be hidden — a FAILED call, the call a permission card
  is parked on, the call an AskUserQuestion form waits on, notices, and a DELIVERED file (its own card at the
  turn's tail; behind the fold it was ABSENT from the DOM, so 「把 X 发给我」 hid its own answer). Intermediate
  prose is folded whether or not the turn failed. **A steer splits the turn, and therefore the fold.** "No tool
  cards on screen" is never evidence of "no tool calls" — a scenario asserting a successful card must expand
  the strip first.
- **A row that alternates between two states must not change GEOMETRY — only its content may** (the transcript
  is bottom-pinned, so a height difference jumps the visible content), and the live line's own text ROLLS for
  the same reason. Paint state with `box-shadow: inset` rather than a border; a status indicator keeps its
  motion under Reduce motion, decorative things lose it.

**Scrolling and widths**

- **Following the bottom must survive async height changes** (a diagram finishing, an image loading, a card
  expanding): watch the CONTENT's box (`ResizeObserver` on a `.chat-content` wrapper — the scrollport's own box
  never changes when its content grows), and **only the READER ends following, never growth**: distance to the
  bottom is not a usable test, because content arriving below the viewport looks exactly like a reader who
  left. Record where the pin landed and stop only when the reader moves above it.
- **Keep the CHOSEN number and the DERIVED one apart.** A width the reader picked is persisted (and Go honours
  it before the window exists); what a pane can SHOW is that number clamped to the room left by every other
  fixed column, re-measured on resize and read from the DOM rather than assumed. The conversation's share is a
  floor PLUS a share (`CHAT_MIN_WIDTH`, `CHAT_MIN_SHARE`) — a bare floor pins the conversation AT the floor and
  hands every extra pixel to the panel — and the reading column is one token (`--reading-width`).

**Sessions and the sidebar**

- **Session-scoped UI state must be keyed by session, or reset on switch**: one React tree renders every
  session, so anything derived from the ACTIVE session silently leaks into the next one. Anything fetched per
  session must be applied unconditionally — `if (u)` keeps stale values when the payload is empty.
- **The agent status is per SESSION, and its event names the session** (`agent:state` carries `sessionId`),
  because the desktop runs every session's turn in its own goroutine and switching sessions cancels nothing.
  The only global surface is the menu bar, which reflects the DISPLAYED one. Emitting only while a session was
  displayed produces four faces at once: the stop control follows the reader into an idle session, a background
  turn's END is never reported, a stale busy state makes the composer queue instead of send, and a chip waits
  on a neighbour's turn. A mount snapshot may only SEED a session no event has named yet.
- **A session's "running" answer and the event that says its turn is over must not disagree**
  (`TestAFinishedTurnIsNotReportedAsRunning`): answer from the per-session STATE, never from
  `sessionRun.running`, because a turn publishes its own `turn_complete` and only THEN exits to `endTurn` — the
  state is set before the event, so it can never contradict it. WHICH session the answer names is a
  second rule, and the compaction window is the one case where they differ (see Desktop Compaction).
- **A turn that fails MID-STREAM keeps the history it already produced** (the partial messages are adopted on
  EVERY error exit that carries them): it has already EXECUTED tool calls by then, and dropping them is how the
  next request comes to be missing work the model did (issue #38). What arrives is complete pairs, so the
  adopted history is always a legal request — and the STATE still says 出错, because keeping the history is not
  calling the turn a success. The guard also stays held for a moment after the error event (the checkpoint is
  being finalized), so a send landing in that window is refused by name.
- **Copying goes through `copyText` (`lib.ts`), never a bare `navigator.clipboard`**: the page is not a secure
  context, so the global is UNDEFINED there and an optional-chain call reads as a no-op with nothing failing
  anywhere. The runtime's own clipboard comes first, so a copy either works or is visible as a failure.
- **A session list is a list of CONVERSATIONS, not of session dirs**: a compaction chain shares one title, so
  rendering `ListSessions` raw shows the pre-compaction session as a second, identical-looking row. The shape
  is derived from `compactedParentId`, and **a link whose parent is NOT in the list still gets a line** (a note,
  not a control, naming it from the title the child recorded) — without it, a parent deleted by hand or swept
  by session cleanup silently erased the fact.
- **The sidebar is GROUPED, and the grouping is a pure join** (`sessionGroups`, before `sessionRows`, so a
  chain folds inside its own group): one header per project (name, count, a `＋` that creates a session INSIDE
  it, a right-click menu) plus one final 无项目 bucket, which is also where a session whose `projectId` names a
  project not in the list belongs — the bucket is what the backend's resolver does with it. **EVERY project
  gets a group, including one with no sessions yet** (the header carries the ＋ that makes the first one). A
  group's rows hang off a visible rail, because indent alone does not say which group a row belongs to.
- **A project group is FOLDED by default, and the ONE exception is the project holding the session the app
  booted into.** The state records the EXPANDED groups, so an empty default needs no per-project seeding and a
  project that appears later cannot leak open first; `revealProject` is the only thing that OPENS a group
  (boot, ⌘P, ⌘K, a project's `＋`) and it never closes one. The fold is DOM-level, so anything reading inside a
  group — a driver included — must expand it first.
- **Deleting a session hands the reader to the NEXT one — and only when the deleted session was the one on
  screen**: the row below inside its own group, else the row above when it was that group's last one. Deleting
  any OTHER row moves nothing; it must not yank the reader out of the conversation they are reading. The
  successor is computed BEFORE the reload, which is the only moment "the row below it" exists.
- **置顶 orders ONE group, and the mark explains the order**: a pinned session goes to the head of its OWN
  group, never above another project. Sorting happens before folding, or a chain's ancestor is torn away from
  the child it hangs under. The flag is the session RECORD's, which is what makes the order survive a restart,
  exempts the session from cleanup, and lets compaction TRANSFER it to the child
  (`TestFinalizeCompact_MovesThePin`).
- **`＋ 新建会话` belongs to the 无项目 group** (it makes a project-less session, so it lives where it adds
  rows), and **⌘N follows the session on screen** — ⌘N means "another one of these", so it passes the current
  session's `projectId` while that project is still in the list. **The menu bar's 「新会话」 is that same call
  reached from the shell**: `desktop/tray.go` emits `ui:new_session` and the FRONTEND makes the session, so all
  three entry points run `newChatLikeCurrent` and cannot drift apart. It also leaves the settings page on its
  way in, which ⌘N deliberately does not — that gate is for a stray keystroke, while an explicit click that
  left the opaque page up would read as nothing having happened.
- **`⌘P` and `⌘K` are one palette in two uses** (projects / conversations), each closing the other, each with
  the same input, list navigation, Escape and caret hand-back to the composer. Four contracts they share:
  filtering is on the COMMITTED query, never the field's live value (**a Chinese IME composition re-renders on
  every candidate key, and the keys that END a composition are the IME's own** — the box holds the raw value,
  the list filters on the committed one, and the keydown handler returns early while composing); the palette
  reads the list the sidebar already renders and issues NO fetch (a shortcut that re-reads re-orders the list
  under the reader's cursor); a pick that the app refuses RETURNS the reason and only "" closes it; and the
  caret goes back to the composer, because ⌘P is the only entry point that closes a dialog ON THE WAY to a
  session. What ⌘K decides for itself: the rows are conversations in the list's own order (pins are ignored —
  a pin orders a row inside its GROUP), the query matches the TITLE only, and which project a row belongs to is
  a TAG rather than a second line of text.
- **A project write re-reads the reader** (`agent:workspace_changed`): the frontend holds workspaces and groups
  BY VALUE, so a rename moves nothing on screen until the user switches sessions.
- **Deleting a session with a turn in flight is REFUSED, by the backend** (the turn owns a goroutine whose
  session writes would go nowhere and whose run-map entry is keyed by nothing but that id): the running flag is
  read and the run removed in ONE critical section, and the frontend renders the reason where it raised the
  delete while the menu entry is `disabled` for a running session.
- **The sidebar has a type floor, and there is ONE caret size** (a disclosure caret drawn as a text glyph
  depends on the font; the shared `<Caret>` is sized by one token).

**A turn's changes, diffs and reviews**

- **A turn's changes are read from its CHECKPOINT trees, everywhere they are shown**, so a file a SHELL
  command wrote counts and the answer does not move when the working tree does; the tool-call fragments are
  only the FALLBACK, and the surface must say which source answered. Four traps: the end snapshot is taken
  BEFORE `turn_complete` and on a context DETACHED from the turn's cancellation; a refused end arrives as a
  note and ZEROES (fall back to the tool-call numbers); a refused end releases only its OWN end ref, because
  the start ref is the next turn's parent; and a turn whose changes came from a shell command has NO
  tool-declared paths, so anything that skips work on an empty path list hides exactly that case.
- **`评审所有改动` is the ONE review entry whose subject is not a turn** (every uncommitted file, read against
  HEAD), and it rides the LATEST turn's footer only. Four things travel with the scope and every one is a lie
  if it lapses: the prompt's provenance sentence, the record, the diff itself, and the chip's own state (one
  footer now carries two entries that would otherwise report each other's runs).
- **Every path in a diff carries the ROOT it came from.** Paths are RELATIVE to one root, so a file without
  its root resolves against the primary one — and a same-named file there is what 预览/打开 would show,
  silently. A group's identity for React keys and fold state is `(root, path)`; `GetTurnDiff` diffs EVERY root
  (a path in an additional root is a DIFFERENT repository, not "outside the repository"); the per-root results
  must never be merged into one text or path set; and **every row is labelled as soon as ONE of them has to
  be**, or the bare one reads as belonging to no tree at all.
- **A review's PANE reads the same diff the reviewer read, by TURN**: fetching by the recorded PATH list alone
  is the working tree against HEAD, which is an empty pane for a review whose changes have since been
  committed — and 「重新评审」 must carry the turn it re-runs.
- **A rewind is only offered for the conversation the reader is still IN** — a compacted-away session is
  refused, and the refusal is raised by the PREVIEW as well as the action (refusing only on confirm would
  build a card offering a rewind that cannot happen).
- **The transcript's turn stamps cover every record of a turn**, not only its opening one: a page is the newest
  N records, so a long turn's opening record sits on an EARLIER page, and resolving the boundary exactly is
  what keeps the longest turns stamped.
- **An entry's data source must belong to the surface the entry opens** (fetched on open, dropped on close, so
  it can never become a stale anchor), a fold default belongs to the surface that shows the diff, and a helper
  whose only caller is gone goes with it.
- **A panel opened BY an event cannot assume what that event announces is on disk yet**: the run's `start` is
  emitted BEFORE the recorder is opened, so a panel's own list read comes back empty and the whole run hides
  behind an empty state. The live follow therefore re-reads the LIST too, and compares by name so the reader's
  own pick is left alone.

**Overlays, the panel and find**

- **A pane has ONE scroller.** The pinned things inside it (the overlay's file index, its head) pin within that
  one box and are measured from the DOM (`ResizeObserver`) rather than hard-coded, because the head wraps; a
  nested `overflow: auto` would leave find and the finding walk searching the wrong box.
- **The row the reader is ON is marked, and the mark follows the SCROLL, not just the click** — the rule is
  stated in the same terms as the jump (both ends need a fallback, or the highlight vanishes at the top and the
  bottom), and the mark is a bar AND a wash AND `aria-current`, never colour alone.
- **A drag writes the element directly and commits once** (never React state per pointermove, which lays the
  whole pane out again), and the reader's width is clamped to the room left, never to the pane's own box. **A
  layout the reader CHOSE is remembered (`localStorage`), a width the pane DERIVES is not** — and the two
  stores split by what has to read them: `desktop_ui.json` is the DURABLE side Go writes and `GetUIState`
  answers (a width, the panel's open flag, the reading size — hand-editable, and it survives a cleared webview
  store), while a preference only the page reads can be `localStorage` alone (the diff layout). The theme is in
  both, because the boot frame needs it before the first paint and Go needs it before the window exists.
- **The lightbox's fit() measures the WINDOW and only ever SHRINKS** (the stage hugs its content, and "as large
  as it fits" in both directions opens the same gesture at wildly different sizes).
- **⌘F searches a SURFACE, not the page** — each viewer searches its OWN subtree, the key owner is installed
  once at module load, the bar is mounted only while open, and `findable` is opt-in per viewer. **The
  transcript's search uses an explicit, POSITIVE scope** (the reader's own words + the model's prose): most of
  a session's text is tool arguments and results, and a search over the whole subtree is a search over file
  dumps (issue #35). The chat's host is the LOWEST in the stack, so a preview or the panel keeps ⌘F while it is
  up — and the find bar is off while the settings page is up, because that view COVERS the chat.
- **The one-off panel's × promises Esc, and who owns the FIRST press is decided by focus**: the global handler
  must not claim every Escape unconditionally, and the panel refuses only an ALREADY-CLAIMED key.

**Text and dialogs**

- **A dialog that renders a PANEL's rows inherits that panel's type scale — and the shared box's max-width**,
  so a modal built from a compact popover's rows reads as a shrunken dialog. Raise the shared sizes SCOPED to
  the dialog and declare its own width.
- **Path tries are byte-keyed: iterate strings by BYTE, not `for i := range s`** (`for i := range s` walks rune
  boundaries, so `s[i]` yields only the first byte of each multi-byte character) — otherwise Chinese file names
  come back mangled from the @-file index. `pkg/container/pathtrie.go`.
- **Do not reintroduce an "activity row" above the composer** (it appeared and vanished once per step and
  shoved the message area up and down), and **do not compensate a layout change from a
  `requestAnimationFrame`** — a covered window stops the page's frames. Compensate in a LAYOUT effect.
- **The window's columns are the sidebar's, not the bar's**: the sidebar runs the FULL height with the traffic
  lights painted onto its empty top strip (`--titlebar-h`, the same 44px Wails' `InvisibleTitleBarHeight` makes a
  native drag handle — keep the two equal), and the toolbar belongs to the content column beside it. So
  `.titlebar` is a SIBLING of the sidebar, not a window-wide row, and the traffic-light clearance moves with the
  fold: 78px on the sidebar's strip while it is open, on `.titlebar` itself under `.app-body.sidebar-folded`, on
  the same curve so the first control never slides under a light. The settings page covers the WHOLE window for
  the same reason (it would otherwise leave a strip of the conversation's own chrome above it) and pays for its
  rail's top clearance with the rail's own padding.
- **The toolbar holds its controls in one cluster** (`titlebar-right`, held at the far edge) plus the session's
  TITLE (from `sessions`, the sidebar's own 未命名会话 fallback — empty while the session has not reached the
  list, so a launch does not flash a name it is about to correct), while the agent state is NOT echoed there
  (the sidebar row and the reply already say 运行中). The displayed session id is readable as
  `data-session-id` on the app root — the copy gesture lives in the session row's menu, and the smoke drivers
  read that attribute to tell "switched session" from "still the same one" (a title cannot: 未命名会话 repeats).
- **The context ring is ANCHORED on the last call's real prompt size, not on the character estimate** (the
  estimate is character-class based and under-counts mixed CJK + JSON), the breakdown is scaled to that total
  because the parts must add up to the number above them, and the correction is a RATIO of the estimate's
  movement — an additive delta with a floor would leave the ring on the pre-compaction number after a
  compaction. The auto-compact THRESHOLD reads the same anchored value, so the trigger and the meter agree.
- **The ring follows each API CALL, not each turn, and it measures the PROMPT of that call** — a reply enters
  it only on the NEXT turn. A ring refreshed per turn sits at what the turn started with while the popover
  (fetched on open) already shows the current value, and a scenario about "the context got smaller" must fill
  the window first or it asserts nothing. "The turn ended" and "the call ended" are different refresh points,
  and two surfaces describing one fact must read it from ONE rule.

## Desktop Themes (the dark palette)

- **The dark theme is Ayu Mirage-derived and its contrast is low ON PURPOSE — do not "fix" it**: the family's
  blue-grey planes lifted one step so the app reads as graphite rather than black, with the warm editor fg as
  the CONTENT ink and the cool UI fg family as the CHROME ink.
- **Every "go" button is ONE flat recipe — do not put the gradient back** (one solid accent fill, tuned in one
  place), and a fill that carries white text always takes the `-strong` variant, because the plain accent and
  `--red` are tuned as INK and put a white glyph below 4.5:1. Find hits are amber on purpose: a hit has to be
  findable inside a diff (green/red already mean added/removed) and inside prose (the accent means a link).
- **The reading surfaces take their sizes from ONE place** (`base.css`'s `--fs-prose` / `--fs-bubble` /
  `--fs-finding`, and `--reading-width` for the column — the same measure a previewed document gets): "the text
  is too small" is an edit there, not a hunt through four files. The scale is deliberately NOT a blanket
  multiplier — labels, badges and metadata are chrome and keep their own sizes. What the READER adds on top is
  a multiplier on the two message sizes and nothing else (`--fs-scale`, ⌘= / ⌘- / ⌘0 in `fontsize.ts`), and a
  message's own markdown follows in em (`widgets.css`), so a bigger body keeps its hierarchy instead of growing
  past a heading fixed in px.
- **What bounds the palette is `--text-faint`'s contrast floor, not the main ink's** (it lands on the LIGHTEST
  plane, so it pins both the ink and the top of the ladder): any "make it lighter" edit trades against that,
  and a WCAG ratio check across every plane is how to verify it, not the eye. The pre-paint window colour
  (`windowBgLight`/`windowBgDark`) is the same decision in a second place — move it with `--bg`.
- **One definition per mode, in `frontend/public/base.css`**: the dark block, resolved by the boot script +
  `src/theme.ts`. No frontend rule reads `prefers-color-scheme`, and no other file carries dark colours — the
  highlighter's tokens map onto the semantic ones, so a palette edit covers code blocks for free.
- **To SEE a palette change, render the tokens instead of capturing the app window** (window capture is dead on
  this OS): extract both blocks from base.css (next to `git show HEAD:desktop/frontend/public/base.css` for the
  before) into a small HTML page whose swatches read `var(--token)`, one scope per variant, and screenshot
  that.
