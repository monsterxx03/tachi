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
      turns.push({ id: `h-${turns.length}`, role: 'user', text: r.text, reminder: rem, reminderCollapsed: rem ? true : undefined, ts: sm.timestamp || undefined })
      cur = null
      return
    }
    if (!cur || cur.role !== 'assistant') {
      cur = { id: `h-${turns.length}`, role: 'assistant', parts: [], ts: sm.timestamp || undefined }
      turns.push(cur)
    }
    if (!cur.parts) cur.parts = []
    if (sm.role === 'assistant') {
      if (sm.thinking) cur.parts.push({ type: 'thinking', text: sm.thinking })
      if (sm.content) cur.parts.push({ type: 'text', text: sm.content })
      cur.ts = cur.ts || sm.timestamp
    } else if (sm.role === 'tool_call') {
      cur.parts.push({ type: 'tool', name: sm.toolName, title: sm.title, args: sm.args, summary: '执行中…', ok: true, done: false, toolCallId: sm.toolCallId })
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
