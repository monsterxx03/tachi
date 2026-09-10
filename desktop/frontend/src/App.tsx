import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import { Dialogs, Events } from '@wailsio/runtime'
import {
  AgentService,
  AgentStatus,
  type AgentState,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import {
  STATUS_META,
  THINKING_LEVELS,
  PAGE_SIZE,
  AT_MAX_RESULTS,
  AT_SEARCH_DEBOUNCE_MS,
  type AgentEvent,
  type AtMatch,
  type AtPickerState,
  type Message,
  type Part,
  type SessionItem,
} from './types'
import { buildTurns, fmtDur, fmtTime, toLocalAsset, tpsTier, actOnKey, atRefAt, countAtRefs, insertRefText, replaceRefText } from './lib'
import {
  ContextRing, CacheRing, ThinkingPart, MessageBubble, ToolCard, MCPPanel, AtFilePicker, AskForm,
  FileCard, fileFromSendFileArgs, PreBlock,
  SettingsIcon, UsageIcon, MCPIcon,
} from './components'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'

// TableScroller wraps GFM tables in a horizontally scrollable container so a
// table wider than the message card scrolls inside it instead of bursting out
// of the layout. Module-level: stable identity across streaming re-renders.
function TableScroller(props: { children?: ReactNode }) {
  return (
    <div className="table-scroll">
      <table>{props.children}</table>
    </div>
  )
}

// ── Ordered turn parts ──────────────────────────────────────────────────────
// A live turn accumulates the SAME ordered `parts` array a rebuilt transcript
// has (see buildTurns): deltas extend the trailing part of their own kind, a
// tool start appends a part, its result closes that part. Without this the
// live view sorted everything into fixed buckets (all tools, then all text),
// so a multi-step turn rendered in the wrong order until a restart rebuilt it.
function appendPartDelta(m: Message, type: 'thinking' | 'text', delta: string): Message {
  if (!delta) return m
  const parts = [...(m.parts || [])]
  const last = parts[parts.length - 1]
  if (last && last.type === type) parts[parts.length - 1] = { ...last, text: (last.text || '') + delta }
  else parts.push({ type, text: delta })
  return { ...m, parts }
}

function pushPart(m: Message, p: Part): Message {
  return { ...m, parts: [...(m.parts || []), p] }
}

// finishToolPart closes the newest still-running call of the named tool.
function finishToolPart(m: Message, name: string, summary: string, ok: boolean, durationMs?: number): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i]
    if (p.type === 'tool' && !p.done && p.name === name) {
      parts[i] = { ...p, summary, ok, done: true, durationMs }
      break
    }
  }
  return { ...m, parts }
}

// updateToolPart fills in the human-readable title/args for the newest
// in-flight call (pushed by the separate agent:tool event).
function updateToolPart(m: Message, name: string, title: string, args: string): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i]
    if (p.type === 'tool' && !p.done && p.name === name) {
      parts[i] = { ...p, title, args }
      break
    }
  }
  return { ...m, parts }
}

// closeOpenToolParts marks every unfinished call as aborted — used when a turn
// is stopped/errors and no tool_result will ever arrive.
function closeOpenToolParts(parts: Part[] | undefined, summary: string): Part[] {
  return (parts || []).map((p) => (p.type === 'tool' && !p.done ? { ...p, ok: false, done: true, summary } : p))
}

function lastRunningAssistantIndex(list: Message[]): number {
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].role === 'assistant' && list[i].running) return i
  }
  return -1
}

// imeActive reports whether a key event belongs to an IME composition (Chinese
// / Japanese / Korean candidate selection). Enter while composing confirms a
// candidate — treating it as "submit" is the classic IME bug. keyCode 229 is
// the fallback some WebKit builds report instead of setting isComposing.
function imeActive(e: { nativeEvent?: { isComposing?: boolean }; keyCode?: number }): boolean {
  return !!e.nativeEvent?.isComposing || e.keyCode === 229
}

// ── Memoized transcript pieces ──────────────────────────────────────────────
// Streaming replaces only the message being written (its siblings keep the
// same object references), so memoization here means every earlier turn — and
// its already-parsed markdown — is skipped on each animation frame. Without
// this, a long session re-parsed the whole transcript dozens of times per
// second, which is what made output feel jumpy.

const MarkdownBlock = memo(function MarkdownBlock({ text, workDir }: { text: string; workDir: string }) {
  const MdImg = useCallback(({ src, alt }: { src?: string; alt?: string }) => (
    <img src={toLocalAsset(src, workDir)} alt={alt || ''} />
  ), [workDir])
  return (
    <div className="assistant-text">
      {/* rehypeHighlight tokenises fenced code; PreBlock turns ```mermaid into a
          diagram and leaves everything else as a plain <pre>. */}
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeHighlight]}
        components={{ img: MdImg, table: TableScroller, pre: PreBlock }}>
        {text}
      </ReactMarkdown>
    </div>
  )
})

const TurnPart = memo(function TurnPart({ part, workDir }: { part: Part; workDir: string }) {
  if (part.type === 'thinking') return <ThinkingPart text={part.text || ''} />
  if (part.type === 'tool') {
    // A SendFile call IS the attachment — show the file card rather than a raw
    // tool card (covers the live turn and reloaded history alike).
    if (part.name === 'SendFile') {
      const file = fileFromSendFileArgs(part.args || '')
      if (file) return <FileCard file={file} />
    }
    return <ToolCard name={part.name || ''} title={part.title} args={part.args} summary={part.summary || ''} ok={!!part.ok} durationMs={part.durationMs} />
  }
  return <MarkdownBlock text={part.text || ''} workDir={workDir} />
})

const AssistantBubble = memo(function AssistantBubble({ m, workDir, runningLabel, ask, onAnswer }: {
  m: Message
  workDir: string
  // Only passed while the turn is running, so finished messages keep a stable
  // props shape and stay memoized.
  runningLabel?: string
  // Pending AskUserQuestion questions for THIS session: the form replaces the
  // tool card that asked them, so they appear in the transcript where they belong.
  ask?: Question[] | null
  onAnswer?: (answers: Record<string, string> | null) => void
}) {
  // Render the form in place of the pending AskUserQuestion card. Any other
  // unfinished card of the same tool is left alone; the loop only ever asks one
  // question set at a time, so the first match is the one waiting.
  let askShown = false
  const parts = (m.parts || []).map((p, i) => {
    if (ask && onAnswer && !askShown && p.type === 'tool' && !p.done && p.name === 'AskUserQuestion') {
      askShown = true
      return <AskForm key={i} questions={ask} onSubmit={onAnswer} onCancel={() => onAnswer(null)} />
    }
    return <TurnPart key={i} part={p} workDir={workDir} />
  })
  return (
    <div className="msg msg-assistant">
      <div className="msg-avatar"><img src="/agent-avatar.png" alt="" draggable={false} /></div>
      <div className="msg-content">
        <div className="turn-parts">{parts}</div>
        {/* Fallback: a pending ask with no matching tool card (e.g. the card was
            closed by an interruption) still has to be answerable. */}
        {ask && onAnswer && !askShown && m.running ? (
          <AskForm questions={ask} onSubmit={onAnswer} onCancel={() => onAnswer(null)} />
        ) : null}
        {m.running ? <span className="running"><span className="typing"><i></i><i></i><i></i></span>{runningLabel ?? '正在执行…'}</span> : null}
        {!m.running && m.stopped ? <span className="stopped-note"><span className="stop-square">⏹</span> 已停止</span> : null}
        {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
        {m.summary ? (
          <div className="msg-footer">
            {m.summary.durationMs > 0 ? <span>⏱ {fmtDur(m.summary.durationMs)}</span> : null}
            {m.summary.iterations > 0 ? <span>{m.summary.iterations} iters</span> : null}
            {m.summary.cost > 0 ? <span>¥{m.summary.cost.toFixed(3)}</span> : null}
            {m.summary.credit > 0 ? <span>{m.summary.credit} 积分</span> : null}
          </div>
        ) : null}
      </div>
    </div>
  )
})

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
  const [hasCacheHit, setHasCacheHit] = useState(false)
  const [workDir, setWorkDir] = useState('')
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
  // Pending queue: messages the user sends while a turn is still running. They
  // are NOT executed immediately — they sit here (each removable, the whole
  // queue clearable) until the next steer point injects them into the running
  // turn, or the turn ends and a natural completion auto-sends them. Keyed by
  // session so queued input follows its own conversation.
  const [pending, setPending] = useState<Record<string, string[]>>({})
  const pendingRef = useRef<Record<string, string[]>>({})
  // sendingNow guards the "立即发送" action against double-clicks while the
  // backend is still stopping the previous turn.
  const [sendingNow, setSendingNow] = useState(false)
  // Auto-follow state: true while the view is parked at the bottom. Scrolling
  // up flips it off (and surfaces the "jump to latest" button) so incoming
  // streaming output no longer yanks the transcript back down under the user.
  const followBottomRef = useRef(true)
  const [showJump, setShowJump] = useState(false)
  // IME composition state for the composer. Some WebKit builds fire
  // compositionend BEFORE the Enter keydown that commits the candidate, so the
  // ref is cleared on the next macrotask — that keeps the guard active for the
  // committing Enter without swallowing a later, genuine Enter-to-send.
  const composingRef = useRef(false)
  const chatRef = useRef<HTMLDivElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)
  const messages = msgCache[currentId] || []
  const pendMsgs = pending[currentId] || []

  // ── @-file completion ─────────────────────────────────────────────────────
  // Typing "@" opens a fuzzy picker over the session's working directory.
  // Accepting a match splices a reference into the text; the reference is
  // expanded backend-side at turn start (agent/atfile), so the transcript keeps
  // showing the raw text the user typed. The trigger rule in atRefAt mirrors
  // agent/atfile.IsRefBoundary — the popup must offer exactly what the backend
  // will expand.
  const [at, setAt] = useState<AtPickerState | null>(null)
  // Query of the last scheduled search — null (not "") means "none yet": the
  // empty query is a real query (it lists the working directory), so "" cannot
  // double as the sentinel or the very first "@" would never be searched.
  const atQueryRef = useRef<string | null>(null)
  const atSeqRef = useRef(0)                     // stale-response guard
  const atTimerRef = useRef<number | null>(null)
  // inputRef mirrors the composer text for event listeners (Wails file drops)
  // that must not re-subscribe on every keystroke.
  const inputRef = useRef(input)
  useEffect(() => { inputRef.current = input }, [input])
  // atStateRef mirrors the picker state for the same reason (see the file-drop
  // listener: a drop with the picker open must replace the reference being
  // typed, not append after it).
  const atStateRef = useRef<AtPickerState | null>(null)
  useEffect(() => { atStateRef.current = at }, [at])

  const closeAt = useCallback(() => {
    atQueryRef.current = null
    atSeqRef.current++
    if (atTimerRef.current !== null) {
      window.clearTimeout(atTimerRef.current)
      atTimerRef.current = null
    }
    setAt(null)
  }, [])

  // scheduleAtSearch runs the (debounced) backend search for a query.
  const scheduleAtSearch = useCallback((query: string) => {
    const sid = currentId
    atQueryRef.current = query
    if (atTimerRef.current !== null) window.clearTimeout(atTimerRef.current)
    const seq = ++atSeqRef.current
    atTimerRef.current = window.setTimeout(() => {
      atTimerRef.current = null
      // Clear the spinner on every path — a rejected (or even synchronously
      // throwing) call must not leave the picker stuck on "搜索中…".
      const applyResult = (items: AtMatch[]) => {
        if (seq !== atSeqRef.current) return // superseded by a newer query
        setAt((p) => (p && p.query === query ? { ...p, items, idx: 0, loading: false } : p))
      }
      try {
        AgentService.SearchFiles(sid, query, AT_MAX_RESULTS)
          .then((res) => applyResult(res || []))
          .catch(() => applyResult([]))
      } catch {
        applyResult([])
      }
    }, AT_SEARCH_DEBOUNCE_MS)
  }, [currentId])

  // syncAtRef recomputes the picker from the composer's value + caret. Called on
  // every text/selection change, which is also how it closes: no reference under
  // the caret means no picker.
  const syncAtRef = useCallback((value: string, caret: number) => {
    const ref = atRefAt(value, caret)
    if (!ref) {
      closeAt()
      return
    }
    const count = countAtRefs(value)
    setAt((prev) => (prev && prev.start === ref.start && prev.query === ref.query
      ? { ...prev, count }
      : { start: ref.start, query: ref.query, items: [], idx: 0, loading: true, count }))
    if (atQueryRef.current !== ref.query) scheduleAtSearch(ref.query)
  }, [closeAt, scheduleAtSearch])

  // insertAtCaret splices text into the composer at the caret, keeping the focus
  // and the caret after the inserted text.
  const insertAtCaret = useCallback((text: string, trailingSpace: boolean, resync: boolean) => {
    const caret = composerRef.current?.selectionStart ?? inputRef.current.length
    const ins = insertRefText(inputRef.current, caret, text, trailingSpace)
    setInput(ins.value)
    requestAnimationFrame(() => {
      const node = composerRef.current
      if (!node) return
      node.focus()
      node.setSelectionRange(ins.caret, ins.caret)
      if (resync) syncAtRef(ins.value, ins.caret)
    })
  }, [syncAtRef])

  // acceptAt REPLACES the reference being typed with the picked path — it must
  // not splice at the caret, or the "@query" the user was typing survives and
  // the text ends up with a stray "@" (counted as a second reference). A
  // directory keeps the picker open on the new prefix so the user can drill in.
  const acceptAt = useCallback((match?: AtMatch) => {
    if (!match || !at) return
    const dir = !!match.isDir
    const text = '@' + match.path + (dir ? '/' : ' ')
    const ins = replaceRefText(inputRef.current, at.start, text)
    atQueryRef.current = null // whatever follows is a fresh query
    setAt(null)
    setInput(ins.value)
    requestAnimationFrame(() => {
      const node = composerRef.current
      if (!node) return
      node.focus()
      node.setSelectionRange(ins.caret, ins.caret)
      if (dir) syncAtRef(ins.value, ins.caret)
    })
  }, [at, syncAtRef])

  // A session switch changes the working directory, so any open picker is stale.
  useEffect(() => { closeAt() }, [currentId, closeAt])

  // ── AskUserQuestion ───────────────────────────────────────────────────────
  // The agent parks the turn and waits for answers; the questions arrive as an
  // event and are answered through AgentService.AnswerQuestion (the TUI answers
  // the same channel via RespondToAskUser). Pending questions are kept per
  // session so a background session's question never hijacks the foreground UI.
  const [asks, setAsks] = useState<Record<string, { toolId: string; questions: Question[] }>>({})
  const clearAsk = useCallback((sid: string) => {
    setAsks((prev) => {
      if (!prev[sid]) return prev
      const next = { ...prev }
      delete next[sid]
      return next
    })
  }, [])

  // answerQuestion answers (or declines, with null) the pending AskUserQuestion.
  const answerQuestion = useCallback((sid: string, answers: Record<string, string> | null) => {
    clearAsk(sid)
    // nil answers = the user declined; the model is told the question went
    // unanswered instead of being handed a made-up choice.
    AgentService.AnswerQuestion(sid, answers, null).catch(() => {})
  }, [clearAsk])

  // answerCurrent is the stable callback the transcript form uses for the
  // session on screen.
  const answerCurrent = useCallback((a: Record<string, string> | null) => answerQuestion(currentId, a), [currentId, answerQuestion])

  // Note: pending questions are NOT cleared on session switch — the agent is
  // still parked, so switching back must show the same form. They are cleared
  // when the turn ends (see the agent:idle / agent:error listeners below).

  // Native file drops arrive from Go (the webview cannot read dropped paths):
  // resolve them into @-references and splice them in at the caret.
  useEffect(() => {
    const off = Events.On('agent:filedrop', (event) => {
      const d = event.data as { paths?: string[] } | undefined
      if (!d?.paths?.length) return
      const paths = d.paths
      ;(async () => {
        try {
          const files = (await AgentService.ResolveDroppedPaths(currentId, paths)) || []
          const refs = files.map((f) => f.ref).filter(Boolean) as string[]
          if (!refs.length) return
          const batch = refs.join(' ')
          const open = atStateRef.current
          closeAt()
          if (open) {
            // Drop while the picker is open: replace the "@query" being typed.
            const ins = replaceRefText(inputRef.current, open.start, batch + ' ')
            setInput(ins.value)
            requestAnimationFrame(() => {
              const node = composerRef.current
              node?.focus()
              node?.setSelectionRange(ins.caret, ins.caret)
            })
            return
          }
          insertAtCaret(batch, true, false)
        } catch { /* ignore */ }
      })()
    })
    return () => off?.()
  }, [currentId, closeAt, insertAtCaret])

  const scrollToBottom = useCallback((force = false) => {
    // force=true is for actions where following is clearly intended (sending a
    // message, switching sessions). Otherwise the pin only happens while the
    // user is parked at the bottom.
    if (force) {
      followBottomRef.current = true
      setShowJump(false)
    }
    const el = chatRef.current
    if (el && followBottomRef.current) el.scrollTop = el.scrollHeight
  }, [])

  // Vim-like keys while the message area has focus: G jumps to the newest
  // message, gg returns to the top, Ctrl+U / Ctrl+D scroll half a page. Keys
  // typed into the composer or a form field are never intercepted.
  const ggPendingRef = useRef(false)
  const onChatKey = useCallback((e: React.KeyboardEvent<HTMLDivElement>) => {
    const target = e.target as HTMLElement | null
    if (target?.closest('input, textarea, [contenteditable="true"]')) return
    const el = chatRef.current
    if (!el || e.metaKey || e.altKey) return

    if (e.ctrlKey && (e.key === 'u' || e.key === 'd')) {
      e.preventDefault()
      el.scrollTop += (e.key === 'd' ? 1 : -1) * el.clientHeight * 0.5
      return
    }
    if (e.ctrlKey) return
    if (e.key === 'G') { e.preventDefault(); scrollToBottom(true); return } // newest message + re-arm following
    if (e.key === 'g') {
      e.preventDefault()
      if (ggPendingRef.current) {
        ggPendingRef.current = false
        el.scrollTop = 0
      } else {
        ggPendingRef.current = true
        window.setTimeout(() => { ggPendingRef.current = false }, 600)
      }
    }
  }, [scrollToBottom])

  // ── Frame-batched deltas ───────────────────────────────────────────────────
  // Token deltas arrive far faster than the display refreshes. Batching them
  // into a single state update per animation frame caps markdown parsing and
  // layout at ~60/s instead of once per token. Non-delta events flush the
  // buffer first, so the transcript order stays exactly as it happened.
  const deltaQueueRef = useRef<{ sid: string; type: 'thinking' | 'text'; delta: string }[]>([])
  const deltaRafRef = useRef<number | null>(null)
  const flushDeltas = useCallback(() => {
    if (deltaRafRef.current !== null) {
      cancelAnimationFrame(deltaRafRef.current)
      deltaRafRef.current = null
    }
    const q = deltaQueueRef.current
    if (!q.length) return
    deltaQueueRef.current = []
    setMsgCache((prev) => {
      // Group per session (order preserved within a session) so background
      // sessions batched in the same frame don't clobber each other.
      const groups = new Map<string, { type: 'thinking' | 'text'; delta: string }[]>()
      for (const it of q) {
        const g = groups.get(it.sid)
        if (g) g.push(it)
        else groups.set(it.sid, [it])
      }
      let out = prev
      for (const [sid, items] of groups) {
        const list = prev[sid] || []
        const idx = lastRunningAssistantIndex(list)
        if (idx < 0) continue
        let cur = list[idx]
        for (const it of items) cur = appendPartDelta(cur, it.type, it.delta)
        if (cur === list[idx]) continue
        const next = [...list]
        next[idx] = cur
        if (out === prev) out = { ...prev }
        out[sid] = next
      }
      return out
    })
  }, [])
  const enqueueDelta = useCallback((sid: string, type: 'thinking' | 'text', delta: string) => {
    if (!delta) return
    deltaQueueRef.current.push({ sid, type, delta })
    if (deltaRafRef.current === null) deltaRafRef.current = requestAnimationFrame(flushDeltas)
  }, [flushDeltas])

  // Pin to the newest content after every committed update while following —
  // doing it in a layout effect means the scroll happens in the same frame the
  // content grows, so the view glides instead of lurching.
  useLayoutEffect(() => {
    const el = chatRef.current
    if (el && followBottomRef.current) el.scrollTop = el.scrollHeight
  }, [msgCache])

  // On session switch, jump straight to the newest message BEFORE paint (via
  // useLayoutEffect) so the view never briefly shows the oldest messages and
  // then snaps down. The async-load path still uses scrollToBottom() after the
  // page arrives.
  useLayoutEffect(() => {
    const el = chatRef.current
    if (el) el.scrollTop = el.scrollHeight
    // A session switch always lands at the newest message — re-arm following.
    followBottomRef.current = true
    setShowJump(false)
  }, [currentId])

  const loadMoreRef = useRef(false)
  const handleScroll = () => {
    const el = chatRef.current
    if (!el) return
    // Track bottom-proximity first: scrolling up must pause auto-follow even
    // while a page of older messages is being fetched (the early return below
    // would otherwise swallow it).
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 48
    if (nearBottom !== followBottomRef.current) {
      followBottomRef.current = nearBottom
      setShowJump(!nearBottom)
    }
    if (loadMoreRef.current || loading) return
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

  // ── Pending queue / steer ─────────────────────────────────────────────────
  // Queue ops keep pendingRef in sync so event listeners (which can only see
  // the latest values through refs, not stale render closures) always act on
  // the freshest queue.
  const enqueuePending = useCallback((sid: string, text: string) => {
    const next = { ...pendingRef.current, [sid]: [...(pendingRef.current[sid] || []), text] }
    pendingRef.current = next
    setPending(next)
  }, [])
  const dropPending = useCallback((sid: string, idx: number) => {
    const list = pendingRef.current[sid] || []
    const next = { ...pendingRef.current, [sid]: list.filter((_, i) => i !== idx) }
    pendingRef.current = next
    setPending(next)
  }, [])
  const clearPending = useCallback((sid: string) => {
    if (!(pendingRef.current[sid] || []).length) return
    const next = { ...pendingRef.current, [sid]: [] }
    pendingRef.current = next
    setPending(next)
  }, [])
  // takePending joins and clears the queue, returning the combined text ("" if
  // empty) — the single drain point for steer injection / send-now / auto-flush.
  const takePending = useCallback((sid: string) => {
    const text = (pendingRef.current[sid] || []).join('\n\n')
    if (text) {
      const next = { ...pendingRef.current, [sid]: [] }
      pendingRef.current = next
      setPending(next)
    }
    return text
  }, [])

  // The current session is producing output when its run is in-flight, or the
  // status bar is in a busy state (covers the simulated-turn fallback too).
  const isCurrentRunning = runningSet.has(currentId) ||
    state.status === 'thinking' || state.status === 'tool_running' || state.status === 'busy'

  // sendText appends the user message + a running assistant placeholder and
  // starts a backend turn. Shared by the composer, the queue's "send now" and
  // the auto-flush after a naturally completed turn.
  const sendText = useCallback((raw: string) => {
    const text = raw.trim()
    if (!text) return
    const sid = currentId
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    setSessionMsgs(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: tsStr },
      { id: `a-${ts}`, role: 'assistant', running: true, parts: [], ts: tsStr },
    ])
    AgentService.SendMessage(text).catch(() => {})
    setRunningSet((prev) => new Set(prev).add(sid))
    scrollToBottom(true)
  }, [currentId, setSessionMsgs, scrollToBottom])

  // send routes the composer text: while a turn is running the message goes to
  // the pending queue (to be steered in at the next tool boundary) instead of
  // starting a second, competing turn.
  const send = useCallback(() => {
    const text = input.trim()
    if (!text) return
    closeAt()
    setInput('')
    if (isCurrentRunning) {
      enqueuePending(currentId, text)
      return
    }
    sendText(text)
  }, [input, isCurrentRunning, currentId, enqueuePending, sendText, closeAt])

  // sendPendingNow is the pending bar's primary action: stop the current turn
  // and send the queued text as a fresh user turn right away. The user bubble
  // + assistant placeholder are shown immediately; if stopping the turn times
  // out on the backend the text is put back into the queue for another try.
  const sendPendingNow = useCallback(async () => {
    const sid = currentId
    const text = takePending(sid)
    if (!text.trim()) return
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    setSessionMsgs(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: tsStr },
      { id: `a-${ts}`, role: 'assistant', running: true, parts: [], ts: tsStr },
    ])
    setRunningSet((prev) => new Set(prev).add(sid))
    setSendingNow(true)
    scrollToBottom(true)
    try {
      const ret = isCurrentRunning
        ? await AgentService.StopAndSend(text)
        : await AgentService.SendMessage(text)
      if (ret && ret !== 'ok') {
        enqueuePending(sid, text)
      }
    } catch { /* ignore */ }
    setSendingNow(false)
  }, [currentId, isCurrentRunning, takePending, setSessionMsgs, enqueuePending, scrollToBottom])

  // injectSteerVisual splits the streaming assistant segment so the queued
  // user text lands AFTER the tool work already shown and BEFORE the reply
  // that continues after the steer point: finalize the running segment, append
  // the user bubble, open a fresh running placeholder for the rest of the turn
  // (subsequent deltas/tool events target the newest running assistant). When
  // the segment has rendered nothing yet the bubble is inserted before it.
  const injectSteerVisual = useCallback((sid: string, text: string, ts: string) => {
    setMsgCache((prev) => {
      const list = prev[sid] || []
      let ai = -1
      for (let i = list.length - 1; i >= 0; i--) {
        if (list[i].role === 'assistant' && list[i].running) { ai = i; break }
      }
      if (ai < 0) return prev
      const seg = list[ai]
      const u: Message = { id: `u-${Date.now()}-steer`, role: 'user', text, ts }
      if (!(seg.parts && seg.parts.length)) {
        const out = [...list]
        out.splice(ai, 0, u)
        return { ...prev, [sid]: out }
      }
      const out = [...list]
      out[ai] = { ...seg, running: false }
      out.push(u, { id: `a-${Date.now()}-steer`, role: 'assistant', running: true, parts: [], ts })
      return { ...prev, [sid]: out }
    })
    // Respect the follow state here too: a steered message landing mid-history
    // must not yank a user who is reading older output back to the bottom.
    scrollToBottom()
  }, [scrollToBottom])

  // answerSteer replies to the agent's steer_check for a session: queued text
  // is drained, shown in the transcript and injected; otherwise the empty
  // string unblocks the agent loop without steering.
  const answerSteer = useCallback((sid: string) => {
    const text = takePending(sid)
    if (!text) {
      AgentService.Steer(sid, '').catch(() => {})
      return
    }
    injectSteerVisual(sid, text, new Date().toISOString())
    AgentService.Steer(sid, text).catch(() => {})
  }, [takePending, injectSteerVisual])

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
      if (u) { setCost(u.cost || 0); setCredit(u.credit || 0); setCacheHitRate(u.cacheHitRate || 0); setHasCacheHit(!!u.hasCacheHit) }
    } catch { /* ignore */ }
  }, [])

  const refreshWorkDir = useCallback(async (id: string) => {
    try {
      const w = await (AgentService as any).GetSessionWorkingDir?.(id)
      setWorkDir(w || '')
    } catch { /* ignore */ }
  }, [])

  // pickWorkDir opens a native folder picker (seeded at the session's current
  // working directory) and applies the chosen directory to the session.
  const pickWorkDir = useCallback(async (id: string) => {
    try {
      const picked: string | string[] = await Dialogs.OpenFile({
        CanChooseDirectories: true,
        CanChooseFiles: false,
        CanCreateDirectories: true,
        Title: '选择工作目录',
        Directory: workDir || undefined,
      })
      if (typeof picked !== 'string' || !picked) return
      await (AgentService as any).SetSessionWorkingDir?.(id, picked).catch(() => {})
      setWorkDir(picked)
    } catch { /* ignore */ }
  }, [workDir])

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
      scrollToBottom(true)
    } else {
      const ns = await AgentService.NewSession().catch(() => null)
      if (ns) {
        setCurrentId(ns.id); setCurrentTitle(ns.title || 'Tachi')
        setMsgCache((p) => ({ ...p, [ns.id]: [] }))
        setHasMore((p) => ({ ...p, [ns.id]: false }))
        setEarliestTs((p) => ({ ...p, [ns.id]: '' }))
        setCost(0); setCredit(0); setTps(0); setLastTps(0)
        refreshWorkDir(ns.id)
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
    scrollToBottom(true)
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
      refreshWorkDir(ns.id)
      const list = (await AgentService.ListSessions().catch(() => null)) || []
      setSessions(list.map((s) => ({ ...s, active: s.id === ns.id })))
      refreshProvider()
    }
  }, [refreshProvider, refreshWorkDir])

  useEffect(() => { loadAll(); refreshRunning(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [])

  // Keyboard shortcuts: Cmd+/ focuses the composer, Cmd+B toggles the sidebar,
  // Cmd+N starts a new session. (No native menu binds them, so the webview sees
  // the key events.)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { setShortcutsOpen(false); setConfirmDel(null); setMenu(null); setReminderModal(null); return }
      if (!e.metaKey) return
      if (e.key === '/' && !e.shiftKey) { e.preventDefault(); composerRef.current?.focus() }
      else if (e.key.toLowerCase() === 'b') { e.preventDefault(); setSidebarCollapsed((v) => !v) }
      else if (e.key.toLowerCase() === 'n') { e.preventDefault(); newChat() }
      else if (e.key === '?' || (e.shiftKey && e.code === 'Slash')) { e.preventDefault(); setShortcutsOpen((v) => !v) }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [newChat])

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
      const d = event.data as { sessionId: string; cost: number; credit: number; cacheHitRate?: number; hasCacheHit?: boolean }
      if (d.sessionId === currentId) { setCost(d.cost || 0); setCredit(d.credit || 0); if (d.cacheHitRate != null) setCacheHitRate(d.cacheHitRate); setHasCacheHit(!!d.hasCacheHit) }
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

  // Questions arrive while the turn is parked; the form renders in the
  // transcript (see AssistantBubble), so pull the view down to it.
  useEffect(() => {
    const off = Events.On('agent:ask', (event) => {
      const d = event.data as { sessionId?: string; toolId?: string; questions?: Question[] } | undefined
      if (!d?.sessionId || !d.questions?.length) return
      setAsks((prev) => ({ ...prev, [d.sessionId as string]: { toolId: d.toolId || '', questions: d.questions as Question[] } }))
      // The form renders in the transcript, so follow it into view — but only
      // while the user is already parked at the bottom: yanking someone who is
      // reading history down to a form would be exactly the kind of
      // interruption this UI should avoid.
      if (d.sessionId === currentId) scrollToBottom()
    })
    return () => off?.()
  }, [currentId, scrollToBottom])

  useEffect(() => {
    const off = Events.On('agent:error', (event) => {
      const d = event.data as { sessionId: string; error: string; interrupted?: boolean }
      clearAsk(d.sessionId) // the turn is over: nothing can still be waiting
      if (d.sessionId !== currentId) return
      const interrupted = !!d.interrupted
      setMsgCache((prev) => {
        const list = prev[currentId] || []
        // The turn being concluded is the newest assistant message. Unlike a
        // running-scan, this still works when agent:event has already flipped
        // running off (listener order is not guaranteed). Mutations are
        // idempotent so double-handling is harmless.
        const target = [...list].reverse().find((m) => m.role === 'assistant')
        if (!target) return prev
        const conclude = (m: Message): Message => {
          // After a stop no tool_result event arrives for in-flight calls, so
          // close their cards; a real error is surfaced as a trailing text
          // part (the transcript renders parts in order — there is no
          // separate error field to append to).
          const parts = interrupted
            ? closeOpenToolParts(m.parts, '已中断')
            : [...(m.parts || []), { type: 'text' as const, text: '⚠ ' + (d.error || '出错') }]
          return { ...m, running: false, stopped: interrupted || m.stopped, parts }
        }
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? conclude(m) : m)) }
      })
    })
    return () => off?.()
  }, [currentId, clearAsk])

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
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? updateToolPart(m, t.name, t.title, t.args) : m)) }
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
      // Deltas ride the frame batcher; every other event must be applied in
      // order, so flush pending text/thinking first.
      if (ev.Type !== 'thinking_delta' && ev.Type !== 'text_delta') flushDeltas()
      switch (ev.Type) {
        case 'thinking_delta': enqueueDelta(sessionId, 'thinking', ev.ThinkingDelta || ''); break
        case 'text_delta': enqueueDelta(sessionId, 'text', ev.TextDelta); break
        case 'tool_call_start': apply(sessionId, 'assistant', (m) => pushPart(m, { type: 'tool', name: ev.ToolName, title: '', args: '', summary: '执行中…', ok: true, done: false })); break
        case 'tool_result': apply(sessionId, 'assistant', (m) => finishToolPart(m, ev.ToolName, ev.ToolResult, !ev.ToolIsError, ev.ToolDuration ? Math.round(ev.ToolDuration / 1e6) : undefined)); break
        case 'turn_complete': apply(sessionId, 'assistant', (m) => ({ ...m, running: false })); break
        case 'steer_check':
          // The agent finished a round of tool calls and is parked, waiting
          // for pending user input to steer the next LLM call. Queued text is
          // drained, shown in the transcript and injected; an empty queue
          // replies "" to unblock the loop without steering.
          answerSteer(sessionId)
          break
        case 'error': {
          const interrupted = ev.Result?.ExitReason === 'interrupted' || ev.Result?.ExitReason === 'cancelled'
          apply(sessionId, 'assistant', (m) => ({ ...m, running: false, stopped: interrupted || m.stopped }))
          break
        }
      }
      // These two are backend round-trips over IPC and used to run for EVERY
      // delta — the single biggest cause of stutter while streaming. They only
      // carry new information at turn boundaries.
      if (ev.Type === 'turn_complete' || ev.Type === 'error') {
        refreshRunning()
        refreshProvider()
      }
    })
    return () => off?.()
  }, [currentId, refreshRunning, refreshProvider, answerSteer, flushDeltas, enqueueDelta])

  // agent:idle fires after a turn goroutine has fully exited (running already
  // reset on the backend). A NATURAL completion auto-sends whatever the user
  // queued while the turn ran (same drain semantics as TUI's TurnComplete);
  // stopped/errored turns leave the queue for the user to review, drop or
  // send manually.
  useEffect(() => {
    const off = Events.On('agent:idle', (event) => {
      const d = event.data as { sessionId: string; reason: string }
      clearAsk(d.sessionId) // the turn is over: nothing can still be waiting
      if (d.sessionId !== currentId || d.reason !== 'complete') return
      const queued = pendingRef.current[d.sessionId] || []
      if (queued.length === 0) return
      sendText(takePending(d.sessionId))
    })
    return () => off?.()
  }, [currentId, sendText, takePending, clearAsk])

  // stopChat aborts the running turn in the current session (backend cancels
  // the turn ctx — same mechanism as tui Ctrl+C / acp prompt cancel).
  const stopChat = useCallback(() => {
    AgentService.Stop().catch(() => {})
  }, [])

  const meta = STATUS_META[state.status as string] ?? STATUS_META.idle
  // The provider picker lists names only; the selected provider's model is
  // exposed as its tooltip instead.
  const currentProvider = providers.find((p) => (p.name ?? p.Name) === providerName)
  const currentProviderModel = currentProvider?.model ?? currentProvider?.Model ?? ''

  return (
    <div className="app">
      <header className="titlebar drag-region">
        <div className="brand no-drag">
          <span className="brand-mark"><svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><defs><linearGradient id="tg1" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stopColor="var(--accent-3)" /><stop offset="1" stopColor="var(--accent-strong)" /></linearGradient></defs><rect x="1" y="1" width="14" height="14" rx="4.5" fill="url(#tg1)" /><path d="M8 3.8 L12.2 8 L8 12.2 L3.8 8 Z" fill="var(--on-accent)" opacity="0.95" /></svg></span>
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
              /* Row (not a <button>): it hosts a rename <input> and a context
                 menu, so it takes role/tabIndex + Enter/Space instead. */
              <div key={s.id} className={`session ${s.active ? 'active' : ''}`} onClick={() => clickSession(s.id)}
                role="button" tabIndex={0} onKeyDown={actOnKey(() => clickSession(s.id))}
                onContextMenu={(e) => { e.preventDefault(); setMenu({ sid: s.id, x: e.clientX, y: e.clientY }) }}>
                {editingId === s.id ? (
                  <input className="session-rename" autoFocus value={editTitle}
                    onChange={(e) => setEditTitle(e.target.value)}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                      // Enter/Escape during IME composition belong to the
                      // candidate window, not to rename/commit.
                      if (imeActive(e)) return
                      if (e.key === 'Enter') { e.stopPropagation(); commitRename(s.id) }
                      else if (e.key === 'Escape') { e.stopPropagation(); setEditingId(''); setEditTitle('') }
                    }} />
                ) : (
                  <div className="session-title" onDoubleClick={() => { setEditingId(s.id); setEditTitle(s.title || ''); setMenu(null) }}>{s.title || '未命名会话'}</div>
                )}
                <div className="session-meta">{runningSet.has(s.id) ? <span className="spin-dot" title="运行中" /> : null}{new Date(s.updatedAt).toLocaleString('zh-CN', { hour12: false })}</div>
              </div>
            ))}
            {sessions.length === 0 && <div className="session-empty">暂无会话</div>}
          </nav>
          <footer className="sidebar-footer">
            {/* Both entries are placeholders: disabled (and dimmed) rather than
                clickable-looking no-ops. */}
            <button className="footer-item" disabled title="暂未实现"><span className="footer-ico"><SettingsIcon /></span> 设置</button>
            <button className="footer-item" disabled title="暂未实现"><span className="footer-ico"><UsageIcon /></span> 用量</button>
          </footer>
        </aside>

        <main className="main">
          <div className="chat-wrap">
            <div className="chat" ref={chatRef} onScroll={handleScroll} tabIndex={0} onKeyDown={onChatKey}>
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
                    <AssistantBubble
                      key={m.id}
                      m={m}
                      workDir={workDir}
                      runningLabel={m.running ? (state.status === 'thinking' ? '正在思考…' : '正在执行…') : undefined}
                      ask={m.running ? (asks[currentId]?.questions || null) : null}
                      onAnswer={answerCurrent}
                    />
                  ),
                )}
              </>
            )}
            </div>
            {showJump && (
              <button className="jump-latest" onClick={() => scrollToBottom(true)} title="回到最新消息">
                <span className="jump-ico">↓</span>回到最新
              </button>
            )}
          </div>

          <footer className="composer">
            {pendMsgs.length > 0 && (
              <div className="pending-bar">
                <div className="pending-head">
                  <span className="pending-title">
                    {isCurrentRunning ? '⏸ 待发送 · 会在本轮工具调用结束后自动插入' : '待发送消息'}
                  </span>
                  <span className="pending-actions">
                    <button className="pending-send" disabled={sendingNow} onClick={sendPendingNow}>
                      {sendingNow ? '正在停止…' : isCurrentRunning ? '立即发送 · 打断' : '发送'}
                    </button>
                    <button className="pending-clear" onClick={() => clearPending(currentId)} title="清空待发送队列">清空</button>
                  </span>
                </div>
                {pendMsgs.map((t, i) => (
                  <div className="pending-item" key={`${i}-${t.slice(0, 8)}`}>
                    <span className="pending-bullet">·</span>
                    <span className="pending-text" title={t}>{t}</span>
                    <button className="pending-del" title="撤回该条" onClick={() => dropPending(currentId, i)}>✕</button>
                  </div>
                ))}
              </div>
            )}
            <div className="composer-box" data-file-drop-target="true">
              {at && (
                <AtFilePicker
                  query={at.query}
                  items={at.items}
                  selected={at.idx}
                  loading={at.loading}
                  refCount={at.count}
                  onPick={(i) => acceptAt(at.items[i])}
                  onHover={(i) => setAt((p) => (p ? { ...p, idx: i } : p))}
                />
              )}
              <div className="composer-input-wrap">
                <textarea className="composer-input" ref={composerRef} value={input}
                  onChange={(e) => { setInput(e.target.value); syncAtRef(e.target.value, e.target.selectionStart ?? e.target.value.length) }}
                  onSelect={(e) => { const el = e.currentTarget; syncAtRef(el.value, el.selectionStart ?? el.value.length) }}
                  onBlur={() => closeAt()}
                  onCompositionStart={() => { composingRef.current = true }}
                  onCompositionEnd={() => { window.setTimeout(() => { composingRef.current = false }, 0) }}
                  onKeyDown={(e) => {
                    const ime = composingRef.current || imeActive(e)
                    // While the @-picker is open it owns the navigation keys;
                    // an IME composing (candidate selection) owns them first.
                    if (!ime && at) {
                      // ↑↓ and Ctrl+N/Ctrl+P both move the highlight (Ctrl+N/P
                      // matches the TUI's keymap, and preventDefault keeps
                      // Cocoa's own Ctrl+N/P caret movement out of the way).
                      const down = e.key === 'ArrowDown' || (e.ctrlKey && e.key.toLowerCase() === 'n')
                      const up = e.key === 'ArrowUp' || (e.ctrlKey && e.key.toLowerCase() === 'p')
                      if (down || up) {
                        e.preventDefault()
                        const delta = down ? 1 : -1
                        setAt((p) => (p && p.items.length
                          ? { ...p, idx: Math.max(0, Math.min(p.items.length - 1, p.idx + delta)) }
                          : p))
                        return
                      }
                      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
                        // Tab/Enter accept the highlighted match; with no match,
                        // Enter still sends (Tab just dismisses the picker).
                        if (at.items.length > 0) { e.preventDefault(); acceptAt(at.items[at.idx]); return }
                        if (e.key === 'Tab') { e.preventDefault(); closeAt(); return }
                      }
                      if (e.key === 'Escape') { e.preventDefault(); closeAt(); return }
                    }
                    // Esc hands the focus to the message area (the Vim-like keys
                    // live there). Not listed in the shortcut sheet on purpose:
                    // it is a focus affordance, not a feature shortcut.
                    if (e.key === 'Escape') { e.preventDefault(); chatRef.current?.focus(); return }
                    if (e.key !== 'Enter' || e.shiftKey) return
                    // Enter while an IME is composing confirms the candidate
                    // (选词), it must not send the message.
                    if (ime) return
                    e.preventDefault()
                    send()
                  }}
                  placeholder={isCurrentRunning ? '正在运行…输入后 Enter 将加入待发送，在工具间隙自动插入' : '发送消息给 Tachi…（Enter 发送，Shift+Enter 换行，@ 引用文件）'} />
                {isCurrentRunning && (
                  <button className="stop-btn" title="停止生成" onClick={stopChat} aria-label="停止生成">
                    <svg viewBox="0 0 24 24" width="10" height="10" fill="currentColor"><rect x="7" y="7" width="10" height="10" rx="1.5" /></svg>
                  </button>
                )}
              </div>
              <div className="composer-actions">
                <button className="send-btn" onClick={send} disabled={!input.trim() || sendingNow}>
                  <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
                  <span>{isCurrentRunning ? '排队' : '发送'}</span>
                </button>
              </div>
            </div>
            {/* Anchored to the composer (position: relative), so the panel always
                floats just above the input instead of covering it. */}
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
            <div className="composer-status">
              <div className="work-dir-wrap">
                <span className="work-dir" title="工作目录（点击选择）" onClick={() => pickWorkDir(currentId)}>
                  <span className="work-dir-ico">⌂</span>{workDir || '未设置工作目录'}
                </span>
              </div>
              {tps > 0 ? <span className={`usage-tps tps-${tpsTier(tps)}`} title="当前输出速率">{tps}/s</span>
                : lastTps > 0 ? <span className="usage-tps tps-paused" title="最近输出速率">{lastTps}/s</span>
                : null}
              <div className="provider-picker">
                <select className="provider-select" value={providerName}
                  title={currentProviderModel ? `模型：${currentProviderModel}` : undefined}
                  onChange={async (e) => {
                    const name = e.target.value
                    await (AgentService as any).SwitchProvider?.(name)
                    refreshProvider()
                  }}>
                  {/* Names only — the model string is long and often identical
                      to the provider name; the tooltip above keeps it reachable. */}
                  {providers.map((p) => <option key={p.name ?? p.Name} value={p.name ?? p.Name}>{(p.name ?? p.Name)}</option>)}
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
                  {hasCacheHit ? <CacheRing rate={cacheHitRate} /> : null}
                  {cost > 0 ? <span className="usage-cost" title="当前会话成本">¥{cost.toFixed(3)}</span> : null}
                  {credit > 0 ? <span className="usage-credit" title="当前会话积分">{credit.toFixed(2)} 积分</span> : null}
                </span>
                <button className="mcp-btn" title="MCP servers / tools" onClick={() => setMcpOpen((v) => !v)}>
                  <span className="mcp-ico"><MCPIcon /></span>
                  <span className="mcp-count">{mcpServers.filter((s) => s.connected).length || ''}</span>
                </button>
              </div>
            </div>
          </footer>
        </main>
      </div>
      {menu && (
        <div className="ctx-menu" role="menu" style={{ left: menu.x, top: menu.y }} onMouseLeave={() => setMenu(null)}>
          <button className="ctx-item" role="menuitem" onClick={() => { setEditingId(menu.sid); setEditTitle(sessions.find((x) => x.id === menu.sid)?.title || ''); setMenu(null) }}>重命名</button>
          <button className="ctx-item danger" role="menuitem" onClick={() => { const t = sessions.find((x) => x.id === menu.sid)?.title || ''; setConfirmDel({ sid: menu.sid, title: t }); setMenu(null) }}>删除</button>
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
            <div className="shortcut-section">全局</div>
            <div className="shortcut-row"><kbd>⌘ /</kbd><span>聚焦输入框</span></div>
            <div className="shortcut-row"><kbd>⌘ N</kbd><span>新建会话</span></div>
            <div className="shortcut-row"><kbd>⌘ B</kbd><span>折叠 / 展开侧栏</span></div>
            <div className="shortcut-row"><kbd>⌘ ?</kbd><span>显示本快捷键列表</span></div>
            <div className="shortcut-section">消息区（聚焦时）</div>
            <div className="shortcut-row"><kbd>G</kbd><span>跳到最新消息</span></div>
            <div className="shortcut-row"><kbd>gg</kbd><span>回到顶部</span></div>
            <div className="shortcut-row"><kbd>Ctrl U / D</kbd><span>上 / 下翻半页</span></div>
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
