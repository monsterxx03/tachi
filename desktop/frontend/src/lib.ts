import { Clipboard } from '@wailsio/runtime'
import type { KeyboardEvent } from 'react'
import type { ProjectVO, SessionMessage, TurnChangesVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { AttachmentInfo, Message } from './types'

// imeActive reports whether a key event belongs to an IME composition (Chinese / Japanese /
// Korean candidate selection). Enter while composing confirms a candidate — treating it as
// "submit" is the classic IME bug. keyCode 229 is the fallback some WebKit builds report
// instead of setting isComposing.
//
// It lives here, next to listNav, because every key handler in the frontend needs it and a
// copy per module is how one of them ends up missing the check (the type is structural, so it
// takes React's synthetic event and a plain DOM KeyboardEvent alike).
export function imeActive(e: { nativeEvent?: { isComposing?: boolean }; keyCode?: number }): boolean {
  return !!e.nativeEvent?.isComposing || e.keyCode === 229
}

export function extractReminder(content: string): { reminder: string; text: string } {
  const re = /<system-reminder>([\s\S]*?)<\/system-reminder>/g
  const blocks: string[] = []; let m
  while ((m = re.exec(content))) blocks.push(m[1].trim())
  const text = content.replace(re, '').trim()
  return { reminder: blocks.join('\n'), text }
}

// fileFromSendFileArgs reconstructs an attachment from a recorded SendFile call — the ONE
// definition of "this call handed a file over", used by the card renderer (parts.tsx shows a
// FileCard instead of a raw tool card) and by the fold's own rule (transcript.ts keeps the
// delivered ones out of the process strip). It takes the args as recorded, so a reloaded
// session renders the same card the live turn did.
export function fileFromSendFileArgs(args: string): AttachmentInfo | null {
  if (!args) return null
  try {
    const path = (JSON.parse(args) as { path?: string }).path
    if (typeof path !== 'string' || !path) return null
    return { path, name: path.split('/').pop() || path }
  } catch {
    return null
  }
}

export function fmtTime(ts?: string): string {
  if (!ts) return ''
  const d = new Date(ts)
  if (isNaN(d.getTime())) return ''
  const now = new Date()
  const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate()
  const hhmm = d.toLocaleTimeString('zh-CN', { hour12: false, hour: '2-digit', minute: '2-digit' })
  if (sameDay) return hhmm
  const md = d.toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit' })
  return `${md} ${hhmm}`
}

// fmtDur renders a millisecond duration as a concise human-readable string.
export function fmtDur(ms: number): string {
  if (ms <= 0) return '0s'
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`
  const m = Math.floor(s / 60)
  const rs = Math.round(s % 60)
  return `${m}m${rs > 0 ? rs + 's' : ''}`
}

// fmtShare renders a share of a whole (0..1) as a percentage: "62%", "8.5%",
// "0.4%". Slices smaller than 1% keep a decimal so they do not all collapse
// into "0%" — a bucket that exists should never look empty.
export function fmtShare(v: number): string {
  if (!isFinite(v) || v <= 0) return '0%'
  const pct = v * 100
  if (pct >= 10) return `${Math.round(pct)}%`
  if (pct >= 1) return `${pct.toFixed(1)}%`
  return `${pct.toFixed(2)}%`
}

// fmtCredit renders a credit amount with a fixed 2 decimals. Credit is an
// accounting unit, so every frontend shows the same amount the same way: this
// matches the Go turn footer (agent.formatCredit) and the web console's
// "credit" helper (web/frontend/src/lib/format.ts) — and it also hides the
// float noise left by summing per-call ledger snapshots.
export function fmtCredit(v: number): string {
  if (!isFinite(v)) return '0.00'
  return v.toFixed(2)
}

// Turn ids must be UNIQUE across pages. History is loaded page by page and
// older pages are PREPENDED into the client-side transcript, so an index-based
// id ("h-0", "h-1", …) repeats in every page — and duplicate React keys make
// cards reuse each other's component state, so clicking one message appeared to
// toggle nothing (or toggled a different message). A module-level counter is
// enough: ids only have to be unique within this document.
let turnSeq = 0
function nextTurnId(): string {
  return `h-${++turnSeq}`
}

// Rebuild turns from RAW session messages, preserving the real in-turn order:
// one assistant card per turn with interleaved thinking / assistant text / tool cards.
export function buildTurns(sms: SessionMessage[]): Message[] {
  const turns: Message[] = []
  let cur: Message | null = null
  let pendingReminder = ''
  // The turn a record belongs to, and what it changed. The backend stamps EVERY record of a
  // turn with the turn it is part of (see stampBefore), so the card reads it from whichever
  // record starts the card; the carry below is for the live path, where a record arrives
  // without one. A page can open in the middle of a turn, which is exactly why the stamp may
  // not be looked up on the opening record alone.
  let turnStamp: { turn?: number; changes?: TurnChangesVO | null } = {}
  sms.forEach((sm) => {
    if (sm.turn) turnStamp = { turn: sm.turn, changes: sm.changes }
    if (sm.role === 'reminder') { pendingReminder += (pendingReminder ? '\n' : '') + sm.content; return }
    if (sm.role === 'user') {
      const r = extractReminder(sm.content)
      const rem = r.reminder || pendingReminder || undefined
      // The bubble shows the user's OWN words: when the record kept them (displayContent)
      // they win over Content, which is what was SENT — for a turn that used @-file
      // references that is the file inlined into the message. The reminder block is still
      // read off Content, which is where it lives.
      const text = sm.displayContent || r.text
      // A steer (an interjection typed while the agent worked) is recorded as a user
      // message too; its iteration is the call it was injected before, while a turn's
      // own prompt has none. Nothing here decides what to DO about that — the rewind
      // menu does (see rewindTargetForMessage).
      turns.push({
        id: nextTurnId(), role: 'user', text, reminder: rem,
        reminderCollapsed: rem ? true : undefined, ts: sm.timestamp || undefined,
        recordIndex: sm.index, steer: (sm.iteration ?? 0) > 0 || undefined,
      })
      cur = null
      return
    }
    if (!cur || cur.role !== 'assistant') {
      cur = {
        id: nextTurnId(), role: 'assistant', parts: [], ts: sm.timestamp || undefined,
        turn: turnStamp.turn, changes: turnStamp.changes,
      }
      turns.push(cur)
    }
    if (!cur.parts) cur.parts = []
    if (sm.role === 'assistant') {
      if (sm.thinking) cur.parts.push({ type: 'thinking', text: sm.thinking })
      if (sm.content) cur.parts.push({ type: 'text', text: sm.content })
      cur.ts = cur.ts || sm.timestamp
    } else if (sm.role === 'tool_call') {
      cur.parts.push({ type: 'tool', name: sm.toolName, title: sm.title, args: sm.args, summary: '执行中…', ok: true, done: false, toolCallId: sm.toolCallId, change: sm.change })
    } else if (sm.role === 'tool_result') {
      const t = [...cur.parts].reverse().find((p) => p.type === 'tool' && (p.toolCallId === sm.toolCallId || !p.done))
      if (t) { t.summary = sm.toolResult; t.ok = !sm.isError; t.done = true }
      else cur.parts.push({ type: 'tool', name: sm.toolName, summary: sm.toolResult, ok: !sm.isError, done: true })
    }
  })
  return turns
}

const HUMANIZE_UNITS = [
  { v: 1e6, s: 'M' },
  { v: 1e3, s: 'K' },
] as const
export function humanize(n: number): string {
  if (!isFinite(n) || n <= 0) return '0'
  for (const u of HUMANIZE_UNITS) {
    if (n >= u.v) {
      const r = Math.round((n / u.v) * 10) / 10
      return `${r % 1 === 0 ? r : r.toFixed(1)}${u.s}`
    }
  }
  return n.toString()
}

// tpsTier maps a tokens/sec rate to a color tier, matching the TUI status bar:
// <60 slow (red), 60–199 normal (yellow), >=200 fast (green).
export function tpsTier(t: number): string {
  return t >= 200 ? 'fast' : t >= 60 ? 'normal' : 'slow'
}

// actOnKey makes a div-with-onClick keyboard-operable. Rows that host their own
// controls (session rows, tool headers, MCP rows) cannot be real <button>s —
// a nested button/input is invalid — so they take role="button" + tabIndex and
// this Enter/Space handler instead.
export function actOnKey(fn: () => void) {
  return (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      fn()
    }
  }
}

// listNav turns a key event into a list-navigation step: ↓ / Ctrl+N is +1, ↑ / Ctrl+P is -1, and
// anything else is 0. The Ctrl pair is the TUI's keymap (readline's next/previous), which is what
// a hand already on the keyboard reaches for in a list — and it is the reason a caller must
// preventDefault on a non-zero step: on macOS the text field underneath reads Ctrl+N/P as its own
// caret movement (emacs bindings), so an unconsumed one would move the caret INSTEAD of, or on top
// of, the highlight.
export function listNav(e: { key: string; ctrlKey: boolean }): -1 | 0 | 1 {
  if (e.key === 'ArrowDown' || (e.ctrlKey && e.key.toLowerCase() === 'n')) return 1
  if (e.key === 'ArrowUp' || (e.ctrlKey && e.key.toLowerCase() === 'p')) return -1
  return 0
}

// SessionRow is one line of the sidebar: a conversation, plus the sessions it was compacted from.
//
// compactedMissing describes the pre-compaction session the fold could NOT reach, and is what tells
// the two cases apart that `compactedFrom` alone cannot: null means the chain ends in the list (the
// normal case), while a string — possibly empty, when the record predates the field — is the title
// recorded on the last link the walk could follow (see sessionRows).
export type SessionRow<T> = { session: T; compactedFrom: T[]; compactedMissing: string | null }

// sessionRows folds a session list into rows, one per conversation.
//
// A compaction chain is ONE conversation to the reader — the child continues where the parent
// stopped, and both carry the same title — so the ancestors hang under their child (closed)
// instead of standing beside it as extra, identically-named sessions. That is also what keeps the
// sidebar the same after a restart: the shape is derived from `compactedParentId` in meta.json, not
// from the live switch the frontend happens to have witnessed.
//
// The input's order is preserved (ListSessions sorts newest-first), so the newest link of a chain
// is the row and its ancestors follow it newest-first. A parent whose child is no longer in the
// list (deleted) is a row of its own — its history is still reachable.
//
// A link whose PARENT is not in the list is the case the row must not swallow: the conversation was
// still compacted, and the reader is owed that fact. `compactedParentTitle` (recorded on each child
// when the chain was made) is what survives the parent's disappearance — a parent deleted by hand,
// or swept by session cleanup's max_keep between two launches — so the row reports the half it
// cannot show instead of looking like a session that was never compacted at all.
export function sessionRows<T extends { id: string; compactedParentId?: string; compactedParentTitle?: string }>(list: T[]): SessionRow<T>[] {
  const byId = new Map(list.map((s) => [s.id, s]))
  const hasChild = new Set<string>()
  for (const s of list) {
    if (s.compactedParentId && byId.has(s.compactedParentId)) hasChild.add(s.compactedParentId)
  }
  const rows: SessionRow<T>[] = []
  for (const s of list) {
    if (hasChild.has(s.id)) continue // its child carries this conversation's row
    const compactedFrom: T[] = []
    let missing: string | null = null
    const seen = new Set([s.id]) // a corrupted pair of links must not loop forever
    // The walk follows links through the records it HAS, so it stops either at the end of the chain
    // or at the first link that leaves the list — and that link's own record is the one that names
    // the missing session (the child for a missing parent, a present ancestor for a missing
    // grandparent).
    let cur: T = s
    while (cur.compactedParentId && !seen.has(cur.compactedParentId)) {
      const parent = byId.get(cur.compactedParentId)
      if (!parent) {
        missing = cur.compactedParentTitle || ''
        break
      }
      seen.add(parent.id)
      compactedFrom.push(parent)
      cur = parent
    }
    rows.push({ session: s, compactedFrom, compactedMissing: missing })
  }
  return rows
}

// pinnedFirst orders ONE group's sessions for the sidebar: the pinned ones first, everything else
// in the order it came in (ListSessions: newest first).
//
// Per GROUP, never the whole list — that is the whole rule. A pin asks for a row to sit at the head
// of its own neighbours: inside the project it belongs to, or inside the project-less bucket. It
// does not lift the row above another project's group, and it does not reorder the groups
// themselves (the user's own project order stands).
//
// It runs BEFORE folding, and that ordering matters: sessionRows folds a compaction chain under its
// newest link, so reordering the rows afterwards would tear a 「压缩前」 ancestor away from the child
// it hangs under. Sorted input, folded output keeps a chain in one piece wherever it lands.
//
// Array.prototype.sort is stable (spec-guaranteed since ES2019), so the newest-first order survives
// inside each half: the pinned block is newest-first, and so is the block below it.
function pinnedFirst<T extends { pinned?: boolean }>(list: T[]): T[] {
  if (!list.some((s) => s.pinned)) return list
  return [...list].sort((a, b) => Number(!!b.pinned) - Number(!!a.pinned))
}

// SessionGroup is one group of the sidebar: a project (or null for the project-less bucket)
// plus the rows that belong to it. Rows are the same conversation rows sessionRows builds —
// grouping happens BEFORE folding, so a compaction chain folds inside its own group.
export type SessionGroup<T> = { project: ProjectVO | null; rows: SessionRow<T>[] }

// liveProjectId answers which group a session belongs to: its `projectId` when a project in the
// list answers to it, and "" (the 无项目 bucket) otherwise — no id at all, or one naming a project
// that is gone (a deleted project, a lost projects.json). A dangling id lands in the bucket on
// purpose: the backend resolves such a session that way (project first when it DRIVES the session,
// else the session's own snapshot), and a row must not claim a group the resolver does not honour.
//
// It is ONE definition because two callers ask this question: the sidebar's grouping below, and
// ⌘P's own 无项目 row (`looseMembers`). A second copy is how that row comes to open a session the
// sidebar files under a project.
function liveProjectId(s: { projectId?: string }, known: Set<string>): string {
  const pid = s.projectId
  return pid && known.has(pid) ? pid : ''
}

// sessionGroupKey answers which GROUP a session lands in — the key `expandedProjects` files a fold
// state under (App.tsx's `groupKey` for the group it renders). It is the same rule as the join
// above, one step further along, and it exists for the paths that reveal a group without a click on
// a row: ⌘K picks a conversation wherever it sits, and a conversation whose project is gone lands in
// the bucket like any other project-less one. Spelling that rule a second time at the call site is
// how a switch into a folded group stops opening it — and a folded group answers with a header and
// nothing under it, which is the same as not having gone there.
export function sessionGroupKey(s: { projectId?: string }, projects: ProjectVO[]): string {
  return liveProjectId(s, new Set(projects.map((p) => p.id))) || LOOSE_GROUP_KEY
}

// sessionGroups renders the sidebar's grouping (design §7.1): one group per project, plus a
// final bucket for the sessions no live project owns.
//
// Membership is by ID, joined here rather than stored on the row: a session carries only
// `projectId`, so renaming a project relabels every row with no write to any session. A
// session whose project is not in the list — a dangling id (the project was deleted under an
// earlier version, or projects.json lost) — lands in the project-less bucket on purpose: the
// backend resolves it exactly that way (project first when it DRIVES the session, else the
// session's own snapshot), and a row must not claim a group the resolver does not honour.
//
// EVERY project gets a group, including one with no sessions yet — that is the whole point of
// the header: it carries the ＋ that creates the first session and the menu that renames, edits
// or deletes the project. Dropping empty groups made a newly created project invisible (the
// reader sees nothing after the create, and cannot reach it to make a session), which is a bug
// the reader reports as "I made a project and nothing happened".
//
// The 无项目 bucket gets one on the same terms, empty included: it is a group like any other (its
// own header, its own ＋, its own fold state), and it is where the sidebar's "make one that
// belongs to no project" action lives. Dropping it while empty is what took that action off the
// screen for a reader whose every conversation happens to sit in a project.
//
// The order inside a group is the order it came in (ListSessions: newest first), except that a
// pinned session goes to the head of its own group (see pinnedFirst).
//
// The join is ONE pass that buckets by projectId, not one pass per project: this runs on every
// render of the app (during a turn, every streaming frame), and a per-project filter made it
// O(projects × sessions) with a fresh Map and Set built per group — at a few thousand sessions
// that is real work per frame, for a list whose shape is fixed until the next fetch.
export function sessionGroups<T extends { id: string; compactedParentId?: string; compactedParentTitle?: string; projectId?: string; pinned?: boolean }>(
  list: T[],
  projects: ProjectVO[],
): SessionGroup<T>[] {
  const known = new Set(projects.map((p) => p.id))
  const byProject = new Map<string, T[]>()
  const loose: T[] = []
  for (const s of list) {
    const pid = liveProjectId(s, known)
    if (!pid) {
      loose.push(s)
      continue
    }
    const bucket = byProject.get(pid)
    if (bucket) bucket.push(s)
    else byProject.set(pid, [s])
  }

  const groups: SessionGroup<T>[] = projects.map((p) => ({
    project: p,
    rows: sessionRows(pinnedFirst(byProject.get(p.id) ?? [])),
  }))
  groups.push({ project: null, rows: sessionRows(pinnedFirst(loose)) })
  return groups
}

// looseMembers returns the sessions of the 无项目 bucket — the same membership `sessionGroups`
// gives the sidebar, asked for by ⌘P's own 无项目 row (how many sessions it holds, and which one
// Enter opens). It reads the same two facts, so it needs no session lookup of its own.
export function looseMembers<T extends { projectId?: string }>(list: T[], projects: ProjectVO[]): T[] {
  const known = new Set(projects.map((p) => p.id))
  return list.filter((s) => !liveProjectId(s, known))
}

// latestSessionOf returns the session of a project that its reader most recently used, or
// undefined when the project has no members yet — the question ⌘P answers with Enter.
//
// It is the project-shaped half of the rule; `newestOf` below holds the rule itself, because ⌘P's
// 无项目 row asks the same question of a bucket that is not a project (the palette shows which
// title Enter will open either way, so the two rows must not disagree about "newest").
export function latestSessionOf<T extends { id: string; updatedAt: string; projectId?: string; compactedParentId?: string }>(
  list: T[],
  projectID: string,
): T | undefined {
  if (!projectID) return undefined
  return newestOf(list.filter((s) => s.projectId === projectID))
}

// newestOf returns the session its reader most recently used among `members`, or undefined when
// there are none.
//
// "Newest" is `updatedAt`, not `createdAt`: that is what the backend means by a project's
// newest session (`memberSessions` in `desktop/projects.go` sorts by it), and it keeps Enter
// from landing in an empty conversation made this morning while the one actually being worked
// in sits above it. It deliberately does NOT track the sidebar's row order, which comes from
// `ListSessions` (createdAt) — the palette therefore SHOWS which title Enter will open, so the
// reader never has to know which clock the choice used.
//
// A session another member was compacted FROM is skipped: compaction leaves the predecessor in
// the list as a folded-away 「压缩前」 row (`sessionRows` folds exactly these, by the same
// `compactedParentId` link), and a conversation's newest link is the one to open.
export function newestOf<T extends { id: string; updatedAt: string; compactedParentId?: string }>(
  members: T[],
): T | undefined {
  const hasChild = new Set<string>()
  for (const s of members) {
    if (s.compactedParentId && members.some((p) => p.id === s.compactedParentId)) hasChild.add(s.compactedParentId)
  }
  let best: T | undefined
  for (const s of members) {
    if (hasChild.has(s.id)) continue
    if (!best || s.updatedAt > best.updatedAt) best = s
  }
  return best
}

// nextSessionAfter returns the session that should take over when `id` is deleted: the row BELOW
// it inside its OWN group — a project's members, or the 无项目 bucket — and the row ABOVE it when
// it was that group's last one. The rule a browser uses for a closing tab, and for the same
// reason: the reader's eye is already there, so the successor belongs next to the row that
// vanished, not at the top of a list nobody was reading.
//
// Only a group left with NO rows falls out of the group: then the next group's first row, and ""
// when nothing is left anywhere (the caller's reload makes a session in that case). "" is also the
// answer for an `id` that is not a visible row of any group — already folded away inside a
// compaction chain, or not in the list at all — so an unplaceable id keeps the caller's own
// behaviour instead of moving the reader somewhere arbitrary.
//
// `id` must still be IN `list`: this is asked BEFORE the reload, which is the only moment the
// deleted row is still there to be located.
export function nextSessionAfter<T extends { id: string; compactedParentId?: string; compactedParentTitle?: string; projectId?: string; pinned?: boolean }>(
  list: T[],
  projects: ProjectVO[],
  id: string,
): string {
  const before = sessionGroups(list, projects)
  const gi = before.findIndex((g) => g.rows.some((r) => r.session.id === id))
  if (gi < 0) return ''
  const at = before[gi].rows.findIndex((r) => r.session.id === id)
  // The same join on the list WITHOUT the deleted session: the row that stood below it now sits at
  // the deleted one's index, and an index past the end means it was the last row — then the row
  // above (the last one left) is the neighbour the reader was looking at.
  const rest = sessionGroups(list.filter((s) => s.id !== id), projects)
  const sameGroup = rest[gi]?.rows ?? []
  if (sameGroup.length > 0) return sameGroup[Math.min(at, sameGroup.length - 1)].session.id
  for (let i = gi + 1; i < rest.length; i++) {
    const row = rest[i].rows[0]
    if (row) return row.session.id
  }
  return ''
}

// projectMatches reports whether a project answers a palette query: a case-insensitive
// substring of its NAME or of one of its directories. Substring rather than a subsequence
// match — a project name is typed whole (「tachi」, 「支付」), not spelled letter by letter, and
// a subsequence match on one or two characters would return essentially every project. The
// paths are searched too because a reader hunting for a project often remembers its directory
// rather than the label someone gave it.
export function projectMatches(p: ProjectVO, query: string): boolean {
  const q = query.trim().toLowerCase()
  if (!q) return true
  if ((p.name || '').toLowerCase().includes(q)) return true
  if ((p.workingDir || '').toLowerCase().includes(q)) return true
  return (p.additionalDirs || []).some((r) => (r.path || '').toLowerCase().includes(q))
}

// LOOSE_LABEL is the app's word for the project-less bucket — the sidebar's last group, ⌘K's tag on
// such a conversation, and ⌘P's row for the bucket. One spelling, because ⌘P's row is also what a
// reader TYPES to reach the bucket (see looseMatches) and ⌘K's tag is what tells them a conversation
// ended up in it.
export const LOOSE_LABEL = '无项目'

// LOOSE_GROUP_KEY is the group key of that bucket, where a project id would otherwise go: the
// sidebar's Fragment key AND its row window are keyed by it (both through `groupKey`, App.tsx), and
// ⌘P's row for the bucket rides on the same key. One spelling, because the two palettes and the
// sidebar are three surfaces identifying one bucket.
export const LOOSE_GROUP_KEY = '__loose__'

// LOOSE_LABEL_HINT is the bucket's stand-in for a working directory, which is what a ⌘P row would
// otherwise show there: the bucket has no directory of its own — each of its sessions carries the
// workspace snapshot it was made with (see the backend resolver) — so the row says that instead.
export const LOOSE_LABEL_HINT = '各自用自己的工作区快照'

// looseMatches reports whether ⌘P's bucket row answers the query: by its own name, and by nothing
// else — a project's name or directory asked for that project, not for the bucket.
export function looseMatches(query: string): boolean {
  const q = query.trim().toLowerCase()
  return !q || LOOSE_LABEL.toLowerCase().includes(q)
}

// sessionChoices returns the ⌘K palette's rows: ONE per conversation — a compaction chain is one
// conversation, so its ancestors are folded away by the same rule the sidebar folds them with
// (sessionRows) — in the order the list arrived in.
//
// That order is `ListSessions`' own (most recently UPDATED first), which is what a switcher wants
// under an empty query, and taking it from the list is the sidebar's rule too: one ordering rule in
// the app rather than two that agree by accident. Pins are deliberately NOT applied: a pin lifts a
// row to the head of its own GROUP, and a flat list has no group to lift it inside — typing is the
// way to bring a pinned conversation up here.
//
// `busy` (when given) lifts one more class of row to the head: the conversations that are WORKING, or
// that are parked ON THE READER — an approval card or a question waiting for an answer. Asking for
// something is the one reason a reader opens this palette at all with a query they cannot type (they
// do not know the title of the conversation that just stopped for them), and "most recently written
// to" does not put those first: a conversation that is parked wrote nothing since it parked, so a
// long-running one falls below whatever finished in the meantime. The reader's own list order is kept
// inside each part — the two facts are one bucket on purpose, because which of them is more urgent is
// not something this list can know, and the row already says which one it is (see SessionPalette).
export function sessionChoices<T extends { id: string; compactedParentId?: string; compactedParentTitle?: string }>(
  list: T[],
  busy?: (id: string) => boolean,
): T[] {
  const rows = sessionRows(list).map((r) => r.session)
  if (!busy) return rows
  const active = rows.filter((s) => busy(s.id))
  if (active.length === 0 || active.length === rows.length) return rows
  return active.concat(rows.filter((s) => !busy(s.id)))
}

// sessionMatches reports whether a conversation answers the ⌘K query: a case-insensitive
// substring of its TITLE, and of nothing else.
//
// The title only, although the row also shows the conversation's project: this palette answers
// 「说过那句话的那次对话在哪」, and what a reader remembers a conversation BY is its name. Listed
// rows the query is not in would have no highlight to explain themselves, and a project already has
// its own search (⌘P) — which is what the palette's empty state points at.
export function sessionMatches(s: { title?: string }, query: string): boolean {
  const q = query.trim().toLowerCase()
  if (!q) return true
  return (s.title || '').toLowerCase().includes(q)
}

// fmtBytes renders a byte count the way the agent's own confirmation message
// does (pkg/strutil.HumanBytes), so "README.md · 12.4 KB" on the card and
// "✅ 文件 README.md (12.4 KB) 已交给用户" in the transcript agree.
export function fmtBytes(n: number): string {
  if (!isFinite(n) || n <= 0) return '0 B'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GB`
}

// toLocalAsset rewrites an image src that points at a local path so it can be
// served by the desktop's /local asset handler (see assetHandler in main.go).
// Absolute paths pass through; relative paths are resolved against workDir.
export function toLocalAsset(src: string | undefined, workDir: string): string {
  if (!src) return src || ''
  if (/^(https?:|data:|blob:|wails:)/.test(src)) return src
  if (src.startsWith('file://')) return `/local?p=${encodeURIComponent(src.slice('file://'.length))}`
  let p = src
  if (!p.startsWith('/')) p = `${workDir || ''}/${p.replace(/^\.\//, '')}`
  return `/local?p=${encodeURIComponent(p)}`
}

// linkTargetPath applies the same resolution to a markdown LINK: the absolute
// path its href names, or null when the href is not a file on disk at all (a web
// URL, a mailto:, an in-page #anchor).
//
// The two live side by side because they have to agree on what "a local path"
// means: a reply that links `[设计](./2026-09-14-desktop-project-design.md)` and
// one that embeds `![](./shot.png)` name the same directory — the session's
// working directory — and a link that resolved differently from the image beside
// it would be a bug with no principle behind it.
//
// Only paths are resolved, not `file:` URLs: the markdown pipeline drops those
// before any component sees them (react-markdown's own protocol allowlist, see
// keepInlineImage in markdown.tsx), and what replies actually contain is paths —
// verified against this repo's own transcripts, where relative links like
// `[desktop.md](desktop.md)` are common and `file://` appears only in prose
// about file URLs.
export function linkTargetPath(href: string, workDir: string): string | null {
  const url = href.trim()
  if (!url || url.startsWith('#')) return null
  // A scheme (`http:`, `mailto:`, `data:` …) means it is not a path. A bare
  // relative path has none, and neither does an absolute one.
  if (/^[a-z][a-z0-9+.-]*:/i.test(url)) return null
  if (url.startsWith('/')) return url
  // A relative path is only a path once there is a directory to resolve it
  // against: without one, `/etc`-style guessing would turn a link like
  // `[etc](etc)` into a file at the filesystem root. A caller with no working
  // directory (a finding's InlineMd) therefore gets no local target at all.
  if (!workDir) return null
  return `${workDir}/${url.replace(/^\.\//, '')}`
}

// toLocalPath is the same asset route in its path form (/local/<path>). It is
// what a previewed document is loaded through: with the disk path in the URL
// PATH, the document's own relative references ("./chart.js") resolve against it
// and come back to the same handler — under the query form the browser would
// resolve them against the app root and 404. Each segment is encoded separately
// so a "/" in the path stays a separator while a "#" or "?" in a FILE NAME is
// escaped rather than read as URL syntax.
export function toLocalPath(path: string): string {
  return '/local' + path.split('/').map(encodeURIComponent).join('/')
}

// ── references: @ a path, # an entity ───────────────────────────────────────
// Mirrors agent/atfile: a reference starts at the beginning of the text or after
// any whitespace, and runs to the next whitespace. Keeping the rule identical on
// both sides means the popup offers exactly the references the backend will expand
// when the message is sent.
export function isRefBoundary(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r'
}

// FILE_REF_MARK names a path; HASH_REF_MARK names an entity (agent/atfile.FileRefMark, HashRefMark).
// The split is what keeps a '#include' or an issue number out of the picker entirely: only the two
// prefixes below open a namespace, and anything else after the mark is ordinary text.
export const FILE_REF_MARK = '@'
export const HASH_REF_MARK = '#'

// HASH_AGENT_PREFIX and HASH_SESSION_PREFIX mirror agent/atfile.AgentRefPrefix and SessionRefPrefix:
// "#a:<node id>" names a node this machine may delegate to, "#s:<id>" another conversation. The
// spelling has to match the Go side exactly — the popup inserts it and the expansion reads it back —
// and only the backend knows what a reference expands to (a pointer, never the content).
export const HASH_AGENT_PREFIX = 'a:'
export const HASH_SESSION_PREFIX = 's:'

// HashNamespace is what a '#' query is asking about, mirroring atfile.HashNamespace.
export type HashNamespace = 'none' | 'candidates' | 'agents' | 'sessions'

// hashNamespace mirrors agent/atfile.HashQueryNamespace: which namespace a '#' query opens, and
// what is left to search for inside it. A query that matches no namespace — and no PREFIX of one —
// is 'none', which is the whole rule that keeps ordinary '#' text out of the picker. It lives here
// as the Go one lives in atfile: one rule, so the two frontends cannot answer the same keystrokes
// differently from each other or from the expansion.
export function hashNamespace(query: string): { ns: HashNamespace; rest: string } {
  if (query.startsWith(HASH_AGENT_PREFIX)) return { ns: 'agents', rest: query.slice(HASH_AGENT_PREFIX.length) }
  if (query.startsWith(HASH_SESSION_PREFIX)) return { ns: 'sessions', rest: query.slice(HASH_SESSION_PREFIX.length) }
  for (const prefix of [HASH_AGENT_PREFIX, HASH_SESSION_PREFIX]) {
    if (prefix.startsWith(query)) return { ns: 'candidates', rest: '' }
  }
  return { ns: 'none', rest: '' }
}

// atRefAt returns the reference the caret sits in — its start index, the mark it opens with, and the
// query typed after it — or null when the caret is not inside one. Both marks are returned: a bare
// '#' is a reference being started, and whether what follows is one at all is hashNamespace's call.
export function atRefAt(value: string, caret: number): { start: number; mark: string; query: string } | null {
  for (let i = caret - 1; i >= 0; i--) {
    const c = value[i]
    if (c === FILE_REF_MARK || c === HASH_REF_MARK) {
      if (i > 0 && !isRefBoundary(value[i - 1])) return null // attached to a word (e.g. an email, C#)
      return { start: i, mark: c, query: value.slice(i + 1, caret) }
    }
    if (isRefBoundary(c)) return null // left the reference without finding its mark
  }
  return null
}

// countAtRefs counts the references in value, used for the picker's
// "already referencing N" hint. A '#' counts only when it opens one of the two
// namespaces (the same rule the backend expands by): counting every '#' would make the hint lie
// about a message that says "#include".
export function countAtRefs(value: string): number {
  let n = 0
  for (let i = 0; i < value.length; i++) {
    if (i > 0 && !isRefBoundary(value[i - 1])) continue
    const c = value[i]
    if (c === FILE_REF_MARK) {
      n++
      continue
    }
    if (c === HASH_REF_MARK) {
      const end = atRefEnd(value, i)
      const body = value.slice(i + 1, end)
      if (body.startsWith(HASH_AGENT_PREFIX) || body.startsWith(HASH_SESSION_PREFIX)) n++
    }
  }
  return n
}

// atRefEnd returns the index just past the reference starting at start — the
// next whitespace, or the end of the text.
export function atRefEnd(value: string, start: number): number {
  let i = start + 1
  while (i < value.length && !isRefBoundary(value[i])) i++
  return i
}

// replaceRefText replaces the whole reference at start (not just up to the
// caret — the user may be editing mid-query) with text, returning the new value
// and the caret position after it. When the text after the reference already
// starts with whitespace, text's own trailing space is dropped and the caret
// skips past that existing separator — so accepting a match mid-sentence neither
// doubles the space nor glues the next keystroke onto the path.
export function replaceRefText(value: string, start: number, text: string): { value: string; caret: number } {
  const rest = value.slice(atRefEnd(value, start))
  const dropped = text.endsWith(' ') && rest !== '' && isRefBoundary(rest[0])
  const ins = dropped ? text.slice(0, -1) : text
  const next = value.slice(0, start) + ins + rest
  let caret = start + ins.length
  if (dropped) while (caret < next.length && isRefBoundary(next[caret])) caret++
  return { value: next, caret }
}

// insertRefText splices a reference (or a space-joined batch of them) into the
// value at caret, keeping a separating space so the reference is recognized.
// Returns the new value and the caret position after the inserted text.
export function insertRefText(value: string, caret: number, text: string, trailingSpace = true): { value: string; caret: number } {
  const before = value.slice(0, caret)
  const after = value.slice(caret)
  const lead = before === '' || isRefBoundary(before[before.length - 1]) ? '' : ' '
  const insert = lead + text + (trailingSpace ? ' ' : '')
  return { value: before + insert + after, caret: caret + insert.length }
}


// copyText writes text to the clipboard. Three paths, in the order that works HERE first — a copy
// that silently does nothing is the one failure mode this function exists to prevent:
//
//  1. the Wails runtime's own clipboard object: a call over the bridge to the NATIVE clipboard, and
//     the only path that does not depend on the webview's permissions at all.
//  2. `navigator.clipboard`, for a context where the runtime is not there (a browser, a test page).
//     It is undefined outside a "secure context" — which is what the app's own page is — so calling
//     it directly is exactly the bug this order fixes: `navigator.clipboard?.writeText(...)` reads
//     as a no-op and the clipboard stays empty.
//  3. the legacy execCommand path, which needs the text in the document AND a user gesture. Both are
//     true at a click, and it is what a webview without either of the above still honours.
export function copyText(text: string): void {
  if (!text) return
  const legacy = () => {
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    } catch { /* ignore */ }
  }
  const viaWeb = () => {
    try {
      navigator.clipboard.writeText(text).catch(legacy)
    } catch {
      legacy()
    }
  }
  try {
    Clipboard.SetText(text).catch(viaWeb)
  } catch {
    viaWeb()
  }
}


// RewindTarget is what "回退到这里" can do with a right-clicked message.
export type RewindTarget =
  | { kind: 'ok'; turn: number }
  | { kind: 'steer' }
  | { kind: 'none' }

// rewindTargetForMessage resolves a transcript message to the checkpointed turn that
// CONTAINS it, or explains why nothing can be done with it.
//
// Two things make this less obvious than it looks:
//
//   - The match is a RANGE test, not an equality one. A checkpoint records where a
//     turn's records BEGIN, and a turn's reminder wrapper is recorded before its user
//     message — so the prompt's own index is turn.records or turn.records+1 depending
//     on whether that turn had a reminder block.
//   - Only the turn's OWN prompt can start a rewind. An interjection is a user message
//     too, but it has no checkpoint (the loop checkpoints turns), and rewinding "from
//     an interjection" would quietly undo the work that happened before it — so it is
//     refused by name instead.
export function rewindTargetForMessage(
  msg: { recordIndex?: number; steer?: boolean; checkpointTurn?: number },
  turns: { turn: number; records: number }[],
): RewindTarget {
  if (msg.steer) return { kind: 'steer' }
  // A live bubble carries the turn outright (stamped when its turn completed); one loaded
  // from disk carries the record index instead and resolves against the boundaries.
  if (msg.checkpointTurn) {
    return turns.some((t) => t.turn === msg.checkpointTurn)
      ? { kind: 'ok', turn: msg.checkpointTurn }
      : { kind: 'none' }
  }
  if (msg.recordIndex == null) return { kind: 'none' }
  let found: { turn: number; records: number } | undefined
  for (const t of turns) {
    if (t.records <= msg.recordIndex) found = t
  }
  return found ? { kind: 'ok', turn: found.turn } : { kind: 'none' }
}

// ── 子系统拥有的会话 ──────────────────────────────────────────────────────────
//
// 今天是接收侧节点的 A2A 执行（session.KindA2AExecution；见 docs/agents/node.md）。桌面上
// 它出现为一个可以被打开、但**不能在这里被驱动**的会话：标题是从远端 prompt 生成的，
// 不标出来就等于把它当成读者自己的对话。
//
// 真正的边界不在前端。任何从这里发出的回合都会被 agent 入口拒掉（agent/managed.go），
// 输入框关掉只是把那次必然失败的尝试省下来。措辞与 TUI 的 managedRowSuffix、web 控制台
// 的 managedLabel 一致：同一个事实，三处同一句话。

/**
 * 这段对话来自哪台机器，只有名字（标签与说明句各自补自己的介词）。
 *
 * 名字是**本机**的说法：配置里的别名对着 pin 住的那份身份。绝不是对端自报的名字——任何密钥都能
 * 声称任何名字，所以只有本机 pin 住的那条记录能回答「这是哪台机器」。而那条记录是**出站**接触
 * 写下的，所以一个本机从没反连过的调用方解析不出名字：那时说的是「未知节点 + id 前几位」，而不是
 * 编一个名字或干脆不提——这是最常见的正确答案，不是缺数据（docs/agents/node.md）。
 */
function managedSourceName(sourceNodeId?: string, sourceNode?: string): string {
  if (!sourceNodeId) return ''
  return sourceNode ? sourceNode : `未知节点 ${sourceNodeId.slice(0, 8)}`
}

/** 这类会话的标签文本；普通会话返回空串。 */
export function managedLabel(kind?: string, sourceNodeId?: string, sourceNode?: string): string {
  if (!kind) return ''
  const name = managedSourceName(sourceNodeId, sourceNode)
  const from = name ? ` · 来自 ${name}` : ''
  return `子系统执行记录 ${kind}${from} · 只读`
}

/** 只读会话里代替输入框的那句话。 */
export function managedReadOnlyNote(kind: string, sourceNodeId?: string, sourceNode?: string): string {
  const name = managedSourceName(sourceNodeId, sourceNode)
  const from = name ? `，来自 ${name}` : ''
  return `这段对话由子系统（${kind}）驱动${from}，不归本窗口：可以在这里查看，但不能继续、回退或重试。` +
    `要基于它继续研究，请新建一段会话。`
}

// ── The right-hand columns' width rule ────────────────────────────────────────
// Two columns can occupy the right edge (the side-channel panel and the file tree), and they are
// the same layout problem: a width the reader chose, a window that may not have room for it, and a
// conversation that must keep a floor of its own. So the arithmetic lives here ONCE — each panel
// brings its own bounds.

// The conversation never gives up more than this, whatever the window size: a side panel that
// swallows the main thing is a bug.
const CHAT_MIN_WIDTH = 560
// …and on a window with room to spare the space is SHARED rather than handed over: the panel's room
// stops at half the content area. A bare floor is not enough — it pins the conversation AT the floor
// and lets the panel take every extra pixel, so a reader who dragged the panel wide saw 480px of
// prose beside an 840px diff on a 1600px window. Above ~1600 the two columns grow together; below
// it the floor is what protects the conversation.
const CHAT_MIN_SHARE = 0.5

// panelRoom is the widest a right-hand column may be SHOWN: the content area (the window, less the
// sidebar — the other column that never shrinks) less what the conversation keeps, capped at max.
// Read from the DOM rather than assumed from the CSS, so a folded sidebar hands its room back. In a
// window too narrow to honour both minimums the panel's own floor wins: something has to give there,
// and the conversation is what yields.
export function panelRoom(min: number, max: number): number {
  if (typeof window === 'undefined') return max
  const sidebar = document.querySelector('.sidebar')
  const used = sidebar ? sidebar.getBoundingClientRect().width : 0
  const content = window.innerWidth - used
  const keep = Math.max(CHAT_MIN_WIDTH, Math.round(content * CHAT_MIN_SHARE))
  return Math.max(min, Math.min(max, content - keep))
}

// clampPanelWidth limits a requested width to the room there is. The result is DISPLAY-ONLY: what
// gets stored stays the width the reader chose, so a narrow window limits what is shown without
// overwriting the choice — and widening the window gives the chosen width back.
export function clampPanelWidth(px: number, room: number, min: number): number {
  return Math.max(min, Math.min(room, Math.round(px)))
}
