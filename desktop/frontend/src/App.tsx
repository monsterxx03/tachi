import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { Events } from '@wailsio/runtime'
import { AgentService, AgentStatus, type AgentState } from '../bindings/github.com/monsterxx03/tachi/desktop'

interface SessionItem {
  id: string
  title: string
  meta: string
  active?: boolean
}

interface Message {
  id: string
  role: 'user' | 'assistant'
  text?: string
  tool?: { name: string; summary: string; ok: boolean }
  running?: boolean
}

// ── Demo data (visual preview only, no agent loop wired up) ─────────────────
const INITIAL_SESSIONS: SessionItem[] = [
  { id: 's1', title: '代码评审：Tachi 桌面端', meta: '刚刚', active: true },
  { id: 's2', title: '重构会话管理', meta: '今天 14:02' },
  { id: 's3', title: '调研 Wails v3 托盘', meta: '昨天' },
  { id: 's4', title: '调试 MCP profile 切换', meta: '周二' },
]

const INITIAL_MESSAGES: Message[] = [
  { id: 'm0', role: 'user', text: '帮我把 tachi 的桌面端跑起来，先看下菜单栏状态对不对。' },
  {
    id: 'm1',
    role: 'assistant',
    text: '好的，我先确认一下环境，再生成菜单栏托盘图标。',
  },
  {
    id: 'm2',
    role: 'assistant',
    tool: { name: 'Bash', summary: '生成 menuicon/tray-*.png · 5 个状态图标', ok: true },
  },
  {
    id: 'm3',
    role: 'assistant',
    text: '菜单栏图标已就绪。左侧会话列表、中间对话流、底部输入框和状态栏都是真实可交互的骨架。接入 agent 后，这些占位内容会被真实的事件流替换。',
  },
]

const STATUS_META: Record<string, { dot: string; desc: string }> = {
  idle: { dot: '●', desc: '空闲' },
  thinking: { dot: '◐', desc: '思考中' },
  tool_running: { dot: '◑', desc: '执行工具' },
  busy: { dot: '◒', desc: '处理中' },
  error: { dot: '▲', desc: '出错' },
}

function App() {
  const [sessions] = useState<SessionItem[]>(INITIAL_SESSIONS)
  const [messages, setMessages] = useState<Message[]>(INITIAL_MESSAGES)
  const [state, setState] = useState<AgentState>({
    status: AgentStatus.StatusIdle,
    label: '空闲',
    detail: '就绪',
  })
  const [input, setInput] = useState('')

  // Subscribe to the live agent:state event emitted from Go.
  useEffect(() => {
    const off = Events.On('agent:state', (event) => {
      const s = event.data as AgentState
      setState(s)
      // When a simulated turn completes, finalise the pending assistant bubble.
      if (s.status === AgentStatus.StatusIdle && s.detail === '已完成回答') {
        setMessages((prev) =>
          prev.map((m) =>
            m.running
              ? {
                  ...m,
                  running: false,
                  text: '（这里是模拟回复 —— 接入真实 agent 后，这里会展示模型的生成内容。）',
                }
              : m,
          ),
        )
      }
    })
    // Initial snapshot.
    AgentService.GetState().then((s) => setState(s)).catch(() => {})
    return () => off?.()
  }, [])

  const send = useCallback(() => {
    const text = input.trim()
    if (!text) return
    setInput('')
    setMessages((prev) => [
      ...prev,
      { id: `u-${Date.now()}`, role: 'user', text },
      { id: `a-${Date.now()}`, role: 'assistant', running: true },
    ])
    AgentService.SendMessage(text).catch(() => {})
  }, [input])

  const meta = STATUS_META[state.status as string] ?? STATUS_META.idle

  return (
    <div className="app">
      {/* ── Global title bar: native traffic-light buttons sit on the left ── */}
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
          <h2>代码评审：Tachi 桌面端</h2>
          <span className="session-meta">4 条消息 · Claude 4</span>
        </div>

        <div className="status-badge no-drag">
          <span className={`dot dot-${state.status}`}>{meta.dot}</span>
          <span className="status-label">{state.label}</span>
        </div>
      </header>

      {/* ── Body: sidebar + conversational main ── */}
      <div className="app-body">
        <aside className="sidebar">
          <button className="new-chat" onClick={() => setInput('')}>
            <span className="new-chat-plus">＋</span> 新建会话
          </button>

          <nav className="session-list">
            <div className="session-section">最近</div>
            {sessions.map((s) => (
              <div key={s.id} className={`session ${s.active ? 'active' : ''}`}>
                <div className="session-title">{s.title}</div>
                <div className="session-meta">{s.meta}</div>
              </div>
            ))}
          </nav>

          <footer className="sidebar-footer">
            <div className="footer-item">
              <span className="footer-ico">⚙</span> 设置
            </div>
            <div className="footer-item">
              <span className="footer-ico">¤</span> 用量
            </div>
          </footer>
        </aside>

        <main className="main">
          <div className="chat">
            {messages.map((m) =>
              m.role === 'user' ? (
                <MessageBubble key={m.id} role="user">
                  {m.text}
                </MessageBubble>
              ) : (
                <MessageBubble key={m.id} role="assistant">
                  {m.tool ? (
                    <ToolCard name={m.tool.name} summary={m.tool.summary} ok={m.tool.ok} />
                  ) : null}
                  {m.text ? <span className="assistant-text">{m.text}</span> : null}
                  {m.running ? (
                    <span className="running">
                      <span className="typing"><i></i><i></i><i></i></span>
                      {state.status === 'thinking' ? '正在思考…' : '正在执行…'}
                    </span>
                  ) : null}
                </MessageBubble>
              ),
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

function MessageBubble({ role, children }: { role: 'user' | 'assistant'; children: ReactNode }) {
  return (
    <div className={`msg msg-${role}`}>
      <div className="msg-avatar">{role === 'user' ? 'U' : '◆'}</div>
      <div className="msg-content">{children}</div>
    </div>
  )
}

function ToolCard({ name, summary, ok }: { name: string; summary: string; ok: boolean }) {
  return (
    <div className="tool-card">
      <div className="tool-head">
        <span className="tool-ico">⚙</span>
        <span className="tool-name">{name}</span>
        <span className={`tool-status ${ok ? 'ok' : 'err'}`}>{ok ? '✓' : '✗'}</span>
      </div>
      <div className="tool-summary">{summary}</div>
      <div className="tool-meta"><code>$ {name} …</code> <span>0.8s</span></div>
    </div>
  )
}

export default App
