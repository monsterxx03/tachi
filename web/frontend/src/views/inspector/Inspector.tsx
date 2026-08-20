import { useCallback, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'

import { api } from '../../api/client'
import type { AuthReporter } from '../../App'
import { useApi } from '../../lib/useApi'
import { SESSION_PAGE_SIZE, useSessionList } from '../../lib/useSessionList'
import { num, shortTime, yuan, compact } from '../../lib/format'
import { Badge, Empty, Loading } from '../../components/ui'
import { buildTurns, TurnBlock } from './TurnBlock'
import { OneOffEventStream } from './OneOffEventStream'
import type { APIRequest, OneOffSummary, SessionSummaryItem } from '../../types/api'

export function Inspector({ onAuthError }: { onAuthError: AuthReporter }) {
  const { id = '' } = useParams()
  const detail = useApi(() => api.getSession(id), [id], onAuthError)
  const sessionList = useSessionList(SESSION_PAGE_SIZE, onAuthError)
  const [tab, setTab] = useState<'messages' | 'oneoffs'>('messages')
  // Oneoff transcript expansion: which file is open, its loaded events
  // (cached per file), and the in-flight/error state of the lazy fetch.
  const [openOneOff, setOpenOneOff] = useState<string | null>(null)
  const [oneOffEvents, setOneOffEvents] = useState<Record<string, unknown[]>>({})
  const [oneOffLoading, setOneOffLoading] = useState(false)
  const [oneOffError, setOneOffError] = useState<string | null>(null)

  // Loads one one-off transcript's full event stream (GET
  // /api/sessions/{id}/oneoff/{file}); results are cached per file.
  const loadOneOff = useCallback(
    async (file: string) => {
      setOneOffLoading(true)
      setOneOffError(null)
      try {
        const data = await api.getOneOff(id, file)
        setOneOffEvents((prev) => ({ ...prev, [file]: data.events }))
      } catch (err: unknown) {
        if (err instanceof Error && err.name === 'AuthError') {
          onAuthError?.(err)
          return
        }
        setOneOffError(err instanceof Error ? err.message : String(err))
      } finally {
        setOneOffLoading(false)
      }
    },
    [id, onAuthError],
  )

  const toggleOneOff = useCallback(
    (o: OneOffSummary) => {
      if (openOneOff === o.file) {
        setOpenOneOff(null)
        return
      }
      setOpenOneOff(o.file)
      if (!oneOffEvents[o.file]) void loadOneOff(o.file)
    },
    [openOneOff, oneOffEvents, loadOneOff],
  )

  // Build the turn → request → step hierarchy once loaded.
  const turns = useMemo(() => {
    if (!detail.data) return []
    return buildTurns(detail.data.messages ?? [])
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
    return <Loading text="加载会话…" />
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
  const oneoffs = detail.data!.oneoffs ?? []

  // Session-wide token totals come from the backend ledger summary (covers
  // subagent/title/etc. calls too, not just the message stream). Input is
  // cache-miss only, and cache read/creation are separate, so the hit rate
  // is read / (read + creation + input). Cache & hit show only when the
  // cache was actually used; the whole stat hides on older backends.
  const usage = detail.data!.usage
  const hasTokens = usage.total_input !== undefined || usage.total_output !== undefined
  const inputTok = usage.total_input ?? 0
  const cacheRead = usage.total_cache_read ?? 0
  const cacheCreation = usage.total_cache_creation ?? 0
  const cacheTok = cacheRead + cacheCreation
  const hitRate =
    cacheRead > 0 && hasTokens
      ? Math.round((cacheRead / (cacheRead + cacheCreation + inputTok)) * 100)
      : undefined
  const tokenText = hasTokens
    ? `in ${compact(usage.total_input)} · out ${compact(usage.total_output)}` +
      (cacheTok > 0 ? ` · cache ${compact(cacheTok)}` : '') +
      (hitRate !== undefined ? ` · hit ${hitRate}%` : '')
    : ''

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
            {hasTokens && (
              <span>Token <b className="text-inkdim mono text-[11px]">{tokenText}</b></span>
            )}
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
                {oneoffs.map((o) => {
                  const open = openOneOff === o.file
                  const events = oneOffEvents[o.file]
                  return (
                    <div key={o.file} className="card px-4 py-3">
                      <div
                        className="flex items-center gap-2 mb-1.5 cursor-pointer select-none"
                        onClick={() => toggleOneOff(o)}
                      >
                        <span className="mono text-xs text-inkdim">{o.file}</span>
                        {o.kind && <Badge color="yellow">{o.kind}</Badge>}
                        <span className="ml-auto text-[11px] text-muted whitespace-nowrap">
                          {open ? '收起 ▲' : '展开 ▼ 查看完整历史'}
                        </span>
                      </div>
                      <div className="flex gap-3.5 text-[11px] text-muted flex-wrap">
                        {o.started_at && <span>开始 <b className="text-inkdim">{shortTime(o.started_at)}</b></span>}
                        {o.model && <span>模型 <b className="text-inkdim mono">{o.model}</b></span>}
                        <span>事件 <b className="text-inkdim">{o.event_count}</b></span>
                        <span>大小 <b className="text-inkdim mono">{num(o.size)} B</b></span>
                      </div>
                      {open && (
                        <div className="mt-3 pt-3 border-t border-line">
                          {oneOffLoading && !events && (
                            <div className="py-4 text-center text-muted text-xs">加载事件…</div>
                          )}
                          {oneOffError && !events && (
                            <div className="py-2 text-center text-xs">
                              <span className="text-danger">加载失败：{oneOffError}</span>{' '}
                              <button
                                className="btn"
                                onClick={() => void loadOneOff(o.file)}
                              >
                                重试
                              </button>
                            </div>
                          )}
                          {events && <OneOffEventStream events={events} />}
                        </div>
                      )}
                    </div>
                  )
                })}
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

