// Scheduled tasks: the status-bar popover (a glance) and the management page
// (a left list + a right editor). Both are readers of the same binding surface
// in desktop/cron.go; no scheduling decision lives here.
//
// The one rule worth stating up front: a task always writes into ONE
// conversation. "在这个会话里继续" binds to the conversation that created it;
// "新建一个专属会话" creates a conversation on the first run and then behaves
// exactly like the first option. The page never shows a "new session every run"
// mode because that is not what the backend does.

import { useEffect, useMemo, useRef, useState } from 'react'
import { Dialogs } from '@wailsio/runtime'
import {
  AgentService,
  type CronJobInput,
  type CronJobVO,
  type CronPreviewVO,
  type CronTargetVO,
  type ProjectVO,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import { IS_MAC } from './platform'

// ── formatting ─────────────────────────────────────────────────────────────

const pad2 = (n: number) => String(n).padStart(2, '0')

function fmtWhen(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const days = Math.round((day(d) - day(new Date())) / 86_400_000)
  const hm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  if (days === 0) return `今天 ${hm}`
  if (days === 1) return `明天 ${hm}`
  if (days === -1) return `昨天 ${hm}`
  return `${d.getMonth() + 1}/${d.getDate()} ${hm}`
}

function fmtEta(iso?: string): string {
  if (!iso) return ''
  const ms = new Date(iso).getTime() - Date.now()
  if (!Number.isFinite(ms) || ms <= 0) return ''
  const mins = Math.round(ms / 60_000)
  if (mins < 60) return `还有 ${Math.max(1, mins)} 分钟`
  const hours = Math.round(mins / 60)
  if (hours < 48) return `还有 ${hours} 小时`
  return `还有 ${Math.round(hours / 24)} 天`
}

function targetLabel(job: CronJobVO): { text: string; cls: string; title: string } {
  if (job.targetKind === 'pending') {
    return { text: '首次运行时创建', cls: 'is-pending', title: '第一次运行会创建一个专属会话，之后一直用它' }
  }
  if (job.targetKind === 'missing') {
    return { text: '会话已删除', cls: 'is-missing', title: '目标会话已经不存在，任务已暂停' }
  }
  if (job.targetIsCurrent) {
    return { text: '此对话', cls: 'is-current', title: job.targetTitle || '当前会话' }
  }
  return { text: job.targetTitle ? `会话 · ${job.targetTitle}` : '会话', cls: '', title: job.targetTitle || '' }
}

function lastRunText(job: CronJobVO): { text: string; bad: boolean } {
  if (!job.lastRunAt) return { text: '还没跑过', bad: false }
  if (job.lastRunStatus === 'error') {
    return { text: `上次失败 · ${fmtWhen(job.lastRunAt)}`, bad: true }
  }
  return { text: `上次成功 · ${fmtWhen(job.lastRunAt)}`, bad: false }
}

function isActive(job: CronJobVO): boolean {
  return job.status === 'active'
}

// ── panel (status bar) ─────────────────────────────────────────────────────

export function CronPanel({
  jobs, loading, scheduled, notice, onClose, onNew, onEdit, onOpenPage, onTogglePaused, onRunNow,
}: {
  jobs: CronJobVO[]
  loading: boolean
  scheduled: boolean
  notice?: string
  onClose: () => void
  onNew: () => void
  onEdit: (job: CronJobVO) => void
  onOpenPage: () => void
  onTogglePaused: (job: CronJobVO) => void
  onRunNow: (job: CronJobVO) => void
}) {
  const active = jobs.filter(isActive).length
  const next = jobs
    .filter((j) => isActive(j) && j.nextRun)
    .map((j) => j.nextRun as string)
    .sort()[0]

  return (
    <div className="cron-panel popover-panel" role="dialog" aria-label="定时任务">
      <div className="cron-head">
        <div className="cron-head-text">
          <div className="cron-title">定时任务 <span className="cron-count">{jobs.length}</span></div>
          <div className="cron-sub">
            {scheduled
              ? `${active} 个启用${next ? ` · 下次 ${fmtWhen(next)}` : ''}`
              : '已在设置里关闭'}
          </div>
        </div>
        <div className="cron-head-actions">
          <button className="btn sm" onClick={onNew} disabled={!scheduled}>＋ 新建</button>
          <button className="cron-x" onClick={onClose} title="关闭" aria-label="关闭">×</button>
        </div>
      </div>

      {!scheduled && !loading ? (
        <div className="cron-off">定时任务已在设置里关闭，任务暂时不会运行。</div>
      ) : null}
      {notice ? <div className="cron-off cron-off-error">{notice}</div> : null}

      <div className="cron-list">
        {loading && jobs.length === 0 ? (
          <div className="cron-empty">正在读取…</div>
        ) : jobs.length === 0 ? (
          <div className="cron-empty">
            <div className="cron-empty-title">还没有定时任务</div>
            <div className="cron-empty-sub">比如「每天 9 点整理站会要点」，或者「每 30 分钟看一眼 CI」。</div>
          </div>
        ) : (
          jobs.map((job) => {
            const target = targetLabel(job)
            const last = lastRunText(job)
            return (
              <div className={`cron-job${isActive(job) ? '' : ' is-paused'}`} key={job.id}>
                <div className="cron-job-top">
                  <span className={`cron-dot${isActive(job) ? '' : ' is-paused'}`} />
                  <span className="cron-job-name">{job.name}</span>
                  <span className={`cron-target ${target.cls}`} title={target.title}>{target.text}</span>
                  {job.type === 'oneshot' ? <span className="cron-once">只一次</span> : null}
                  <span className="cron-grow" />
                  <span className="cron-sched">{job.scheduleText || job.schedule}</span>
                </div>
                <div className="cron-job-line">
                  {job.running ? (
                    <span className="cron-running"><span className="cron-spin" /> 运行中</span>
                  ) : isActive(job) && job.nextRun ? (
                    <>下次 <b>{fmtWhen(job.nextRun)}</b>{fmtEta(job.nextRun) ? ` · ${fmtEta(job.nextRun)}` : ''}</>
                  ) : isActive(job) ? (
                    <span className="cron-bad">{job.nextRunError || '计划不可用'}</span>
                  ) : (
                    '已暂停 · 下次不会运行'
                  )}
                </div>
                <div className="cron-job-line cron-last" title={job.lastRunError || undefined}>
                  <span className={last.bad ? 'cron-bad' : ''}>{last.text}</span>
                  {job.lastRunError ? <span className="cron-bad"> · {job.lastRunError}</span> : null}
                </div>
                <div className="cron-job-actions">
                  {scheduled ? (
                    <button className="btn ghost sm" onClick={() => onRunNow(job)} disabled={job.running}>
                      {job.running ? '运行中…' : '运行一次'}
                    </button>
                  ) : null}
                  <button className="btn ghost sm" onClick={() => onEdit(job)}>编辑</button>
                  {scheduled ? (
                    <button className="btn ghost sm" onClick={() => onTogglePaused(job)}>
                      {isActive(job) ? '暂停' : '恢复'}
                    </button>
                  ) : null}
                </div>
              </div>
            )
          })
        )}
      </div>

      <div className="cron-foot">
        <div className="cron-foot-hint">任务在 Tachi 里运行。退出应用后不会执行，也不会补跑。</div>
        <button className="cron-foot-link" onClick={onOpenPage}>打开完整页面 →</button>
      </div>
    </div>
  )
}

// ── page (full view) ───────────────────────────────────────────────────────

type ScheduleMode = 'daily' | 'weekly' | 'hourly' | 'custom'

type Draft = {
  id: string
  name: string
  mode: ScheduleMode
  time: string
  weekdays: number[] // 1..7, 7 = Sunday
  hourEvery: number
  custom: string
  timezone: string
  prompt: string
  oneshot: boolean
  notify: 'always' | 'when_relevant'
  reply: 'session' | 'new'
  sessionID: string
  projectID: string
  workingDir: string
  providerName: string
  skipApproval: boolean
}

function parseSchedule(schedule: string): Pick<Draft, 'mode' | 'time' | 'weekdays' | 'hourEvery' | 'custom'> {
  const text = schedule.trim()
  const f = text.split(/\s+/)
  if (f.length === 5) {
    const [mi, ho, dom, mon, dow] = f
    const m = /^\d+$/.test(mi) ? Number(mi) : -1
    const h = /^\d+$/.test(ho) ? Number(ho) : -1
    if (m >= 0 && h >= 0 && dom === '*' && mon === '*') {
      const time = `${pad2(h)}:${pad2(m)}`
      if (dow === '*') return { mode: 'daily', time, weekdays: [], hourEvery: 2, custom: text }
      const days = dow
        .split(',')
        .map((x) => (x === '0' || x === '7' ? 7 : Number(x)))
        .filter((n) => Number.isInteger(n) && n >= 1 && n <= 7)
      if (days.length) return { mode: 'weekly', time, weekdays: days, hourEvery: 2, custom: text }
    }
    if (mi === '0' && dom === '*' && mon === '*' && dow === '*' && /^\*\/\d+$/.test(ho)) {
      return { mode: 'hourly', time: '09:00', weekdays: [], hourEvery: Number(ho.slice(2)), custom: text }
    }
  }
  return { mode: 'custom', time: '09:00', weekdays: [], hourEvery: 2, custom: text }
}

function defaultDraft(targets: CronTargetVO[], projects: ProjectVO[], job?: CronJobVO | null): Draft {
  const current = targets.find((t) => t.isCurrent) || targets[0]
  if (!job) {
    return {
      id: '',
      name: '',
      mode: 'daily',
      time: '09:00',
      weekdays: [1, 2, 3, 4, 5],
      hourEvery: 2,
      custom: '0 9 * * *',
      timezone: '',
      prompt: '',
      oneshot: false,
      notify: 'always',
      reply: 'session',
      sessionID: current?.sessionId || '',
      projectID: current?.projectId || '',
      workingDir: current?.workingDir || '',
      providerName: current?.providerName || '',
      skipApproval: current?.skipApproval || false,
    }
  }
  const sched = parseSchedule(job.schedule)
  const bound = job.targetKind === 'session' || job.targetKind === 'missing'
  // When a bound job is switched to 新会话, the new conversation should start
  // from the TARGET's workspace/provider/mode, not from whatever conversation
  // happens to be on screen.
  const base = (job.targetSessionId && targets.find((t) => t.sessionId === job.targetSessionId)) || current
  return {
    id: job.id,
    name: job.name,
    ...sched,
    timezone: job.timezone || '',
    prompt: job.prompt,
    oneshot: job.type === 'oneshot',
    notify: job.notify === 'when_relevant' ? 'when_relevant' : 'always',
    reply: bound ? 'session' : 'new',
    // A deleted target cannot be selected; fall back to the conversation on
    // screen so the form is immediately saveable again.
    sessionID: job.targetKind === 'missing'
      ? current?.sessionId || ''
      : bound ? job.targetSessionId || base?.sessionId || '' : current?.sessionId || '',
    projectID: job.targetProjectId || base?.projectId || '',
    workingDir: job.targetWorkingDir || base?.workingDir || '',
    providerName: job.targetProvider || base?.providerName || '',
    skipApproval: job.targetSkipApproval ?? base?.skipApproval ?? false,
  }
}

function clockOf(draft: Draft): { h: number; m: number } | null {
  const m = /^(\d{1,2}):(\d{2})$/.exec(draft.time.trim())
  if (!m) return null
  const h = Number(m[1])
  const min = Number(m[2])
  if (h < 0 || h > 23 || min < 0 || min > 59) return null
  return { h, m: min }
}

function buildSchedule(draft: Draft): string {
  if (draft.mode === 'custom') return draft.custom.trim()
  const c = clockOf(draft)
  if (!c) return ''
  switch (draft.mode) {
    case 'daily':
      return `${c.m} ${c.h} * * *`
    case 'weekly': {
      if (draft.weekdays.length === 0) return ''
      const days = [...new Set(draft.weekdays)].sort((a, b) => a - b).map((d) => (d === 7 ? 0 : d))
      return `${c.m} ${c.h} * * ${days.join(',')}`
    }
    case 'hourly': {
      const n = Math.min(23, Math.max(1, Math.round(draft.hourEvery) || 1))
      return `0 */${n} * * *`
    }
  }
  return ''
}

export function CronPage({
  jobs, targets, projects, loading, scheduled, selectedId, creating, notice: actionNotice, onBack, onSelect, onNew,
  onSaved, onTogglePaused, onRunNow, onDelete,
}: {
  jobs: CronJobVO[]
  targets: CronTargetVO[]
  projects: ProjectVO[]
  loading: boolean
  scheduled: boolean
  selectedId: string | null
  creating: boolean
  notice?: string
  onBack: () => void
  onSelect: (id: string | null) => void
  onNew: () => void
  onSaved: (id: string) => void | Promise<void>
  onTogglePaused: (job: CronJobVO) => void
  onRunNow: (job: CronJobVO) => void
  onDelete: (job: CronJobVO) => void
}) {
  const selected = useMemo(
    () => (selectedId ? jobs.find((j) => j.id === selectedId) || null : null),
    [jobs, selectedId],
  )
  const [draft, setDraft] = useState<Draft>(() => defaultDraft(targets, projects, selected))
  const [preview, setPreview] = useState<CronPreviewVO | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [confirmDelete, setConfirmDelete] = useState(false)
  // Esc leaves the page, like every other full-window view. It has to live HERE
  // rather than in App's global handler: the page owns the delete confirmation,
  // and Esc must take that down first instead of leaving the page under it.
  const onBackRef = useRef(onBack)
  onBackRef.current = onBack
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      if (confirmDelete) {
        setConfirmDelete(false)
        return
      }
      onBackRef.current()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [confirmDelete])
  // The form follows the selection; editing the schedule after a run finished
  // must not be reset by the list's own refresh, so the draft only reloads when
  // the selected JOB changes (not when its last-run row changes).
  const loadedId = useRef<string | null>(null)
  useEffect(() => {
    // A save moves the selection to the job it just wrote; the draft is already
    // that job, so reloading it would only clear the "已创建" notice.
    if (saving) return
    const key = creating ? '__new__' : selected?.id || null
    if (loadedId.current === key) return
    loadedId.current = key
    setDraft(defaultDraft(targets, projects, creating ? null : selected))
    setError('')
    setNotice('')
    setConfirmDelete(false)
  }, [creating, selected, targets, projects, saving])

  const schedule = buildSchedule(draft)
  const activeTarget = targets.find((t) => t.sessionId === draft.sessionID)
  const editing = creating || !!selected

  // The page can be opened from the popover's 新建 before ListCronTargets has
  // answered. The draft then has no session; fill it from the conversation on
  // screen as soon as the list arrives, so the form is saveable without the
  // reader having to pick the conversation they are already in.
  useEffect(() => {
    if (!creating) return
    const current = targets.find((t) => t.isCurrent) || targets[0]
    if (!current) return
    setDraft((prev) => {
      if (prev.sessionID) return prev
      return {
        ...prev,
        sessionID: current.sessionId,
        projectID: prev.projectID || current.projectId || '',
        workingDir: prev.workingDir || current.workingDir || '',
        providerName: prev.providerName || current.providerName || '',
        skipApproval: current.skipApproval || false,
      }
    })
  }, [creating, targets])

  useEffect(() => {
    if (!schedule) {
      setPreview(null)
      return
    }
    let alive = true
    const timer = window.setTimeout(() => {
      AgentService.PreviewCron(schedule, draft.timezone)
        .then((p) => { if (alive) setPreview(p) })
        .catch(() => { if (alive) setPreview(null) })
    }, 180)
    return () => { alive = false; window.clearTimeout(timer) }
  }, [schedule, draft.timezone])

  const patch = (p: Partial<Draft>) => setDraft((prev) => ({ ...prev, ...p }))

  const pickDir = async () => {
    try {
      const picked: string | string[] = await Dialogs.OpenFile({
        CanChooseDirectories: true,
        CanChooseFiles: false,
        CanCreateDirectories: true,
        AllowsMultipleSelection: false,
        Title: '选择专属会话的工作目录',
        Directory: draft.workingDir || undefined,
      })
      if (typeof picked === 'string' && picked) patch({ workingDir: picked })
    } catch { /* ignore */ }
  }

  const save = async () => {
    setError('')
    setNotice('')
    if (!draft.name.trim()) { setError('任务名不能为空'); return }
    if (!draft.prompt.trim()) { setError('任务内容不能为空'); return }
    if (!schedule) { setError('请把运行时间填完整'); return }
    if (draft.reply === 'session' && !draft.sessionID) { setError('请选择一个会话'); return }
    setSaving(true)
    try {
      const input: CronJobInput = {
        id: draft.id,
        name: draft.name.trim(),
        schedule,
        prompt: draft.prompt,
        type: draft.oneshot ? 'oneshot' : 'recurring',
        notify: draft.notify,
        timezone: draft.timezone.trim(),
        mode: draft.reply,
        sessionId: draft.reply === 'session' ? draft.sessionID : '',
        projectId: draft.reply === 'new' ? draft.projectID : '',
        workingDir: draft.reply === 'new' ? draft.workingDir : '',
        additionalDirs: [],
        providerName: draft.providerName,
        skipApproval: draft.reply === 'new' ? draft.skipApproval : false,
      }
      const res = await AgentService.SaveCronJob(input)
      if (!res.startsWith('ok:')) { setError(res || '保存失败'); return }
      const savedID = res.slice(3)
      setNotice(draft.id ? '已保存' : '已创建')
      // Keep the form as it is: the selection is about to move to this job,
      // and the load effect must not reset (and clear the notice) on that move.
      loadedId.current = savedID
      setDraft((prev) => ({ ...prev, id: savedID }))
      await onSaved(savedID)
    } catch (e) {
      setError(String(e) || '保存失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="cron-view">
      <aside className="cron-rail">
        <div className={`window-strip${IS_MAC ? '' : ' drag-region'}`} aria-hidden="true" />
        <button className="cron-back" onClick={onBack} title="返回对话（Esc）">
          <span aria-hidden="true">←</span> 返回对话
        </button>
        <div className="cron-rail-head">
          <div>
            <div className="cron-rail-title">定时任务</div>
            <div className="cron-rail-sub">{jobs.length} 个任务 · {jobs.filter(isActive).length} 个启用</div>
          </div>
        </div>
        <button className="cron-new" onClick={onNew} disabled={!scheduled}>＋ 新建任务</button>
        <div className="cron-nav">
          {loading && jobs.length === 0 ? <div className="cron-nav-empty">正在读取…</div> : null}
          {!loading && jobs.length === 0 ? <div className="cron-nav-empty">还没有定时任务</div> : null}
          {jobs.map((job) => (
            <button
              key={job.id}
              className={`cron-nav-row${!creating && selected?.id === job.id ? ' is-active' : ''}${isActive(job) ? '' : ' is-paused'}`}
              onClick={() => onSelect(job.id)}
            >
              <span className={`cron-dot${isActive(job) ? '' : ' is-paused'}`} />
              <span className="cron-nav-text">
                <span className="cron-nav-name">{job.name}</span>
                <span className="cron-nav-meta">
                  {job.scheduleText || job.schedule}
                  {job.type === 'oneshot' ? ' · 只一次' : ''}
                  {job.targetKind === 'pending' ? ' · 专属会话（待创建）' : job.targetKind === 'missing' ? ' · 会话已删除' : job.targetTitle ? ` · ${job.targetTitle}` : ''}
                </span>
              </span>
            </button>
          ))}
        </div>
        <div className="cron-rail-foot">任务在 Tachi 里运行；退出应用后不会执行，也不会补跑。</div>
      </aside>

      <main className="cron-main">
        <header className="cron-toolbar">
          <div className="cron-crumb">
            定时任务 <span className="cron-sep">/</span>{' '}
            <b>{creating || !selected ? '新建任务' : selected.name}</b>
      </div>
          <span className="cron-grow" />
          {!creating && selected ? (
            <>
              <button className="btn ghost sm" onClick={() => onRunNow(selected)}
                disabled={!scheduled || selected.running}>
                {selected.running ? '运行中…' : '运行一次'}
              </button>
              <span className="cron-switch-label">{isActive(selected) ? '已启用' : '已暂停'}</span>
              <button
                className={`cron-switch${isActive(selected) ? ' is-on' : ''}`}
                onClick={() => onTogglePaused(selected)}
                disabled={!scheduled}
                title={isActive(selected) ? '暂停' : '恢复'}
                aria-label={isActive(selected) ? '暂停' : '恢复'}
              />
              <button className="btn ghost sm cron-danger" onClick={() => setConfirmDelete(true)} disabled={!scheduled}>删除</button>
            </>
          ) : null}
        </header>

        {editing ? (
        <div className="cron-form-scroll">
          <div className="cron-form">
            <div className="cron-form-head">
              <h3>{creating ? '新建任务' : '编辑任务'}</h3>
              <div className="cron-form-hint">
                定时任务沿用目标会话的审批模式：会话设成「无需审批」时，任务也不会被问；否则命中审批规则的命令会被拒绝。
              </div>
            </div>

            {actionNotice ? <div className="cron-notice is-warn">{actionNotice}</div> : null}
            {!scheduled ? (
              <div className="cron-notice is-warn">定时任务已在设置里关闭，保存和运行都不可用。</div>
            ) : null}
            {selected?.targetKind === 'missing' ? (
              <div className="cron-notice is-error">
                ⚠ 目标会话已经不存在，任务已暂停。在下面重选一个会话，或改成「新建一个专属会话」。
              </div>
            ) : null}
            {error ? <div className="cron-notice is-error">⚠ {error}</div> : null}
            {notice ? <div className="cron-notice is-ok">{notice}</div> : null}

            <div className="cron-field">
              <label className="cron-label">名称</label>
              <input className="cron-input cron-name" data-field="name" value={draft.name} maxLength={80}
                placeholder="比如：每日站会总结"
                onChange={(e) => patch({ name: e.target.value })} />
            </div>

            <div className="cron-field">
              <label className="cron-label">什么时候运行</label>
              <div className="cron-seg" role="group" aria-label="运行频率">
                {([
                  ['daily', '每天'], ['weekly', '每周'], ['hourly', '每 N 小时'], ['custom', '自定义'],
                ] as [ScheduleMode, string][]).map(([mode, label]) => (
                  <button key={mode} className={draft.mode === mode ? 'is-on' : ''}
                    onClick={() => patch({ mode })}>
                    {label}
                  </button>
                ))}
              </div>

              {draft.mode === 'custom' ? (
                <input className="cron-input cron-cron-expr" value={draft.custom}
                  placeholder="0 9 * * 1-5" onChange={(e) => patch({ custom: e.target.value })} />
              ) : draft.mode === 'hourly' ? (
                <div className="cron-sched-row">
                  <span className="cron-hint">每</span>
                  <input className="cron-input cron-num" type="number" min={1} max={23}
                    value={draft.hourEvery}
                    onChange={(e) => patch({ hourEvery: Number(e.target.value) })} />
                  <span className="cron-hint">小时</span>
                </div>
              ) : (
                <div className="cron-sched-row">
                  <span className="cron-hint">时间</span>
                  <input className="cron-input cron-time" type="time" value={draft.time}
                    onChange={(e) => patch({ time: e.target.value })} />
                </div>
              )}

              {draft.mode === 'weekly' ? (
                <div className="cron-weekdays" role="group" aria-label="星期">
                  {[1, 2, 3, 4, 5, 6, 7].map((d) => (
                    <button key={d} className={draft.weekdays.includes(d) ? 'is-on' : ''}
                      onClick={() => patch({
                        weekdays: draft.weekdays.includes(d)
                          ? draft.weekdays.filter((x) => x !== d)
                          : [...draft.weekdays, d],
                      })}>
                      {d === 7 ? '日' : '一二三四五六'[d - 1]}
                    </button>
                  ))}
                </div>
              ) : null}

              <div className="cron-preview">
                {preview?.error ? (
                  <span className="cron-bad">⚠ {preview.error}</span>
                ) : preview && preview.next && preview.next.length > 0 ? (
                  <>
                    <span className="cron-hint">下次运行</span>
                    <b>{fmtWhen(preview.next[0])}</b>
                    <span className="cron-hint">· {schedule}</span>
                    {preview.next[1] ? <span className="cron-hint">· 之后 {fmtWhen(preview.next[1])}</span> : null}
                  </>
                ) : null}
              </div>

              <details className="cron-adv">
                <summary>高级：cron 表达式与时间设置</summary>
                <div className="cron-adv-body">
                  <div className="cron-adv-row">
                    <span className="cron-hint">cron</span>
                    <input className="cron-input cron-cron-expr" value={draft.mode === 'custom' ? draft.custom : schedule}
                      placeholder="0 9 * * *"
                      onChange={(e) => patch({ mode: 'custom', custom: e.target.value })} />
                  </div>
                  <div className="cron-adv-row">
                    <span className="cron-hint">时区</span>
                    <input className="cron-input cron-tz" value={draft.timezone}
                      placeholder={`本机（${preview?.localTimezone || '系统时区'}）`}
                      list="cron-timezones"
                      onChange={(e) => patch({ timezone: e.target.value })} />
                    <datalist id="cron-timezones">
                      <option value="Asia/Shanghai" />
                      <option value="Asia/Tokyo" />
                      <option value="Asia/Singapore" />
                      <option value="UTC" />
                      <option value="Europe/London" />
                      <option value="Europe/Berlin" />
                      <option value="America/New_York" />
                      <option value="America/Los_Angeles" />
                    </datalist>
                  </div>
                </div>
              </details>

              {/* Type is a separate fact from the schedule shape. A oneshot can
                  be "每天 09:00" (it runs at the next 09:00, then deletes); the
                  old "只跑一次" preset hid that and made an existing oneshot
                  read as recurring in the editor. */}
              <label className="cron-check">
                <input type="checkbox" checked={draft.oneshot}
                  onChange={(e) => patch({ oneshot: e.target.checked })} />
                <span>只跑一次（跑完自动删除）</span>
              </label>
            </div>

            <div className="cron-field">
              <label className="cron-label">做什么</label>
              <textarea className="cron-input cron-textarea" data-field="prompt" value={draft.prompt}
                placeholder="整理今天的站会要点：已合入的 PR、待评审的改动、CI 状态，以及需要我决策的事项。没有变化就说没有变化。"
                onChange={(e) => patch({ prompt: e.target.value })} />
            </div>

            <div className="cron-field">
              <label className="cron-label">在哪儿回复</label>
              <div className="cron-choices">
                <button className={`cron-choice${draft.reply === 'session' ? ' is-on' : ''}`}
                  onClick={() => patch({ reply: 'session' })}>
                  <span className="cron-radio" />
                  <span>
                    <span className="cron-choice-title">在这个会话里继续</span>
                    <span className="cron-choice-desc">继承它的上下文。结果就出现在这段对话里。</span>
                  </span>
                </button>
                <button className={`cron-choice${draft.reply === 'new' ? ' is-on' : ''}`}
                  onClick={() => patch({ reply: 'new' })}>
                  <span className="cron-radio" />
                  <span>
                    <span className="cron-choice-title">新建一个专属会话</span>
                    <span className="cron-choice-desc">第一次运行时创建（标题是任务名），之后一直在这个会话里继续，不会每次新开。</span>
                  </span>
                </button>
              </div>

              {draft.reply === 'session' ? (
                <div className="cron-session-pick">
                  <select className="cron-input cron-select" value={draft.sessionID}
                    onChange={(e) => patch({ sessionID: e.target.value })}>
                    {targets.map((t) => (
                      <option key={t.sessionId} value={t.sessionId}>
                        {t.title}{t.isCurrent ? '（当前）' : ''}{t.projectName ? ` · ${t.projectName}` : ''}
                      </option>
                    ))}
                  </select>
                  <span className="cron-hint">
                    {activeTarget ? `审批模式：${activeTarget.skipApproval ? '无需审批' : '需要审批'}（跟随这个会话，可在会话底部切换）` : ''}
                  </span>
                </div>
              ) : (
                <div className="cron-workspace">
                  <div className="cron-form-hint">第一次运行时会用下面的位置创建这个专属会话；之后一直用它。</div>
                  <div className="cron-ws-row">
                    <span className="cron-hint cron-ws-label">项目</span>
                    <select className="cron-input cron-select" value={draft.projectID}
                      onChange={(e) => {
                        const id = e.target.value
                        const p = projects.find((x) => x.id === id)
                        patch({ projectID: id, workingDir: p ? p.workingDir : draft.workingDir })
                      }}>
                      <option value="">不绑定项目</option>
                      {projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                    </select>
                  </div>
                  {!draft.projectID ? (
                    <div className="cron-ws-row">
                      <span className="cron-hint cron-ws-label">目录</span>
                      <input className="cron-input cron-ws-path" value={draft.workingDir}
                        placeholder="不填就用默认工作区" onChange={(e) => patch({ workingDir: e.target.value })} />
                      <button className="btn sm" onClick={() => void pickDir()}>选择目录…</button>
                    </div>
                  ) : null}
                  <div className="cron-ws-row">
                    <span className="cron-hint cron-ws-label">审批</span>
                    <select className="cron-input cron-select" value={draft.skipApproval ? 'skip' : 'ask'}
                      onChange={(e) => patch({ skipApproval: e.target.value === 'skip' })}>
                      <option value="ask">需要审批</option>
                      <option value="skip">无需审批</option>
                    </select>
                    <span className="cron-hint">创建之后由这个会话自己的开关决定。</span>
                  </div>
                </div>
              )}
            </div>

            <div className="cron-field">
              <label className="cron-label">通知</label>
              <div className="cron-seg">
                <button className={draft.notify === 'always' ? 'is-on' : ''} onClick={() => patch({ notify: 'always' })}>每次都通知</button>
                <button className={draft.notify === 'when_relevant' ? 'is-on' : ''} onClick={() => patch({ notify: 'when_relevant' })}>有值得看的内容时</button>
              </div>
              <div className="cron-form-hint">选后者时，回复仍然留在会话里，只是没有值得看的内容时不弹通知。</div>
            </div>

            <div className="cron-form-actions">
              {/* 取消 leaves the editor: it does not "revert and stay" — a button
                  that only undid the fields looked like it did nothing when the
                  form had not been touched. Back to the list, where the row is. */}
              <button className="btn" onClick={() => onSelect(null)} disabled={saving}>取消</button>
              <button className="btn primary" onClick={() => void save()} disabled={saving || !scheduled}>
                {saving ? '保存中…' : '保存'}
              </button>
            </div>
          </div>
        </div>
        ) : (
          <div className="cron-placeholder">
            <div className="cron-placeholder-title">选一个任务，或者新建一个</div>
            <div className="cron-form-hint">左边的任务会按计划自动运行，结果写进它绑定的那个会话。</div>
            <button className="btn primary" onClick={onNew} disabled={!scheduled}>＋ 新建任务</button>
          </div>
        )}

        {confirmDelete && selected ? (
          <div className="cron-confirm" role="dialog" aria-label="删除任务">
            <div className="cron-confirm-box">
              <div className="cron-confirm-msg">删除任务「{selected.name}」？</div>
              <div className="cron-form-hint">任务会停止调度，已经跑出来的会话和记录不受影响。</div>
              <div className="cron-confirm-actions">
                <button className="btn" onClick={() => setConfirmDelete(false)}>取消</button>
                <button className="btn danger" onClick={() => { setConfirmDelete(false); onDelete(selected) }}>删除</button>
              </div>
            </div>
          </div>
        ) : null}
      </main>
    </div>
  )
}
