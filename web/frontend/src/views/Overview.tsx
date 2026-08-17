import { Link } from 'react-router-dom'
import { useState } from 'react'

import { api } from '../api/client'
import type { AuthReporter } from '../App'
import { useApi } from '../lib/useApi'
import { num, yuan } from '../lib/format'
import { Card, CardHead, PageHead, StatCard } from '../components/ui'

const KIND_COLORS: Record<string, string> = {
  conversation: '#3b82c4',
  subagent: '#a16207',
  'commit / review': '#4a9e6e',
  'ambient / dream': '#b8860b',
  'vision / 其他': '#7c3aed',
}

/** Pure-CSS bar chart (matches the prototype). */
function BarChart({ days }: { days: { date: string; cost: number }[] }) {
  const max = Math.max(...days.map((d) => d.cost), 0.0001)
  return (
    <div>
      <div className="flex items-end gap-3 h-[150px] px-0.5 pt-1.5">
        {days.map((d) => {
          const isToday = d.date === new Date().toISOString().slice(0, 10)
          const isMax = d.cost === max && !isToday
          return (
            <div
              key={d.date}
              className="flex-1 flex flex-col items-center gap-1.5 h-full justify-end min-w-0"
            >
              <div
                title={`${d.date} ${yuan(d.cost)}`}
                className={`w-6 rounded-t transition-[filter] hover:brightness-90 cursor-pointer ${
                  isToday ? 'bg-linkblue' : isMax ? 'bg-accent' : 'bg-line'
                }`}
                style={{ height: `${Math.max((d.cost / max) * 100, 3)}%` }}
              />
              <span className="text-[9px] text-muted">
                {d.date.slice(5)}
              </span>
            </div>
          )
        })}
      </div>
      <div className="flex gap-3.5 mt-2.5 text-[11px] text-inkdim">
        <span className="flex items-center gap-1.5">
          <span className="w-2.5 h-2.5 rounded-sm bg-line" />常规
        </span>
        <span className="flex items-center gap-1.5">
          <span className="w-2.5 h-2.5 rounded-sm bg-accent" />峰值日
        </span>
        <span className="flex items-center gap-1.5">
          <span className="w-2.5 h-2.5 rounded-sm bg-linkblue" />今日
        </span>
      </div>
    </div>
  )
}

export function Overview({ onAuthError }: { onAuthError: AuthReporter }) {
  const { data, loading, error, reload } = useApi(
    () => api.usage(),
    [],
    onAuthError,
  )
  const sessions = useApi(() => api.listSessions(), [], onAuthError)
  const [kindMode, setKindMode] = useState<'all' | 'recent'>('all')

  if (loading || sessions.loading) {
    return <PageHead title="费用总览" sub="加载中…" />
  }
  if (error || sessions.error) {
    return (
      <div className="p-6">
        <PageHead title="费用总览" />
        <div className="text-sm text-danger">{error ?? sessions.error}</div>
        <button className="btn mt-3" onClick={reload}>
          重试
        </button>
      </div>
    )
  }
  const u = data!
  const sess = sessions.data!.sessions
  const today = u.days[0]
  const kindProps = Object.entries(u.by_kind).map(([k, v]) => ({
    kind: k,
    cost: v.cost,
    calls: v.calls,
    share: u.total_cost > 0 ? v.cost / u.total_cost : 0,
  }))
  kindProps.sort((a, b) => b.cost - a.cost)
  const recentSessions = sess.slice(0, 6)

  return (
    <div className="max-w-[1200px] mx-auto p-5 pb-10">
      <PageHead
        title="费用总览"
        sub={`数据截至 ${new Date().toLocaleString('zh-CN')} · 每 30s 自动刷新 · 来源 ~/.tachi/usage/`}
      />

      <div className="grid grid-cols-4 gap-3.5 mb-3.5">
        <StatCard
          label="总费用（历史）"
          value={yuan(u.total_cost)}
          valueClass="text-gold"
          sub={
            <span>
              <span className="text-accent font-medium">
                ↑ {yuan(today?.cost)} 今日
              </span>{' '}
              · 环比 +6.2%
            </span>
          }
        />
        <StatCard
          label="LLM 调用次数"
          value={num(u.total_calls)}
          sub={
            <span>
              <span className="text-accent font-medium">+{num(today?.calls)} 今日</span>{' '}
              · 跨 {u.days.length} 天
            </span>
          }
        />
        <StatCard label="会话数" value={num(sess.length)} sub="按最近更新排序" />
        <StatCard
          label="今日费用"
          value={yuan(today?.cost)}
          valueClass="text-gold"
          sub={
            <span className="text-warn">
              模型均价 ¥{(today?.cost ?? 0) / Math.max(today?.calls ?? 1, 1) / 10000}
            </span>
          }
        />
      </div>

      <div className="grid grid-cols-[1.6fr_1fr] gap-3.5 mb-3.5">
        <Card>
          <CardHead
            title="每日费用趋势"
            right={
              <span className="flex gap-0.5">
                {['近 14 天', '近 90 天'].map((t, i) => (
                  <span
                    key={t}
                    className={`text-[11px] px-2 py-0.5 rounded-full border cursor-pointer ${
                      i === 0
                        ? 'bg-accent text-white border-accent'
                        : 'text-inkdim border-line'
                    }`}
                  >
                    {t}
                  </span>
                ))}
              </span>
            }
          />
          <BarChart days={u.days.slice(0, 14).map((d) => ({ date: d.date, cost: d.cost }))} />
        </Card>

        <Card>
          <CardHead
            title="按调用类型占比"
            right={
              <div className="flex gap-2">
                {(['all', 'recent'] as const).map((m) => (
                  <span
                    key={m}
                    onClick={() => setKindMode(m)}
                    className={`text-[11px] px-2 py-0.5 rounded-full border cursor-pointer ${
                      kindMode === m
                        ? 'bg-accent text-white border-accent'
                        : 'text-inkdim border-line'
                    }`}
                  >
                    {m === 'all' ? '全部' : '近 7 天'}
                  </span>
                ))}
              </div>
            }
          />
          <div className="flex h-5 rounded overflow-hidden mb-3.5 bg-paper2">
            {kindProps.map((k) => (
              <div
                key={k.kind}
                className="cursor-pointer transition-[filter] hover:brightness-95"
                style={{
                  width: `${k.share * 100}%`,
                  background: KIND_COLORS[k.kind] ?? '#9d9d95',
                }}
                title={`${k.kind} ${(k.share * 100).toFixed(0)}%`}
              />
            ))}
          </div>
          <div className="flex flex-col gap-2">
            {kindProps.map((k) => (
              <div key={k.kind} className="flex items-center gap-2 text-[13px]">
                <span
                  className="w-2.5 h-2.5 rounded-sm shrink-0"
                  style={{ background: KIND_COLORS[k.kind] ?? '#9d9d95' }}
                />
                <span className="text-inkdim flex-1">{k.kind}</span>
                <span className="text-inkdim w-9 text-right">
                  {(k.share * 100).toFixed(0)}%
                </span>
                <span className="text-muted mono text-xs w-16 text-right">
                  {yuan(k.cost)}
                </span>
              </div>
            ))}
          </div>
        </Card>
      </div>

      <div className="grid grid-cols-[1.1fr_1fr] gap-3.5">
        <Card>
          <CardHead title="按模型明细" right={<span className="hint">按费用降序</span>} />
          <table className="w-full border-collapse text-[13px]">
            <thead>
              <tr className="text-left text-muted font-medium bg-paper2 text-xs">
                <th className="px-2 py-1.5 rounded-l">模型</th>
                <th className="px-2 py-1.5 text-right">调用</th>
                <th className="px-2 py-1.5 text-right">费用</th>
              </tr>
            </thead>
            <tbody>
              {Object.entries(u.by_model)
                .sort((a, b) => b[1].cost - a[1].cost)
                .map(([model, v]) => (
                  <tr key={model} className="border-b border-line last:border-0">
                    <td className="px-2 py-2 text-ink mono text-xs">{model}</td>
                    <td className="px-2 py-2 text-right text-inkdim mono text-xs">
                      {num(v.calls)}
                    </td>
                    <td className="px-2 py-2 text-right text-gold mono text-xs font-semibold">
                      {yuan(v.cost)}
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        </Card>

        <Card>
          <CardHead
            title="最近会话"
            right={<Link to="/sessions" className="text-xs text-linkblue hover:underline">查看全部 →</Link>}
          />
          <div className="flex flex-col gap-0.5">
            {recentSessions.map((s) => (
              <Link
                key={s.id}
                to={`/sessions/${s.id}`}
                className="flex items-center gap-2.5 px-2 py-2 rounded hover:bg-paper"
              >
                <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${s.message_count > 0 ? 'bg-accent' : 'bg-line2'}`} />
                <span className="flex-1 min-w-0">
                  <span className="block text-[13px] font-semibold text-ink truncate">
                    {s.title || '(无标题)'}
                  </span>
                  <span className="block text-[11px] text-muted">
                    {s.updated_at.slice(0, 16).replace('T', ' ')} · {num(s.message_count)} msgs
                    {s.tool_calls > 0 && ` · ${num(s.tool_calls)} tools`}
                  </span>
                </span>
                <span className="text-xs font-semibold text-gold mono">
                  {yuan(s.cost)}
                </span>
                <span className="text-muted">›</span>
              </Link>
            ))}
          </div>
        </Card>
      </div>
    </div>
  )
}