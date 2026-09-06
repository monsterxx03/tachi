import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Events } from '@wailsio/runtime'
import {
  AgentService,
  AgentStatus,
  type AgentState,
  type SessionInfo,
  type SessionMessage,
} from '../bindings/github.com/monsterxx03/tachi/desktop'

interface SessionItem extends SessionInfo { active?: boolean }

interface ToolCardData { name: string; title?: string; arguments?: string; summary: string; ok: boolean; done?: boolean }

interface Part {
  type: 'thinking' | 'text' | 'tool'
  text?: string
  name?: string; title?: string; args?: string; summary?: string; ok?: boolean; done?: boolean; toolCallId?: string
}

interface Message {
  id: string
  role: 'user' | 'assistant'
  text?: string
  ts?: string
  reminder?: string
  reminderCollapsed?: boolean
  parts?: Part[]             // historical assistant turn: ordered segments
  thinking?: string          // streaming turn
  thinkingCollapsed?: boolean
  tools?: ToolCardData[]     // streaming turn
  running?: boolean
}

const STATUS_META: Record<string, { dot: string; desc: string }> = {
  idle: { dot: '●', desc: '空闲' }, thinking: { dot: '◐', desc: '思考中' }, tool_running: { dot: '◑', desc: '执行工具' },
  busy: { dot: '◒', desc: '处理中' }, error: { dot: '▲', desc: '出错' },
}

const THINKING_LEVELS = ['default', 'none', 'low', 'medium', 'high', 'xhigh', 'max']
// PAGE_SIZE is the number of raw session messages loaded per "page" when
// switching to a session or scrolling up for older history.
const PAGE_SIZE = 100

interface AgentEvent {
  Type: string; TextDelta: string; ThinkingDelta: string; ToolName: string; ToolResult: string; ToolIsError: boolean
}

function extractReminder(content: string): { reminder: string; text: string } {
  const re = /<system-reminder>([\s\S]*?)<\/system-reminder>/g
  const blocks: string[] = []; let m
  while ((m = re.exec(content))) blocks.push(m[1].trim())
  const text = content.replace(re, '').trim()
  return { reminder: blocks.join('\n'), text }
}

function fmtTime(ts?: string): string {
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

// Rebuild turns from RAW session messages, preserving the real in-turn order:
// one assistant card per turn with interleaved thinking / assistant text / tool cards.
function buildTurns(sms: SessionMessage[]): Message[] {
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

function App() {
  const [sessions, setSessions] = useState<SessionItem[]>([])
  const [currentId, setCurrentId] = useState<string>('')
  const [currentTitle, setCurrentTitle] = useState<string>('Tachi')
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
      if (u) { setCost(u.cost || 0); setCredit(u.credit || 0) }
    } catch { /* ignore */ }
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
  }, [msgCache, scrollToBottom, refreshProvider, refreshCost, refreshMCP])

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
  }, [sessions, msgCache, scrollToBottom, refreshRunning, refreshProvider, refreshCost, refreshMCP])

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
      const d = event.data as { sessionId: string; cost: number; credit: number }
      if (d.sessionId === currentId) { setCost(d.cost || 0); setCredit(d.credit || 0) }
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
        case 'tool_result': apply(sessionId, 'assistant', (m) => { const tools = (m.tools || []).map((t) => (t.done ? t : { ...t, summary: ev.ToolResult, ok: !ev.ToolIsError, done: true })); return { ...m, tools, thinkingCollapsed: true } }); break
        case 'turn_complete': apply(sessionId, 'assistant', (m) => ({ ...m, running: false, thinkingCollapsed: true })); refreshRunning(); break
        case 'error': apply(sessionId, 'assistant', (m) => ({ ...m, running: false, text: (m.text || '') + '\n（出错，见日志）' })); break
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
      { id: aid, role: 'assistant', running: true, text: '' },
    ])
    AgentService.SendMessage(text).catch(() => {})
    setRunningSet((prev) => new Set(prev).add(sid))
    scrollToBottom()
  }, [input, currentId, setSessionMsgs, scrollToBottom])

  const toggleThinking = useCallback((id: string) => setSessionMsgs(currentId, (prev) => prev.map((m) => (m.id === id ? { ...m, thinkingCollapsed: !m.thinkingCollapsed } : m))), [currentId, setSessionMsgs])
  const toggleReminder = useCallback((id: string) => setSessionMsgs(currentId, (prev) => prev.map((m) => (m.id === id ? { ...m, reminderCollapsed: !m.reminderCollapsed } : m))), [currentId, setSessionMsgs])

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
          <h2>{currentTitle}</h2>
          <span className="session-meta">{runningSet.has(currentId) ? '运行中' : `${sessions.length} 个会话`}</span>
        </div>
        <div className="status-badge no-drag"><span className={`dot dot-${state.status}`}>{meta.dot}</span><span className="status-label">{state.label}</span></div>
      </header>

      <div className="app-body">
        <aside className={`sidebar${sidebarCollapsed ? ' collapsed' : ''}`}>
          <button className="new-chat" onClick={newChat}><span className="new-chat-plus">＋</span> 新建会话</button>
          <nav className="session-list">
            <div className="session-section">最近</div>
            {sessions.map((s) => (
              <div key={s.id} className={`session ${s.active ? 'active' : ''}`} onClick={() => clickSession(s.id)}>
                <div className="session-title">{s.title || '未命名会话'}</div>
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
                      {m.reminder ? <ReminderBadge reminder={m.reminder} collapsed={!!m.reminderCollapsed} onToggle={() => toggleReminder(m.id)} /> : null}
                      <MessageBubble role="user">{m.text}{m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}</MessageBubble>
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
                          {(m.tools || []).map((t, i) => <ToolCard key={i} name={t.name} title={t.title} args={t.arguments} summary={t.summary} ok={t.ok} />)}
                          {m.text ? <div className="assistant-text"><ReactMarkdown remarkPlugins={[remarkGfm]}>{m.text}</ReactMarkdown></div> : null}
                          {m.running ? <span className="running"><span className="typing"><i></i><i></i><i></i></span>{state.status === 'thinking' ? '正在思考…' : '正在执行…'}</span> : null}
                        </>
                      )}
                      {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
                    </MessageBubble>
                  ),
                )}
              </>
            )}
          </div>

          <footer className="composer">
            <div className="composer-box">
              <textarea className="composer-input" value={input} onChange={(e) => setInput(e.target.value)}
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
    </div>
  )
}

// humanize renders a token count as a compact human-friendly number, e.g.
// 128512 → "128.5K", 2000000 → "2M". Used for the context-ring tooltip.
// Token counts never reach the G scale (context windows max out around M), so
// only K/M are emitted; anything above M is shown verbatim as a fallback.
const HUMANIZE_UNITS = [
  { v: 1e6, s: 'M' },
  { v: 1e3, s: 'K' },
] as const
function humanize(n: number): string {
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
function tpsTier(t: number): string {
  return t >= 200 ? 'fast' : t >= 60 ? 'normal' : 'slow'
}

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
      <div className="thinking-head" onClick={onToggle} title="思考过程"><span className="thinking-ico">◐</span></div>
      {!collapsed && <div className="thinking-body" ref={bodyRef}>{lines.map((l, i) => <div key={i} className="thinking-line">{l}</div>)}</div>}
    </div>
  )
}

function ReminderBadge({ reminder, collapsed, onToggle }: { reminder: string; collapsed: boolean; onToggle: () => void }) {
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => { if (!collapsed && bodyRef.current) bodyRef.current.scrollTop = bodyRef.current.scrollHeight }, [reminder, collapsed])
  const lines = reminder.split('\n')
  return (
    <div className="think-badge">
      <button className="think-dot" onClick={onToggle} title="系统提醒"><span className="think-icon">!</span></button>
      {!collapsed && <div className="think-bubble" ref={bodyRef}>{lines.map((l, i) => <div key={i} className="think-line">{l}</div>)}</div>}
    </div>
  )
}

function MessageBubble({ role, children }: { role: 'user' | 'assistant'; children: ReactNode }) {
  return (<div className={`msg msg-${role}`}><div className="msg-avatar">{role === 'user' ? 'U' : '◆'}</div><div className="msg-content">{children}</div></div>)
}

function CopyIcon() {
  return (<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" /><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" /></svg>)
}

function ToolCard({ name, title, args, summary, ok }: { name: string; title?: string; args?: string; summary: string; ok: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const long = ((args?.length || 0) + summary.length) > 120
  const prettyArgs = (() => { if (!args) return ''; try { return JSON.stringify(JSON.parse(args), null, 2) } catch { return args } })()
  const fallbackCopy = (text: string) => { try { const ta = document.createElement('textarea'); ta.value = text; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta) } catch { /* ignore */ } }
  const copy = (text: string) => { if (!text) return; try { navigator.clipboard.writeText(text).catch(() => fallbackCopy(text)) } catch { fallbackCopy(text) } }
  return (
    <div className="tool-card">
      <div className="tool-head" style={{ cursor: 'pointer' }} onClick={() => setExpanded((e) => !e)}>
        <span className="tool-ico">⚙</span><span className="tool-name">{name}</span>
        {title ? <span className="tool-title">{title}</span> : null}
        <span className={`tool-status ${ok ? 'ok' : 'err'}`}>{ok ? '✓' : '✗'}</span>
        {long ? <span className="tool-toggle">{expanded ? '收起' : '展开'}</span> : null}
      </div>
      {expanded && args ? <div className="tool-args-wrap"><div className="tool-args-bar"><span className="tool-args-label">参数</span><button className="tool-copy" title="复制参数" onClick={(e) => { e.stopPropagation(); copy(args) }}><CopyIcon /></button></div><pre className="tool-args">{prettyArgs}</pre></div> : null}
      {summary ? <div className={`tool-summary ${long && !expanded ? 'clamp' : ''}`}>{summary}</div> : null}
      <div className="tool-meta"><code>$ {name} …</code>{summary ? <button className="tool-copy" title="复制结果" onClick={(e) => { e.stopPropagation(); copy(summary) }}><CopyIcon /></button> : null}<span>0.8s</span></div>
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
                <div className="mcp-server-row" onClick={() => setCollapsed((p) => ({ ...p, [s.name]: !p[s.name] }))}>
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

export default App
