import { useState } from 'react'
import type { APIRequest, Message } from '../../types/api'
import { compact, durMs, yuan } from '../../lib/format'
import type { Step } from './Inspector'

// ── event icons / labels ───────────────────────────────────────────────────

const ICONS: Record<string, string> = {
  user: '👤',
  assistant: '💬',
  thinking: '💭',
  tool_call: '🔧',
  tool_result: '📋',
  reminder: 'ℹ️',
  confirm: '❓',
}

// ── one message ────────────────────────────────────────────────────────────

function MessageEvent({ m }: { m: Message }) {
  const isCall = m.type === 'tool_call'
  const hasCollapsible = isCall || m.type === 'tool_result'
  const isReminder = m.type === 'reminder'
  const [open, setOpen] = useState(isCall || isReminder ? false : hasCollapsible)

  return (
    <div className={`ev ev-${m.type}`}>
      <div
        className={`flex items-start gap-2 px-2 py-1.5 rounded hover:bg-paper ${
          hasCollapsible || isReminder ? 'cursor-pointer' : ''
        }`}
        onClick={hasCollapsible || isReminder ? () => setOpen(!open) : undefined}
      >
        <span className="shrink-0 w-[21px] text-center text-[13px] pt-0.5">
          {ICONS[m.type] ?? '•'}
        </span>
        <div className="ml-[13px] pl-3 pr-1 pb-1 pt-0.5 border-l-2 border-line min-w-0 flex-1">
          {m.type === 'tool_call' && (
            <div
              className="flex items-center gap-2 text-[13px]"
              onClick={(e) => {
                e.stopPropagation()
                setOpen(!open)
              }}
            >
              <span className="mono font-semibold text-linkblue">{m.name ?? 'tool'}</span>
              {m.content && (
                <span className="text-inkdim text-xs truncate">{m.content}</span>
              )}
              <span className="ml-auto text-[11px] text-muted mono">
                {m.usage ? `tok ${compact(m.usage.output_tokens)}` : ''}
                {argsString(m) ? ` · ${open ? '收起' : '展开参数'}` : ''}
              </span>
            </div>
          )}
          {m.type === 'tool_result' && (
            <div className="text-[11px] text-muted mono">
              {m.is_error ? '✗ error' : '✓ ok'}
              {m.name && ` · ${m.name}`}
              {m.duration_ms !== undefined && <> · {durMs(m.duration_ms)}</>}
              {m.result ? ` · ${open ? '收起' : '展开结果'}` : ''}
            </div>
          )}
          {m.type === 'reminder' ? (
            <div className="mt-0.5 text-xs">
              {!open && (
                <div className="text-inkdim italic flex items-center gap-1.5">
                  <span className="text-[10px] text-muted">▸</span>
                  system reminder · 自动注入的上下文（点击展开）
                  {m.content && (
                    <span className="text-muted/70 normal not-italic mono text-[10px]">
                      {m.content.length.toLocaleString()} chars
                    </span>
                  )}
                </div>
              )}
              {open && (
                <pre className="mt-1 bg-paper border border-line rounded p-2.5 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[280px] overflow-auto">
                  {m.content}
                </pre>
              )}
            </div>
          ) : (
            showableContent(m) && (
              <div
                className={`mt-1 text-[13.5px] leading-relaxed whitespace-pre-wrap break-words ${
                  m.type === 'thinking'
                    ? 'text-inkdim italic text-[12.5px]'
                    : 'text-ink'
                }`}
              >
                {truncate(m.content ?? '', 900)}
              </div>
            )
          )}
          {hasCollapsible && open && argsString(m) !== null && (
            <pre className="mt-1.5 bg-paper border border-line rounded p-2.5 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[180px] overflow-auto">
              {argsString(m)}
            </pre>
          )}
          {hasCollapsible && open && m.result && (
            <pre
              className={`mt-1.5 rounded p-2.5 mono text-xs whitespace-pre-wrap break-all max-h-[180px] overflow-auto ${
                m.is_error ? 'bg-dangersoft text-danger' : 'bg-accent-soft text-ink'
              }`}
            >
              {m.result}
            </pre>
          )}
        </div>
      </div>
    </div>
  )
}

function showableContent(m: Message): boolean {
  if (m.type === 'thinking' && !m.content) return false
  if (m.type === 'tool_call') return false // args shown above
  return Boolean(m.content)
}

// argsString normalizes a tool_call's args for display. args may be an
// object, a raw JSON string, or absent — return null when nothing to show.
function argsString(m: Message): string | null {
  if (m.args === undefined || m.args === null) return null
  if (typeof m.args === 'string') {
    const s = m.args.trim()
    if (!s) return null
    // Pretty-print when the string is JSON.
    try {
      return JSON.stringify(JSON.parse(s), null, 2)
    } catch {
      return s
    }
  }
  return JSON.stringify(m.args, null, 2)
}

function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n) + '…' : s
}

// ── one request group ──────────────────────────────────────────────────────

export interface RequestGroupProps {
  iteration: number
  model?: string
  cost: number
  inTokens?: number
  outTokens?: number
  prompt?: string
  toolCount: number
  req?: APIRequest
  /** Non-step messages bound to this iteration (thinking / assistant / ...). */
  events: Message[]
  /** Ordered tool executions (tool_call + tool_result pairs). */
  steps: Step[]
  defaultCollapsed?: boolean
}

export function RequestGroup({
  iteration,
  model,
  cost,
  inTokens,
  outTokens,
  prompt,
  toolCount,
  req,
  events,
  steps,
  defaultCollapsed = true,
}: RequestGroupProps) {
  const [collapsed, setCollapsed] = useState(defaultCollapsed)
  const [payloadOpen, setPayloadOpen] = useState(false)

  return (
    <div className="border border-line rounded-card bg-card shadow-card mb-4 max-w-[900px] overflow-hidden">
      <div
        className="flex items-center gap-2.5 px-4 py-2.5 cursor-pointer select-none bg-card border-b border-line hover:bg-paper"
        onClick={() => setCollapsed(!collapsed)}
      >
        <span
          className={`text-[11px] text-muted transition-transform ${collapsed ? '-rotate-90' : ''}`}
        >
          ▼
        </span>
        <span className="text-[13px] font-bold text-linkblue whitespace-nowrap">
          Request #{iteration}
        </span>
        {model && <span className="text-[11px] text-inkdim mono">{model}</span>}
        {(inTokens !== undefined || outTokens !== undefined) && (
          <span className="text-[10px] text-inkdim bg-paper2 border border-line rounded-full px-2 py-0.5 whitespace-nowrap font-mono">
            in <span className="text-accent">{compact(inTokens)}</span> · out{' '}
            <span className="text-accent">{compact(outTokens)}</span>
          </span>
        )}
        {cost > 0 && (
          <span className="text-[10px] text-inkdim bg-paper2 border border-line rounded-full px-2 py-0.5 whitespace-nowrap font-mono">
            <span className="text-gold font-semibold">{yuan(cost)}</span>
          </span>
        )}
        <span className="text-xs text-muted flex-1 min-w-0 truncate">
          {prompt ?? ''}
        </span>
        {toolCount > 0 && (
          <span className="text-[10px] text-linkblue bg-linksoft rounded-full px-2 py-0.5 whitespace-nowrap border border-line">
            🔧 {toolCount} tools
          </span>
        )}
      </div>

      {!collapsed && (
        <div className="py-1.5">
          {req && (
            <>
              {/* 请求详情（默认折叠） */}
              <div className="mx-4 mb-2.5 border border-dashed border-line2 rounded bg-paper">
                <div
                  className="flex items-center gap-2 px-3 py-2 cursor-pointer text-xs text-inkdim hover:bg-card"
                  onClick={() => setPayloadOpen(!payloadOpen)}
                >
                  <span
                    className={`text-[10px] text-muted transition-transform ${payloadOpen ? 'rotate-90' : ''}`}
                  >
                    ▸
                  </span>
                  <span className="font-semibold">📨 请求详情</span>
                  <span className="ml-auto text-muted mono text-[11px]">
                    {req.thinking ? `thinking ${req.thinking}` : ''}
                  </span>
                </div>

                {payloadOpen && (
                  <div className="px-3 pb-2.5">
                    {/* User prompt */}
                    {req.user_prompt && (
                      <>
                        <div className="border-t border-dotted border-line" />
                        <div className="flex items-center gap-2 px-1 py-2 text-xs text-inkdim">
                          <span className="font-semibold">User prompt</span>
                        </div>
                        <pre className="pl-2 border-l-[3px] border-linkblue rounded-r bg-card p-2.5 mono text-xs text-inkdim whitespace-pre-wrap break-all">
                          {req.user_prompt}
                        </pre>
                      </>
                    )}
                    {/* System prompt */}
                    <div className="border-t border-dotted border-line" />
                    <div className="flex items-center gap-2 px-1 py-2 text-xs text-inkdim">
                      <span className="font-semibold">System Prompt</span>
                      <span className="ml-auto text-muted mono text-[11px]">
                        {req.system_prompt.length.toLocaleString()} chars
                      </span>
                    </div>
                    <pre className="bg-card border border-line rounded p-2.5 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[240px] overflow-auto">
                      {req.system_prompt}
                    </pre>
                    {/* Tools */}
                    {req.tools && req.tools.length > 0 && (
                      <>
                        <div className="border-t border-dotted border-line mt-2" />
                        <div className="flex items-center gap-2 px-1 py-2 text-xs text-inkdim">
                          <span className="font-semibold">Tools（{req.tools.length}）</span>
                          <span className="ml-auto text-muted mono text-[11px]">
                            点击名称查看 schema
                          </span>
                        </div>
                        <ToolChips tools={req.tools} />
                      </>
                    )}
                  </div>
                )}
              </div>
            </>
          )}

          {events.map((m, i) => (
            <MessageEvent key={i} m={m} />
          ))}
          {steps.map((s, i) => (
            <StepBlock key={i} step={s} />
          ))}
        </div>
      )}
    </div>
  )
}

// ── one step (tool_call + tool_result) ────────────────────────────────────

function StepBlock({ step }: { step: Step }) {
  const [open, setOpen] = useState(false)
  const { call, result } = step
  const resultErr = result?.is_error
  return (
    <div className="ev ev-tool_call">
      <div
        className="flex items-start gap-2 px-2 py-1.5 rounded hover:bg-paper cursor-pointer"
        onClick={() => setOpen(!open)}
      >
        <span className="shrink-0 w-[21px] text-center text-[13px] pt-0.5">🔧</span>
        <div className="ml-[13px] pl-3 pr-1 pb-1 pt-0.5 border-l-2 border-warn min-w-0 flex-1">
          <div className="flex items-center gap-2 text-[13px]">
            <span className="mono font-semibold text-linkblue">{call.name ?? 'tool'}</span>
            <span className="ml-auto text-[11px] text-muted mono">
              {result
                ? resultErr
                  ? '✗ error'
                  : '✓ ok'
                : '… 执行中'}
              {call.duration_ms !== undefined && <> · {durMs(call.duration_ms)}</>}
              {(argsString(call) || result?.result) && (
                <> · {open ? '收起' : '展开'}</>
              )}
            </span>
          </div>
          {open && argsString(call) !== null && (
            <pre className="mt-1.5 bg-paper border border-line rounded p-2.5 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[180px] overflow-auto">
              {argsString(call)}
            </pre>
          )}
          {open && result?.result && (
            <pre
              className={`mt-1.5 rounded p-2.5 mono text-xs whitespace-pre-wrap break-all max-h-[220px] overflow-auto ${
                resultErr ? 'bg-dangersoft text-danger' : 'bg-accent-soft text-ink'
              }`}
            >
              {result.result}
            </pre>
          )}
        </div>
      </div>
    </div>
  )
}

// ── tool schema chips ──────────────────────────────────────────────────────

function ToolChips({ tools }: { tools: APIRequest['tools'] }) {
  const [selected, setSelected] = useState<string | null>(null)
  if (!tools) return null
  const active = tools.find((t) => t.name === selected)
  return (
    <div>
      <div className="flex flex-wrap gap-1.5">
        {tools.map((t) => (
          <button
            key={t.name}
            className={`mono text-[11px] px-2 py-0.5 rounded border transition-colors cursor-pointer ${
              selected === t.name
                ? 'bg-linkblue text-white border-linkblue'
                : 'text-linkblue bg-linksoft border-line hover:bg-linkblue hover:text-white'
            }`}
            onClick={() => setSelected(selected === t.name ? null : t.name)}
          >
            {t.name}
          </button>
        ))}
      </div>
      {active && (
        <pre className="mt-2 bg-card border border-line rounded p-2.5 mono text-xs text-inkdim whitespace-pre-wrap break-all max-h-[240px] overflow-auto">
          {JSON.stringify(active, null, 2)}
        </pre>
      )}
    </div>
  )
}