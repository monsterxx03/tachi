# Tachi — @-References Handbook (split out of `.tachi.md`)

Read this **before touching `agent/atfile/`, `agent/sessionref`, a frontend's `@` picker or its expansion
step, or the pasted-image path** (`session.SavePastedImage`, `pkg/clipboard`, the composer's paste handling)
— a reference is spelled in one place and expanded in another, and the two have to agree.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New lessons about references or pastes belong in
THIS file, not back in `.tachi.md`** — and the rules that ride with that file (freshness in the same turn,
convention-not-history, split + index when a subject grows) apply to this split too.

## One scanner owns the syntax, the FRONTEND expands

- **One scanner owns the syntax, and the FRONTEND expands before the agent is called**
  (`tui/commands_agent.go`, `desktop/agent_turn.go`) — so a new reference kind is wired in BOTH frontends, and
  a picker may only offer what `atfile.Expand` reads back (hence `RefForPath`/`RefForSession`: the spelling
  lives on the expansion side).

## `@session:` is a POINTER, never the transcript

- Files are inlined (or annotated, as `[@file: path]`); **`@session:<id>` becomes a POINTER into the session
  store — id, title, record count and transcript path, never the transcript** (`agent/sessionref`, injected
  through `atfile.Options.Sessions`). Inlining another conversation would spend this session's context on text
  nobody asked to read, for the rest of the conversation.

## A paste is BYTES with no path

- **Both frontends store it in the session's own directory and attach it by `@`-reference**
  (`session.SavePastedImage` → `<SessionDir>/<id>/pasted/`, the OS temp dir when there is no session yet) —
  the route a dropped file takes, expanded into a content part at send time and recorded with the message (so
  a reload keeps the image, the reference being expanded once).

## A terminal's paste channel carries TEXT only

- **A clipboard image never reaches `tea.PasteMsg`** — the terminal reports the gesture as not performable and
  sends nothing — so the TUI reads the OS clipboard itself (`pkg/clipboard`; on macOS through
  osascript/AppKit, since `pbpaste` handles text only — its `-Prefer` takes txt/rtf/ps — and on Linux through
  wl-paste/xclip, bytes validated by magic number). `ctrl+v` is that read and pastes text too; an EMPTY
  `tea.PasteMsg` takes the same path, for terminals that do report the gesture.

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md) (the picker, the composer's paste handling),
[session-store.md](session-store.md) (where a session's records and blobs live).
