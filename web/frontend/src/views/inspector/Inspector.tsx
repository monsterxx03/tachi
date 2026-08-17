import { useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'

import { api } from '../../api/client'
import type { AuthReporter } from '../../App'
import { useApi } from '../../lib/useApi'
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

export interface Step {
  call: Message
  result?: Message
}

export interface RequestView {
  iteration: number
  req?: APIRequest
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

  const closeReq = () => {
    if (curReq) {
      curTurn!.requests.push(curReq)
      curReq = null
    }
  }

  for (const m of messages) {
    const it = m.iteration ?? 0
    const newTurn = it !== 0 && startNewTurn(prevIter, it)

    if (newTurn || curTurn === null) {
      closeReq()
      curTurn = { id: turns.length, requests: [], prelude: [], postlude: [] }
      turns.push(curTurn)
      curReq = null
    }

    if (it === 0) {
      // iteration-less message: reminder / user / steer. Attach to the turn.
      if (m.type === 'user' && curTurn.requests.length === 0) {
        curTurn.user = m
      } else {
        // steer or trailing reminder — keep in postlude (or prelude if first)
        if (curTurn.requests.length === 0) curTurn.prelude.push(m)
        else curTurn.postlude.push(m)
      }
    } else {
      prevIter = it
      if (curReq && curReq.iteration !== it) {
        closeReq()
      }
      if (!curReq) {
        curReq = { iteration: it, events: [], steps: [] }
      }
      if (m.type === 'tool_call') {
        // try to pair with an immediately following tool_result of same name
        curReq.steps.push({ call: m })
      } else if (m.type === 'tool_result') {
        // attach to the last unpaired step with matching name
        const last = curReq.steps[curReq.steps.length - 1]
        if (last && last.call.name === m.name && !last.result) last.result = m
        else curReq.events.push(m)
      } else {
        curReq.events.push(m)
      }
    }
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

function TurnBlock({
  turn,
  reqByIter,
}: {
  turn: TurnView
  reqByIter: Record<number, APIRequest>
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
    lastReq?.req?.user_prompt ||
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
          {prompt || '(无触发消息)'}
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
          {/* prelude (leading reminders) */}
          {turn.prelude.length > 0 && (
            <div className="mb-2 space-y-1">
              {turn.prelude.map((m, i) => (
                <PreludeReminder key={i} m={m} />
              ))}
            </div>
          )}

          <div className="space-y-2">
            {turn.requests.map((rv) => {
              const req = rv.req ?? reqByIter[rv.iteration]
              const { inTok, outTok } = requestTokens(rv.events)
              return (
                <RequestGroup
                  key={rv.iteration}
                  iteration={rv.iteration}
                  model={req?.model}
                  cost={requestCost(rv.events)}
                  inTokens={inTok}
                  outTokens={outTok}
                  prompt={req?.user_prompt ? truncate(req.user_prompt, 80) : undefined}
                  toolCount={req?.tools?.length ?? 0}
                  req={req}
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
  const sessions = useApi(() => api.listSessions(), [], onAuthError)
  const [tab, setTab] = useState<'messages' | 'oneoffs'>('messages')

  // Build the turn → request → step hierarchy once loaded.
  const turns = useMemo(() => {
    if (!detail.data) return []
    return buildTurns(detail.data.messages)
  }, [detail.data])

  // Map iteration → API request (for system prompt / tools in the request).
  // Iteration numbers restart at 1 per turn.
  const reqByIter: Record<number, APIRequest> = useMemo(() => {
    const map: Record<number, APIRequest> = {}
    for (const req of detail.data?.api_requests ?? []) {
      if (req.iteration) map[req.iteration] = req
    }
    return map
  }, [detail.data])

  if (detail.loading || sessions.loading) {
    return <div className="h-full flex items-center justify-center text-sm text-inkdim">加载会话…</div>
  }
  if (detail.error || sessions.error) {
    return (
      <div className="p-6">
        <div className="text-sm text-danger">{detail.error ?? sessions.error}</div>
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
          {sessions.data!.sessions.length} 个会话
        </div>
        <div className="flex-1 overflow-y-auto px-2">
          {sessions.data!.sessions.map((s) => (
            <SessionRow key={s.id} s={s} active={s.id === id} />
          ))}
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
                  reqByIter={reqByIter}
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