import { api } from '../api/client'
import type { AuthReporter } from '../App'
import { useApi } from '../lib/useApi'
import { compact, num, yuan } from '../lib/format'
import { Card, CardHead, PageHead } from '../components/ui'

export function Usage({ onAuthError }: { onAuthError: AuthReporter }) {
  const { data, loading, error, reload } = useApi(
    () => api.usage(),
    [],
    onAuthError,
  )

  if (loading) return <PageHead title="Usage 明细" sub="加载中…" />
  if (error) {
    return (
      <div className="p-6">
        <PageHead title="Usage 明细" />
        <div className="text-sm text-danger">{error}</div>
        <button className="btn mt-3" onClick={reload}>重试</button>
      </div>
    )
  }
  const u = data!

  return (
    <div className="max-w-[1200px] mx-auto p-5 pb-10">
      <PageHead
        title="Usage 明细"
        sub="费用账本 · 按日分片 JSONL · 永久保留"
      />
      <Card className="px-3 py-2">
        <CardHead
          title="每日账本"
          right={
            <span className="text-[11px] text-muted">
              来源 ~/.tachi/usage/YYYY-MM-DD.jsonl
            </span>
          }
        />
        <table className="w-full border-collapse text-[13px]">
          <thead>
            <tr className="text-left text-muted font-medium bg-paper2 text-xs">
              <th className="px-2 py-2 rounded-l">日期</th>
              <th className="px-2 py-2 text-right">调用</th>
              <th className="px-2 py-2 text-right">输入 tok</th>
              <th className="px-2 py-2 text-right">输出 tok</th>
              <th className="px-2 py-2 text-right">缓存读 tok</th>
              <th className="px-2 py-2 text-right">未计价</th>
              <th className="px-2 py-2 text-right rounded-r">费用</th>
            </tr>
          </thead>
          <tbody>
            {u.days.map((d) => (
              <tr key={d.date} className="border-b border-line last:border-0">
                <td className="px-2 py-2 mono text-xs text-ink font-medium">{d.date}</td>
                <td className="px-2 py-2 text-right mono text-xs text-inkdim">{num(d.calls)}</td>
                <td className="px-2 py-2 text-right mono text-xs text-inkdim">{d.input ?? '-'}</td>
                <td className="px-2 py-2 text-right mono text-xs text-inkdim">{d.output ?? '-'}</td>
                <td className="px-2 py-2 text-right mono text-xs text-inkdim">{d.cache ?? '-'}</td>
                <td className="px-2 py-2 text-right mono text-xs text-inkdim">{num(d.unpriced)}</td>
                <td className="px-2 py-2 text-right mono text-xs text-gold font-semibold">{yuan(d.cost)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>

      <div className="grid grid-cols-2 gap-3.5 mt-3.5">
        <Card>
          <CardHead title="按调用类型" right={<span className="hint">全部时间</span>} />
          <table className="w-full border-collapse text-[13px]">
            <thead>
              <tr className="text-left text-muted font-medium bg-paper2 text-xs">
                <th className="px-2 py-1.5 rounded-l">kind</th>
                <th className="px-2 py-1.5 text-right">调用</th>
                <th className="px-2 py-1.5 text-right rounded-r">费用</th>
              </tr>
            </thead>
            <tbody>
              {Object.entries(u.by_kind)
                .sort((a, b) => b[1].cost - a[1].cost)
                .map(([kind, v]) => (
                  <tr key={kind} className="border-b border-line last:border-0">
                    <td className="px-2 py-2 text-ink">{kind}</td>
                    <td className="px-2 py-2 text-right mono text-xs text-inkdim">{num(v.calls)}</td>
                    <td className="px-2 py-2 text-right mono text-xs text-gold font-semibold">{yuan(v.cost)}</td>
                  </tr>
                ))}
            </tbody>
          </table>
        </Card>
        <Card>
          <CardHead title="按模型" right={<span className="hint">全部时间</span>} />
          <table className="w-full border-collapse text-[13px]">
            <thead>
              <tr className="text-left text-muted font-medium bg-paper2 text-xs">
                <th className="px-2 py-1.5 rounded-l">模型</th>
                <th className="px-2 py-1.5 text-right">调用</th>
                <th className="px-2 py-1.5 text-right rounded-r">费用</th>
              </tr>
            </thead>
            <tbody>
              {Object.entries(u.by_model)
                .sort((a, b) => b[1].cost - a[1].cost)
                .map(([model, v]) => (
                  <tr key={model} className="border-b border-line last:border-0">
                    <td className="px-2 py-2 mono text-xs text-ink">{model}</td>
                    <td className="px-2 py-2 text-right mono text-xs text-inkdim">{num(v.calls)}</td>
                    <td className="px-2 py-2 text-right mono text-xs text-gold font-semibold">{yuan(v.cost)}</td>
                  </tr>
                ))}
            </tbody>
          </table>
        </Card>
      </div>

      <div className="mt-4 text-xs text-muted">
        总余额估算：<span className="mono text-gold">{yuan(u.total_cost)}</span>（
        {compact(u.total_calls)} 次调用）
      </div>
    </div>
  )
}