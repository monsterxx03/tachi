import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Command } from 'cmdk'
import type { ProjectVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { fmtTime, imeActive, latestSessionOf, looseMatches, looseMembers, newestOf, projectMatches, sessionChoices, sessionMatches, useFilterInput, LOOSE_LABEL, LOOSE_LABEL_HINT, LOOSE_GROUP_KEY } from './lib'
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

/** choiceKey is a row's identity in the list — the same key facts() shows and cmdk selects by. */
const choiceKey = (c: ProjectChoice) => (c.kind === 'project' ? c.project.id : LOOSE_GROUP_KEY)

// The keys cmdk's root handler consumes. They matter only while an IME owns the keyboard, where a
// synthetic composition carries neither of cmdk's own guards (`isComposing`, `keyCode === 229`) —
// see paletteKeys.
const CMDK_CONSUMED = new Set(['ArrowDown', 'ArrowUp', 'Home', 'End', 'Enter'])

// paletteKeys is the half of the keyboard cmdk does NOT own: Esc closes, Ctrl+N / Ctrl+P walk the
// list.
//
// The other half is deliberately handed over: ↑↓, Home/End, Enter, keeping the selected row in
// view, and the listbox/option roles all come from cmdk. Its handler sits on its ROOT element and
// runs OURS first, then skips anything `defaultPrevented` — which is what lets the two halves share
// one keystroke without fighting.
//
// `vimBindings={false}` is not an oversight: cmdk's ctrl+n/j/p/k pair also claims Ctrl+J and Ctrl+K,
// and Ctrl+K in a text field is 「删到行尾」 (macOS's emacs bindings). A list that eats it makes the
// caret unable to delete. tachi's keymap is n/p only (readline's next/previous, the TUI's own pair),
// so those two are re-implemented here — the price of not taking the other two with them.
function paletteKeys({ keys, value, setValue, onClose, onEnter, composingRef }: {
  /** Every row's key, in list order — what next/previous walks. */
  keys: string[]
  /** The row cmdk has selected; the step is taken from here, not from a local index. */
  value: string
  setValue: React.Dispatch<React.SetStateAction<string>>
  onClose: () => void
  /** Enter acts on a ROW KEY, resolved here rather than by cmdk (see below). */
  onEnter: (key: string) => void
  composingRef: React.MutableRefObject<boolean>
}) {
  return (e: React.KeyboardEvent) => {
    // A composing IME owns these keys first: Esc cancels its candidate, and ↑↓ / Ctrl+N·P walk the
    // candidate list rather than ours (see useFilterInput). cmdk has its own guard, but a synthetic
    // composition (what the smoke driver builds) reports neither `isComposing` nor keyCode 229, and
    // the Enter that confirms a candidate would then be read as 「打开这一行」. Marking those keys
    // handled is what keeps cmdk off them; Esc is left alone so the IME still sees it.
    if (composingRef.current || imeActive(e)) {
      if (CMDK_CONSUMED.has(e.key)) e.preventDefault()
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
      return
    }
    // Enter is resolved HERE, from the same `keys`/`value` the arrows move, and never left to cmdk:
    // cmdk reads the highlighted row out of the DOM, and a highlight that was re-seated a moment
    // ago (a query dropped every old row — the one-shot assignment a paste or an IME's
    // compositionend makes) has not been painted yet. The press would then land on nothing at all.
    // Keys that are gone fall back to the head of the list, which is the row the reader sees
    // highlighted once that paint happens.
    if (e.key === 'Enter') {
      e.preventDefault()
      const at = keys.indexOf(value)
      const key = keys[at < 0 ? 0 : at]
      if (key !== undefined) onEnter(key)
      return
    }
    // Cmd+N is 「新建会话」 and must reach the app; only the plain Ctrl pair switches rows.
    if (!e.ctrlKey || e.metaKey || e.altKey) return
    const k = e.key.toLowerCase()
    if (k !== 'n' && k !== 'p') return
    e.preventDefault()
    if (keys.length === 0) return
    const step = k === 'n' ? 1 : -1
    // A FUNCTIONAL update: a held key (or two keydowns in the same tick, which is how the smoke
    // driver walks two rows) must advance twice, and a closed-over `value` would advance once.
    setValue((cur) => {
      const at = keys.indexOf(cur)
      return keys[at < 0 ? 0 : (at + step + keys.length) % keys.length]
    })
  }
}

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
  // The selected row is cmdk's `value`: a row's KEY, not an index. A re-filter that drops rows
  // then cannot leave the selection pointing past the end — cmdk re-selects the first surviving
  // row itself, which is one of the two effects this file used to carry by hand.
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

  // Start on the FIRST row. Controlled `value` is what lets Ctrl+N/P move the highlight, but it
  // also takes away cmdk's own initial pick: left empty, the list has no highlight until the first
  // arrow key — and Enter would do nothing.
  const [value, setValue] = useState(() => (rows[0] ? choiceKey(rows[0]) : ''))
  const [notice, setNotice] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)

  // …and keep it inside the list afterwards. cmdk picks the first row once, on mount, and does NOT
  // re-pick when the selected row filters away (with shouldFilter={false} it never re-runs its own
  // search): type a query that drops every previous row and the list is left with NO highlight —
  // Enter then does nothing at all. A whole-paste query (and an IME's one-shot compositionend) hits
  // exactly that case. Re-seating the selection on the first surviving row is this file's job.
  // LAYOUT effect, not a passive one: the re-seat has to be in the DOM before the browser paints,
  // or there is a frame in which the list has rows but no highlight — and the very next Enter (a
  // driver's, or a reader's fast one) would be aimed at nothing.
  useLayoutEffect(() => {
    if (value && rows.some((c) => choiceKey(c) === value)) return
    if (rows.length > 0) setValue(choiceKey(rows[0]))
  }, [rows, value])

  // Focus the box on mount: moving this dialog with the keyboard without a text field in
  // front of it would make typing filter nothing.
  useEffect(() => { inputRef.current?.focus() }, [])

  // A refusal belongs to the query that produced it: retype, and it goes away. The selection needs
  // no reset here — it follows the COMMITTED query (so an IME composition does not walk it), and
  // cmdk both drops a selection whose row filtered out and keeps the selected row in view.
  useEffect(() => { setNotice('') }, [query])

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
      key: choiceKey(c),
      name: p ? p.name : LOOSE_LABEL,
      where: p ? p.workingDir : LOOSE_LABEL_HINT,
      whereTitle: p ? p.workingDir : `不属于任何项目：${LOOSE_LABEL_HINT}`,
      count: members.length,
      action,
      blocked,
    }
  }

  // ↑↓, Home/End and Enter are cmdk's now (`loop` keeps the wrap this list has always had);
  // the Ctrl pair and Escape come from paletteKeys. Both halves read the same `value`, so the
  // keyboard and the pointer can never highlight two different rows.
  const keys = useMemo(() => rows.map(choiceKey), [rows])
  const onKeyDown = paletteKeys({
    keys, value, setValue, onClose, composingRef,
    onEnter: (key) => {
      const c = rows.find((x) => choiceKey(x) === key)
      if (c) pick(c)
    },
  })

  return (
    <div className="palette-overlay" onClick={onClose}>
      <div className="palette-box" role="dialog" aria-label="项目搜索" onClick={(e) => e.stopPropagation()}>
        {/* shouldFilter={false}: the rows are already filtered by the sidebar's own rules
            (projectMatches / looseMatches), including the bucket and the directory match. cmdk
            filtering a second time would only disagree with them about the same query. */}
        <Command loop shouldFilter={false} vimBindings={false}
          value={value} onValueChange={setValue} onKeyDown={onKeyDown}>
          <input ref={inputRef} className="palette-input" {...inputProps}
            placeholder="搜索项目（名字或目录）" />
          <Command.List className="palette-list" aria-label="项目">
            {rows.length === 0 ? (
              <div className="palette-empty">没有匹配的项目</div>
            ) : rows.map((choice) => {
              const f = facts(choice)
              return (
                <Command.Item key={f.key} value={f.key} className="palette-row"
                  onSelect={() => pick(choice)}>
                  <div className="palette-main">
                    <div className="palette-name">{f.name}</div>
                    <div className="palette-path" title={f.whereTitle}>{f.where}</div>
                  </div>
                  <div className="palette-side">
                    <div className="palette-meta">{f.count > 0 ? `${f.count} 个会话` : '还没有会话'}</div>
                    <div className={`palette-action${f.blocked ? ' is-blocked' : ''}`}>{f.action}</div>
                  </div>
                </Command.Item>
              )
            })}
          </Command.List>
        </Command>
        {notice ? <div className="palette-notice">{notice}</div> : null}
        <div className="palette-hint">↑↓ 选择 · Enter 打开（该组最近的会话，没有就新建）· Esc 关闭</div>
      </div>
    </div>
  )
}

// SessionPalette is ⌘K: type to filter the desktop's conversations by title, Enter to open the one
// chosen. The sibling of the ⌘P palette above and deliberately the same box — the same input
// behaviour (useFilterInput), the same ↑↓ / Ctrl+N·P navigation (cmdk plus paletteKeys), the same
// Escape — because the two are one gesture apart, and a reader who has used one has learned the
// other.
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
  const { committed: query, composingRef, inputProps } = useFilterInput()

  const rows = useMemo(
    () => sessionChoices(sessions, (id) => running.has(id) || !!waitingOf(id)).filter((s) => sessionMatches(s, query)),
    [sessions, running, waitingOf, query],
  )

  // Start on the FIRST row, and keep the selection inside the list after every re-filter — cmdk
  // only picks a row once, on mount (see ProjectPalette for the whole story: a one-shot query
  // would otherwise leave the list with no highlight at all and Enter would do nothing).
  const [value, setValue] = useState(() => rows[0]?.id ?? '')
  const inputRef = useRef<HTMLInputElement>(null)

  // Layout, not passive — the array has to carry a highlight before the paint (see ProjectPalette).
  useLayoutEffect(() => {
    if (value && rows.some((s) => s.id === value)) return
    if (rows.length > 0) setValue(rows[0].id)
  }, [rows, value])

  // Focus the box on mount: moving this dialog with the keyboard without a text field in front of
  // it would make typing filter nothing.
  useEffect(() => { inputRef.current?.focus() }, [])

  // The selection follows the COMMITTED query (so an IME composition does not walk it); cmdk
  // re-selects the first surviving row when a filter drops the selected one, and scrolls it into
  // view itself — the two effects this component used to carry by hand.

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

  // ↑↓, Home/End and the wrap are cmdk's (`loop`); the Ctrl pair, Escape and Enter are
  // paletteKeys'. Enter opens the row this list has selected — see there for why cmdk is not the
  // one resolving it.
  const keys = useMemo(() => rows.map((s) => s.id), [rows])
  const onKeyDown = paletteKeys({
    keys, value, setValue, onClose, composingRef,
    onEnter: (key) => {
      const s = rows.find((x) => x.id === key)
      if (s) { onPick(s); onClose() }
    },
  })

  return (
    <div className="palette-overlay" onClick={onClose}>
      <div className="palette-box" role="dialog" aria-label="会话搜索" onClick={(e) => e.stopPropagation()}>
        {/* shouldFilter={false}: sessionChoices + sessionMatches already decided which rows exist
            and in what order (chains folded, working/waiting lifted). A second filter inside cmdk
            would disagree with the sidebar about the same query. */}
        <Command loop shouldFilter={false} vimBindings={false}
          value={value} onValueChange={setValue} onKeyDown={onKeyDown}>
          <input ref={inputRef} className="palette-input" {...inputProps}
            placeholder="搜索会话标题" />
          <Command.List className="palette-list" aria-label="会话">
            {rows.length === 0 ? (
              // The empty state names the OTHER search: a reader who typed a project (or a word from
              // the conversation rather than from its title) is one keystroke away from ⌘P, and
              // "no match" alone would leave them retyping into the same box.
              <div className="palette-empty">没有匹配的会话（{MOD_KEY}P 搜的是项目）</div>
            ) : rows.map((s) => (
              <Command.Item key={s.id} value={s.id} className="palette-row"
                onSelect={() => { onPick(s); onClose() }}>
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
              </Command.Item>
            ))}
          </Command.List>
        </Command>
        <div className="palette-hint">↑↓ 选择 · Enter 打开 · Esc 关闭（进行中、在等你的会话排最前）</div>
      </div>
    </div>
  )
}
