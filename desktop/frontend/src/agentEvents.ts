// The desktop's subscriptions to what the backend reports: the agent's status, the active
// session's live numbers, and the agent stream that writes the transcript.
//
// They were ~200 lines of useEffect scattered through App.tsx. They live together here
// because they share one rule that is easy to break by accident: every event carries the
// session it belongs to, and only SOME of them are about the session on screen. Writing the
// guards once, next to the event that needs them, is what keeps a background session's
// output from landing in the foreground transcript (and the reverse).

import { useCallback, useEffect, useState } from 'react'
import { Events } from '@wailsio/runtime'
import {
  AgentService,
  AgentStatus,
  type AgentState,
  type FileChangeVO,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { AgentEvent, Message } from './types'
import { pushPart, finishToolPart, updateToolPart, closeOpenToolParts } from './transcript'

// ── Agent status ────────────────────────────────────────────────────────────
// The statusbar's dot. NOT session-scoped: the backend runs one turn at a time, so this is
// simply "what the agent is doing now", and it is the second signal behind isCurrentRunning
// (a turn that has stopped but whose run has not been reaped yet).
const AGENT_IDLE: AgentState = { status: AgentStatus.StatusIdle, label: '空闲', detail: '就绪' }

export function useAgentStatus(): AgentState {
  const [state, setState] = useState<AgentState>(AGENT_IDLE)
  useEffect(() => {
    const off = Events.On('agent:state', (event) => { setState(event.data as AgentState) })
    // The event only fires on change, so the current value is fetched once at mount —
    // otherwise a window opened mid-turn would show 空闲 until the next transition.
    AgentService.GetState().then((s) => setState(s)).catch(() => {})
    return () => off?.()
  }, [])
  return state
}

// ── The active session's numbers ────────────────────────────────────────────
// Cumulative cost/credit ("积分") from the usage ledger, the cache-hit rate behind the ring,
// and the output rate. Session-scoped as a whole: switching sessions clears the row first
// and then re-reads the new session's ledger, because anything fetched per session that is
// merely overwritten keeps the previous session's value on screen whenever the payload is
// empty (that is exactly how a brand-new session inherited the last one's cache ring).
export function useSessionUsage(currentId: string) {
  const [cost, setCost] = useState(0)
  const [credit, setCredit] = useState(0)
  const [cacheHitRate, setCacheHitRate] = useState(0)
  const [hasCacheHit, setHasCacheHit] = useState(false)
  const [tps, setTps] = useState(0)
  const [lastTps, setLastTps] = useState(0)

  // refresh re-reads one session's ledger. Applied unconditionally: a missing payload means
  // "nothing recorded", not "keep what is there".
  const refresh = useCallback(async (id: string) => {
    try {
      const u = await (AgentService as any).GetSessionUsage?.(id)
      setCost(u?.cost || 0); setCredit(u?.credit || 0)
      setCacheHitRate(u?.cacheHitRate || 0); setHasCacheHit(!!u?.hasCacheHit)
    } catch { /* ignore: a failed fetch is not evidence of zero */ }
  }, [])

  // clear blanks the row immediately — the other half of refresh: the fetch confirms the
  // zeros, and this makes sure the row never shows another session's numbers in the
  // meantime.
  const clear = useCallback(() => {
    setCost(0); setCredit(0); setCacheHitRate(0); setHasCacheHit(false)
  }, [])

  // The output rate is per-turn-ish: a session switch resets it rather than carrying a
  // number measured on the previous transcript.
  const resetRate = useCallback(() => {
    setTps(0); setLastTps(0)
  }, [])

  useEffect(() => {
    const offCost = Events.On('agent:cost', (event) => {
      const d = event.data as { sessionId: string; cost: number; credit: number; cacheHitRate?: number; hasCacheHit?: boolean }
      if (d.sessionId === currentId) { setCost(d.cost || 0); setCredit(d.credit || 0); if (d.cacheHitRate != null) setCacheHitRate(d.cacheHitRate); setHasCacheHit(!!d.hasCacheHit) }
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

  return { cost, credit, cacheHitRate, hasCacheHit, tps, lastTps, refresh, clear, resetRate }
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
    refreshProvider, answerSteer, clearAsk, onSessionTitle,
  } = deps

  useEffect(() => {
    const off = Events.On('agent:tool', (event) => {
      const t = event.data as { name: string; title: string; args: string; change?: FileChangeVO | null }
      updateSession(currentId, (list) => {
        const target = [...list].reverse().find((m) => m.role === 'assistant' && m.running)
        if (!target) return list
        return list.map((m) => (m.id === target.id ? updateToolPart(m, t.name, t.title, t.args, t.change) : m))
      })
    })
    return () => off?.()
  }, [currentId, updateSession])

  useEffect(() => {
    const off = Events.On('agent:turn', (event) => {
      const d = event.data as { sessionId: string; durationMs: number; iterations: number; cost: number; credit: number }
      if (d.sessionId !== currentId) return
      updateSession(currentId, (list) => {
        const target = [...list].reverse().find((m) => m.role === 'assistant')
        if (!target) return list
        return list.map((m) => (m.id === target.id ? { ...m, summary: d } : m))
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
        case 'tool_call_start': applyToSession(sessionId, 'assistant', (m) => pushPart(m, { type: 'tool', name: ev.ToolName, title: '', args: '', summary: '执行中…', ok: true, done: false, expand: ev.ToolAutoExpand }), true); break
        case 'tool_result': applyToSession(sessionId, 'assistant', (m) => finishToolPart(m, ev.ToolName, ev.ToolResult, !ev.ToolIsError, ev.ToolDuration ? Math.round(ev.ToolDuration / 1e6) : undefined), true); break
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
