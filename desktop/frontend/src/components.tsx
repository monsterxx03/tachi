import { useEffect, useRef, useState, type ReactNode } from 'react'
import { actOnKey, fmtDur, humanize } from './lib'

function ContextRing({ estimate, window: w }: { estimate: number; window: number }) {
  const pct = w > 0 ? Math.min(100, (estimate / w) * 100) : 0
  const r = 8, c = 2 * Math.PI * r
  const off = c - (pct / 100) * c
  const title = w > 0 ? `${pct.toFixed(1)}% (${humanize(estimate)} / ${humanize(w)})` : '上下文 —'
  return (
    <svg className="ctx-ring" width="22" height="22" viewBox="0 0 22 22" role="img" aria-label={title}>
      <title>{title}</title>
      <circle cx="11" cy="11" r={r} fill="none" stroke="var(--border)" strokeWidth="2.5" />
      <circle cx="11" cy="11" r={r} fill="none" stroke={pct > 80 ? 'var(--amber)' : 'var(--accent)'} strokeWidth="2.5" strokeDasharray={c} strokeDashoffset={off} strokeLinecap="round" transform="rotate(-90 11 11)" />
    </svg>
  )
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

function ToolCard({ name, title, args, summary, ok, durationMs }: { name: string; title?: string; args?: string; summary: string; ok: boolean; durationMs?: number }) {
  const [expanded, setExpanded] = useState(false)
  const long = ((args?.length || 0) + summary.length) > 120
  const prettyArgs = (() => { if (!args) return ''; try { return JSON.stringify(JSON.parse(args), null, 2) } catch { return args } })()
  const fallbackCopy = (text: string) => { try { const ta = document.createElement('textarea'); ta.value = text; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta) } catch { /* ignore */ } }
  const copy = (text: string) => { if (!text) return; try { navigator.clipboard.writeText(text).catch(() => fallbackCopy(text)) } catch { fallbackCopy(text) } }
  return (
    <div className="tool-card">
      <div className="tool-head" role="button" tabIndex={0} aria-expanded={expanded}
        onClick={() => setExpanded((e) => !e)} onKeyDown={actOnKey(() => setExpanded((e) => !e))}>
        <span className="tool-ico">⚙</span><span className="tool-name">{name}</span>
        {title ? <span className="tool-title">{title}</span> : null}
        <span className={`tool-status ${ok ? 'ok' : 'err'}`}>{ok ? '✓' : '✗'}</span>
        {durationMs ? <span className="tool-dur" title="耗时">{fmtDur(durationMs)}</span> : null}
        {summary ? <button className="tool-copy" title="复制结果" onClick={(e) => { e.stopPropagation(); copy(summary) }}><CopyIcon /></button> : null}
        {long ? <span className="tool-toggle">{expanded ? '收起' : '展开'}</span> : null}
      </div>
      {expanded && args ? <div className="tool-args-wrap"><div className="tool-args-bar"><span className="tool-args-label">参数</span><button className="tool-copy" title="复制参数" onClick={(e) => { e.stopPropagation(); copy(args) }}><CopyIcon /></button></div><pre className="tool-args">{prettyArgs}</pre></div> : null}
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
  return (
    <>
      <div className="mcp-overlay" onClick={onClose} />
      <div className="mcp-panel">
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

export { ContextRing, CacheRing, ThinkingPart, ThinkingBlock, MessageBubble, CopyIcon, ToolCard, MCPPanel, SettingsIcon, UsageIcon, MCPIcon }
