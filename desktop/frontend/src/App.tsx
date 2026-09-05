import { Fragment, useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import { Events } from '@wailsio/runtime'
import {
  AgentService,
  AgentStatus,
  type AgentState,
  type SessionInfo,
  type SessionMessage,
} from '../bindings/github.com/monsterxx03/tachi/desktop'

interface SessionItem extends SessionInfo {
  active?: boolean
}

interface ToolCardData {
  name: string
  summary: string
  ok: boolean
  done?: boolean
}

interface Message {
  id: string
  role: 'user' | 'assistant'
  text?: string
  reminder?: string
  reminderCollapsed?: boolean
  thinking?: string
  thinkingCollapsed?: boolean
  tools?: ToolCardData[]
  running?: boolean
}

const STATUS_META: Record<string, { dot: string; desc: string }> = {
  idle: { dot: '●', desc: '空闲' },
  thinking: { dot: '◐', desc: '思考中' },
  tool_running: { dot: '◑', desc: '执行工具' },
  busy: { dot: '◒', desc: '处理中' },
  error: { dot: '▲', desc: '出错' },
}

interface AgentEvent {
  Type: string
  TextDelta: string
  ThinkingDelta: string
  ToolName: string
  ToolResult: string
  ToolIsError: boolean
}

// Pull the trailing/inline <system-reminder> blocks out of a user message so
// they can be shown collapsed as a badge instead of as part of the text.
function extractReminder(content: string): { reminder: string; text: string } {
  const re = /<system-reminder>([\s\S]*?)<\/system-reminder>/g
  const blocks: string[] = []
  let m
  while ((m = re.exec(content))) blocks.push(m[1].trim())
  const text = content.replace(re, '').trim()
  return { reminder: blocks.join('\n'), text }
}

// Group a flat LLM message list (from session history) back into conversational
// turns: an assistant message plus its tool_call/tool_result become ONE
// assistant bubble with tool cards.
function buildTurns(msgs: SessionMessage[]): Message[] {
  const turns: Message[] = []
  let cur: Message | null = null
  msgs.forEach((sm) => {
    if (sm.role === 'system') return
    if (sm.role === 'user') {
      const r = extractReminder(sm.content)
      turns.push({
        id: `h-${turns.length}`,
        role: 'user',
        text: r.text,
        reminder: r.reminder || undefined,
        reminderCollapsed: r.reminder ? true : undefined, // default folded
      })
      cur = null
      return
    }
    if (sm.role === 'assistant') {
      const tools = (sm.toolCalls || []).map((tc) => ({ name: tc.name, summary: '', ok: true, done: false }))
      cur = {
        id: `h-${turns.length}`,
        role: 'assistant',
        text: sm.content,
        thinking: sm.thinking || undefined,
        thinkingCollapsed: sm.thinking ? true : undefined, // default folded
        tools: tools.length ? tools : undefined,
      }
      turns.push(cur)
      return
    }
    if (sm.role === 'tool') {
      if (cur) {
        const pending = (cur.tools || []).find((t) => !t.done)
        if (pending) {
          pending.summary = (sm.toolResult || '')
          pending.ok = true
          pending.done = true
        } else {
          cur.tools = cur.tools || []
          cur.tools.push({ name: sm.toolName || 'tool', summary: (sm.toolResult || ''), ok: true, done: true })
        }
      }
      return
    }
  })
  return turns
}

function App() {
  const [sessions, setSessions] = useState<SessionItem[]>([])
  const [currentTitle, setCurrentTitle] = useState<string>('Tachi')
  const [messages, setMessages] = useState<Message[]>([])
  const [state, setState] = useState<AgentState>({
    status: AgentStatus.StatusIdle,
    label: '空闲',
    detail: '就绪',
  })
  const [input, setInput] = useState('')
  const [loading, setLoading] = useState(false)
  const [visibleCount, setVisibleCount] = useState(80)
  const chatRef = useRef<HTMLDivElement>(null)

  const scrollToBottom = () => {
    setTimeout(() => {
      const el = chatRef.current
      if (el) el.scrollTop = el.scrollHeight
    }, 60)
  }

  const loadMoreRef = useRef(false)
  const handleScroll = () => {
    const el = chatRef.current
    if (!el || loadMoreRef.current || loading) return
    if (messages.length === 0 || visibleCount >= messages.length) return
    // Reached the top → load earlier messages, keeping the viewport anchored.
    if (el.scrollTop <= 40) {
      loadMoreRef.current = true
      const old = el.scrollHeight
      setVisibleCount((v) => v + 100)
      requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          const c = chatRef.current
          if (c) c.scrollTop = c.scrollHeight - old
          loadMoreRef.current = false
        })
      })
    }
  }

  const refreshSessions = useCallback(async (activeId: string) => {
    const list = (await AgentService.ListSessions().catch(() => null)) || []
    setSessions(list.map((s) => ({ ...s, active: s.id === activeId })))
  }, [])

  const loadAll = useCallback(async () => {
    setLoading(true)
    setVisibleCount(80)
    const list = (await AgentService.ListSessions().catch(() => null)) || []
    let cur = await AgentService.CurrentSession().catch(() => null)
    if (!cur && list.length > 0) cur = list[0]
    if (cur) {
      setCurrentTitle(cur.title || 'Tachi')
      const msgs = (await AgentService.LoadSession(cur.id).catch(() => null)) || []
      setMessages(buildTurns(msgs))
      scrollToBottom()
    } else {
      const ns = await AgentService.NewSession().catch(() => null)
      if (ns) {
        setCurrentTitle(ns.title || 'Tachi')
        setMessages([])
      }
    }
    setSessions(list.map((s) => ({ ...s, active: s.id === cur?.id })))
    setLoading(false)
  }, [])

  const clickSession = useCallback(async (id: string) => {
    setLoading(true)
    setVisibleCount(80)
    const msgs = (await AgentService.LoadSession(id).catch(() => null)) || []
    setMessages(buildTurns(msgs))
    setCurrentTitle(sessions.find((s) => s.id === id)?.title || 'Tachi')
    setSessions((prev) => prev.map((s) => ({ ...s, active: s.id === id })))
    setLoading(false)
    scrollToBottom()
  }, [sessions])

  const newChat = useCallback(async () => {
    const ns = await AgentService.NewSession().catch(() => null)
    if (ns) {
      setCurrentTitle(ns.title || 'Tachi')
      setMessages([])
      await refreshSessions(ns.id)
    }
  }, [refreshSessions])

  useEffect(() => {
    loadAll()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    const off = Events.On('agent:state', (event) => {
      setState(event.data as AgentState)
    })
    AgentService.GetState().then((s) => setState(s)).catch(() => {})
    return () => off?.()
  }, [])

  useEffect(() => {
    const off = Events.On('agent:event', (event) => {
      const ev = event.data as AgentEvent

      const mutate = (role: 'user' | 'assistant', fn: (m: Message) => Message) => {
        setMessages((prev) => {
          const target =
            role === 'user'
              ? [...prev].reverse().find((m) => m.role === 'user')
              : [...prev].reverse().find((m) => m.role === 'assistant' && m.running)
          if (!target) return prev
          return prev.map((m) => (m.id === target.id ? fn(m) : m))
        })
      }

      switch (ev.Type) {
        case 'thinking_delta':
          mutate('assistant', (m) => ({ ...m, thinking: (m.thinking || '') + (ev.ThinkingDelta || '') }))
          break
        case 'text_delta':
          mutate('assistant', (m) => ({ ...m, text: (m.text || '') + ev.TextDelta, thinkingCollapsed: m.thinking ? true : m.thinkingCollapsed }))
          break
        case 'tool_call_start':
          mutate('assistant', (m) => {
            const tools = m.tools || []
            tools.push({ name: ev.ToolName, summary: '执行中…', ok: true, done: false })
            return { ...m, tools, thinkingCollapsed: true }
          })
          break
        case 'tool_result':
          mutate('assistant', (m) => {
            const tools = (m.tools || []).map((t) => (t.done ? t : { ...t, summary: ev.ToolResult, ok: !ev.ToolIsError, done: true }))
            return { ...m, tools, thinkingCollapsed: true }
          })
          break
        case 'turn_complete':
          mutate('assistant', (m) => ({ ...m, running: false, thinkingCollapsed: true }))
          break
        case 'error':
          mutate('assistant', (m) => ({ ...m, running: false, text: (m.text || '') + '\n（出错，见日志）' }))
          break
      }
    })
    return () => off?.()
  }, [])

  const send = useCallback(() => {
    const text = input.trim()
    if (!text) return
    setInput('')
    const ts = Date.now()
    const assistantId = `a-${ts}`
    setMessages((prev) => [
      ...prev,
      { id: `u-${ts}`, role: 'user', text },
      { id: assistantId, role: 'assistant', running: true, text: '' },
    ])
    AgentService.SendMessage(text).catch(() => {})
  }, [input])

  const toggleThinking = useCallback((id: string) => {
    setMessages((prev) => prev.map((m) => (m.id === id ? { ...m, thinkingCollapsed: !m.thinkingCollapsed } : m)))
  }, [])

  const toggleReminder = useCallback((id: string) => {
    setMessages((prev) => prev.map((m) => (m.id === id ? { ...m, reminderCollapsed: !m.reminderCollapsed } : m)))
  }, [])

  const meta = STATUS_META[state.status as string] ?? STATUS_META.idle

  return (
    <div className="app">
      <header className="titlebar drag-region">
        <div className="brand no-drag">
          <span className="brand-mark">
            <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
              <defs>
                <linearGradient id="tg1" x1="0" y1="0" x2="1" y2="1">
                  <stop offset="0" stopColor="#8aa2ff" />
                  <stop offset="1" stopColor="#4b5fd6" />
                </linearGradient>
              </defs>
              <rect x="1" y="1" width="14" height="14" rx="4.5" fill="url(#tg1)" />
              <path d="M8 3.8 L12.2 8 L8 12.2 L3.8 8 Z" fill="#fff" opacity="0.95" />
            </svg>
          </span>
          <span className="brand-name">Tachi</span>
        </div>

        <div className="titlebar-divider" />

        <div className="titlebar-title no-drag">
          <h2>{currentTitle}</h2>
          <span className="session-meta">{sessions.length} 个会话</span>
        </div>

        <div className="status-badge no-drag">
          <span className={`dot dot-${state.status}`}>{meta.dot}</span>
          <span className="status-label">{state.label}</span>
        </div>
      </header>

      <div className="app-body">
        <aside className="sidebar">
          <button className="new-chat" onClick={newChat}>
            <span className="new-chat-plus">＋</span> 新建会话
          </button>

          <nav className="session-list">
            <div className="session-section">最近</div>
            {sessions.map((s) => (
              <div key={s.id} className={`session ${s.active ? 'active' : ''}`} onClick={() => clickSession(s.id)}>
                <div className="session-title">{s.title || '未命名会话'}</div>
                <div className="session-meta">{new Date(s.updatedAt).toLocaleString('zh-CN', { hour12: false })}</div>
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
            {loading ? (
              <div className="chat-loading">加载会话…</div>
            ) : (
              <>
                {visibleCount < messages.length && null}
                {messages.length === 0 && (
                  <div className="welcome">
                    <div className="welcome-mark">◆</div>
                    <div className="welcome-title">你好，我是 Tachi</div>
                    <div className="welcome-sub">在下方输入问题开始对话。左侧可切换或新建会话。</div>
                  </div>
                )}
                {messages.slice(-visibleCount).map((m) =>
              m.role === 'user' ? (
                <Fragment key={m.id}>
                  {m.reminder ? (
                    <ReminderBadge reminder={m.reminder} collapsed={!!m.reminderCollapsed} onToggle={() => toggleReminder(m.id)} />
                  ) : null}
                  <MessageBubble role="user">{m.text}</MessageBubble>
                </Fragment>
              ) : (
                <MessageBubble key={m.id} role="assistant">
                  {m.thinking ? (
                    <ThinkingBlock thinking={m.thinking} collapsed={!!m.thinkingCollapsed} onToggle={() => toggleThinking(m.id)} />
                  ) : null}
                  {(m.tools || []).map((t, i) => <ToolCard key={i} name={t.name} summary={t.summary} ok={t.ok} />)}
                  {m.text ? (
                    <div className="assistant-text">
                      <ReactMarkdown>{m.text}</ReactMarkdown>
                    </div>
                  ) : null}
                  {m.running ? (
                    <span className="running">
                      <span className="typing"><i></i><i></i><i></i></span>
                      {state.status === 'thinking' ? '正在思考…' : '正在执行…'}
                    </span>
                  ) : null}
                </MessageBubble>
              ),
            )}
              </>
            )}
          </div>

          <footer className="composer">
            <div className="composer-box">
              <textarea
                className="composer-input"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    send()
                  }
                }}
                placeholder="发送消息给 Tachi…（Enter 发送，Shift+Enter 换行）"
              />
              <div className="composer-actions">
                <button className="icon-btn" title="附件">＋</button>
                <button className="send-btn" onClick={send} disabled={!input.trim()}>
                  <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                    <line x1="5" y1="12" x2="19" y2="12" />
                    <polyline points="12 5 19 12 12 19" />
                  </svg>
                  <span>发送</span>
                </button>
              </div>
            </div>
            <div className="composer-status">
              <span className="composer-dot">{meta.dot}</span>
              <span className="composer-state">{state.label} · {state.detail}</span>
            </div>
          </footer>
        </main>
      </div>
    </div>
  )
}

function ThinkingBlock({ thinking, collapsed, onToggle }: { thinking: string; collapsed: boolean; onToggle: () => void }) {
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!collapsed && bodyRef.current) {
      bodyRef.current.scrollTop = bodyRef.current.scrollHeight
    }
  }, [thinking, collapsed])
  const lines = thinking.split('\n')
  return (
    <div className="thinking-block">
      <div className="thinking-head" onClick={onToggle}>
        <span className="thinking-ico">◐</span>
        <span className="thinking-label">思考过程</span>
        <span className="thinking-toggle">{collapsed ? '展开' : '收起'}</span>
      </div>
      {!collapsed && (
        <div className="thinking-body" ref={bodyRef}>
          {lines.map((l, i) => (
            <div key={i} className="thinking-line">{l}</div>
          ))}
        </div>
      )}
    </div>
  )
}

function ReminderBadge({ reminder, collapsed, onToggle }: { reminder: string; collapsed: boolean; onToggle: () => void }) {
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!collapsed && bodyRef.current) {
      bodyRef.current.scrollTop = bodyRef.current.scrollHeight
    }
  }, [reminder, collapsed])
  const lines = reminder.split('\n')
  return (
    <div className="think-badge">
      <button className="think-dot" onClick={onToggle} title="系统提醒">
        <span className="think-icon">!</span>
      </button>
      {!collapsed && (
        <div className="think-bubble" ref={bodyRef}>
          {lines.map((l, i) => (
            <div key={i} className="think-line">{l}</div>
          ))}
        </div>
      )}
    </div>
  )
}

function MessageBubble({ role, children }: { role: 'user' | 'assistant'; children: ReactNode }) {
  return (
    <div className={`msg msg-${role}`}>
      <div className="msg-avatar">{role === 'user' ? 'U' : '◆'}</div>
      <div className="msg-content">{children}</div>
    </div>
  )
}

function ToolCard({ name, summary, ok }: { name: string; summary: string; ok: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const long = summary.length > 120
  return (
    <div className="tool-card">
      <div className="tool-head" style={{ cursor: 'pointer' }} onClick={() => setExpanded((e) => !e)}>
        <span className="tool-ico">⚙</span>
        <span className="tool-name">{name}</span>
        <span className={`tool-status ${ok ? 'ok' : 'err'}`}>{ok ? '✓' : '✗'}</span>
        {long ? <span className="tool-toggle">{expanded ? '收起' : '展开'}</span> : null}
      </div>
      <div className={`tool-summary ${long && !expanded ? 'clamp' : ''}`}>{summary}</div>
      <div className="tool-meta"><code>$ {name} …</code> <span>0.8s</span></div>
    </div>
  )
}

export default App
