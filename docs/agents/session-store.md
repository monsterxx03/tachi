# Tachi — Session Store Handbook (split out of `.tachi.md`)

Read this **before touching `session/store.go`, anything that reads the session list, or any path that writes
`meta.json`** — every frontend's session list, the desktop sidebar, `--resume`, and the sidecar files a turn
keeps beside a session all go through this one store.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New session-store lessons belong in THIS file, not
back in `.tachi.md`** — and the rules that ride with that file (freshness in the same turn,
convention-not-history, split + index when a subject grows) apply to this split too.

## The list is memoized

- **`ListSessions` serves one directory walk to every caller; `LoadMeta` must stay un-memoized.** One UI
  refresh needs "all sessions" several times over — the desktop's sidebar asks for its rows, then each
  project's member count, then one project's members, as three separate calls — so `ListSessions` answers all
  of them from one snapshot (a miss costs one walk: ~49ms per 1000 sessions, essentially all of it
  open/read/close). The snapshot is dropped by every write the store makes (`CreateSession` / `UpdateMeta` /
  `DeleteSession`, called AFTER the write) and by the base directory's mtime moving, which is what covers a
  session created or deleted by ANOTHER process.
- **`LoadMeta` must not be memoized**: it feeds the live record and the read half of a metadata
  read-modify-write (`desktop/roots.go`'s `updateSessionMeta` rewrites the whole `meta.json`), where a stale
  read becomes a lost write. `session/store.go`'s `FileStore` comment carries the full contract, including the
  one hole it has.

## Writes

- **`meta.json` is written ATOMICALLY (tmp + rename).** Keep it that way: an in-place rewrite lets a
  concurrent reader — a fresh store under `--resume`, the desktop's sidebar, another process — read a
  truncated file, and `scanSessions` can only skip what does not parse, so a live turn would silently drop its
  own session from someone else's list. `session/store_test.go`'s
  `TestListSessionsNeverMissesASessionBeingRewritten` pins it (against `os.WriteFile` it fails within a handful
  of scans).
- **The desktop builds EVERY manager on one store** (`d.sessionStore()`): a turn writes through its own
  per-run manager, and only a shared store lets that write drop the snapshot the sidebar's next read would
  otherwise be served from. `session.NewManager(nil)` under `desktop/` is the mistake —
  [desktop.md](desktop.md) (Desktop Backend) has the reasoning.
- **Image bytes leave the record inside `AppendMessage`, and that is what makes them safe.** A multi-modal
  message's parts are persisted on the record (`Message.Parts`), but their bytes are not: `AppendMessage` —
  the ONE writer a session record has — moves each image into the session's `images/` directory as a
  content-addressed blob (the SHA-256 of the decoded bytes, so an image sent twice costs one file) and leaves
  the reference behind. Two rules ride on that: nothing else may write a session record (the subagent and
  one-off recorders own their files and call `DropUnstoredImageParts` — a reference they cannot resolve is
  worse than no part), and a record on disk never carries base64, which is what keeps a conversation full of
  screenshots a readable JSONL file inside `LoadMessages`' scanner buffer. The read half is `LoadImage`,
  reached through the manager as `agent.ImageResolver`; a blob that is gone degrades that one part, it never
  fails the conversion ([prompt-cache.md](prompt-cache.md) explains why the rebuild has to be byte-exact).

## What a cleanup may delete

- **A session can be HELD, and a held session is never deleted — the record is on disk, not in the memory of
  the manager that took it.** `session/retention.go` is the authority: one file per session under
  `<sessionDir>/retention/holds/`, each naming the owner, the reason (`task` / `context`) and a deadline. The
  count-based sweep skips a held session (`Manager.CleanupOldSessions`) and an explicit delete is REFUSED with
  the reason (`Manager.Delete` → `SessionHeldError`), so `desktop.DeleteSession`, ACP's delete and a sweep in
  another process cannot get round it. `Pinned`, `ThreadID` and `Kind` are NOT the mechanism — a pin is the
  reader's own statement and lifting it must not drop a running task (design §9.4).
- **Every decision that ends in a deletion is made under ONE cross-process lock**
  (`<sessionDir>/retention/retention.lock`, `Retention.Lock`), which is a different lock from the node's
  lifecycle lock and never shares its path: a node being online must not stop another process from cleaning up
  legally. Candidates are enumerated OUTSIDE the lock (a directory scan must not be inside a cross-process
  critical section); the re-check of "is it held, is it still current" is INSIDE. The lock file is never
  removed on release, like every other flock in this repo.
- **The in-process lock comes SECOND.** The fixed order is file lock → `Manager`/store lock, so a caller must
  not hold the manager's mutex when it reaches for the retention lock; `Manager.Delete` and
  `ManagerCleanupOldSessions` are written that way, and `Manager.New` sweeps only after releasing its own lock.
  The reverse order is a deadlock between a sweep and a node binding a context.
- **A hold also says what it can no longer know.** A hold whose file cannot be read keeps the session (skipped
  and reported), never releases it: unreadable protection is not the same as no protection. The owner of a
  hold's lifetime reconciles leftovers at startup (`node/runner.go`'s `reconcile`), because a crash can leave
  a reason nobody will ever withdraw.

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md) (Desktop Backend, the sidebar's reads),
[checkpoints.md](checkpoints.md) (the sidecar files a turn writes under a session).
