# Tachi — Badge Handbook (the AI Passport device link, split out of desktop.md)

The badge is a SECOND FRONTEND for the same sessions: a BLE (or serial) link, a small screen for asks, and a
voice path that turns speech into a message. Its host half lives in `desktop/` (`link/`, `badge.go`, and the
Swift shim in `stt/` — which the desktop's OWN microphone button also records through; that consumer's rules
are in `docs/agents/desktop.md`); its device half is a different repository — `ai-passport` (this machine:
`~/repos/ai-passport`, ESP-IDF at `~/esp/esp-idf-v5.5.3`) — which carries its own `AGENTS.md`, its own skills
and its own gate.

## Contents — read the section you need, not the file

| Section | Read it when |
| --- | --- |
| **Badge Link over BLE** | you touch `desktop/link/`, the badge transport setting, or the Bluetooth Info.plist key — **or the link died and the badge screen stopped updating** |
| **Badge Voice (speech → text)** | you touch `desktop/stt` or the voice path in `desktop/badge.go` — **or a transcription fails and you need to know why** |
| **Mic capture (the shim's other half)** | you touch the RECORDING side of `desktop/stt` (`tachi_stt_record_start/stop`, i.e. the desktop's microphone button `desktop/voice.go`) — **especially when that button says 「没有听到内容」**: the capture traps are in the `Building desktop/stt` section below. Its own rules (events, the state machine, the smoke stand-in) are in `docs/agents/desktop.md` |
| **Badge → the desktop window (its keys)** | you touch the `key` protocol message, the device's status-screen keys, or the host's `Key` mapping |
| **Building `desktop/stt` (the Swift shim)** | you touch `shim.swift`, the shim fails to link, or a change to it does nothing — **the library is rebuilt by a build step, not by the compiler** |
| **The Device Side** | you are about to change, build or flash the firmware — **or the host and the device disagree about the protocol** |

## Badge Link over BLE

- **A connected-but-SILENT badge is the badge's own doing, and its serial log says which.** Two device-side
  traps produce the exact symptom "the link is up, nothing arrives", both found only by capturing the badge's
  console: (1) the badge must FRAME what it sends — `@@` + body + `\n` — or the host discards every line as
  console noise, because GATT gives the sender message boundaries and the host a byte stream; (2) the badge
  must not clear its `s_subscribed` flag on the CONNECT event, because on a bonded reconnect NimBLE announces
  the NVS-restored subscription (`reason=RESTORE`) *before* that event, and the wipe mutes the badge for the
  whole connection — its own guard log then reads `secured=1 subscribed=0`, every send is silently refused,
  and the badge's "the host went quiet" rule tears the link down 30 seconds later. Take a capture of the badge
  before theorising about which side is at fault (`main/badge_ble.c` in the ai-passport repo carries the
  mechanisms).
- **NOTHING tells the host the peer is gone, so the transport must ASK.** tinygo's darwin adapter keeps
  `DidDisconnectPeripheral` to itself (it only feeds `Connect`'s own waiting channel), and `bleTransport.Read`
  has no exit but `Close()` — so a peer-initiated disconnect (the badge's own "host went silent" rule firing, a
  board reset, the radio going out of range) leaves `link.Server.Run` blocked forever: `badgeLoop` never Accepts
  again, every later state push goes nowhere, and the badge screen freezes on its last frame with nothing
  logged on either side. `Read` therefore polls `device.Connected()` (`bleLivenessEvery`) and returns
  `errLinkGone` — that error's only job is to travel back through `Server.Run` to `badgeLoop`, which is what
  makes a reconnect happen — and `Write` asks the same question FIRST, because a `without-response` write to a
  gone peer polls for 10 seconds *per chunk* instead of failing. Do **not** re-implement this as a "no data for
  N seconds" timeout: the badge's 10s `sync` is what makes silence readable, and that number belongs to the
  firmware.
- **A zero `bluetooth.Device` panics on EVERY method — `Disconnect` included.** A failed `Connect` returns a
  zero Device *with a nil error*, so "did it connect" is the `Address == bluetooth.Address{}` check, not `err`;
  disconnecting goes through the `disconnect()` helper for the same reason (`Close()` on a transport that never
  got a device would otherwise take the app down). The unit test for the liveness watchdog is what caught this —
  a fake `alive` is enough to exercise it, since that verdict is the one part of this layer that does not need
  hardware.
- **The desktop app logs through `pkg/logger`, never the std `log` package.** Std `log` writes to stderr, and an
  app launched as a bundle has no reader there: those lines exist only in a terminal you may not have open. This
  is what turned the first real "screen froze on its last frame" failure into a guess — `badge link: device
  connected` / `device disconnected` / `waiting for device` are the only record of when the link came and went
  (they land in `~/.tachi/logs/debug.log`), and they were going to stderr while it happened. `desktop/badge.go`
  is migrated; the rest of `desktop/` still has ~13 std `log` call sites.
- **`NSBluetoothAlwaysUsageDescription` is needed by anything that LINKS the package, and tccd judges the
  RESPONSIBLE process — the threshold is far lower than "when you actually use Bluetooth".** `tinygo.org/x/bluetooth`'s
  darwin adapter builds a `CBCentralManager` in a **package-level variable** (`adapter_darwin.go`'s `DefaultAdapter`),
  so importing it is enough to touch CoreBluetooth — no call required. A responsible bundle without the key gets
  `Refusing authorization request for service kTCCServiceBluetoothAlways … without NSBluetoothAlwaysUsageDescription
  key` and `This app has crashed because it attempted to access privacy-sensitive data without a usage description`,
  i.e. SIGABRT with no Go stack, arriving **asynchronously** (the CentralManager's callback thread), so it looks like
  "the first test passed, the second one aborted". Both `build/darwin/Info.plist` and `Info.dev.plist` carry the key.
  A `bin/Tachi.app` built before that line will abort `go test ./desktop/...` launched from it, because the test
  binary inherits Tachi.app as its responsible process — rebuild the app and restart it before believing a desktop
  test failure. Embedding an `__TEXT,__info_plist` section in the test binary does **not** help: tccd never looks at
  the requesting process here (find the reason with
  `log show --last 5m --predicate 'eventMessage CONTAINS "kTCCServiceBluetoothAlways"'`). The same mechanism is why
  the smoke bundle is signed with the local certificate instead of ad-hoc: a fresh identity is a fresh app to
  macOS, and it gets asked again on every run (see the smoke section in `docs/agents/desktop.md`).
- **CoreBluetooth caps a `without-response` write at ~182 bytes and fails SILENTLY above it.** The cap is
  `maximumWriteValueLength(for:.withoutResponse)`, which the library does not expose; `bleChunks` slices at
  `bleWriteChunk` (180) and the badge reassembles at the newline. Short messages never reveal this — an 854-byte
  state vanished whole while 131-byte ones looked perfectly healthy.
- **`WriteWithoutResponse` on an authenticated characteristic is dropped with no error and no pairing prompt** — it
  says nothing about whether the link is secure. `waitSecure` therefore forces the question with one **with-response**
  write of a bare `\n` (the badge discards an empty line, so no protocol row is invented): that write blocks while
  macOS runs pairing — the 6-digit code is on the badge screen, and it must be typed on the computer — and fails with
  `Encryption is insufficient` while the link is still plaintext.
- **macOS does not re-pair because the device forgot.** The badge stores one bond in NVS (`BLE_MAX_BONDS=1`); when its
  key is gone but the Mac's row remains, the Mac encrypts with the stale key and drops the connection instead of
  prompting — the device logs `断开 reason=531`. Only 系统设置 → 蓝牙 → Forget that device (or a fresh name/address)
  gets past it; the device side is 长按上 twice on the status screen (`badge_ble_forget`), which also tells the user
  the computer has to be dealt with separately. (长按确定 is not the forget gesture — it sends voice: **hold to talk**,
  press starts the recording and release sends it, so the idle screen's footer rotates between the two hints.)

## Badge Voice (speech → text)

- **A failed transcription must say WHY, and `NULL` from the shim means only "it never ran".** `tachi_stt_transcribe`
  returns NULL solely for the `#available` / bad-argument guards; every real failure comes back as JSON —
  `{"error":"SFSpeechErrorDomain 4: No GeneralASR asset for language en","missing":[…],"installing":[…]}` — and
  `ParseTranscription` turns the `error` field into a Go error while KEEPING `missing`/`installing` on the returned
  `stt.Result`. `desktop/badge.go` needs them to choose the sentence the badge shows (`voiceFailureText`): 「缺离线
  资产」is a machine state that heals itself, 「引擎坏了」is not, and collapsing both into one 「转写失败」makes them
  indistinguishable from the log alone.
- **`SpeechTranscriber` needs a per-language ASSET, macOS EVICTS it, and one missing language kills the other too.**
  The asset is `com.apple.speech.asr.transcription.<lang>` under
  `/System/Library/AssetsV2/com_apple_MobileAsset_UAF_Speech_AutomaticSpeechRecognition/purpose_auto/`; it is
  downloaded on demand and dropped again once nothing uses it, so "installed once, stays forever" is false. With one
  missing, the analyzer's input loop dies — `SFSpeechErrorDomain Code=4`, "No GeneralASR asset for language <lang>"
  — and because the shim puts zh-CN and en-US in ONE analyzer, the other language's result dies with it. Two
  mechanisms answer that: `shim.swift`'s `transcribe` retries the modules ONE AT A TIME when the pair fails (so the
  usable language's text still gets delivered), and `installNeeded` asks `AssetInventory.assetInstallationRequest`
  for what is missing and calls `downloadAndInstall()` in the background (never inline — that is a 100 MB+ download
  on the "hold to talk" path). Two traps: `SpeechTranscriber.installedLocales` lists `en_US` even when its asset is
  gone, and `AssetInventory.status(forModules:)` answers `.supported` for a language with no asset at all — neither
  is the "can I use it" predicate, so do not add a pre-flight check built on them.
  **When a transcription fails**: the app log carries the message we compose, and the framework's own line (plus the
  asset downloads) is in the unified log —
  `log show --last 30m --predicate 'process == "Tachi"' --info --debug | grep -i speech`.
- **The badge screen gets a result either way.** `link.Server.VoiceText` carries a transcript, `VoiceError` a failure —
  same `emit` path (own seq) because the transcription runs on its own goroutine, not the link's. The firmware renders
  an `error`'s `message` as a 5-second notice and ignores its `id` (one recording at a time), so the host only has to
  fit `BADGE_DETAIL_MAX` (96 bytes) — which is why `voiceFailureText` composes whole Chinese sentences rather than
  passing the framework's English text through.
- **`voice: chunk count mismatch` in the host log means an upload STARTED and then came apart, not a broken
  link.** The firmware already discards a hold shorter than `BADGE_VOICE_MIN_MS` (400 ms) *before* `voice_begin`
  goes out, so the two presses of a double-press never reach the wire; a line in the host log therefore means the
  upload really did start (a real hold) and then lost chunks — `link/voice.go`'s parts check voids it and logs
  the discard. Nothing to fix on the host side; look at the link (BLE retries, a serial port contention) if it
  becomes frequent.

## Badge → the desktop window (its keys)

The badge drives the desktop WINDOW: the short up/down press turns the conversation half a page (the same
action as Ctrl+U/Ctrl+D inside it) and a double press on 确定 brings the window to the front. The device
knows NOTHING about the window — it reports gestures and the host decides what they mean
(`desktop/remote.go`; the device side is `main.c`'s key routing).

- **One protocol message carries the whole feature, and it is why `Proto` is 3** (the removed `hud` state is
  what made it a breaking change): device → host `{"t":"key","key":"up|down|activate"}` (`link.Source.Key`),
  mapped in `desktop/remote.go`'s `Key`. up/down emit `chat:scroll` at the main window and the FRONTEND does
  the scrolling — `App.tsx`'s `scrollByHalfPage`, which Ctrl+U/Ctrl+D call as well, so 「翻页」 cannot mean
  two things depending on where the finger is (how far a page is, and where the reader is, are DOM facts).
  `activate` is `Show()` + `Focus()`, the same pair the tray's 「打开主窗口」 and a clicked notification use.
- **The version is checked in TWO places and nothing checks them against each other**: `link.Proto`
  (host) and `BADGE_PROTO_VERSION` (`main/badge_proto.h`, written into `hello`) must move together. Bumping
  one alone is silent on the host side (the mismatch is answered to the DEVICE as an `error` message and
  never logged here) and shows on the badge as a 5-second notice — so the symptom is 「badge 连上了但什么
  都不动」. Both were 1 while `info` was still in the protocol; changing only the host while flashing a
  device is exactly how the two ends get out of step mid-upgrade, and the fix is the same on both sides.
- **The device reports those gestures on the STATUS screen only** (no pending ask): on the ask screen the
  same keys are the ANSWER (up/down move the cursor, 确定 submits — `badge_state_key`), and a window being
  open on the computer is no reason to change what a key means while someone is waiting for an answer.
- **A tap is not a recording, which is what lets one button mean two things** — and that rule is the firmware's
  OWN, older one (`badge_voice.h`'s `BADGE_VOICE_MIN_MS`, 400 ms: a shorter hold never even sends `voice_begin`).
  So press-and-hold still talks, while the two taps of a double press are free to be a key (「切到电脑」); nothing
  stray goes on the wire for either.

## Building `desktop/stt` (the Swift shim)

- **The package has TWO halves that answer to two different permissions, and the shim carries both.**
  Transcription (`tachi_stt_transcribe`) needs the speech-recognition grant; capture
  (`tachi_stt_record_start/stop`) needs the MICROPHONE one, `NSMicrophoneUsageDescription` — a second plist
  key, in both `build/darwin/Info.plist` and `Info.dev.plist`, whose absence kills the app the moment the mic
  is opened (the Bluetooth trap above, same class). **A HARDENED-RUNTIME build needs an ENTITLEMENT on top of the
  key**, and neither the plists nor `build/darwin/Taskfile.yml` carries one today — so the locally signed
  `make build` works while a notarized one (`wails3 tool sign --notarize`, whose default is
  `-hardened-runtime`) would fail to open the mic (and to use Bluetooth) until
  `com.apple.security.device.audio-input` / `com.apple.security.device.bluetooth` are signed in. Both callers of
  `Transcribe` feed it the same shape — 16 kHz
  mono Int16 — the device half by DECODING the uplink's ADPCM (`desktop/badge.go`), this half by resampling the
  hardware's own buffers in the shim; that sameness is what lets the two consumers share every judgement above
  it (`Choose`, the missing-asset degradation, the failure sentences). Capture is deliberately NOT gated on
  macOS 26 (`AVCaptureSession` is far older) — the version gate belongs to transcribe, i.e. to `Available()`.
- **Mic capture is `AVCaptureSession`, and that is a Bluetooth fact, not a preference — switching it back to
  `AVAudioEngine` fails SILENTLY and only intermittently.** `AVAudioEngine`'s IO cycle takes its sample rate from
  the OUTPUT device, while one headset can be 44.1 kHz out and 16 kHz in — so `inputNode.outputFormat` reports a
  rate the input hardware does not have. Reading the format and installing the tap with it freezes that rate
  *before* the headset leaves A2DP (idle: no usable input at all) for HFP, and that switch happens only during
  `start()`: it returns without error, `isRunning` flips false, and the tap is **never called** — zero samples,
  which the user sees as 「没有听到内容」. `AVCaptureSession.startRunning` instead WAITS for the
  profile switch (measured ~57 ms) and every buffer carries its own format, so the shim resamples per buffer
  (`RecordSink`) instead of committing up front. What makes this hard to catch by hand: it works whenever some
  other app has already pulled the headset into HFP, so the same build is broken, then fine, with nothing
  changed — the device's state is system-wide, not per-process. The one line that separates the two causes is
  `desktop voice: transcribed`: a captured-but-empty take still logs it (`chars=0`), a take with no samples does
  not. Also, the delegate is held WEAKLY by `setSampleBufferDelegate` — lose the strong `recordSink` reference
  and the callbacks stop silently, with exactly the same symptom.
- **`desktop/stt` is half Swift, and the Swift half is a BUILD STEP** (`stt/build_shim.sh` →
  `stt/libtachi_stt.a`, NOT committed — `desktop/Makefile`'s `build`/`test` targets run it, and
  `//go:generate ./build_shim.sh` is the source-level pointer to it). The reason it cannot follow the
  repo's usual cgo pattern: macOS 26's transcriber API (`SpeechAnalyzer` / `SpeechTranscriber` /
  `AssetInventory`) exists ONLY in Swift's module interface — the ObjC headers carry nothing but the old
  `SFSpeechRecognizer` — so `webview_awake_darwin.go`'s inline-ObjC approach cannot express it. Three
  consequences that are easy to get wrong:
  - **Compile the shim for the app's deployment target (12.0), not for 26.0.** `swiftc -target …-macosx26.0`
    makes the whole binary require 26.0 symbols, so the app stops LAUNCHING on macOS 15 rather than merely
    losing the feature. At 12.0 the 26-only symbols come out weakly linked (`nm -m` shows `weak external`)
    and every use is behind `#available(macOS 26.0, *)` — `stt.Available()` then answers false instead.
  - **A missing `libtachi_stt.a` fails the whole PACKAGE build**, not one test, and cgo buries the cause
    (`clang: error: no such file or directory: '…/libtachi_stt.a'`) at the end of a very long command line —
    which is why the Makefile targets carry the step rather than a comment telling you to remember it.
  - **The Go build cache does NOT see `libtachi_stt.a` change, so a rebuilt shim can be silently left out —
    the build succeeds and the app runs the shim from before it.** cgo names the library only inside the LDFLAGS string
    (a constant `-L` path), so its contents are not part of the build action's ID: rewrite `shim.swift`, rebuild
    the `.a`, run `make build` — and if those exact build flags ran before the change, the previous LINK result
    is reused. Zero warnings, and it looks exactly like "my fix does not work". **Ask the binary, not the build
    log**: `nm -u bin/Tachi | grep AVCapture` (an ObjC class reference is an undefined symbol, so it survives
    `-ldflags="-w -s"`; `strings` does NOT show it). Pick a symbol that only the change under test could have
    introduced — `AVCaptureSession` for the capture rewrite, while `AVCaptureDevice` proves nothing (the old
    code referenced it too). The cure is `cd desktop && GOWORK=off go clean -cache`, then `make build`; a new
    `go run ./something` package is built fresh and will therefore look fine while the app does not, which is
    the trap's most misleading face.
  - **The static library ALONE does not link, and it does not RUN either: a rpath is needed once
    the shim uses Swift concurrency.** The link half: the Swift back-deployment libraries its
    auto-link directives name live in the TOOLCHAIN, not the SDK — `stt/build_shim.sh` derives that
    directory from `xcrun --find swiftc` and symlinks the libraries into `stt/`, and `stt_darwin.go`'s
    LDFLAGS carry `-L${SRCDIR}`. The run half: `libswift_Concurrency.dylib` is found through
    `@rpath`, which an app bundle has and a Go test binary does not — without
    `-Wl,-rpath,/usr/lib/swift` the test binary aborts with
    `Library not loaded: @rpath/libswift_Concurrency.dylib`, while the app itself is fine.
  - **Do not conclude anything from `go test ./stt/ ./link/`.** An app-level regression that only
    shows up when the whole binary is linked and RUN (the rpath above is exactly one) is invisible
    to per-package tests. After touching `desktop/`, run `cd desktop && make test` — it builds the
    shim first and then does `go test ./...`, which is the only command that links the real thing.
    ⚠ **`go build ./stt` catches NONE of the above**: a library package compiles without linking, so a
    broken stt link only surfaces in `go test` or in a build of the app.

## The Device Side

- **The firmware has its own repository, its own rules and its own gate — nothing on the host side covers it.**
  `~/repos/ai-passport` is an ESP-IDF 5.5.3 project (`~/esp/esp-idf-v5.5.3`): read its `AGENTS.md` before changing
  anything there (it routes to its own guides, requires its own skills, and mandates `./tools/validate.sh` — the
  `--firmware` half needs that IDF activated). The host's `make lint` / `go test` say NOTHING about it.
- **A dev flash must PRESERVE NVS, and the merged image does not.** Firmware and stored data are different things:
  writing `FoloToy-AI-Passport-full.bin` from `0x0` (what `idf.py flash` would do with a merged image) can reset the
  NVS and PHY regions — that is the badge's BLE bond, i.e. the pairing the Mac's row must match (see the re-pair trap
  above). For a normal iteration write the three segments at their configured offsets instead
  (`0x0` bootloader / `0x8000` partition table / `0x10000` app), which leaves NVS at `0x9000` alone; the boot log then
  shows `订阅: notify=1（从 NVS 恢复）` and `加密后：安全状态变为 已认证加密`, which is the proof the bond survived.
  `idf.py erase-flash` is a separate, destructive choice — never a default repair.
- **The badge's serial console is the only device log, and the host shares that port.** `idf.py monitor` and the
  host's serial transport contend for the same device (see the `TACHI_BADGE` note in `docs/agents/desktop.md`), and a
  capture is the fastest way to tell a device-side trap from a host-side one. The host's own `@@` protocol lines
  appear in that same capture.
- **Where the rest of the badge lives.** The device is also a SECOND READER of the desktop's pending-ask table (never
  a second answer path) — that property, and the `badge` config block / `TACHI_BADGE` override / session-order rules
  it comes with, are in `docs/agents/desktop.md` (Desktop Backend).
