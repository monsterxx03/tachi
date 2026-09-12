// The transcript store: everything that belongs to a CONVERSATION rather than to the
// view — each session's messages, which sessions are running, how far back each one is
// loaded, and the frame batcher feeding them.
//
// The point of the hook is ownership, not tidiness. One React tree renders every
// session, so state that really means "the active session's X" leaks into the next
// session the moment it is not keyed by a session id; that bug shipped three times (the
// review notice, the plan/findings panel, and the cache-hit ring plus cost). Here it
// cannot happen by accident: every mutator takes the session it is for, and there is no
// way to read or write a transcript without naming one. `currentId` is used only where
// the UI is genuinely about "the session on screen" (patchMessage), never to address
// storage.

import { useCallback, useRef, useState } from 'react'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { Message } from './types'
import { appendPartDelta, lastRunningAssistantIndex } from './transcript'

// A window of a session's history plus the cursor it was read with. The three fields
// are written together because they only mean anything together: `hasMore` describes
// the tail of the loaded window, `earliestTs` is where the next older page starts.
export type SessionPage = { messages: Message[]; hasMore: boolean; earliestTs: string }

export function useSessionTranscript(currentId: string, scrollToBottom: (force?: boolean) => void) {
  const [msgCache, setMsgCache] = useState<Record<string, Message[]>>({})
  const [runningSet, setRunningSet] = useState<Set<string>>(new Set())
  const [hasMore, setHasMore] = useState<Record<string, boolean>>({})
  const [earliestTs, setEarliestTs] = useState<Record<string, string>>({})

  // ── The one funnel ─────────────────────────────────────────────────────────
  // Every transcript mutation goes through here: it hands ONE session's list to fn and
  // keeps the previous record when fn returns the list untouched, so an event that
  // changes nothing costs no re-render (the stream is chatty: turn_complete, idle and
  // the tool events all land on messages that may already be closed).
  const updateSession = useCallback((sid: string, fn: (list: Message[]) => Message[]) => {
    setMsgCache((prev) => {
      const list = prev[sid] || []
      const next = fn(list)
      return next === list ? prev : { ...prev, [sid]: next }
    })
  }, [])

  // patchMessage rewrites ONE message of the current session, by id: the shape a
  // transcript-local interaction needs (expanding a diff card), so the message map
  // stays the single source of truth for what is open.
  const patchMessage = useCallback((msgId: string, fn: (m: Message) => Message) => {
    updateSession(currentId, (list) => {
      const i = list.findIndex((m) => m.id === msgId)
      if (i < 0) return list
      const next = [...list]
      next[i] = fn(list[i])
      return next
    })
  }, [currentId, updateSession])

  // applyToSession patches the newest message of the given role in a session: every
  // streaming handler funnels through it (text/tool deltas and the auto-compaction
  // notices), so a live transcript is only ever mutated in one place — and it keys
  // purely off the session ID the backend sent, never off currentId, so a background
  // session still updates correctly.
  //
  // openIfMissing is for STREAM events (a delta, a tool call): if the session has no
  // running assistant, they open one. A turn can start before the frontend has placed
  // its placeholder — exactly what "立即发送" does, because the backend begins the next
  // turn while the stop call is still on its way back — and without this every delta of
  // that turn was dropped on the floor ("the reply never appeared"). Terminal events
  // (turn_complete, error) deliberately do NOT open one: they exist to close a turn, so
  // creating a message for them would turn an interrupted turn into a phantom bubble.
  const applyToSession = useCallback((sid: string, role: 'user' | 'assistant', fn: (m: Message) => Message, openIfMissing = false) => {
    updateSession(sid, (list) => {
      const target = role === 'user'
        ? [...list].reverse().find((m) => m.role === 'user')
        : [...list].reverse().find((m) => m.role === 'assistant' && m.running)
      if (!target) {
        if (!openIfMissing || role !== 'assistant') return list
        return [...list, fn(openAssistantMessage())]
      }
      return list.map((m) => (m.id === target.id ? fn(m) : m))
    })
  }, [updateSession])

  // sealRunningSegment finalizes the turn's currently-streaming assistant segment.
  //
  // An interrupted turn's terminal event (error / ExitReason=cancelled) is applied to
  // the newest RUNNING assistant — so if the next message's placeholder is already in
  // the transcript when it arrives, the flag lands on the wrong message. That is exactly
  // what "立即发送" used to produce: the brand-new placeholder read 已停止, and the reply
  // that followed had no running message to attach to and was dropped. Sealing first
  // leaves the incoming event with nothing to hit; a segment that rendered nothing is
  // removed rather than left as an empty bubble.
  const sealRunningSegment = useCallback((sid: string) => {
    updateSession(sid, (list) => {
      const back = [...list].reverse().findIndex((m) => m.role === 'assistant' && m.running)
      if (back < 0) return list
      const idx = list.length - 1 - back
      const out = [...list]
      if (!(list[idx].parts && list[idx].parts.length)) out.splice(idx, 1)
      else out[idx] = { ...list[idx], running: false, stopped: true }
      return out
    })
  }, [updateSession])

  // injectSteerVisual splits the streaming assistant segment so the queued user text
  // lands AFTER the tool work already shown and BEFORE the reply that continues after
  // the steer point: finalize the running segment, append the user bubble, open a fresh
  // running placeholder for the rest of the turn (subsequent deltas/tool events target
  // the newest running assistant). When the segment has rendered nothing yet the bubble
  // is inserted before it.
  const injectSteerVisual = useCallback((sid: string, text: string, ts: string) => {
    updateSession(sid, (list) => {
      let ai = -1
      for (let i = list.length - 1; i >= 0; i--) {
        if (list[i].role === 'assistant' && list[i].running) { ai = i; break }
      }
      if (ai < 0) return list
      const seg = list[ai]
      const now = Date.now()
      const u: Message = { id: `u-${now}-steer`, role: 'user', text, ts }
      const out = [...list]
      if (!(seg.parts && seg.parts.length)) {
        out.splice(ai, 0, u)
        return out
      }
      out[ai] = { ...seg, running: false }
      out.push(u, { id: `a-${now}-steer`, role: 'assistant', running: true, parts: [], ts })
      return out
    })
    // Respect the follow state here too: a steered message landing mid-history must not
    // yank a user who is reading older output back to the bottom.
    scrollToBottom()
  }, [updateSession, scrollToBottom])

  // ── Frame-batched deltas ───────────────────────────────────────────────────
  // Token deltas arrive far faster than the display refreshes. Batching them into a
  // single state update per animation frame caps markdown parsing and layout at ~60/s
  // instead of once per token. Non-delta events flush the buffer first, so the
  // transcript order stays exactly as it happened.
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
      // Group per session (order preserved within a session) so background sessions
      // batched in the same frame don't clobber each other.
      const groups = new Map<string, { type: 'thinking' | 'text'; delta: string }[]>()
      for (const it of q) {
        const g = groups.get(it.sid)
        if (g) g.push(it)
        else groups.set(it.sid, [it])
      }
      let out = prev
      for (const [sid, items] of groups) {
        let list = prev[sid] || []
        let idx = lastRunningAssistantIndex(list)
        if (idx < 0) {
          // Same reason as applyToSession's openIfMissing: deltas can arrive before the
          // frontend has placed the placeholder for the turn producing them.
          list = [...list, openAssistantMessage()]
          prev = { ...prev, [sid]: list }
          out = prev
          idx = list.length - 1
        }
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

  // ── Running set ────────────────────────────────────────────────────────────
  // Which sessions have a turn in flight, per the backend (the status bar's own
  // busy state is a second, frontend-side signal — see isCurrentRunning).
  const refreshRunning = useCallback(async () => {
    const list = (await AgentService.RunningSessions().catch(() => null)) || []
    setRunningSet(new Set(list))
  }, [])
  const markRunning = useCallback((sid: string, running: boolean) => {
    setRunningSet((prev) => {
      if (prev.has(sid) === running) return prev
      const next = new Set(prev)
      if (running) next.add(sid)
      else next.delete(sid)
      return next
    })
  }, [])

  // ── Loaded-history cursor ──────────────────────────────────────────────────
  const setCursor = useCallback((sid: string, page: SessionPage) => {
    setHasMore((p) => ({ ...p, [sid]: page.hasMore }))
    setEarliestTs((p) => ({ ...p, [sid]: page.earliestTs }))
  }, [])

  // setSessionPage replaces a session's loaded window (first load, or a re-read).
  const setSessionPage = useCallback((sid: string, page: SessionPage) => {
    setMsgCache((prev) => ({ ...prev, [sid]: page.messages }))
    setCursor(sid, page)
  }, [setCursor])

  // prependSessionPage puts an OLDER page in front of what is loaded (scrolling to the
  // top) and moves the cursor back with it.
  const prependSessionPage = useCallback((sid: string, page: SessionPage) => {
    setMsgCache((prev) => ({ ...prev, [sid]: [...page.messages, ...(prev[sid] || [])] }))
    setCursor(sid, page)
  }, [setCursor])

  // openSession registers a brand-new session with nothing loaded.
  const openSession = useCallback((sid: string) => {
    setSessionPage(sid, { messages: [], hasMore: false, earliestTs: '' })
  }, [setSessionPage])

  // markHistoryEnd records that the loaded window IS the whole history: the next scroll
  // to the top is a no-op instead of another round trip for an empty page.
  const markHistoryEnd = useCallback((sid: string) => {
    setHasMore((p) => ({ ...p, [sid]: false }))
  }, [])

  // rekeySession follows auto-compaction into the child session. It is deliberately NOT
  // a cache rename: the compacted session is a different conversation — it begins at the
  // summary, not at the messages that were summarised — so the parent's loaded window is
  // dropped rather than carried over (carrying it left pre-compaction messages visible
  // under the new session, which is exactly what a user scrolling up must not see). The
  // running flag DOES move: it describes the run, not the transcript.
  const rekeySession = useCallback((from: string, to: string) => {
    if (!from || !to || from === to) return
    const dropKey = <T,>(rec: Record<string, T>): Record<string, T> => {
      if (!(from in rec) && !(to in rec)) return rec
      const { [from]: dropped, [to]: replaced, ...rest } = rec
      return rest
    }
    setMsgCache(dropKey)
    setHasMore(dropKey)
    setEarliestTs(dropKey)
    setRunningSet((prev) => (prev.has(from) ? new Set([...prev].map((id) => (id === from ? to : id))) : prev))
  }, [])

  return {
    // data
    msgCache, runningSet, hasMore, earliestTs,
    // transcript mutation
    updateSession, patchMessage, applyToSession, sealRunningSegment, injectSteerVisual,
    // history window
    setSessionPage, prependSessionPage, markHistoryEnd, openSession, rekeySession,
    // running set
    refreshRunning, markRunning,
    // streaming
    enqueueDelta, flushDeltas,
  }
}

// openAssistantMessage is the placeholder a stream event opens when the frontend has
// not placed one yet: an empty running assistant its deltas and tool cards attach to.
function openAssistantMessage(now = Date.now()): Message {
  return { id: `a-${now}-open`, role: 'assistant', running: true, parts: [], ts: new Date(now).toISOString() }
}
