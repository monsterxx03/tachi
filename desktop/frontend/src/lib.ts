import type { KeyboardEvent } from 'react'
import type { SessionMessage } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { Message } from './types'

export function extractReminder(content: string): { reminder: string; text: string } {
  const re = /<system-reminder>([\s\S]*?)<\/system-reminder>/g
  const blocks: string[] = []; let m
  while ((m = re.exec(content))) blocks.push(m[1].trim())
  const text = content.replace(re, '').trim()
  return { reminder: blocks.join('\n'), text }
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
  sms.forEach((sm) => {
    if (sm.role === 'reminder') { pendingReminder += (pendingReminder ? '\n' : '') + sm.content; return }
    if (sm.role === 'user') {
      const r = extractReminder(sm.content)
      const rem = r.reminder || pendingReminder || undefined
      turns.push({ id: nextTurnId(), role: 'user', text: r.text, reminder: rem, reminderCollapsed: rem ? true : undefined, ts: sm.timestamp || undefined })
      cur = null
      return
    }
    if (!cur || cur.role !== 'assistant') {
      cur = { id: nextTurnId(), role: 'assistant', parts: [], ts: sm.timestamp || undefined }
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

// fmtBytes renders a byte count the way the agent's own confirmation message
// does (pkg/strutil.HumanBytes), so "README.md · 12.4 KB" on the card and
// "✅ 文件 README.md (12.4 KB) 已加入发送队列" in the transcript agree.
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

// ── @-file references ───────────────────────────────────────────────────────
// Mirrors agent/atfile.IsRefBoundary: a reference starts at the beginning of
// the text or after any whitespace, and runs to the next whitespace. Keeping
// the rule identical on both sides means the popup offers exactly the
// references the backend will expand when the message is sent.
export function isRefBoundary(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r'
}

// atRefAt returns the @-reference the caret sits in — its start index and the
// query typed after it — or null when the caret is not inside one.
export function atRefAt(value: string, caret: number): { start: number; query: string } | null {
  for (let i = caret - 1; i >= 0; i--) {
    const c = value[i]
    if (c === '@') {
      if (i > 0 && !isRefBoundary(value[i - 1])) return null // attached to a word (e.g. an email)
      return { start: i, query: value.slice(i + 1, caret) }
    }
    if (isRefBoundary(c)) return null // left the reference without finding its @
  }
  return null
}

// countAtRefs counts the @-references in value, used for the picker's
// "already referencing N" hint.
export function countAtRefs(value: string): number {
  let n = 0
  for (let i = 0; i < value.length; i++) {
    if (value[i] === '@' && (i === 0 || isRefBoundary(value[i - 1]))) n++
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


// copyText writes text to the clipboard. The async clipboard API can be
// unavailable or reject in a webview without permission, so a hidden textarea
// (the legacy execCommand path) stays as the fallback.
export function copyText(text: string): void {
  if (!text) return
  const fallback = () => {
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    } catch { /* ignore */ }
  }
  try {
    navigator.clipboard.writeText(text).catch(fallback)
  } catch {
    fallback()
  }
}
