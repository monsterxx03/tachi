import { useState } from 'react'

import { credit, shortTime, yuan } from '../../lib/format'
import type { APIRequest, Message } from '../../types/api'
import { RequestGroup } from './RequestGroup'

// ── turn → request → step grouping ─────────────────────────────────────────
//
// Hierarchy: a Turn is one complete agent loop, started by a user message.
// Within a turn the LLM is called repeatedly; each call is an iteration
// (== one API request). Each API request may spawn 0..N tool executions,
// each an ordered tool_call + tool_result pair = a Step.
//
// Turn boundary: iterations restart at 1 for a fresh user prompt, so a
// message whose iteration drops (previous set iteration > current) starts a
// new turn. Steering (a mid-turn injected user prompt) does not reset the
// counter and stays within the current turn.
//
// User messages themselves carry no iteration (the backend records them
// without one), so they can't trigger a boundary directly. They are buffered
// as "pending" until the next iteration-bearing message arrives: if that
// message starts a new turn (iteration dropped), the buffered user becomes
// the new turn's title; otherwise (steer / trailing reminder) it stays at
// the end of the current turn.

export interface Step {
  call: Message
  result?: Message
}

export interface RequestView {
  iteration: number
  /** Session-wide request seq (from the messages of this request), used to
   *  look up the matching api_requests record across turns. */
  seq?: number
  events: Message[] // non-step messages bound to this iteration (thinking/assistant)
  steps: Step[]
}

export interface TurnView {
  id: number
  user?: Message // triggering user message (iteration may be unset)
  requests: RequestView[] // ordered by iteration
  prelude: Message[] // leading iteration-0 messages (reminders etc. before first request)
  postlude: Message[] // trailing iteration-0 messages (final reminder, steer user, ...)
}

function startNewTurn(prev: number | undefined, cur: number): boolean {
  if (cur === 0) return false
  if (prev === undefined) return false // first request in the stream
  return prev > cur
}

// Build turns from a flat message stream using the iteration-reset rule.
export function buildTurns(messages: Message[]): TurnView[] {
  const turns: TurnView[] = []
  let prevIter: number | undefined
  let curTurn: TurnView | null = null
  let curReq: RequestView | null = null
  // Iteration-less messages (user / reminder) buffered until the next
  // iteration-bearing message decides which turn they belong to.
  let pending: Message[] = []

  const openTurn = (): TurnView => {
    const t: TurnView = { id: turns.length, requests: [], prelude: [], postlude: [] }
    turns.push(t)
    return t
  }

  const closeReq = () => {
    if (curReq) {
      curTurn!.requests.push(curReq)
      curReq = null
    }
  }

  // Whether the current turn already holds request content (an open request
  // counts too — requests are closed only when the iteration changes).
  const turnHasContent = () => curReq !== null || curTurn!.requests.length > 0

  // Assign buffered iteration-less messages to curTurn: a user at the head
  // of an empty turn becomes the turn title; anything else is a prelude
  // (leading reminder) or postlude (trailing reminder / steer).
  const flushPending = () => {
    for (const m of pending) {
      if (m.type === 'user') {
        if (!turnHasContent() && !curTurn!.user) curTurn!.user = m
        else curTurn!.postlude.push(m)
      } else {
        if (!turnHasContent()) curTurn!.prelude.push(m)
        else curTurn!.postlude.push(m)
      }
    }
    pending = []
  }

  for (const m of messages) {
    const it = m.iteration ?? 0
    const newTurn = it !== 0 && startNewTurn(prevIter, it)

    if (it === 0) {
      // iteration-less message: reminder / user / steer.
      if (curTurn === null) curTurn = openTurn()
      if (!turnHasContent() && pending.length === 0) {
        // Turn head: assign immediately (reminder precedes user here).
        if (m.type === 'user') {
          if (!curTurn.user) curTurn.user = m
          else curTurn.postlude.push(m)
        } else {
          curTurn.prelude.push(m)
        }
      } else {
        // Mid-turn or trailing: defer until the next iteration decides.
        pending.push(m)
      }
      continue
    }

    // An iteration-bearing message: first settle any buffered messages.
    // A buffered user preceding a dropped-or-flat iteration (it <= prev)
    // was the next turn's trigger, so open that turn before flushing. Flat
    // (1 → 1) can't be caught by startNewTurn's strict drop, but the
    // pending user makes the boundary unambiguous — a steer always keeps
    // the counter rising (APICalls increments monotonically).
    let openedForPending = false
    if (pending.length > 0) {
      openedForPending = prevIter !== undefined && it <= prevIter && turnHasContent()
      if (openedForPending) {
        closeReq()
        curTurn = openTurn()
      }
      flushPending()
    }

    if (newTurn && !openedForPending) {
      closeReq()
      curTurn = openTurn()
      curReq = null
    }

    prevIter = it
    if (curReq && curReq.iteration !== it) {
      closeReq()
    }
    if (!curReq) {
      // Seq comes from the request-bound messages themselves: all messages
      // of one API call share the same session-wide seq (new data only;
      // legacy messages have none).
      curReq = { iteration: it, seq: m.seq, events: [], steps: [] }
    }
    if (m.type === 'tool_call') {
      curReq.steps.push({ call: m })
    } else if (m.type === 'tool_result') {
      // Attach to the unpaired step with the same tool_call_id — the only
      // reliable key when the same tool runs in parallel (name alone would
      // mispair, leaving steps stuck at "… 执行中"). Legacy data without
      // ids falls back to the previous last-step name match.
      const byId = curReq.steps.find(
        (s) => !s.result && s.call.tool_call_id && s.call.tool_call_id === m.tool_call_id,
      )
      if (byId) {
        byId.result = m
      } else {
        const last = curReq.steps[curReq.steps.length - 1]
        if (last && last.call.name === m.name && !last.result) last.result = m
        else curReq.events.push(m)
      }
    } else {
      curReq.events.push(m)
    }
  }

  // Stream ended with buffered iteration-less messages (e.g. a user prompt
  // whose turn has not produced output yet): keep them at the end of the
  // current turn.
  if (pending.length > 0 && curTurn) {
    flushPending()
  }
  closeReq()
  return turns
}

// Cost contributed by one request's assistant events (from their usage).
function requestCost(events: Message[]): number {
  let cost = 0
  for (const m of events) {
    if (m.type === 'assistant' && m.usage) {
      cost += (m.usage.input_tokens ?? 0) + (m.usage.output_tokens ?? 0) + (m.usage.cache_read_input_tokens ?? 0)
    }
  }
  // Rough ¥ by a nominal per-token rate (the backend /usage has precise
  // prices; this is a UI-only estimate for the request header).
  return cost * 0.000002
}

function requestTokens(events: Message[]) {
  let inTok = 0
  let outTok = 0
  for (const m of events) {
    if (m.type === 'assistant' && m.usage) {
      inTok += m.usage.input_tokens ?? 0
      outTok += m.usage.output_tokens ?? 0
    }
  }
  return { inTok: inTok || undefined, outTok: outTok || undefined }
}

// ── Turn block ──────────────────────────────────────────────────────────────
// A turn is a collapsible unit (one complete agent loop): header shows the
// triggering user prompt + request/step/cost summary; inside, each API
// request renders as a RequestGroup with its steps.

// resolveReq finds the APIRequest record for one request view via the
// session-wide seq (unique across turns). Records written before seq existed
// have no link and yield undefined — the request details are simply not
// shown for such legacy data.
function resolveReq(
  rv: RequestView,
  reqBySeq: Record<number, APIRequest>,
): APIRequest | undefined {
  return rv.seq !== undefined ? reqBySeq[rv.seq] : undefined
}

// Cost shown for one request: prefer the backend's precise per-call figure
// (attached to the resolved api_request), falling back to a token-based
// estimate for legacy data that predates cost resolution.
function requestCostOf(rv: RequestView, reqBySeq: Record<number, APIRequest>): number {
  const req = resolveReq(rv, reqBySeq)
  if (req?.cost !== undefined && req.cost > 0) return req.cost
  return requestCost(rv.events)
}

// requestCreditOf mirrors requestCostOf but for credit: the backend fills it
// from the ledger (see APIRequest.credit); there is no event-based estimate,
// so unpaired/legacy requests simply report 0 (and stay hidden).
function requestCreditOf(rv: RequestView, reqBySeq: Record<number, APIRequest>): number {
  const req = resolveReq(rv, reqBySeq)
  if (req?.credit !== undefined && req.credit > 0) return req.credit
  return 0
}

export function TurnBlock({
  turn,
  reqBySeq,
}: {
  turn: TurnView
  reqBySeq: Record<number, APIRequest>
}) {
  const [open, setOpen] = useState(true)

  const stepCount = turn.requests.reduce((a, r) => a + r.steps.length, 0)
  const totalCost = turn.requests.reduce(
    (a, r) => a + requestCostOf(r, reqBySeq),
    0,
  )
  const totalCredit = turn.requests.reduce(
    (a, r) => a + requestCreditOf(r, reqBySeq),
    0,
  )
  const lastReq = turn.requests[turn.requests.length - 1]
  const prompt =
    turn.user?.content ||
    (lastReq ? resolveReq(lastReq, reqBySeq)?.user_prompt : undefined) ||
    turn.requests[0]?.events.find((m) => m.type === 'user')?.content ||
    undefined
  const startTime = turn.user?.timestamp || turn.prelude[0]?.timestamp

  return (
    <div className="border border-line rounded-card bg-card shadow-card overflow-hidden">
      {/* turn header */}
      <div
        className="flex items-center gap-2.5 px-4 py-3 cursor-pointer select-none hover:bg-paper"
        onClick={() => setOpen(!open)}
      >
        <span
          className={`text-[11px] text-muted transition-transform ${
            open ? '' : '-rotate-90'
          }`}
        >
          ▼
        </span>
        <span className="text-[13px] font-bold text-accent whitespace-nowrap">
          Turn {turn.id + 1}
        </span>
        {startTime && (
          <span className="text-[11px] text-muted mono">{shortTime(startTime)}</span>
        )}
        <span className="flex-1 min-w-0 truncate text-[13px] text-inkdim">
          {prompt && (
            <span className="mr-2 text-muted">«</span>
          )}
          {prompt ? truncate(prompt, 80) : '(无触发消息)'}
          {prompt && <span className="ml-2 text-muted">»</span>}
        </span>
        <span className="text-[10px] text-inkdim bg-paper2 border border-line rounded-full px-2 py-0.5 whitespace-nowrap mono">
          {turn.requests.length} 请求
        </span>
        {stepCount > 0 && (
          <span className="text-[10px] text-inkdim bg-paper2 border border-line rounded-full px-2 py-0.5 whitespace-nowrap mono">
            {stepCount} steps
          </span>
        )}
        {totalCost > 0 && (
          <span className="text-[10px] text-gold font-semibold bg-warnsoft border border-line rounded-full px-2 py-0.5 whitespace-nowrap mono">
            {yuan(totalCost)}
          </span>
        )}
        {totalCredit > 0 && (
          <span className="text-[10px] text-gold font-semibold bg-warnsoft border border-line rounded-full px-2 py-0.5 whitespace-nowrap mono">
            {credit(totalCredit)}
          </span>
        )}
      </div>

      {open && (
        <div className="border-t border-line p-3">
          {/* prelude (leading reminders) — these system reminders were sent
              together with the user prompt, so the full prompt follows them */}
          {turn.prelude.length > 0 && (
            <div className="mb-2 space-y-1">
              {turn.prelude.map((m, i) => (
                <PreludeReminder key={i} m={m} />
              ))}
            </div>
          )}

          {/* full user prompt (trigger message) */}
          {turn.user && (
            <div className="mb-2 flex items-start gap-2 rounded border border-line bg-linksoft px-3 py-2">
              <span className="shrink-0 text-[13px] pt-0.5">👤</span>
              <div className="min-w-0 flex-1">
                <div className="text-[13px] leading-relaxed text-ink whitespace-pre-wrap break-words">
                  {turn.user.content}
                </div>
                <div className="mt-0.5 text-[11px] text-muted mono">
                  {(turn.user.content ?? '').length.toLocaleString()} chars
                </div>
              </div>
            </div>
          )}

          <div className="space-y-2">
            {turn.requests.map((rv) => {
              const req = resolveReq(rv, reqBySeq)
              const { inTok, outTok } = requestTokens(rv.events)
              // Clean user input for this request, only when this request
              // actually carried new user input: a steer / continuation
              // bound to it (they carry iteration now), or the turn's
              // trigger message on the first request. Intermediate tool
              // loops have no user prompt — and shouldn't show one.
              const userPrompt =
                rv.events.filter((m) => m.type === 'user').pop()?.content ??
                (rv.iteration === 1 ? turn.user?.content : undefined)
              return (
                <RequestGroup
                  key={rv.iteration}
                  iteration={rv.iteration}
                  model={req?.model}
                  cost={requestCostOf(rv, reqBySeq)}
                  credit={requestCreditOf(rv, reqBySeq)}
                  inTokens={inTok}
                  outTokens={outTok}
                  toolCount={req?.tools?.length ?? 0}
                  req={req}
                  userPrompt={userPrompt}
                  events={rv.events}
                  steps={rv.steps}
                />
              )
            })}
          </div>

          {/* postlude (trailing reminders / steer) */}
          {turn.postlude.length > 0 && (
            <div className="mt-2 space-y-1">
              {turn.postlude.map((m, i) => (
                <PreludeReminder key={i} m={m} />
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// PreludeReminder renders an iteration-less message (reminder / steer user)
// as a compact collapsible row inside a turn.
function PreludeReminder({ m }: { m: Message }) {
  const [open, setOpen] = useState(m.type !== 'reminder')
  return (
    <div
      className={`text-xs px-2 py-1 rounded hover:bg-paper cursor-pointer ${
        m.type === 'user' ? 'text-blue bg-linksoft' : 'text-inkdim'
      }`}
      onClick={() => setOpen(!open)}
    >
      {m.type === 'reminder' ? (
        <span className="text-inkdim italic">
          <span className="text-[10px] text-muted">▸</span> system reminder · 自动注入的上下文
          {open && (
            <pre className="mt-1 bg-paper border border-line rounded p-2 mono text-[11px] text-inkdim whitespace-pre-wrap break-all max-h-[200px] overflow-auto">
              {m.content}
            </pre>
          )}
        </span>
      ) : (
        <span className="text-ink">
          <span className="text-[10px] text-muted mr-1">👤</span>
          {truncate(m.content ?? '', 200)}
        </span>
      )}
    </div>
  )
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n) + '…' : s
}
