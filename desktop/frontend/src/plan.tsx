// The plan panel: the footer chip's popover — the plan this session is working from, the
// other plans it has, the plan's own prose, and the file behind it.
//
// It hangs off its trigger like the MCP and working-directory panels instead of covering
// the conversation: a plan is something you consult WHILE the agent works ("which step is
// it on now?"), and a modal would hide the very transcript you are following.
//
// Data comes from the plan files (AgentService.GetPlan), not from the transcript: SavePlan
// writes one file per plan, so the files are the documents — which is what makes the panel
// survive a restart, show plans saved by another frontend, and list a session's earlier
// plans at all.

import { memo, useEffect, useRef, useState } from 'react'
import { AgentService, type PlanEntryVO, type PlanVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { FilePreviewOverlay } from './filepreview'
import { MarkdownBlock } from './markdown'

// STEP_META is the three statuses SavePlan accepts, in the order they take over a step's
// life. The glyph carries the meaning; the colour only reinforces it.
const STEP_META: Record<string, { icon: string; label: string }> = {
  completed: { icon: '✓', label: '已完成' },
  in_progress: { icon: '▶', label: '进行中' },
  pending: { icon: '○', label: '待办' },
}

// planProgress counts a plan's steps, for the chip's "3/7" and the panel's header.
function planProgress(plan: PlanVO | null): { done: number; total: number } {
  const steps = plan?.steps || []
  return { done: steps.filter((s) => s.status === 'completed').length, total: steps.length }
}

// planWhen renders a plan's mtime the way a list needs it: short, and unambiguous between
// today and an older day.
function planWhen(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const sameDay = d.toDateString() === new Date().toDateString()
  return d.toLocaleString('zh-CN', sameDay
    ? { hour12: false, hour: '2-digit', minute: '2-digit' }
    : { hour12: false, month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

// PlanChip is the footer trigger. It only exists when the session has a plan, and it
// carries the progress so the usual question ("how far along?") needs no click.
export const PlanChip = memo(function PlanChip({ plan, open, onToggle }: {
  plan: PlanVO | null
  open: boolean
  onToggle: () => void
}) {
  const { done, total } = planProgress(plan)
  return (
    <button type="button" className="plan-chip" aria-expanded={open}
      title={`本会话的计划：${plan?.title || ''}（点击查看）`} onClick={onToggle}>
      <span className="plan-ico">☑</span>
      <span className="plan-chip-label">计划</span>
      {total > 0 ? <span className="plan-chip-count">{done}/{total}</span> : null}
    </button>
  )
})

export const PlanPanel = memo(function PlanPanel({ plan, workDir, mode, onSelect, onDelete, onSwitchMode, onClose }: {
  plan: PlanVO | null
  workDir: string
  // The session's mode. In plan mode the panel is also the way OUT of it: a plan you
  // have just read is a plan you want executed, and that means auto.
  mode?: string
  // Switch which plan is shown. The list is already in the payload; App re-reads the
  // chosen file so the panel never renders a plan it has not loaded.
  onSelect?: (path: string) => void
  onDelete?: (path: string) => void
  onSwitchMode?: (mode: string) => void
  onClose: () => void
}) {
  const boxRef = useRef<HTMLDivElement>(null)
  const [peek, setPeek] = useState(false)
  const [showDoc, setShowDoc] = useState(false)
  const [confirm, setConfirm] = useState<PlanEntryVO | null>(null)

  // Same dismissal contract as the MCP / roots panels: click outside, or Esc. The chip
  // that owns open/close is excluded, or closing here would fight its own click. The
  // delete confirmation is part of the panel, so Esc closes it first.
  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || boxRef.current?.contains(t) || t.closest('.plan-chip')) return
      onClose()
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      if (confirm) { setConfirm(null); return }
      onClose()
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [onClose, confirm])

  const steps = plan?.steps || []
  const { done, total } = planProgress(plan)
  const plans = plan?.plans || []
  // Captured outside the JSX so the callbacks below see a narrowed string (a `?.` in a
  // render branch does not narrow inside a closure).
  const path = plan?.path || ''

  return (
    <div className="popover-panel plan-panel" ref={boxRef}>
      <div className="plan-head">
        <span className="plan-ico">☑</span>
        <span className="plan-title" title={plan?.title}>{plan?.title || '计划'}</span>
        {total > 0 ? <span className="plan-count">{done}/{total}</span> : null}
      </div>
      {/* The list only appears when there is something to choose: a session accumulates
          plans (one file each), and before this they were invisible — the panel showed the
          newest and mentioned the rest only as a number. */}
      {plans.length > 1 ? (
        <div className="plan-list">
          {plans.map((p) => (
            <div key={p.path} className={`plan-item${p.path === path ? ' is-current' : ''}`}>
              <button type="button" className="plan-item-main" onClick={() => onSelect?.(p.path)}
                title={p.path}>
                <span className="plan-item-title">{p.title}</span>
                <span className="plan-item-meta">{p.done}/{p.total}{planWhen(p.updatedAt) ? ' · ' + planWhen(p.updatedAt) : ''}</span>
              </button>
              {onDelete ? (
                <button type="button" className="plan-item-del" title="删除这份计划"
                  onClick={() => setConfirm(p)}>✕</button>
              ) : null}
            </div>
          ))}
        </div>
      ) : null}
      {plan?.note ? <div className="plan-note">{plan.note}</div> : null}
      {steps.length > 0 ? (
        <ol className="plan-steps">
          {steps.map((s, i) => {
            const meta = STEP_META[s.status] || STEP_META.pending
            return (
              <li key={i} className={`plan-step is-${s.status}`} title={meta.label}>
                <span className="plan-step-ico">{meta.icon}</span>
                <span className="plan-step-text">{s.content}</span>
              </li>
            )
          })}
        </ol>
      ) : null}
      {/* The prose is a click away: the steps are what you check while working, and the
          plan document is what you read when you come back to it. */}
      {plan?.content ? (
        <div className="plan-doc-wrap">
          <button type="button" className="plan-doc-toggle" onClick={() => setShowDoc((v) => !v)}>
            {showDoc ? '收起正文' : '查看正文'}
          </button>
          {showDoc ? <div className="plan-doc"><MarkdownBlock text={plan.content} workDir={workDir} /></div> : null}
        </div>
      ) : null}
      {path ? (
        <div className="plan-foot">
          <span className="plan-path" title={path}><bdi>{path}</bdi></span>
          <button type="button" className="plan-btn" onClick={() => setPeek(true)}>预览</button>
          <button type="button" className="plan-btn"
            onClick={() => { AgentService.OpenPath(path).catch(() => {}) }}>打开</button>
        </div>
      ) : null}
      {/* In plan mode this panel is also the way out: reading the plan is the point at
          which you want it executed, and executing means leaving plan mode. */}
      {mode === 'plan' && onSwitchMode ? (
        <button type="button" className="plan-start" onClick={() => onSwitchMode('auto')}>
          开始执行（切到 auto 模式）
        </button>
      ) : null}
      {peek && path ? (
        <FilePreviewOverlay path={path} name={path.split('/').pop()} onClose={() => setPeek(false)} />
      ) : null}
      {confirm ? (
        <div className="confirm-overlay" onClick={() => setConfirm(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">删除计划「{confirm.title}」？</div>
            <div className="confirm-sub">文件会被删除，不可恢复：{confirm.path.split('/').pop()}</div>
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setConfirm(null)}>取消</button>
              <button className="btn danger"
                onClick={() => { const p = confirm.path; setConfirm(null); onDelete?.(p) }}>删除</button>
            </div>
          </div>
        </div>
      ) : null}
    </div>
  )
})
