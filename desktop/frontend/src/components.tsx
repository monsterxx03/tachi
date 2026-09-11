import { memo, useCallback, useEffect, useRef, useState, type ComponentPropsWithoutRef, type ReactNode, type RefObject } from 'react'
import { createPortal } from 'react-dom'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { actOnKey, copyText, fmtDur, fmtShare, humanize, toLocalAsset } from './lib'
import { useThemeSnapshot, type Theme } from './theme'
import type { AttachmentInfo } from './types'
import type { ContextInfoVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

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
    <span className="ctx-wrap">
      {open ? (
        <div className="ctx-panel" ref={boxRef}>
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

// fileFromSendFileArgs reconstructs an attachment from a recorded SendFile call,
// so reloaded sessions show the file card instead of a raw tool card.
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

// FileCard is a file the agent handed over: name, size, and the two actions a
// desktop user wants — open it, or show it in Finder. Images get an inline
// preview (served through the /local asset handler).
export function FileCard({ file }: { file: AttachmentInfo }) {
  const [preview, setPreview] = useState(false)
  const isImage = /\.(png|jpe?g|gif|webp|svg)$/i.test(file.path)
  return (
    <div className="file-card">
      <div className="file-head">
        <span className="file-ico">{isImage ? '🖼' : '📄'}</span>
        <span className="file-name" title={file.path}>{file.name}</span>
        <span className="file-actions">
          {isImage ? (
            <button className="file-btn" onClick={() => setPreview((v) => !v)}>{preview ? '收起' : '预览'}</button>
          ) : null}
          <button className="file-btn" onClick={() => AgentService.OpenPath(file.path).catch(() => {})}>打开</button>
          <button className="file-btn" onClick={() => AgentService.RevealPath(file.path).catch(() => {})}>在 Finder 中显示</button>
        </span>
      </div>
      {isImage && preview ? <img className="file-preview" src={toLocalAsset(file.path, '')} alt={file.name} /> : null}
    </div>
  )
}

function MessageBubble({ role, children }: { role: 'user' | 'assistant'; children: ReactNode }) {
  return (<div className={`msg msg-${role}`}><div className="msg-avatar">{role === 'user' ? 'U' : '◆'}</div><div className="msg-content">{children}</div></div>)
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

function ToolCard({ name, title, args, summary, ok, durationMs }: { name: string; title?: string; args?: string; summary: string; ok: boolean; durationMs?: number }) {
  const [expanded, setExpanded] = useState(false)
  const long = ((args?.length || 0) + summary.length) > 120
  const prettyArgs = (() => { if (!args) return ''; try { return JSON.stringify(JSON.parse(args), null, 2) } catch { return args } })()
  return (
    <div className="tool-card">
      <div className="tool-head" role="button" tabIndex={0} aria-expanded={expanded}
        onClick={() => setExpanded((e) => !e)} onKeyDown={actOnKey(() => setExpanded((e) => !e))}>
        <span className="tool-ico">⚙</span><span className="tool-name">{name}</span>
        {title ? <span className="tool-title">{title}</span> : null}
        <span className={`tool-status ${ok ? 'ok' : 'err'}`}>{ok ? '✓' : '✗'}</span>
        {durationMs ? <span className="tool-dur" title="耗时">{fmtDur(durationMs)}</span> : null}
        {summary ? <button className="tool-copy" title="复制结果" onClick={(e) => { e.stopPropagation(); copyText(summary) }}><CopyIcon /></button> : null}
        {long ? <span className="tool-toggle">{expanded ? '收起' : '展开'}</span> : null}
      </div>
      {expanded && args ? <div className="tool-args-wrap"><div className="tool-args-bar"><span className="tool-args-label">参数</span><button className="tool-copy" title="复制参数" onClick={(e) => { e.stopPropagation(); copyText(args || '') }}><CopyIcon /></button></div><pre className="tool-args">{prettyArgs}</pre></div> : null}
      {expanded && summary ? <div className="tool-summary">{summary}</div> : null}
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
      <div className="mcp-panel" ref={boxRef}>
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

// MermaidDiagram renders a ```mermaid fence as a diagram.
//
// mermaid is imported lazily, so the library only loads when a diagram actually
// appears in a reply, and rendering is debounced: while the fence is still being
// streamed the source changes every frame, which would otherwise re-parse and
// re-layout the diagram ~60x/s. Anything that does not parse (still incomplete,
// or genuinely invalid) falls back to showing the raw code instead of an empty
// box.
export const MermaidDiagram = memo(function MermaidDiagram({ code }: { code: string }) {
  const [svg, setSvg] = useState('')
  const [failed, setFailed] = useState(false)
  const [open, setOpen] = useState(false)
  // Mermaid draws its own palette, so it has to be told which theme is on
  // screen — the app theme, not the OS preference: a user who picked dark on a
  // light Mac must not get a white diagram in a dark transcript.
  const dark = useThemeSnapshot() === 'dark'

  useEffect(() => {
    let alive = true
    const timer = window.setTimeout(async () => {
      try {
        const mermaid = (await import('mermaid')).default
        mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: dark ? 'dark' : 'default' })
        const { svg: out } = await mermaid.render(`mermaid-${Math.random().toString(36).slice(2, 10)}`, code)
        if (alive) { setSvg(out); setFailed(false) }
      } catch {
        if (alive) { setSvg(''); setFailed(true) }
      }
    }, 250)
    return () => { alive = false; window.clearTimeout(timer) }
  }, [code, dark])

  if (failed || !svg) {
    return (
      <div className="mermaid-wrap">
        <div className="code-actions">
          <CopyButton title="复制 mermaid 源码" getText={() => code} />
        </div>
        <pre className="mermaid-pending"><code>{code}</code></pre>
      </div>
    )
  }
  return (
    <div className="mermaid-wrap">
      <div className="code-actions">
        <CopyButton title="复制 mermaid 源码" getText={() => code} />
        <button className="code-copy" title="全屏查看" aria-label="全屏查看" onClick={() => setOpen(true)}>⤢</button>
      </div>
      {/* Clicking the diagram is the obvious gesture for "show me this bigger". */}
      <div className="mermaid" title="点击全屏查看" onClick={() => setOpen(true)} dangerouslySetInnerHTML={{ __html: svg }} />
      {open ? <MermaidViewer svg={svg} code={code} onClose={() => setOpen(false)} /> : null}
    </div>
  )
})

// CopyButton copies the text it is given and briefly confirms with "已复制".
function CopyButton({ getText, title = '复制' }: { getText: () => string; title?: string }) {
  const [done, setDone] = useState(false)
  useEffect(() => {
    if (!done) return
    const t = window.setTimeout(() => setDone(false), 1200)
    return () => window.clearTimeout(t)
  }, [done])
  return (
    <button className={`code-copy${done ? ' is-done' : ''}`} title={title} aria-label={title}
      onClick={(e) => { e.stopPropagation(); copyText(getText()); setDone(true) }}>
      {done ? <span className="code-copy-done">已复制</span> : <CopyIcon />}
    </button>
  )
}

// PreBlock intercepts fenced code blocks: ```mermaid becomes a diagram, every
// other block stays a normal <pre> (already tokenised by rehype-highlight).
// Either way the block gets a copy button — the code itself, or for a diagram
// its mermaid source.
export function PreBlock(props: ComponentPropsWithoutRef<'pre'>) {
  const ref = useRef<HTMLPreElement>(null)
  const child = Array.isArray(props.children) ? props.children[0] : props.children
  const el = child as { props?: { className?: string; children?: ReactNode } } | null
  if ((el?.props?.className || '').includes('language-mermaid')) {
    return <MermaidDiagram code={String(el?.props?.children ?? '').replace(/\n$/, '')} />
  }
  return (
    <div className="code-wrap">
      {/* Read the rendered text rather than walking the token tree: after
          rehype-highlight the code element is a tree of spans, and innerText
          gives back exactly what the user sees. */}
      <div className="code-actions">
        <CopyButton title="复制代码" getText={() => ref.current?.innerText ?? ''} />
      </div>
      <pre {...props} ref={ref} />
    </div>
  )
}

// Zoom bounds for the overlay viewer (0.2x … 6x covers "放大看细节" and
// "整体扫一眼" without letting the diagram become unusable).
const MERMAID_MIN_ZOOM = 0.2
const MERMAID_MAX_ZOOM = 6
const MERMAID_ZOOM_STEP = 1.25

// useDragPan turns a scroll container into a grab-and-drag surface. While a
// diagram is zoomed, dragging is what people reach for (the scrollbar is thin
// and the pointer is already on the diagram), so panning moves the container's
// scroll offsets under the cursor.
function useDragPan(ref: RefObject<HTMLElement | null>) {
  const [dragging, setDragging] = useState(false)
  const [pannable, setPannable] = useState(false)
  const origin = useRef({ x: 0, y: 0, left: 0, top: 0 })

  const sync = useCallback(() => {
    const el = ref.current
    if (!el) return
    setPannable(el.scrollWidth > el.clientWidth + 1 || el.scrollHeight > el.clientHeight + 1)
  }, [ref])

  const onPointerDown = (e: React.PointerEvent) => {
    const el = ref.current
    if (!el || e.button !== 0) return
    if (el.scrollWidth <= el.clientWidth && el.scrollHeight <= el.clientHeight) return // nothing to pan
    e.preventDefault()
    origin.current = { x: e.clientX, y: e.clientY, left: el.scrollLeft, top: el.scrollTop }
    setDragging(true)
    el.setPointerCapture(e.pointerId)
  }
  const onPointerMove = (e: React.PointerEvent) => {
    const el = ref.current
    if (!el || !dragging) return
    el.scrollLeft = origin.current.left - (e.clientX - origin.current.x)
    el.scrollTop = origin.current.top - (e.clientY - origin.current.y)
  }
  const end = (e: React.PointerEvent) => {
    const el = ref.current
    if (!el || !dragging) return
    setDragging(false)
    try { el.releasePointerCapture(e.pointerId) } catch { /* already released */ }
  }

  return {
    dragging,
    pannable,
    sync,
    handlers: { onPointerDown, onPointerMove, onPointerUp: end, onPointerCancel: end },
  }
}

// MermaidViewer shows one diagram as a full-viewport overlay: dim backdrop,
// diagram centred, controls floating at the bottom. It is rendered through a
// portal into document.body — inside the transcript some ancestor creates a
// containing block for `position: fixed` (transform/filter), which pinned the
// overlay to the message box instead of the window.
function MermaidViewer({ svg, code, onClose }: { svg: string; code: string; onClose: () => void }) {
  const [zoom, setZoom] = useState(1)
  const stageRef = useRef<HTMLDivElement>(null)
  const pan = useDragPan(stageRef)

  const clamp = (z: number) => Math.min(MERMAID_MAX_ZOOM, Math.max(MERMAID_MIN_ZOOM, z))
  const fit = useCallback(() => {
    const stage = stageRef.current
    const el = stage?.querySelector('svg')
    if (!stage || !el) return
    // Measure at zoom 1 (the SVG's own layout size), then scale to fit.
    setZoom(1)
    requestAnimationFrame(() => {
      const r = el.getBoundingClientRect()
      if (!r.width || !r.height) return
      setZoom(clamp(Math.min((stage.clientWidth - 40) / r.width, (stage.clientHeight - 40) / r.height)))
    })
  }, [])

  useEffect(() => { fit() }, [fit])
  // Re-check scrollability whenever the zoom (or the diagram) changes: the
  // "grab" cursor should only promise something the user can actually do.
  useEffect(() => { pan.sync() }, [zoom, svg, pan])
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); onClose() }
      else if (e.key === '+' || e.key === '=') { e.preventDefault(); setZoom((z) => clamp(z * MERMAID_ZOOM_STEP)) }
      else if (e.key === '-' || e.key === '_') { e.preventDefault(); setZoom((z) => clamp(z / MERMAID_ZOOM_STEP)) }
      else if (e.key === '0') { e.preventDefault(); setZoom(1) }
      else if (e.key === '1') { e.preventDefault(); fit() }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [fit, onClose])

  // Clicking the backdrop (any blank area) closes; clicks on the diagram or the
  // control bar stop there.
  return createPortal(
    <div className="mermaid-overlay" onClick={onClose} role="dialog" aria-modal="true" aria-label="Mermaid 图表">
      <div className={`mermaid-stage${pan.dragging ? ' is-dragging' : ''}${pan.pannable ? ' is-pannable' : ''}`}
        ref={stageRef} {...pan.handlers} onClick={(e) => e.stopPropagation()}
        onWheel={(e) => { if (e.ctrlKey || e.metaKey) { e.preventDefault(); setZoom((z) => clamp(e.deltaY < 0 ? z * MERMAID_ZOOM_STEP : z / MERMAID_ZOOM_STEP)) } }}>
        <div className="mermaid-zoom" style={{ zoom }} dangerouslySetInnerHTML={{ __html: svg }} />
      </div>
      <div className="mermaid-controls" onClick={(e) => e.stopPropagation()}>
        <button className="mm-btn" title="缩小（-）" onClick={() => setZoom((z) => clamp(z / MERMAID_ZOOM_STEP))}>−</button>
        <span className="mermaid-zoom-label">{Math.round(zoom * 100)}%</span>
        <button className="mm-btn" title="放大（+）" onClick={() => setZoom((z) => clamp(z * MERMAID_ZOOM_STEP))}>+</button>
        <span className="mm-sep" />
        <button className="mm-btn" title="实际大小（0）" onClick={() => setZoom(1)}>100%</button>
        <button className="mm-btn" title="适应窗口（1）" onClick={fit}>适应窗口</button>
        <span className="mm-sep" />
        <CopyButton title="复制 mermaid 源码" getText={() => code} />
        <button className="mm-btn" title="关闭（Esc）" onClick={onClose}>✕</button>
      </div>
    </div>,
    document.body,
  )
}

// AtFilePicker is the @-file completion popup. It floats above the composer,
// listing the files the backend fuzzy-matched under the session's working
// directory. Keyboard handling lives in the composer (which owns the caret and
// the text); this component only renders and reports picks.
function AtFilePicker({ query, items, selected, loading, refCount, onPick, onHover }: {
  query: string
  items: { path: string; isDir: boolean }[]
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
            <div key={m.path} role="option" aria-selected={i === selected} title={m.path}
              className={`at-picker-item${i === selected ? ' is-selected' : ''}`}
              onMouseDown={(e) => { e.preventDefault(); onPick(i) }}
              onMouseEnter={() => onHover(i)}>
              <span className="at-picker-ico">{m.isDir ? '▸' : '·'}</span>
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

export { ContextMeter, CacheRing, ThinkingPart, ThinkingBlock, MessageBubble, CopyIcon, ToolCard, MCPPanel, AtFilePicker, AskForm, SettingsIcon, UsageIcon, MCPIcon, ThemeToggle }
