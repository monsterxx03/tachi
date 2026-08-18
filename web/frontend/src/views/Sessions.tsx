import { Link, useNavigate } from 'react-router-dom'

import type { AuthReporter } from '../App'
import { Badge, Card, Loading, PageHead } from '../components/ui'
import { num, shortTime, yuan } from '../lib/format'
import { SESSION_PAGE_SIZE, useSessionList } from '../lib/useSessionList'

export function Sessions({ onAuthError }: { onAuthError: AuthReporter }) {
  const navigate = useNavigate()
  const {
    sessions,
    total,
    loading,
    loadingMore,
    error,
    loadMoreError,
    hasMore,
    reload,
    loadMore,
    containerRef,
    sentinelRef,
  } = useSessionList(SESSION_PAGE_SIZE, onAuthError)

  if (loading) return <Loading text="加载会话列表…" />
  if (error) {
    return (
      <div className="p-6">
        <PageHead title="Sessions" />
        <div className="text-sm text-danger">{error}</div>
        <button className="btn mt-3" onClick={reload}>重试</button>
      </div>
    )
  }

  return (
    <div ref={containerRef} className="h-full overflow-y-auto">
      <div className="max-w-[1200px] mx-auto p-5 pb-10">
        <PageHead
          title="Sessions"
          sub={`全部会话 · 共 ${total} 个 · 按创建时间倒序（滚动加载）`}
        />
        <Card className="px-3 py-2">
          <table className="w-full border-collapse text-[13px]">
            <thead>
              <tr className="text-left text-muted font-medium bg-paper2 text-xs">
                <th className="px-2 py-2 rounded-l">标题</th>
                <th className="px-2 py-2">ID</th>
                <th className="px-2 py-2">Provider</th>
                <th className="px-2 py-2">模式</th>
                <th className="px-2 py-2 text-right">消息</th>
                <th className="px-2 py-2 text-right">工具</th>
                <th className="px-2 py-2 text-right">OneOffs</th>
                <th className="px-2 py-2">更新</th>
                <th className="px-2 py-2 text-right rounded-r">费用</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr
                  key={s.id}
                  className="border-b border-line last:border-0 hover:bg-paper cursor-pointer"
                  onClick={() => navigate(`/sessions/${s.id}`)}
                >
                  <td className="px-2 py-2 text-ink font-medium max-w-[260px] truncate">
                    {s.title || '(无标题)'}
                  </td>
                  <td className="px-2 py-2 mono text-xs text-inkdim">
                    {s.id.slice(0, 19)}…
                  </td>
                  <td className="px-2 py-2 mono text-xs text-inkdim">{s.provider ?? '-'}</td>
                  <td className="px-2 py-2">
                    <Badge color="green">{s.mode || 'auto'}</Badge>
                  </td>
                  <td className="px-2 py-2 text-right mono text-xs text-inkdim">
                    {num(s.message_count)}
                  </td>
                  <td className="px-2 py-2 text-right mono text-xs text-inkdim">
                    {num(s.tool_calls)}
                  </td>
                  <td className="px-2 py-2 text-right mono text-xs text-inkdim">
                    {num(s.oneoff_count)}
                  </td>
                  <td className="px-2 py-2 mono text-xs text-inkdim">
                    {shortTime(s.updated_at)}
                  </td>
                  <td className="px-2 py-2 text-right mono text-xs text-gold font-semibold">
                    {yuan(s.cost)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {sessions.length === 0 && (
            <div className="py-10 text-center text-muted text-sm">暂无会话</div>
          )}
        </Card>

        {/* 滚动加载哨兵：进入视口时自动加载下一页 */}
        <div ref={sentinelRef} />
        {loadingMore && (
          <div className="py-4 text-center text-muted text-xs">加载更多…</div>
        )}
        {loadMoreError && (
          <div className="py-4 text-center text-xs">
            <span className="text-danger">加载失败：{loadMoreError}</span>{' '}
            <button className="btn" onClick={loadMore}>重试</button>
          </div>
        )}
        {!hasMore && sessions.length > 0 && (
          <div className="mt-3 text-center text-xs text-muted">
            已加载全部 {total} 个会话
          </div>
        )}

        <div className="mt-3 text-xs text-muted">
          点击行进入{' '}
          <Link to="/" className="text-linkblue hover:underline">
            会话详情
          </Link>
        </div>
      </div>
    </div>
  )
}
