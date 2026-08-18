import { useState } from 'react'

import { shortTime } from '../../lib/format'
import type { APIRequest, Message } from '../../types/api'
import { MessageEvent } from './RequestGroup'
import { buildTurns, TurnBlock } from './TurnBlock'

// Renders the full event stream of one one-off transcript file. Events are
// raw JSONL lines (the backend returns them as untyped objects); the type
// field decides how each is drawn:
//   - "meta"        → collapsed into a header strip
//   - message types → reused MessageEvent (same look as the message tab)
//   - "api_request" → a collapsible request-payload card
//   - anything else → raw JSON fallback

const MESSAGE_TYPES = new Set([
  'user',
  'assistant',
  'thinking',
  'tool_call',
  'tool_result',
  'confirm',
  'reminder',
])

interface OneOffMeta {
  type?: string
  kind?: string
  session_id?: string
  cwd?: string
  provider?: string
  model?: string
  started_at?: string
  system_prompt?: string
  extra?: Record<string, string>
}

interface OneOffAPIRequest {
  type?: string
  timestamp?: string
  iteration?: number
  seq?: number
  system_prompt?: string
  user_prompt?: string
  model?: string
  provider?: string
  thinking?: string
  tools?: { name?: string; description?: string }[]
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n) + '…' : s
}

function MetaStrip({ meta }: { meta: OneOffMeta }) {
  return (
    <div className="px-2 py-2 mb-2 bg-paper border border-line rounded text-[11px] text-muted flex flex-wrap gap-x-4 gap-y-1">
      {meta.kind && (
        <span>
          kind <b className="text-inkdim">{meta.kind}</b>
        </span>
      )}
      {meta.model && (
        <span>
          模型 <b className="text-inkdim mono">{meta.model}</b>
        </span>
      )}
      {meta.provider && (
        <span>
          provider <b className="text-inkdim mono">{meta.provider}</b>
        </span>
      )}
      {meta.started_at && (
        <span>
          开始 <b className="text-inkdim">{shortTime(meta.started_at)}</b>
        </span>
      )}
      {meta.cwd && (
        <span className="hidden xl:inline">
          目录 <b className="text-inkdim mono">{meta.cwd}</b>
        </span>
      )}
      {meta.session_id && (
        <span className="hidden lg:inline">
          session <b className="text-inkdim mono">{meta.session_id}</b>
        </span>
      )}
      {meta.extra &&
        Object.entries(meta.extra).map(([k, v]) => (
          <span key={k}>
            {k} <b className="text-inkdim mono">{v}</b>
          </span>
        ))}
    </div>
  )
}

function APIRequestCard({ req }: { req: OneOffAPIRequest }) {
  const [open, setOpen] = useState(false)
  const toolNames = (req.tools ?? []).map((t) => t.name).filter(Boolean)
  return (
    <div className="ev ev-api-request">
      <div
        className="flex items-start gap-2 px-2 py-1.5 rounded hover:bg-paper cursor-pointer"
        onClick={() => setOpen(!open)}
      >
        <span className="shrink-0 w-[21px] text-center text-[13px] pt-0.5">📡</span>
        <div className="ml-[13px] pl-3 pr-1 pb-1 pt-0.5 border-l-2 border-line min-w-0 flex-1">
          <div className="flex items-center gap-2 text-[13px]">
            <span className="mono font-semibold text-linkblue">API 请求</span>
            {req.model && <span className="text-inkdim text-xs mono">{req.model}</span>}
            {req.provider && <span className="text-inkdim text-xs">· {req.provider}</span>}
            {req.iteration != null && (
              <span className="text-muted text-xs mono">iter {req.iteration}</span>
            )}
            {req.seq != null && <span className="text-muted text-xs mono">seq {req.seq}</span>}
            <span className="ml-auto text-[11px] text-muted whitespace-nowrap">
              {open ? '收起' : '展开'}
            </span>
          </div>
          {req.user_prompt && (
            <div className="mt-1 text-[13px] text-ink whitespace-pre-wrap break-words">
              {truncate(req.user_prompt, 300)}
            </div>
          )}
          {open && (
            <>
              {req.system_prompt && (
                <pre className="mt-1.5 bg-paper border border-line rounded p-2.5 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[280px] overflow-auto">
                  {req.system_prompt}
                </pre>
              )}
              {toolNames.length > 0 && (
                <div className="mt-1.5 text-[11px] text-muted">
                  工具 · <span className="mono">{toolNames.join(' · ')}</span>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  )
}

/** Full event stream of one one-off transcript file. */
export function OneOffEventStream({ events }: { events: unknown[] }) {
  const metas = events.filter(
    (e): e is OneOffMeta => isRecord(e) && e.type === 'meta',
  )
  // Message lines are session.Message JSON — the same shape the Inspector's
  // message tab renders, including iteration/seq on current data.
  const messages = events
    .filter((e) => isRecord(e) && typeof e.type === 'string' && MESSAGE_TYPES.has(e.type))
    .map((e) => e as unknown as Message)
  const apiRequests = events
    .filter((e) => isRecord(e) && e.type === 'api_request')
    .map((e) => {
      const { type: _type, ...req } = e as unknown as { type: string } & APIRequest
      return req
    })

  // Current data: messages carry iteration/seq, so render exactly like the
  // session detail (turn → request → step hierarchy, api_requests linked by
  // seq). Legacy oneoff files (written before iteration/seq existed) have no
  // such grouping and fall back to the flat stream below.
  if (messages.some((m) => m.iteration)) {
    const reqBySeq: Record<number, APIRequest> = {}
    for (const req of apiRequests) {
      if (req.seq) reqBySeq[req.seq] = req
    }
    const turns = buildTurns(messages)
    return (
      <div>
        {metas.map((meta, i) => (
          <MetaStrip key={i} meta={meta} />
        ))}
        <div className="space-y-3">
          {turns.map((turn) => (
            <TurnBlock key={turn.id} turn={turn} reqBySeq={reqBySeq} />
          ))}
        </div>
      </div>
    )
  }

  const rest = events.filter((e) => !(isRecord(e) && e.type === 'meta'))

  return (
    <div>
      {metas.map((meta, i) => (
        <MetaStrip key={i} meta={meta} />
      ))}
      <div className="flex flex-col gap-1">
        {rest.map((ev, i) => {
          if (!isRecord(ev) || typeof ev.type !== 'string') {
            return (
              <pre key={i} className="bg-paper border border-line rounded p-2 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[200px] overflow-auto">
                {JSON.stringify(ev, null, 2)}
              </pre>
            )
          }
          if (MESSAGE_TYPES.has(ev.type)) {
            return <MessageEvent key={i} m={ev as unknown as Message} />
          }
          if (ev.type === 'api_request') {
            return <APIRequestCard key={i} req={ev as unknown as OneOffAPIRequest} />
          }
          return (
            <pre key={i} className="bg-paper border border-line rounded p-2 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[200px] overflow-auto">
              {JSON.stringify(ev, null, 2)}
            </pre>
          )
        })}
      </div>
    </div>
  )
}
