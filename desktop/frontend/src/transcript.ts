// Transcript mutations: the pure half of the live view's message model.
//
// A live turn accumulates the same ordered `parts` array a rebuilt transcript has (see
// buildTurns), so both render through one model — these helpers are what a streaming event
// applies to it. They were lifted out of App.tsx because they hold no React state: reading
// or changing them should not mean reading a 2000-line component.

import { fileFromSendFileArgs, fmtDur } from './lib'
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

// ── Turn view: the process fold ─────────────────────────────────────────────
//
// A long turn renders one row per part (thinking, tool card, intermediate text), so a
// 30-step turn is 30 rows of skeleton. The noise is the ROW COUNT — every part is already
// collapsed on its own (ThinkingPart defaults to collapsed, ToolCard to folded) — so the
// conversation folds a turn's PROCESS into one strip and keeps its CONCLUSION visible.
// What must never be hidden stays outside the fold: a failed call, the call a permission
// card is parked on, the call an AskUserQuestion form is waiting on, the call that is
// running right now, and notices.
//
// A DELIVERED attachment is not process either, and unlike the rest it does not stay in its
// own place: a call that handed a file over IS the file card, so it is pinned to the turn's
// TAIL (App renders view.attachments after the conclusion). A file the agent sent is the
// point of the turn, not a step of it — behind the fold it did not merely collapse, it was
// never in the DOM at all, so the reader had to open the process row to find what they asked
// for. It is also NOT rendered inside the timeline: the strip stands in for the process, and
// one card must never appear twice on one turn.
//
// Rendering the timeline is still TurnPart's job: the one-off panel replays the same parts
// and exists to show them all, so the fold belongs to the conversation, not to the part
// renderer. This helper only decides WHAT is visible, and it is pure so both render paths
// (live and rebuilt) share it.
// See docs/2026-09-13-desktop-transcript-density-design.md.

export interface IndexedPart {
  part: Part
  index: number
}

// ProcessSummary counts a turn's process. Every field is derived from the parts alone —
// never from prose, and never from a model call: a summary that needed an API call would
// cost a request per turn and could disagree with the history after a reload.
export interface ProcessSummary {
  steps: number // tool calls
  thinking: number // thinking blocks
  notes: number // intermediate prose folded away (not the conclusion)
  failed: number // tool calls that did not succeed
  files: number // ReadFile
  edits: number // EditFile / WriteFile
  sent: number // SendFile — shown at the turn's tail, so the row must not call them 其它
  commands: number // Bash
  searches: number // Grep / Glob / WebSearch / WebFetch / MCP search
  other: number // everything else (SubAgent, SavePlan, Skill, …)
}

// LiveStep is the call a running turn is executing RIGHT NOW: what the strip reports while
// the turn is in flight, so "still working" is visible without watching cards scroll by.
export interface LiveStep {
  name: string
  title?: string
  // step is its ordinal among the turn's tool calls (第 N 步) — with the finished ones
  // counted, because the reader is following the turn, not a card.
  step: number
}

export interface TurnView {
  summary: ProcessSummary
  // folded: what the strip hides until the reader opens it.
  folded: IndexedPart[]
  // exposed: parts that stay visible whether or not the strip is open.
  exposed: IndexedPart[]
  // attachments: the turn's delivered files (a SendFile call), to be rendered as a group at
  // the turn's TAIL — after the conclusion, in the order they were sent. Never in `folded`
  // (see the header comment) and never in the timeline.
  attachments: IndexedPart[]
  // conclusion: the turn's LAST prose — the answer, always visible.
  conclusion: IndexedPart | null
  // live: the call running right now, or null (finished, between calls, or parked on a form).
  live: LiveStep | null
}

export interface TurnViewOptions {
  // The tool call a permission card is parked on (matched by call id).
  permissionToolCallId?: string
  // An AskUserQuestion form is waiting for an answer.
  pendingAsk?: boolean
}

export function turnView(parts: Part[] | undefined, opts: TurnViewOptions = {}): TurnView {
  const list = parts || []

  let conclusionIndex = -1
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].type === 'text') {
      conclusionIndex = i
      break
    }
  }
  // The newest unfinished call: it supplies the strip's LIVE line ("正在 X · 第 N 步"), but it is
  // NOT exposed. An exposed running card was the P1 stopgap (with no live row, it was the only
  // thing answering "is it still working?"), and it cost a row that appeared and vanished once
  // per step — the turn's prose jumped down and back up as each call started and finished. The
  // live row says the same thing in place, and a reader who wants the card itself opens the
  // timeline, where it stays put and updates in place.
  let runningIndex = -1
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].type === 'tool' && !list[i].done) {
      runningIndex = i
      break
    }
  }
  let askIndex = -1
  if (opts.pendingAsk) {
    askIndex = list.findIndex((p) => p.type === 'tool' && p.name === 'AskUserQuestion' && !p.done)
  }
  // A call parked on a permission card is WAITING, not running: its card is replaced by the form
  // (which says so itself), and the live row must not claim it is 正在执行.
  let parkedIndex = -1
  if (opts.permissionToolCallId) {
    parkedIndex = list.findIndex((p) => p.type === 'tool' && p.toolCallId === opts.permissionToolCallId)
  }

  const summary: ProcessSummary = { steps: 0, thinking: 0, notes: 0, failed: 0, files: 0, edits: 0, sent: 0, commands: 0, searches: 0, other: 0 }
  const folded: IndexedPart[] = []
  const exposed: IndexedPart[] = []
  const attachments: IndexedPart[] = []
  let live: LiveStep | null = null

  list.forEach((part, index) => {
    if (part.type === 'tool') {
      summary.steps++
      if (index === runningIndex && index !== parkedIndex && index !== askIndex) {
        live = { name: part.name || '', title: part.title, step: summary.steps }
      }
      if (part.done && !part.ok) summary.failed++
      countTool(summary, part.name || '')
    } else if (part.type === 'thinking') {
      summary.thinking++
    } else if (part.type === 'text') {
      summary.notes++
    }

    // A call that HANDED A FILE OVER is not process: the card is what the reader asked for, so it
    // goes to the tail group instead of the fold. The predicate is deliberately the renderer's own
    // (TurnPart draws a FileCard for exactly these), which is why a FAILED send stays put as a
    // failure card — it delivered nothing — and a call whose args do not parse stays a tool card.
    if (deliveredAttachment(part)) {
      attachments.push({ part, index })
      return
    }

    const staysVisible =
      part.type === 'notice' ||
      index === conclusionIndex ||
      index === askIndex ||
      index === parkedIndex ||
      (part.type === 'tool' && part.done && !part.ok)
    if (staysVisible) exposed.push({ part, index })
    else folded.push({ part, index })
  })

  // A folded prose part is a NOTE, never the conclusion; keep the count honest by
  // excluding the conclusion from it.
  if (conclusionIndex >= 0) summary.notes--

  let conclusion: IndexedPart | null = null
  if (conclusionIndex >= 0) {
    const at = exposed.findIndex((it) => it.index === conclusionIndex)
    conclusion = at >= 0 ? exposed.splice(at, 1)[0] : null
  }
  return { summary, folded, exposed, attachments, conclusion, live }
}

// deliveredAttachment reports whether a part IS the file card of a SendFile that succeeded.
function deliveredAttachment(part: Part): boolean {
  return part.type === 'tool' && part.name === 'SendFile' && !!part.done && !!part.ok
    && fileFromSendFileArgs(part.args || '') !== null
}

// countTool buckets a tool call for the strip's mix line. Unknown names (a new tool, an
// MCP server's) land in `other` rather than being dropped.
function countTool(s: ProcessSummary, name: string): void {
  switch (name) {
    case 'ReadFile':
      s.files++
      return
    case 'EditFile':
    case 'WriteFile':
      s.edits++
      return
    case 'SendFile':
      s.sent++
      return
    case 'Bash':
      s.commands++
      return
    case 'Grep':
    case 'Glob':
    case 'WebSearch':
    case 'WebFetch':
    case 'MCPSearchTools':
      s.searches++
      return
  }
  if (name.startsWith('mcp__')) s.searches++
  else s.other++
}

// processSummaryLine renders the strip's middle: only the categories that happened.
export function processSummaryLine(s: ProcessSummary): string {
  const bits: string[] = []
  if (s.files) bits.push(`读了 ${s.files} 个文件`)
  if (s.edits) bits.push(`改了 ${s.edits} 个文件`)
  if (s.sent) bits.push(`发了 ${s.sent} 个文件`)
  if (s.commands) bits.push(`跑了 ${s.commands} 条命令`)
  if (s.searches) bits.push(`搜了 ${s.searches} 次`)
  if (s.other) bits.push(`其它 ${s.other}`)
  if (s.notes > 0) bits.push(`含 ${s.notes} 段过程说明`)
  return bits.join(' · ')
}

// processLiveLine is the "what is happening now" line, shared by the turn's strip and the
// composer's activity row: one fact, one wording — a second phrasing would drift.
export function processLiveLine(live: LiveStep, elapsedMs?: number): string {
  const head = `正在 ${live.name}${live.title ? ` · ${live.title}` : ''} · 第 ${live.step} 步`
  return elapsedMs ? `${head} · ${fmtDur(elapsedMs)}` : head
}

// processStepLabel is the strip's leading count. A turn with only reasoning shows its
// thinking blocks instead of a meaningless "0 步".
export function processStepLabel(s: ProcessSummary): string {
  if (s.steps > 0) return `${s.steps} 步`
  if (s.thinking > 0) return `思考 ${s.thinking} 段`
  return ''
}

// imeActive reports whether a key event belongs to an IME composition (Chinese
// / Japanese / Korean candidate selection). Enter while composing confirms a
// candidate — treating it as "submit" is the classic IME bug. keyCode 229 is
// the fallback some WebKit builds report instead of setting isComposing.
