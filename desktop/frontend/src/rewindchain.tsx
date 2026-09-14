// RewindChainOverlay is the session's list of rewind points, newest first.
//
// It exists because a rewind could only be started from the bubble that began the turn: on a
// long session that means scrolling back to find it. The list comes from `RewindTurns` (the
// checkpoint manifest), NOT from the transcript — the transcript holds the newest page of
// records (sessionPageSize), so the opening record of a long turn is simply not on screen, and
// "go back to a turn from an hour ago" is exactly what this surface is for.
//
// It only CHOOSES. The files to restore, the irreversible marks and the confirmation all stay
// in the card the bubble's menu also opens (openRewindCard): one rewind, one confirmation.
//
// The numbers per row come from the manifest too (`RewindTurnVO.Diff`, recorded when the turn
// ended), so opening the list runs no git at all. A preview per row would mean one `git diff`
// over every root per row, which is the cost this list is designed not to pay.

import { useEffect, useMemo, useRef, useState } from 'react'
import type { RewindTurnVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

// REWIND_CHAIN_TAIL is how many of the newest turns are listed before the rest are folded
// behind one line. The reader almost always goes back a few turns; the full list is one click
// away, not gone.
const REWIND_CHAIN_TAIL = 20

// fmtWhen renders a turn's time the way a reader compares it: a time of day for today, a date
// for anything older. The timestamp is the session's own local time string (RFC3339 was
// formatted server-side as "2006-01-02 15:04:05"), so it is parsed as local, not as UTC.
export function fmtWhen(at: string): string {
  if (!at) return ''
  const d = new Date(at.replace(' ', 'T'))
  if (Number.isNaN(d.getTime())) return at
  const now = new Date()
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  if (d.toDateString() === now.toDateString()) return hm
  const yesterday = new Date(now)
  yesterday.setDate(now.getDate() - 1)
  if (d.toDateString() === yesterday.toDateString()) return `昨天 ${hm}`
  const md = `${d.getMonth() + 1}/${d.getDate()}`
  return d.getFullYear() === now.getFullYear() ? md : `${d.getFullYear()}/${md}`
}

// chainFileState is the row's file half, in the same three states the confirmation card uses
// (see RewindPreview). `unknown` is the one that must not be quiet: it means nobody recorded
// what the workspace looked like, so a rewind to this turn moves the conversation and leaves
// every file where it is.
export type ChainFileState = 'files' | 'unchanged' | 'unknown'

export function chainFileState(t: RewindTurnVO): ChainFileState {
  if (t.diff && !t.diff.note && t.diff.files > 0) return 'files'
  if (t.noFiles && t.reason) return 'unknown'
  return 'unchanged'
}

export function RewindChainOverlay({ turns, title, blocked, onPick, onClose }: {
  // The session's checkpoints, oldest first (the order the backend lists them in).
  turns: RewindTurnVO[]
  title: string
  // blocked, when set, is why this session cannot be rewound at all (it was compacted onwards):
  // the list is then shown for reading with every row disabled, and the reason is said once at
  // the top instead of being rediscovered per click. It comes from the same call as `turns`
  // (RewindChain), so the surface never has to try a rewind to find out.
  blocked?: string
  onPick: (turn: number) => void
  onClose: () => void
}) {
  const newestFirst = useMemo(() => [...turns].reverse(), [turns])
  const [expanded, setExpanded] = useState(false)
  const shown = expanded ? newestFirst : newestFirst.slice(0, REWIND_CHAIN_TAIL)
  const hidden = newestFirst.length - shown.length
  const [sel, setSel] = useState(0)
  const bodyRef = useRef<HTMLDivElement>(null)

  // The keyboard walk is over the rows ON SCREEN (a folded row is not a target: Enter has to
  // mean "this one I can see").
  useEffect(() => { setSel(0) }, [turns, expanded])
  useEffect(() => {
    const el = bodyRef.current?.querySelector<HTMLElement>(`[data-row="${sel}"]`)
    el?.scrollIntoView({ block: 'nearest' })
  }, [sel])

  const pick = (turn: number) => { if (!blocked) onPick(turn) }
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); setSel((s) => Math.min(s + 1, shown.length - 1)) }
    else if (e.key === 'ArrowUp') { e.preventDefault(); setSel((s) => Math.max(s - 1, 0)) }
    else if (e.key === 'Enter') {
      e.preventDefault()
      const row = shown[sel]
      if (row) pick(row.turn)
    }
  }

  return (
    <div className="rewind-chain-overlay" onClick={onClose}>
      <div className="rewind-chain" role="dialog" aria-label="回退链" tabIndex={-1}
        onClick={(e) => e.stopPropagation()} onKeyDown={onKeyDown} ref={bodyRef}
        autoFocus>
        <div className="rewind-chain-head">
          <div className="rewind-chain-title">
            <b>回退链</b>
            <span className="rewind-chain-sess">
              {title} · {turns.length} 个可回退点
            </span>
            <button type="button" className="rewind-chain-close" title="关闭（Esc）" onClick={onClose}>✕</button>
          </div>
          <div className="rewind-chain-why">
            回到某一轮开始前：<b>文件</b>与<b>对话</b>一起退回那一刻；这一轮<b>之后</b>的轮次会被放弃
            （被放弃的对话留在会话目录里，不在这里列出）。
          </div>
        </div>

        {blocked ? (
          <div className="rewind-chain-banner">
            <span><b>不能回退：</b>{blocked}</span>
          </div>
        ) : null}

        <div className="rewind-chain-now"><span>现在 · 对话末尾</span></div>

        {shown.length === 0 ? (
          <div className="rewind-chain-empty">
            这个会话还没有可回退的点：检查点从「第一次会写文件的轮次」开始记。
          </div>
        ) : null}

        <div className="rewind-chain-list">
          {shown.map((t, i) => {
            const state = chainFileState(t)
            const cls = [
              'rewind-row',
              i === sel ? 'is-sel' : '',
              blocked ? 'is-blocked' : '',
            ].filter(Boolean).join(' ')
            return (
              <div key={t.turn} className={cls} data-row={i} role="button" tabIndex={-1}
                onMouseEnter={() => setSel(i)}
                onClick={() => pick(t.turn)}>
                <div className="rewind-row-when">
                  <b>第 {t.turn} 轮</b>
                  <span>{fmtWhen(t.at)}</span>
                </div>
                <div className="rewind-row-what">
                  <div className="rewind-row-prompt">{t.userText || '(没有记录提示词)'}</div>
                  <div className="rewind-row-badges">
                    {state === 'files' ? (
                      <span className="rewind-badge is-files">可还原 {t.diff!.files} 个文件</span>
                    ) : null}
                    {state === 'unchanged' ? (
                      <span className="rewind-badge is-quiet">这一轮没写文件</span>
                    ) : null}
                    {state === 'unknown' ? (
                      <span className="rewind-badge is-unknown" title={t.reason}>
                        不知道当时的状态
                      </span>
                    ) : null}
                  </div>
                  {state === 'unknown' ? (
                    <div className="rewind-row-note">选它只回退对话：不会还原任何文件。</div>
                  ) : null}
                </div>
                <div className="rewind-row-stat">
                  {t.diff && !t.diff.note && t.diff.files > 0 ? (
                    <span className="rewind-row-num">
                      🧾 {t.diff.files}
                      {t.diff.added > 0 ? <span className="diff-count is-add">+{t.diff.added}</span> : null}
                      {t.diff.removed > 0 ? <span className="diff-count is-del">−{t.diff.removed}</span> : null}
                    </span>
                  ) : (
                    <span className="rewind-row-num is-none">—</span>
                  )}
                  <button type="button" className="rewind-row-go" disabled={!!blocked}
                    onClick={(e) => { e.stopPropagation(); pick(t.turn) }}>回退到这里</button>
                </div>
              </div>
            )
          })}
        </div>

        {hidden > 0 ? (
          <button type="button" className="rewind-chain-more" onClick={() => setExpanded(true)}>
            展开更早的 {hidden} 个回退点
          </button>
        ) : null}

        <div className="rewind-chain-foot">
          <span><kbd>↑</kbd> <kbd>↓</kbd> 选择</span>
          <span><kbd>Enter</kbd> 打开确认卡片</span>
          <span><kbd>Esc</kbd> 关闭</span>
          <span className="rewind-chain-hint">数字来自检查点（不跑 git）；点开时才算一次预览</span>
        </div>
      </div>
    </div>
  )
}
