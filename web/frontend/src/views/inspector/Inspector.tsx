import { useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'

import { api } from '../../api/client'
import type { AuthReporter } from '../../App'
import { useApi } from '../../lib/useApi'
import { SESSION_PAGE_SIZE, useSessionList } from '../../lib/useSessionList'
import { num, shortTime, yuan } from '../../lib/format'
import { Badge, Empty } from '../../components/ui'
import { RequestGroup } from './RequestGroup'
import type { APIRequest, Message, SessionSummaryItem } from '../../types/api'

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
function buildTurns(messages: Message[]): TurnView[] {
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

function TurnBlock({
  turn,
  reqBySeq,
}: {
  turn: TurnView
  reqBySeq: Record<number, APIRequest>
}) {
  const [open, setOpen] = useState(true)

  const stepCount = turn.requests.reduce((a, r) => a + r.steps.length, 0)
  const totalCost = turn.requests.reduce(
    (a, r) => a + requestCost(r.events),
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
                  cost={requestCost(rv.events)}
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

export function Inspector({ onAuthError }: { onAuthError: AuthReporter }) {
  const { id = '' } = useParams()
  const detail = useApi(() => api.getSession(id), [id], onAuthError)
  const sessionList = useSessionList(SESSION_PAGE_SIZE, onAuthError)
  const [tab, setTab] = useState<'messages' | 'oneoffs'>('messages')

  // Build the turn → request → step hierarchy once loaded.
  const turns = useMemo(() => {
    if (!detail.data) return []
    return buildTurns(detail.data.messages)
  }, [detail.data])

  // Session-wide seq → APIRequest (unique across turns, unlike the per-turn
  // iteration). Records without seq (legacy data) simply don't resolve.
  const reqBySeq: Record<number, APIRequest> = useMemo(() => {
    const map: Record<number, APIRequest> = {}
    for (const req of detail.data?.api_requests ?? []) {
      if (req.seq) map[req.seq] = req
    }
    return map
  }, [detail.data])

  if (detail.loading || sessionList.loading) {
    return <div className="h-full flex items-center justify-center text-sm text-inkdim">加载会话…</div>
  }
  if (detail.error || sessionList.error) {
    return (
      <div className="p-6">
        <div className="text-sm text-danger">{detail.error ?? sessionList.error}</div>
        <Link to="/" className="btn mt-3 inline-block">返回</Link>
      </div>
    )
  }
  const meta = detail.data!.meta
  const oneoffs = detail.data!.oneoffs

  return (
    <div className="flex h-full">
      {/* 左侧会话列表 */}
      <div className="w-[290px] shrink-0 bg-paper2 border-r border-line flex flex-col">
        <div className="p-3 pb-2">
          <input
            className="w-full bg-card border border-line rounded px-2.5 py-1.5 text-[13px] outline-none focus:border-accent"
            placeholder="🔍 搜索会话…"
          />
        </div>
        <div className="text-[11px] text-muted px-3 pb-1">
          {sessionList.total} 个会话
        </div>
        <div ref={sessionList.containerRef} className="flex-1 overflow-y-auto px-2">
          {sessionList.sessions.map((s) => (
            <SessionRow key={s.id} s={s} active={s.id === id} />
          ))}
          {/* 滚动加载哨兵 */}
          <div ref={sessionList.sentinelRef} />
          {sessionList.loadingMore && (
            <div className="py-2 text-center text-[11px] text-muted">加载中…</div>
          )}
          {sessionList.loadMoreError && (
            <div className="py-2 text-center text-[11px]">
              <span className="text-danger">加载失败</span>{' '}
              <button className="btn" onClick={sessionList.loadMore}>重试</button>
            </div>
          )}
          {!sessionList.hasMore && sessionList.total > 0 && (
            <div className="py-2 text-center text-[11px] text-muted">
              已加载全部 {sessionList.total} 个会话
            </div>
          )}
        </div>
      </div>

      {/* 右侧详情 */}
      <div className="flex-1 flex flex-col min-w-0">
        <div className="px-5 pt-3.5 pb-2.5 bg-card border-b border-line">
          <div className="flex items-center gap-2 flex-wrap mb-1">
            <span className="text-[15px] font-semibold mr-1">
              {meta.title || '(无标题)'}
            </span>
            <Badge color="green">{meta.mode || 'auto'}</Badge>
            {meta.provider_name && <Badge color="gray">{meta.provider_name}</Badge>}
            <Badge color="gold">{yuan(detail.data!.usage.total_cost)}</Badge>
            <span className="flex-1" />
            <a
              className="btn-primary"
              href={api.transcriptUrl(id)}
              target="_blank"
              rel="noreferrer"
            >
              ⬇ 导出 Transcript
            </a>
          </div>
          <div className="flex gap-3.5 text-xs text-muted flex-wrap">
            <span>
              ID <b className="text-inkdim mono text-[11px]">{meta.id}</b>
            </span>
            <span>创建 <b className="text-inkdim">{shortTime(meta.created_at)}</b></span>
            <span>消息 <b className="text-inkdim">{num(detail.data!.messages.length)}</b></span>
            <span>工具 <b className="text-inkdim">{num(detail.data!.messages.filter((m) => m.type === 'tool_call').length)}</b></span>
            {meta.working_dir && (
              <span className="hidden xl:inline">目录 <b className="text-inkdim">{meta.working_dir}</b></span>
            )}
          </div>
        </div>

        {/* 子 tab */}
        <div className="flex items-center bg-card border-b border-line px-3.5">
          <SubTab active={tab === 'messages'} onClick={() => setTab('messages')}>
            消息流 <span className="count-badge">{turns.length} turn</span>
          </SubTab>
          <SubTab active={tab === 'oneoffs'} onClick={() => setTab('oneoffs')}>
            OneOffs <span className="count-badge">{oneoffs.length}</span>
          </SubTab>
        </div>

        {tab === 'messages' ? (
          <div className="flex-1 overflow-y-auto px-5 py-4 pb-10">
            {turns.length > 0 && (
              <p className="text-[11px] text-muted mb-3">
                共 {turns.length} 个 turn · 每个 turn 是一次完整的 agent loop，内部按 API 请求递进、每次工具调用是一个 step。
              </p>
            )}
            <div className="space-y-3">
              {turns.map((turn) => (
                <TurnBlock
                  key={turn.id}
                  turn={turn}
                  reqBySeq={reqBySeq}
                />
              ))}
            </div>
            {turns.length === 0 && (
              <Empty icon="💬" title="此会话暂无消息" />
            )}
          </div>
        ) : (
          <div className="flex-1 overflow-y-auto px-5 py-4 pb-10">
            {oneoffs.length === 0 ? (
              <Empty
                icon="🧩"
                title="此会话暂无 oneoff（旁路执行）记录"
                hint="侧信道调用（/commit、/review、ambient、dream）会记录在这里"
              />
            ) : (
              <div className="flex flex-col gap-2 max-w-[860px]">
                {oneoffs.map((o) => (
                  <div key={o.file} className="card px-4 py-3">
                    <div className="flex items-center gap-2 mb-1.5">
                      <span className="mono text-xs text-inkdim">{o.file}</span>
                      {o.kind && <Badge color="yellow">{o.kind}</Badge>}
                    </div>
                    <div className="flex gap-3.5 text-[11px] text-muted flex-wrap">
                      {o.started_at && <span>开始 <b className="text-inkdim">{shortTime(o.started_at)}</b></span>}
                      {o.model && <span>模型 <b className="text-inkdim mono">{o.model}</b></span>}
                      <span>事件 <b className="text-inkdim">{o.event_count}</b></span>
                      <span>大小 <b className="text-inkdim mono">{num(o.size)} B</b></span>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function SessionRow({ s, active }: { s: SessionSummaryItem; active: boolean }) {
  return (
    <Link
      to={`/sessions/${s.id}`}
      className={`flex items-center gap-2.5 px-2 py-2 rounded cursor-pointer hover:bg-paper ${
        active ? 'bg-accent-soft outline outline-1 outline-line2' : ''
      }`}
    >
      <span
        className={`w-1.5 h-1.5 rounded-full shrink-0 ${s.message_count > 0 ? 'bg-accent' : 'bg-line2'}`}
      />
      <span className="flex-1 min-w-0">
        <span className="block text-[13px] font-semibold text-ink truncate">
          {s.title || '(无标题)'}
        </span>
        <span className="block text-[11px] text-muted">
          {shortTime(s.updated_at)} · {num(s.message_count)} msgs
        </span>
      </span>
      <span className="text-xs font-semibold text-gold mono">{yuan(s.cost)}</span>
    </Link>
  )
}

function SubTab({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      onClick={onClick}
      className={`px-3.5 py-2.5 text-[13px] border-b-2 flex items-center gap-1.5 -mb-px cursor-pointer ${
        active
          ? 'text-accent font-semibold border-accent'
          : 'text-inkdim border-transparent hover:text-ink'
      }`}
    >
      {children}
    </button>
  )
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n) + '…' : s
}