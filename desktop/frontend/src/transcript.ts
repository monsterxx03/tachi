// Transcript mutations: the pure half of the live view's message model.
//
// A live turn accumulates the same ordered `parts` array a rebuilt transcript has (see
// buildTurns), so both render through one model — these helpers are what a streaming event
// applies to it. They were lifted out of App.tsx because they hold no React state: reading
// or changing them should not mean reading a 2000-line component.

import type { Message, Part } from './types'
import type { FileChangeVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

// ── Ordered turn parts ──────────────────────────────────────────────────────
// A live turn accumulates the SAME ordered `parts` array a rebuilt transcript
// has (see buildTurns): deltas extend the trailing part of their own kind, a
// tool start appends a part, its result closes that part. Without this the
// live view sorted everything into fixed buckets (all tools, then all text),
// so a multi-step turn rendered in the wrong order until a restart rebuilt it.
export function appendPartDelta(m: Message, type: 'thinking' | 'text', delta: string): Message {
  if (!delta) return m
  const parts = [...(m.parts || [])]
  const last = parts[parts.length - 1]
  if (last && last.type === type) parts[parts.length - 1] = { ...last, text: (last.text || '') + delta }
  else parts.push({ type, text: delta })
  return { ...m, parts }
}

export function pushPart(m: Message, p: Part): Message {
  return { ...m, parts: [...(m.parts || []), p] }
}

// finishToolPart closes the newest still-running call of the named tool.
export function finishToolPart(m: Message, name: string, summary: string, ok: boolean, durationMs?: number): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i]
    if (p.type === 'tool' && !p.done && p.name === name) {
      parts[i] = { ...p, summary, ok, done: true, durationMs }
      break
    }
  }
  return { ...m, parts }
}

// finishNotice completes the newest in-flight notice (auto-compaction pushes one
// when it starts and fills in its outcome here). A notice that never had a start
// — the window was opened while a compaction was already running — is appended
// as an already-finished one rather than dropped.
export function finishNotice(m: Message, label: string, summary?: string): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    if (parts[i].type === 'notice' && parts[i].done === false) {
      parts[i] = { ...parts[i], label, summary, done: true }
      return { ...m, parts }
    }
  }
  return { ...m, parts: [...parts, { type: 'notice', label, summary, done: true }] }
}

// updateToolPart fills in the human-readable title/args (and the derived change) for
// the newest in-flight call, pushed by the separate agent:tool event. That event
// fires twice — once when the model starts the call (args still empty) and once with
// the complete arguments right before execution — so the second patch is what
// actually brings the diff in.
export function updateToolPart(m: Message, name: string, title: string, args: string, change?: FileChangeVO | null): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i]
    if (p.type === 'tool' && !p.done && p.name === name) {
      parts[i] = { ...p, title, args, change: change ?? p.change }
      break
    }
  }
  return { ...m, parts }
}

// setPartDiffs sets diffOpen on every diff-carrying part of ONE message: the turn
// footer's chip opens/closes the whole turn at once. value undefined = toggle each
// part on its own current state... which is why the chip passes an explicit value.
export function setPartDiffs(m: Message, value: boolean): Message {
  return {
    ...m,
    parts: (m.parts || []).map((p) => (p.type === 'tool' && p.change ? { ...p, diffOpen: value } : p)),
  }
}

// togglePartDiff flips one card's diff (the per-card entry).
export function togglePartDiff(m: Message, index: number): Message {
  return {
    ...m,
    parts: (m.parts || []).map((p, i) => (i === index && p.type === 'tool' ? { ...p, diffOpen: !p.diffOpen } : p)),
  }
}

// turnDiffStat aggregates a turn's changes for the footer chip: files deduped by path
// (the same file edited twice counts once), fragment line counts summed, and whether
// the turn ran shell commands at all — whose changes never show up in a diff derived
// from tool arguments.
export function turnDiffStat(parts: Part[] | undefined): { files: number; added: number; removed: number; shell: boolean; paths: string[] } {
  const byPath = new Map<string, { added: number; removed: number }>()
  let shell = false
  for (const p of parts || []) {
    if (p.type !== 'tool') continue
    if (p.name === 'Bash') shell = true
    if (!p.change || !p.done || !p.ok) continue
    const cur = byPath.get(p.change.path) || { added: 0, removed: 0 }
    byPath.set(p.change.path, { added: cur.added + (p.change.added || 0), removed: cur.removed + (p.change.removed || 0) })
  }
  let added = 0
  let removed = 0
  for (const v of byPath.values()) {
    added += v.added
    removed += v.removed
  }
  return { files: byPath.size, added, removed, shell, paths: [...byPath.keys()] }
}

// closeOpenToolParts marks every unfinished call as aborted — used when a turn
// is stopped/errors and no tool_result will ever arrive.
export function closeOpenToolParts(parts: Part[] | undefined, summary: string): Part[] {
  return (parts || []).map((p) => (p.type === 'tool' && !p.done ? { ...p, ok: false, done: true, summary } : p))
}

export function lastRunningAssistantIndex(list: Message[]): number {
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].role === 'assistant' && list[i].running) return i
  }
  return -1
}

// imeActive reports whether a key event belongs to an IME composition (Chinese
// / Japanese / Korean candidate selection). Enter while composing confirms a
// candidate — treating it as "submit" is the classic IME bug. keyCode 229 is
// the fallback some WebKit builds report instead of setting isComposing.
