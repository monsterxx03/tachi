import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { DiffBlock } from './diff'
import { actOnKey, copyText, fmtDur, fmtShare, humanize, imeActive } from './lib'
import type { MCPAuthState } from './agentEvents'
import type { Theme } from './theme'
import { processLiveState, processStepLabel, processSummaryLine, type LiveStep, type ProcessSummary } from './transcript'
import type { AtMatch, Part, PermissionDecision, PermissionKind, PermissionRequest } from './types'
import type { CommandVO, ContextInfoVO, FileChangeVO, MCPToolSchemaVO, SessionRootsVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

// MoreRows is the tail of a group's row WINDOW in the sidebar (App.tsx owns the window itself): the
// line that says how many of that group's conversations are not rendered yet, and the thing that
// renders the next slice of them. The sidebar grows without bound — this machine has a hundred
// project-less sessions — and the reader is looking at the newest handful, so the rest is scroll
// nobody asked to paint.
//
// Two ways in, ONE action. Reaching the line by scrolling grows the window: the sidebar is a
// scroller, and arriving at the end of what is on screen IS the request for more. A click does the
// same for a reader who would rather not scroll, and it is the only thing to do there, so a real
// <button> (Enter/Space included) rather than a div with a role. The margin pre-loads a little
// before the line is actually visible, so the list does not visibly stop and then start again.
//
// It is a NOTE when it does nothing, and that is not a state: the caller renders it only while rows
// are hidden (App.tsx), so the two readings can never disagree with the list above it.
function MoreRows({ hidden, onGrow }: { hidden: number; onGrow: () => void }) {
  const ref = useRef<HTMLButtonElement>(null)
  // The action lives in a ref, and the observer is created ONCE per mounted line. `onGrow` is a
  // fresh closure on every render of its parent (App maps the groups inline), so a dependency on it
  // would tear the observer down and build a new one on each of them — during a turn, once per
  // streamed frame. That is not merely wasteful: a brand-new observer reports the CURRENT
  // intersection immediately, so a line already on screen would grow the window again and again.
  const grow = useRef(onGrow)
  useEffect(() => { grow.current = onGrow })
  useEffect(() => {
    const el = ref.current
    // No IntersectionObserver (an old runtime, or a test harness): the click still works, and the
    // line stops being an affordance that promises nothing.
    if (!el || typeof IntersectionObserver === 'undefined') return
    const obs = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) grow.current()
    }, { rootMargin: '160px 0px' })
    obs.observe(el)
    return () => obs.disconnect()
  }, [])
  return (
    <button type="button" className="session-more" ref={ref} onClick={() => grow.current()}
      title="展开更早的会话（滚到这里也会自动展开）">
      还有 {hidden} 条 ▾
    </button>
  )
}

// ContextRing is the meter itself: used fraction of the context window as a
// ring. Purely decorative — ContextMeter (below) owns the button semantics and
// the popover, so the SVG carries no title/role of its own.
function ContextRing({ estimate, window: w }: { estimate: number; window: number }) {
  const pct = w > 0 ? Math.min(100, (estimate / w) * 100) : 0
  const r = 8, c = 2 * Math.PI * r
  const off = c - (pct / 100) * c
  return (
    <svg className="ctx-ring" width="22" height="22" viewBox="0 0 22 22" aria-hidden="true">
      <circle cx="11" cy="11" r={r} fill="none" stroke="var(--border)" strokeWidth="2.5" />
      <circle cx="11" cy="11" r={r} fill="none" stroke={pct > 80 ? 'var(--amber)' : 'var(--accent)'} strokeWidth="2.5" strokeDasharray={c} strokeDashoffset={off} strokeLinecap="round" transform="rotate(-90 11 11)" />
    </svg>
  )
}

// ContextMeter is the composer's context ring plus the popover behind it: the
// ring answers "how full is the window", the popover answers "full of WHAT" —
// the same buckets /usage prints (system prompt, tool schemas, the
// conversation, tool output), from the agent's own estimate (see
// desktop/contextinfo.go). Fetched on open rather than polled: the numbers only
// move when a turn runs.
function ContextMeter({ sessionId, estimate, window: w }: { sessionId: string; estimate: number; window: number }) {
  const [open, setOpen] = useState(false)
  const [info, setInfo] = useState<ContextInfoVO | null>(null)
  const [loading, setLoading] = useState(false)
  const boxRef = useRef<HTMLDivElement>(null)

  const pct = w > 0 ? Math.min(100, (estimate / w) * 100) : 0
  const label = w > 0 ? `上下文 ${pct.toFixed(1)}%（${humanize(estimate)} / ${humanize(w)}）` : '上下文 —'

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setInfo((await AgentService.GetContextInfo(sessionId)) || null)
    } catch {
      setInfo(null) // no agent yet (simulated mode) — the panel says so
    } finally {
      setLoading(false)
    }
  }, [sessionId])

  // Fetch on open, and again when the session or the estimate moves while it
  // stays open — otherwise a popover left open during a turn would keep showing
  // the numbers from the moment it was opened. estimate only changes per API
  // call, so this is one cheap IPC per turn iteration, not a poll.
  useEffect(() => {
    if (open) void load()
  }, [open, load, estimate])

  // Same dismissal contract as the MCP panel: click outside, or Esc.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || boxRef.current?.contains(t) || t.closest('.ctx-btn')) return
      setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const toggle = () => setOpen((v) => !v)

  return (
    // The wrapper is the popover's containing block: the panel cannot live
    // inside the <button> (a button may not contain a block element, and clicks
    // would land on it), so the ring's box is what the panel hangs off.
    <span className="popover-anchor">
      {open ? (
        <div className="popover-panel ctx-panel" ref={boxRef}>
          <ContextPanel info={info} loading={loading} />
        </div>
      ) : null}
      <button type="button" className="ctx-btn" aria-label={`${label}，点击查看明细`} aria-expanded={open} onClick={toggle}>
        <ContextRing estimate={estimate} window={w} />
      </button>
    </span>
  )
}

// ContextPanel is the popover body: one stacked bar on top, one row per bucket
// below it. Buckets come from Go already labelled and already filtered.
function ContextPanel({ info, loading }: { info: ContextInfoVO | null; loading: boolean }) {
  const est = info?.estimate || 0
  const win = info?.contextWindow || 0
  const parts = info?.parts || []

  return (
    <>
      <div className="ctx-head">
        <span className="ctx-title">上下文占用</span>
        <span className="ctx-total">{info === null && loading ? '读取中…' : win > 0 ? `${humanize(est)} / ${humanize(win)}` : humanize(est)}</span>
      </div>
      {est > 0 && win > 0 ? <div className="ctx-sub">{pctOf(est, win)} 的上下文窗口</div> : null}
      {est <= 0 ? (
        <div className="ctx-empty">还没有可用的估算<br />下一轮对话后可见</div>
      ) : parts.length === 0 ? (
        <div className="ctx-empty">分项明细需下一轮对话后可见<br />（当前总量来自历史会话）</div>
      ) : (
        <>
          {/* flex-grow carries the proportion, so the bar needs no rounding math
              and can never fall short of 100% because of it. */}
          <div className="ctx-bar">
            {parts.map((p) => (
              <span key={p.key} className={`ctx-seg ctx-c-${p.key}`} style={{ flexGrow: p.tokens }} />
            ))}
          </div>
          <div className="ctx-rows">
            {parts.map((p) => (
              <div className="ctx-row" key={p.key}>
                <span className={`ctx-dot ctx-c-${p.key}`} />
                <span className="ctx-name">{p.label}</span>
                <span className="ctx-tokens">{humanize(p.tokens)}</span>
                <span className="ctx-pct">{fmtShare(p.tokens / est)}</span>
              </div>
            ))}
          </div>
        </>
      )}
      {/* The total is anchored on what the provider billed (see the Go side,
          convState.contextEstimate) while the split is the local character
          estimate — so the caveat names both instead of disowning the number
          as "not API usage", which stopped being true once it was anchored.
          Total first: it is the line above the parts. */}
      <div className="ctx-foot">总量以最近一次 API 用量为准，分项为本地 chars/4 估算</div>
    </>
  )
}

function pctOf(part: number, whole: number): string {
  return `${((part / whole) * 100).toFixed(1)}%`
}

// CacheRing renders the cache-hit rate as a ring (like ContextRing for
// context usage). Color tiers: low red / mid amber / high green.
function CacheRing({ rate }: { rate: number }) {
  const pct = Math.min(100, rate * 100)
  const r = 8, c = 2 * Math.PI * r
  const off = c - (pct / 100) * c
  const color = pct >= 60 ? 'var(--green)' : pct >= 30 ? 'var(--amber)' : 'var(--red)'
  const title = `缓存命中率 ${pct.toFixed(2)}%`
  return (
    <svg className="ctx-ring" width="22" height="22" viewBox="0 0 22 22" role="img" aria-label={title}>
      <title>{title}</title>
      <circle cx="11" cy="11" r={r} fill="none" stroke="var(--border)" strokeWidth="2.5" />
      <circle cx="11" cy="11" r={r} fill="none" stroke={color} strokeWidth="2.5" strokeDasharray={c} strokeDashoffset={off} strokeLinecap="round" transform="rotate(-90 11 11)" />
    </svg>
  )
}

function ThinkingPart({ text }: { text: string }) {
  const [c, setC] = useState(true)
  return <ThinkingBlock thinking={text} collapsed={c} onToggle={() => setC(!c)} />
}

function ThinkingBlock({ thinking, collapsed, onToggle }: { thinking: string; collapsed: boolean; onToggle: () => void }) {
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => { if (!collapsed && bodyRef.current) bodyRef.current.scrollTop = bodyRef.current.scrollHeight }, [thinking, collapsed])
  const lines = thinking.split('\n')
  return (
    <div className="thinking-block">
      <div className={`thinking-head${collapsed ? '' : ' open'}`} onClick={onToggle} onKeyDown={actOnKey(onToggle)} tabIndex={0} title={collapsed ? '点击展开思考过程' : '点击收起思考过程'} role="button" aria-expanded={!collapsed}>
        <span className="thinking-ico"><Caret open={!collapsed} /></span><span className="thinking-label">thinking</span>
      </div>
      {!collapsed && <div className="thinking-body" ref={bodyRef}>{lines.map((l, i) => <div key={i} className="thinking-line">{l}</div>)}</div>}
    </div>
  )
}

// NoticePart renders a transcript notice: a one-line event that is not part of
// the conversation (auto-compaction, so far). Its body — the generated history
// summary — stays folded, because the point of the notice is that something
// happened to the context, not to re-read what it produced.
function NoticePart({ part }: { part: Part }) {
  const [open, setOpen] = useState(false)
  const body = part.summary || ''
  return (
    <div className="notice-block">
      <div
        className={`notice-head${part.done === false ? ' running' : ''}${open ? ' open' : ''}${body ? '' : ' static'}`}
        role={body ? 'button' : undefined}
        tabIndex={body ? 0 : undefined}
        aria-expanded={body ? open : undefined}
        title={body ? (open ? '点击收起摘要' : '点击查看压缩摘要') : undefined}
        onClick={body ? () => setOpen((v) => !v) : undefined}
        onKeyDown={body ? actOnKey(() => setOpen((v) => !v)) : undefined}
      >
        <span className="notice-ico">⟳</span>
        <span className="notice-label">{part.label || ''}</span>
        {body ? <span className="notice-toggle">{open ? '收起' : '摘要'}</span> : null}
      </div>
      {open && body ? <div className="notice-body">{body}</div> : null}
    </div>
  )
}

// RollingLine renders one line of text that ROLLS when it changes — the outgoing line slides up
// out of view while the new one comes up from below — instead of swapping instantly. An instant
// swap is what makes a fast turn look like a flicker: the model goes 思考 → 工具 → 思考 faster
// than a reader can register a word, and what lands on screen is unrelated labels flashing.
//
// Two rules keep the animation from fighting its own input:
//
//   - a MINIMUM DWELL before the next change is drawn (rollDwellMs). A change that arrives while
//     the current line is still young is HELD, and the held value is always the LATEST one — so a
//     burst shows a few readable states and ends on the true current one, instead of restarting
//     the animation every 50ms (an animation that never plays out is a stutter, not motion);
//   - a React `key` on the animated box, so each accepted change starts a fresh roll from the top
//     rather than continuing a stale transform.
//
// Reduced motion: base.css's ONE explicit list decides, and it does not freeze this — it swaps the
// slide for a fade (widgets.css's roll-in), because a swap that becomes an instant cut is the thing
// the roll exists to fix. Nothing to do here; the two rules above are about pacing, not decoration.
const rollDwellMs = 260

function RollingLine({ text, className }: { text: string; className?: string }) {
  const [pair, setPair] = useState<{ from: string | null; to: string; seq: number }>({ from: null, to: text, seq: 0 })
  const latest = useRef(text)
  const shownAt = useRef(Date.now())
  const timer = useRef<number | null>(null)

  useEffect(() => {
    latest.current = text
    if (text === pair.to) return
    const apply = () => {
      timer.current = null
      const next = latest.current
      // The last one wins: a burst that lands inside the dwell collapses into one roll onto the
      // newest state, which is the one the reader has to end up seeing.
      setPair((p) => (next === p.to ? p : { from: p.to, to: next, seq: p.seq + 1 }))
      shownAt.current = Date.now()
    }
    const wait = Math.max(0, rollDwellMs - (Date.now() - shownAt.current))
    if (wait === 0) {
      apply()
      return
    }
    if (timer.current === null) timer.current = window.setTimeout(apply, wait)
  }, [text, pair.to])

  useEffect(() => () => { if (timer.current !== null) window.clearTimeout(timer.current) }, [])

  // The leaving line is dropped when its animation ends, and that is not tidiness: it is clipped
  // but still TEXT (it would show up in a text extraction, and a reader of the DOM — including a
  // smoke driver sampling the row — would see two states where the screen shows one). Dropping it
  // also drops the `rolling` class, which takes the animation away; with the line gone the remaining
  // one sits where the animation had just left it (top of the box, both for the slide and for the
  // reduced-motion fade — see base.css's .roll rule, which hides the leaving line entirely).
  // One handler serves both: animationend bubbles from whichever element carries the animation.
  const rolling = pair.from !== null
  return (
    <span className={`roll${rolling ? ' rolling' : ''}${className ? ' ' + className : ''}`}>
      <span className="roll-inner" key={pair.seq}
        onAnimationEnd={() => setPair((p) => (p.from === null ? p : { ...p, from: null }))}>
        {rolling ? <span className="roll-line">{pair.from}</span> : null}
        <span className="roll-line">{pair.to}</span>
      </span>
    </span>
  )
}

// ReviewChip is one of a turn's two review entries. They differ only in SCOPE — this turn's
// checkpoint pair, or the whole working tree against HEAD — while the state machine is identical:
// 评审x → 评审中… → 已评审 N 条 · 查看, where the last two LOOK at a run (one to watch it work, one
// to read it) and only the first starts one. Written once and told which by `scope`, because two
// copies of a four-state chip is how they drift apart.
function ReviewChip({ label, scope, pending, done, notice, busy, onClick }: {
  label: string
  scope: string
  pending: boolean
  done?: string
  notice?: string
  busy: boolean
  onClick: () => void
}) {
  return (
    <>
      <button type="button" className="diff-chip"
        /* Both 「评审中…」 and 「已评审 · 查看」 LOOK at a run — one to watch it work, one to
           read it — so both stay clickable. Only STARTING a review waits for the turn to
           finish. (Disabling on sessionBusy alone is what made the chip unclickable while
           it was busy running the very review it was reporting on.) */
        disabled={!pending && !done && busy}
        title={pending ? '评审进行中（过程在右侧面板）'
          : done ? '看这次评审的过程、意见与报告（右侧面板）'
            : busy ? '等这一轮跑完' : scope}
        onClick={onClick}>
        {pending ? '评审中…' : (done || label)}
      </button>
      {/* The refusal reason is a TOOLTIP, not a paragraph: printed in full here it shoved the
          chips around (the sentence is a whole line), and hovering is the honest place for the
          detail — 「完整 diff」 spells out the same thing. This is per ENTRY, not per turn: one
          can refuse (nothing of its own to review) while the other has plenty. */}
      {notice ? <span className="diff-notice" title={notice}>⚠ 没有开始评审</span> : null}
    </>
  )
}

// ProcessStrip is one turn's process, folded into a single row: it stands in for every
// thinking block, tool card and intermediate message of that turn, which stay behind the
// "展开时序" toggle. The row is the summary — steps, what the tools touched, and whether
// anything failed — so the turn's answer is what the reader lands on.
//
// It renders ONLY the row; the timeline belongs to the caller, which owns the open state
// (per turn, presentational) and the part renderer. Failure is not hidden: the count turns
// the row red, and the failed cards themselves stay outside the fold (see turnView).
function ProcessStrip({ summary, open, onToggle, foldable, live, elapsedMs }: {
  summary: ProcessSummary
  open: boolean
  onToggle: () => void
  // foldable: whether there is anything behind the toggle. A running turn can have nothing
  // folded yet (its only part is the call that is running), so the row is then a status line
  // rather than a button — a toggle that opens nothing is worse than none.
  foldable?: boolean
  // live: the call running right now. While it is set the row says WHAT is happening
  // instead of how much has happened — the counts are the same fact told afterwards, and
  // the elapsed time is the difference between "working" and "stuck".
  live?: LiveStep | null
  elapsedMs?: number
}) {
  const steps = processStepLabel(summary)
  const line = processSummaryLine(summary)
  const failed = summary.failed > 0
  return (
    <div
      className={`process-head${open ? ' open' : ''}${failed ? ' failed' : ''}${live ? ' live' : ''}${foldable ? '' : ' static'}`}
      role={foldable ? 'button' : undefined}
      tabIndex={foldable ? 0 : undefined}
      aria-expanded={foldable ? open : undefined}
      title={foldable ? (open ? '收起这一轮的过程' : '展开这一轮的过程：思考、工具调用与中间说明') : undefined}
      onClick={foldable ? onToggle : undefined}
      onKeyDown={foldable ? actOnKey(onToggle) : undefined}
    >
      {live ? <span className="process-dot" aria-hidden="true" /> : <span className="process-caret"><Caret open={open} /></span>}
      {live ? (
        <>
          <RollingLine className="process-now" text={processLiveState(live)} />
          {elapsedMs ? <span className="process-elapsed">· {fmtDur(elapsedMs)}</span> : null}
          {failed ? <span className="process-failed">⚠ {summary.failed} 步失败</span> : null}
        </>
      ) : (
        <>
          {steps ? <span className="process-steps">{steps}</span> : null}
          {line ? <span className="process-line">{line}</span> : null}
          {failed ? <span className="process-failed">⚠ {summary.failed} 步失败</span> : null}
        </>
      )}
      {foldable ? <span className="process-toggle">{open ? '收起' : '展开时序'}</span> : null}
    </div>
  )
}

// UserBubble is the user's own message. It carries no avatar: the bubble is
// already unmistakable — right-aligned, tinted, opposite the agent's — so an
// avatar next to it would only add a chip repeating "this one is you" (and the
// row would lose 42px of room for what you actually wrote).
function UserBubble({ children, onContextMenu }: { children: ReactNode; onContextMenu?: (e: React.MouseEvent) => void }) {
  return (
    <div className="msg msg-user" onContextMenu={onContextMenu}>
      <div className="msg-content">{children}</div>
    </div>
  )
}

function CopyIcon() {
  return (<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" /><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" /></svg>)
}

// Small 14px stroke icons for the sidebar footer and the MCP button. They
// replace the ad-hoc glyphs (⚙ ¤ M) whose shape depends on whichever system
// font happens to supply them.
// Caret is the disclosure arrow every foldable surface uses: a sidebar group, a compaction chain,
// the thinking block, a turn's process row, a diff file, a directory in the @-picker.
//
// It is an ICON, not the ▸/▾ characters. Those are text glyphs: their size follows the font, and at
// the sizes a sidebar can afford (9–12px) they draw a DOT — which is the report "the arrow is too
// small to make out". A viewBox plus a stroke is the size the reader sees, whatever the font does.
//
// `open` rotates it a quarter turn (right → down) with the transition in .caret, so one component
// covers every site. Its box is --caret-size (see base.css): one number for all of them.
function Caret({ open }: { open?: boolean }) {
  return (
    <svg className="caret" viewBox="0 0 8 12" aria-hidden="true"
      style={open === undefined ? undefined : { transform: open ? 'rotate(90deg)' : undefined }}>
      <path d="M1.6 1.6 L6.4 6 L1.6 10.4" fill="none" stroke="currentColor" strokeWidth="1.5"
        strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

function SettingsIcon() {
  return (<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><line x1="4" y1="8" x2="20" y2="8" /><line x1="4" y1="16" x2="20" y2="16" /><circle cx="9" cy="8" r="2.2" /><circle cx="15" cy="16" r="2.2" /></svg>)
}

function UsageIcon() {
  return (<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><line x1="6" y1="20" x2="6" y2="12" /><line x1="12" y1="20" x2="12" y2="5" /><line x1="18" y1="20" x2="18" y2="15" /></svg>)
}

function MCPIcon() {
  return (<svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M9 3v6" /><path d="M15 3v6" /><path d="M6 9h12v3a6 6 0 0 1-12 0V9z" /><path d="M12 18v3" /></svg>)
}

// PinIcon marks a pinned session row. It is drawn rather than written as 📌: the emoji's shape and
// colour come from the system font (and it lands in whatever colours the OS picked), while this is
// the same accent ink as the rest of the sidebar's marks at any size the sidebar can afford.
function PinIcon() {
  return (
    <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M9.6 3h4.8" />
      <path d="M10.6 3v5.2L7.4 11.4v2.4h9.2v-2.4l-3.2-3.2V3" />
      <path d="M12 13.8V21" />
    </svg>
  )
}

function SunIcon() {
  return (
    <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
      <circle cx="12" cy="12" r="4.1" />
      <path d="M12 2.8v2.3M12 18.9v2.3M2.8 12h2.3M18.9 12h2.3M5.5 5.5l1.6 1.6M16.9 16.9l1.6 1.6M18.5 5.5l-1.6 1.6M7.1 16.9l-1.6 1.6" />
    </svg>
  )
}

function MoonIcon() {
  return (
    <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20.5 14.6A8.6 8.6 0 0 1 9.4 3.5a8.6 8.6 0 1 0 11.1 11.1z" />
    </svg>
  )
}

// EyeIcon is the reveal control on a credential field, and it says the STATE the box is in — a
// struck-through eye while the value is masked, an open one while it is in the clear — because that
// is what a reader glancing at a row of inputs needs to know about it. The two readings ("currently
// hidden" / "click to hide") are the same picture, so the button's title spells out the ACTION.
//
// Drawn, like the other icons here, rather than written as 👁: the emoji is whatever the system font
// draws, in whatever colours it picked, and this one has to sit at the input's own size in
// currentColor beside a monospace key.
function EyeIcon({ open }: { open: boolean }) {
  return (
    <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.8"
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M2.2 12S5.9 5.6 12 5.6 21.8 12 21.8 12 18.1 18.4 12 18.4 2.2 12 2.2 12z" />
      <circle cx="12" cy="12" r="2.7" />
      {open ? null : <path d="M4.4 19.6 19.6 4.4" />}
    </svg>
  )
}

// ThemeToggle is the titlebar light/dark switch: a two-slot pill with a sliding
// thumb, so the state reads as "which side is on" even before the icons are
// parsed. The label states the ACTION (what a click does), matching how the
// sidebar toggle is labelled.
function ThemeToggle({ theme, onToggle }: { theme: Theme; onToggle: () => void }) {
  const dark = theme === 'dark'
  const label = dark ? '切换到浅色模式' : '切换到深色模式'
  return (
    <button
      type="button"
      className="theme-toggle no-drag"
      data-state={theme}
      role="switch"
      aria-checked={dark}
      aria-label={label}
      title={label}
      onClick={onToggle}
    >
      <span className="theme-ico theme-ico-sun" aria-hidden="true"><SunIcon /></span>
      <span className="theme-ico theme-ico-moon" aria-hidden="true"><MoonIcon /></span>
      <span className="theme-thumb" aria-hidden="true" />
    </button>
  )
}

// defaultExpanded opens the card with its output already showing: set for tool
// calls the user requested directly (/sh), where the output IS the answer. Only
// the initial state — the header still folds it away like any other card.
//
// A card that carries a change is CONTROLLED: its open state is Part.diffOpen, owned
// by App, because the turn footer's chip opens/closes every diff of the turn at once.
// Cards without a change keep the local state they always had.
function ToolCard({ name, title, args, summary, ok, done, change, diffOpen, durationMs, defaultExpanded, onToggleDiff }: {
  name: string
  title?: string
  args?: string
  summary: string
  ok: boolean
  // done gates the diff: a call that is still running (or failed) changed nothing,
  // so drawing its diff would claim something that has not happened.
  done?: boolean
  change?: FileChangeVO | null
  diffOpen?: boolean
  durationMs?: number
  defaultExpanded?: boolean
  onToggleDiff?: () => void
}) {
  const [expanded, setExpanded] = useState(!!defaultExpanded)
  // The diff is what the card is FOR once a call has changed a file: it becomes the body,
  // and the tool's own output is NOT repeated below it — for EditFile/WriteFile that
  // output is the same change described as text, so the diff is the better rendering of
  // it, not an addition to it. A call that failed has no diff (see hasDiff), so its error
  // text is shown as usual.
  const hasDiff = !!(change && done && ok)
  const open = hasDiff ? !!diffOpen : expanded
  const toggle = hasDiff ? onToggleDiff : () => setExpanded((e) => !e)
  const long = ((args?.length || 0) + summary.length) > 120
  const prettyArgs = (() => { if (!args) return ''; try { return JSON.stringify(JSON.parse(args), null, 2) } catch { return args } })()
  return (
    <div className="tool-card">
      <div className="tool-head" role="button" tabIndex={0} aria-expanded={open}
        onClick={toggle} onKeyDown={actOnKey(() => toggle?.())}>
        <span className="tool-ico">⚙</span><span className="tool-name">{name}</span>
        {title ? <span className="tool-title">{title}</span> : null}
        {/* The per-card entry into the diff: the same numbers the footer chip sums,
            so the light touch and the full view agree. */}
        {hasDiff ? (
          <span className="tool-diffstat">
            {change!.added > 0 ? <span className="diff-count is-add">+{change!.added}</span> : null}
            {change!.removed > 0 ? <span className="diff-count is-del">−{change!.removed}</span> : null}
          </span>
        ) : null}
        <span className={`tool-status ${ok ? 'ok' : 'err'}`}>{ok ? '✓' : '✗'}</span>
        {durationMs ? <span className="tool-dur" title="耗时">{fmtDur(durationMs)}</span> : null}
        {/* Copying the result only makes sense when the result is on screen: a diff card
            replaced it with the diff. */}
        {summary && !hasDiff ? <button className="tool-copy" title="复制结果" onClick={(e) => { e.stopPropagation(); copyText(summary) }}><CopyIcon /></button> : null}
        {hasDiff ? <span className="tool-toggle">{open ? '收起' : '查看'}</span>
          : long ? <span className="tool-toggle">{expanded ? '收起' : '展开'}</span> : null}
      </div>
      {hasDiff && open ? <DiffBlock change={change!} /> : null}
      {!hasDiff && expanded && args ? <div className="tool-args-wrap"><div className="tool-args-bar"><span className="tool-args-label">参数</span><button className="tool-copy" title="复制参数" onClick={(e) => { e.stopPropagation(); copyText(args || '') }}><CopyIcon /></button></div><pre className="tool-args">{prettyArgs}</pre></div> : null}
      {!hasDiff && expanded && summary ? <div className="tool-summary">{summary}</div> : null}
    </div>
  )
}

function MCPPanel({ servers, loading, profile, auth, onClose, onToggleServer, onToggleTool, onToggleProfile, onAuthorize, onCompleteAuth }: {
  servers: any[]
  loading: Record<string, boolean>
  profile: { active: string; available: string[] }
  onClose: () => void
  onToggleServer: (name: string, enabled: boolean) => void
  onToggleTool: (name: string, enabled: boolean) => void
  onToggleProfile: (name: string) => void
  // The OAuth side. auth holds one entry per server that has been through an
  // authorization (or a refused one) IN THIS PROCESS — a server that needs a token and has
  // never been tried has no entry, and is offered the button purely because the backend
  // marked it NeedsAuth. The panel keeps no state of its own: what it renders is what the
  // backend last said, which is what makes a reloaded window agree with a live one.
  auth: Record<string, MCPAuthState>
  onAuthorize: (name: string) => void
  onCompleteAuth: (name: string, redirectURL: string) => void
}) {
  // Which servers the reader has opened, by name. EMPTY means "all folded", and the panel
  // opens as a list of servers — name, state, tool count, actions — with the tools appearing
  // on demand.
  //
  // The default is uniform for a reason: tools belong to a connected server, so a rule tied
  // to that (expand when connected) leaves an unconnected server showing nothing at all, and
  // the same control then looks like two different ones. Folded is also the only sane start
  // for a server with dozens of tools.
  //
  // Not remembered across openings, and that is a choice rather than an omission: the state
  // is this component's, the panel unmounts when it closes, and a panel reopened later should
  // start at the top instead of reshuffling what someone left behind. Persisting it belongs
  // in desktop_ui.json beside the other UI state, not here.
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  // One draft redirect URL per server: the panel is a list, and a paste into one row must
  // not appear in another's box.
  const [pasteURL, setPasteURL] = useState<Record<string, string>>({})
  // Per-tool detail: whether the row is open and, once fetched, the tool's contract. Fetched
  // on first open rather than shipped with the list — see GetMCPToolSchema.
  const [toolDetail, setToolDetail] = useState<Record<string, { open: boolean; loading: boolean; schema?: MCPToolSchemaVO }>>({})

  // toggleToolDetail opens a tool's row and fetches its schema the first time. A tool that is
  // already open keeps its schema: the panel re-renders on every refresh (turn end, an
  // authorization), and refetching a hundred rows' worth of nothing on each of those is the
  // kind of churn nobody sees but everybody pays for.
  const toggleToolDetail = useCallback((name: string) => {
    const willOpen = !toolDetail[name]?.open
    setToolDetail((prev) => ({ ...prev, [name]: { open: willOpen, loading: willOpen && !prev[name]?.schema, schema: prev[name]?.schema } }))
    if (!willOpen || toolDetail[name]?.schema) return
    AgentService.GetMCPToolSchema(name)
      .then((vo) => setToolDetail((prev) => ({ ...prev, [name]: { open: true, loading: false, schema: vo } })))
      .catch(() => setToolDetail((prev) => ({ ...prev, [name]: { open: true, loading: false } })))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [toolDetail])
  const profileBusy = !!loading.__profile__
  // The head's totals, summed from the very rows this panel renders — not from a separate
  // backend number, so the two can never disagree. A server that is down contributes 0 to
  // both, which is what its own row says (0 工具).
  const totalTools = (servers || []).reduce((n, s) => n + ((s.tools || []).length), 0)
  const totalLoaded = (servers || []).reduce((n, s) => n + ((s.tools || []).filter((t: any) => t.loaded).length), 0)
  const boxRef = useRef<HTMLDivElement>(null)

  // Click outside closes the popover (its natural behaviour). The toggle button
  // is excluded: it owns open/close, and closing here would fight the reopen
  // from its own click handler.
  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || boxRef.current?.contains(t) || t.closest('.mcp-btn')) return
      onClose()
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [onClose])

  return (
    <>
      <div className="popover-panel mcp-panel" ref={boxRef}>
        <div className="mcp-head">
          {/* The head carries the totals the rows below add up to: how much of everything this
              session actually has. It is a subtitle under the title rather than a fourth item on
              the row — the row already holds the profile select and the close button, and a
              360px panel cannot fit a title, a count and both of them side by side. Hidden when
              there is nothing to sum: a panel of unconnected servers would read 已加载 0/0 工具,
              which says less than not being there. */}
          <span className="mcp-head-text">
            <span className="mcp-title">MCP Servers</span>
            {totalTools > 0 && (
              <span className="mcp-total" title={`所有 server 合计：已加载 ${totalLoaded} / 共 ${totalTools} 个工具`}>
                已加载 {totalLoaded}/{totalTools} 工具
              </span>
            )}
          </span>
          <div className="mcp-profile">
            <span className="mcp-profile-label">profile</span>
            <select className="mcp-profile-select" value={profile.active || ''} disabled={profileBusy} onChange={(e) => onToggleProfile(e.target.value)}>
              <option value="">default</option>
              {(profile.available || []).map((p) => <option key={p} value={p}>{p}</option>)}
            </select>
          </div>
          <button className="mcp-close" onClick={onClose}>✕</button>
        </div>
        <div className="mcp-body">
          {servers.length === 0 && <div className="mcp-empty">未配置 MCP server（~/.tachi/mcp.json）</div>}
          {servers.map((s) => {
            const isOpen = !!expanded[s.name]
            const tools: any[] = s.tools || []
            const toolCount = tools.length
            // Loaded out of total, because the row is folded by default and the total alone
            // does not say how much of this server this session actually HAS. The two
            // numbers are what the greyed tool rows below spell out one by one; a server
            // with no tools (not connected) shows the plain 0 工具 instead of a hollow 0/0.
            const loadedCount = tools.filter((t) => t.loaded).length
            const busy = !!loading[s.name]
            const state = auth[s.name] as MCPAuthState | undefined
            const authPhase = state?.phase || ''
            // The authorization card, shown while a server is being authorized or has just
            // refused: the progress sentence, the URL to open or copy, and the paste box
            // for the way back. It disappears the moment the row connects, because then
            // there is nothing left to do here.
            const authCard = state && !s.connected ? (
              <div className="mcp-auth">
                {state.message && (
                  <div className={`mcp-auth-msg${authPhase === 'error' ? ' err' : ''}`}>{state.message}</div>
                )}
                {state.authUrl && (
                  <div className="mcp-auth-url">
                    <input readOnly value={state.authUrl} onFocus={(e) => e.target.select()} />
                    <button className="mcp-toggle" title="复制授权链接" onClick={() => { copyText(state.authUrl) }}>复制</button>
                  </div>
                )}
                {(authPhase === 'browser' || authPhase === 'waiting' || authPhase === 'manual') && (
                  <div className="mcp-auth-paste">
                    <input
                      className="mcp-auth-paste-input"
                      placeholder="浏览器打不开时：粘贴地址栏里的完整 URL"
                      value={pasteURL[s.name] || ''}
                      onChange={(e) => setPasteURL((p) => ({ ...p, [s.name]: e.target.value }))}
                    />
                    <button className="mcp-toggle" onClick={() => onCompleteAuth(s.name, pasteURL[s.name] || '')}>完成授权</button>
                  </div>
                )}
              </div>
            ) : null
            return (
              <div key={s.name} className="mcp-server">
                <div className="mcp-server-row" role="button" tabIndex={0} aria-expanded={isOpen}
                  onClick={() => setExpanded((p) => ({ ...p, [s.name]: !p[s.name] }))}
                  onKeyDown={actOnKey(() => setExpanded((p) => ({ ...p, [s.name]: !p[s.name] })))}>
                  {/* The fold is the row's own control, so the row has to LOOK like one:
                      with the tools folded by default, a name + state + count reads as a
                      static line unless something says it opens. Same caret as the thinking
                      block, so "this line unfolds" is already a known shape in this UI —
                      and LEADING, the way a tree's disclosure sits before the thing it
                      opens, because a trailing one competes with the row's buttons. */}
                  <span className="mcp-caret" aria-hidden="true"><Caret open={isOpen} /></span>
                  <span className="mcp-server-name">{s.name}</span>
                  <span className={`mcp-server-state ${s.connected ? 'on' : (s.needsAuth ? 'auth' : 'off')}`}>
                    {s.connected ? '已连接' : (s.needsAuth ? '需要授权' : '未连接')}
                  </span>
                  <span className="mcp-toolcount" title={`已加载 ${loadedCount} / 共 ${toolCount} 个工具`}>
                    {toolCount > 0 ? `${loadedCount}/${toolCount} 工具` : '0 工具'}
                  </span>
                  {/* The authorize button belongs beside the toggle, not inside the
                      expanded body: it is what the row's own state is telling the user to
                      do, and the row is where they are looking. */}
                  {!s.connected && s.needsAuth && (
                    <button className="mcp-toggle mcp-auth-btn" disabled={busy} onClick={(e) => { e.stopPropagation(); onAuthorize(s.name) }}>
                      {authPhase === 'starting' ? <><span className="mcp-spinner" /> 启动中…</> : '授权'}
                    </button>
                  )}
                  <button className={`mcp-toggle ${s.connected ? 'on' : ''}`} disabled={busy} onClick={(e) => { e.stopPropagation(); onToggleServer(s.name, !s.connected) }}>
                    {busy ? <><span className="mcp-spinner" /> 连接中…</> : (s.connected ? '禁用' : '启用')}
                  </button>
                </div>
                {authCard}
                {isOpen && (s.tools || []).map((t) => {
                  const tbusy = !!loading[t.name]
                  const open = !!toolDetail[t.name]?.open
                  const schema = toolDetail[t.name]?.schema
                  // Deliberately not called "loading": that is this panel's prop, and shadowing
                  // it here made `loading[t.name]` above read the local one.
                  const schemaLoading = !!toolDetail[t.name]?.loading
                  // A tool that lives in the shared pool but is not loaded into THIS session is
                  // listed all the same, greyed — the panel is the directory of what the server
                  // offers, and hiding the unloaded half would make a 44-tool server look empty
                  // under tool search, which auto-loads none of them. Loaded stays at full
                  // strength (normal name, ✓ in green, the action reading 禁用), so the two are
                  // told apart at a glance rather than by reading a chip.
                  return (
                    <div key={t.name} className={`mcp-tool${t.loaded ? '' : ' unloaded'}`}>
                      <div className="mcp-tool-row" role="button" tabIndex={0} aria-expanded={open}
                        onClick={() => toggleToolDetail(t.name)}
                        onKeyDown={actOnKey(() => toggleToolDetail(t.name))}>
                        <span className="mcp-caret" aria-hidden="true"><Caret open={open} /></span>
                        <span className={`mcp-tool-state ${t.loaded ? 'loaded' : ''}`}>{t.loaded ? '✓ 已加载' : '○ 未加载'}</span>
                        <span className="mcp-tool-name" title={t.description || ''}>{t.toolName}</span>
                        <button className={`mcp-toggle ${t.loaded ? 'on' : ''}`} disabled={tbusy} onClick={(e) => { e.stopPropagation(); onToggleTool(t.name, !t.loaded) }}>
                          {tbusy ? <><span className="mcp-spinner" /> 处理中…</> : (t.loaded ? '禁用' : '启用')}
                        </button>
                      </div>
                      {/* The description and the parameter schema are fetched per tool, when it
                          is opened: the list is up to a hundred rows and is re-read on every
                          panel open, so the schemas cannot ride along with it. */}
                      {open && (
                        <div className="mcp-tool-detail">
                          {schemaLoading && !schema ? (
                            <div className="mcp-param-none">读取中…</div>
                          ) : !schema ? (
                            <div className="mcp-param-none">这个工具已经不在池子里了（server 可能已断开）</div>
                          ) : (
                            <>
                              <div className="mcp-tool-desc">{schema.description || '（这个工具没有描述）'}</div>
                              {(schema.parameters || []).length === 0 ? (
                                <div className="mcp-param-none">无参数</div>
                              ) : (
                                (schema.parameters || []).map((p) => (
                                  <div className="mcp-param" key={p.name}>
                                    <span className="mcp-param-name">
                                      {p.name}
                                      {p.required ? <span className="mcp-param-req" title="必填">*</span> : null}
                                    </span>
                                    <span className="mcp-param-type">
                                      {p.type}{p.itemType ? `[${p.itemType}]` : ''}
                                    </span>
                                    {p.description ? <span className="mcp-param-desc">{p.description}</span> : null}
                                    {(p.enum || []).length > 0 ? (
                                      <span className="mcp-param-meta">可选值：{(p.enum || []).join(' / ')}</span>
                                    ) : null}
                                    {p.default ? <span className="mcp-param-meta">默认：{p.default}</span> : null}
                                  </div>
                                ))
                              )}
                            </>
                          )}
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            )
          })}
        </div>
      </div>
    </>
  )
}

// BranchChip is one root's git branch, shown beside its path. Two checkouts of the same repository
// are otherwise indistinguishable in this list — both rows read "tachi" — and the branch is the
// thing a reader checks before sending a turn that edits files.
//
// It draws its own icon rather than using a character: ⎇ and friends depend on the font, which is
// how the disclosure carets ended up as dots (see Caret).
function BranchChip({ branch, detached }: { branch?: string; detached?: boolean }) {
  if (!branch) return null
  return (
    <span className="roots-branch"
      title={detached ? `游离 HEAD，不在任何分支上（${branch}）` : `git 分支：${branch}`}>
      <svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor"
        strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
        <circle cx="4.6" cy="3.4" r="1.7" />
        <circle cx="4.6" cy="12.6" r="1.7" />
        <circle cx="11.4" cy="6.4" r="1.7" />
        <path d="M4.6 5.1v5.8" />
        <path d="M11.4 8.1c0 2.1-1.7 2.9-3.2 3.1" />
      </svg>
      {branch}
    </span>
  )
}

// RootsPanel is the workspace popover behind the composer's directory chip: the
// session's primary directory plus its additional roots, and the two actions the
// list needs — change the primary, add more.
//
// The primary is not removable (only replaceable): bash's cwd, relative paths and
// the git probe all hang off it, so "no primary" is not a state worth offering.
// A root whose directory has vanished is shown greyed rather than dropped — does an
// unmounted volume come back? That is the user's call, not ours.
//
// A session in a PROJECT has no roots of its own to manage (design §7.2): the panel is
// read-only and points at the project instead, because the backend refuses those three writers
// while a project drives the session — buttons that can only fail are worse than no buttons. It
// goes back to the editable form when the project stops driving the session (deleted, or its
// primary is gone), which is the same rule the backend's guards use: the panel must never be
// read-only while the writers are open, or the user has no way to fix their workspace.
function RootsPanel({ roots, error, busy, onPickPrimary, onAdd, onRemove, onClose, onEditProject, onNewSessionInProject }: {
  roots: SessionRootsVO | null
  error: string
  busy: boolean
  onPickPrimary: () => void
  onAdd: () => void
  onRemove: (path: string) => void
  onClose: () => void
  onEditProject: (projectId: string) => void
  onNewSessionInProject: (projectId: string) => void
}) {
  const primary = roots?.primary || ''
  const additional = roots?.additional || []
  const projectId = roots?.projectId || ''
  // A project still DRIVES this session only while it exists and its primary is usable; a
  // projectMissing payload with a name means "still there, unusable" and with no name means
  // "gone" (see SessionRootsVO). Both are editable here, and both say which one it is.
  const managed = !!projectId && !roots?.projectMissing
  const boxRef = useRef<HTMLDivElement>(null)

  // Same dismissal contract as the MCP panel: click outside, or Esc. The chip that
  // owns open/close is excluded, or closing here would fight its own click.
  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || boxRef.current?.contains(t) || t.closest('.work-dir')) return
      onClose()
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [onClose])

  return (
    <div className="popover-panel roots-panel" ref={boxRef}>
      <div className="roots-head">
        <span className="roots-title">工作区目录</span>
        <span className="roots-sub">
          {managed
            ? `由项目「${roots?.projectName || '项目'}」管理`
            : '相对路径以主目录为准，附加目录用绝对路径访问'}
        </span>
      </div>

      {managed ? (
        <>
          {/* Read-only branch (design §7.2): the project owns these roots, the backend refuses
              writes to them from here, and the two actions below are the way out. */}
          <div className="roots-sec">主目录</div>
          <div className="roots-row">
            <span className="roots-path roots-path-main" title={primary || undefined}><bdi>{primary || '未设置'}</bdi></span>
            <BranchChip branch={roots?.primaryBranch} detached={roots?.primaryDetached} />
          </div>
          <div className="roots-sec">附加目录{additional.length > 0 ? `（${additional.length}）` : ''}</div>
          {additional.length === 0
            ? <div className="roots-empty">还没有附加目录</div>
            : additional.map((r) => (
              <div key={r.path} className={`roots-row${r.exists ? '' : ' is-stale'}`}>
                <span className="roots-name" title={r.path}>{baseName(r.path)}</span>
                <span className="roots-path" title={r.path}><bdi>{r.path}</bdi></span>
                <BranchChip branch={r.branch} detached={r.detached} />
                {r.exists ? null : <span className="roots-stale">已失效</span>}
              </div>
            ))}
          <div className="roots-note">该项目下的会话共用这组目录，修改项目会影响其中所有会话。</div>
          <div className="roots-actions">
            <button className="roots-add" onClick={() => onEditProject(projectId)}>编辑项目…</button>
            <button className="roots-add" onClick={() => onNewSessionInProject(projectId)}>新建项目会话</button>
          </div>
        </>
      ) : (
        <>
          {projectId ? (
            // The session still carries a project_id the project no longer DRIVES (deleted, or
            // its primary directory is gone). The panel stays editable on purpose — a read-only
            // panel plus three refused writers would leave the user no way out (design §5) — but
            // it must say why the project stopped applying.
            <div className="roots-note roots-note-warn">
              ⚠ {roots?.projectName
                ? `项目「${roots.projectName}」的目录不可用，该会话暂时使用自己的快照，可以编辑；目录恢复后会自动回到项目。`
                : '该项目已不存在，该会话已回到自己的工作区，可以编辑。'}
            </div>
          ) : null}

          <div className="roots-sec">主目录</div>
          <div className="roots-row">
            <span className="roots-path roots-path-main" title={primary || undefined}><bdi>{primary || '未设置'}</bdi></span>
            <BranchChip branch={roots?.primaryBranch} detached={roots?.primaryDetached} />
            <button className="roots-btn" disabled={busy} onClick={onPickPrimary}>{primary ? '更换…' : '选择…'}</button>
          </div>

          <div className="roots-sec">附加目录{additional.length > 0 ? `（${additional.length}）` : ''}</div>
          {additional.length === 0
            ? <div className="roots-empty">还没有附加目录</div>
            : additional.map((r) => (
              <div key={r.path} className={`roots-row${r.exists ? '' : ' is-stale'}`}>
                <span className="roots-name" title={r.path}>{baseName(r.path)}</span>
                <span className="roots-path" title={r.path}><bdi>{r.path}</bdi></span>
                <BranchChip branch={r.branch} detached={r.detached} />
                {r.exists ? null : (
                  <span className="roots-stale" title="目录已不存在：system prompt 不再列出它，@ 搜索也会跳过">已失效</span>
                )}
                <button className="roots-btn" disabled={busy} title="从附加目录中移除" onClick={() => onRemove(r.path)}>移除</button>
              </div>
            ))}

          {error ? <div className="roots-error">{error}</div> : null}

          <div className="roots-actions">
            <button className="roots-add" disabled={busy || !primary}
              title={primary ? '添加附加目录（可多选）' : '请先设置主目录'}
              onClick={onAdd}>＋ 添加目录</button>
          </div>
        </>
      )}
    </div>
  )
}

// baseName is the display name of a root: its last path segment.
function baseName(p: string): string {
  const trimmed = p.replace(/\/+$/, '')
  const i = trimmed.lastIndexOf('/')
  return i >= 0 ? trimmed.slice(i + 1) : trimmed
}

// AtFilePicker is the @-file completion popup. It floats above the composer,
// listing the files the backend fuzzy-matched under the session's working
// directory. Keyboard handling lives in the composer (which owns the caret and
// the text); this component only renders and reports picks.
function AtFilePicker({ query, items, selected, loading, refCount, onPick, onHover }: {
  query: string
  items: AtMatch[]
  selected: number
  loading: boolean
  refCount: number
  onPick: (index: number) => void
  onHover: (index: number) => void
}) {
  const listRef = useRef<HTMLDivElement>(null)
  // Keep the highlighted row in view while arrowing through a long list.
  useEffect(() => {
    const el = listRef.current?.children[selected] as HTMLElement | undefined
    el?.scrollIntoView({ block: 'nearest' })
  }, [selected, items])

  return (
    <div className="at-picker" role="listbox" aria-label="@ 文件">
      <div className="at-picker-head">
        <span className="at-picker-title">
          @ 文件{query ? <span className="at-picker-query">{query}</span> : null}
          {refCount > 1 ? <span className="at-picker-count">已引用 {refCount} 个</span> : null}
        </span>
        <span className="at-picker-hint">↑↓/Ctrl+P·N 选择 · Tab/Enter 确认 · Esc 关闭</span>
      </div>
      {items.length === 0 ? (
        <div className="at-picker-empty">{loading ? '搜索中…' : '无匹配文件'}</div>
      ) : (
        <div className="at-picker-list" ref={listRef}>
          {items.map((m, i) => (
            /* onMouseDown (not onClick) with preventDefault: the textarea must
               keep focus, or the caret we splice the reference into is lost. */
            <div key={(m.ref || m.path) + m.root} role="option" aria-selected={i === selected} title={m.ref || m.path}
              className={`at-picker-item${i === selected ? ' is-selected' : ''}`}
              onMouseDown={(e) => { e.preventDefault(); onPick(i) }}
              onMouseEnter={() => onHover(i)}>
              <span className="at-picker-ico">{m.isDir ? <Caret /> : '·'}</span>
              {/* The root label comes FIRST, as a word rather than a colour: two
                  roots can hold the same relative path, and "which one is this"
                  has to be answerable at a glance (and without colour vision). */}
              {m.root ? <span className="at-picker-root">[{m.root}]</span> : null}
              <span className="at-picker-path">{m.path}</span>
              {m.isDir ? <span className="at-picker-tag">目录</span> : null}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// AskForm renders an AskUserQuestion form INLINE in the transcript (no modal):
// it takes the place of the pending AskUserQuestion tool card, so the questions
// appear exactly where the agent asked them. One block per question, options as
// single-choice rows or checkboxes, plus a free-text field. Answers are keyed by
// the full question text with values joined by ", " — the convention the TUI
// established (see tui/askuserview.go GetAnswers), so the model sees one shape
// whichever frontend asked.

// CommandPicker lists the slash commands a bare "/" prefix matches. It shares the
// @-file picker's shell (the two never show at once — one is triggered by "@",
// the other by a leading "/"), so the composer has a single floating-picker look.
function CommandPicker({ items, selected, onPick }: { items: CommandVO[]; selected: number; onPick: (i: number) => void }) {
  return (
    <div className="at-picker">
      <div className="at-picker-head">
        <span className="at-picker-title"><span className="at-picker-query">/</span> 命令</span>
        <span className="at-picker-hint">↑↓ 选择 · Tab/Enter 补全</span>
      </div>
      {items.length === 0 ? (
        <div className="at-picker-empty">没有匹配的命令</div>
      ) : (
        <div className="at-picker-list">
          {items.map((c, i) => (
            <div
              key={c.name}
              className={`at-picker-item${i === selected ? ' is-selected' : ''}`}
              onClick={() => onPick(i)}
              onMouseEnter={() => onPick(i)}
              role="option"
              aria-selected={i === selected}
            >
              <span className="at-picker-path">/{c.name}</span>
              <span className="cmd-desc">{c.description}</span>
              {c.inputHint ? <span className="at-picker-tag">{c.inputHint}</span> : null}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function AskForm({ questions, onSubmit, onCancel }: {
  questions: Question[]
  onSubmit: (answers: Record<string, string>) => void
  onCancel: () => void
}) {
  const [picked, setPicked] = useState<Record<number, string[]>>({})
  const [text, setText] = useState<Record<number, string>>({})

  const toggle = (qi: number, label: string, multi: boolean) => {
    setPicked((prev) => {
      const cur = prev[qi] || []
      if (!multi) return { ...prev, [qi]: cur[0] === label ? [] : [label] }
      return { ...prev, [qi]: cur.includes(label) ? cur.filter((l) => l !== label) : [...cur, label] }
    })
  }

  const partsFor = (qi: number) => {
    const parts = [...(picked[qi] || [])]
    const t = (text[qi] || '').trim()
    if (t) parts.push(t)
    return parts
  }
  // Submittable as soon as the form is open: unanswered questions are simply
  // left out of the map (the model sees what was answered). A hard "answer every
  // question" gate used to disable the button — with no :disabled styling it
  // looked clickable and did nothing, which is exactly how a silent dead button
  // happens.
  const submit = () => {
    const answers: Record<string, string> = {}
    questions.forEach((q, qi) => {
      const parts = partsFor(qi)
      if (parts.length) answers[q.question] = parts.join(', ')
    })
    onSubmit(answers)
  }

  // Enter IN A FREE-TEXT FIELD submits, and that is the field's own rule rather than the form's:
  // the box is single-line, so Enter has no other job there, and the reader who has just typed
  // their own answer is finished with the form (`⌘↩` remains the shortcut that works with the
  // focus anywhere — including on an option row, where a bare Enter still toggles it).
  //
  // Two keys are deliberately not this: an Enter that belongs to an IME's candidate window
  // (confirming 中文/日本語 input must never answer the question — the codebase's rule for every
  // key handler), and a MODIFIED Enter, which is the window-level shortcut's and must not submit
  // twice.
  const onInputKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter' || imeActive(e)) return
    if (e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return
    e.preventDefault()
    submit()
  }

  // Cmd/Ctrl+Enter submits. Esc deliberately does NOT decline: the form lives in
  // the transcript, where Esc is already "dismiss the @-picker / close a modal",
  // and silently dropping a question would be a nasty surprise.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); submit() }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  })

  return (
    <div className="ask-form" aria-label="Tachi 提问">
      <div className="ask-head">
        <span className="ask-title">Tachi 想确认几个问题</span>
        <span className="ask-sub">回答后本轮继续执行</span>
      </div>
      <div className="ask-body">
        {questions.map((q, qi) => {
          const options = q.options || []
          const multi = !!q.multi_select
          const chosen = picked[qi] || []
          return (
            <div className="ask-q" key={qi}>
              <div className="ask-q-head">
                <span className="ask-chip">{q.header}</span>
                <span className="ask-question">{q.question}</span>
                {multi ? <span className="ask-tag">可多选</span> : null}
              </div>
              {options.length > 0 && (
                <div className="ask-options">
                  {options.map((o) => {
                    const on = chosen.includes(o.label)
                    return (
                      <div key={o.label} role={multi ? 'checkbox' : 'radio'} aria-checked={on} tabIndex={0}
                        className={`ask-option${on ? ' is-on' : ''}`}
                        title={o.description || ''}
                        onClick={() => toggle(qi, o.label, multi)}
                        onKeyDown={actOnKey(() => toggle(qi, o.label, multi))}>
                        <span className={`ask-mark ${multi ? 'is-box' : 'is-radio'}`}>{on ? (multi ? '✓' : '●') : ''}</span>
                        <span className="ask-label">{o.label}</span>
                        {o.description ? <span className="ask-desc">{o.description}</span> : null}
                      </div>
                    )
                  })}
                </div>
              )}
              <input className="ask-input" value={text[qi] || ''} onChange={(e) => setText((p) => ({ ...p, [qi]: e.target.value }))}
                onKeyDown={onInputKeyDown}
                placeholder={options.length > 0 ? '其他回答（自由输入，可留空）' : '在此输入回答'} />
            </div>
          )
        })}
      </div>
      <div className="ask-actions">
        <span className="ask-hint">Enter / ⌘↩ 提交</span>
        <button className="btn ghost" onClick={onCancel}>跳过</button>
        <button className="btn" onClick={submit}>提交</button>
      </div>
    </div>
  )
}

// PermissionForm renders a parked bash permission decision INLINE in the transcript,
// in place of the tool card that is waiting — the same placement convention AskForm
// uses, for the same reason: the decision belongs where the call is, not in a modal
// that hides the conversation it is about.
//
// It shows the agent's own preview verbatim (`$ command` + the matched rule). The
// three buttons mirror the TUI's y/a/n: allow once, stop asking for the rest of this
// session, deny. The middle one is deliberately about the SESSION, not about this
// command: an agent's shell commands almost never repeat verbatim, so 「始终允许这一条」
// would be a button that cannot deliver — and the TUI's prompt has always labelled the
// same choice `[a]lways(session)`. Deny rules stay in force either way (the agent
// decides them before asking at all), which is what the hint says.
//
// No Esc and no default-on-Enter: Esc already means "dismiss the picker / close a
// viewer" everywhere else in this UI, and the decision here can be a destructive
// command — it should not be one habitual keypress away.
// The chrome per kind of ask. Everything that differs between the two cards is
// here, and everything that does NOT differ (the preview, the three buttons,
// their wire values) is shared — the preview is the agent's own text and stays
// verbatim in both.
//
// The bash copy exists to avoid claiming a rule that may not exist (see the note
// below); the search copy exists because the reader is being asked to accept a
// SPEND, and 「确认这条命令」 on a card with no command in it sends them looking
// for one.
const permCopy: Record<PermissionKind, { title: string; sub: string; hint: string }> = {
  bash: {
    title: '需要你确认这条命令',
    sub: '允许后本轮继续；deny 规则不受影响',
    hint: '仅这一条；或本会话不再询问 bash（deny 规则仍然生效）',
  },
  mcp_search: {
    title: '加载 MCP 工具会花掉一笔钱',
    sub: '新工具会让整段 prompt 缓存失效，重读一次；拒绝后本会话不再加载',
    hint: '仅这一次；或本会话不再询问 MCP 工具加载花费',
  },
  files: {
    // The preview below names the tool and the path, so this copy must not read
    // like a bash card: 「拒绝」 here refuses THIS call only — the turn carries on.
    title: '需要你确认这次文件操作',
    sub: '允许后本轮继续；deny 规则不受影响',
    hint: '仅这一次；或本会话不再询问文件规则（判定仍然会跑）',
  },
}

function PermissionForm({ perm, onAnswer }: {
  perm: { req: PermissionRequest; busy?: boolean; err?: string }
  onAnswer: (decision: PermissionDecision) => void
}) {
  const busy = !!perm.busy
  const copy = permCopy[perm.req.kind] ?? permCopy.bash
  return (
    <div className="perm-form" aria-label="等待授权">
      <div className="perm-head">
        <span className="perm-title">{copy.title}</span>
        {/* Not 「命中 ask 规则」: that was hard-coded and FALSE for the other way an ask is
            raised — a command the policy could not parse, where no rule was consulted at all (and
            the card then sent the reader looking for a rule they never wrote). What the preview
            below says is the reason; this line only states the consequence. */}
        <span className="perm-sub">{copy.sub}</span>
      </div>
      <pre className="perm-preview">{perm.req.preview}</pre>
      {perm.err ? <div className="perm-err">{perm.err}</div> : null}
      <div className="perm-actions">
        <span className="perm-hint">{copy.hint}</span>
        <button className="btn ghost" disabled={busy} onClick={() => onAnswer('deny')}>拒绝</button>
        <button className="btn ghost" disabled={busy} onClick={() => onAnswer('allow_session')}>本会话全部允许</button>
        <button className="btn" disabled={busy} onClick={() => onAnswer('allow_once')}>{busy ? '已发送…' : '允许一次'}</button>
      </div>
    </div>
  )
}

export { ContextMeter, CacheRing, ProcessStrip, RollingLine, ReviewChip, ThinkingPart, ThinkingBlock, NoticePart, UserBubble, CommandPicker, CopyIcon, ToolCard, MCPPanel, RootsPanel, AtFilePicker, AskForm, PermissionForm, SettingsIcon, UsageIcon, MCPIcon, PinIcon, ThemeToggle, Caret, EyeIcon, MoreRows }
