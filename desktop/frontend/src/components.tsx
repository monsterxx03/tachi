import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { DiffBlock } from './diff'
import { actOnKey, copyText, fmtDur, fmtShare, humanize } from './lib'
import type { Theme } from './theme'
import type { AtMatch, Part } from './types'
import type { CommandVO, ContextInfoVO, FileChangeVO, SessionRootsVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

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
        <div className="ctx-empty">分项明细需下一轮对话后可见<br />（当前为历史会话的估算总量）</div>
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
      <div className="ctx-foot">本地 chars/4 估算，非 API 返回用量</div>
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
        <span className="thinking-ico">▸</span><span className="thinking-label">thinking</span>
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

// UserBubble is the user's own message. It carries no avatar: the bubble is
// already unmistakable — right-aligned, tinted, opposite the agent's — so an
// avatar next to it would only add a chip repeating "this one is you" (and the
// row would lose 42px of room for what you actually wrote).
function UserBubble({ children }: { children: ReactNode }) {
  return (
    <div className="msg msg-user">
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
function SettingsIcon() {
  return (<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><line x1="4" y1="8" x2="20" y2="8" /><line x1="4" y1="16" x2="20" y2="16" /><circle cx="9" cy="8" r="2.2" /><circle cx="15" cy="16" r="2.2" /></svg>)
}

function UsageIcon() {
  return (<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><line x1="6" y1="20" x2="6" y2="12" /><line x1="12" y1="20" x2="12" y2="5" /><line x1="18" y1="20" x2="18" y2="15" /></svg>)
}

function MCPIcon() {
  return (<svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M9 3v6" /><path d="M15 3v6" /><path d="M6 9h12v3a6 6 0 0 1-12 0V9z" /><path d="M12 18v3" /></svg>)
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

function MCPPanel({ servers, loading, profile, onClose, onToggleServer, onToggleTool, onToggleProfile }: {
  servers: any[]
  loading: Record<string, boolean>
  profile: { active: string; available: string[] }
  onClose: () => void
  onToggleServer: (name: string, enabled: boolean) => void
  onToggleTool: (name: string, enabled: boolean) => void
  onToggleProfile: (name: string) => void
}) {
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})
  const profileBusy = !!loading.__profile__
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
          <span className="mcp-title">MCP Servers</span>
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
            const isOpen = !collapsed[s.name]
            const toolCount = (s.tools || []).length
            const busy = !!loading[s.name]
            return (
              <div key={s.name} className="mcp-server">
                <div className="mcp-server-row" role="button" tabIndex={0} aria-expanded={isOpen}
                  onClick={() => setCollapsed((p) => ({ ...p, [s.name]: !p[s.name] }))}
                  onKeyDown={actOnKey(() => setCollapsed((p) => ({ ...p, [s.name]: !p[s.name] })))}>
                  <span className="mcp-server-name">{s.name}</span>
                  <span className={`mcp-server-state ${s.connected ? 'on' : 'off'}`}>{s.connected ? '已连接' : '未连接'}</span>
                  <span className="mcp-toolcount">{toolCount} 工具</span>
                  <button className={`mcp-toggle ${s.connected ? 'on' : ''}`} disabled={busy} onClick={(e) => { e.stopPropagation(); onToggleServer(s.name, !s.connected) }}>
                    {busy ? <><span className="mcp-spinner" /> 连接中…</> : (s.connected ? '禁用' : '启用')}
                  </button>
                </div>
                {isOpen && (s.tools || []).map((t) => {
                  const tbusy = !!loading[t.name]
                  return (
                    <div key={t.name} className="mcp-tool">
                      <span className={`mcp-tool-state ${t.loaded ? 'loaded' : ''}`}>{t.loaded ? '✓ 已加载' : '○ 未加载'}</span>
                      <span className="mcp-tool-name" title={t.description || ''}>{t.toolName}</span>
                      <button className={`mcp-toggle ${t.loaded ? 'on' : ''}`} disabled={tbusy} onClick={() => onToggleTool(t.name, !t.loaded)}>
                        {tbusy ? <><span className="mcp-spinner" /> 处理中…</> : (t.loaded ? '禁用' : '启用')}
                      </button>
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

// RootsPanel is the workspace popover behind the composer's directory chip: the
// session's primary directory plus its additional roots, and the two actions the
// list needs — change the primary, add more.
//
// The primary is not removable (only replaceable): bash's cwd, relative paths and
// the git probe all hang off it, so "no primary" is not a state worth offering.
// A root whose directory has vanished is shown greyed rather than dropped — does an
// unmounted volume come back? That is the user's call, not ours.
function RootsPanel({ roots, error, busy, onPickPrimary, onAdd, onRemove, onClose }: {
  roots: SessionRootsVO | null
  error: string
  busy: boolean
  onPickPrimary: () => void
  onAdd: () => void
  onRemove: (path: string) => void
  onClose: () => void
}) {
  const primary = roots?.primary || ''
  const additional = roots?.additional || []
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
        <span className="roots-sub">相对路径以主目录为准，附加目录用绝对路径访问</span>
      </div>

      <div className="roots-sec">主目录</div>
      <div className="roots-row">
        <span className="roots-path roots-path-main" title={primary || undefined}><bdi>{primary || '未设置'}</bdi></span>
        <button className="roots-btn" disabled={busy} onClick={onPickPrimary}>{primary ? '更换…' : '选择…'}</button>
      </div>

      <div className="roots-sec">附加目录{additional.length > 0 ? `（${additional.length}）` : ''}</div>
      {additional.length === 0
        ? <div className="roots-empty">还没有附加目录</div>
        : additional.map((r) => (
          <div key={r.path} className={`roots-row${r.exists ? '' : ' is-stale'}`}>
            <span className="roots-name" title={r.path}>{baseName(r.path)}</span>
            <span className="roots-path" title={r.path}><bdi>{r.path}</bdi></span>
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
              <span className="at-picker-ico">{m.isDir ? '▸' : '·'}</span>
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
                placeholder={options.length > 0 ? '其他回答（自由输入，可留空）' : '在此输入回答'} />
            </div>
          )
        })}
      </div>
      <div className="ask-actions">
        <span className="ask-hint">⌘↩ 提交</span>
        <button className="btn ghost" onClick={onCancel}>跳过</button>
        <button className="btn" onClick={submit}>提交</button>
      </div>
    </div>
  )
}

export { ContextMeter, CacheRing, ThinkingPart, ThinkingBlock, NoticePart, UserBubble, CommandPicker, CopyIcon, ToolCard, MCPPanel, RootsPanel, AtFilePicker, AskForm, SettingsIcon, UsageIcon, MCPIcon, ThemeToggle }
