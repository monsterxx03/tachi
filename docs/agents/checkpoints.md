# Tachi — Checkpoints Handbook (split out of `.tachi.md`)

Read this **before touching `agent/checkpoint/`, anything that resolves a workspace root** (a folder change,
an added/removed additional root, a desktop project edit, a worktree checkout), **or the compaction path** — a
turn's ability to write files depends on what is here.

`.tachi.md` is injected WHOLE into the first message of every session, so it keeps only the conventions that
apply anywhere in the repo and points here for the rest. **New checkpoint lessons belong in THIS file, not
back in `.tachi.md`** — and the rules that ride with that file (freshness in the same turn,
convention-not-history, split + index when a subject grows) apply to this split too.

## A root is identified by PATH, never by position

- **A workspace root's identity is its PATH, never its index.** A checkpoint records which tree and which
  store it covers (`RootState.Root` / `.Store`), and everything that reads one goes through `repoFor(state)`;
  `currentRepo` is for snapshotting THIS turn only. The root set is SORTED and mutable (folder change,
  added/removed additional root, desktop project edit, worktree checkout), so resolving a store or a work tree
  by index silently rewrites one workspace's files into another — `parentRef` matches on path+store for the
  same reason. `agent/checkpoint/rootchange_test.go` pins it;
  `docs/2026-09-14-desktop-project-design.md` §14 has the full account.

## The turn's binding is session-scoped

- **A turn's checkpoint binding is SESSION-scoped, and auto-compaction swaps the session mid-turn.** The
  manifest lives under the session's own directory, so a turn begun in the parent means nothing to the child:
  every write-capable tool for the REST of that turn is refused ("turn N was never begun", by design — an
  unrecorded write is one a rewind cannot take back) until the turn ends. `maybeAutoCompact` therefore
  re-binds the turn (`rebindCheckpointTurn`) right where it refreshes the other session-scoped values (the SDK
  session id); a root change alone does NOT need this, since the manifest is shared per session.
  `TestCheckpointRebindsAfterCompaction` pins it.

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md) (projects and the root set),
[session-store.md](session-store.md) (where a session's files live).
