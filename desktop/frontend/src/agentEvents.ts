// The desktop's subscriptions to what the backend reports: the agent's status, the active
// session's live numbers, and the agent stream that writes the transcript.
//
// They were ~200 lines of useEffect scattered through App.tsx. They live together here
// because they share one rule that is easy to break by accident: every event carries the
// session it belongs to, and only SOME of them are about the session on screen. Writing the
// guards once, next to the event that needs them, is what keeps a background session's
// output from landing in the foreground transcript (and the reverse).

import { useCallback, useEffect, useRef, useState } from 'react'
import { Events } from '@wailsio/runtime'
import {
  AgentService,
  AgentStatus,
  type AgentState,
  type FileChangeVO,
  type TurnChangesVO,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { AgentEvent, Message } from './types'
import { pushPart, finishToolPart, updateToolPart, closeOpenToolParts } from './transcript'

// ── Agent status ────────────────────────────────────────────────────────────
// What each session is doing right now — one entry per conversation, keyed by the id every
// payload carries. The caller gets the session on screen's own state, never a neighbour's.
//
// It used to be ONE global value, on the belief that "the backend runs one turn at a time".
// That is not true of this frontend: every session owns its agent and turn goroutine,
// switching sessions cancels nothing, and the sidebar marks each running session — so a
// single value was one conversation's status read as another's. Two symptoms, one cause: the
// stop control followed the user into sessions with nothing to stop (and clicking it did
// nothing — Stop acts on the displayed session's turn), and since a state change was only
// pushed while its session was displayed, a background turn's END was never reported: the
// spinner was there to stay.
//
// The state a session reports before anything has been heard about it: a session that has
// not changed state in this process is idle, so the fallback is idle and never busy — the
// safe direction (a spinner that shows up late is a nuisance; one that cannot be cleared is
// the bug). It carries no session id on purpose: it describes no session.
const AGENT_IDLE: AgentState = { sessionId: '', status: AgentStatus.StatusIdle, label: '空闲', detail: '就绪', waiting: '' }

// The caller gets the session on screen's own state AND the whole map: the sidebar marks
// EVERY session's row (a background conversation waiting on the reader is exactly the case
// the mark is for), and two subscriptions to one event would be two copies of the same
// fact, kept in step by hand.
export function useAgentStatus(currentId: string): { state: AgentState; byId: Record<string, AgentState> } {
  const [byId, setById] = useState<Record<string, AgentState>>({})
  useEffect(() => {
    // An unnamed payload cannot be filed — and would otherwise be applied to whatever is on
    // screen when it lands, which is the bug this hook exists to prevent. The backend stamps
    // the id on every one (see setSessionState).
    const file = (st?: AgentState | null) => {
      if (!st?.sessionId) return
      setById((prev) => ({ ...prev, [st.sessionId]: st }))
    }
    // seed files a session nothing has been heard about yet. The mount snapshot below is the
    // second source of the same fact, and the two have no ordering rule between them: an app
    // that starts working immediately (the smoke drivers; or a window opened onto a session that
    // is already running) can have the read land AFTER the turn's own end event it predates, and
    // then the older value wins — the session stays 运行中, with a stop control that clears
    // nothing and a composer that queues every send behind a turn that is over. A live event is
    // always the newer truth for a session it has named.
    const seed = (st?: AgentState | null) => {
      if (!st?.sessionId) return
      setById((prev) => (prev[st.sessionId] ? prev : { ...prev, [st.sessionId]: st }))
    }
    const off = Events.On('agent:state', (event) => file(event.data as AgentState))
    // The event only fires on change, so the displayed session's current value is fetched
    // once at mount — otherwise a window reloaded mid-turn would show 空闲 (and lose its
    // stop control) until the next transition, however long the running tool call takes.
    AgentService.GetState().then(seed).catch(() => {})
    return () => off?.()
  }, [])
  return { state: byId[currentId] || AGENT_IDLE, byId }
}

// ── Conversations that finished while the reader was elsewhere ──────────────
// A turn ending in the conversation on screen is not news (the reply appears right there, live);
// one ending in ANOTHER conversation is, and it stays news until that conversation is looked at.
// That is the one thing the running spinner cannot say — it stops exactly when the work does — so
// the sidebar's highlight outlives the turn.
//
// Session-scoped, deliberately not focus-scoped: window focus is the notification's business (see
// desktop/notify.go), and a marker that depended on it would light up for whatever happened to end
// while the reader was in another app — including the conversation already on screen, which has
// nothing to catch up on. So a conversation on screen can never be unseen.
//
// In memory on purpose: this is "since you last looked", and the app coming back up onto a
// conversation IS a look at it.
export function useUnseenSessions(currentId: string): Record<string, true> {
  const [unseen, setUnseen] = useState<Record<string, true>>({})
  useEffect(() => {
    // `current` is computed in Go at the moment the turn ended — the authoritative answer to "was
    // the reader looking at this one?", which is why it is read from the payload and not compared
    // here (a switch landing in between would otherwise mark the session being opened).
    const off = Events.On('agent:idle', (event) => {
      const d = event.data as { sessionId?: string; current?: boolean }
      if (!d || !d.sessionId || d.current) return
      setUnseen((prev) => ({ ...prev, [d.sessionId as string]: true }))
    })
    return () => off?.()
  }, [])

  // Looking at a conversation is what clears it. Every way of arriving at one ends up setting
  // currentId — the row's click, both palettes, the successor a delete hands over, the session the
  // app opens with — so one rule covers all of them. It re-runs on the MAP as well: a turn ending
  // in the same instant the reader switches to its conversation must not leave that row marked.
  useEffect(() => {
    if (!currentId) return
    setUnseen((prev) => {
      if (!prev[currentId]) return prev
      const next = { ...prev }
      delete next[currentId]
      return next
    })
  }, [unseen, currentId])

  // The map that goes out never contains the conversation on screen, so a row cannot be marked
  // "unseen" for even the frame it takes the effect above to catch up.
  if (!currentId || !unseen[currentId]) return unseen
  const out = { ...unseen }
  delete out[currentId]
  return out
}

// ── Scheduled runs ──────────────────────────────────────────────────────────
// A cron run is a normal turn in the conversation, but the backend starts it —
// so the user's bubble has to come from an event. It is also the signal that
// this turn is not steerable: the pending bar says 「等定时任务结束后发送」
// instead of promising an injection at the next tool boundary.
export function useCronRunningSessions(): Record<string, true> {
  const [byID, setByID] = useState<Record<string, true>>({})
  useEffect(() => {
    const set = (sid: string | undefined, on: boolean) => {
      if (!sid) return
      setByID((prev) => {
        if (on) {
          if (prev[sid]) return prev
          return { ...prev, [sid]: true }
        }
        if (!prev[sid]) return prev
        const next = { ...prev }
        delete next[sid]
        return next
      })
    }
    const offTurn = Events.On('agent:cron_turn', (event) => {
      const d = event.data as { sessionId?: string }
      set(d?.sessionId, true)
    })
    const offIdle = Events.On('agent:idle', (event) => {
      const d = event.data as { sessionId?: string }
      set(d?.sessionId, false)
    })
    const offErr = Events.On('agent:error', (event) => {
      const d = event.data as { sessionId?: string }
      set(d?.sessionId, false)
    })
    return () => { offTurn?.(); offIdle?.(); offErr?.() }
  }, [])
  return byID
}

// ── The active session's numbers ────────────────────────────────────────────
// Cumulative cost/credit ("积分") from the usage ledger, the cache-hit rate behind the ring,
// the output rate, and the context-window estimate behind the context ring. Session-scoped as
// a whole: switching sessions clears the row first and then re-reads the new session's
// numbers, because anything fetched per session that is merely overwritten keeps the previous
// session's value on screen whenever the payload is empty (that is exactly how a brand-new
// session inherited the last one's cache ring).
//
// The context estimate belongs here rather than in App because it shares this hook's one hard
// requirement — it must follow the API calls, not the turn. It used to be read only on
// mount / new / switch / turn_complete, so during a long turn (many tool rounds) the ring sat
// at the value the turn started with while the popover, which fetches when it opens, already
// knew the new one (「上下文圆环一直是空的，点开倒是有」 — pinned by ctx-ring).
export function useSessionUsage(currentId: string) {
  const [cost, setCost] = useState(0)
  const [credit, setCredit] = useState(0)
  const [cacheHitRate, setCacheHitRate] = useState(0)
  const [hasCacheHit, setHasCacheHit] = useState(false)
  const [tps, setTps] = useState(0)
  const [lastTps, setLastTps] = useState(0)
  const [ctxEstimate, setCtxEstimate] = useState(0)
  const [ctxWindow, setCtxWindow] = useState(0)

  // refresh re-reads one session's numbers. Applied unconditionally: a missing payload means
  // "nothing recorded", not "keep what is there".
  //
  // Both reads are BY ID. The context numbers could come from GetProviderInfo in the same
  // round trip, but that one describes whatever the backend has active — and a refresh that
  // races a session switch would then put one session's window into another's ring.
  const refresh = useCallback(async (id: string) => {
    try {
      const u = await (AgentService as any).GetSessionUsage?.(id)
      setCost(u?.cost || 0); setCredit(u?.credit || 0)
      setCacheHitRate(u?.cacheHitRate || 0); setHasCacheHit(!!u?.hasCacheHit)
    } catch { /* ignore: a failed fetch is not evidence of zero */ }
    // The estimate for a session that has not run a turn in this process yet — just created,
    // or resumed from disk — which the per-call event cannot supply.
    try {
      const info = await AgentService.GetContextInfo(id)
      setCtxEstimate(info?.estimate || 0)
      setCtxWindow(info?.contextWindow || 0)
    } catch { /* ignore: same rule as above */ }
  }, [])

  // clear blanks the row immediately — the other half of refresh: the fetch confirms the
  // zeros, and this makes sure the row never shows another session's numbers in the
  // meantime.
  const clear = useCallback(() => {
    setCost(0); setCredit(0); setCacheHitRate(0); setHasCacheHit(false)
    setCtxEstimate(0); setCtxWindow(0)
  }, [])

  // The output rate is per-turn-ish: a session switch resets it rather than carrying a
  // number measured on the previous transcript.
  const resetRate = useCallback(() => {
    setTps(0); setLastTps(0)
  }, [])

  useEffect(() => {
    const offCost = Events.On('agent:cost', (event) => {
      const d = event.data as {
        sessionId: string; cost: number; credit: number
        cacheHitRate?: number; hasCacheHit?: boolean
        contextEstimate?: number; contextWindow?: number
      }
      if (d.sessionId === currentId) {
        setCost(d.cost || 0); setCredit(d.credit || 0)
        if (d.cacheHitRate != null) setCacheHitRate(d.cacheHitRate)
        setHasCacheHit(!!d.hasCacheHit)
        // Every API call moves these (see desktop/agent_turn.go emitUsage), which is what
        // makes the ring keep up with a turn in flight.
        if (d.contextEstimate != null) setCtxEstimate(d.contextEstimate)
        if (d.contextWindow != null) setCtxWindow(d.contextWindow)
      }
    })
    const offTps = Events.On('agent:tps', (event) => {
      const d = event.data as { sessionId: string; tps: number; lastTps?: number }
      if (d.sessionId !== currentId) return
      // tps == 0 means "the turn stopped producing"; the last measured rate is kept so the
      // statusbar can show a paused number instead of a bare 0.
      if (d.tps > 0) { setTps(d.tps); setLastTps(0) }
      else { setTps(0); if (d.lastTps) setLastTps(d.lastTps) }
    })
    return () => { offCost?.(); offTps?.() }
  }, [currentId])

  return { cost, credit, cacheHitRate, hasCacheHit, tps, lastTps, ctxEstimate, ctxWindow, refresh, clear, resetRate }
}

// ── The agent stream ────────────────────────────────────────────────────────
// Everything the running turn reports about itself, applied to the transcript. The session
// each event names is authoritative: the guards below compare against currentId only where
// the UI genuinely means "the session on screen" (the sidebar row, the error bubble), never
// to address storage — `updateSession`/`applyToSession` take the session id the backend sent.
export type AgentStreamDeps = {
  currentId: string
  // Transcript store (useTranscript.ts).
  updateSession: (sid: string, fn: (list: Message[]) => Message[]) => void
  applyToSession: (sid: string, role: 'user' | 'assistant', fn: (m: Message) => Message, openIfMissing?: boolean) => void
  enqueueDelta: (sid: string, type: 'thinking' | 'text', delta: string) => void
  flushDeltas: () => void
  // The transcript's steer splitter: another screen's interjection lands where the window's own
  // queued steer would (after the tool work already shown, before the reply that continues).
  injectSteerVisual: (sid: string, text: string, ts: string) => void
  refreshRunning: () => void
  // App-owned actions. All must be stable (useCallback) — they are effect dependencies.
  refreshProvider: () => void
  // Reply to the agent's steer_check: drain the session's pending queue into the transcript.
  answerSteer: (sid: string) => void
  // A turn is over: nothing can still be waiting on an answer.
  clearAsk: (sid: string) => void
  onSessionTitle: (sid: string, title: string) => void
}

export function useAgentStream(deps: AgentStreamDeps) {
  const {
    currentId, updateSession, applyToSession, enqueueDelta, flushDeltas, refreshRunning,
    refreshProvider, answerSteer, clearAsk, onSessionTitle, injectSteerVisual,
  } = deps

  // A scheduled run has no composer to place its user bubble, so the backend's
  // start event does it. The running placeholder is placed here too: the first
  // text delta would open one anyway, but until then the reader would see a
  // bubble with no "working" line under it.
  useEffect(() => {
    const off = Events.On('agent:cron_turn', (event) => {
      const d = event.data as { sessionId?: string; display?: string }
      if (!d?.sessionId || !d.display) return
      const now = Date.now()
      const ts = new Date(now).toISOString()
      updateSession(d.sessionId, (list) => [
        ...list,
        { id: `u-${now}-cron`, role: 'user', text: d.display as string, ts },
        { id: `a-${now}-cron`, role: 'assistant', running: true, parts: [], ts },
      ])
    })
    return () => off?.()
  }, [updateSession])

  // A message another SCREEN sent (the phone; the device's voice path speaks through the
  // composer, so it arrives as a normal send). The window's own bubbles are drawn at submit
  // time, never from an event — so origin === 'desktop' is skipped, and without this the window
  // showed only the assistant's reply to a question it never displayed.
  useEffect(() => {
    const off = Events.On('agent:user_message', (event) => {
      const d = event.data as {
        sessionId?: string; text?: string; displayText?: string; steer?: boolean; origin?: string
      }
      if (!d?.sessionId || d.origin === 'desktop') return
      const text = d.displayText || d.text || ''
      if (!text) return
      const now = Date.now()
      const ts = new Date(now).toISOString()
      if (d.steer) {
        // An interjection splits the live segment exactly like the window's own queued steer.
        injectSteerVisual(d.sessionId, text, ts)
        return
      }
      updateSession(d.sessionId, (list) => [
        ...list,
        { id: `u-${now}-echo`, role: 'user', text, ts },
      ])
    })
    return () => off?.()
  }, [injectSteerVisual, updateSession])

  useEffect(() => {
    const off = Events.On('agent:tool', (event) => {
      // toolCallId is what tells one card from another when the same tool is called several times
      // in one response (see matchToolPart) — without it every parallel call's args landed on the
      // newest card (issue #39).
      const t = event.data as { toolCallId?: string; name: string; title: string; args: string; change?: FileChangeVO | null }
      updateSession(currentId, (list) => {
        const target = [...list].reverse().find((m) => m.role === 'assistant' && m.running)
        if (!target) return list
        return list.map((m) => (m.id === target.id ? updateToolPart(m, t.toolCallId || '', t.name, t.title, t.args, t.change) : m))
      })
    })
    return () => off?.()
  }, [currentId, updateSession])

  useEffect(() => {
    const off = Events.On('agent:turn', (event) => {
      const d = event.data as {
        sessionId: string; durationMs: number; iterations: number; cost: number; credit: number
        checkpointTurn?: number
        // What this turn changed, from its own two checkpoint trees (absent when there is no
        // pair for it — the footer then falls back to the tool-call fragments).
        changes?: TurnChangesVO | null
      }
      if (d.sessionId !== currentId) return
      updateSession(currentId, (list) => {
        const target = [...list].reverse().find((m) => m.role === 'assistant')
        // The turn is also the moment we learn WHICH checkpoint it was, and the newest
        // non-steer user bubble is the prompt that started it — so this is where a live
        // bubble (assembled here, not loaded from disk) gets the turn to rewind to.
        const bubble = [...list].reverse().find((m) => m.role === 'user' && !m.steer)
        const turn = d.checkpointTurn || undefined
        return list.map((m) => {
          // The card it belongs to, NOT the user bubble: the footer chip hangs off the
          // assistant card, and the turn is what the diff panel and the review are scoped by.
          if (target && m.id === target.id) return { ...m, summary: d, turn, changes: d.changes ?? undefined }
          if (bubble && m.id === bubble.id && turn) return { ...m, checkpointTurn: turn }
          return m
        })
      })
    })
    return () => off?.()
  }, [currentId, updateSession])

  useEffect(() => {
    const off = Events.On('agent:error', (event) => {
      const d = event.data as { sessionId: string; error: string; interrupted?: boolean }
      // The turn is over, so its question cannot still be waiting — in EVERY session, which
      // is why this runs before the current-session guard below.
      clearAsk(d.sessionId)
      if (d.sessionId !== currentId) return
      const interrupted = !!d.interrupted
      updateSession(currentId, (list) => {
        // The turn being concluded is the newest assistant message. Unlike a running-scan,
        // this still works when agent:event has already flipped running off (listener order
        // is not guaranteed). Mutations are idempotent so double-handling is harmless.
        const target = [...list].reverse().find((m) => m.role === 'assistant')
        if (!target) return list
        const conclude = (m: Message): Message => {
          // After a stop no tool_result event arrives for in-flight calls, so close their
          // cards; a real error is surfaced as a trailing text part (the transcript renders
          // parts in order — there is no separate error field to append to).
          const parts = interrupted
            ? closeOpenToolParts(m.parts, '已中断')
            : [...(m.parts || []), { type: 'text' as const, text: '⚠ ' + (d.error || '出错') }]
          return { ...m, running: false, stopped: interrupted || m.stopped, parts }
        }
        return list.map((m) => (m.id === target.id ? conclude(m) : m))
      })
    })
    return () => off?.()
  }, [currentId, updateSession, clearAsk])

  useEffect(() => {
    const off = Events.On('agent:event', (event) => {
      const d = event.data as { sessionId: string; event: AgentEvent }
      const { sessionId, event: ev } = d
      // Deltas ride the frame batcher; every other event must be applied in order, so flush
      // pending text/thinking first.
      if (ev.Type !== 'thinking_delta' && ev.Type !== 'text_delta') flushDeltas()
      switch (ev.Type) {
        case 'thinking_delta': enqueueDelta(sessionId, 'thinking', ev.ThinkingDelta || ''); break
        case 'text_delta': enqueueDelta(sessionId, 'text', ev.TextDelta); break
        // The two stream events that may have to OPEN the message they write to: a turn can
        // start before the frontend has placed its placeholder (see applyToSession).
        case 'tool_call_start': applyToSession(sessionId, 'assistant', (m) => pushPart(m, { type: 'tool', name: ev.ToolName, toolCallId: ev.ToolID, title: '', args: '', summary: '执行中…', ok: true, done: false, expand: ev.ToolAutoExpand }), true); break
        case 'tool_result': applyToSession(sessionId, 'assistant', (m) => finishToolPart(m, ev.ToolID || '', ev.ToolName, ev.ToolResult, !ev.ToolIsError, ev.ToolDuration ? Math.round(ev.ToolDuration / 1e6) : undefined), true); break
        case 'turn_complete': applyToSession(sessionId, 'assistant', (m) => ({ ...m, running: false })); break
        case 'session_title': {
          // The generated title arrives here and nowhere else: the sidebar renders the list it
          // fetched on load/switch, so without this the row kept saying 未命名会话 for the rest
          // of the session (the title was in the meta file all along, and only a restart — or a
          // session switch — ever showed it).
          if (ev.Title) onSessionTitle(sessionId, ev.Title)
          break
        }
        case 'steer_check':
          // The agent finished a round of tool calls and is parked, waiting for pending user
          // input to steer the next LLM call. Queued text is drained, shown in the transcript
          // and injected; an empty queue replies "" to unblock the loop without steering.
          answerSteer(sessionId)
          break
        case 'error': {
          const interrupted = ev.Result?.ExitReason === 'interrupted' || ev.Result?.ExitReason === 'cancelled'
          applyToSession(sessionId, 'assistant', (m) => ({ ...m, running: false, stopped: interrupted || m.stopped }))
          break
        }
      }
      // These two are backend round-trips over IPC and used to run for EVERY delta — the
      // single biggest cause of stutter while streaming. They only carry new information at
      // turn boundaries.
      if (ev.Type === 'turn_complete' || ev.Type === 'error') {
        refreshRunning()
        refreshProvider()
      }
    })
    return () => off?.()
  }, [applyToSession, refreshRunning, refreshProvider, answerSteer, flushDeltas, enqueueDelta, onSessionTitle])
}

// OneOffRun is one side-channel run (/review, /commit) as the frontend sees it.
//
// The backend reports its LIFECYCLE only — start, then end or error, with the facts the
// conversation's one-line anchor needs — never its content: the run writes its own record
// file, and the panel reads that. One source of truth, and no second streaming path to keep
// in step with the transcript's. Design §4.2.
export type OneOffRun = {
  kind: string
  phase: 'start' | 'end' | 'error'
  // findings is the number of ReportFinding calls the run made (0 for /commit).
  findings: number
  durationMs: number
  iterations: number
  // interrupted marks a stop the user asked for — a conclusion, not a failure.
  interrupted?: boolean
  error?: string
  // at distinguishes two runs of the same kind: the payload carries no run id, and the
  // effects that react to a result must fire once per run.
  at: number
}

// useOneOffStream follows the side-channel runs of the session ON SCREEN: `live` while one
// is running (the panel polls its record), `result` once it ended.
export function useOneOffStream(currentId: string) {
  const [live, setLive] = useState<OneOffRun | null>(null)
  const [result, setResult] = useState<OneOffRun | null>(null)

  useEffect(() => {
    const off = Events.On('agent:oneoff', (event) => {
      const d = event.data as {
        sessionId?: string; kind?: string; phase?: string; findings?: number
        durationMs?: number; iterations?: number; interrupted?: boolean; error?: string
      }
      // Only the displayed session's runs: the anchor is a line in THIS conversation, and a
      // run in a background session has no line here to fill in.
      if (!d?.sessionId || d.sessionId !== currentId) return
      const run: OneOffRun = {
        kind: d.kind || '',
        phase: d.phase === 'start' ? 'start' : d.phase === 'error' ? 'error' : 'end',
        findings: d.findings || 0,
        durationMs: d.durationMs || 0,
        iterations: d.iterations || 0,
        interrupted: d.interrupted,
        error: d.error,
        at: Date.now(),
      }
      if (run.phase === 'start') {
        setLive(run)
        setResult(null)
        return
      }
      setLive(null)
      setResult(run)
    })
    return () => off?.()
  }, [currentId])

  return { live, result }
}

// ── Workspace changes ───────────────────────────────────────────────────────
// A project write (create / rename / edit roots / delete) moves the workspace of every member
// session at once, and the frontend holds that state by value: the composer's chip, the roots
// panel, the sidebar's groups were all pulled at some earlier moment (design §7.4). Without
// this, an already-open panel keeps naming the old directory — and a deleted project's
// read-only branch keeps refusing edits the backend now allows.
//
// The handler is the caller's: it re-reads what it shows (the session on screen plus the
// sidebar), because what is stale is not a value this hook could patch. The payload names
// what changed for readers that want to be cleverer than that; nothing here requires it.
export function useWorkspaceChanged(onChange: () => void): void {
  const ref = useRef(onChange)
  ref.current = onChange
  useEffect(() => {
    // The guard exists so a handler re-created every render does not re-subscribe every render
    // (the backend emits rarely; the resubscription would be pure churn).
    const off = Events.On('agent:workspace_changed', () => ref.current())
    return () => off?.()
  }, [])
}

// ── MCP authorization ───────────────────────────────────────────────────────
// One authorization is a conversation with the user: it starts, it usually needs a browser
// opened, it can end up asking for the redirect URL to be pasted back, and it finishes by
// connecting a server or failing. None of that fits in a return value — the binding that
// starts a flow answers "ok" immediately and the outcome arrives later — so the progress is
// kept here, per server, from the events.
//
// Per server, not globally: sessions share one pool and one set of connections, but the
// panel lists several servers and two of them can be in different states at once (one
// waiting for a paste while another connects).
export type MCPAuthState = {
  // starting | browser | waiting | manual | ok | error — the backend's own phases
  // (MCPAuthEvent, desktop/agent_mcp.go), plus "starting" set locally before the reply.
  phase: string
  message: string
  authUrl: string
}

export function useMCPAuth(onDone: () => void): {
  auth: Record<string, MCPAuthState>
  setAuth: (server: string, state: MCPAuthState) => void
  clearAuth: (server: string) => void
} {
  const [auth, setAuth] = useState<Record<string, MCPAuthState>>({})
  const ref = useRef(onDone)
  ref.current = onDone
  useEffect(() => {
    const off = Events.On('agent:mcp_auth', (event) => {
      const ev = event.data as { server: string; phase: string; message?: string; authUrl?: string }
      if (!ev?.server) return
      setAuth((prev) => ({
        ...prev,
        [ev.server]: { phase: ev.phase || '', message: ev.message || '', authUrl: ev.authUrl || '' },
      }))
      // A server that just came up has tools and a new connection state to show, and the
      // panel holds both by value — the same reason a project write re-reads the sidebar.
      if (ev.phase === 'ok') ref.current()
    })
    return () => off?.()
  }, [])

  const setOne = useCallback((server: string, state: MCPAuthState) => {
    setAuth((prev) => ({ ...prev, [server]: state }))
  }, [])
  const clearOne = useCallback((server: string) => {
    setAuth((prev) => {
      const next = { ...prev }
      delete next[server]
      return next
    })
  }, [])
  return { auth, setAuth: setOne, clearAuth: clearOne }
}

// ── MCP finished coming up ──────────────────────────────────────────────────
// The shared pool is filled only after EVERY configured server has answered, while a
// connection counts as up as soon as its own handshake and tools/list succeed. Between those
// two moments a server is 已连接 with no tools to list, and a panel opened on a read from
// inside that window would show nothing and never correct itself — so the backend says when
// the pool is settled and the panel re-reads.
export function useMCPReady(onReady: () => void): void {
  const ref = useRef(onReady)
  ref.current = onReady
  useEffect(() => {
    const off = Events.On('agent:mcp_ready', () => ref.current())
    return () => off?.()
  }, [])
}

// ── the node reading moved ─────────────────────────────────────────────────
// The node board and the count on its trigger are snapshots nobody re-reads by themselves: the
// panel reads once when it opens, and the probe that read kicks off answers LATER — so a node
// discovery had just found would sit on 检查中… until 刷新 was pressed, and the count would not move
// at all. The backend says when the reading changed (a machine entered the directory, or a round of
// answers came back different), and this is what re-reads; it is deliberately not a payload, so the
// event and the read can never disagree about a row.
export function usePeersChanged(onChanged: () => void): void {
  const ref = useRef(onChanged)
  ref.current = onChanged
  useEffect(() => {
    const off = Events.On('agent:peers_changed', () => ref.current())
    return () => off?.()
  }, [])
}
