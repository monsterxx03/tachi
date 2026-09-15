# Tachi — Desktop Handbook (split out of .tachi.md)

Read this **before changing anything under `desktop/`**: the smoke suite (drivers, fixtures, the
bundle/pkill/keep-awake rules), macOS build & signing, and the frontend's UI-state conventions (which
module owns what).

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New desktop lessons belong in THIS file, not back
in `.tachi.md`** — and the rules that ride with that file (freshness in the same turn, convention-not-history,
split + index when a subject grows) apply to every split of it, this one included.

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

Desktop-only paths (Wails wiring, rendered UI, end-to-end flows) need the real window: unit tests and itest
cannot see whether a binding's result reaches a card. **Use the in-repo suite** for anything touching desktop
state, events or UI, and add a scenario when a fix deserves a regression test. It owns the sandbox, the
isolated HOME, the scripted model and the report; the verdict is an exit code.

```sh
make desktop-smoke                                  # every scenario
make desktop-smoke ARGS="-run send-now -v"          # one of them, every assertion printed
make desktop-smoke ARGS="-keep"                     # keep the sandbox after a pass (artifacts)
```

The suite does not need its window in front, and does not take the front: demo mode keeps the page awake
while another window covers it, and a run launches without activating itself — so a run can be left going
while you work.

The suite is `itest/desktop/`: `main.go` (runner: sandbox, launch, verdict, report), `scenarios.go` (one
conversation per scenario — the mock's steps plus the Go-side `after` assertions), `sandbox.go` (bundle prep,
isolated HOME, launch/kill), `sink.go` (the loopback endpoint drivers report to), `drivers/` (harness.js +
one script per scenario). Per run it builds a throwaway sandbox holding a private copy of the app (executable
renamed so no `pkill` can reach the user's own Tachi), an isolated HOME with a `config.yaml` pointing at the
scripted model, the work dir, `driver.js`, `dom.html` (the page as the verdict found it) and
`mock-requests.txt` (what the model was actually asked). **`desktop/bin/Tachi.app` must exist**
(`make -C desktop build`); the runner refreshes the executable inside it from `bin/Tachi` on every run, so the
app under test is always the current tree.

A driver asserts what the UI **shows** (transcript shape, chips, buttons); the scenario's Go half asserts what
actually **happened** (the prompt the model received, the files written) — a regression usually shows up in
exactly one of the two, so write both. **Adding a scenario** = `drivers/<name>.js` written against the harness
(`smoke.waitFor` — never a fixed sleep — `smoke.check(label, ok, detail)`, `smoke.finish()`, plus
`q`/`qa`/`text`/`allText`/`type`/`pick`/`click`/`key`/`sleep`) and an entry in `scenarios.go` (the mock's
`steps`, the work-dir `files`, an `after` func for the Go-side checks). Keep both halves small: one behaviour
per scenario, and assert what the user would notice. A scenario that needs an ADDITIONAL workspace root sets
`extraRoots` (one directory per entry under the sandbox, holding that entry's files): the root set is seeded
into the session fixture rather than added in the UI, because the UI's only way in is a NATIVE directory
picker no driver can click — reach a seeded root from the work dir as `../<name>/…`, since a scripted bash
command cannot know the sandbox's absolute path. A scenario that needs settings the shared sandbox config
does not carry sets `config` — a YAML block appended to the generated `config.yaml`, which is how a
parked-permission scenario gets its `permissions.bash.ask` rule. `smoke.type` picks the native value setter
by ELEMENT (textarea vs input): React ignores `el.value = …`, and a textarea's setter called on an input
throws, so an `<input>` field (the find bar) could not be driven at all until the harness stopped assuming
the composer's textarea.

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
sleep 24                                     # mount + the driver's own timing (the app's own
                                             # bootstrap waits are ~0.75s; see demo.go's
                                             # TACHI_DEMO_*_MS note — this sleep is generous
                                             # because a hand-run has no sink to wait on)
R=$(osascript -e 'tell application "System Events" to tell process "TachiSmoke" to get {position, size} of window 1')
I=$(echo "$R" | tr -dc '0-9, '); screencapture -x -R "$I" /tmp/tachi-smoke/shot.png
pkill -f TachiSmoke; kill $MOCK
```

- `--env TACHI_DESKTOP_APPEARANCE=dark|light` forces a theme (dev aid); run twice for both palettes
- The `osascript` query above needs **Accessibility granted to whatever runs it**: without it you get no
  window bounds and a system prompt left on screen for the user to dismiss. The suite never needs it — it
  drives the page from inside the app and pins window-level facts in Go tests
- The scripted model is a driver program, not a config: `mockllm.NewServer(...)`,
  `Script(Step{Reply: Stream(Text("…"), Finish("stop"), UsageWithCache(…), Done())})`, print `BaseURL()`,
  block. It writes the isolated `config.yaml` (the port is random) and dumps `mock.Requests()` to a file —
  **assert at the LLM boundary**, not only on the rendered bubble
- Fixtures: `$HOME/.tachi/session/<id>/{meta.json,messages.jsonl}` (shapes as `session.Store`),
  `additional_dirs`, `desktop_ui.json`; bump `updated_at` so the app auto-loads it

### Driver JS (`TACHI_DEMO_JS`, injected by `desktop/demo.go` — two waits, ~0.75s by default)

The injection waits (`TACHI_DEMO_BOOTSTRAP_MS` / `TACHI_DEMO_LOAD_MS`, demo.go) are the floor
on EVERY scenario in the suite, so they are kept tight and must not be trimmed by eye: too
small and the script is silently dropped (the scenario then reports an empty
`mock-requests.txt` and rides out its whole budget — see the comment on those vars, including
the grid the current values were measured at).

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

- **Only one instance may run**: `open` on a live instance only hands the launch to the running process and
  passes NO environment (so the driver never runs and the previous scenario's app answers), and
  `pkill -f "Tachi.app/Contents/MacOS/Tachi"` matches the user's running app. The unique executable name is
  what makes `pkill -f TachiSmoke` safe — check with `pgrep -fl Tachi`. But that pattern also matches the
  SANDBOX's own build (`codesign --force --deep --sign - …/TachiSmoke.app` carries it on the command line),
  so a stray `pkill -f TachiSmoke` while a run is preparing kills the ad-hoc signing and the scenario reports
  `sandbox — codesign …: signal: terminated` before its app ever launches. Never kill a run in flight: every
  sandbox app shares the bundle id `com.monsterxx03.tachi.smoke`, so the instance it leaves behind is exactly
  what the next run's `open` will activate.
- **Run the suite SERIALLY — one invocation at a time.** Each driver has a 1m30s budget and its assertions
  wait ~3s for a transient state, so a starved webview misses both. An EMPTY `mock-requests.txt` in the
  failing scenario's sandbox is the signature of a starved run rather than a broken app. A failing run keeps
  its sandbox (`artifacts: …`), so they pile up in `$TMPDIR` — inert, but worth clearing when the root volume
  gets tight. Before believing a failure, check `pgrep -fl "itest/desktop|TachiSmoke"` and re-run the
  scenario on its own: the synthetic DRAGS are the flakiest of all, so a lone drag assertion failing in a full
  run is the harness until proven otherwise.
- `open -a <path>` matches by BUNDLE and `pkill` by executable path; a capture of the wrong window looks
  plausible, so confirm the shot is yours (titlebar / session id).
- **A capture is not part of the suite**: the window-capture APIs are gone on this OS (`-R` fails on
  macOS 26) and the window would have to be on the visible Space anyway — a "screenshot" that silently
  returns the desktop is worse than none. The suite keeps `dom.html` + the assertion lines; take a picture
  by hand when you want one.
- **A driver must not click an action that hands the screen to another app**: 打开会话目录 launches the real
  Finder — a window on the user's machine, outside the sandbox, taking the foreground — so the sidebar
  scenario asserts the menu item is there (and that its own mouseleave rule closes the menu) and leaves the
  click alone. The path it would hand over is pinned where the argument is known:
  `TestOpenSessionDirOpensTheSessionDirectory` stubs `openFile` (attach.go) and reads the argv. Any
  "open in …" action has this shape — assert the control on screen, pin the argument off screen.
- **A probe only measures what it reads at the right moment, and only ever proves one direction**: content
  that grows AFTER a pin (a mermaid diagram finishing its async render) is pinned again from a
  `ResizeObserver`, which the browser runs after layout and before paint — so a read taken BEFORE that pin
  reports a gap nobody ever saw. Reading `scrollHeight` FORCES layout, so a `requestAnimationFrame` callback
  is exactly such a read. Read from the driver's OWN `ResizeObserver` instead — observers are called in
  registration order, so one registered after the app's runs after its pin and still before the paint — and
  keep a timer series as the fallback. Then verify the judge BOTH ways: 0 with the fix in place, and the drift
  with the fix disabled.
- **An assertion belongs to the scenario that produces the fact** (and to the line that produces it). A check
  that sat in `oneoff-footer`'s `after` while the driver producing the fact is `oneoff-panel`'s read a
  default forever; the fix was moving the check, not the code. Its sibling: **a control used as a TRIGGER for
  another behaviour pins neither** — a button and a page switch must not ride on one click.
- **A transient state belongs to the trail; a steady state belongs to a direct read.** A ~10ms sampler that
  catches the middle of a state machine can be starved while the webview is busy, so its last tick may never
  see the state that persisted. Read the END state off the element the wait returned.
- **The console is part of the report**: the driver's `console.error` lines ride in the result body
  (`Result.Console`), and the runner prints them for passing runs too.
- **A proxy signal becomes true before the fact it stands for — wait for the fact the action reads**, not for
  a neighbour that merely correlates with it. The turn's 评审本轮改动 chip exists after its FIRST changed
  file, not after its last, so a click on 「完整 diff」 that waits only for the chip captures a one-file
  `paths` set. And widen a suspect window before you trust a fix: a race you cannot widen is one you cannot
  verify a fix against.
- **A stale element swallows a gesture**: re-query the handle (`.composer-resizer`) for EVERY synthetic drag
  — a drag that lands on a detached node passes against the previous value without dragging at all.
- **A run must not take the foreground either** (`TACHI_DEMO_NO_ACTIVATE=1`, set by `sandbox.go`). Three
  things hold it down, each leaking a ~0.6s foreground flash per launch when missing: `sandbox.go` launches
  with **`open -g`**; the app starts as an **ACCESSORY** app (`activationPolicy()` in `demo.go`); and its
  window is created `Hidden: true` and ordered in by the demo bootstrap with
  `orderWindow:NSWindowBelow relativeTo:0` (on screen but at the BOTTOM of the window stack, so it neither
  becomes key nor covers the user's work). Two traps live in that bootstrap: the show must come AFTER the
  app's main loop starts (a main-thread dispatch before that never runs, so the driver is never injected at
  all) and BEFORE the driver's own wait (a script injected into a loading document is simply lost). An
  interactive hand-run leaves the var unset on purpose, so `open` still brings the window forward.
- **A covered window is no longer a paused run — demo mode keeps the page awake, deliberately.** WebKit reads
  a page's visibility off the WINDOW alone (on screen, not miniaturized, not occluded) and never off which
  app is frontmost — so an ordinary window that another window covers is a *hidden* page: its frames stop and
  its DOM timers clamp to 1Hz. With that, every `waitFor` in a driver runs ~10x slower and a run can post
  NOTHING before the runner's `-timeout` prints `等了 90s 没收到结果` — read that message as *the page was
  throttled*, not as *the driver hung*; the run's own `页面节流已解除` line says whether the switch was lifted
  and names the reason when it was not. `demoEnabled()` therefore lifts it from `runJsDemo` through
  `keepPageAwake` (`desktop/webview_awake_darwin.go`: `_setWindowOcclusionDetectionEnabled:NO` +
  `_setHiddenPageDOMTimerThrottlingEnabled:NO` — private SPI, declared locally, applied only after a
  `respondsToSelector:` check). Two things about that call are load-bearing: it must run on the MAIN thread,
  and it must WAIT for the window (the NSWindow is created on the main thread from the Wails window's own
  `run()`, so `keepPageAwake` retries the two transient answers). Three things keep the result honest: the app
  hands the outcome — and, on failure, its reason — to the driver (`window.__tachiDemoUnthrottled` /
  `__tachiDemoUnthrottleError`, riding in the SAME `ExecJS` call as the driver) and the harness asserts it on
  EVERY scenario (`页面节流已解除`), so a Wails/WebKit update that drops the SPI fails on one readable line;
  this is demo-only, since the shipped app must follow the machine's power behaviour; and the window still has
  to be on screen and un-minimized. A driver still must not pace on a *frame*: keep waits event-driven
  (`waitFor` + a macrotask yield), and leave a rAF-paced probe to a hand-run on an idle machine.
- **With Reduce motion ON, EVERY property change is a real transition** (`transition-property` defaults to
  `all`, and base.css's blanket `@media (prefers-reduced-motion: reduce)` sets `transition-duration: 0.01ms
  !important`). 0.01ms is not zero: it creates a `CSSTransition`, whose USED value only advances on the next
  frame. So a driver that writes a style and reads `getBoundingClientRect()`/`getComputedStyle()` **in the
  same task gets the OLD value, no matter how correct the code is**. Wait for the value; never assert it in
  the write's own task.
- **A control run is worth nothing until the build is PROVEN to carry it** (`make frontend && go build
  -o bin/Tachi .`): `npm run build` is `tsc && vite build`, so a control edit that does not type-check fails
  tsc, the `&&` skips the Go build, and the run measures the PREVIOUS binary. Check the freshness
  (`[ bin/Tachi -nt <the edited source> ]`), and disable nothing via a build that can fail silently.

## Desktop Build & Signing (macOS)

- **`make build` leaves the app ad-hoc signed; run `make sign-local` after it.** macOS ties TCC grants
  (notification permission included) to the bundle's code identity, and an ad-hoc identity is a content hash
  — it changes on every rebuild, so a granted permission is forgotten and the native notification path can
  end up refused for good (`Notifications are not allowed for this application`). `sign-local` re-signs with
  the local self-signed identity `Tachi Local Code Signing` (login keychain, trusted for code signing), which
  is stable across rebuilds.
- **Build a cert once with `security` + OpenSSL if the keychain has none** (`security find-identity -v -p
  codesigning` → `0 valid identities found`): `openssl req -x509 -newkey rsa:2048 -nodes -subj "/CN=<name>"
  -addext extendedKeyUsage=critical,codeSigning …`, export with **`-legacy`** (macOS cannot verify OpenSSL
  3's default PKCS#12 algorithms — "MAC verification failed"), `security import … -T /usr/bin/codesign`, then
  `security add-trusted-cert -r trustRoot -p codeSign -k ~/Library/Keychains/login.keychain-db cert.pem`.
- **Notifications only fire while the window is NOT focused** (`desktop/notify.go`): testing with the app in
  front proves nothing — and a no-activate smoke run is *never* focused, so that path is live during a run; it
  stays silent only because the sandbox bundle is ad-hoc signed and TCC refuses the request. The TUI's
  notifications are a different mechanism entirely (`terminal-notifier` / `osascript`) — **but they can still
  be the desktop's fault**: the app is often launched from a herdr pane, inherits
  `HERDR_ENV`/`HERDR_SOCKET_PATH`/`HERDR_PANE_ID`, and then auto-enables the herdr hook (`agent/configureHooks`
  → `hooks.DetectHerdr`), so herdr, not Tachi, raises a terminal notification for a window that has no pane.
  `DetectHerdr` therefore also requires stdout to be a terminal. To check what actually ran:
  `log show --last 10m --predicate 'eventMessage CONTAINS "terminal-notifier"' --style compact` prints the TCC
  attribution with the **responsible** process. `notifyTurnDone` is for the transcript lane only: a
  side-channel run (/review, /commit) has no turn on screen, so it gets its own copy through
  `notifyOneOffDone` — 评审完成 · N 条意见 / 未报问题 / 已停止 / 未完成（见日志）, 提交完成 — raised from
  `commands.go`, where the outcome is known.

## Desktop Backend (bindings & paths)

- **The Wails bindings are GENERATED and COMMITTED**
  (`frontend/bindings/github.com/monsterxx03/tachi/desktop/agentservice.ts`). Adding, renaming or deleting an
  exported method on `AgentService` means re-running
  `cd desktop && GOWORK=off wails3 generate bindings -clean=true -ts -i` (that is `build/Taskfile.yml`'s
  `generate:bindings`, which `make build` runs for you; `-clean` empties `frontend/bindings` first). Never
  hand-edit one: the numeric `$Call.ByID(...)` is derived from the method, and a frontend calling a method the
  generator has not seen fails `tsc` — which is the only thing that type-checks the call at all. Regenerating
  on a clean tree is a no-op, so a diff in that directory is always a real change.
- **Only `desktop/` is a Go module of its own** (it is NOT listed in the repo-root `go.work`), so every Go
  command for it runs with `GOWORK=off`. **The root `go test ./...` therefore does NOT compile `desktop/`'s
  test package** — a signature change under `desktop/` can leave its tests unbuildable with every root-level
  check still green. After touching `desktop/`, run `cd desktop && GOWORK=off go test ./...` too; `make lint`
  at the root does not cover it either.
- **A session's directory is `config.SessionDir()` + ONE path element**: `sessionDirPath(id)` in
  `sessiondir.go` is the single resolver, and it REFUSES anything a webview could send that is not a single
  name (`../..`, `a/b`, `.`, empty). `oneOffDir` shares it — every "where is this conversation stored"
  question goes through that one function.
- **A GUI process's working directory is `/` — never derive a workspace root from it.** macOS launches an app
  with CWD `/`, so anything that falls back to the ambient CWD sees the filesystem root: `wdctx.Dir(ctx)`
  returns the process CWD when the context carries none, so a rewind invoked from an RPC handler (a bare
  `context.Background()`) would rebuild its snapshot manager with root `/` and `git add -A --work-tree=/`
  would walk the whole filesystem. Workspace roots come from the SESSION (`sess.WorkingDir` + `AdditionalDirs`
  — what `/cd` updates and what survives a reload) and go through ONE exit, `desktop/roots.go`'s
  `sessionRoots` / `sessionRootsFrom`: the turn's `wdctx`, the prompt, the `@`-file root, git diff and the
  skill store all read that, so they cannot drift apart after a directory change. A root that is `/`,
  `$HOME`, or one that contains `config.BaseDir()` is refused outright (Tachi's own state — shadow checkpoint
  repos, session records, worktrees — must never sit inside a workspace root). `NewSession` applies the same
  rule (`defaultWorkspaceFor` / `wideRootReason`).
- **A desktop project OWNS its members' workspaces, and the session record becomes a snapshot.**
  `projects.json` (next to `desktop_ui.json`; `desktop/projects.go`) holds `{名称, 主目录, 附加目录}`;
  a member session stores only `project_id`, and every read resolves through
  `(*desktopApp).sessionRootsFrom` — the project's roots win while the project DRIVES the session, otherwise
  the session's own `WorkingDir`/`AdditionalDirs` (creation snapshot) are used, and `GetSessionRoots` flags
  `projectMissing` so the panel can say so instead of showing dead paths. It is ONE predicate
  (`projectTable.drives`) shared by the resolver, the write guards and the panel's `rootsUsable`, so a
  session can never be project-owned for a read and editable for a write: `SetSessionWorkingDir` /
  `AddSessionRoots` / `RemoveSessionRoot` refuse with the project's name while it drives the session, and
  stop refusing when it no longer does (a lost `projects.json` must degrade, not lock the user out). It
  judges the PRIMARY only: a vanished ADDITIONAL root is a fact about one directory — kept in the set,
  reported `exists=false`, skipped by the prompt and the @-file search, exactly as for a session — and
  never a reason to move every member back to its snapshot. Roots are validated on the way in AND on the way
  out (the file is hand-editable), and a project's root can never be wider than a session's. A
  `projects.json` that cannot be parsed reads as "no projects" but is NEVER written over — a save would
  replace a file the user can still repair with just the entry being added — and `upsert` re-reads it first,
  so repairing it is enough, with no restart. Two consequences worth remembering: editing a project writes
  ONE file (no fan-out into session meta, which is why nothing can clobber a running turn's snapshot), and
  the skill store is the exception, since a store's scan roots are fixed when it is built: a project edit
  INVALIDATES every live member (`invalidateMemberSkills`) and each member re-points itself when its own
  next turn starts — the only point that can prove it is not mid-turn. `AgentConfig.RootsFunc` carries the
  same resolver to the agent, so a checkpoint covers the tree the tools worked in rather than the record's
  snapshot; `nil` there means "read the record" and is what tui / acp / channel / `-p` keep doing. Snapshot
  fields travel with the conversation on COMPACTION too: `FinalizeCompact` copies `AdditionalDirs` and
  `project_id` by hand, and one it misses is silently zeroed — for a member that means the child stops
  resolving through the project and works in the snapshot instead (see `agent/compact.go`).
- **A binding is made in exactly two places.** `NewSession(projectID)` is the only SETTER: `""` is an
  ordinary session (unchanged), otherwise the project's roots are copied into the record as the snapshot and
  `project_id` goes in with them; a project that cannot drive a session (deleted, primary gone) yields an
  ordinary session rather than one bound to nothing. `DeleteProject` is the only un-setter, and it DETACHES
  (design §6.2): members keep their workspace, with the snapshot refreshed to the project's roots as of that
  moment, and it is refused while any member is mid-turn — the running check and the meta writes share one
  `d.mu` critical section (`updateSessionMetaLocked`), because a turn starting in between would be writing
  while we rewrite. §7.4's refresh event is still to come with the sidebar (P3), so today the panel a
  frontend already has open does not hear about either operation.
- **Handing a path to the system is `attach.go`**: `OpenPath` (default app; a directory opens its Finder
  window) and `RevealPath` (`open -R`), both `stat`-ing first and returning `"ok"` or the reason. New
  open/reveal actions reuse them, and a test replaces the single `openFile` var to read the argv instead of
  popping a real Finder window.
- **A bash `ask` rule is a question for the USER, and the desktop answers it the way ACP does — the shape
  the TUI confirmation and ACP's `session/request_permission` already use — never with a fourth
  `PermissionMode`**: every session's agent is built with `agent.PermissionModeExternal` plus a
  `PermissionHandler` (`desktop/permission.go`), so `agent_permission.go`'s ask branch parks the turn on the
  window and the decision comes back through `AgentService.AnswerPermission`. `PermissionModeSkip` is the
  wrong posture here: its ask branch is the *unattended* one (channel / subagent / `tachi -p`) — it refuses
  the command and says "add an allow rule", to the user who wrote that rule. `deny` behaves identically
  either way; only `ask` differs. Four properties worth keeping: an answer is addressed by **session + tool
  call id** (`permKey`), because the ids come from the model and two conversations can generate the same one,
  and `AnswerPermission` refuses an id that is not the ask actually waiting; the pending entry is dropped by
  the waiting goroutine itself, so Stop (ctx cancel) cannot leave an approvable orphan behind; a side-channel
  run has no turn on screen, so `commands.go` marks its ctx (`withoutAsk`) and the handler refuses instead of
  parking a run nobody could answer; and 「本会话全部允许」 is a **session** decision, not a command memory —
  both frontends flip the same switch (`Policy.AllowAllAsksForSession`), so later asks never reach either
  frontend at all (an agent's shell commands almost never repeat verbatim, so remembering the approved one
  was a button that could not deliver; the TUI's prompt labels the choice `[a]lways(session)`). The switch
  sits BELOW the deny checks in `CheckBash` on purpose — "stop asking me" must never become "run what I
  forbade", and `perm-session` pins both halves.

## Desktop Skills (the store belongs to the SESSION, never to the process cwd)

- **Skills are ON.** `DisableSkills` is the knob for the NON-interactive modes (`tachi -p`, `tachi commit`);
  this frontend has a human in front of it.
- **The store is built per session, from that session's primary workspace root** (`sessionSkillStore` →
  `sessionPrimaryDir` in `agent_driver.go` — the same resolver the prompt and the tools use, never the record
  read directly) and handed to the agent as `AgentConfig.SkillStore`. The DEFAULT path is the trap:
  `initSkills()` builds `skill.NewStore(config.FindProjectRoot())` — the PROCESS cwd — and a Finder-launched
  app's cwd is `/`, so a default-built store would scan `/.tachi/skills`, offer no project skills at all, and
  point `Skill create`'s `source: project` at the filesystem root. Same shape as the system-reminder trap
  (`config.FindProjectRootFrom`, `systemreminder.workDir`): a per-session thing resolved from a process
  global.
- **A store's scan roots are FIXED when it is built** (`skill.Store` re-reads the disk on every `List`/`Load`
  but never re-resolves its directories), so a session that MOVES has to be re-pointed — and the re-pointing
  happens in `beginTurn` and NOWHERE ELSE. A folder change (`SetSessionWorkingDir`) and a project edit
  (`invalidateMemberSkills`) only set `sessionRun.skillsStale`: both run on the UI goroutine, which cannot
  prove the session is not mid-turn, and `ReloadSkillsIn` rewrites `Config.SkillStore` and the tool registry
  under whatever is reading them. The session's own next turn is quiescent by construction, and nothing but a
  turn reads the store in between, so the deferred swap is invisible. Without it the session keeps serving
  the old tree's project skills — and sends `Skill create`'s project target there. The reload also clears the
  activation state, which is correct: the same name may resolve to another file after the move.
- **The scope is the session's git root, plus the global dir** (`config.GlobalSkillsDir()` = `<base>/skills`,
  note NOT `<base>/.tachi/skills` — the two shapes differ, and a test fixture written for one is invisible to
  the other). **Additional roots are NOT scanned**: they are extra places to read and write, not a second
  configuration home.
- **There is no `/skill` command and no skill UI here**: the shared registry's `Modes` for it exclude
  `ModeDesktop`, so the catalog reminder is what tells the model what exists and the model calls the `Skill`
  tool itself. Adding the command is the registry entry PLUS a desktop handler — its own decision, not a side
  effect of turning discovery on. Until then the catalog's closing line ("or the user can type /skill-name")
  is a dead end in this frontend: `RunCommand` answers 「未知命令」 for that form and 「desktop 暂不支持
  /skill」 for the bare one.
- **The `skill-catalog` smoke scenario pins the scan**: its fixture tree carries
  `.tachi/skills/smoke-skill/SKILL.md`, and the assertion is on the REQUEST (`SMOKE-SKILL-MARKER` found in
  the first request's skill catalog) — nothing in the UI names a skill until one is used. The driver's half
  asserts the injection disturbed nothing: a turn ran, no tool card, no permission card.

## Desktop Turn History (the interrupted-turn merge)

- **A turn's history is the CONVERTED form, reminder blocks included.** `runHistory` holds `[]llm.Message` as
  `agent.ConvertSessionToLLMMessages` builds it, and that conversion re-attaches the `<system-reminder>` block
  to the user message it was stored with — deliberately, since `historyHasReminder` reads the prefix to avoid
  re-injecting first-message-only reminders.
- **So anything that MOVES a message between turns must unwrap it first.** The one such site is
  `mergeTrailingUserMessage` (`agent_turn.go`): an interrupted or killed session can leave the history ending
  on a user message, and that message is folded into the next turn's text so the provider never sees two
  consecutive user messages. Merging the content verbatim would carry the PREVIOUS turn's block along — the
  model would read a stale plan id, stale diagnostics and a stale branch as if they were current, and because
  the merged text is recorded as the new message the block would walk forward through the session turn after
  turn. It now strips via `systemreminder.UnwrapUserMessage`, the single implementation of that inverse.
- **A trailing message that is nothing but a block is left alone — deliberately.** Two shapes look identical
  there: a reminder the loop injected mid-run (`injectLoopReminders` appends one as a user message, so a Stop
  landing right there leaves it trailing), which is stale scaffolding; and a trailing artifact reminder, which
  `ConvertSessionToLLMMessages` appends as its own user message ON PURPOSE so a reload cannot drop it (an
  `/research` or `/review` finding must still reach the model after a restart). No marker separates them, so
  dropping the message would break the second to tidy up the first. Do not "finish the job" here without a
  way to tell them apart.
- **That state is reached by RELOAD or by a well-timed Stop, not by an ordinary Stop**: a stop mid-stream has
  already recorded the partial assistant text, so the history ends on an assistant message and no merge
  happens. What produces a trailing user message with real text in it is a session reopened after the reply
  was never recorded (the process was killed, or the turn hard-failed) — which is why no smoke scenario covers
  it, and why `desktop/agent_turn_test.go` builds its fixture through the REAL conversion instead of
  hand-writing a history shape.

## Desktop UI State

- **A turn's changes are read from its CHECKPOINT trees, everywhere they are shown.** The footer chip
  (`N files +x −y`), 「完整 diff」 and 「评审本轮改动」 all go through `GetTurnChanges`/`reviewScopeFor`
  (`desktop/turnchanges.go`), which diffs the turn's recorded start and end trees — so a file a SHELL
  command wrote counts, created/deleted files are exact, and the answer does not move when the working tree
  does (a commit no longer empties the review's subject). The tool-call fragments (`turnDiffStat`) are only
  the FALLBACK for when there are no checkpoints, and the surface must say which source answered
  (`TurnChangesVO.Source` / `TurnDiffVO.Source`): the same number means two different things. Four traps come
  with it. (1) The turn's end snapshot must be taken BEFORE the `turn_complete` event (`emitTurnComplete`),
  or the footer reads nil and silently falls back — and it must run on a context DETACHED from the turn's
  cancellation (`context.WithoutCancel`), because the error/cancel exit is a stopped turn whose context is
  already dead, and git on it fails instantly. (2) A refused end (`Diff.Skipped`) arrives as a note and
  ZEROES: the chip must fall back to the tool-call numbers rather than take them as the answer, or a turn
  that DID change files renders no footer at all (the chip, 「完整 diff」 and 「评审本轮改动」 all live in
  it). (3) A refused end releases only its own end ref: the START ref is the turn's rewind point AND the
  parent `parentRef` names for the NEXT writing turn, so dropping it makes `commit-tree -p <ref>` fail, and
  every turn after that loses its checkpoint too. (4) A turn whose changes came from a shell command has NO
  tool-declared paths, so anything that skips work on an empty path list hides exactly that case (the
  `TurnDiffOverlay` fetch keys on the turn, not on the paths). A file the working tree no longer has where
  the turn left it is marked 「之后又改过」 rather than shown as if the panel were the disk.
- **Every path in a diff carries the ROOT it came from.** A session can hold additional workspace
  roots (`Session.AdditionalDirs`), and the paths inside each diff are RELATIVE to it — so a file
  without its root resolves against the primary one, and a same-named file there is what 预览/打开
  would show, silently. `FileDiffVO.Root` (absolute) + `RootLabel` ("" for the primary, else the
  base name, or the full path when two roots share a basename — `rootLabels`, the SAME rule and
  strings as `AtMatch.root`, so two roots are told apart everywhere and not in two different ways),
  and the panel resolves `abs` from the file's own root. Three traps: (1) a group's identity for
  React keys and fold state is `(root, path)` — with the path alone, folding one root's file folds
  the other's (`diff.tsx`'s `keyOf`); (2) `GetTurnDiff` diffs EVERY root — a path in an additional
  root is in a DIFFERENT repository, not "outside the repository", and judging it per root reported
  every such path as belonging nowhere; (3) `checkpoint.Manager.TurnDiff`/`ChangedSinceTurn` return
  per root (`RootDiff`/`RootChanged`) and must never be merged into one text or one path set, which
  is also why the review's scope groups by root (`ReviewOptions.ScopeRoots` → `AppendReviewScope`).
- **A review's PANE reads the same diff the reviewer read, by TURN.** The run record keeps the turn
  (`OneOffKeyTurn` → `OneOffVO.Turn`, set from `reviewScope.Turn`), and the findings pane fetches
  `GetTurnChanges(sessionId, turn, …)` — so a review of changes that were committed, or deleted, since
  still shows the files it read, with its findings anchored. Two traps: the pane used to fetch by the
  recorded PATH list alone, which is the working tree against HEAD (an empty pane whose findings all fell
  into 「不在本轮差异里」); and 「重新评审」 used to hardcode turn 0, so re-running from the pane was refused
  with 「工作树里已经没有未提交的差异」 exactly when the frozen pair made it possible. The `frozen-panel`
  scenario pins both (turn 2 deletes what turn 1 wrote, and the pane must still show it).
- **A rewind is only offered for the conversation the reader is still IN — a compacted-away session is
  refused.** Compaction does not rewrite the session it compacts: it starts a NEW one that continues from a
  summary (`agent/compact.go`), so the predecessor's checkpoints describe the state before that point while
  the live conversation is the successor. Rewinding there would move the workspace backwards under a
  successor that has kept writing since, leaving two sessions whose checkpoints describe inconsistent trees
  and neither knowing it. `AIAgent.rewindBlockedByCompaction` finds the successor from the session's own link
  OR from a session naming it as parent (`compact.go` writes the predecessor's side BEST-EFFORT, so the scan
  is not redundant), and the refusal is raised by the PREVIEW (`RewindPreview.Blocked`) as well as the action:
  the card then says 「不能回退：…」, disables 回退 and lists no files. Refusing only on confirm would have
  built a card offering a rewind that cannot happen. `compact` pins both the card and the filesystem half
  (a refused rewind leaves no `rewound/` sidecar).
- **The transcript's turn stamps cover every record of a turn, not only its opening one.** `sessionTurnStamps`
  (`desktop/turnchanges.go`) keys turns by the record that BEGINS them; `buildSessionMessages` resolves that
  to "the turn in force at this record" (`stampBefore`) and stamps every record of the page with it. A page is
  the newest `sessionPageSize` records, so a long turn's opening record sits on an EARLIER page — resolving
  the boundary exactly leaves the card of exactly the longest (shell-heavy) turns without its turn when the
  transcript is reloaded: no chip numbers, and a diff panel / review scoped by turn 0. The frontend reads the
  stamp off the record that starts the card (`buildTurns`), keeping the carry across user/reminder records for
  the live path.
- **A turn's process is folded by the CONVERSATION, not by the part renderer**: `turnView()` (`transcript.ts`,
  pure, shared by the live view and a rebuilt transcript) decides what a turn shows — one strip
  (`ProcessStrip`, `components.tsx`) standing in for its thinking blocks, tool cards and intermediate
  messages, the turn's LAST prose, and the parts that must never be hidden: a FAILED call, the call a
  permission card is parked on, the call an AskUserQuestion form waits on, and notices. **A DELIVERED file is
  a class of its own**: a `SendFile` call that succeeded IS the file card, so `turnView` puts it in
  `attachments` and App renders that group at the turn's TAIL (`.turn-files`, after the conclusion) — behind
  the fold it was not merely collapsed but ABSENT from the DOM, so 「把 X 发给我」 hid its own answer behind
  「展开时序」. It is never in the timeline as well (one card, one place, or two cards read as two files), and a
  turn whose only tool call was the send then has nothing folded left, so it shows no strip at all.
  **Intermediate prose
  (any but the LAST `text`) is folded whether or not the turn failed** — an exposed failure card does not drag
  its own round's text out with it, and the recall affordance is the strip's 「含 N 段过程说明」; `transcript-fold`
  pins both directions (a failing turn and a failure-free control turn). **A steer splits the turn**, and
  therefore the fold: `injectSteerVisual` seals the currently streaming segment and opens a fresh assistant
  message for what follows, so each segment folds on its own — the prose before a steer becomes THAT segment's
  conclusion, step counts are per segment (N steers = N+1 strips), and a rebuild splits in the same place (the
  steer is a user record with `iteration > 0`); `steer-fold`'s Go half also proves the steered text reached the
  model as the next request's user message. A call that is merely RUNNING is not among the exposed: the live
  row reports it (`processLiveLine`), and a call parked on a form is waiting rather than running. `TurnPart`
  stays atomic because `oneoff.tsx` replays the very same parts and exists to show them all. The open/closed
  state is per turn and presentational (never persisted), and the footer's diff chip opens the fold when it
  opens every diff, since those diffs live inside the folded cards. Smoke cost: a scenario that asserts a
  *successful* tool card must expand the strip first (`.process-head`), and "no tool cards" is no longer
  evidence of "no tool calls" — assert the strip's absence too; `perm-deny` needs neither (a denied call is a
  failed part, and failures never fold); and an ATTACHMENT card is the exception to the expand-first rule —
  it is on screen folded, so wait for it directly (`file-find`).
  **Do not reintroduce an "activity row" above the composer**: one
  existed, appeared and vanished once per step, and shoved the message area up and down; if it ever comes
  back, it must hold its place for the whole turn. **Do not compensate a layout change from a
  `requestAnimationFrame`** — a covered window makes WebKit stop the page's frames outright, and a
  `setTimeout` is throttled just as unpredictably. Compensate in a LAYOUT effect instead: the new content is
  in the DOM and the adjustment lands before paint. This is how the fold toggle keeps a bottom-following
  reader pinned — without it, expanding a turn slides the view up and auto-follow switches itself off.
- **A row that alternates between two states must not change GEOMETRY — only its content may.** The
  process row flips between the live line and the summary once per tool call, so anything that differs
  between those two states changes the row's height that many times, and the transcript is bottom-pinned:
  the visible content jumps with it. A `border: 1px` on the live state alone was exactly 2px of that
  (24px live vs 22px static, twice per step for the whole turn); the ring is a `box-shadow: inset 0 0 0
  1px` now, which draws the same edge without taking space — the same reason the codebase paints every
  focus ring with `box-shadow`. `transcript-live` samples `.process-head`'s height every 10ms across a
  two-call turn (live → between calls → live) and requires ONE value, plus that the row never disappears
  mid-turn; its fixture pauses before the second call so the "between calls" state is reachable at all.

- **The titlebar is `App.tsx`'s `<header className="titlebar">`**: brand, the sidebar toggle, the session id
  (click to copy), then `titlebar-right` — the side-panel toggle and the theme switch, held at the far edge
  by one `margin-left: auto`; a new titlebar control belongs in that cluster. **The agent state is not echoed
  in the chrome**: 运行中 already reads from the sidebar row's `spin-dot` and the reply's typing dots. The
  removed status badge took its own styles with it — `STATUS_META` (`types.ts`), `.status-badge` / `.dot*` /
  `.status-label` / `dot-pulse` (`layout.css`) and the reduced-motion exemption (`base.css`) — so a status dot
  that comes back starts from those, not from a bare node.

- **The context ring is ANCHORED on the last call's real prompt size, not on the character estimate**
  (`desktop/contextinfo.go` → `AIAgent.LastInputEstimateWithBreakdown`). The estimate
  (`agent/token_estimate.go`) is character-class based and its bias depends on the content: it over-counts
  plain English and UNDER-counts mixed CJK + JSON (whitespace is charged nothing). So the reported number is
  the last call's real prompt size (`llm.PromptTokens`: input + cache read + cache creation for Anthropic;
  the total alone for OpenAI-family, whose `prompt_tokens` already contains the cache reads) scaled by the
  estimate's movement since that call — the bias then applies to one turn's additions instead of the whole
  prompt. Two consequences worth keeping: the breakdown in the popover is scaled to that total
  (`Breakdown.ScaleTo`, because the parts must add up to the number above them — `ctx-ring` asserts they agree
  within 0.5 points), and a session restored from disk anchors on the last recorded message
  (`estimateFromMessages`), so a reload is not a step backwards. The popover's own caveat has to follow: it
  must not call the number a local estimate and NOT API usage once it is anchored. The correction is a RATIO
  of the estimate's movement applied to the anchored real value, not an additive delta with a floor: after a
  compaction the history is REPLACED by a summary, and a floor would leave the ring sitting on the
  pre-compaction number — the one number compaction exists to bring down (`compact` asserts the meter falls).
  The auto-compact THRESHOLD reads the anchored value too (`shouldAutoCompact`), so the trigger and the meter
  agree; the cooldown is the one comparison still on the raw estimate, and rightly so (it measures the
  conversation against itself, where the bias cancels). A smoke scenario about the meter, or about
  AUTO-compaction, must give the mock a plausible prompt size (`textStreamPrompt`): a fixed 1200-token report
  next to a 30k-token history is a state the app cannot reach in production.

- **The context ring measures the PROMPT of the last call — a reply enters it only on the NEXT turn**
  (`agent/token_estimate.go` + `desktop/agent_model.go`): the estimate is computed before each API call, so a
  turn that ends in a plain reply leaves the meter describing the history *without* that reply, and a
  compaction of a small conversation is invisible under the system-prompt/tool-schema floor. `compact`
  therefore puts TWO turns in front of the compaction: turn 1's reply is what turn 2's prompt measures. A
  scenario about "the context got smaller" must fill the window first, or it asserts nothing.

- **A number that "follows the turn" lags a long turn — the context ring must follow each API CALL**
  (`desktop/agent_turn.go` + `desktop/frontend/src/agentEvents.ts`): the ring's estimate was read only on
  mount / new / switch / `turn_complete` (through `refreshProvider`), while the popover fetches
  `GetContextInfo` when it opens — so during a long turn the ring sat at whatever the turn started with while
  the popover at the SAME moment already showed the current value, and switching sessions only "fixed" it
  because switching happens to refresh. `emitUsage` already ran after every API call, so the estimate now
  rides that event (`agent:cost` carries `contextEstimate`/`contextWindow`, from the one `contextUsageOf`
  rule `GetProviderInfo` shares) and the frontend keeps it in `useSessionUsage`, re-reading by id
  (`GetContextInfo`) only for a session that has not run a turn in this process yet. Two lessons: "the turn
  ended" and "the call ended" are different refresh points, and two surfaces describing the same fact must
  read it from ONE rule. Pinned by `ctx-ring` (a `sleep 4` tool holds the window open and the ring is read
  while the tool runs).

- **The sidebar is `sessionRow` in `App.tsx`**: the conversation list, the folded compaction chains
  (`sessionRows` in `lib.ts` supplies the rows), rename, and the row's right-click menu (打开会话目录 /
  重命名 / 删除). A new row-level action belongs there rather than in a second list component — and one that
  hands the screen to another app is asserted, never clicked, in a driver.

- **A session list is a list of CONVERSATIONS, not of session dirs** (`sessionRows` in
  `desktop/frontend/src/lib.ts`): a compaction chain shares one title, so rendering `ListSessions` raw shows
  the pre-compaction session as a second, identical-looking row. The shape is derived from
  `compactedParentId` (`SessionInfo`, from `meta.json`), folded under the newest link and closed; the
  ancestors stay clickable history.

- **Deleting a session with a turn in flight is REFUSED, by the backend** (`DeleteSession` in
  `desktop/agent_session.go`): the running turn owns a goroutine whose session writes (`AppendMessage` —
  `O_APPEND` with no `O_CREATE`, so it just fails once the directory is gone) and run-map writes
  (`setSessionState` → `getRun`) are keyed by nothing but that id, so deleting underneath it loses the
  transcript silently, keeps the model running and the tools firing, and can rebuild a directory holding
  `meta.json` but no `messages.jsonl` — a phantom row in the sidebar that opens empty. Stopping is therefore
  the user's explicit call, and the refusal (`refuseDeleteRunning`) names it. Two rules ride with it: the
  running flag is read and the run removed in ONE critical section (a turn starting between the two would be
  dropped from the map mid-flight), and the frontend renders the reason in the confirmation box that raised
  the delete while the menu's 删除 entry is `disabled` for a running session — a `.catch(() => {})` here would
  read as a no-op. Covered by `TestDeleteSessionRefuses*` and the `delete-running` smoke scenario.

- **Following the bottom must survive async height changes, not just message updates**
  (`desktop/frontend/src/App.tsx`): the transcript pin ran only when `msgCache` changed, so anything that grew
  the content afterwards — a mermaid diagram finishing its async render, an image/attachment card loading, a
  tool card expanding — slid the visible content up by exactly that height until the next delta pinned it
  back. The fix is a `ResizeObserver` on a `.chat-content` wrapper (the scrollport's own box never changes
  when its content grows) that re-pins while following. Any new "sticky bottom" behavior must go through the
  same observer.

- **A width bound must subtract every other fixed column — and it must limit what is SHOWN, not what is
  STORED**: `.oneoff-panel` exists so the conversation keeps a real reading width, but computing its ceiling
  from `window.innerWidth` alone forgets the 280px sidebar. Read the sibling's REAL width from the DOM
  (`panelRoom()` in `frontend/src/oneoff.tsx`, so a folded sidebar hands the room back) and re-measure on
  `resize`. Then keep the two numbers apart: App holds the width the reader CHOSE (persisted), the panel
  derives what there is room to show (`clampPanelWidth(chosen, room)`) — writing the clamped value back to
  state makes "widening the window gives the width back" true only on the next launch.
  **The conversation's share is a FLOOR plus a SHARE (`CHAT_MIN_WIDTH` 560, `CHAT_MIN_SHARE` 0.5), and the
  share is the half that is easy to leave out**: a bare floor pins the conversation AT the floor and hands
  every extra pixel to the panel — on the default 1200px window that was 480px of prose beside a 440px
  panel, and on a 1600px one 480 beside 840. With both, 1200 gives 560/360 and anything past ~1600 splits
  evenly. `oneoff-panel` asserts the two facts (prose ≥ 560, panel ≤ half the content area) rather than
  recomputing the formula — but only the FLOOR is really pinned there: at the smoke's 1200px window the two
  halves of the rule agree (560 of 920), so deleting the share would not turn that run red. The share earns
  its keep past ~1600px, which the suite cannot stage. **And the reading column itself is `--reading-width`
  (860px) — the same measure `.viewer-doc` gives a previewed document**, so a reply and a previewed file are
  read at one size: a change to either is a change to both.

- **Anything cached per run or per session must be KEYED by it**: key the cache with `sessionId/run` (a
  record's name is a timestamp within its session, so two sessions can hold the same one), and prefer the
  run's OWN record over anything a caller passes in: `agent.OneOffKeyPaths` is per run by construction, while
  a caller's idea of "which turn was this" is not. **The key says what is IN HAND, never "we asked once"**:
  the report pane's text is a file, and its PATH is recorded when the run starts (it is what the round's
  prompt tells the model to write) while the file arrives at the END — so a read taken in between finds
  nothing, and storing that under the run's key freezes the pane for good (「review 过后，点击报告页，是空
  的」: 「报告是空的」 over a report written a moment later). Store only content; keep the reason in a second,
  equally keyed state (`reportMissNote` in `oneoff.tsx`: missing / unreadable / not text / genuinely empty),
  because an empty pane cannot tell them apart — the debt `diffError` pays on the diff's own surface. The
  read is driven by the PANE (tab + run + liveness, with the cleared key as the retry), so switching runs and
  the run ENDING both re-read, and ⟳ drops what was read instead of leaving a failure on screen.

- **A panel opened BY an event cannot assume what that event announces is on disk yet**: the run's `start` is
  emitted BEFORE the recorder is opened, so the side panel's own list read comes back empty — and with
  nothing selected the detail poll has nothing to ask for, so a whole review hides behind 「这个会话还没有
  旁路运行（评审、提交）」 until it ends. The live follow therefore re-reads the LIST too and takes the record
  that appears as the one it is on (`seenNewest`, compared by name, so the reader's own pick in the switcher
  is left alone). Both halves are pinned by `oneoff-report`, whose first checks are red when only the list
  half is missing and whose report checks are red when only the read is.

- **The lightbox's fit() measures the WINDOW, not the stage** (`desktop/frontend/src/viewer.tsx`): the stage is
  a flex item with `margin: auto`, so it hugs its content instead of filling the overlay — fitting to that box
  can only shrink (measured: a diagram 708px wide in the message gave a 336×53 stage inside a 1200×780
  lightbox, so fit chose 0.245× and the diagram opened 73px wide, smaller than the message it was clicked
  from). `viewerArea` reads the overlay's padding box instead. Two more traps come with the same feature:
  (1) a mermaid SVG carries `width="100%"` plus its natural size in its own inline `max-width`, and inside a
  `width: max-content` paper that percentage falls back to the replaced-element default (300px) — so "1:1"
  meant 300px until `MermaidViewer` copies that max-width into an explicit width; (2) **`getBoundingClientRect`
  inside a CSS-zoomed subtree is in PRE-zoom units** in this WebKit (probe: at zoom 3 the paper reads 303
  while its unzoomed parent reads 909), so a reading of "how big is it on screen" must multiply by the zoom —
  `mermaid-zoom` computes it that way, and its first version was fooled by exactly that into thinking a 0.245×
  diagram was 298px wide.
- **⌘F searches a SURFACE, not the page** (`desktop/frontend/src/find.tsx`): the diff pane, a previewed file
  and a review's report each host the find bar, and each searches its OWN subtree — ⌘F in a preview must not
  scroll the transcript behind it. The side panel is ONE host covering its three panes (过程 / 意见+diff /
  报告), so the pane decides what "here" means. Four rules ride with it:
  · hits are painted with the **CSS Custom Highlight API** (`::highlight()`, two names registered in
    `base.css`), never by wrapping text in `<mark>` — these panes re-render under the reader (a live run's
    record is re-read every second, a report fills in when it lands), and a DOM mutation would be thrown
    away by the next render; a re-render only invalidates the ranges, which a MutationObserver collects
    again (debounced: one text walk per streamed token is work nobody asked for);
  · the key owner is installed ONCE at **module load** (window, capture). A bar may mount after the overlay
    it sits inside, and among capture listeners on the same target the first registered runs first — so a
    later-installed bar would lose Esc to the overlay. It consumes Esc with `stopImmediatePropagation`,
    which is what lets the bar close instead of the viewer behind it;
  · the bar is **mounted only while open on every surface** (`findable && find.open`) and the host handle's
    `call()` always goes through the open path. Branching on "the field already exists" is the trap: a
    mounted-but-closed bar looks open to the handle, `open` never becomes true, and the collect effects
    (gated on it) never run — every count reads 无匹配 while the field and the document look perfect;
  · `findable` is **opt-in per viewer**: an image, or an HTML/PDF frame, has no text in THIS document. The
    shell still owns ⌘F there, so it cannot fall through to the bar of the panel dimmed behind it.
  `file-find` pins the whole interaction (count, painted hits, Enter/Shift+Enter, the Esc ordering) and
  `oneoff-footer` / `oneoff-report` pin the two panel panes.

- **Never run a side effect inside a state updater**: React may invoke an updater more than once for the same
  update (StrictMode does; concurrent re-basing can), so `setState(prev => { fetch(...); return ... })` fires
  the request twice — and an `if (prev[k]) return prev` guard cannot help, because both invocations see the
  same pre-update state. Keep the in-flight bookkeeping in a ref instead.

- **A transient state cannot be waited for — sample the sequence**: a UI state whose length is the WORK's
  length (the review chip's 评审中… lasts as long as the run) makes `waitFor` a coin flip, and a synchronous
  read right after `.click()` sees the pre-Render state because React commits asynchronously. Sample the
  element on a ~10ms timer, dedupe into a trail, and assert the trail CONTAINS the state; the trail then
  doubles as the failure message.

- **A state flag must be ended by the fact that ends it**: `reviewPending` was cleared when `ReviewChanges`
  returned (that call only STARTS the fork) and, in a second rule, whenever the session was idle — but the
  session is still idle in the window between the click and the fork going busy, so the middle state was wiped
  before it could ever be seen, and the chip fell through to 已评审 while being disabled by the very run it
  described. The run's own end event is the fact that ends it.

- **Session-scoped UI state must be keyed by session, or reset on switch**: one React tree renders every
  session, so anything derived from the ACTIVE session silently leaks into the next one — the review notice
  (keyed by the clicked turn's message id), plan/findings (reloaded on switch), the cache-hit ring + cost (a
  new session inheriting the previous one's numbers), and the agent STATUS. Anything fetched per session must
  be applied unconditionally; `if (u)` keeps stale values when the payload is empty.

- **The agent status is per SESSION, and its event names the session**: `agent:state` carries `sessionId`, and
  `useAgentStatus(currentId)` keeps one entry per conversation — a status is never one global value here,
  because the desktop runs every session's turn in its own goroutine (switching sessions cancels nothing). So
  `setSessionState` emits for EVERY session, and the only global surface is the menu bar, which reflects the
  DISPLAYED one (`reflectTray` decides that under the same lock that read `activeID`; `ActivateSession`
  re-reports the session it switches to, so the tray follows the switch too). Emitting only while that session
  was displayed — plus a frontend that kept the one value it received — produces four faces at once: the stop
  control follows the reader into an idle session; a background turn's END is never reported at all, so it
  never clears (and Stop acts on the DISPLAYED session — the dead button); a stale busy state makes the
  composer QUEUE a message instead of sending it; and the review chip waits on a neighbour's turn. Pinned by
  the `session-running` scenario.

- **An entry's data source must belong to the surface the entry opens** (`desktop/frontend/src/App.tsx` +
  `diff.tsx`): the turn footer's 「完整 diff」 promises *this turn* against git HEAD (its own tooltip says
  真实文件行号), so it opens a surface of its own — `TurnDiffOverlay` (`ViewerOverlay` +
  `DiffFindingsPane` with `findings=[]`), whose paths are the click's own: fetched on open, dropped on close,
  so they can never become a stale anchor. The side-channel panel cannot do better: after P4 its diff comes
  from the run's recorded scope (`header.paths`) and nowhere else, because a caller-supplied set outlives the
  run it came from. Two corollaries worth keeping: **a fold default belongs to the surface that shows the
  diff** (the panel's rule "a file with findings opens, one without stays folded" folded EVERY file on a
  surface that has no findings at all, so the promised diff arrived as a list of filenames — hence
  `expandAll`), and **a helper whose only caller is gone goes with it**.

- **The transcript itself lives in `desktop/frontend/src/useTranscript.ts`** (messages per session, running
  flags, the loaded window + its cursor, the frame-batched delta queue). Every mutator takes the session it
  is for — `updateSession` is the one funnel, everything else is built on it — so "this belongs to a
  conversation" is enforced by the signature instead of remembered. App.tsx must not declare its own
  `useState<Record<string, Message[]>>`; a new per-session need is a new op in that hook.

- **A system reminder is a HISTORY artifact, not a live one** (`lib.ts` + `App.tsx`): `Message.reminder` is
  set only by `buildTurns`, from the store's `role: reminder` rows, so the `!` affordance appears on a user
  bubble once the transcript is REBUILT (load / switch / restart) — a turn that has just run shows none,
  however much context the model actually received. Assert injected context at the LLM boundary (the
  `project-context` scenario) or through a rebuild; a driver waiting for `.reminder-head` on a live first
  message waits forever.

- **A rebuilt transcript shows the user's OWN words, not what was SENT** (`lib.ts` + `session.Message`):
  the `@`-file expansion happens BEFORE the agent is called, so a `user` record's `Content` is the file
  inlined between `UNTRUSTED FILE CONTENT` markers while `displayContent` holds what was typed — and the
  bubble reads the latter, falling back to `Content` (which is also the shape of every record written
  before the field existed). **`Content` stays authoritative for the model**: `ConvertSessionToLLMMessages`
  rebuilds the next request from it, so writing the raw text there instead would send a prefix the model
  never saw (the provider's prompt cache gone from that point) and retroactively rewrite what the history
  says it was told. The session TITLE comes from the user's own words too, or a conversation that started
  with `@README.md 看看` gets named after the inlined file (and pays a file's worth of tokens for it).
  **A driver cannot pin a reload by switching sessions** — that serves the in-memory copy, so the bug is
  invisible there — which is why `at-file-reload` seeds the transcript (`scenario.seedMessages`, written
  before the app launches) and asserts the FIRST render: that is what a restart finds.

- **Backend events are subscribed in `desktop/frontend/src/agentEvents.ts`** (`useAgentStatus` /
  `useSessionUsage` / `useAgentStream`) — status, the active session's live numbers, and the agent stream that
  writes the transcript. Each payload names its session; compare it against `currentId` only where the UI
  genuinely means "on screen" (a sidebar row, the error bubble), never to address storage.

- **The composer is `desktop/frontend/src/composer.tsx`** (`useComposer` + the `Composer` element): the input
  box, the @-picker, the "/" palette, the queue of messages typed while a turn runs, and the parked-question
  form. All four share one decision — `route`: send now, queue for the next steer point, or dispatch as a
  slash command — so keep new input/queue behavior there, not in App.tsx.

- **A parked permission is the composer's state too, and its card is matched by tool CALL ID**
  (`composer.tsx` + `App.tsx`): `perms` sits next to `asks` and follows the same rules — keyed by session,
  NOT cleared on session switch (the agent is still parked), moved on auto-compaction, cleared on
  `agent:idle` — and the form replaces the tool card whose `toolCallId` equals the request's (every live push
  of a tool part sets it from `ev.ToolID`). Matching by tool NAME is the trap: a model emits a whole batch of
  calls before any of them runs, so "the newest unfinished Bash card" is usually a different call than the
  one waiting. `agent:permission` carries the agent's own preview rendering (`bashAskPreview` — `$ command` +
  the matched rule) and the form shows it verbatim: parsing it in the frontend would be a second definition
  of a format the backend owns. The form keeps its answer's own state (`busy`/`err`) and clears only when
  `AnswerPermission` returns `"ok"` — a refused one (the run is gone, it was already answered) must stay on
  screen with its reason rather than vanishing into a turn that will never move again.

- **Path tries are byte-keyed: iterate strings by BYTE, not `for i := range s`** (`for i := range s` walks
  rune boundaries, so `s[i]` yields only the first byte of each multi-byte character) — otherwise Chinese
  file names come back mangled from the @-file index (the picker shows `?` and the inserted `@`-reference
  points at a file that does not exist). `pkg/container/pathtrie.go`.

- **A pane's width is its MIN-CONTENT width, and that is set by whichever row cannot shrink**
  (`desktop/frontend/public/chat.css`): in the side panel every diff head row (`.diff-panel-head`,
  `.diff-file-head`, `.diff-findings`) holds non-shrinkable badges/buttons, and a `white-space: nowrap` path
  with the default `min-width: auto` contributes its WHOLE string — so the pane's content scrolls sideways
  and the send bar's ends go off-screen with it. Three things together fix it: `flex-wrap: wrap` on those
  rows, `flex: 1 1 0; min-width: 0` on the path (basis alone is not enough — the automatic minimum is the
  min-content), and `max-width: min(1100px, 100%)` on `.viewer-doc.is-diff`, which the file-preview overlay
  and the panel share. Moving the button to the bar's left end does NOT fix it: the bar spans the content
  width, so either end can scroll away. The user's bubble is the same problem in miniature: a pasted URL has
  no break opportunity, so it takes `overflow-wrap: anywhere` rather than `break-word`, because only the
  former also shrinks the min-content (pinned by `bubble-wrap`).

- **A control the reader uses WHILE a pane scrolls belongs in the sticky bar, not in the header**
  (`desktop/frontend/src/diff.tsx`): the 意见 pane's ↑/↓ walk sits in `.diff-sendbar` (`position: sticky;
  bottom: 0`), the only thing in that pane that stays put — jumping to a finding must not scroll the control
  away, and where you are in the list is not a decision about sending. The cost is `flex-wrap: wrap` on the
  bar: it holds seven controls, and its min-content would otherwise widen the panel past its 419px column.

- **The width the reader drags is NOT React state** (`desktop/frontend/src/oneoff.tsx`): the width lives on
  the panel element, so a `setState` per `pointermove` re-renders the whole panel — the 意见 pane's diff
  included, a couple of hundred rows in the smoke fixture and thousands on a real review — before the browser
  has even laid the new width out. The drag writes `panelRef.current.style.width` straight from the
  pointermove handler and commits to React once, on release (which is also what persists it). A
  `useLayoutEffect` re-asserts the drag's own number after every commit, because a render that happens
  mid-drag must not put React's older width back on screen. Pinned by `oneoff-panel` (press → follow → clamp
  → commit → persist); the *smoothness* has no assertion — a same-task read cannot see it (see the
  Reduce-motion trap).

- **The one-off panel's × promises Esc, and who owns the FIRST press is decided by focus**
  (`desktop/frontend/src/oneoff.tsx`): a column is not a `ViewerOverlay`, so the viewers' dismissal contract
  never covered it. Two rules: `App`'s global handler must not claim every Escape unconditionally (it would
  swallow the panel's — `defaultPrevented` is the handshake), and the panel is a window-level listener that
  refuses only an ALREADY-CLAIMED key, so a viewer or modal still wins by construction (ViewerOverlay claims
  it in the capture phase and stops it there). Both orderings are pinned in `oneoff-footer`, because the
  composer is the common case: `/review` leaves the focus in the composer, so there the first Esc only hands
  the focus to the message area (`.chat`) and the SECOND closes; a field inside the panel blurs on the first.
  The ordering is deliberate: the panel's own input field blurs on the first press, and the composer's Esc
  is the same gesture.

## Desktop Themes (the dark palette)

- **The dark theme is Ayu Mirage-derived and its contrast is low ON PURPOSE — do not "fix" it.** The family
  is that theme's blue-grey (`surface.base` #1F2430, panel #282E3B, panel shadow black @0.2), lifted one
  OKLCH step so the app reads as graphite instead of black (`--bg` #252a35, chroma ~0.022 against a neutral
  ~0.010), with Ayu's *warm* grey editor fg as the CONTENT ink (`--text` #cecdc6) and its cool `ui.fg` family
  as the CHROME ink (`--text-dim`/`--text-faint`, 10.5–11.5px metadata).
- **Every "go" button is ONE flat recipe — do not put the gradient back.** `.send-btn`, `.pending-send`,
  `.diff-sendbar-go` and `.plan-start` share a single solid `--accent-strong` fill: no gradient, no inset
  sheen, no coloured glow (`.new-chat` is the tinted variant of the same idea). Two consequences worth
  keeping: button contrast is tuned in ONE place (`--accent-strong`, which is why the dark theme's sits at
  chroma ~0.11), and **a fill that carries white text always takes the `-strong` variant** — `--red` is tuned
  as INK and puts a white glyph below 4.5:1.
- **Find hits are amber on purpose** (`--find-hit` wash + `--find-hit-current`, a solid plane with dark ink
  painted over the others — base.css, both themes): a hit has to be findable inside a diff, where green and
  red already mean added and removed, and inside prose, where the accent means a link or inline code.
- **The reading surfaces take their sizes from ONE place** (`base.css`: `--fs-prose` 15.5px / `--fs-bubble`
  15px / `--fs-finding` 13px). "The text is too small" is an edit there, not a hunt through four files: the
  transcript's prose, the reader's own message and a review finding all read it, and code inside prose
  follows its parent (0.88em). The scale is deliberately NOT a blanket multiplier — labels, badges and
  metadata are chrome and keep their own sizes, and the panel's diff body stays at 11px so a finding is what
  stands out against the code it is about.
- **What bounds the palette is `--text-faint`'s 4.5:1 floor, not `--text`.** It is the token that lands on
  the *lightest* plane (placeholders, MCP labels, `.file-note` — panel-solid, and panel-2 under the
  picker/CSV headers), so it pins both the ink (~#9ca3b1) and the top of the ladder (~#303541). Any "make it
  lighter/greyer still" edit has to trade against that: raise the planes and the smallest text is what breaks
  first, and `--text` has room to spare. Verify with a WCAG ratio check across every plane (`luminance` +
  alpha compositing), not by eye.
- **`windowBgLight`/`windowBgDark` (main.go) are the same decision in a second place** — the pre-paint window
  colour, which `uitheme.go` also feeds to the native appearance. Move them with `--bg`.
- **One definition per mode, and it lives in `frontend/public/base.css`**: the `:root[data-theme="dark"]`
  block, resolved by the boot script in index.html + `src/theme.ts`. No frontend rule reads
  `prefers-color-scheme`, and no other file carries dark colours — the hljs tokens in `widgets.css` map onto
  the semantic tokens, so a palette edit covers code blocks for free; upward shadows use `--shadow-up`.
- **To SEE a palette change, do not reach for the app window** (window capture is dead on macOS 26). Render
  the tokens instead: extract both blocks from base.css (next to
  `git show HEAD:desktop/frontend/public/base.css` for the before) into a small HTML page whose swatches/mock
  rows read `var(--token)`, one scope per variant, and screenshot it with headless Chrome
  (`--headless=new --force-device-scale-factor=2.6 --window-size=…`). That gives a real A/B of the REAL
  values without a rebuild loop.
