// The plan panel: the footer chip's popover, showing the newest plan this session saved —
// title, steps with their statuses, the plan's own prose, and the file behind it.
//
// It hangs off its trigger like the MCP and working-directory panels instead of covering
// the conversation: a plan is something you consult WHILE the agent works ("which step is
// it on now?"), and a modal would hide the very transcript you are following.
//
// Data comes from the plan file (AgentService.GetPlan), not from the transcript: SavePlan
// overwrites one file per plan per session, so the file is the document — and reading it
// is what makes the panel survive a restart and show plans saved by another frontend.

import { memo, useEffect, useRef, useState } from 'react'
import { AgentService, type PlanVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
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

export const PlanPanel = memo(function PlanPanel({ plan, workDir, mode, onSwitchMode, onClose }: {
  plan: PlanVO | null
  workDir: string
  // The session's mode. In plan mode the panel is also the way OUT of it: a plan you
  // have just read is a plan you want executed, and that means auto.
  mode?: string
  onSwitchMode?: (mode: string) => void
  onClose: () => void
}) {
  const boxRef = useRef<HTMLDivElement>(null)
  const [peek, setPeek] = useState(false)
  const [showDoc, setShowDoc] = useState(false)

  // Same dismissal contract as the MCP / roots panels: click outside, or Esc. The chip
  // that owns open/close is excluded, or closing here would fight its own click.
  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || boxRef.current?.contains(t) || t.closest('.plan-chip')) return
      onClose()
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [onClose])

  const steps = plan?.steps || []
  const { done, total } = planProgress(plan)
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
      {plan?.others ? (
        <div className="plan-others">这个会话还有 {plan.others} 份其它计划（同目录下的另一个标题）</div>
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
    </div>
  )
})
