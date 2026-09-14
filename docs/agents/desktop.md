# Tachi — Desktop Handbook (split out of .tachi.md)

Read this **before changing anything under `desktop/`**: the smoke suite (drivers, fixtures, the
bundle/pkill rules, and the traps that make a green run mean nothing), macOS build & signing, and the
frontend's UI-state conventions (which module owns what).

Why it lives here: `.tachi.md` is injected WHOLE into the first message of every session, so it only
keeps the conventions that apply anywhere in the repo and points here for the rest. **New desktop
lessons belong in THIS file, not back in `.tachi.md`** — and the rules that ride with that file
(freshness in the same turn, convention-not-history, split + index when a subject grows) apply to every
split of it, this one included.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Desktop Smoke Verification** | you changed anything a driver can see (UI, events, a binding, an end-to-end flow) |
| **Desktop Build & Signing** | you build/package the app, or debug notifications/TCC |
| **Desktop Backend (bindings & paths)** | you touch `AgentService` — a method, a binding, a session path, an open/reveal action |
| **Desktop Skills** | you touch skill discovery, or anything that changes which tree a session works in |
| **Desktop Turn History** | you touch how a turn's history is assembled, merged or continued (the interrupted-turn path) |
| **Desktop UI State** | you change the frontend: which module owns what, and the bug each convention prevents |
| **Desktop Themes** | you touch colours |

## Desktop Smoke Verification (demo + mockllm)

Desktop-only paths (Wails wiring, rendered UI, end-to-end flows) need the real window: unit
tests and itest cannot see whether a binding's result reaches a card. **Use the in-repo
suite** for anything touching desktop state, events or UI, and add a scenario when a fix
deserves a regression test. It owns the sandbox, the isolated HOME, the scripted model and the
report; the verdict is an exit code.

```sh
make desktop-smoke                                  # every scenario
make desktop-smoke ARGS="-run send-now -v"          # one of them, every assertion printed
make desktop-smoke ARGS="-keep"                     # keep the sandbox after a pass (artifacts)
```

The suite is `itest/desktop/`: `main.go` (runner: sandbox, launch, verdict, report), `scenarios.go`
(one conversation per scenario — the mock's steps plus the Go-side `after` assertions), `sandbox.go`
(bundle prep, isolated HOME, launch/kill), `sink.go` (the loopback endpoint drivers report to),
`drivers/` (harness.js + one script per scenario). Per run it builds a throwaway sandbox holding a
private copy of the app (executable renamed so no `pkill` can reach the user's own Tachi), an isolated
HOME with a `config.yaml` pointing at the scripted model, the work dir, `driver.js`, `dom.html` (the
page as the verdict found it) and `mock-requests.txt` (what the model was actually asked). **`desktop/bin/Tachi.app`
must exist** (`make -C desktop build`); the runner refreshes the executable inside it from `bin/Tachi` on
every run, so the app under test is always the current tree.

A driver asserts what the UI **shows** (transcript shape, chips, buttons); the scenario's Go half asserts
what actually **happened** (the prompt the model received, the files written) — a regression usually shows
up in exactly one of the two, so write both. **Adding a scenario** = `drivers/<name>.js` written against
the harness (`smoke.waitFor` — never a fixed sleep — `smoke.check(label, ok, detail)`, `smoke.finish()`,
plus `q`/`qa`/`text`/`allText`/`type`/`pick`/`click`/`key`/`sleep`) and an entry in `scenarios.go` (the
mock's `steps`, the work-dir `files`, an `after` func for the Go-side checks). Keep both halves small:
one behaviour per scenario, and assert what the user would notice. A scenario that needs settings the
shared sandbox config does not carry sets `config` — a YAML block appended to the generated
`config.yaml`, which is how a parked-permission scenario gets its `permissions.bash.ask` rule.

### Manual recipe — only for exploring something the suite does not cover

```bash
# one-time per sandbox: a private bundle whose executable name is unique, so no pkill can
# hit the user's own Tachi (see the bundle rules below)
cp -R desktop/bin/Tachi.app /tmp/tachi-smoke/TachiSmoke.app
cp desktop/bin/Tachi /tmp/tachi-smoke/TachiSmoke.app/Contents/MacOS/TachiSmoke
plutil -replace CFBundleExecutable -string TachiSmoke /tmp/tachi-smoke/TachiSmoke.app/Contents/Info.plist
codesign --force --deep --sign - /tmp/tachi-smoke/TachiSmoke.app

# every run — launch → drive → capture → kill in ONE shell invocation
cd desktop && make frontend && go build -o bin/Tachi .
pkill -f TachiSmoke; sleep 2; test "$(pgrep -f TachiSmoke | wc -l)" = 0
HOME=/tmp/tachi-smoke /tmp/mock-bin &        # writes config.yaml + dumps requests
MOCK=$!; sleep 2
open /tmp/tachi-smoke/TachiSmoke.app \
  --env HOME=/tmp/tachi-smoke --env TACHI_DEMO=1 --env TACHI_DEMO_JS=/tmp/drive.js
sleep 24                                     # mount (~2s) + the driver's own timing
R=$(osascript -e 'tell application "System Events" to tell process "TachiSmoke" to get {position, size} of window 1')
I=$(echo "$R" | tr -dc '0-9, '); screencapture -x -R "$I" /tmp/tachi-smoke/shot.png
pkill -f TachiSmoke; kill $MOCK
```

- `--env TACHI_DESKTOP_APPEARANCE=dark|light` forces a theme (dev aid); run twice for both palettes
- The scripted model is a driver program, not a config: `mockllm.NewServer(...)`,
  `Script(Step{Reply: Stream(Text("…"), Finish("stop"), UsageWithCache(…), Done())})`, print
  `BaseURL()`, block. It writes the isolated `config.yaml` (the port is random) and dumps
  `mock.Requests()` to a file — **assert at the LLM boundary**, not only on the rendered bubble
- Fixtures: `$HOME/.tachi/session/<id>/{meta.json,messages.jsonl}` (shapes as `session.Store`),
  `additional_dirs`, `desktop_ui.json`; bump `updated_at` so the app auto-loads it

### Driver JS (`TACHI_DEMO_JS`, injected by `desktop/demo.go` ~2s after mount)

```js
// React inputs ignore `el.value = …`: native setter + input event
const type = (el, t) => {
  Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(el, t)
  el.dispatchEvent(new Event('input', { bubbles: true }))
}
// selects need the same treatment, plus a change event; window-capture key handlers get the
// event dispatched on the focused element
// assertions in a banner (#smoke-result), not the console: one screenshot carries UI + verdict
```

### Rules and traps

- **Only one instance may run**: `open` on a live instance merely activates it and passes NO
  environment (so the driver never runs and the previous scenario's app answers), and
  `pkill -f "Tachi.app/Contents/MacOS/Tachi"` matches the user's running app. The unique
  executable name is what makes `pkill -f TachiSmoke` safe — check with `pgrep -fl Tachi`. But that
  pattern also matches the SANDBOX's own build (`codesign --force --deep --sign - …/TachiSmoke.app`
  carries it on the command line), so a stray `pkill -f TachiSmoke` while a run is preparing kills
  the ad-hoc signing and the scenario reports `sandbox — codesign …: signal: terminated` before its
  app ever launches — the same pattern the runner's own "kill it first" message recommends. Never
  kill a run in flight for a second reason: every sandbox app shares the bundle id
  `com.monsterxx03.tachi.smoke`, so the instance it leaves behind is exactly what the next run's
  `open` will activate (measured: a killed run's app from an old sandbox answered the next run's
  `send-now`, which then hung with the app dying of a stale SIGTERM mid-scenario)
- **Run the suite SERIALLY — one invocation at a time.** The drivers are timing-sensitive in a way that
  turns load into false failures: each has a 1m30s budget, assertions wait 3s for a synthetic drag or a
  transient state, and a starved webview misses both. Measured: two overlapping `desktop-smoke` runs (or a
  full suite racing a single-scenario run) produced 6/17 failing with "driver 在预算内完成 — 等了 1m30s"
  plus phantom drag failures in `composer-height` / `oneoff-panel`, while the very same tree passed 17/17 in
  1m14s when run alone. Before believing a failure, check `pgrep -fl "itest/desktop|TachiSmoke"` — and
  re-run the scenario on its own. The synthetic DRAGS are the flakiest of all: a second gesture is
  sometimes swallowed (`composer-height`, `oneoff-panel`), and both scenarios pass on their own on the
  same tree — so a lone drag assertion failing in a full run is the harness until proven otherwise.
- A background process started by a finished tool call gets reaped — keep the mock, app,
  capture and kill in one invocation
- `open -a <path>` matches by BUNDLE and `pkill` by executable path; a capture of the wrong
  window looks plausible, so confirm the shot is yours (titlebar / session id)
- **A capture is not part of the suite**: macOS 15+ closed the window-capture APIs
  (`CGWindowListCreateImage` obsoleted, `-R` fails on macOS 26, `-l` blank for a WebKit
  window) and the window must be on the visible Space anyway — a "screenshot" that silently
  returns the desktop is worse than none. The suite keeps `dom.html` + the assertion lines;
  take a picture by hand when you want one
- **A driver must not click an action that hands the screen to another app**: 打开会话目录 launches the
  real Finder, which takes the foreground and suspends the webview mid-run (the trap above) — so the sidebar
  scenario asserts the menu item is there (and that its own mouseleave rule closes the menu) and leaves the
  click alone. Which path it would hand over is pinned where the argument is known: the Go test
  `TestOpenSessionDirOpensTheSessionDirectory` stubs `openFile` (attach.go) and reads the argv. Any "open in …"
  action has this shape — assert the control on screen, pin the argument off screen
- **A probe only measures what it reads at the right moment, and only ever proves one direction**:
  content that grows AFTER a pin (a mermaid diagram finishing its async render) is pinned again
  from a `ResizeObserver`, which the browser runs after layout and before paint — so a read taken
  BEFORE that pin reports a gap nobody ever saw. Reading `scrollHeight` FORCES layout, so a
  `requestAnimationFrame` callback is exactly such a read (measured in `switch-scroll`: a lone
  298px frame with 0 on both sides, i.e. a gap the pin closed before anything was drawn). Read
  from the driver's OWN `ResizeObserver` instead — observers are called in registration order, so
  one registered after the app's runs after its pin and still before the paint — and keep a timer
  series as the fallback for when no delivery arrives. Then verify the judge BOTH ways: 0 with the
  fix in place, and the drift with the fix disabled (that negative control is what turns a number
  into evidence)
- **Run one smoke at a time**: a second window in front stops the browser's rendering steps, which
  starves a resize-observer probe exactly as it starves a rAF one
- **An assertion belongs to the scenario that produces the fact** (and to the line that produces
  it — see the missing `reviewedMsg` assignment). The panel-width check sat in `oneoff-footer`'s
  `after` while the driver that drags the handle is `oneoff-panel`'s: it read
  `oneOffPanelWidth=0` forever, and the fix was moving the check, not the code. Its sibling:
  **a control used as a TRIGGER for another behaviour pins neither.** `oneoff-footer` pressed the
  footer's 「完整 diff」 to get INTO the 意见 pane, so when that button was re-pointed at the turn's
  own diff overlay the assertion silently stopped being about the pane it named — a button and a
  page switch were riding on one click. The chip's assertions now sit BEFORE any review exists
  (the moment that fact is produced: no run, overlay opens, this turn's files, real line numbers,
  files expanded, Esc closes) and the 意见 pane is entered by clicking its own tab.
- **A transient state belongs to the trail; a steady state belongs to a direct read.** The 10ms
  sampler that catches the middle of a state machine (`评审中…`) can be starved while the webview
  is busy, so its last tick may never see the state that persisted — one `oneoff-footer` run
  reported 「已评审 · 查看」 as disabled because the trail had no 已评审 entry at all, and the chip
  the wait had just returned was fine. Read the END state off the element the wait returned.
- **The console is part of the report**: the driver's `console.error` lines used to be a second
  POST to `/console` sent AFTER `/result` — which is what unblocks the runner — so they raced it
  and lost, exactly when a strange failure needed them. They now ride in the result body
  (`Result.Console`), and the runner prints them for passing runs too.
- **A proxy signal becomes true before the fact it stands for — a driver must wait for the fact its action
  consumes** (`oneoff-footer`): the turn's 评审本轮改动 chip exists after its FIRST changed file, not after its
  last, so a click on 「完整 diff」 that waits only for the chip captures a one-file `paths` set and the overlay
  shows the wrong files (measured: 2/78 in a full run). The driver waits for the turn's own closing line, and
  the scenario parks the second write for two seconds (`slowWritePause`) so the window is wide enough to be
  TESTED rather than raced — with that wait removed the same run fails deterministically (8/34), with it it
  passes 78/78. Two rules, both cheap: wait for the fact the action reads, not for a neighbour that merely
  correlates with it, and widen a suspect window before you trust a fix, because a race you cannot widen is one
  you cannot verify a fix against.
- **A stale element swallows a gesture**: re-query the handle for EVERY synthetic drag. A driver
  that captured `.composer-resizer` once had its second drag land on a detached node, and the
  "ceiling" assertion then passed against the previous height (200px) without dragging at all.
- **A suspended page takes the whole verdict with it**: when the smoke window loses the foreground,
  WebKit suspends the content process (`log show --last 10m --predicate 'process CONTAINS
  "TachiSmoke"'` spells it out: `WebProcess::prepareToSuspend` / `ProcessThrottler … foregroundActivities=0`).
  A suspended page runs no `requestAnimationFrame` **and no `setTimeout`** — so the harness's own
  watchdog (a timer) never fires either, and the run posts NOTHING: the report shows only the last
  line that got out, `等了 90s 没收到结果`, with no console lines to explain it. The runner's
  `-timeout` is the only safety net, so a driver must never wait on a *frame* — pace with a
  macrotask yield at most, and keep the run short. Anything that needs a frame (a rAF-paced probe,
  a real drag measurement) belongs in a hand-run on an idle machine, not in the suite.
- **With Reduce motion ON, EVERY property change is a real transition** (`transition-property`
  defaults to `all`, and base.css's blanket `@media (prefers-reduced-motion: reduce)` sets
  `transition-duration: 0.01ms !important` — this machine has it on: `defaults read
  com.apple.universalaccess reduceMotion` → 1). 0.01ms is not zero: it creates a `CSSTransition`,
  whose USED value only advances on the next frame. So a driver that writes a style and reads
  `getBoundingClientRect()`/`getComputedStyle()` **in the same task gets the OLD value, no matter
  how correct the code is** (measured on the panel's width: inline `380px`, computed `420px`,
  `document.getAnimations()` naming `CSSTransition:width` on that very element, 380 one frame
  later). Wait for the value; never assert it in the write's own task.
- **A control run is worth nothing until the build is PROVEN to carry it** (`make frontend && go build
  -o bin/Tachi .`): `npm run build` is `tsc && vite build`, so a control edit that does not type-check
  fails tsc, the `&&` skips the Go build, and the run measures the PREVIOUS binary — the panel's Esc
  control came back fully green that way, and the tell was on disk, not on screen: `bin/Tachi` older
  than the edited source, the control's marker absent from `dist/assets/*.js`. (The edit that got
  rejected: `window.addEventListener('keydown-control-off', onKey)` — TS2769, the `type: string`
  overload wants an `EventListener`, and `(e: KeyboardEvent) => void` is not one. Changing the KEY
  inside the existing listener instead type-checks, which is why the second control did go red.)
  So: check the freshness (`[ bin/Tachi -nt <the edited source> ]`), disable nothing via a build that
  can fail silently, and mind the suspension trap above — the foreground window is what matters, so a
  build glued to a run in one tool call can push the run past the moment the page is suspended.

## Desktop Build & Signing (macOS)

- **`make build` leaves the app ad-hoc signed; run `make sign-local` after it.** macOS ties TCC grants (notification permission included) to the bundle's code identity, and an ad-hoc identity is a content hash — it changes on every rebuild, so a granted permission is forgotten and the native notification path can end up refused for good (`Notifications are not allowed for this application`). `sign-local` re-signs with the local self-signed identity `Tachi Local Code Signing` (login keychain, trusted for code signing), which is stable across rebuilds.
- **Build a cert once with `security` + OpenSSL if the keychain has none** (`security find-identity -v -p codesigning` → `0 valid identities found`): `openssl req -x509 -newkey rsa:2048 -nodes -subj "/CN=<name>" -addext extendedKeyUsage=critical,codeSigning …`, export with **`-legacy`** (macOS cannot verify OpenSSL 3's default PKCS#12 algorithms — "MAC verification failed"), `security import … -T /usr/bin/codesign`, then `security add-trusted-cert -r trustRoot -p codeSign -k ~/Library/Keychains/login.keychain-db cert.pem`.
- **Notifications only fire while the window is NOT focused** (`desktop/notify.go`): testing with the app in front proves nothing. And the TUI's notifications are a different mechanism entirely (`terminal-notifier` / `osascript`), so a notification whose source is terminal-notifier is never the desktop's own — **but it can still be the desktop's fault**: the app is often launched from a herdr pane, inherits `HERDR_ENV`/`HERDR_SOCKET_PATH`/`HERDR_PANE_ID`, and then auto-enables the herdr hook (`agent/configureHooks` → `hooks.DetectHerdr`) — so herdr, not Tachi, raises a terminal notification for a window that has no pane. `DetectHerdr` therefore also requires stdout to be a terminal (a pane's process renders into it; a GUI app and an editor-hosted ACP server merely inherited the env). To check what actually ran: `log show --last 10m --predicate 'eventMessage CONTAINS "terminal-notifier"' --style compact` prints the TCC attribution with the **responsible** process (`com.mitchellh.ghostty` = a terminal launched it). `notifyTurnDone` is for the transcript lane only: a side-channel run (/review, /commit) has no turn on screen, so it gets its own copy through `notifyOneOffDone` — 评审完成 · N 条意见 / 未报问题 / 已停止 / 未完成（见日志）, 提交完成 — raised from `commands.go`, where the outcome is known.

## Desktop Backend (bindings & paths)

- **The Wails bindings are GENERATED and COMMITTED**
  (`frontend/bindings/github.com/monsterxx03/tachi/desktop/agentservice.ts`). Adding, renaming or deleting
  an exported method on `AgentService` means re-running
  `cd desktop && GOWORK=off wails3 generate bindings -clean=true -ts -i` (that is `build/Taskfile.yml`'s
  `generate:bindings`, which `make build` runs for you; `-clean` empties `frontend/bindings` first). Never
  hand-edit one: the numeric `$Call.ByID(...)` is derived from the method, and a frontend calling a method the
  generator has not seen fails `tsc` — which is the only thing that type-checks the call at all. Regenerating
  on a clean tree is a no-op, so a diff in that directory is always a real change.
- **Only `desktop/` is a Go module of its own** (it is NOT listed in the repo-root `go.work`), so every Go
  command for it runs with `GOWORK=off`: `desktop/Makefile` exports it, and the root `desktop-smoke` target
  sets it for the one command it runs from the parent directory.
- **A session's directory is `config.SessionDir()` + ONE path element**: `sessionDirPath(id)` in
  `sessiondir.go` is the single resolver, and it REFUSES anything a webview could send that is not a single
  name (`../..`, `a/b`, `.`, empty). The rule came from `oneOffDir`, which now shares it — every
  "where is this conversation stored" question goes through that one function.
- **Handing a path to the system is `attach.go`**: `OpenPath` (default app; a directory opens its Finder
  window) and `RevealPath` (`open -R`), both `stat`-ing first and returning `"ok"` or the reason. New
  open/reveal actions reuse them, and a test replaces the single `openFile` var to read the argv instead of
  popping a real Finder window.
- **A bash `ask` rule is a question for the USER, and the desktop answers it the way ACP does — never with a
  fourth `PermissionMode`**: every session's agent is built with `agent.PermissionModeExternal` plus a
  `PermissionHandler` (`desktop/permission.go`), so `agent_permission.go`'s ask branch parks the turn on the
  window and the decision comes back through `AgentService.AnswerPermission`. `PermissionModeSkip` is the wrong
  posture here: its ask branch is the *unattended* one (channel / subagent / `tachi -p`) — it refuses the command
  and says "add an allow rule", to the user who wrote that rule, in the one frontend that has them in front of
  it. `deny` behaves identically either way; only `ask` differs. Taking the external path (the shape the TUI
  confirmation and ACP's `session/request_permission` already use) is what leaves the other entry modes'
  behaviour untouched — no agent-loop change at all.
  Four properties worth keeping: an answer is addressed by **session + tool call id** (`permKey`), because the
  ids come from the model and two conversations can generate the same one, and `AnswerPermission` refuses an id
  that is not the ask actually waiting (a stale card cannot approve a later command); the pending entry is
  dropped by the waiting goroutine itself, so Stop (ctx cancel) cannot leave an approvable orphan behind; a
  side-channel run has no turn on screen, so `commands.go` marks its ctx (`withoutAsk`) and the handler refuses
  instead of parking a run nobody could answer; and 「本会话全部允许」 is a **session** decision, not a command
  memory — both frontends flip the same switch (`Policy.AllowAllAsksForSession`), so later asks never reach
  either frontend at all. That last one is a correction worth not undoing: an agent's shell commands almost
  never repeat verbatim, so remembering the approved one was a button that could not deliver, while the TUI's
  prompt had been labelling the choice `[a]lways(session)` all along. The switch sits BELOW the deny checks in
  `CheckBash` on purpose — "stop asking me" must never become "run what I forbade", and `perm-session` pins
  both halves (a second, different command runs with no card; a denied command still refuses).

## Desktop Skills (the store belongs to the SESSION, never to the process cwd)

- **Skills are ON.** `DisableSkills` was left `true` in the initial wiring commit, where it sat beside
  `DisableMCP` / `DisableSystemReminders` as an S2 stopgap ("skip for now, wired in S3") — those two were
  switched on and given a reason, this one was forgotten. The flag is the knob for the NON-interactive modes
  (`tachi -p`, `tachi commit`); this frontend has a human in front of it.
- **The store is built per session, from that session's working directory** (`sessionSkillStore` in
  `agent_driver.go`) and handed to the agent as `AgentConfig.SkillStore`. The DEFAULT path is the trap:
  `initSkills()` builds `skill.NewStore(config.FindProjectRoot())` — the PROCESS cwd — and a Finder-launched
  app's cwd is `/`, so a default-built store would scan `/.tachi/skills`, offer no project skills at all, and
  point `Skill create`'s `source: project` at the filesystem root. Same shape as the system-reminder trap
  (`config.FindProjectRootFrom`, `systemreminder.workDir`): a per-session thing resolved from a process global.
- **A store's scan roots are FIXED when it is built** (`skill.Store` re-reads the disk on every `List`/`Load`
  but never re-resolves its directories), so a session that MOVES is re-pointed explicitly:
  `SetSessionWorkingDir` → `AIAgent.ReloadSkillsIn(dir)`. Without that call the session keeps serving the old
  tree's project skills — and sends `Skill create`'s project target there — until it is reloaded. The reload
  also clears the activation state, which is correct: the same name may resolve to another file after the move.
- **The scope is the session's git root, plus the global dir** (`config.GlobalSkillsDir()` = `<base>/skills`,
  note NOT `<base>/.tachi/skills` — the two shapes differ, and a test fixture written for one is invisible to
  the other). **Additional roots are NOT scanned**: they are extra places to read and write, not a second
  configuration home — the same line @-references and other agents' per-directory configs draw.
- **There is no `/skill` command and no skill UI here**: the shared registry's `Modes` for it exclude
  `ModeDesktop`, so the catalog reminder is what tells the model what exists and the model calls the `Skill`
  tool itself. Adding the command is the registry entry PLUS a desktop handler — its own decision, not a
  side effect of turning discovery on. Until then the catalog's closing line ("or the user can type
  /skill-name") is a dead end in this frontend: `RunCommand` answers 「未知命令」 for that form and
  「desktop 暂不支持 /skill」 for the bare one.
- **The `skill-catalog` smoke scenario pins the scan**: its fixture tree carries
  `.tachi/skills/smoke-skill/SKILL.md`, and the assertion is on the REQUEST (`SMOKE-SKILL-MARKER` found in the
  first request's skill catalog) — nothing in the UI names a skill until one is used, so the prompt is the only
  place the scan is visible. The driver's half asserts the injection disturbed nothing: a turn ran, no tool
  card, no permission card.

## Desktop Turn History (the interrupted-turn merge)

- **A turn's history is the CONVERTED form, reminder blocks included.** `runHistory` holds
  `[]llm.Message` as `agent.ConvertSessionToLLMMessages` builds it, and that conversion re-attaches
  the `<system-reminder>` block to the user message it was stored with — deliberately, since
  `historyHasReminder` reads the prefix to avoid re-injecting first-message-only reminders.
- **So anything that MOVES a message between turns must unwrap it first.** The one such site is
  `mergeTrailingUserMessage` (`agent_turn.go`): an interrupted or killed session can leave the
  history ending on a user message, and that message is folded into the next turn's text so the
  provider never sees two consecutive user messages. Merging the content verbatim carried the
  PREVIOUS turn's block along — the model then read a stale plan id, stale diagnostics and a stale
  branch as if they were current, and because the merged text is recorded as the new message, the
  block walked forward through the session turn after turn. It now strips via
  `systemreminder.UnwrapUserMessage`, the single implementation of that inverse (the vision
  fallback's old private copy delegates to it too).
- **A trailing message that is nothing but a block is left alone — deliberately.** Two shapes look
  identical there: a reminder the loop injected mid-run (`injectLoopReminders` appends one as a user
  message, so a Stop landing right there leaves it trailing), which is stale scaffolding; and a
  trailing artifact reminder, which `ConvertSessionToLLMMessages` appends as its own user message ON
  PURPOSE so a reload cannot drop it (an `/research` or `/review` finding must still reach the model
  after a restart). No marker separates them, so dropping the message would break the second to tidy
  up the first. Do not "finish the job" here without a way to tell them apart.
- **That state is reached by RELOAD or by a well-timed Stop, not by an ordinary Stop**: a stop
  mid-stream has already recorded the partial assistant text, so the history ends on an assistant
  message and no merge happens. What produces a trailing user message with real text in it is a
  session reopened after the reply was never recorded (the process was killed, or the turn
  hard-failed) — which is why no smoke scenario covers it, and why `desktop/agent_turn_test.go`
  builds its fixture through the REAL conversion instead of hand-writing a history shape.
- Design context and the mechanism it belongs to: [plan-panel design §7](2026-09-12-desktop-plan-panel-design.md).

## Desktop UI State

- **A turn's process is folded by the CONVERSATION, not by the part renderer**: `turnView()` (`transcript.ts`,
  pure, shared by the live view and a rebuilt transcript) decides what a turn shows — one strip
  (`ProcessStrip`, `components.tsx`) standing in for its thinking blocks, tool cards and intermediate
  messages, the turn's LAST prose, and the parts that must never be hidden: a FAILED call, the call a
  permission card is parked on, the call an AskUserQuestion form waits on, and notices. A call that is
  merely RUNNING is not among them — the live row reports it, an exposed card appeared and vanished once
  per step and made the prose jump, and a call parked on a form is waiting rather than running. `TurnPart` stays atomic because `oneoff.tsx` replays the very same parts and exists to show
  them all — a change there would make the side panel lose the process it is for. The open/closed state is
  per turn and presentational (never persisted), and the footer's diff chip opens the fold when it opens
  every diff, since those diffs live inside the folded cards.
  Smoke cost: a scenario that asserts a *successful* tool card must expand the strip first
  (`.process-head`), and "no tool cards" is no longer evidence of "no tool calls" — assert the strip's
  absence too. `perm-deny` needs neither: a denied call is a failed part, and failures never fold.
  While a call is RUNNING the same row reports the current step instead (`processLiveLine`); it renders
  even when nothing is folded yet (the running card is exposed, so `folded` is empty) and is then a
  status line rather than a toggle. There was an "activity row" above the composer saying the same
  thing, and it was REMOVED: it existed only while a call was running, so it appeared and vanished once
  per step and shoved the message area up and down — and the fact was already in the transcript twice
  over. If it ever comes back, it must hold its place for the whole turn.
  Design + clickable prototype: `docs/2026-09-13-desktop-transcript-density-design.md` (+ `.html` beside it).
  **Do not compensate a layout change from a `requestAnimationFrame`**: an occluded app window never
  delivers one (measured: the callback simply never ran while the driver's window was behind another
  app), and a `setTimeout` is throttled just as unpredictably. Compensate in a LAYOUT effect instead —
  the new content is in the DOM and the adjustment lands before paint. This is how the fold toggle keeps
  a bottom-following reader pinned: without it, expanding a turn slides the view up by the height that
  appeared, the next scroll event reads as "the reader scrolled away", and auto-follow switches itself
  off (the design's 「展开/收起不该让滚动位置跳」).

- **The titlebar is `App.tsx`'s `<header className="titlebar">`**: brand, the sidebar toggle, the session id
  (click to copy), then `titlebar-right` — the side-panel toggle and the theme switch, held at the far edge
  by one `margin-left: auto`; a new titlebar control belongs in that cluster. The work-status badge that used
  to end it (状态圆点 + 文字) was removed on 2026-09-13 （「右上角的工作状态指示删掉」）: 运行中 already reads
  from the sidebar row's `spin-dot` and the reply's typing dots, so the agent state needs no echo in the
  chrome. It took its own styles with it — `STATUS_META` (`types.ts`), `.status-badge` / `.dot*` /
  `.status-label` / `dot-pulse` (`layout.css`) and the reduced-motion exemption (`base.css`) — so a status
  dot that comes back starts from those, not from a bare node.

- **The context ring is ANCHORED on the last call's real prompt size, not on the character
  estimate** (`desktop/contextinfo.go` → `AIAgent.LastInputEstimateWithBreakdown`). The estimate
  (`agent/token_estimate.go`) is character-class based and its bias depends on the content: it
  over-counts plain English and UNDER-counts mixed CJK + JSON (whitespace is charged nothing), which
  measured **0.815x** on a long Chinese conversation full of tool results — a ring reading 716,714
  while the provider had billed 879,259. So the reported number is the last call's real prompt size
  (`llm.PromptTokens`: input + cache read + cache creation for Anthropic; the total alone for
  OpenAI-family, whose `prompt_tokens` already contains the cache reads) scaled by the estimate's
  movement since that call — the bias then applies to one turn's additions instead of the whole
  prompt. Two
  consequences worth keeping: the breakdown in the popover is scaled to that total (`Breakdown.ScaleTo`,
  because the parts must add up to the number above them — `ctx-ring` asserts they agree within 0.5
  points), and a session restored from disk anchors on the last recorded message instead
  (`estimateFromMessages`), so a reload is not a step backwards. The popover's own caveat has to
  follow: it said the number was a local estimate and NOT API usage, which the anchoring made false
  (`components.tsx`, the context panel's footer).
  The correction is a RATIO of the estimate's movement applied to the anchored real value, not an
  additive delta with a floor: after a compaction the history is REPLACED by a summary, and a floor
  would leave the ring sitting on the pre-compaction number — the one number compaction exists to
  bring down (`compact` asserts the meter falls). One consequence for the smoke suite: a scenario
  about the meter must have the mock report a prompt size that matches the conversation it was given
  (`textStreamPrompt`); a fixed 1200-token report next to a 30k-token history is a state the app
  cannot reach in production, so the scenario would be testing the mock's arithmetic. The auto-compact
  THRESHOLD reads the anchored value too (`shouldAutoCompact`), so the trigger and the meter agree —
  the reader can predict when it fires. The cooldown is the one comparison still on the raw estimate,
  and rightly so: it measures the conversation against itself, where the bias cancels. A scenario that
  exercises AUTO-compaction therefore has to give the mock a plausible prompt size as well — the
  anchored value is what fires.

- **The sidebar is `sessionRow` in `App.tsx`**: the conversation list, the folded compaction chains
  (`sessionRows` in `lib.ts` supplies the rows), rename, and the row's right-click menu (打开会话目录 /
  重命名 / 删除). A new row-level action belongs there rather than in a second list component — and one that
  hands the screen to another app is asserted, never clicked, in a driver (see the smoke rules).

- **Following the bottom must survive async height changes, not just message updates** (`desktop/frontend/src/App.tsx`): the transcript pin ran only when `msgCache` changed, so anything that grew the content afterwards — a mermaid diagram finishing its async render, an image/attachment card loading, a tool card expanding — slid the visible content up by exactly that height until the next delta pinned it back ("切回会话时先向上飘，再跳到底"; measured at 298px in `switch-scroll`). The fix is a `ResizeObserver` on a `.chat-content` wrapper (the scrollport's own box never changes when its content grows) that re-pins while following. Any new "sticky bottom" behavior must go through the same observer.

- **A width bound must subtract every other fixed column — and it must limit what is SHOWN, not what is STORED**: `.oneoff-panel` exists so the conversation keeps 480px, but the first version computed its ceiling from `window.innerWidth` alone — forgetting the 280px sidebar — so in the default 1200px window the panel could be dragged to 720px and leave the conversation 200px (the comment promised the opposite). Read the sibling's REAL width from the DOM (`panelRoom()` in `frontend/src/oneoff.tsx`, so a folded sidebar hands the room back) and re-measure on `resize`. Then keep the two numbers apart: App holds the width the reader CHOSE (persisted), the panel derives what there is room to show (`clampPanelWidth(chosen, room)`) — writing the clamped value back to state (the first fix) made "widening the window gives the width back" true only on the next launch, which is the kind of promise-versus-implementation gap a review should catch.

- **Anything cached per run or per session must be KEYED by it**: two leaks in one file, both from a cache that outlived its subject — the report text (a file fetched by name, shown with another run's file line, and never refetched because the guard was `!== null`) and the caller-supplied `paths` (a turn's file set that, once handed to the panel, diffed EVERY later run against it, with `hasPaths` still true). Key the cache with `sessionId/run` (a record's name is a timestamp within its session, so two sessions can hold the same one), and prefer the run's OWN record over anything a caller passes in: `agent.OneOffKeyPaths` is per run by construction, a caller's idea of "which turn was this" is not.

- **Never run a side effect inside a state updater**: React may invoke an updater more than once for the same update (StrictMode does; concurrent re-basing can), so `setState(prev => { fetch(...); return ... })` fires the request twice — and an `if (prev[k]) return prev` guard cannot help, because both invocations see the same pre-update state. Keep the in-flight bookkeeping in a ref instead.

- **A transient state cannot be waited for — sample the sequence**: a UI state whose length is the WORK's length (the review chip's 评审中… lasts as long as the run: a few hundred ms under mockllm, whose replies carry no delay — `textStream`'s second argument is cacheRead, NOT a delay) makes `waitFor` a coin flip, and a synchronous read right after `.click()` sees the pre-Render state because React commits asynchronously. Sample the element on a ~10ms timer, dedupe into a trail, and assert the trail CONTAINS the state; the trail then doubles as the failure message. What made 评审中… invisible was not the sampling though — see the next point.

- **A state flag must be ended by the fact that ends it**: `reviewPending` was cleared when `ReviewChanges` returned (that call only STARTS the fork) and, in a second rule, whenever the session was idle — but the session is still idle in the window between the click and the fork going busy, so the middle state was wiped before it could ever be seen, and the chip fell through to 已评审 while being disabled by the very run it described (the report: 「立刻会变成已评审查看，但没法点击」). The run's own end event is the fact that ends it. Verified by putting the idle rule back for one control run: the chip's trail became 评审本轮改动 → 已评审 1 条 · 查看 with no middle state at all.

- **Session-scoped UI state must be keyed by session, or reset on switch**: one React tree renders every session, so anything derived from the ACTIVE session silently leaks into the next one. Hit three times: the review notice (now keyed by the clicked turn's message id), plan/findings (reloaded on switch), and the cache-hit ring + cost (a new session inherited the previous one's numbers — anything fetched per session must be applied unconditionally, `if (u)` keeps stale values when the payload is empty).
- **An entry's data source must belong to the surface the entry opens** (`desktop/frontend/src/App.tsx` +
  `diff.tsx`): the turn footer's 「完整 diff」 promises *this turn* against git HEAD (its own tooltip says
  真实文件行号) but had been routed into the side-channel panel, which is keyed by RUN — so on a turn nobody
  had reviewed it opened an empty column (「当没有 review 时，点击『完整 diff』，侧边栏展示的是空的」: the
  panel's list has no runs, and it says so), and whenever a run did exist it showed the *selected* run's file
  set rather than this turn's. The panel cannot do better: after P4 its diff comes from the run's recorded
  scope (`header.paths`) and nowhere else, because a caller-supplied set outlives the run it came from (that
  leak is why P4 removed it). So the turn's diff went back to a surface of its own — `TurnDiffOverlay`
  (`ViewerOverlay` + `DiffFindingsPane` with `findings=[]`), whose paths are the click's own: fetched on
  open, dropped on close, so they can never become that stale anchor. Two corollaries worth keeping:
  **a fold default belongs to the surface that shows the diff** (the panel's rule "a file with findings
  opens, one without stays folded" folded EVERY file on a surface that has no findings at all, so the
  promised diff arrived as a list of filenames — measured `.diff-ln` = 0, hence `expandAll`), and
  **a helper whose only caller is gone goes with it** (`openFindings`, the second way into 意见, was deleted
  rather than left as a path nobody walks).

- **The transcript itself lives in `desktop/frontend/src/useTranscript.ts`** (messages per session, running flags, the loaded window + its cursor, the frame-batched delta queue). Every mutator takes the session it is for — `updateSession` is the one funnel, everything else is built on it — so "this belongs to a conversation" is enforced by the signature instead of remembered. App.tsx must not declare its own `useState<Record<string, Message[]>>`; a new per-session need is a new op in that hook.
- **A system reminder is a HISTORY artifact, not a live one** (`lib.ts` + `App.tsx`): `Message.reminder` is set only by `buildTurns`, from the store's `role: reminder` rows, so the `!` affordance appears on a user bubble once the transcript is REBUILT (load / switch / restart) — a turn that has just run shows none, however much context the model actually received. Assert injected context at the LLM boundary (see the `project-context` scenario) or through a rebuild; a driver waiting for `.reminder-head` on a live first message waits forever.
- **Backend events are subscribed in `desktop/frontend/src/agentEvents.ts`** (`useAgentStatus` / `useSessionUsage` / `useAgentStream`) — status, the active session's live numbers, and the agent stream that writes the transcript. Each payload names its session; compare it against `currentId` only where the UI genuinely means "on screen" (a sidebar row, the error bubble), never to address storage.

- **A number that "follows the turn" lags a long turn — the context ring must follow each API CALL** (`desktop/agent_turn.go` + `desktop/frontend/src/agentEvents.ts`): the ring's estimate was read only on mount / new / switch / `turn_complete` (through `refreshProvider`), while the popover fetches `GetContextInfo` when it opens — so during a long turn (many tool rounds) the ring sat at whatever the turn started with (0.0% for a fresh session) while the popover at the SAME moment already said 4.4%, and switching sessions only "fixed" it because switching happens to refresh (「新建会话后 agent 执行了很多，圆环一直是空的，点开倒是有，切走再回来就正常」). `emitUsage` already ran after every API call, so the estimate now rides that event (`agent:cost` carries `contextEstimate`/`contextWindow`, from the one `contextUsageOf` rule `GetProviderInfo` shares) and the frontend keeps it in `useSessionUsage`, re-reading by id (`GetContextInfo`) only for a session that has not run a turn in this process yet. Two lessons: "the turn ended" and "the call ended" are different refresh points, and two surfaces describing the same fact must read it from ONE rule, or they can answer differently at the same instant. Pinned by `ctx-ring` (a `sleep 4` tool holds the window open and the ring is read while the tool runs).
- **The composer is `desktop/frontend/src/composer.tsx`** (`useComposer` + the `Composer` element): the input box, the @-picker, the "/" palette, the queue of messages typed while a turn runs, and the parked-question form. All four share one decision — `route`: send now, queue for the next steer point, or dispatch as a slash command — so keep new input/queue behavior there, not in App.tsx.
- **A parked permission is the composer's state too, and its card is matched by tool CALL ID** (`composer.tsx` + `App.tsx`): `perms` sits next to `asks` and follows the same rules — keyed by session, NOT cleared on session switch (the agent is still parked), moved on auto-compaction, cleared on `agent:idle` — and the form replaces the tool card whose `toolCallId` equals the request's (every live push of a tool part sets it from `ev.ToolID`, as the history replay already does). Matching by tool NAME is the trap: a model emits a whole batch of calls before any of them runs, so "the newest unfinished Bash card" is usually a different call than the one waiting, and the answer would land on the wrong card.
 `agent:permission` carries the agent's own preview rendering (`bashAskPreview` — `$ command` + the matched rule) and the form shows it verbatim: parsing it in the frontend would be a second definition of a format the backend owns. The form keeps its answer's own state (`busy`/`err`) and clears only when `AnswerPermission` returns `"ok"` — the fact that ends the wait is the backend ACCEPTING the decision, and a refused one (the run is gone, it was already answered) must stay on screen with its reason rather than vanishing into a turn that will never move again.

- **Path tries are byte-keyed: iterate strings by BYTE, not `for i := range s`** (`for i := range s` walks rune boundaries, so `s[i]` yields only the first byte of each multi-byte character). Cost: Chinese file names came back mangled from the @-file index — the picker showed 7 `?` and the inserted `@`-reference pointed at a file that did not exist (`pkg/container/pathtrie.go`, fixed 2026-09-12).

- **A pane's width is its MIN-CONTENT width, and that is set by whichever row cannot shrink** (`desktop/frontend/public/chat.css`): in the side panel every diff head row (`.diff-panel-head`, `.diff-file-head`, `.diff-findings`) holds non-shrinkable badges/buttons, and a `white-space: nowrap` path with the default `min-width: auto` contributes its WHOLE string — so the pane measured 734px inside a 419px panel, its content scrolled sideways, and the send bar's ends went off-screen with it (the report: 「diff 文件过宽要挪动滚动条才能看到按钮」; measured `scrollW/clientW = 827/419`, and after the fix `419/419`). Three things together: `flex-wrap: wrap` on those rows, `flex: 1 1 0; min-width: 0` on the path (basis alone is not enough — the automatic minimum is the min-content), and `max-width: min(1100px, 100%)` on `.viewer-doc.is-diff`, which the file-preview overlay and the panel share. Moving the button to the bar's left end does NOT fix it: the bar spans the content width, so either end can scroll away. The user's bubble is the same problem in miniature: a pasted URL has no break opportunity, so it ran 1213px past a 520px bubble (「贴了个长链接…突破了消息气泡」; `scrollW/clientW = 1733/520` → `520/520`, pinned by `bubble-wrap`) — and it takes `overflow-wrap: anywhere` rather than `break-word`, because only the former also shrinks the min-content.

- **The context ring measures the PROMPT of the last call — a reply enters it only on the NEXT turn** (`agent/token_estimate.go` + `desktop/agent_model.go`): the estimate is computed before each API call, so a turn that ends in a plain reply leaves the meter describing the history *without* that reply, and a compaction of a small conversation is invisible under the system-prompt/tool-schema floor (measured: 4.4% before, 4.4% after — on a fixture where the ring legitimately does not move). `compact` therefore puts TWO turns in front of the compaction: turn 1's reply is what turn 2's prompt measures. A scenario about "the context got smaller" must fill the window first, or it asserts nothing.

- **A session list is a list of CONVERSATIONS, not of session dirs** (`sessionRows` in `desktop/frontend/src/lib.ts`): a compaction chain shares one title, so rendering `ListSessions` raw shows the pre-compaction session as a second, identical-looking row — during the session and, because nothing remembered the switch, again after a restart. The shape is derived from `compactedParentId` (`SessionInfo`, from `meta.json`), folded under the newest link and closed; the ancestors stay clickable history.

- **A control the reader uses WHILE a pane scrolls belongs in the sticky bar, not in the header** (`desktop/frontend/src/diff.tsx`): the 意见 pane's ↑/↓ walk sat next to the finding count at the top, and jumping down to a finding scrolled the header — and the walk with it — off the screen, so the reader had to scroll back up to jump again (「一往下跳箭头就看不到了」). `.diff-sendbar` is the only thing in that pane that stays put (`position: sticky; bottom: 0`), so the walk moved into it (and it no longer depends on `onSend`: where you are in the list is not a decision about sending). The cost is `flex-wrap: wrap` on the bar: it now holds seven controls, and its min-content would otherwise widen the panel past its 419px column — the very regression the width fix removed. Verified BOTH ways on a fixture whose second finding is 60 lines down: in the bar `jump=[710,727] ⊂ pane=[124,780]`, back in the header `jump=[-732,-715]` (732px above the pane's top edge).

- **The width the reader drags is NOT React state** (`desktop/frontend/src/oneoff.tsx`): the width lives on the panel element, so a `setState` per `pointermove` re-rendered the whole panel — the 意见 pane's diff included, a couple of hundred rows in the smoke fixture and thousands on a real review — before the browser had even laid the new width out; that is 「左右拖动时很卡」. The drag now writes `panelRef.current.style.width` straight from the pointermove handler and commits to React once, on release (which is also what persists it). A `useLayoutEffect` re-asserts the drag's own number after every commit, because a render that happens mid-drag (a streamed delta, a re-measured `room`) must not put React's older width back on screen. The behaviour is pinned by `oneoff-panel` (press → follow → clamp → commit → persist); the *smoothness* has no assertion — see the Reduce-motion trap above for why a same-task read cannot see it.

- **The one-off panel's × promised Esc and nothing listened — and who owns the FIRST press is decided by focus** (`desktop/frontend/src/oneoff.tsx`): a column is not a `ViewerOverlay`, so the viewers' dismissal contract never covered it and the key did nothing at all. Two consequences worth keeping: `App`'s global handler must not claim every Escape unconditionally (it would swallow the panel's before the panel could see it as unclaimed — `defaultPrevented` is the handshake), and the panel is a window-level listener that refuses only an ALREADY-CLAIMED key, so a viewer or modal still wins by construction (ViewerOverlay claims it in the capture phase and stops it there). Both orderings are pinned in `oneoff-footer`, because the composer is the common case, not a corner: `/review` leaves the focus in the composer, so there the first Esc only hands the focus to the message area (`.chat`, verified) and the SECOND closes; a field inside the panel blurs on the first. The reader chose this ordering over "panel wins" — 「面板内输入框第一次只退出输入」 and the composer's Esc are the same gesture.

## Desktop Themes (the dark palette)

- **The dark theme is Ayu Mirage-derived and its contrast is low ON PURPOSE — do not "fix" it.** The
  family is that theme's blue-grey (`surface.base` #1F2430, panel #282E3B, panel shadow black @0.2),
  lifted one OKLCH step so the app reads as graphite instead of black (`--bg` #252a35, chroma ~0.022
  against the old neutral ~0.010), with Ayu's *warm* grey editor fg as the CONTENT ink (`--text`
  #cecdc6) and its cool `ui.fg` family as the CHROME ink (`--text-dim`/`--text-faint`, 10.5–11.5px
  metadata). Body text runs ~9:1, against ~12.8:1 before the 2026-09-12 pass.
- **Every "go" button is ONE flat recipe — do not put the gradient back.** `.send-btn`, `.pending-send`,
  `.diff-sendbar-go` and `.plan-start` share a single solid `--accent-strong` fill: no gradient, no inset
  sheen, no coloured glow (`.new-chat` is the tinted variant of the same idea). That 165deg gradient was the
  last raised surface in an otherwise hairline-based UI, and its light end carried the button's own white
  label at **3.1:1** — flattening fixed the look and a legibility bug in one move. Two consequences worth
  keeping: button contrast is now tuned in ONE place (`--accent-strong`, which is why the dark theme's sits
  at chroma ~0.11), and **a fill that carries white text always takes the `-strong` variant** — the stop
  button's hover used `--red` (tuned as INK) and put a white glyph on 2.5:1.
- **What bounds the palette is `--text-faint`'s 4.5:1 floor, not `--text`.** It is the token that lands
  on the *lightest* plane (placeholders, MCP labels, `.file-note` — panel-solid, and panel-2 under the
  picker/CSV headers), so it pins both the ink (~#9ca3b1) and the top of the ladder (~#303541). Any
  "make it lighter/greyer still" edit has to trade against that: raise the planes and the smallest
  text is what breaks first, and `--text` has room to spare. Verify with a WCAG ratio check across
  every plane (`luminance` + alpha compositing), not by eye.
- **`windowBgLight`/`windowBgDark` (main.go) are the same decision in a second place** — the pre-paint
  window colour, which `uitheme.go` also feeds to the native appearance. They had silently drifted
  (light was #f6f7fb against a #f7f4ee page); move them with `--bg`.
- **One definition per mode, and it lives in `frontend/public/base.css`**: the `:root[data-theme="dark"]`
  block, resolved by the boot script in index.html + `src/theme.ts`. No frontend rule reads
  `prefers-color-scheme`, and no other file carries dark colours — the hljs tokens in `widgets.css`
  map onto the semantic tokens, so a palette edit covers code blocks for free. A one-off shadow in
  `chat.css` was the exception and is now `--shadow-up`.
- **To SEE a palette change, do not reach for the app window** (window capture is dead on macOS 26 —
  see the smoke section). Render the tokens instead: extract both blocks from base.css (next to
  `git show HEAD:desktop/frontend/public/base.css` for the before) into a small HTML page whose
  swatches/mock rows read `var(--token)`, one scope per variant, and screenshot it with headless
  Chrome (`--headless=new --force-device-scale-factor=2.6 --window-size=…`). That gives a real A/B of
  the REAL values, and a 3-way render is how 「再灰一点 / 再亮一档」 gets decided without a rebuild
  loop.

