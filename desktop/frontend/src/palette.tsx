import { useEffect, useMemo, useRef, useState } from 'react'
import type { ProjectVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { fmtTime, imeActive, latestSessionOf, listNav, projectMatches, sessionChoices, sessionMatches } from './lib'
import type { SessionItem } from './types'

// usePaletteInput owns a palette's filter box: what it HOLDS and what the LIST filters by are two
// values while a Chinese IME is composing, and keeping them apart is the whole point.
//
// React updates a controlled input on every keystroke — including the pinyin a composition is
// still spelling — so a list filtered by `e.target.value` re-filters (and re-highlights) on 你/ni/
// nihao while the reader is still typing, moving the row under the cursor on every candidate key.
// The two keys that end a composition are IME-owned too: Enter commits the candidate and Escape
// cancels it, so either one would open a conversation or close the box on its way past. So: the box
// holds the raw value (never rewritten mid-composition — writing into a composing field tears the
// candidate window), the list filters by the COMMITTED query only, and the keydown handlers return
// early while the composition is live (`composingRef.current || imeActive(e)`).
//
// The ref is cleared on the next macrotask for the reason the composer's own box gives: some WebKit
// builds fire compositionend BEFORE the keydown that commits the candidate, and that Enter belongs
// to the IME — while a later, genuine Enter must still be seen.
function usePaletteInput() {
  const [value, setValue] = useState('')
  const [committed, setCommitted] = useState('')
  const composingRef = useRef(false)
  return {
    value,
    committed,
    composingRef,
    inputProps: {
      value,
      spellCheck: false,
      onChange: (e: React.ChangeEvent<HTMLInputElement>) => {
        setValue(e.target.value)
        // Raw keystrokes filter as they are typed; a composition's do not until it lands.
        if (!composingRef.current) setCommitted(e.target.value)
      },
      onCompositionStart: () => { composingRef.current = true },
      onCompositionEnd: (e: React.CompositionEvent<HTMLInputElement>) => {
        // The composition has landed: this is the first moment the list may react to it.
        setCommitted(e.currentTarget.value)
        window.setTimeout(() => { composingRef.current = false }, 0)
      },
    },
  }
}

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
  const [idx, setIdx] = useState(0)
  const [notice, setNotice] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const { committed: query, composingRef, inputProps } = usePaletteInput()

  const rows = useMemo(() => projects.filter((p) => projectMatches(p, query)), [projects, query])

  // Focus the box on mount: moving this dialog with the keyboard without a text field in
  // front of it would make typing filter nothing.
  useEffect(() => { inputRef.current?.focus() }, [])

  // The highlight follows the query rather than the list: a re-filter that left the index
  // past the end would make Enter a no-op. It follows the COMMITTED query (see usePaletteInput),
  // so an IME composition does not walk it either.
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

  // ↑↓ and Ctrl+N/Ctrl+P both walk the list (`listNav` — the same pair the composer's pickers
  // take, matching the TUI's keymap). Navigation WRAPS here, unlike those pickers: the palette is
  // a switcher, so ↑ off the first row landing on the last is what a reader expects.
  const onKeyDown = (e: React.KeyboardEvent) => {
    // A composing IME owns these keys first: Enter commits its candidate and Escape cancels it,
    // and ↑↓ / Ctrl+N·P walk the candidate list.
    if (composingRef.current || imeActive(e)) return
    const delta = listNav(e)
    if (delta) {
      e.preventDefault()
      if (rows.length === 0) return
      setIdx((i) => (i + delta + rows.length) % rows.length)
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
        <input ref={inputRef} className="palette-input" {...inputProps}
          placeholder="搜索项目（名字或目录）" onKeyDown={onKeyDown} />
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
                  <div className="palette-path" title={p.workingDir}>{p.workingDir}</div>
                </div>
                <div className="palette-side">
                  <div className="palette-meta">{count > 0 ? `${count} 个会话` : '还没有会话'}</div>
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

// SessionPalette is ⌘K: type to filter the desktop's conversations by title, Enter to open the one
// chosen. The sibling of the ⌘P palette above and deliberately the same box — the same input
// behaviour (usePaletteInput), the same ↑↓ / Ctrl+N·P navigation (`listNav`), the same Escape —
// because the two are one gesture apart, and a reader who has used one has learned the other.
//
// Three things it decides for itself:
//   • which rows there are — one per CONVERSATION (`sessionChoices`: a compaction chain folds under
//     its newest link, exactly as the sidebar folds it), in the list's own order, with the query
//     matched against the TITLE only (`sessionMatches`).
//   • what a row says — the project it belongs to as a TAG (the one fact about a conversation that
//     its title cannot carry; a project the list does not know reads as 无项目, the same bucket the
//     sidebar puts such a row in) and when it was last written to, which is what makes the order
//     legible instead of arbitrary.
//   • which row is 「当前」 — the one the app is showing, marked so the list has an anchor.
//
// Unlike ⌘P there is no refusal to report: opening a conversation always works (a workspace that is
// gone shows up IN the conversation, not here), so a pick closes the box unconditionally — the
// reason string ⌘P's `onPick` returns exists for the projects that cannot be opened into anything.
export function SessionPalette({ sessions, projects, activeId, onPick, onClose }: {
  sessions: SessionItem[]
  projects: ProjectVO[]
  activeId: string
  // onPick opens the chosen conversation. It has no refusal to answer with (see above).
  onPick: (s: SessionItem) => void
  onClose: () => void
}) {
  const [idx, setIdx] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const { committed: query, composingRef, inputProps } = usePaletteInput()

  const rows = useMemo(() => sessionChoices(sessions).filter((s) => sessionMatches(s, query)), [sessions, query])

  // Focus the box on mount: moving this dialog with the keyboard without a text field in front of
  // it would make typing filter nothing.
  useEffect(() => { inputRef.current?.focus() }, [])

  // The highlight follows the query rather than the list: a re-filter that left the index past the
  // end would make Enter a no-op. It follows the COMMITTED query (see usePaletteInput), so an IME
  // composition does not walk it either.
  useEffect(() => { setIdx(0) }, [query])

  useEffect(() => {
    const el = listRef.current?.querySelectorAll('.palette-row')[idx] as HTMLElement | undefined
    el?.scrollIntoView({ block: 'nearest' })
  }, [idx, rows.length])

  // The tag says which PROJECT a conversation belongs to, and that it belongs to one AT ALL — the
  // one fact about a row that its title cannot say. Only a project that still EXISTS names a group
  // (the sidebar's own rule), so a dangling projectId reads as 无项目 rather than as a heading
  // nothing renders. A conversation with no project gets plain text and NO chrome: the tag is what
  // says 「belongs to a project」, so its absence is the distinction (see overlays.css).
  const projectTag = (s: SessionItem) => {
    const p = projects.find((x) => x.id === s.projectId)
    return p
      ? <span className="palette-proj" title={`项目：${p.name}`}>{p.name}</span>
      : <span className="palette-loose" title="不属于任何项目：它用自己的工作区快照">无项目</span>
  }

  // ↑↓ and Ctrl+N/Ctrl+P walk the list; navigation wraps, as it does in ⌘P. Enter opens the
  // highlighted conversation and Escape closes — and a composing IME owns all four (see
  // usePaletteInput).
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (composingRef.current || imeActive(e)) return
    const delta = listNav(e)
    if (delta) {
      e.preventDefault()
      if (rows.length === 0) return
      setIdx((i) => (i + delta + rows.length) % rows.length)
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      const s = rows[Math.min(idx, rows.length - 1)]
      if (s) { onPick(s); onClose() }
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
    }
  }

  return (
    <div className="palette-overlay" onClick={onClose}>
      <div className="palette-box" role="dialog" aria-label="会话搜索" onClick={(e) => e.stopPropagation()}>
        <input ref={inputRef} className="palette-input" {...inputProps}
          placeholder="搜索会话标题" onKeyDown={onKeyDown} />
        <div className="palette-list" ref={listRef} role="listbox" aria-label="会话">
          {rows.length === 0 ? (
            // The empty state names the OTHER search: a reader who typed a project (or a word from
            // the conversation rather than from its title) is one keystroke away from ⌘P, and
            // "no match" alone would leave them retyping into the same box.
            <div className="palette-empty">没有匹配的会话（⌘P 搜的是项目）</div>
          ) : rows.map((s, i) => (
            <div key={s.id} className={`palette-row${i === idx ? ' is-sel' : ''}`}
              role="option" aria-selected={i === idx}
              onMouseMove={() => setIdx(i)}
              onClick={() => { onPick(s); onClose() }}>
              <div className="palette-main">
                <div className="palette-name">{s.title || '未命名会话'}</div>
                <div className="palette-sub">{projectTag(s)}</div>
              </div>
              <div className="palette-side">
                <div className="palette-meta">{fmtTime(s.updatedAt)}</div>
                {s.id === activeId ? <div className="palette-here">当前会话</div> : null}
              </div>
            </div>
          ))}
        </div>
        <div className="palette-hint">↑↓ 选择 · Enter 打开 · Esc 关闭（按最近更新排序）</div>
      </div>
    </div>
  )
}
