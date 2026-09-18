# Tachi — Desktop Handbook (split out of .tachi.md)

Read this **before changing anything under `desktop/`**.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New desktop lessons belong in THIS file, not back
in `.tachi.md`** — and the rules that ride with that file (freshness in the same turn, convention-not-history,
split + index when a subject grows) apply to every split of it, this one included.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Desktop Smoke Verification** | you changed anything a driver can see (UI, events, a binding, an end-to-end flow) — **start with its routing table** |
| **Desktop Build & Signing** | you build/package the app, or debug notifications/TCC |
| **Desktop Backend (bindings & paths)** | you touch `AgentService` — a method, a binding, a session path, an open/reveal action |
| **Desktop MCP** | you touch MCP authorization, the MCP panel, or the deferred-tools pool/reminder path |
| **Desktop Session Tree** | you touch skill discovery, permissions, MCP profile scope, or anything else that resolves "which tree does this session work in" |
| **Desktop Turn History** | you touch how a turn's history is assembled, merged or continued |
| **Desktop UI State** | you change the frontend: which module owns what, and the bug each convention prevents |
| **Desktop Themes** | you touch colours |

## Desktop Smoke Verification (demo + mockllm)

Desktop-only paths (Wails wiring, rendered UI, end-to-end flows) need the real window: unit tests and itest
cannot see whether a binding's result reaches a card. **Use the in-repo suite** for anything touching desktop
state, events or UI, and add a scenario when a fix deserves a regression test.

```sh
make desktop-smoke ARGS="-run send-now,perm-allow -v"   # the scenarios your change touches (see below)
make desktop-smoke                                      # everything (~45 scenarios, minutes)
make desktop-smoke ARGS="-keep"                         # keep the sandbox after a pass (artifacts)
```

### Which scenarios to run — run the touched ones, not the suite

**Pick from this table; a full run is for a change that reshapes the transcript or the event plumbing
itself.** When two rows apply, run both. `-run` takes a comma-separated list.

| You changed | Run |
| --- | --- |
| transcript rendering / turn folding / process strip | `transcript`, `transcript-fold`, `transcript-live` |
| the live status line, its roll, or a row's geometry | `transcript-live`, `live-roll` |
| a message bubble's own layout (long links, wrapping) | `bubble-wrap` |
| composer, send/queue/steer routing | `send-now`, `steer-fold`, `composer-height` |
| scroll pinning, session switch, load | `switch-load`, `switch-scroll` |
| sidebar rows, grouping, titles, type scale, carets | `sessions`, `sidebar-text`, `switch-load` |
| the stop control / which session a status belongs to | `session-running` |
| projects (groups, binding, panel, palette, forms) | `projects`, `project-palette`, `form-text` |
| permission cards / bash `ask` / session-wide allow | `perm-allow`, `perm-deny`, `perm-session` |
| a turn's *changes* (footer chip, diff pane, review scope) | `bash-diff`, `frozen-panel`, `multi-root` |
| the diff overlay (file index, sections, drag, current row) | `file-index`, `multi-root`, `oneoff-footer` |
| review / commit side panel, report, findings | `oneoff-panel`, `oneoff-footer`, `oneoff-report`, `review-all` |
| rewind / checkpoints | `rewind`, `compact` |
| context meter, usage numbers, auto-compaction | `ctx-ring`, `compact` |
| MCP panel, OAuth, deferred-tools reminder | `mcp-auth` |
| skills discovery / the catalog | `skill-catalog`, `project-context` |
| workspace roots (add/remove, git branch, @-files) | `roots-branch`, `multi-root`, `at-file-reload` |
| a previewed file / the viewer / lightbox / find | `file-find`, `zoom-fit`, `mermaid-zoom` |
| plans, `SavePlan`, the plan panel | `plan-writeback` |
| sending/receiving files or images | `paste-image` |
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
exactly one of the two, so write both.

**Adding a scenario** = `drivers/<name>.js` written against the harness (`smoke.waitFor` — never a fixed
sleep — `smoke.check(label, ok, detail)`, `smoke.finish()`, plus
`q`/`qa`/`text`/`allText`/`type`/`pick`/`click`/`key`/`sleep`) and an entry in `scenarios.go` (the mock's
`steps`, the work-dir `files`, an `after` func for the Go-side checks). Keep both halves small: one behaviour
per scenario, and assert what the user would notice. Then add the scenario to the routing table above.

Fixtures, all because the real path needs something a driver cannot do:

- `extraRoots` — additional workspace roots (`<name>: {<relative path>: <content>}`); the root set is seeded
  into the session rather than added in the UI, because the UI's only way in is a NATIVE directory picker no
  driver can click. Reach a seeded root from the work dir as `../<name>/…`.
- `config` — a YAML block appended to the generated `config.yaml` (how a parked-permission scenario gets its
  `permissions.bash.ask` rule).
- `project` — a desktop project (the primary directory's files + its additional roots): `projects.json` and
  the session's `project_id` are seeded, same native-picker reason.
- `seedMessages` / `seedSecond` — a transcript written before the app launches. A driver cannot produce a
  reload by itself (switching sessions serves the in-memory copy), so this is how a restart-shaped assertion
  is staged.
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
- **An unchanged demo bootstrap is not worth re-deriving.** `TACHI_DEMO_LOAD_MS` (demo.go) is the driver's
  head start, the payload is re-issued for a 10s window (`demoInjectStep` / `demoInjectWindow`) because a
  script injected into a loading document is dropped, and the repeats are safe because the payload guards on
  `window.__tachiDriverStarted` (a scenario reporting TWICE means that guard is gone). The demo starts from
  `ApplicationDidFinishLaunching`, never a sleep: a main-thread dispatch before `App.Run()` builds the platform
  impl PANICS the process, which from outside is indistinguishable from "the app did not start". Leave this
  machinery alone unless you are fixing it.
- **A covered window is not a paused run — demo mode keeps the page awake, deliberately.** WebKit reads a
  page's visibility off the WINDOW alone (on screen, not miniaturized, not occluded) and never off which app is
  frontmost, so an ordinary covered window is a *hidden* page: frames stop and DOM timers clamp to 1Hz, and a
  run can post NOTHING before the timeout. Read `等了 90s 没收到结果` as *the page was throttled*, not *the
  driver hung*; the run's `页面节流已解除` line says whether the switch was lifted. `keepPageAwake`
  (`webview_awake_darwin.go`) lifts it from `runJsDemo` with two private SPIs, applied only after a
  `respondsToSelector:` check — but the call must run on the MAIN thread and must WAIT for the window, so
  `keepPageAwake` retries the two transient answers. The harness asserts the outcome on EVERY scenario, so a
  Wails/WebKit update that drops the SPI fails on one readable line. This is demo-only and the window must
  still be on screen and un-minimized.
- **The console is part of the report**: the driver's `console.error` lines ride in `Result.Console`, printed
  for passing runs too.

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
  front proves nothing. The TUI's notifications are a different mechanism (`terminal-notifier` / `osascript`),
  **but they can still be the desktop's fault**: the app is often launched from a herdr pane, inherits
  `HERDR_ENV`/`HERDR_SOCKET_PATH`/`HERDR_PANE_ID`, and then auto-enables the herdr hook — so herdr, not Tachi,
  raises a terminal notification for a window that has no pane. `DetectHerdr` therefore also requires stdout
  to be a terminal. To check what actually ran: `log show --last 10m --predicate 'eventMessage CONTAINS
  "terminal-notifier"' --style compact` prints the TCC attribution with the **responsible** process.
  `notifyTurnDone` is for the transcript lane only: a side-channel run (/review, /commit) has no turn on
  screen, so it gets its own copy through `notifyOneOffDone` — 评审完成 · N 条意见 / 未报问题 / 已停止 /
  未完成（见日志）, 提交完成 — raised from `commands.go`, where the outcome is known.

## Desktop Backend (bindings & paths)

- **The Wails bindings are GENERATED and COMMITTED**
  (`frontend/bindings/github.com/monsterxx03/tachi/desktop/agentservice.ts`). Adding, renaming or deleting an
  exported method on `AgentService` means re-running
  `cd desktop && GOWORK=off wails3 generate bindings -clean=true -ts -i` (that is `build/Taskfile.yml`'s
  `generate:bindings`, which `make build` runs for you). Never hand-edit one: the numeric `$Call.ByID(...)` is
  derived from the method, and a frontend calling a method the generator has not seen fails `tsc` — the only
  thing that type-checks the call at all. Regenerating on a clean tree is a no-op.
- **Only `desktop/` is a Go module of its own** (it is NOT listed in the repo-root `go.work`), so every Go
  command for it runs with `GOWORK=off`. **The root `go test ./...` does NOT compile `desktop/`'s test
  package** — a signature change under `desktop/` can leave its tests unbuildable with every root-level check
  still green. After touching `desktop/`, run `cd desktop && GOWORK=off go test ./...` too; `make lint` at the
  root does not cover it either.
- **A new direct dependency of the ROOT module must be added to `desktop/go.mod` too.** `desktop/` is a
  separate module that `replace`s the root one, so it carries its own `require`/`go.sum` for everything the
  root code imports. A package the root starts importing (`github.com/goccy/go-yaml`, when `config` gained
  `SavePartial`) builds and tests fine at the root while `desktop/` fails with
  `missing go.sum entry for module providing package ...` — and, because the root `go test ./...` does not
  compile `desktop/`, **every root-level check stays green**. Fix: `cd desktop && GOWORK=off go mod tidy`.
  The reverse also holds: bumping such a dependency at the root must be re-tidied here.
- **`d.cfg` is read under `cfgMu` and written through ONE entry point** (`desktopApp.config()` /
  `withConfig()`, in `agent.go`). It used to be plain-field access, which was safe only while nothing wrote
  it after startup — the settings page is exactly the thing that changes that.
- **⚠️ Never swap the `d.cfg` POINTER; mutate the existing object.** `withConfig`'s comment carries the full
  reason and it is worth re-reading before any "let me just install a fresh config" idea: every provider is
  wrapped by `wrapForUsage`, whose price/credit resolvers CLOSE OVER the `*config.Config` and dereference it
  per LLM call, so a replaced object leaves the usage ledger billing at the old prices; each session's agent
  holds it as `AgentConfig.FullConfig` (26 read sites); the shared MCP manager and its refresher hold it too.
  Those holders read LAZILY, which is what makes an in-place edit take effect everywhere at once — and what
  makes a pointer swap fail everywhere at once, silently. So `atomic.Pointer[config.Config]` is the wrong
  tool here, however tempting it looks.
- **One SAVE is one locked batch.** `withConfig` runs under the write lock, so a multi-field save needs its
  reads to happen under the same lock — which they do: `SaveSettings` passes the whole batch to
  `config.ApplyUpdates` inside ONE `withConfig` call, all-or-nothing, so no reader can ever observe a
  half-applied batch. (`mcpServerByName` is still the one documented exception: a single bool written in
  place, outside the lock.)
- **A config key's "what does it take to make this live" is declared in `desktop/settings.go`, one row per key
  in `settingsTable`** (`applyLazy` = readers already re-read it / `applyPush` = something must be rebuilt,
  `Apply` carries that / `applyRestart` / `applyUnused` = another entry mode owns it). Two tests in
  `desktop/settings_test.go` hold it to the whole of `config.Config` by reflection, so **adding a config field
  fails the build until it gets a row**; `settingApply.needsRestart()` is DERIVED (`Apply == nil` and not
  lazy/unused), never a flag to remember. A row's `Apply` only does the PUSH half — the value itself is
  written into `d.cfg` by the caller — and anything that has to reach every session goes through
  `d.applyToLiveAgents(fn)` (`agent.go`), which returns failures labelled with the session's title (or its id).
  The settings page's two bound methods are `GetSettingsSchema` (the table filtered through the CONFIG's
  leaves — `config.PathsFor` — one field per editable leaf) and `SaveSettings` (resolve every key and refuse the
  whole batch, validate, snapshot + `config.ApplyUpdates` all-or-nothing, push each row's `Apply`, and write
  `config.SavePartial` LAST: a failed write restores the exact snapshot, which is why the snapshot primitive
  exists instead of re-reading through `ReadPath` — that substitutes defaults for unset pointers).
  Values are read and written by the SAME dot path as the file, via `config.ReadPath`/`SetPath`: one grammar
  for the file, the form and the error messages.
  A row's PUSH half is a reload method from `agent/agent_reload.go` (`ReloadTools`, `ReloadHooks`, `ReloadLSP`,
  `ReloadMemory`, `ReloadSubagent`, `ReloadMCPToolRefresh`) or a call into one that already existed
  (`SetResolvedProvider` under the session's own provider name, `SetPermissionRoot`, `Setup*Provider`): each is
  idempotent, does only the push half, and never replaces something a FRONTEND injected
  (`AgentConfig.HookDispatcher` / `.SubagentRunner`, tracked by `hooksInjected` / `subagentInjected`).
  One row is the exception: `active_mcp_profile` sets `OwnsWrite`, because its push IS the writer —
  the batch skips the leaf so a failed switch cannot leave the field naming a profile whose servers
  are not loaded (the desktop's `switchMCPProfileTo` writes it under `cfgMu`, after the reconcile).
  Two invariants the whole family shares: a REMINDER cannot be replaced (the collector only appends), so the
  agent holds `LSPDiagnosticsReminder` / `MemoryRecallReminder` itself and re-points them
  (`SetProvider` / `SetBackend` — nil means off; both setters are synchronized, because they are called from
  the settings goroutine while a turn generates); and a rebuild that owns a child process or an external
  resource tears the old one down FIRST (`ReloadLSP` → `Shutdown`, or its servers would be orphaned).
  A rebuilt subsystem is not identical to a constructed one: `ReloadMemory` must re-apply the two wirings
  construction adds AFTER configure (the dedicated keyword provider, the topic backend's session fallback)
  plus `MemoryState.SkipRecall` — losing them is silent degradation, not an error.
  PUSHES RUN ONLY AFTER `config.yaml` is written: a failed save pushes nothing, which is what makes the
  rollback message true (a push has no undo — log level, rebuilt tools/backends, MCP reconnects). One save
  is serialized end to end by `d.settingsMu`. A push is also never applied UNDER a running turn: the fan-out
  queues it for a session whose `r.running` is set and `endTurn` runs the queue (`desktop/deferred_push.go`),
  which is why a save made mid-turn answers `K 项等这一轮结束生效（…）：会话「甲」正在运行` instead of
  claiming the key is live. Process-wide and shared-layer rows (log level, prompt-cache clear, the manager's
  own limits, a profile switch, the poller) are NOT deferred — none of them swaps a field a turn reads
  unlocked, and their "session" is the displayed one, not the running one.
  MCP rows are shared-layer facts driven ONCE from the app (`appAgentForMCP`), never per session — and
  `mcp_tool_search` re-derives the auto-load set from the pool (`Manager.RecomputeAutoLoad`, which counts the
  same tools `PopulateFromConnect` pools, so a fresh start and a toggle land in the same place) instead of
  reconnecting every server.
  `TestTheLikelyEditsStayHot` fences the keys users edit most: if a refactor makes one of
  them need a restart again, that test — not a confused user — is what fails.
  Design: `docs/2026-09-14-desktop-settings-design.md` §六、§七、§八之三.
- **Closing the main window HIDES it; only 退出 Tachi Desktop quits the app** (`desktop/main.go`, the
  `RegisterHook(WindowClosing)` right beside the window's creation). On darwin a user-initiated close (⌘W, the
  red button) always DESTROYED the window — AppKit's `windowShouldClose` returns NO, the framework emits a
  closing event instead, and its own listener runs `impl.close` + `Window.Remove(id)`. Every handle then
  pointed at nothing while the app kept running in the menu bar, and all three ways back went silent together
  (tray 「打开主窗口」, a tray click, a notification click, a Dock click). Hooks run SYNCHRONOUSLY before the
  listeners and `WindowEvent.Cancel` stops the rest, which is the only lever that turns the close into a hide
  — and hiding is what makes the three paths work again without touching them, the window staying in the
  manager as "exists, not visible" (page, drafted message and streaming turn all survive). The interception is
  on `Common.WindowClosing`, the id the destroy listener is registered on. Two rules ride with it: the
  shutdown path must pass through (`desktopApp.quitting`, set from `app.OnShutdown`), and the hide is
  load-bearing, not cosmetic — the notifier, the theme controller and the tray all hold the window pointer,
  which is only safe while nothing destroys it.
- **A session's directory is `config.SessionDir()` + ONE path element**: `sessionDirPath(id)` in
  `sessiondir.go` is the single resolver, and it REFUSES anything a webview could send that is not a single
  name (`../..`, `a/b`, `.`, empty). `oneOffDir` shares it.
- **A GUI process's working directory is `/` — never derive a workspace root from it.** macOS launches an app
  with CWD `/`, so a fallback to the ambient CWD sees the filesystem root: a rewind invoked from an RPC
  handler (a bare `context.Background()`) would rebuild its snapshot manager with root `/`. Workspace roots
  come from the SESSION (`sess.WorkingDir` + `AdditionalDirs`) and go through ONE exit, `desktop/roots.go`'s
  `sessionRoots` / `sessionRootsFrom`: the turn's `wdctx`, the prompt, the `@`-file root, git diff and the
  skill store all read that, so they cannot drift apart after a directory change. A root that is `/`,
  `$HOME`, or one that contains `config.BaseDir()` is refused outright (Tachi's own state must never sit
  inside a workspace root). `NewSession` applies the same rule (`defaultWorkspaceFor` / `wideRootReason`).
- **A desktop project OWNS its members' workspaces, and the session record becomes a snapshot.**
  `projects.json` (next to `desktop_ui.json`; `desktop/projects.go`) holds `{名称, 主目录, 附加目录}`; a member
  stores only `project_id`, and every read resolves through `(*desktopApp).sessionRootsFrom` — the project's
  roots win while the project DRIVES the session, otherwise the session's own fields (creation snapshot) are
  used, and `GetSessionRoots` flags `projectMissing` so the panel can say so instead of showing dead paths. It
  is ONE predicate (`projectTable.drives`) shared by the resolver, the write guards and the panel's
  `rootsUsable`, so a session can never be project-owned for a read and editable for a write. It judges the
  PRIMARY only: a vanished ADDITIONAL root is a fact about one directory — kept in the set, reported
  `exists=false`, skipped by the prompt and the @-file search. A `projects.json` that cannot be parsed reads as
  "no projects" but is NEVER written over (a save would replace a file the user can still repair), and `upsert`
  re-reads it first. Two consequences: editing a project writes ONE file (no fan-out into session meta, which
  is why nothing can clobber a running turn's snapshot), and the skill store is the exception — a store's scan
  roots are fixed when it is built, so a project edit INVALIDATES every live member
  (`invalidateMemberSkills`) and each member re-points itself when its own next turn starts. `AgentConfig.RootsFunc`
  carries the same resolver to the agent, so a checkpoint covers the tree the tools worked in rather than the
  record's snapshot. Snapshot fields travel with the conversation on COMPACTION too: `FinalizeCompact` copies
  `AdditionalDirs` and `project_id` by hand, and one it misses is silently zeroed (see `agent/compact.go`).
- **A binding is made in exactly two places.** `NewSession(projectID)` is the only SETTER: `""` is an ordinary
  session, otherwise the project's roots are copied into the record as the snapshot and `project_id` goes in
  with them; a project that cannot drive a session yields an ordinary one rather than one bound to nothing.
  `DeleteProject` is the only un-setter, and it DETACHES: members keep their workspace, with the snapshot
  refreshed to the project's roots as of that moment, and it is refused while any member is mid-turn — the
  running check and the meta writes share one `d.mu` critical section (`updateSessionMetaLocked`).
- **Handing a path to the system is `attach.go`**: `OpenPath` (default app; a directory opens its Finder
  window) and `RevealPath` (`open -R`), both `stat`-ing first and returning `"ok"` or the reason. New
  open/reveal actions reuse them, and a test replaces the single `openFile` var to read the argv.
- **Every root in the workspace panel carries its git BRANCH** (`SessionRootVO.Branch`/`Detached`;
  `shutil.GitBranch`): that panel lists the session's directories by path, and two checkouts of the same
  repository are then the same row — the branch is the one thing that tells them apart before a turn edits
  files. It is probed PER ROOT when the panel opens (never stored: a checkout moves under you), skipped for a
  root that does not exist, and EMPTY for one that is not a work tree — "not a repository" is never dressed up
  as a branch name. A detached HEAD reports the short commit AND sets `Detached`. `shutil.GitBranch` is the
  ONE probe: the system reminder's `Git branch` line reads it too.
- **A pasted image is stored, then attached by reference — the same route a dropped file takes**
  (`SavePastedImage`, + `composer.tsx`'s `onPaste`): a screenshot is BYTES with no path, so the composer sends
  them (base64) to a binding that writes them under the SESSION's own directory (`pasted/`, beside `oneoff/`
  and `subagent/`, so the file dies with the conversation and never appears in the user's workspace diff) and
  answers with an ABSOLUTE `@`-reference. Three things to know: the answer is a VO, not "ok"/reason (a paste
  that silently does nothing is the same class of bug as a message that silently goes nowhere); **nothing is
  re-expanded on RELOAD** — the record keeps the expanded text plus the raw display text, so the image reaches
  the model for the turn it was sent with and a later turn sees only the placeholder (true of every
  `@`-image); and one file per paste, hence the short uuid (a timestamp alone collided for two pastes in the
  same millisecond). `paste-image` asserts the pasted BYTES at the LLM boundary — which requires the smoke
  provider to declare vision (`spec.vision: true`, or the agent describes the image instead,
  `agent/vision_fallback.go`) and the assertion to read `RawBody` (the normalized view keeps text only).
- **A send NAMES its session, and a send that does not happen SAYS SO** (`SendMessage` / `StopAndSend`,
  `desktop/agent_turn.go`). Both used to read the session from `d.activeID` and answered `"ok"` for a message
  they never started a turn for. The frontend draws the user's bubble and a running placeholder BEFORE the
  call, so a dropped message left the reader looking at their own text under a 正在执行 nothing could clear —
  and it is unrecoverable, the text never reaching a session file at all. The id comes from the caller now and
  every refusal is a returned string, which the composer renders in the placeholder it opened
  (`finishNotice` + `markRunning(false)`). Two traps: `sessionIsKnown` is NOT a usable guard (it reads the RUN
  map, so it is false for a session that exists on disk and has not been loaded — every first send), and the
  busy check has to come BEFORE the agent build.
- **Every desktop manager is built on ONE `FileStore` (`d.sessionStore()`).** `FileStore.ListSessions`
  memoizes the session list, because the sidebar asks for its rows, then the per-project counts, then one
  project's members, each as a separate call. A manager built any other way (`session.NewManager(nil)`) holds
  a store of its own, whose snapshot nothing can drop: a turn's `AppendMessage → UpdateMeta` runs on the
  per-run manager, and an in-place `meta.json` rewrite moves no directory mtime, so the listing manager would
  keep serving the pre-turn title (`TestEveryDesktopManagerSharesTheSessionStore` is the one that fails).
  `LoadMeta` stays un-memoized on purpose — it feeds the live record and the read half of
  `updateSessionMeta`'s read-modify-write, where a stale read is a lost write.
- **A bash `ask` rule is a question for the USER, and the desktop answers it the way ACP does — never with a
  fourth `PermissionMode`**: every session's agent is built with `agent.PermissionModeExternal` plus a
  `PermissionHandler` (`desktop/permission.go`), so the turn parks on the window and the decision comes back
  through `AgentService.AnswerPermission`. `PermissionModeSkip` is the wrong posture: its ask branch is the
  *unattended* one (channel / subagent / `tachi -p`) — it refuses the command and says "add an allow rule", to
  the user who wrote that rule. `deny` behaves identically either way; only `ask` differs. Four properties:
  an answer is addressed by **session + tool call id** (`permKey`), because two conversations can generate the
  same id; the pending entry is dropped by the waiting goroutine itself, so Stop cannot leave an approvable
  orphan; a side-channel run has no turn on screen, so `commands.go` marks its ctx (`withoutAsk`) and the
  handler refuses instead of parking a run nobody could answer; and 「本会话全部允许」 is a **session**
  decision, not a command memory — both frontends flip the same switch
  (`Policy.AllowAllAsksForSession`). The switch sits BELOW the deny checks in `CheckBash` on purpose — "stop
  asking me" must never become "run what I forbade".

## Desktop MCP

- **`enableMCPServer` is the ONE enable path.** The panel's toggle and the end of an authorization both go
  through it, so an authorized server ends up exactly as connected as a toggled one: tools into the shared
  pool AND `NotifyDeferredToolsAdded` for every live agent. That reminder fires at most once per session, so a
  server connected mid-session is invisible to the model without the dirty mark — a pool write alone is the
  bug (a real session's debug log showed zero `DeferredToolReminder fired` after a mid-session enable). A new
  "connect a server" path belongs INSIDE it. Toggling is a GLOBAL fact (one pool, one connection for every
  session), so all live agents are notified (`applyToLiveAgents()`), and disabling runs `UnregisterMCPServer`
  (registry + every session's discovered set), not a bare pool removal. `.tachi.md` carries the general rule.
- **The MCP panel opens with every server FOLDED** (`expanded` in `MCPPanel`, empty = all folded): a tools
  list belongs to a connected server, so "expand when connected" left an unconnected server showing nothing
  and the same control read as two different ones. The tool rows do not EXIST in the DOM until the row is
  clicked, and a folded row needs its caret. The fold is per-opening, not remembered.
- **A connected server can be TOOL-LESS for a few seconds, and the panel must survive it.** `manager.connect`
  registers a connection as soon as its own handshake + `tools/list` succeed, but the shared pool is filled
  only after EVERY server has answered (`ConnectAll` waits for the slowest) — so "已连接 + 池子里还没有工具"
  is a real window, seconds wide when one server is slow. The panel re-reads on every open (nothing else would
  correct a stale snapshot) and the backend emits `agent:mcp_ready` when the pool is settled, which a panel
  that was ALREADY open needs (`useMCPReady`).
- **A tool row opens its own contract, fetched on demand** (`GetMCPToolSchema`). On demand, not part of
  `ListMCPServers`: that list is up to a hundred rows and is re-read on every panel open. The VO is FLATTENED
  — type resolved to a single string, required marks placed, enum/default/array-element type as separate
  fields, parameters SORTED. An unknown name returns the zero value rather than an error, because the panel
  may be showing a list fetched before that server went away.
- **Tools in the pool are LISTED whether or not this session has loaded them, and the two must be told apart.**
  Under MCP tool search a server auto-loads nothing, so "not loaded" is the normal state and a row that simply
  disappears reads as a missing tool: unloaded rows carry `.unloaded` while loaded ones stay at full strength.
  The action button is never greyed — it is the way out.
- **Tachi answers MCP's `roots` capability with the workspaces of the frontend it is serving.**
  `Manager.SetRootsFunc` makes every connection declare the capability and answer `roots/list` on demand. The
  ANSWER differs by mode because the connection lifetime does: the desktop wires `desktopApp.mcpRoots` on its
  shared manager (one connection serves every session, so the answer is the UNION of their workspaces), while
  ACP wires it per session through `AgentConfig.MCPRootsFunc`. The shape itself (file:// URIs, only directories
  that exist, deduplicated, sorted) is ONE rule, `mcp.RootsForDirs`, for both. It is not optional in practice:
  a roots-aware server CONFINes file access to what it is told, and the official chrome-devtools server, when
  the client declares no roots, allows only the OS temp dir — i.e. a tool that exports a file into the project
  dying on a path check. Asserted from the SERVER's side (`agent_mcp_test.go`'s stub exposes a tool that asks
  for them), because nothing about the exchange is visible from Tachi.
- **`client.Start` is what installs the handler for requests the SERVER sends** (`roots/list`, sampling,
  elicitation) — not `transport.Start`, and not the SDK's convenience constructors. Skipping it is silent on
  Tachi's side and answers the server with "No request handler configured".
- **A panel toggle is written back into the mcp.json the server came from** (`MCPServerConfig.SourceFile`,
  stamped at load with the file that WON a name clash; `config.SetServerEnabledInFile`). The rule is "the file
  it came from", never "the active profile": a server in `mcp.json` and one in `mcp.<profile>.json` are
  different entries in different files, and writing both into either would lose the change on the next profile
  switch. The edit is SURGICAL (one value's bytes; indentation, key order and unknown fields survive), because
  mcp.json is hand-written. The **profile SWITCH stays runtime-only** — nothing writes `active_mcp_profile`
  into config.yaml, so a dropdown cannot silently make one profile the default for every future launch.
- **The panel's per-server state comes from the BACKEND, never from the panel.** `MCPServerVO.NeedsAuth` is set
  wherever a connect attempt came back `*mcp.OAuthRequiredError` — at startup (`populateSharedMCP`) as well as
  on a manual enable — because a server that wants a token otherwise looks exactly like one that is down.
  `desktopApp`'s `mcpAuth*` fields carry their own lock (never `d.mu`: they are written from a background
  authorization goroutine).
- **`AuthorizeMCPServer(name)` starts the flow and returns "ok"; the progress arrives as `agent:mcp_auth`.** It
  has to: the flow needs a browser and often a human, so there is no answer to wait for.
  `CompleteMCPServerAuth(name, url)` is the paste-back path, and it CANCELS an in-flight flow first — both ways
  out spend the same single-use authorization code.
- **The browser is a variable (`mcp.BrowserOpener`), and a scripted run replaces it.** `TACHI_DEMO_BROWSER_FETCH=1`
  (desktop/demo.go, installed before the agent is built) turns "open the user's browser" into one HTTP GET —
  enough against an authorization endpoint that redirects straight back to the callback listener. A smoke run
  must never open a window on the machine it is running on; a demo RECORDING must, which is why this is a flag
  of its own rather than part of `TACHI_DEMO=1`.
- **The callback address is fixed BEFORE anything is registered or exchanged** (`mcp.ensureCallbackAddr`):
  dynamic client registration sends the redirect_uri and the token exchange sends it again, so a port chosen
  after registration hands the two different values. The flow persists its state + PKCE verifier on the BROWSER
  path too, not only the manual one: pasting the redirect URL out of the address bar has to work while the
  callback is still pending.
- **A 401 is one fact however it arrives.** `mcp.isAuthRequired` maps both transport sentinels
  (`ErrOAuthAuthorizationRequired`, `ErrAuthorizationRequired`) to `OAuthRequiredError` at the `Initialize` AND
  the `ListTools` step. Mapping only the later one left an unauthenticated server reporting a plain connect
  error, so no frontend could offer the fix.
- **The fixture is `itest/mcpauth/`, and both halves of the feature run against it.** One process, one
  loopback origin: 401 + `WWW-Authenticate` → PRM → AS metadata → DCR → `/authorize` → `/token` (it refuses a
  verifier that does not match the challenge) → an MCP endpoint that requires the bearer. The desktop's unit
  test drives it in-process; `mcp-auth` starts it in the runner and points the sandbox's `mcp.json` at it (its
  port is only known at run time). `desktop/` reaches the package through its module `replace`: one fake, not
  two. The scenario delays the consent step (`mcpauth.WithAuthorizeDelay` + the runner's `mcpConsentDelay`),
  because a flow that completes in microseconds can only be asserted on after the fact — the panel's WAITING
  state would never be on screen when the driver looks.

## Desktop Session Tree (every path that is per-tree must come from the SESSION)

- **The process cwd is `/` and is never the answer.** A Finder/LaunchServices-launched app gets the
  filesystem root as its working directory, and desktop hosts several sessions in different trees anyway — so
  `config.FindProjectRoot()` (which reads the process cwd, and returns `/` there, NOT "") describes nothing
  useful. Anything that must resolve "which tree does this conversation work in" goes through
  `sessionRoots`/`sessionPrimaryDir` (the resolver the prompt, `wdctx`, `@`-files and checkpoints already
  share). The failure mode is always the same and always SILENT: a lookup under `/.tachi/<file>` finds
  nothing, the global scope still works, so nothing looks broken.
- **`config.FindProjectRootFrom(dir)` is the session-scoped exit**, and `FindProjectRootFrom("")` returns ""
  ("no project") — the correct answer for a session with no folder yet.
- **Three call sites were wrong and are now fixed**; a new one is a bug of the same shape:
  - skills — `sessionSkillStore` (per session, injected as `AgentConfig.SkillStore`). The DEFAULT path is the
    trap: `initSkills()` builds `skill.NewStore(config.FindProjectRoot())`, so a default-built store scans
    `/.tachi/skills`, offers no project skills, and points `Skill create`'s `source: project` at the
    filesystem root.
  - permissions — `AIAgent.SetPermissionRoot` (see below).
  - MCP profile scope — `desktopApp.mcpProjectRoot`, used by `ListMCPProfiles` / `SetMCPProfile`; the project
    `mcp.<profile>.json` lives in `<tree>/.tachi/` and was read from `/.tachi/`.
  - MCP server LOAD — `desktopApp.reloadMCPForActiveTree`, called from every place the displayed tree can
    move (session activation/`LoadSession`, `SetSessionWorkingDir`, a project's roots edit/delete, and the
    end of the bootstrap connect). It re-reads `<tree>/.tachi/mcp.json` through `SwitchMCPProfile`, whose
    diff keeps unchanged connections; the tree that wins is the DISPLAYED session's, so a background session
    in another tree does not hold its own servers (per-project splitting is the project design's P4). A new
    "the tree moved" writer is a bug of the same shape as the three above.
- **Skills are ON.** `DisableSkills` is the knob for the NON-interactive modes (`tachi -p`, `tachi commit`);
  this frontend has a human in front of it.
- **The scope is the session's git root, plus the global dir** (`config.GlobalSkillsDir()` = `<base>/skills`,
  note NOT `<base>/.tachi/skills` — a fixture written for one is invisible to the other). **Additional roots
  are NOT scanned**: they are extra places to read and write, not a second configuration home.
- **There is no `/skill` command and no skill UI here**: the shared registry's `Modes` for it exclude
  `ModeDesktop`, so the catalog reminder is what tells the model what exists. Adding the command is the
  registry entry PLUS a desktop handler — its own decision, not a side effect of turning discovery on.
- **The `skill-catalog` smoke scenario pins the scan**: its fixture carries
  `.tachi/skills/smoke-skill/SKILL.md`, and the assertion is on the REQUEST (`SMOKE-SKILL-MARKER` in the first
  request's skill catalog) — nothing in the UI names a skill until one is used.

### Tree-derived state: ONE stale flag, re-pointed in beginTurn

- **Inputs fixed at build time must be re-pointed when the session MOVES, and that happens in `beginTurn`
  and NOWHERE ELSE.** A folder change (`SetSessionWorkingDir`) and a project edit (`invalidateMemberSkills` /
  `DeleteProject`) only set `sessionRun.treeStale`: both run on the UI goroutine, which cannot prove the
  session is not mid-turn, and re-pointing rewrites state under whatever is reading it. `beginTurn` is the
  one place that can prove no turn of this session is in flight.
- **The flag is ONE, and its members move together**: `ReloadSkillsIn(sessionTree)` (scan roots) and
  `SetPermissionRoot(ctx, sessionTree)` (project rules). A second, parallel "stale" flag is how one member
  ends up describing the tree the other just left — the two are fed from the same resolved
  `sessionRootsFrom` value in the same block for that reason. Adding a third tree-derived member means adding
  it THERE, not growing a new flag.
- **The permission policy needs `SetPermissionRoot` because `configure` resolves its project from the process
  cwd** (`agent/agent_configure.go`): the project half of `permissions.bash` lives in
  `<tree>/.tachi/permissions.yaml`, so `/`-cwd meant the file was `/.tachi/permissions.yaml` — never found.
  Global rules kept working, which is why the loss was invisible.
- **A rebuilt policy inherits the session's approvals** (`Policy.InheritSessionApprovals`): `allowAllAsks` is
  the user's 「本会话全部允许」 answer and MUST survive a re-point, or a folder change re-asks a question they
  already answered. The RULES do not carry over — that is the point — and deny is decided before any ask, so
  an inherited approval can never resurrect a forbidden command (`TestSetPermissionRoot_*` in `agent/`,
  `TestSessionTreeMove*` in `desktop/`).

## Desktop Turn History (the interrupted-turn merge)

- **A turn's history is the CONVERTED form, reminder blocks included.** `runHistory` holds `[]llm.Message` as
  `agent.ConvertSessionToLLMMessages` builds it, and that conversion re-attaches the `<system-reminder>` block
  to the user message it was stored with — deliberately, since `historyHasReminder` reads the prefix to avoid
  re-injecting first-message-only reminders.
- **So anything that MOVES a message between turns must unwrap it first.** The one such site is
  `mergeTrailingUserMessage` (`agent_turn.go`): an interrupted or killed session can leave the history ending
  on a user message, and that message is folded into the next turn's text so the provider never sees two
  consecutive user messages. Merging the content verbatim would carry the PREVIOUS turn's block along — the
  model would read a stale plan id, stale diagnostics and a stale branch as current, and because the merged
  text is recorded as the new message the block would walk forward turn after turn. It strips via
  `systemreminder.UnwrapUserMessage`, the single implementation of that inverse.
- **A trailing message that is nothing but a block is left alone — deliberately.** Two shapes look identical
  there: a reminder the loop injected mid-run (`injectLoopReminders` appends one as a user message, so a Stop
  landing right there leaves it trailing), which is stale scaffolding; and a trailing artifact reminder, which
  `ConvertSessionToLLMMessages` appends as its own user message ON PURPOSE so a reload cannot drop it (an
  `/research` or `/review` finding must still reach the model after a restart). No marker separates them, so
  dropping the message would break the second to tidy up the first.
- **That state is reached by RELOAD or by a well-timed Stop, not by an ordinary Stop**: a stop mid-stream has
  already recorded the partial assistant text, so the history ends on an assistant message and no merge
  happens. What produces a trailing user message with real text is a session reopened after the reply was never
  recorded (the process was killed, or the turn hard-failed) — which is why no smoke scenario covers it, and
  why `desktop/agent_turn_test.go` builds its fixture through the REAL conversion.

## Desktop UI State

Each bullet is a rule a change must keep, with the bug it prevents.

**Ownership — where new code goes**

- **The transcript lives in `useTranscript.ts`** (messages per session, running flags, the loaded window + its
  cursor, the frame-batched delta queue). Every mutator takes the session it is for — `updateSession` is the
  one funnel — so "this belongs to a conversation" is enforced by the signature. App.tsx must not declare its
  own `useState<Record<string, Message[]>>`; a new per-session need is a new op in that hook.
- **The composer is `composer.tsx`** (`useComposer` + the `Composer` element): the input box, the @-picker,
  the "/" palette, the queue of messages typed while a turn runs, and the parked-question form. All four share
  one decision — `route`: send now, queue for the next steer point, or dispatch as a slash command.
- **The composer's HEIGHT has three ways in and one implementation** (`nudgeHeight`): the handle's drag, ↑/↓ on
  the focused handle, and ⌘⇧↑/⌘⇧↓ typed in the textarea — all three share the step and the floor/ceiling
  (`COMPOSER_INPUT_KEY_STEP`, `composerInputMaxH()`), and at the floor the value returns to `null` so "not
  resized" stays one value (the stylesheet's). The pair is bound in the TEXTAREA's own keydown, never
  window-wide: ⌥↑/⌥↓ is already the diff pane's finding walk and bare ⌘↑/⌘↓ the text system's caret gestures.
- **Backend events are subscribed in `agentEvents.ts`** (`useAgentStatus` / `useSessionUsage` /
  `useAgentStream`). Each payload names its session; compare it against `currentId` only where the UI genuinely
  means "on screen" (a sidebar row, the error bubble), never to address storage.
- **A parked permission is the composer's state too, and its card is matched by tool CALL ID** (`perms` sits
  next to `asks`: keyed by session, NOT cleared on session switch — the agent is still parked — moved on
  auto-compaction, cleared on `agent:idle`). Matching by tool NAME is the trap: a model emits a whole batch of
  calls before any of them runs, so "the newest unfinished Bash card" is usually a different call than the one
  waiting. The form shows the backend's own preview verbatim and clears only when `AnswerPermission` returns
  `"ok"` — a refused one must stay on screen with its reason.

**The transcript**

- **A turn's process is folded by the CONVERSATION, not by the part renderer**: `turnView()` (`transcript.ts`,
  pure, shared by the live view and a rebuilt transcript) decides what a turn shows — one strip
  (`ProcessStrip`) standing in for its thinking blocks, tool cards and intermediate messages, the turn's LAST
  prose, and the parts that must never be hidden: a FAILED call, the call a permission card is parked on, the
  call an AskUserQuestion form waits on, and notices. **A DELIVERED file is a class of its own**: a `SendFile`
  call that succeeded IS the file card, rendered at the turn's TAIL (`.turn-files`, after the conclusion) —
  behind the fold it was not merely collapsed but ABSENT from the DOM, so 「把 X 发给我」 hid its own answer.
  It is never in the timeline as well (one card, one place). **Intermediate prose (any but the LAST `text`) is
  folded whether or not the turn failed**; the recall affordance is the strip's 「含 N 段过程说明」.
  **A steer splits the turn**, and therefore the fold: `injectSteerVisual` seals the streaming segment and
  opens a fresh assistant message, so each segment folds on its own (N steers = N+1 strips). Smoke cost: a
  scenario asserting a *successful* tool card must expand the strip first (`.process-head`), and "no tool
  cards" is no longer evidence of "no tool calls"; an ATTACHMENT card is the exception — it is on screen
  folded (`file-find`).
- **A row that alternates between two states must not change GEOMETRY — only its content may.** The process
  row flips between the live line and the summary once per tool call, and the transcript is bottom-pinned: the
  visible content jumps with any height difference. A `border: 1px` on the live state alone was exactly 2px of
  that; the ring is a `box-shadow: inset` now — the same reason the codebase paints every focus ring with
  `box-shadow`. **The same rule governs the live line's own text, which ROLLS when its state changes**
  (`RollingLine`): a fast turn swaps the label faster than a word can be read. Its box is exactly `1lh` and
  clips — `1lh` and not an `em` estimate, which came out 0.5px taller and broke the height assertion. The roll
  is a THIRD case in base.css's `prefers-reduced-motion` block: a STATUS indicator keeps its motion,
  decoration loses it, and a content SWAP changes which animation plays.
- **Never run a side effect inside a state updater**: React may invoke an updater more than once for the same
  update, so `setState(prev => { fetch(...); return ... })` fires the request twice — and an
  `if (prev[k]) return prev` guard cannot help, because both invocations see the same pre-update state. Keep
  in-flight bookkeeping in a ref.
- **A state flag must be ended by the fact that ends it**: `reviewPending` was cleared when `ReviewChanges`
  returned (that call only STARTS the fork), so the middle state was wiped before it could be seen. The run's
  own end event is the fact that ends it.
- **A system reminder is a HISTORY artifact, not a live one**: `Message.reminder` is set only by `buildTurns`,
  from the store's `role: reminder` rows, so the `!` affordance appears on a user bubble once the transcript is
  REBUILT (load / switch / restart). Assert injected context at the LLM boundary (`project-context`) or through
  a rebuild; a driver waiting for `.reminder-head` on a live first message waits forever.
- **A rebuilt transcript shows the user's OWN words, not what was SENT**: the `@`-file expansion happens
  BEFORE the agent is called, so a `user` record's `Content` is the file inlined between `UNTRUSTED FILE
  CONTENT` markers while `displayContent` holds what was typed — and the bubble reads the latter.
  **`Content` stays authoritative for the model**: `ConvertSessionToLLMMessages` rebuilds the next request from
  it, so writing the raw text there would send a prefix the model never saw (prompt cache gone from that point)
  and retroactively rewrite what the history says it was told. The session TITLE comes from the user's own words
  too. **A driver cannot pin a reload by switching sessions** — that serves the in-memory copy — which is why
  `at-file-reload` seeds the transcript and asserts the FIRST render.

**Scrolling and layout**

- **Following the bottom must survive async height changes, not just message updates**: the transcript pin ran
  only when `msgCache` changed, so a mermaid diagram finishing its async render, an image loading, or a tool
  card expanding slid the visible content up by exactly that height. The fix is a `ResizeObserver` on a
  `.chat-content` wrapper (the scrollport's own box never changes when its content grows); any new "sticky
  bottom" behavior goes through the same observer.
- **…and only the READER ends following, never growth.** `handleScroll` used to decide "the reader scrolled
  away" from the distance to the bottom (over 48px away = gone), which cannot tell a reader from a transcript
  that grew: content arriving BELOW the viewport leaves `scrollTop` where it was and pushes the bottom away.
  Every pin now goes through `pinBottom`, which RECORDS where it landed, and following ends only when
  `scrollTop` decreases below that record.
- **A width bound must subtract every other fixed column — and it must limit what is SHOWN, not what is
  STORED**: `.oneoff-panel`'s ceiling computed from `window.innerWidth` alone forgets the 280px sidebar. Read
  the sibling's REAL width from the DOM (`panelRoom()`, so a folded sidebar hands the room back) and re-measure
  on `resize`. Then keep the two numbers apart: App holds the width the reader CHOSE (persisted), the panel
  derives what there is room to show (`clampPanelWidth(chosen, room)`). **The conversation's share is a FLOOR
  plus a SHARE (`CHAT_MIN_WIDTH` 560, `CHAT_MIN_SHARE` 0.5)**: a bare floor pins the conversation AT the floor
  and hands every extra pixel to the panel (480px of prose beside an 840px panel on a 1600px window).
  **And the reading column itself is `--reading-width` (860px) — the same measure `.viewer-doc` gives a
  previewed document.**
- **A pane's width is its MIN-CONTENT width, and that is set by whichever row cannot shrink**: every diff head
  row holds non-shrinkable badges/buttons, and a `white-space: nowrap` path with the default `min-width: auto`
  contributes its WHOLE string — so the pane scrolls sideways. Three things together fix it: `flex-wrap: wrap`
  on those rows, `flex: 1 1 0; min-width: 0` on the path (basis alone is not enough — the automatic minimum is
  the min-content), and `max-width: min(1100px, 100%)` on `.viewer-doc.is-diff` (`min(1390px, 100%)` when the
  overlay's file index is showing). The user's bubble is the same problem in miniature: a pasted URL takes
  `overflow-wrap: anywhere` rather than `break-word`, because only the former also shrinks the min-content.
- **A control the reader uses WHILE a pane scrolls belongs in the sticky bar, not in the header**: the 意见
  pane's ↑/↓ walk sits in `.diff-sendbar` (`position: sticky; bottom: 0`). The cost is `flex-wrap: wrap` on
  the bar: it holds seven controls, and its min-content would otherwise widen the panel past its column.

**Sessions and the sidebar**

- **Session-scoped UI state must be keyed by session, or reset on switch**: one React tree renders every
  session, so anything derived from the ACTIVE session silently leaks into the next one — the review notice
  (keyed by the clicked turn's message id), plan/findings (reloaded on switch), the cache-hit ring + cost, and
  the agent STATUS. Anything fetched per session must be applied unconditionally; `if (u)` keeps stale values
  when the payload is empty.
- **The agent status is per SESSION, and its event names the session**: `agent:state` carries `sessionId`, and
  `useAgentStatus(currentId)` keeps one entry per conversation — a status is never one global value here,
  because the desktop runs every session's turn in its own goroutine (switching sessions cancels nothing). The
  only global surface is the menu bar, which reflects the DISPLAYED one (`reflectTray`; `ActivateSession`
  re-reports the session it switches to). Emitting only while that session was displayed produces four faces
  at once: the stop control follows the reader into an idle session; a background turn's END is never reported;
  a stale busy state makes the composer QUEUE a message instead of sending it; and the review chip waits on a
  neighbour's turn. The mount snapshot (`GetState`) may only SEED a session that no event has named yet.
- **A session's "running" answer and the event that says its turn is over must not disagree**
  (`RunningSessions`): answer from the per-session STATE, never from `sessionRun.running`. The flag is the
  guard that keeps a second turn from starting, and `endTurn` clears it — but a turn publishes its own
  `turn_complete` from inside the event loop and only THEN exits to `endTurn`, while the frontend re-reads the
  running set exactly once, on that event. The state is set BEFORE the event is emitted, so it can never
  contradict it (`TestAFinishedTurnIsNotReportedAsRunning`).
- **A session list is a list of CONVERSATIONS, not of session dirs** (`sessionRows` in `lib.ts`): a compaction
  chain shares one title, so rendering `ListSessions` raw shows the pre-compaction session as a second,
  identical-looking row. The shape is derived from `compactedParentId`, folded under the newest link and
  closed; the ancestors stay clickable history.
- **The sidebar is GROUPED, and the grouping is a pure join** (`sessionGroups(sessions, projects)` in `lib.ts`):
  one header per project (name, member count, a `＋` that creates a session INSIDE it, a right-click menu) and
  one final 无项目 bucket. Grouping happens BEFORE `sessionRows`, so a compaction chain folds inside its own
  group. A session whose `projectId` names a project NOT in the list belongs to the bucket, because that is
  what the backend's resolver does with it. **EVERY project gets a group, including one with no sessions yet**
  — the header carries the ＋ that makes the first session, so hiding empty groups made a freshly created
  project invisible. The chip and the workspace panel wear the project's NAME while a project drives the
  session, and the panel is READ-ONLY then (the three session-level writers are refused by the backend).
- **`＋ 新建会话` belongs to the 无项目 group, and ⌘N follows the session on screen**: the button makes a
  PROJECT-LESS session, so it lives above the label of the bucket it adds rows to — pinned at the top of the
  sidebar it was a full-width accent pill, the loudest thing on screen, reading as the page's primary action
  instead of "one more row here". ⌘N passes the current session's `projectId` when that project is still in
  the list: ⌘N means "another one of these", and the project supplies the workspace, the extra roots and the
  skills. Membership is resolved by the same rule as the grouping, so a dangling id makes a project-less
  session too.
- **⌘P opens the project palette** (`palette.tsx`, `App.tsx`'s `pickProject`): type to filter, ↑/↓ (or
  Ctrl+N/Ctrl+P — `listNav`, the same pair the composer's pickers take), Enter, Esc. Enter is the whole
  feature: open that project's most recently used conversation, or make its first one. **The caret then goes
  back to the composer** (`focusAfterPalette`): ⌘P is the only entry point that closes a dialog ON THE WAY to a
  session, and the palette's own input holds focus while it is open — removing it drops focus on `<body>`.
  "Most recently used" is the MEMBER with the largest `updatedAt` (`latestSessionOf`), which is the backend's
  own meaning (`memberSessions` in `desktop/projects.go`) — deliberately NOT the sidebar's row order, which is
  `ListSessions` by `createdAt`; a session another member was compacted FROM is skipped. Four contracts:
  `onPick` RETURNS the refusal reason ("" = it acted) and the palette closes only on ""; a project whose
  primary directory is gone with NO members refuses to create one (the backend would bind NOTHING); the palette
  reads the list the sidebar already renders and issues no fetch; and it EXPANDS the target group — ⌘P means
  「take me there」, and a collapsed group answers with a header and nothing under it. That expansion belongs to
  THIS entry point alone (a collapse is the reader's own doing, and forcing groups open everywhere would make
  collapsing the group you are sitting in impossible).
- **A project write re-reads the reader** (`agent:workspace_changed`, from `CreateProject` / `RenameProject` /
  `SetProjectRoots` / `DeleteProject`, handled by `useWorkspaceChanged`): the frontend holds a session's
  workspace and the sidebar's groups BY VALUE, so a rename moved nothing on screen until the user switched
  sessions. The payload names what changed (`projectId`, and `sessionId` for a detach).
- **Deleting a session with a turn in flight is REFUSED, by the backend**: the running turn owns a goroutine
  whose session writes (`AppendMessage` is `O_APPEND` with no `O_CREATE`, so it just fails once the directory
  is gone) and run-map writes are keyed by nothing but that id — so deleting underneath it loses the
  transcript silently, keeps the model running and the tools firing, and can rebuild a directory holding
  `meta.json` but no `messages.jsonl`. Two rules ride with it: the running flag is read and the run removed in
  ONE critical section, and the frontend renders the reason in the confirmation box that raised the delete
  while the menu's 删除 entry is `disabled` for a running session.
- **The sidebar has a type floor and an ORDER, and there is ONE caret size** (`--caret-size` in `base.css`):
  the disclosure carets had grown four different answers (8–9px of ▾ is a DOT — the one control that says
  "these rows are a group"), so they are `components.tsx`'s `<Caret>` now: an inline SVG sized by
  `--caret-size`, NOT the glyphs, because a text glyph's size follows the font. The sidebar is 15px/620 group
  headers against 14px rows with 12.5px timestamps, and nothing inside it is under 12px — 14px against 13.5px
  was true on paper and invisible on screen.

**A turn's changes, diffs and reviews**

- **A turn's changes are read from its CHECKPOINT trees, everywhere they are shown.** The footer chip
  (`N files +x −y`), 「完整 diff」 and 「评审本轮改动」 all go through `GetTurnChanges`/`reviewScopeFor`
  (`desktop/turnchanges.go`), which diffs the turn's recorded start and end trees — so a file a SHELL command
  wrote counts, created/deleted files are exact, and the answer does not move when the working tree does. The
  tool-call fragments (`turnDiffStat`) are only the FALLBACK for when there are no checkpoints, and the
  surface must say which source answered (`TurnChangesVO.Source`). Four traps: (1) the turn's end snapshot must
  be taken BEFORE the `turn_complete` event, and on a context DETACHED from the turn's cancellation
  (`context.WithoutCancel`), because the error/cancel exit is a stopped turn whose context is already dead;
  (2) a refused end (`Diff.Skipped`) arrives as a note and ZEROES — the chip must fall back to the tool-call
  numbers rather than take them as the answer; (3) a refused end releases only its own end ref — the START ref
  is the turn's rewind point AND the parent `parentRef` for the NEXT writing turn, so dropping it makes
  `commit-tree -p <ref>` fail and every later turn loses its checkpoint too; (4) a turn whose changes came from
  a shell command has NO tool-declared paths, so anything that skips work on an empty path list hides exactly
  that case.
- **`评审所有改动` is the ONE review entry whose subject is not a turn** (`ReviewAllChanges`): every
  uncommitted file in the working tree, from any turn, read against HEAD. It rides the LATEST turn's footer
  only (`lastAssistantIndex`), because the tree as it is NOW belongs to the turn just watched — and it must
  STILL be there on a turn that changed nothing of its own. Four things travel with the scope and every one is
  a lie if it lapses: the prompt's provenance sentence (`ReviewScope.WholeTree`), the record
  (`OneOffKeyWholeTree`), the diff itself (`GetWorkingTreeChanges` → `TurnDiffVO.Source == "head"`, so the pane
  heads itself 全部改动), and the chip's own state (`reviewDoneLabel` pairs on (msgId, whole), since one footer
  now carries two entries that would otherwise report each other's runs).
- **Every path in a diff carries the ROOT it came from.** A session can hold additional workspace roots, and
  the paths inside each diff are RELATIVE to one — so a file without its root resolves against the primary one,
  and a same-named file there is what 预览/打开 would show, silently. `FileDiffVO.Root` (absolute) +
  `RootLabel` ("" for the primary, else the base name, or the full path when two roots share a basename —
  `rootLabels`, the SAME rule and strings as `AtMatch.root`). Four traps: (1) a group's identity for React keys
  and fold state is `(root, path)` — with the path alone, folding one root's file folds the other's; (2)
  `GetTurnDiff` diffs EVERY root — a path in an additional root is in a DIFFERENT repository, not "outside the
  repository"; (3) `checkpoint.Manager.TurnDiff`/`ChangedSinceTurn` return per root (`RootDiff`/`RootChanged`)
  and must never be merged into one text or path set; (4) **every row is labelled as soon as ONE of them has
  to be** (`multiRoot` in `DiffFindingsPane`) — labelling only the additional roots left the primary's row bare,
  and a bare row beside a labelled one reads as belonging to no tree at all. The primary is named 主目录
  (`PRIMARY_ROOT_LABEL`).
- **A review's PANE reads the same diff the reviewer read, by TURN.** The run record keeps the turn
  (`OneOffKeyTurn` → `OneOffVO.Turn`) and the findings pane fetches `GetTurnChanges(...)` — so a review of
  changes that were committed or deleted since still shows the files it read, with its findings anchored. Two
  traps: the pane used to fetch by the recorded PATH list alone, which is the working tree against HEAD (an
  empty pane); and 「重新评审」 used to hardcode turn 0, so re-running from the pane was refused exactly when
  the frozen pair made it possible.
- **A rewind is only offered for the conversation the reader is still IN — a compacted-away session is
  refused.** Compaction starts a NEW session that continues from a summary, so the predecessor's checkpoints
  describe the state before that point while the live conversation is the successor; rewinding there would move
  the workspace backwards under a successor that has kept writing since. `rewindBlockedByCompaction` finds the
  successor from the session's own link OR from a session naming it as parent, and the refusal is raised by the
  PREVIEW (`RewindPreview.Blocked`) as well as the action — refusing only on confirm would have built a card
  offering a rewind that cannot happen.
- **The transcript's turn stamps cover every record of a turn, not only its opening one.** `sessionTurnStamps`
  keys turns by the record that BEGINS them; `buildSessionMessages` resolves that to "the turn in force at this
  record" (`stampBefore`) and stamps every record of the page. A page is the newest `sessionPageSize` records,
  so a long turn's opening record sits on an EARLIER page — resolving the boundary exactly leaves the card of
  exactly the longest (shell-heavy) turns without its turn when the transcript is reloaded.
- **An entry's data source must belong to the surface the entry opens.** The turn footer's 「完整 diff」 opens
  a surface of its own (`TurnDiffOverlay`) whose paths are the click's own: fetched on open, dropped on close,
  so they can never become a stale anchor. Two corollaries: **a fold default belongs to the surface that shows
  the diff** (the panel's rule "a file with findings opens" folded EVERY file on a surface with no findings —
  hence `expandAll`), and **a helper whose only caller is gone goes with it**.
- **Anything cached per run or per session must be KEYED by it**: key the cache with `sessionId/run` (a
  record's name is a timestamp WITHIN its session, so two sessions can hold the same one), and prefer the run's
  OWN record over anything a caller passes in. **The key says what is IN HAND, never "we asked once"**: the
  report pane's text is a file whose PATH is recorded when the run starts while the file arrives at the END —
  so a read taken in between finds nothing, and storing that under the run's key freezes the pane for good.
  Store only content; keep the reason in a second, equally keyed state (`reportMissNote`: missing /
  unreadable / not text / genuinely empty), because an empty pane cannot tell them apart. The read is driven
  by the PANE (tab + run + liveness), so switching runs and the run ENDING both re-read.
- **A panel opened BY an event cannot assume what that event announces is on disk yet**: the run's `start` is
  emitted BEFORE the recorder is opened, so the side panel's own list read comes back empty — and with nothing
  selected the detail poll has nothing to ask for, so a whole review hides behind 「这个会话还没有旁路运行」
  until it ends. The live follow therefore re-reads the LIST too and takes the record that appears as the one
  it is on (`seenNewest`, compared by name, so the reader's own pick in the switcher is left alone).

**Overlays, the panel and find**

- **The overlay's file index PINS inside the pane's existing scroller — it never gets one of its own.** The
  pinned thing is `.diff-filelist-col` (the list PLUS its drag handle, one flex item beside the diff). It pins
  BELOW the pane's pinned head, at the head's MEASURED height (`--diff-head-h`, published by the pane from a
  `ResizeObserver`: the head WRAPS, so there is no constant to hard-code), and a jumped-to file's
  `scroll-margin-top` reads the same variable. Two traps: the scroller is whichever box the pane already lives
  in (a nested `overflow: auto` would leave find and the finding walk searching the wrong one), and the index
  is OVERLAY-ONLY and absent with a single file — the panel is a column the reader drags, where a 210px index
  takes a third of the width.
- **The row the reader is ON is marked, and the mark follows the SCROLL, not just the click** (`currentKey`):
  a pinned index whose highlight only moves when clicked reports the reader's last jump rather than the file in
  front of them. The rule is stated in the SAME terms as the jump — a jumped-to file's top comes to rest on the
  line `--diff-land` names, and the current file is the LAST group whose top has reached it — with the line
  PUBLISHED from one place (the pane's `publish()`) and consumed twice (the CSS `scroll-margin-top` and this
  rule). **Both ENDS need a fallback**: at the top of the scroll nothing has reached the line yet, and at the
  bottom nothing is left to reach it; without them the highlight vanishes in both places. The listeners are
  `window` + CAPTURE (`scroll` does not bubble), the walk is coalesced onto a frame, and the mark is a bar AND
  a wash AND `aria-current` — never colour alone (the bar is an inset shadow, so marking a row cannot change
  its height).
- **The index is draggable, and the width the reader drags is NOT React state** (`startResize`): what is SHOWN
  is the reader's width clamped to the pane (`180`–`560`, never leaving the body under `420`), the drag writes
  the element's style straight from the pointermove handler (a `setState` per move re-renders the whole panel,
  the diff included, before the browser has laid the new width out) and commits to React once on release.
  A `useLayoutEffect` re-asserts the drag's own number after every commit. The width is remembered at MODULE
  level (`fileListWidth`) so closing and reopening the overlay does not forget it — deliberately NOT persisted:
  `desktop_ui.json` is where the panel's width lives because Go has to honour it before the window exists.
- **A click unfolds the file FIRST and scrolls after that commit** (the same two steps as the finding walk),
  lands it 10px below the scroller's top and flashes the same `is-jump-target` a finding gets.
- **The lightbox's `fit()` measures the WINDOW, not the stage**: the stage is a flex item with `margin: auto`,
  so it hugs its content instead of filling the overlay — fitting to that box can only shrink. `viewerArea`
  reads the overlay's padding box instead. **`fit()` only ever SHRINKS** — "as large as it fits" applied in
  both directions opened the SAME gesture at wildly different sizes depending on a thing the reader cannot see.
  **`getBoundingClientRect` inside a CSS-zoomed subtree is in PRE-zoom units** in this WebKit, so a reading of
  "how big is it on screen" must multiply by the zoom.
- **⌘F searches a SURFACE, not the page** (`find.tsx`): the diff pane, a previewed file and a review's report
  each host the find bar, and each searches its OWN subtree — ⌘F in a preview must not scroll the transcript
  behind it. The side panel is ONE host covering its three panes, so the pane decides what "here" means. Four
  rules: hits are painted with the **CSS Custom Highlight API** (`::highlight()`), never by wrapping text in
  `<mark>` — these panes re-render under the reader and a DOM mutation would be thrown away; the key owner is
  installed ONCE at **module load** (a bar may mount after the overlay it sits inside, and among capture
  listeners on the same target the first registered runs first), and it consumes Esc with
  `stopImmediatePropagation` so the bar closes instead of the viewer behind it; the bar is **mounted only while
  open on every surface** (a mounted-but-closed bar looks open to the host handle, `open` never becomes true,
  and every count reads 无匹配); and `findable` is **opt-in per viewer** — an image or an HTML/PDF frame has no
  text in THIS document, and the shell still owns ⌘F there.
- **The one-off panel's × promises Esc, and who owns the FIRST press is decided by focus**: a column is not a
  `ViewerOverlay`, so the viewers' dismissal contract never covered it. `App`'s global handler must not claim
  every Escape unconditionally (`defaultPrevented` is the handshake), and the panel is a window-level listener
  that refuses only an ALREADY-CLAIMED key. Because `/review` leaves the focus in the composer, the first Esc
  there only hands the focus to the message area and the SECOND closes; a field inside the panel blurs on the
  first.

**Text and dialogs**

- **A dialog that renders a PANEL's rows inherits that panel's type scale — and the shared box's max-width**:
  the project form reuses the roots panel's rows (`.roots-*`), sized for a compact popover (11px paths and
  buttons), so the 520px modal read as a shrunken dialog between its own 14px title and 13.5px field — and
  `.confirm-box`'s `max-width: 380px` beat `.project-form`'s `width: 520px`. The form raises those shared sizes
  SCOPED to `.project-form` and declares its own `max-width`. `form-text` pins a floor rather than the numbers
  (nothing under 12px, and the box is the width it claims).
- **Path tries are byte-keyed: iterate strings by BYTE, not `for i := range s`** (`for i := range s` walks rune
  boundaries, so `s[i]` yields only the first byte of each multi-byte character) — otherwise Chinese file names
  come back mangled from the @-file index. `pkg/container/pathtrie.go`.
- **Do not reintroduce an "activity row" above the composer**: one existed, appeared and vanished once per
  step, and shoved the message area up and down; if it ever comes back, it must hold its place for the whole
  turn. **Do not compensate a layout change from a `requestAnimationFrame`** — a covered window makes WebKit
  stop the page's frames outright. Compensate in a LAYOUT effect instead: the new content is in the DOM and the
  adjustment lands before paint.
- **The titlebar is `App.tsx`'s `<header className="titlebar">`**: brand, the sidebar toggle, the session id
  (click to copy), then `titlebar-right` — the side-panel toggle and the theme switch, held at the far edge by
  one `margin-left: auto`; a new titlebar control belongs in that cluster. **The agent state is not echoed in
  the chrome**: 运行中 already reads from the sidebar row's `spin-dot` and the reply's typing dots.
- **The context ring is ANCHORED on the last call's real prompt size, not on the character estimate**
  (`desktop/contextinfo.go` → `AIAgent.LastInputEstimateWithBreakdown`). The estimate
  (`agent/token_estimate.go`) is character-class based and its bias depends on the content: it over-counts
  plain English and UNDER-counts mixed CJK + JSON. So the reported number is the last call's real prompt size
  (`llm.PromptTokens`: input + cache read + cache creation for Anthropic; the total alone for OpenAI-family)
  scaled by the estimate's movement since that call. The breakdown in the popover is scaled to that total
  (`Breakdown.ScaleTo`, because the parts must add up to the number above them), and a session restored from
  disk anchors on the last recorded message. The correction is a RATIO of the estimate's movement applied to
  the anchored real value, not an additive delta with a floor: after a compaction the history is REPLACED by a
  summary, and a floor would leave the ring sitting on the pre-compaction number. The auto-compact THRESHOLD
  reads the anchored value too, so the trigger and the meter agree; the cooldown is the one comparison still on
  the raw estimate (it measures the conversation against itself, where the bias cancels).
- **The context ring must follow each API CALL, not each turn** (`desktop/agent_turn.go` + `agentEvents.ts`):
  the ring's estimate was read only on mount / new / switch / `turn_complete`, while the popover fetches
  `GetContextInfo` when it opens — so during a long turn the ring sat at whatever the turn started with while
  the popover at the SAME moment already showed the current value. `emitUsage` already ran after every API
  call, so the estimate now rides that event (`agent:cost` carries `contextEstimate`/`contextWindow`, from the
  one `contextUsageOf` rule `GetProviderInfo` shares). Two lessons: "the turn ended" and "the call ended" are
  different refresh points, and two surfaces describing the same fact must read it from ONE rule.
- **The context ring measures the PROMPT of the last call — a reply enters it only on the NEXT turn**: the
  estimate is computed before each API call, so a turn that ends in a plain reply leaves the meter describing
  the history *without* that reply. A scenario about "the context got smaller" must fill the window first, or
  it asserts nothing.

## Desktop Themes (the dark palette)

- **The dark theme is Ayu Mirage-derived and its contrast is low ON PURPOSE — do not "fix" it.** The family
  is that theme's blue-grey (`surface.base` #1F2430, panel #282E3B), lifted one OKLCH step so the app reads as
  graphite instead of black (`--bg` #252a35, chroma ~0.022 against a neutral ~0.010), with Ayu's *warm* grey
  editor fg as the CONTENT ink (`--text` #cecdc6) and its cool `ui.fg` family as the CHROME ink
  (`--text-dim`/`--text-faint`, 10.5–11.5px metadata).
- **Every "go" button is ONE flat recipe — do not put the gradient back.** `.send-btn`, `.pending-send`,
  `.diff-sendbar-go` and `.plan-start` share a single solid `--accent-strong` fill: no gradient, no inset
  sheen, no coloured glow (`.new-chat` is the tinted variant of the same idea). Two consequences: button
  contrast is tuned in ONE place, and **a fill that carries white text always takes the `-strong` variant** —
  `--red` is tuned as INK and puts a white glyph below 4.5:1.
- **Find hits are amber on purpose** (`--find-hit` wash + `--find-hit-current`, base.css, both themes): a hit
  has to be findable inside a diff, where green and red already mean added and removed, and inside prose, where
  the accent means a link or inline code.
- **The reading surfaces take their sizes from ONE place** (`base.css`: `--fs-prose` 15.5px / `--fs-bubble`
  15px / `--fs-finding` 13px). "The text is too small" is an edit there, not a hunt through four files. The
  scale is deliberately NOT a blanket multiplier — labels, badges and metadata are chrome and keep their own
  sizes, and the panel's diff body stays at 11px so a finding is what stands out against the code it is about.
- **What bounds the palette is `--text-faint`'s 4.5:1 floor, not `--text`.** It is the token that lands on the
  *lightest* plane (placeholders, MCP labels, `.file-note`), so it pins both the ink and the top of the ladder
  (~#303541). Any "make it lighter still" edit has to trade against that: raise the planes and the smallest
  text is what breaks first, and `--text` has room to spare. Verify with a WCAG ratio check across every plane
  (`luminance` + alpha compositing), not by eye.
- **`windowBgLight`/`windowBgDark` (main.go) are the same decision in a second place** — the pre-paint window
  colour, which `uitheme.go` also feeds to the native appearance. Move them with `--bg`.
- **One definition per mode, and it lives in `frontend/public/base.css`**: the `:root[data-theme="dark"]`
  block, resolved by the boot script in index.html + `src/theme.ts`. No frontend rule reads
  `prefers-color-scheme`, and no other file carries dark colours — the hljs tokens in `widgets.css` map onto
  the semantic tokens, so a palette edit covers code blocks for free; upward shadows use `--shadow-up`.
- **To SEE a palette change, do not reach for the app window** (window capture is dead on macOS 26). Render
  the tokens instead: extract both blocks from base.css (next to `git show HEAD:desktop/frontend/public/base.css`
  for the before) into a small HTML page whose swatches/mock rows read `var(--token)`, one scope per variant,
  and screenshot it with headless Chrome (`--headless=new --force-device-scale-factor=2.6 --window-size=…`).
  That gives a real A/B of the REAL values without a rebuild loop.
