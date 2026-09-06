import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Events } from '@wailsio/runtime'
import {
  AgentService,
  AgentStatus,
  type AgentState,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import {
  STATUS_META,
  THINKING_LEVELS,
  PAGE_SIZE,
  type AgentEvent,
  type Message,
  type SessionItem,
} from './types'
import { buildTurns, fmtDur, fmtTime, tpsTier } from './lib'
import {
  ContextRing, CacheRing, ThinkingPart, ThinkingBlock, MessageBubble, ToolCard, MCPPanel,
} from './components'
function App() {
  const [sessions, setSessions] = useState<SessionItem[]>([])
  const [currentId, setCurrentId] = useState<string>('')
  const [menu, setMenu] = useState<{ sid: string; x: number; y: number } | null>(null)
  const [confirmDel, setConfirmDel] = useState<{ sid: string; title: string } | null>(null)
  const [shortcutsOpen, setShortcutsOpen] = useState(false)
  const [reminderModal, setReminderModal] = useState<string | null>(null)
  const [editingId, setEditingId] = useState('')
  const [editTitle, setEditTitle] = useState('')
  const [, setCurrentTitle] = useState<string>('Tachi')
  const [msgCache, setMsgCache] = useState<Record<string, Message[]>>({})
  const [runningSet, setRunningSet] = useState<Set<string>>(new Set())
  const [state, setState] = useState<AgentState>({ status: AgentStatus.StatusIdle, label: '空闲', detail: '就绪' })
  const [input, setInput] = useState('')
  const [loading, setLoading] = useState(false)
  const [providers, setProviders] = useState<any[]>([])
  const [providerName, setProviderName] = useState('')
  const [thinkingLevel, setThinkingLevel] = useState('none')
  const [ctxEstimate, setCtxEstimate] = useState(0)
  const [ctxWindow, setCtxWindow] = useState(0)
  const [cost, setCost] = useState(0)
  const [credit, setCredit] = useState(0)
  const [cacheHitRate, setCacheHitRate] = useState(0)
  const [workDir, setWorkDir] = useState('')
  const [editingWd, setEditingWd] = useState(false)
  const [wdInput, setWdInput] = useState('')
  const [tps, setTps] = useState(0)
  const [lastTps, setLastTps] = useState(0)
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  const [mcpOpen, setMcpOpen] = useState(false)
  const [mcpServers, setMcpServers] = useState<any[]>([])
  const [mcpLoading, setMcpLoading] = useState<Record<string, boolean>>({})
  const [mcpProfile, setMcpProfile] = useState<{ active: string; available: string[] }>({ active: '', available: [] })
  // Per-session message pagination: how much of the session history is loaded
  // on the client, and the oldest loaded raw-message timestamp (used as the
  // "load earlier" cursor).
  const [hasMore, setHasMore] = useState<Record<string, boolean>>({})
  const [earliestTs, setEarliestTs] = useState<Record<string, string>>({})
  const chatRef = useRef<HTMLDivElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)
  const messages = msgCache[currentId] || []

  const scrollToBottom = useCallback(() => {
    setTimeout(() => { const el = chatRef.current; if (el) el.scrollTop = el.scrollHeight }, 60)
  }, [])

  // On session switch, jump straight to the newest message BEFORE paint (via
  // useLayoutEffect) so the view never briefly shows the oldest messages and
  // then snaps down. The async-load path still uses scrollToBottom() after the
  // page arrives.
  useLayoutEffect(() => {
    const el = chatRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [currentId])

  const loadMoreRef = useRef(false)
  const handleScroll = () => {
    const el = chatRef.current
    if (!el || loadMoreRef.current || loading) return
    // Load older messages from the backend when the user scrolls to the top and
    // more history is known to exist.
    if (el.scrollTop <= 40 && hasMore[currentId] && earliestTs[currentId]) {
      loadMoreRef.current = true
      const old = el.scrollHeight
      const before = earliestTs[currentId]
      ;(async () => {
        try {
          const page: any = await (AgentService as any).LoadSessionMore?.(currentId, before, PAGE_SIZE)
          if (!page || !page.messages || page.messages.length === 0) {
            setHasMore((p) => ({ ...p, [currentId]: false }))
            return
          }
          const more = buildTurns(page.messages)
          setMsgCache((prev) => ({ ...prev, [currentId]: [...more, ...(prev[currentId] || [])] }))
          setEarliestTs((p) => ({ ...p, [currentId]: page.messages[0].timestamp }))
          setHasMore((p) => ({ ...p, [currentId]: !!page.hasMore }))
        } catch { /* ignore */ }
        requestAnimationFrame(() => requestAnimationFrame(() => {
          const c = chatRef.current
          if (c) c.scrollTop = c.scrollHeight - old
          loadMoreRef.current = false
        }))
      })()
    }
  }

  const refreshRunning = useCallback(async () => {
    const list = (await AgentService.RunningSessions().catch(() => null)) || []
    setRunningSet(new Set(list))
  }, [])
  const setSessionMsgs = useCallback((id: string, fn: (l: Message[]) => Message[]) => {
    setMsgCache((prev) => ({ ...prev, [id]: fn(prev[id] || []) }))
  }, [])

  const refreshProvider = useCallback(async () => {
    try {
      const info = await (AgentService as any).GetProviderInfo?.()
      if (info) { setProviderName(info.provider); setCtxEstimate(info.contextEstimate || 0); setCtxWindow(info.contextWindow || 0) }
      const lv = await (AgentService as any).GetThinkingLevel?.()
      if (lv) setThinkingLevel(lv)
    } catch { /* ignore */ }
  }, [])

  // refreshCost fetches the current session's cumulative cost/credit ("积分")
  // from the backend's usage ledger (counterpart to TUI's statusbar cost).
  const refreshCost = useCallback(async (id: string) => {
    try {
      const u = await (AgentService as any).GetSessionUsage?.(id)
      if (u) { setCost(u.cost || 0); setCredit(u.credit || 0); setCacheHitRate(u.cacheHitRate || 0) }
    } catch { /* ignore */ }
  }, [])

  const refreshWorkDir = useCallback(async (id: string) => {
    try {
      const w = await (AgentService as any).GetSessionWorkingDir?.(id)
      setWorkDir(w || '')
    } catch { /* ignore */ }
  }, [])

  const commitWorkDir = useCallback(async (id: string, dir: string) => {
    setEditingWd(false)
    const t = dir.trim()
    if (!t) return
    await (AgentService as any).SetSessionWorkingDir?.(id, t).catch(() => {})
    setWorkDir(t)
  }, [])

  // refreshMCP reloads the MCP servers/tools + profile for the current session.
  const refreshMCP = useCallback(async () => {
    try {
      const sv = (await (AgentService as any).ListMCPServers?.()) || []
      setMcpServers(sv)
      const prof = await (AgentService as any).ListMCPProfiles?.()
      if (prof) setMcpProfile({ active: prof.active || '', available: prof.available || [] })
    } catch { /* ignore */ }
  }, [])
  const toggleServer = useCallback(async (name: string, enabled: boolean) => {
    setMcpLoading((p) => ({ ...p, [name]: true }))
    try { await (AgentService as any).SetMCPServerEnabled?.(name, enabled) } catch { /* ignore */ }
    refreshMCP()
    setMcpLoading((p) => ({ ...p, [name]: false }))
  }, [refreshMCP])
  const toggleTool = useCallback(async (name: string, enabled: boolean) => {
    setMcpLoading((p) => ({ ...p, [name]: true }))
    try { await (AgentService as any).SetMCPToolEnabled?.(name, enabled) } catch { /* ignore */ }
    refreshMCP()
    setMcpLoading((p) => ({ ...p, [name]: false }))
  }, [refreshMCP])
  const toggleProfile = useCallback(async (name: string) => {
    if (name === mcpProfile.active) return
    setMcpLoading((p) => ({ ...p, __profile__: true }))
    try { await (AgentService as any).SetMCPProfile?.(name) } catch { /* ignore */ }
    refreshMCP()
    setMcpLoading((p) => ({ ...p, __profile__: false }))
  }, [mcpProfile.active, refreshMCP])

  const commitRename = useCallback(async (id: string) => {
    const t = editTitle.trim()
    setEditingId(''); setEditTitle('')
    if (!t) return
    await (AgentService as any).RenameSession?.(id, t).catch(() => {})
    setSessions((prev) => prev.map((s) => (s.id === id ? { ...s, title: t } : s)))
  }, [editTitle])

  const loadAll = useCallback(async () => {
    setLoading(true)
    const list = (await AgentService.ListSessions().catch(() => null)) || []
    let cur = await AgentService.CurrentSession().catch(() => null)
    if (!cur && list.length > 0) cur = list[0]
    if (cur) {
      setCurrentId(cur.id); setCurrentTitle(cur.title || 'Tachi')
      if (!msgCache[cur.id]) {
        const page: any = await (AgentService as any).LoadSession?.(cur.id, PAGE_SIZE)
        if (page?.messages) {
          setMsgCache((prev) => ({ ...prev, [cur!.id]: buildTurns(page.messages) }))
          setHasMore((p) => ({ ...p, [cur!.id]: !!page.hasMore }))
          setEarliestTs((p) => ({ ...p, [cur!.id]: page.messages[0]?.timestamp }))
        }
      }
      refreshCost(cur.id)
      scrollToBottom()
    } else {
      const ns = await AgentService.NewSession().catch(() => null)
      if (ns) {
        setCurrentId(ns.id); setCurrentTitle(ns.title || 'Tachi')
        setMsgCache((p) => ({ ...p, [ns.id]: [] }))
        setHasMore((p) => ({ ...p, [ns.id]: false }))
        setEarliestTs((p) => ({ ...p, [ns.id]: '' }))
        setCost(0); setCredit(0); setTps(0); setLastTps(0)
      }
    }
    setSessions(list.map((s) => ({ ...s, active: s.id === cur?.id })))
    setLoading(false)
    refreshProvider()
    refreshMCP()
    if (cur) refreshWorkDir(cur.id)
  }, [msgCache, scrollToBottom, refreshProvider, refreshCost, refreshMCP, refreshWorkDir])
  const confirmDelete = useCallback(async (id: string) => {
    await (AgentService as any).DeleteSession?.(id).catch(() => {})
    loadAll()
  }, [loadAll])

  const clickSession = useCallback(async (id: string) => {
    setCurrentId(id)
    setCurrentTitle(sessions.find((s) => s.id === id)?.title || 'Tachi')
    setSessions((prev) => prev.map((s) => ({ ...s, active: s.id === id })))
    // Always sync the backend's active session (cheap) so provider/thinking
    // selectors reflect this session even when its messages are already cached.
    await (AgentService as any).ActivateSession?.(id).catch(() => {})
    if (!msgCache[id]) {
      setLoading(true)
      const page: any = await (AgentService as any).LoadSession?.(id, PAGE_SIZE)
      if (page?.messages) {
        setMsgCache((prev) => ({ ...prev, [id]: buildTurns(page.messages) }))
        setHasMore((p) => ({ ...p, [id]: !!page.hasMore }))
        setEarliestTs((p) => ({ ...p, [id]: page.messages[0]?.timestamp }))
      }
      setLoading(false)
    }
    setTps(0); setLastTps(0)
    scrollToBottom()
    refreshRunning()
    refreshCost(id)
    refreshProvider()
    refreshMCP()
    refreshWorkDir(id)
  }, [sessions, msgCache, scrollToBottom, refreshRunning, refreshProvider, refreshCost, refreshMCP, refreshWorkDir])

  const newChat = useCallback(async () => {
    const ns = await AgentService.NewSession().catch(() => null)
    if (ns) {
      setCurrentId(ns.id); setCurrentTitle(ns.title || 'Tachi')
      setMsgCache((prev) => ({ ...prev, [ns.id]: [] }))
      setHasMore((p) => ({ ...p, [ns.id]: false }))
      setEarliestTs((p) => ({ ...p, [ns.id]: '' }))
      setCost(0); setCredit(0); setTps(0); setLastTps(0)
      const list = (await AgentService.ListSessions().catch(() => null)) || []
      setSessions(list.map((s) => ({ ...s, active: s.id === ns.id })))
      refreshProvider()
    }
  }, [refreshProvider])

  useEffect(() => { loadAll(); refreshRunning(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [])

  // Keyboard shortcuts: Cmd+/ focuses the composer, Cmd+B toggles the sidebar.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { setShortcutsOpen(false); setConfirmDel(null); setMenu(null); setReminderModal(null); return }
      if (!e.metaKey) return
      if (e.key === '/' && !e.shiftKey) { e.preventDefault(); composerRef.current?.focus() }
      else if (e.key.toLowerCase() === 'b') { e.preventDefault(); setSidebarCollapsed((v) => !v) }
      else if (e.key === '?' || (e.shiftKey && e.code === 'Slash')) { e.preventDefault(); setShortcutsOpen((v) => !v) }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  useEffect(() => {
    const loadProv = async () => {
      try {
        const ps = (await (AgentService as any).ListProviders?.()) || []
        setProviders(ps)
        const info = await (AgentService as any).GetProviderInfo?.()
        if (info) { setProviderName(info.provider) }
      } catch { /* ignore */ }
    }
    loadProv()
  }, [])

  useEffect(() => {
    const off = Events.On('agent:state', (event) => { setState(event.data as AgentState) })
    AgentService.GetState().then((s) => setState(s)).catch(() => {})
    return () => off?.()
  }, [])

  useEffect(() => {
    const off = Events.On('agent:cost', (event) => {
      const d = event.data as { sessionId: string; cost: number; credit: number; cacheHitRate?: number }
      if (d.sessionId === currentId) { setCost(d.cost || 0); setCredit(d.credit || 0); if (d.cacheHitRate != null) setCacheHitRate(d.cacheHitRate) }
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const off = Events.On('agent:tps', (event) => {
      const d = event.data as { sessionId: string; tps: number; lastTps?: number }
      if (d.sessionId !== currentId) return
      if (d.tps > 0) { setTps(d.tps); setLastTps(0) }
      else { setTps(0); if (d.lastTps) setLastTps(d.lastTps) }
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const off = Events.On('agent:error', (event) => {
      const d = event.data as { sessionId: string; error: string }
      if (d.sessionId !== currentId) return
      setMsgCache((prev) => {
        const list = prev[currentId] || []
        const target = [...list].reverse().find((m) => m.role === 'assistant' && m.running)
        if (!target) return prev
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? { ...m, running: false, text: (m.text || '') + '\n\n⚠ ' + (d.error || '出错') } : m)) }
      })
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const off = Events.On('agent:turn', (event) => {
      const d = event.data as { sessionId: string; durationMs: number; iterations: number; cost: number; credit: number }
      if (d.sessionId !== currentId) return
      setMsgCache((prev) => {
        const list = prev[currentId] || []
        const target = [...list].reverse().find((m) => m.role === 'assistant')
        if (!target) return prev
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? { ...m, summary: d } : m)) }
      })
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const off = Events.On('agent:tool', (event) => {
      const t = event.data as { name: string; title: string; args: string }
      setMsgCache((prev) => {
        const list = prev[currentId] || []
        const target = [...list].reverse().find((m) => m.role === 'assistant' && m.running)
        if (!target) return prev
        const tools = (target.tools || []).map((c) => (c.name === t.name ? { ...c, title: t.title, arguments: t.args } : c))
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? { ...m, tools } : m)) }
      })
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const apply = (sid: string, role: 'user' | 'assistant', fn: (m: Message) => Message) => {
      setMsgCache((prev) => {
        const list = prev[sid] || []
        const target = role === 'user'
          ? [...list].reverse().find((m) => m.role === 'user')
          : [...list].reverse().find((m) => m.role === 'assistant' && m.running)
        if (!target) return prev
        return { ...prev, [sid]: list.map((m) => (m.id === target.id ? fn(m) : m)) }
      })
    }
    const off = Events.On('agent:event', (event) => {
      const d = event.data as { sessionId: string; event: AgentEvent }
      const { sessionId, event: ev } = d
      switch (ev.Type) {
        case 'thinking_delta': apply(sessionId, 'assistant', (m) => ({ ...m, thinking: (m.thinking || '') + (ev.ThinkingDelta || '') })); break
        case 'text_delta': apply(sessionId, 'assistant', (m) => ({ ...m, text: (m.text || '') + ev.TextDelta, thinkingCollapsed: m.thinking ? true : m.thinkingCollapsed })); break
        case 'tool_call_start': apply(sessionId, 'assistant', (m) => { const tools = m.tools || []; tools.push({ name: ev.ToolName, title: '', arguments: '', summary: '执行中…', ok: true, done: false }); return { ...m, tools, thinkingCollapsed: true } }); break
        case 'tool_result': apply(sessionId, 'assistant', (m) => { const tools = (m.tools || []).map((t) => (t.done ? t : { ...t, summary: ev.ToolResult, ok: !ev.ToolIsError, done: true, durationMs: ev.ToolDuration ? Math.round(ev.ToolDuration / 1e6) : undefined })); return { ...m, tools, thinkingCollapsed: true } }); break
        case 'turn_complete': apply(sessionId, 'assistant', (m) => ({ ...m, running: false, thinkingCollapsed: true })); refreshRunning(); break
        case 'error': apply(sessionId, 'assistant', (m) => ({ ...m, running: false })); break
      }
      if (sessionId === currentId) scrollToBottom()
      refreshRunning()
      refreshProvider()
    })
    return () => off?.()
  }, [currentId, refreshRunning, scrollToBottom, refreshProvider])

  const send = useCallback(() => {
    const text = input.trim()
    if (!text) return
    setInput('')
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    const sid = currentId
    const aid = `a-${ts}`
    setSessionMsgs(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: tsStr },
      { id: aid, role: 'assistant', running: true, text: '', ts: tsStr },
    ])
    AgentService.SendMessage(text).catch(() => {})
    setRunningSet((prev) => new Set(prev).add(sid))
    scrollToBottom()
  }, [input, currentId, setSessionMsgs, scrollToBottom])

  const toggleThinking = useCallback((id: string) => setSessionMsgs(currentId, (prev) => prev.map((m) => (m.id === id ? { ...m, thinkingCollapsed: !m.thinkingCollapsed } : m))), [currentId, setSessionMsgs])

  const meta = STATUS_META[state.status as string] ?? STATUS_META.idle

  return (
    <div className="app">
      <header className="titlebar drag-region">
        <div className="brand no-drag">
          <span className="brand-mark"><svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><defs><linearGradient id="tg1" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stopColor="#8aa2ff" /><stop offset="1" stopColor="#4b5fd6" /></linearGradient></defs><rect x="1" y="1" width="14" height="14" rx="4.5" fill="url(#tg1)" /><path d="M8 3.8 L12.2 8 L8 12.2 L3.8 8 Z" fill="#fff" opacity="0.95" /></svg></span>
          <span className="brand-name">Tachi</span>
        </div>
        <button className={`sidebar-toggle no-drag ${sidebarCollapsed ? 'is-collapsed' : ''}`} onClick={() => setSidebarCollapsed((v) => !v)} title={sidebarCollapsed ? '展开会话侧栏' : '收起会话侧栏'}>
          <svg width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"><rect x="1.5" y="2.5" width="13" height="11" rx="2"/><line x1="6" y1="2.5" x2="6" y2="13.5"/></svg>
        </button>
        <div className="titlebar-divider" />
        <div className="titlebar-title no-drag">
          {currentId ? <span className="session-id" title={currentId + '（点击复制）'} onClick={() => navigator.clipboard?.writeText(currentId).catch(() => {})}>{currentId}</span> : null}
        </div>
        <div className="status-badge no-drag"><span className={`dot dot-${state.status}`}>{meta.dot}</span><span className="status-label">{state.label}</span></div>
      </header>

      <div className="app-body">
        <aside className={`sidebar${sidebarCollapsed ? ' collapsed' : ''}`}>
          <button className="new-chat" onClick={newChat}><span className="new-chat-plus">＋</span> 新建会话</button>
          <nav className="session-list">
            <div className="session-section">最近</div>
            {sessions.map((s) => (
              <div key={s.id} className={`session ${s.active ? 'active' : ''}`} onClick={() => clickSession(s.id)}
                onContextMenu={(e) => { e.preventDefault(); setMenu({ sid: s.id, x: e.clientX, y: e.clientY }) }}>
                {editingId === s.id ? (
                  <input className="session-rename" autoFocus value={editTitle}
                    onChange={(e) => setEditTitle(e.target.value)}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') { e.stopPropagation(); commitRename(s.id) }
                      else if (e.key === 'Escape') { e.stopPropagation(); setEditingId(''); setEditTitle('') }
                    }} />
                ) : (
                  <div className="session-title" onDoubleClick={() => { setEditingId(s.id); setEditTitle(s.title || ''); setMenu(null) }}>{s.title || '未命名会话'}</div>
                )}
                <div className="session-meta">{runningSet.has(s.id) ? <span className="spin-dot" title="运行中" /> : null}{new Date(s.updatedAt).toLocaleString('zh-CN', { hour12: false })}</div>
              </div>
            ))}
            {sessions.length === 0 && <div className="session-meta" style={{ padding: '6px 8px' }}>暂无会话</div>}
          </nav>
          <footer className="sidebar-footer">
            <div className="footer-item"><span className="footer-ico">⚙</span> 设置</div>
            <div className="footer-item"><span className="footer-ico">¤</span> 用量</div>
          </footer>
        </aside>

        <main className="main">
          <div className="chat" ref={chatRef} onScroll={handleScroll}>
            {loading ? <div className="chat-loading">加载会话…</div> : (
              <>
                {messages.length === 0 && (
                  <div className="welcome"><div className="welcome-mark">◆</div><div className="welcome-title">你好，我是 Tachi</div><div className="welcome-sub">在下方输入问题开始对话。左侧可切换或新建会话。</div></div>
                )}
                {messages.map((m) =>
                  m.role === 'user' ? (
                    <Fragment key={m.id}>
                      <MessageBubble role="user">
                        {m.text}
                        <span className="user-meta">
                          {m.reminder ? <button className="reminder-head" title="系统提醒" onClick={() => setReminderModal(m.reminder || '')}><span className="reminder-ico">!</span></button> : null}
                          {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
                        </span>
                      </MessageBubble>
                    </Fragment>
                  ) : (
                    <MessageBubble key={m.id} role="assistant">
                      {m.parts ? (
                        <div className="turn-parts">
                          {m.parts.map((p, i) => {
                            if (p.type === 'thinking') return <ThinkingPart key={i} text={p.text || ''} />
                            if (p.type === 'tool') return <ToolCard key={i} name={p.name || ''} title={p.title} args={p.args} summary={p.summary || ''} ok={!!p.ok} />
                            return <div key={i} className="assistant-text"><ReactMarkdown remarkPlugins={[remarkGfm]}>{p.text}</ReactMarkdown></div>
                          })}
                        </div>
                      ) : (
                        <>
                          {m.thinking ? <ThinkingBlock thinking={m.thinking} collapsed={!!m.thinkingCollapsed} onToggle={() => toggleThinking(m.id)} /> : null}
                          {(m.tools || []).map((t, i) => <ToolCard key={i} name={t.name} title={t.title} args={t.arguments} summary={t.summary} ok={t.ok} durationMs={t.durationMs} />)}
                          {m.text ? <div className="assistant-text"><ReactMarkdown remarkPlugins={[remarkGfm]}>{m.text}</ReactMarkdown></div> : null}
                          {m.running ? <span className="running"><span className="typing"><i></i><i></i><i></i></span>{state.status === 'thinking' ? '正在思考…' : '正在执行…'}</span> : null}
                        </>
                      )}
                      {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
                      {m.summary ? (
                        <div className="msg-footer">
                          {m.summary.durationMs > 0 ? <span>⏱ {fmtDur(m.summary.durationMs)}</span> : null}
                          {m.summary.iterations > 0 ? <span>{m.summary.iterations} iters</span> : null}
                          {m.summary.cost > 0 ? <span>¥{m.summary.cost.toFixed(3)}</span> : null}
                          {m.summary.credit > 0 ? <span>{m.summary.credit} 积分</span> : null}
                        </div>
                      ) : null}
                    </MessageBubble>
                  ),
                )}
              </>
            )}
          </div>

          <footer className="composer">
            <div className="composer-box">
              <textarea className="composer-input" ref={composerRef} value={input} onChange={(e) => setInput(e.target.value)}
                onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send() } }}
                placeholder="发送消息给 Tachi…（Enter 发送，Shift+Enter 换行）" />
              <div className="composer-actions">
                <button className="icon-btn" title="附件">＋</button>
                <button className="send-btn" onClick={send} disabled={!input.trim()}>
                  <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
                  <span>发送</span>
                </button>
              </div>
            </div>
            <div className="composer-status">
              <div className="work-dir-wrap">
                {editingWd ? (
                  <input className="work-dir-input" autoFocus value={wdInput}
                    onChange={(e) => setWdInput(e.target.value)}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') { e.stopPropagation(); commitWorkDir(currentId, wdInput) }
                      else if (e.key === 'Escape') { e.stopPropagation(); setEditingWd(false) }
                    }}
                    onBlur={() => setEditingWd(false)} />
                ) : (
                  <span className="work-dir" title="工作目录（点击修改）" onClick={() => { setWdInput(workDir); setEditingWd(true) }}>
                    <span className="work-dir-ico">⌂</span>{workDir || '未设置工作目录'}
                  </span>
                )}
              </div>
              <div className="provider-picker">
                <select className="provider-select" value={providerName} onChange={async (e) => {
                  const name = e.target.value
                  await (AgentService as any).SwitchProvider?.(name)
                  refreshProvider()
                }}>
                  {providers.map((p) => <option key={p.name ?? p.Name} value={p.name ?? p.Name}>{(p.name ?? p.Name)} · {(p.model ?? p.Model)}</option>)}
                </select>
                <select className="provider-select" value={thinkingLevel} onChange={async (e) => {
                  const lv = e.target.value
                  await (AgentService as any).SetThinkingLevel?.(lv)
                  setThinkingLevel(lv)
                }}>
                  {THINKING_LEVELS.map((l) => <option key={l} value={l}>{l}</option>)}
                </select>
                <ContextRing estimate={ctxEstimate} window={ctxWindow} />
                <span className="usage-meta">
                  {tps > 0 ? <span className={`usage-tps tps-${tpsTier(tps)}`} title="当前输出速率">{tps}/s</span>
                    : lastTps > 0 ? <span className="usage-tps tps-paused" title="最近输出速率">{lastTps}/s</span>
                    : null}
                  {cacheHitRate > 0 ? <CacheRing rate={cacheHitRate} /> : null}
                  {cost > 0 ? <span className="usage-cost" title="当前会话成本">¥{cost.toFixed(3)}</span> : null}
                  {credit > 0 ? <span className="usage-credit" title="当前会话积分">{credit} 积分</span> : null}
                </span>
                <button className="mcp-btn" title="MCP servers / tools" onClick={() => setMcpOpen((v) => !v)}>
                  <span className="mcp-ico">M</span>
                  <span className="mcp-count">{mcpServers.filter((s) => s.connected).length || ''}</span>
                </button>
              </div>
            </div>
          </footer>
        </main>
      </div>
      {mcpOpen && (
        <MCPPanel
          servers={mcpServers}
          loading={mcpLoading}
          profile={mcpProfile}
          onClose={() => setMcpOpen(false)}
          onToggleServer={toggleServer}
          onToggleTool={toggleTool}
          onToggleProfile={toggleProfile}
        />
      )}
      {menu && (
        <div className="ctx-menu" style={{ left: menu.x, top: menu.y }} onMouseLeave={() => setMenu(null)}>
          <div className="ctx-item" onClick={() => { setEditingId(menu.sid); setEditTitle(sessions.find((x) => x.id === menu.sid)?.title || ''); setMenu(null) }}>重命名</div>
          <div className="ctx-item danger" onClick={() => { const t = sessions.find((x) => x.id === menu.sid)?.title || ''; setConfirmDel({ sid: menu.sid, title: t }); setMenu(null) }}>删除</div>
        </div>
      )}
      {confirmDel && (
        <div className="confirm-overlay" onClick={() => setConfirmDel(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">删除会话「{confirmDel.title || '未命名会话'}」？</div>
            <div className="confirm-sub">此操作不可恢复。</div>
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setConfirmDel(null)}>取消</button>
              <button className="btn danger" onClick={() => { const id = confirmDel.sid; setConfirmDel(null); confirmDelete(id) }}>删除</button>
            </div>
          </div>
        </div>
      )}
      {shortcutsOpen && (
        <div className="confirm-overlay" onClick={() => setShortcutsOpen(false)}>
          <div className="confirm-box shortcuts-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">快捷键</div>
            <div className="shortcut-row"><kbd>⌘ /</kbd><span>聚焦输入框</span></div>
            <div className="shortcut-row"><kbd>⌘ B</kbd><span>折叠 / 展开侧栏</span></div>
            <div className="shortcut-row"><kbd>⌘ ?</kbd><span>显示本快捷键列表</span></div>
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setShortcutsOpen(false)}>关闭（Esc）</button>
            </div>
          </div>
        </div>
      )}
      {reminderModal && (
        <div className="confirm-overlay" onClick={() => setReminderModal(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">系统提醒</div>
            <div className="reminder-modal-body">{reminderModal}</div>
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setReminderModal(null)}>关闭（Esc）</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

// humanize renders a token count as a compact human-friendly number, e.g.
// 128512 → "128.5K", 2000000 → "2M". Used for the context-ring tooltip.
// Token counts never reach the G scale (context windows max out around M), so
// only K/M are emitted; anything above M is shown verbatim as a fallback.
export default App
