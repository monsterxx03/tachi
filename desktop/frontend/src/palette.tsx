import { useEffect, useMemo, useRef, useState } from 'react'
import type { ProjectVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { fmtTime, imeActive, latestSessionOf, listNav, looseMatches, looseMembers, newestOf, projectMatches, sessionChoices, sessionMatches, useFilterInput, LOOSE_LABEL, LOOSE_LABEL_HINT, LOOSE_GROUP_KEY } from './lib'
import { MOD_KEY } from './platform'
import type { SessionItem } from './types'

// ProjectChoice is one ⌘P row: a project, or the project-less bucket (无项目). The bucket is a row
// because it is a GROUP on the other side of the same gesture — the sidebar keeps it at the foot of
// its list, and it is where a session no live project owns lives (⌘⇧N and the sidebar's own ＋ make
// those), so a palette that could only name projects would be unable to take the reader back to one.
//
// The bucket is not a ProjectVO on purpose: it has no id, no name of its own to rename and no
// directory to drive, and a fake one would be a row the project machinery could be handed.
export type ProjectChoice = { kind: 'project'; project: ProjectVO } | { kind: 'loose' }

// ProjectPalette is ⌘P: type to filter the desktop's projects and the 无项目 bucket, Enter to open
// the chosen one. "Open" is the only decision it makes — switch to that row's most recent
// conversation, or make one — and it makes it by delegating: `onPick` returns the refusal REASON (or
// "" when the choice was handled), so the rule about what a row can be opened into lives in App.tsx
// with the two actions it picks between.
//
// Every row therefore says what Enter will do before it is pressed: the title of the
// conversation that will open, 「新建会话」 for a row with none, or the reason it can do
// neither. A palette that only listed names would make Enter a guess, and the guess would be
// wrong exactly where it matters — a project whose directory is gone, where the backend
// would otherwise create an UNBOUND session (NewSession falls back to the last used
// workspace when the project cannot drive, and nothing says so).
export function ProjectPalette({ projects, sessions, onPick, onClose }: {
  projects: ProjectVO[]
  sessions: SessionItem[]
  // onPick handles the chosen row. It returns "" when the palette should close, or the
  // reason to show IN PLACE of closing (a refusal must not look like success).
  onPick: (c: ProjectChoice) => string
  onClose: () => void
}) {
  const [idx, setIdx] = useState(0)
  const [notice, setNotice] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const { committed: query, composingRef, inputProps } = useFilterInput()

  // The bucket's members, by the SIDEBAR's own membership rule (looseMembers): the row shows how
  // many there are and which one Enter opens, so the two surfaces cannot disagree about which
  // sessions belong to it.
  const loose = useMemo(() => looseMembers(sessions, projects), [sessions, projects])

  // The bucket is LAST, as it is in the sidebar, and a query reaches it by its own name only
  // (looseMatches): typing a project's name or directory asked for that project, not for this.
  const rows = useMemo<ProjectChoice[]>(() => {
    const out: ProjectChoice[] = projects
      .filter((p) => projectMatches(p, query))
      .map((p) => ({ kind: 'project', project: p }))
    if (looseMatches(query)) out.push({ kind: 'loose' })
    return out
  }, [projects, query])

  // Focus the box on mount: moving this dialog with the keyboard without a text field in
  // front of it would make typing filter nothing.
  useEffect(() => { inputRef.current?.focus() }, [])

  // The highlight follows the query rather than the list: a re-filter that left the index
  // past the end would make Enter a no-op. It follows the COMMITTED query (see useFilterInput),
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
  const pick = (c: ProjectChoice) => {
    const reason = onPick(c)
    if (reason) setNotice(reason)
    else onClose()
  }

  // facts is what a row SHOWS and what Enter will DO, derived per row because both kinds answer in
  // one shape. The three action cases are mutually exclusive and exhaustive, and the third is the
  // one the backend cannot say out loud: an unusable project refuses to CREATE (a session made here
  // would land unbound). It is ⌘P's only refusal — a bucket session is always openable, and making
  // one needs no workspace of anyone's — so the bucket row never blocks.
  const facts = (c: ProjectChoice) => {
    const p = c.kind === 'project' ? c.project : null
    const members = p ? sessions.filter((s) => s.projectId === p.id) : loose
    const latest = p ? latestSessionOf(sessions, p.id) : newestOf(loose)
    const blocked = !latest && !!p && !p.rootsUsable
    const action = latest ? `→ ${latest.title || '未命名会话'}`
      : blocked ? '主目录不可用，无法新建会话'
        : '→ 新建会话'
    return {
      // The sidebar gives the bucket's group this same key: one bucket, one key.
      key: p ? p.id : LOOSE_GROUP_KEY,
      name: p ? p.name : LOOSE_LABEL,
      where: p ? p.workingDir : LOOSE_LABEL_HINT,
      whereTitle: p ? p.workingDir : `不属于任何项目：${LOOSE_LABEL_HINT}`,
      count: members.length,
      action,
      blocked,
    }
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
      const c = rows[Math.min(idx, rows.length - 1)]
      if (c) pick(c)
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
          ) : rows.map((choice, i) => {
            const f = facts(choice)
            return (
              <div key={f.key} className={`palette-row${i === idx ? ' is-sel' : ''}`}
                role="option" aria-selected={i === idx}
                onMouseMove={() => setIdx(i)}
                onClick={() => pick(choice)}>
                <div className="palette-main">
                  <div className="palette-name">{f.name}</div>
                  <div className="palette-path" title={f.whereTitle}>{f.where}</div>
                </div>
                <div className="palette-side">
                  <div className="palette-meta">{f.count > 0 ? `${f.count} 个会话` : '还没有会话'}</div>
                  <div className={`palette-action${f.blocked ? ' is-blocked' : ''}`}>{f.action}</div>
                </div>
              </div>
            )
          })}
        </div>
        {notice ? <div className="palette-notice">{notice}</div> : null}
        <div className="palette-hint">↑↓ 选择 · Enter 打开（该组最近的会话，没有就新建）· Esc 关闭</div>
      </div>
    </div>
  )
}

// SessionPalette is ⌘K: type to filter the desktop's conversations by title, Enter to open the one
// chosen. The sibling of the ⌘P palette above and deliberately the same box — the same input
// behaviour (useFilterInput), the same ↑↓ / Ctrl+N·P navigation (`listNav`), the same Escape —
// because the two are one gesture apart, and a reader who has used one has learned the other.
//
// Three things it decides for itself:
//   • which rows there are — one per CONVERSATION (`sessionChoices`: a compaction chain folds under
//     its newest link, exactly as the sidebar folds it), in the list's own order with the working and
//     the parked-on-you conversations lifted to the head (see sessionChoices), and the query matched
//     against the TITLE only (`sessionMatches`).
//   • what a row says — the project it belongs to as a TAG (the one fact about a conversation that
//     its title cannot carry; a project the list does not know reads as 无项目, the same bucket the
//     sidebar puts such a row in), when it was last written to, and — for the rows the order lifted —
//     the SAME two markers the sidebar row carries (the amber 在等你 dot, the running spinner). Both
//     halves are load-bearing: an order a reader cannot explain reads as a bug in the list, and one
//     marker for two different states would make 「要我做什么」 and 「它还在忙」 the same sentence.
//   • which row is 「当前」 — the one the app is showing, marked so the list has an anchor.
//
// Unlike ⌘P there is no refusal to report: opening a conversation always works (a workspace that is
// gone shows up IN the conversation, not here), so a pick closes the box unconditionally — the
// reason string ⌘P's `onPick` returns exists for the projects that cannot be opened into anything.
export function SessionPalette({ sessions, projects, activeId, waitingOf, running, onPick, onClose }: {
  sessions: SessionItem[]
  projects: ProjectVO[]
  activeId: string
  // waitingOf answers WHO is parked on the reader and WHAT for: the sentence the agent wrote into
  // that conversation's own state (AgentState.Waiting — an approval card, a question). It is read
  // here for the order and shown on the row, exactly as the sidebar reads it: one source for "who is
  // waiting", so the two views cannot disagree about it.
  waitingOf: (id: string) => string
  // running is the sidebar's own running set (the session's turn is in flight in this process).
  running: Set<string>
  // onPick opens the chosen conversation. It has no refusal to answer with (see above).
  onPick: (s: SessionItem) => void
  onClose: () => void
}) {
  const [idx, setIdx] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  const { committed: query, composingRef, inputProps } = useFilterInput()

  const rows = useMemo(
    () => sessionChoices(sessions, (id) => running.has(id) || !!waitingOf(id)).filter((s) => sessionMatches(s, query)),
    [sessions, running, waitingOf, query],
  )

  // Focus the box on mount: moving this dialog with the keyboard without a text field in front of
  // it would make typing filter nothing.
  useEffect(() => { inputRef.current?.focus() }, [])

  // The highlight follows the query rather than the list: a re-filter that left the index past the
  // end would make Enter a no-op. It follows the COMMITTED query (see useFilterInput), so an IME
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
      : <span className="palette-loose" title="不属于任何项目：它用自己的工作区快照">{LOOSE_LABEL}</span>
  }

  // ↑↓ and Ctrl+N/Ctrl+P walk the list; navigation wraps, as it does in ⌘P. Enter opens the
  // highlighted conversation and Escape closes — and a composing IME owns all four (see
  // useFilterInput).
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
            <div className="palette-empty">没有匹配的会话（{MOD_KEY}P 搜的是项目）</div>
          ) : rows.map((s, i) => (
            <div key={s.id} className={`palette-row${i === idx ? ' is-sel' : ''}`}
              role="option" aria-selected={i === idx}
              onMouseMove={() => setIdx(i)}
              onClick={() => { onPick(s); onClose() }}>
              <div className="palette-main">
                <div className="palette-name">{s.title || '未命名会话'}</div>
                <div className="palette-sub">
                  {/* The two markers the sidebar row carries, in the same order and for the same
                      reason: the amber dot asks for something (it is the one mark here that does),
                      the spinner says the turn is still going. They are what explains a row that
                      sorted to the head — an unexplained jump reads as a bug in the list. */}
                  {waitingOf(s.id)
                    ? <span className="wait-dot" role="img" aria-label={waitingOf(s.id)} title={waitingOf(s.id)} />
                    : null}
                  {running.has(s.id) ? <span className="spin-dot" title="运行中" /> : null}
                  {projectTag(s)}
                </div>
              </div>
              <div className="palette-side">
                <div className="palette-meta">{fmtTime(s.updatedAt)}</div>
                {s.id === activeId ? <div className="palette-here">当前会话</div> : null}
              </div>
            </div>
          ))}
        </div>
        <div className="palette-hint">↑↓ 选择 · Enter 打开 · Esc 关闭（进行中、在等你的会话排最前）</div>
      </div>
    </div>
  )
}
