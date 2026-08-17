import { api } from '../api/client'
import type { AuthReporter } from '../App'
import { useApi } from '../lib/useApi'
import { compact, shortTime } from '../lib/format'
import { Card, CardHead, Empty, PageHead } from '../components/ui'
import type { OneOffSummary } from '../types/api'

const KIND_BADGE: Record<string, 'green' | 'blue' | 'yellow' | 'purple' | 'gray'> = {
  commit: 'green',
  review: 'purple',
  ambient: 'yellow',
  dream: 'gold' as 'gray',
}

function kindBadge(kind: string) {
  if (kind === 'dream') return 'gold' as const
  return (KIND_BADGE[kind] ?? 'gray') as 'green' | 'blue' | 'yellow' | 'purple' | 'gray'
}

function OneOffTable({ items, kind }: { items: OneOffSummary[]; kind: string }) {
  return (
    <Card>
      <CardHead
        title={<span className="font-semibold">{kind}</span>}
        right={
          <span className={`badge ${kindBadge(kind)}`}>
            {items.length} 条
          </span>
        }
      />
      <table className="w-full border-collapse text-[13px]">
        <thead>
          <tr className="text-left text-muted font-medium bg-paper2 text-xs">
            <th className="px-2 py-1.5 rounded-l">文件</th>
            <th className="px-2 py-1.5">时间</th>
            <th className="px-2 py-1.5 text-right rounded-r">事件</th>
          </tr>
        </thead>
        <tbody>
          {items.map((o) => (
            <tr key={o.file} className="border-b border-line last:border-0">
              <td className="px-2 py-2 mono text-xs text-inkdim">{o.file}</td>
              <td className="px-2 py-2 mono text-xs text-inkdim">
                {o.started_at ? shortTime(o.started_at) : '-'}
              </td>
              <td className="px-2 py-2 text-right mono text-xs text-muted">
                {o.event_count}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  )
}

export function OneOffs({ onAuthError }: { onAuthError: AuthReporter }) {
  const { data, loading, error, reload } = useApi(
    () => api.globalOneOffs(),
    [],
    onAuthError,
  )

  if (loading) return <PageHead title="OneOffs" sub="加载中…" />
  if (error) {
    return (
      <div className="p-6">
        <PageHead title="OneOffs" />
        <div className="text-sm text-danger">{error}</div>
        <button className="btn mt-3" onClick={reload}>重试</button>
      </div>
    )
  }
  const kinds = data!.kinds
  const entries = Object.entries(kinds).filter(([, v]) => v.length > 0)
  const total = entries.reduce((acc, [, v]) => acc + v.reduce((a, o) => a + o.event_count, 0), 0)

  return (
    <div className="max-w-[1200px] mx-auto p-5 pb-10">
      <PageHead
        title="OneOffs"
        sub={`旁路执行记录 · ${entries.length} 个 kind · ${compact(total)} 事件 · 来源 ~/.tachi/oneoff/<kind>/`}
      />
      {entries.length === 0 ? (
        <Empty icon="🧩" title="暂无旁路记录" hint="侧信道调用（/commit、/review、ambient、dream）会记录在这里" />
      ) : (
        <div className="grid grid-cols-2 gap-3.5">
          {entries.map(([kind, items]) => (
            <OneOffTable key={kind} kind={kind} items={items} />
          ))}
        </div>
      )}
    </div>
  )
}