import { useEffect, useMemo, useRef, useState } from 'react'
import type { ProjectVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { latestSessionOf, projectMatches } from './lib'
import type { SessionItem } from './types'

// ProjectPalette is ⌘P: type to filter the desktop's projects, Enter to open the chosen
// one. "Open" is the only decision it makes — switch to that project's most recent
// conversation, or make the first one — and it makes it by delegating: `onPick` returns the
// refusal REASON (or "" when the choice was handled), so the rule about what a project can
// be opened into lives in App.tsx with the two actions it picks between.
//
// Every row therefore says what Enter will do before it is pressed: the title of the
// conversation that will open, 「新建会话」 for a project with none, or the reason it can do
// neither. A palette that only listed names would make Enter a guess, and the guess would be
// wrong exactly where it matters — a project whose directory is gone, where the backend
// would otherwise create an UNBOUND session (NewSession falls back to the last used
// workspace when the project cannot drive, and nothing says so).
export function ProjectPalette({ projects, sessions, onPick, onClose }: {
  projects: ProjectVO[]
  sessions: SessionItem[]
  // onPick handles the chosen project. It returns "" when the palette should close, or the
  // reason to show IN PLACE of closing (a refusal must not look like success).
  onPick: (p: ProjectVO) => string
  onClose: () => void
}) {
  const [query, setQuery] = useState('')
  const [idx, setIdx] = useState(0)
  const [notice, setNotice] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)

  const rows = useMemo(() => projects.filter((p) => projectMatches(p, query)), [projects, query])

  // Focus the box on mount: moving this dialog with the keyboard without a text field in
  // front of it would make typing filter nothing.
  useEffect(() => { inputRef.current?.focus() }, [])

  // The highlight follows the query rather than the list: a re-filter that left the index
  // past the end would make Enter a no-op.
  useEffect(() => { setIdx(0); setNotice('') }, [query])

  // Keep the highlighted row visible. `block: 'nearest'` so a row already on screen does not
  // jump to the middle, which at three projects would read as the list flickering.
  useEffect(() => {
    const el = listRef.current?.querySelectorAll('.palette-row')[idx] as HTMLElement | undefined
    el?.scrollIntoView({ block: 'nearest' })
  }, [idx, rows.length])

  // pick hands the choice to the parent and closes only when it says it acted. A refusal
  // (the project cannot be opened into anything right now) is shown where the reader is
  // looking instead: dismissing the box on it would read as "done", and the next ⌘P would
  // present the same project with the same problem.
  const pick = (p: ProjectVO) => {
    const reason = onPick(p)
    if (reason) setNotice(reason)
    else onClose()
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (rows.length === 0) return
      const step = e.key === 'ArrowDown' ? 1 : -1
      setIdx((i) => (i + step + rows.length) % rows.length)
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      const p = rows[Math.min(idx, rows.length - 1)]
      if (p) pick(p)
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
    }
  }

  return (
    <div className="palette-overlay" onClick={onClose}>
      <div className="palette-box" role="dialog" aria-label="项目搜索" onClick={(e) => e.stopPropagation()}>
        <input ref={inputRef} className="palette-input" value={query} placeholder="搜索项目（名字或目录）"
          spellCheck={false} onChange={(e) => setQuery(e.target.value)} onKeyDown={onKeyDown} />
        <div className="palette-list" ref={listRef} role="listbox" aria-label="项目">
          {rows.length === 0 ? (
            <div className="palette-empty">没有匹配的项目</div>
          ) : rows.map((p, i) => {
            const latest = latestSessionOf(sessions, p.id)
            const count = sessions.filter((s) => s.projectId === p.id).length
            // What Enter does, in the row itself. The three cases are mutually exclusive and
            // exhaustive, and the third is the one the backend cannot say out loud: an
            // unusable project refuses to CREATE (a session made here would land unbound).
            const action = latest
              ? `→ ${latest.title || '未命名会话'}`
              : p.rootsUsable ? '→ 新建会话' : '主目录不可用，无法新建会话'
            return (
              <div key={p.id} className={`palette-row${i === idx ? ' is-sel' : ''}`}
                role="option" aria-selected={i === idx}
                onMouseMove={() => setIdx(i)}
                onClick={() => pick(p)}>
                <div className="palette-main">
                  <div className="palette-name">{p.name}</div>
                  <div className="palette-dir" title={p.workingDir}>{p.workingDir}</div>
                </div>
                <div className="palette-side">
                  <div className="palette-count">{count > 0 ? `${count} 个会话` : '还没有会话'}</div>
                  <div className={`palette-action${!latest && !p.rootsUsable ? ' is-blocked' : ''}`}>{action}</div>
                </div>
              </div>
            )
          })}
        </div>
        {notice ? <div className="palette-notice">{notice}</div> : null}
        <div className="palette-hint">↑↓ 选择 · Enter 打开（该项目最近的会话，没有就新建）· Esc 关闭</div>
      </div>
    </div>
  )
}
