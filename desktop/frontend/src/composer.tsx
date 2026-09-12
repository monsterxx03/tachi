// The composer: everything between the user's hands and a running turn — the text they are
// typing, the @-file picker and the "/" palette that assist it, the queue of messages typed
// while a turn is already running, and the answers to a question the agent is parked on.
//
// Those four are one unit because of one fact: what the user is *about to say* becomes a
// turn through a single decision — send it now, queue it for the next steer point, or
// dispatch it as a slash command (see `route`). Keeping that decision next to the queue it
// feeds is what stops the two from drifting apart; the queue is also the state that has to
// follow its own conversation (keyed by session, moved on auto-compaction).

import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'
import { Events } from '@wailsio/runtime'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import {
  AT_MAX_RESULTS,
  AT_SEARCH_DEBOUNCE_MS,
  type AtMatch,
  type AtPickerState,
  type Message,
} from './types'
import type { CommandVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { atRefAt, countAtRefs, insertRefText, replaceRefText } from './lib'
import { finishNotice } from './transcript'
import { AtFilePicker, CommandPicker } from './components'

// imeActive reports whether a key event belongs to an IME composition (Chinese / Japanese /
// Korean candidate selection). Enter while composing confirms a candidate — treating it as
// "submit" is the classic IME bug. keyCode 229 is the fallback some WebKit builds report
// instead of setting isComposing.
export function imeActive(e: { nativeEvent?: { isComposing?: boolean }; keyCode?: number }): boolean {
  return !!e.nativeEvent?.isComposing || e.keyCode === 229
}

export type ComposerDeps = {
  currentId: string
  // A turn is already running in the session on screen, so text belongs in the queue rather
  // than in a second, competing turn.
  running: boolean
  // The transcript store (useTranscript.ts): what a send writes into.
  transcript: {
    updateSession: (sid: string, fn: (list: Message[]) => Message[]) => void
    applyToSession: (sid: string, role: 'user' | 'assistant', fn: (m: Message) => Message, openIfMissing?: boolean) => void
    injectSteerVisual: (sid: string, text: string, ts: string) => void
    sealRunningSegment: (sid: string) => void
    markRunning: (sid: string, running: boolean) => void
    refreshRunning: () => void
  }
  scrollToBottom: (force?: boolean) => void
  // Esc in the composer hands focus to the message area (the Vim-like keys live there).
  chatRef: RefObject<HTMLDivElement | null>
}

export function useComposer(deps: ComposerDeps) {
  const { currentId, running, transcript, scrollToBottom, chatRef } = deps
  const { updateSession, applyToSession, injectSteerVisual, sealRunningSegment, markRunning, refreshRunning } = transcript

  const [input, setInput] = useState('')
  const [sendingNow, setSendingNow] = useState(false)
  // IME composition state. Some WebKit builds fire compositionend BEFORE the Enter keydown
  // that commits the candidate, so the ref is cleared on the next macrotask — that keeps the
  // guard active for the committing Enter without swallowing a later, genuine Enter-to-send.
  const composingRef = useRef(false)
  const composerRef = useRef<HTMLTextAreaElement>(null)

  // ── @-file completion ─────────────────────────────────────────────────────
  // Typing "@" opens a fuzzy picker over the session's working directory. Accepting a match
  // splices a reference into the text; the reference is expanded backend-side at turn start
  // (agent/atfile), so the transcript keeps showing the raw text the user typed. The trigger
  // rule in atRefAt mirrors agent/atfile.IsRefBoundary — the popup must offer exactly what
  // the backend will expand.
  const [at, setAt] = useState<AtPickerState | null>(null)
  // Slash commands the backend supports (fetched once: the set is static per build) and the
  // "/" palette's highlight. The palette is derived from the input below rather than stored,
  // so there is no way for it to go stale.
  const [cmdList, setCmdList] = useState<CommandVO[]>([])
  const [cmdDismissed, setCmdDismissed] = useState<string | null>(null)
  const [cmdIdx, setCmdIdx] = useState(0)
  // Query of the last scheduled search — null (not "") means "none yet": the empty query is a
  // real query (it lists the working directory), so "" cannot double as the sentinel or the
  // very first "@" would never be searched.
  const atQueryRef = useRef<string | null>(null)
  const atSeqRef = useRef(0) // stale-response guard
  const atTimerRef = useRef<number | null>(null)
  // inputRef mirrors the composer text for event listeners (Wails file drops) that must not
  // re-subscribe on every keystroke.
  const inputRef = useRef(input)
  useEffect(() => { inputRef.current = input }, [input])
  // atStateRef mirrors the picker state for the same reason (see the file-drop listener: a
  // drop with the picker open must replace the reference being typed, not append after it).
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
      // Clear the spinner on every path — a rejected (or even synchronously throwing) call
      // must not leave the picker stuck on "搜索中…".
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

  // syncAtRef recomputes the picker from the composer's value + caret. Called on every
  // text/selection change, which is also how it closes: no reference under the caret means
  // no picker.
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

  // insertAtCaret splices text into the composer at the caret, keeping the focus and the
  // caret after the inserted text.
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

  // acceptAt REPLACES the reference being typed with the picked path — it must not splice at
  // the caret, or the "@query" the user was typing survives and the text ends up with a stray
  // "@" (counted as a second reference). A directory keeps the picker open on the new prefix
  // so the user can drill in.
  const acceptAt = useCallback((match?: AtMatch) => {
    if (!match || !at) return
    const dir = !!match.isDir
    // The backend decides the reference form (relative under the primary root, absolute under
    // an additional one); Path is only what the row displays.
    const text = (match.ref || '@' + match.path) + (dir ? '/' : ' ')
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

  // ── AskUserQuestion ───────────────────────────────────────────────────────
  // The agent parks the turn and waits for answers; the questions arrive as an event and are
  // answered through AgentService.AnswerQuestion (the TUI answers the same channel via
  // RespondToAskUser). Pending questions are kept per session so a background session's
  // question never hijacks the foreground UI — and NOT cleared on session switch: the agent
  // is still parked, so switching back must show the same form.
  const [asks, setAsks] = useState<Record<string, { toolId: string; questions: Question[] }>>({})
  const clearAsk = useCallback((sid: string) => {
    setAsks((prev) => {
      if (!prev[sid]) return prev
      const next = { ...prev }
      delete next[sid]
      return next
    })
  }, [])
  const answer = useCallback((sid: string, answers: Record<string, string> | null) => {
    clearAsk(sid)
    // nil answers = the user declined; the model is told the question went unanswered instead
    // of being handed a made-up choice.
    AgentService.AnswerQuestion(sid, answers, null).catch(() => {})
  }, [clearAsk])
  // answerCurrent is the stable callback the transcript form uses for the session on screen.
  const answerCurrent = useCallback((a: Record<string, string> | null) => answer(currentId, a), [currentId, answer])

  // ── Pending queue / steer ─────────────────────────────────────────────────
  // Queue ops keep pendingRef in sync so event listeners (which can only see the latest
  // values through refs, not stale render closures) always act on the freshest queue.
  const [pending, setPending] = useState<Record<string, string[]>>({})
  const pendingRef = useRef<Record<string, string[]>>({})
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
  // takePending joins and clears the queue, returning the combined text ("" if empty) — the
  // single drain point for steer injection / send-now / auto-flush.
  const takePending = useCallback((sid: string) => {
    const text = (pendingRef.current[sid] || []).join('\n\n')
    if (text) {
      const next = { ...pendingRef.current, [sid]: [] }
      pendingRef.current = next
      setPending(next)
    }
    return text
  }, [])
  // rekeySessions follows auto-compaction: the queued messages are the conversation's, so
  // they move to the child session with it (the transcript does NOT — see useTranscript).
  const rekeySessions = useCallback((from: string, to: string) => {
    if (!from || !to || from === to) return
    const moveKey = <T,>(rec: Record<string, T>): Record<string, T> => {
      if (!(from in rec)) return rec
      const { [from]: kept, ...rest } = rec
      return { ...rest, [to]: kept }
    }
    setPending(moveKey)
    pendingRef.current = moveKey(pendingRef.current)
    setAsks(moveKey)
  }, [])

  // ── Sending ───────────────────────────────────────────────────────────────
  // sendText appends the user message + a running assistant placeholder and starts a backend
  // turn. Shared by the composer, the queue's "send now" and the auto-flush after a naturally
  // completed turn.
  const sendText = useCallback((raw: string) => {
    const text = raw.trim()
    if (!text) return
    const sid = currentId
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    updateSession(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: tsStr },
      { id: `a-${ts}`, role: 'assistant', running: true, parts: [], ts: tsStr },
    ])
    AgentService.SendMessage(text).catch(() => {})
    markRunning(sid, true)
    scrollToBottom(true)
  }, [currentId, updateSession, markRunning, scrollToBottom])

  // runCommand sends a slash command. It renders exactly like a message — user bubble plus a
  // running assistant placeholder, so the command's streamed output has somewhere to land —
  // but the backend dispatches it instead of starting a chat turn. A non-empty result means
  // the command never ran (unknown, or the session is busy): that is shown as a notice rather
  // than an empty reply.
  const runCommand = useCallback((text: string) => {
    const sid = currentId
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    updateSession(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: tsStr },
      { id: `a-${ts}`, role: 'assistant', running: true, parts: [], ts: tsStr },
    ])
    markRunning(sid, true)
    scrollToBottom(true)
    const refuse = (label: string) => {
      applyToSession(sid, 'assistant', (m) => ({ ...finishNotice(m, label), running: false }))
      markRunning(sid, false)
    }
    AgentService.RunCommand(text).then((refusal) => {
      if (refusal) refuse(refusal)
    }).catch(() => refuse('命令执行失败'))
  }, [currentId, updateSession, markRunning, scrollToBottom, applyToSession])

  // route is the single decision about text the user has committed: a leading "/" is a
  // command (the backend owns the list and answers with a notice when it does not know the
  // name); while a turn is running the text queues for the next steer point instead of
  // starting a competing turn; otherwise it starts one.
  const route = useCallback((text: string) => {
    if (text.startsWith('/')) {
      runCommand(text)
      return
    }
    if (running) {
      enqueuePending(currentId, text)
      return
    }
    sendText(text)
  }, [running, currentId, enqueuePending, sendText, runCommand])

  // submit sends text the user did not type into the box — the diff panel's 发给 agent.
  // It rides this route rather than a channel of its own, for the same reason: a busy
  // session has to queue, not refuse.
  const submit = useCallback((text: string) => {
    const t = text.trim()
    if (t) route(t)
  }, [route])

  // send is the composer's own entry point: it owns the text box, so it clears it first.
  const send = useCallback(() => {
    const text = input.trim()
    if (!text) return
    closeAt()
    setInput('')
    route(text)
  }, [input, closeAt, route])

  // acceptCommand completes the palette's highlighted name into the input, leaving a trailing
  // space so arguments follow naturally. Completing rather than sending is deliberate:
  // "/rev" + Enter should not run the wrong command.
  const acceptCommand = useCallback((cmd?: CommandVO) => {
    if (!cmd) return
    setCmdIdx(0)
    setInput('/' + cmd.name + ' ')
    requestAnimationFrame(() => composerRef.current?.focus())
  }, [])

  // sendPendingNow is the queue's "[立即发送]": interrupt the running turn and send the queued
  // text as the next one.
  //
  // Two things have to be true at once, and the ORDER is what makes them so:
  //   1. the interrupted turn's segment is sealed BEFORE the new message is placed — its
  //      terminal event (error / interrupted) targets "the newest running assistant", so a
  //      message placed first would be the one marked 已停止 (and its own reply dropped);
  //   2. the assistant placeholder is NOT placed here at all — the stream events open it (see
  //      applyToSession's openIfMissing), because the backend starts the next turn while the
  //      stop call is still on its way back, so its first delta can beat this function.
  // Only the user's bubble goes in now: it has to stay above the reply.
  const sendPendingNow = useCallback(async () => {
    const sid = currentId
    const text = takePending(sid)
    if (!text.trim()) return
    setSendingNow(true)

    if (!running) {
      // Nothing to interrupt: the plain send path places both bubbles and owns the IPC.
      sendText(text)
      setSendingNow(false)
      return
    }

    sealRunningSegment(sid)
    const ts = Date.now()
    updateSession(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: new Date(ts).toISOString() },
    ])
    markRunning(sid, true)
    scrollToBottom(true)

    const requeue = () => {
      // Nothing was sent, so the segment we just opened must be closed again — and the text
      // belongs back in the queue rather than in the transcript as if it had gone out.
      sealRunningSegment(sid)
      enqueuePending(sid, text)
      refreshRunning()
    }
    try {
      const ret = await AgentService.StopAndSend(text)
      if (ret && ret !== 'ok') requeue()
    } catch {
      requeue()
    }
    setSendingNow(false)
  }, [currentId, running, takePending, updateSession, enqueuePending, scrollToBottom,
      sealRunningSegment, markRunning, refreshRunning, sendText])

  // answerSteer replies to the agent's steer_check for a session: queued text is drained,
  // shown in the transcript and injected; otherwise the empty string unblocks the agent loop
  // without steering.
  const answerSteer = useCallback((sid: string) => {
    const text = takePending(sid)
    if (!text) {
      AgentService.Steer(sid, '').catch(() => {})
      return
    }
    injectSteerVisual(sid, text, new Date().toISOString())
    AgentService.Steer(sid, text).catch(() => {})
  }, [takePending, injectSteerVisual])

  // ── Subscriptions ─────────────────────────────────────────────────────────
  // A session switch changes the working directory, so any open picker is stale.
  useEffect(() => { closeAt() }, [currentId, closeAt])

  // The slash commands the desktop supports. Fetched once — the set is baked into the build —
  // and used both by the "/" palette and to decide what a lead-in "/" means when sending.
  useEffect(() => {
    AgentService.ListCommands().then((list) => setCmdList(list || [])).catch(() => {})
  }, [])

  // Native file drops arrive from Go (the webview cannot read dropped paths): resolve them
  // into @-references and splice them in at the caret.
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

  // Questions arrive while the turn is parked; the form renders in the transcript (see
  // AssistantBubble), so pull the view down to it.
  useEffect(() => {
    const off = Events.On('agent:ask', (event) => {
      const d = event.data as { sessionId?: string; toolId?: string; questions?: Question[] } | undefined
      if (!d?.sessionId || !d.questions?.length) return
      setAsks((prev) => ({ ...prev, [d.sessionId as string]: { toolId: d.toolId || '', questions: d.questions as Question[] } }))
      // The form renders in the transcript, so follow it into view — but only while the user is
      // already parked at the bottom: yanking someone who is reading history down to a form
      // would be exactly the kind of interruption this UI should avoid.
      if (d.sessionId === currentId) scrollToBottom()
    })
    return () => off?.()
  }, [currentId, scrollToBottom])

  // agent:idle fires after a turn goroutine has fully exited (running already reset on the
  // backend). A NATURAL completion auto-sends whatever the user queued while the turn ran
  // (same drain semantics as TUI's TurnComplete); stopped/errored turns leave the queue for
  // the user to review, drop or send manually.
  useEffect(() => {
    const off = Events.On('agent:idle', (event) => {
      const d = event.data as { sessionId: string; reason: string; current?: boolean }
      clearAsk(d.sessionId) // the turn is over: nothing can still be waiting
      // `current` comes from the backend (was this the displayed session?) rather than being
      // compared against this closure's currentId: an auto-compaction switch can move the
      // session between the two events, and React state read through a stale closure would
      // then silently drop the queued message.
      if (!d.current || d.reason !== 'complete') return
      const queued = pendingRef.current[d.sessionId] || []
      if (queued.length === 0) return
      sendText(takePending(d.sessionId))
    })
    return () => off?.()
  }, [sendText, takePending, clearAsk])

  // The "/" palette is DERIVED from the input rather than stored, so it can never disagree
  // with what would actually be dispatched: it is open while the input is a bare command name
  // still being typed (a space means arguments follow, an exact name means it is complete),
  // and Esc dismisses it for the current query only, so typing on reopens it.
  const cmdQuery = input.startsWith('/') && !/\s/.test(input.slice(1)) ? input.slice(1) : null
  const cmdMatches = cmdQuery === null ? [] : cmdList.filter((c) => c.name.startsWith(cmdQuery.toLowerCase()))
  const cmdOpen = cmdQuery !== null && cmdDismissed !== cmdQuery && !cmdList.some((c) => c.name === cmdQuery)

  return {
    // input box
    input, setInput, composerRef, composingRef, syncAtRef, closeAt,
    focusInput: () => composerRef.current?.focus(),
    focusTranscript: () => chatRef.current?.focus(),
    // @-picker + "/" palette
    at, setAt, acceptAt, cmdOpen, cmdMatches, cmdIdx, setCmdIdx, cmdQuery, setCmdDismissed, acceptCommand,
    // queue
    pending: pending[currentId] || [], dropPending: (i: number) => dropPending(currentId, i),
    clearPending: () => clearPending(currentId), sendPendingNow, sendingNow, rekeySessions,
    // questions
    ask: asks[currentId] || null, answer, answerCurrent, clearAsk,
    // sending
    send, submit, answerSteer,
  }
}

export type ComposerApi = ReturnType<typeof useComposer>

// Composer renders the input row: the pending queue above it, the two pickers and the
// textarea, and the stop/send buttons. It owns no state — `api` is useComposer's return
// value — so what is rendered and what is sent can never disagree. The <footer> around it
// (and the status row that shares that footer) belongs to the caller.
export function Composer({ api, running, onStop }: { api: ComposerApi; running: boolean; onStop: () => void }) {
  const { input, setInput, composerRef, composingRef, syncAtRef, closeAt, at, setAt, acceptAt, cmdOpen, cmdMatches, cmdIdx, setCmdIdx, setCmdDismissed, cmdQuery, acceptCommand, pending, dropPending, clearPending, sendPendingNow, sendingNow, send, focusTranscript } = api

  return (
    <>
      {pending.length > 0 && (
        <div className="pending-bar">
          <div className="pending-head">
            <span className="pending-title">
              {running ? '⏸ 待发送 · 会在本轮工具调用结束后自动插入' : '待发送消息'}
            </span>
            <span className="pending-actions">
              <button className="pending-send" disabled={sendingNow} onClick={sendPendingNow}>
                {sendingNow ? '正在停止…' : running ? '立即发送 · 打断' : '发送'}
              </button>
              <button className="pending-clear" onClick={clearPending} title="清空待发送队列">清空</button>
            </span>
          </div>
          {pending.map((t, i) => (
            <div className="pending-item" key={`${i}-${t.slice(0, 8)}`}>
              <span className="pending-bullet">·</span>
              <span className="pending-text" title={t}>{t}</span>
              <button className="pending-del" title="撤回该条" onClick={() => dropPending(i)}>✕</button>
            </div>
          ))}
        </div>
      )}
      <div className="composer-box" data-file-drop-target="true">
        {cmdOpen && (
          <CommandPicker
            items={cmdMatches}
            selected={Math.min(cmdIdx, Math.max(0, cmdMatches.length - 1))}
            onPick={(i) => acceptCommand(cmdMatches[i])}
          />
        )}
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
              // While the @-picker is open it owns the navigation keys; an IME composing
              // (candidate selection) owns them first.
              if (!ime && at) {
                // ↑↓ and Ctrl+N/Ctrl+P both move the highlight (Ctrl+N/P matches the TUI's
                // keymap, and preventDefault keeps Cocoa's own Ctrl+N/P caret movement out of
                // the way).
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
                  // Tab/Enter accept the highlighted match; with no match, Enter still sends
                  // (Tab just dismisses the picker).
                  if (at.items.length > 0) { e.preventDefault(); acceptAt(at.items[at.idx]); return }
                  if (e.key === 'Tab') { e.preventDefault(); closeAt(); return }
                }
                if (e.key === 'Escape') { e.preventDefault(); closeAt(); return }
              }
              // The "/" palette owns the navigation keys while it is open (the @-picker cannot
              // be open at the same time: one is triggered by "@", the other by a leading "/").
              if (!ime && cmdOpen) {
                const down = e.key === 'ArrowDown' || (e.ctrlKey && e.key.toLowerCase() === 'n')
                const up = e.key === 'ArrowUp' || (e.ctrlKey && e.key.toLowerCase() === 'p')
                if (down || up) {
                  e.preventDefault()
                  const delta = down ? 1 : -1
                  setCmdIdx((i) => Math.max(0, Math.min(cmdMatches.length - 1, i + delta)))
                  return
                }
                if (e.key === 'Tab') { e.preventDefault(); acceptCommand(cmdMatches[Math.min(cmdIdx, cmdMatches.length - 1)]); return }
                if (e.key === 'Escape') { e.preventDefault(); setCmdDismissed(cmdQuery); return }
                // Enter completes rather than sends while the name is still partial:
                // "/rev" + Enter must not run the wrong command.
                if (e.key === 'Enter' && !e.shiftKey && cmdMatches.length > 0) {
                  e.preventDefault()
                  acceptCommand(cmdMatches[Math.min(cmdIdx, cmdMatches.length - 1)])
                  return
                }
              }
              // Esc hands the focus to the message area (the Vim-like keys live there). Not
              // listed in the shortcut sheet on purpose: it is a focus affordance, not a
              // feature shortcut.
              if (e.key === 'Escape') { e.preventDefault(); focusTranscript(); return }
              if (e.key !== 'Enter' || e.shiftKey) return
              // Enter while an IME is composing confirms the candidate (选词), it must not send
              // the message.
              if (ime) return
              e.preventDefault()
              send()
            }}
            placeholder={running ? '正在运行…输入后 Enter 将加入待发送，在工具间隙自动插入' : '发送消息给 Tachi…（Enter 发送，Shift+Enter 换行，@ 引用文件）'} />
          {running && (
            <button className="stop-btn" title="停止生成" onClick={onStop} aria-label="停止生成">
              <svg viewBox="0 0 24 24" width="10" height="10" fill="currentColor"><rect x="7" y="7" width="10" height="10" rx="1.5" /></svg>
            </button>
          )}
        </div>
        <div className="composer-actions">
          <button className="send-btn" onClick={send} disabled={!input.trim() || sendingNow}>
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
            <span>{running ? '排队' : '发送'}</span>
          </button>
        </div>
      </div>
    </>
  )
}
