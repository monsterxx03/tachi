// Diff rendering: two views of the same data model.
//
//   - DiffBlock — the per-call FRAGMENT diff on a tool card. Line numbers are
//     fragment-relative there, so they are not shown (a fragment has no position in
//     the file), and a long diff folds.
//   - DiffFindingsPane — the working-tree diff of a file set against git HEAD (real file
//     coordinates, so numbers ARE shown), with a run's findings anchored on the lines they
//     name. It is rendered as the side-channel panel's 意见 pane, and — with no findings —
//     as TurnDiffOverlay, the turn's own 「完整 diff」.
//
// Both render the same `Hunk` lines through DiffLines, which is what keeps the two
// views from drifting: the difference is a flag, not a second implementation.

import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode, useSyncExternalStore } from 'react'
import { AgentService, type FileChangeVO, type FileDiffVO, type FindingVO, type TurnDiffVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { Hunk } from '../bindings/github.com/monsterxx03/tachi/pkg/linediff'
import { FilePreviewOverlay } from './filepreview'
import { CloseButton, ViewerOverlay } from './viewer'
import { Caret } from './components'
import { InlineMd } from './markdown'

// DIFF_FOLD_LINES is how much of a change shows before folding.
const DIFF_FOLD_LINES = 24

// The file index's width: the column the reader drags, its bounds, and one arrow-key step.
//
// The default is the number the CSS used to hard-code; the ceiling exists because the index is a
// way to FIND a file, not a second diff to read, and the floor because a column narrower than the
// shortest path shows nothing at all. The body keeps DIFF_FILELIST_BODY_MIN whatever the reader
// asks for — this column grows into the room the pane has, never into the diff.
const DIFF_FILELIST_DEFAULT_WIDTH = 230
const DIFF_FILELIST_MIN_WIDTH = 180
const DIFF_FILELIST_MAX_WIDTH = 560
const DIFF_FILELIST_BODY_MIN = 420
const DIFF_FILELIST_KEY_STEP = 16
// How far below the pinned head a jumped-to file comes to rest. It is published as `--diff-land`
// (the head's height plus this) and read by BOTH the CSS `scroll-margin-top` and the highlight's
// own rule, so the jump and the mark are one definition — hard-coding the number in either place
// is how they drift, and reading the computed `scroll-margin-top` back turned out to be a value
// WebKit may hand over empty (measured: the landing line collapsed to the scroller's top, and the
// highlight vanished on the first scroll).
const DIFF_FILE_JUMP_GAP = 10

// fileListWidth survives the pane being unmounted (the overlay is closed and reopened), because a
// width the reader set a second ago must not be forgotten on reopen. Module level on purpose:
// this is the same kind of memory as the theme's own (see theme.ts), NOT a preference the app
// persists — the panel's width lives in desktop_ui.json because Go has to honour it before the
// window exists, which is a bargain this column does not need to strike.
let fileListWidth = DIFF_FILELIST_DEFAULT_WIDTH

// clampFileListWidth bounds a requested width by the pane it has to fit in: never narrower than
// the floor, never so wide that the diff body drops below DIFF_FILELIST_BODY_MIN. A pane that
// cannot be measured yet (the first render, before the element exists) has no ceiling to impose —
// bounding by that zero would show the floor for a frame and only correct itself on the next
// render, which never comes until the reader does something.
function clampFileListWidth(px: number, paneWidth: number): number {
  const ceiling = paneWidth > 0
    ? Math.max(DIFF_FILELIST_MIN_WIDTH, Math.min(DIFF_FILELIST_MAX_WIDTH, paneWidth - DIFF_FILELIST_BODY_MIN))
    : DIFF_FILELIST_MAX_WIDTH
  return Math.max(DIFF_FILELIST_MIN_WIDTH, Math.min(ceiling, Math.round(px)))
}

// ── The diff's layout: 上下 (unified) or 左右 (side-by-side) ──────────────────
//
// A READING preference, not a layout the window is built from — which is why it lives at MODULE
// level, like the file index's width and unlike the side panel's (that one is a column the window's
// own geometry depends on). One value for both surfaces the pane mounts in: toggling it in the
// overlay is how the side panel reads next time, which is what a reader who picked a layout means.
//
// It persists in localStorage rather than in desktop_ui.json (where the widths go): the widths are
// GEOMETRY, and Go owns the window those numbers have to fit; this is a preference about reading,
// like the theme, and the same store already keeps that one. Reading it here — synchronously, at
// module load — is also what keeps a pane from painting 上下 and then flipping to 左右.
const DIFF_LAYOUT_KEY = 'tachi.desktop.diffLayout'

function storedDiffSplit(): boolean {
  try {
    return localStorage.getItem(DIFF_LAYOUT_KEY) === 'split'
  } catch {
    return false // storage can be blocked; the toggle still works for this session
  }
}

let diffSplit = storedDiffSplit()
const layoutListeners = new Set<() => void>()

/** Sets the diff layout (and remembers it) and re-renders every pane (both surfaces). */
function setDiffSplit(on: boolean) {
  if (diffSplit === on) return
  diffSplit = on
  try {
    localStorage.setItem(DIFF_LAYOUT_KEY, on ? 'split' : 'unified')
  } catch {
    /* storage blocked: this session still switches, it just will not persist */
  }
  layoutListeners.forEach((cb) => cb())
}

function subscribeDiffLayout(cb: () => void): () => void {
  layoutListeners.add(cb)
  return () => { layoutListeners.delete(cb) }
}

function useDiffSplit(): boolean {
  return useSyncExternalStore(subscribeDiffLayout, () => diffSplit, () => diffSplit)
}

// scrollableAncestor is the box the pane actually scrolls in — the overlay's stage, the panel's
// body (the pane itself is the content, not the scroller; see the CSS). Found by walking up rather
// than passed in, because the same pane is mounted in both surfaces and neither one's class is the
// other's business. It is also what "which file am I looking at" has to be measured against.
function scrollableAncestor(el: HTMLElement | null): HTMLElement | null {
  for (let n = el?.parentElement ?? null; n; n = n.parentElement) {
    const oy = getComputedStyle(n).overflowY
    if (oy === 'auto' || oy === 'scroll' || oy === 'overlay') return n
  }
  return null
}

// findingMatchesFile compares a finding's path with a panel entry's: the reviewer may
// have written it relative to the repo or absolutely, and both mean the same file.
function findingMatchesFile(root: string, findingPath: string, filePath: string): boolean {
  let p = findingPath.replace(/^\.\//, '')
  if (root) {
    const prefix = root.endsWith('/') ? root : root + '/'
    if (p.startsWith(prefix)) p = p.slice(prefix.length)
  }
  return p === filePath
}

// DiffLines renders hunk lines. `lineNumbers` adds the real-file-coordinate column —
// only the working-tree panel may ask for it.
//
// Placement of findings is this component's job; how one is DRAWN belongs to the caller
// (in the panel a row carries a pick box and a comment field; a tool card's fragment diff
// has no findings to draw at all).
//
// It is exported because the settings page's preview of the generated config.yaml is the third
// caller: the same `Hunk` model, the same renderer — a second implementation of "show a diff" is
// how the two come to disagree about what a changed line looks like.
// splitRows pairs a hunk's lines for the 左右 layout: a CONTEXT line is a row of its own — the same
// text on BOTH sides, because each column is meant to read as one whole file — and a run of removals
// followed by a run of additions becomes one row per index: the line that went, beside the line that
// came. Whatever is left over on either side keeps its own row with an empty cell opposite, which is
// what makes an uneven replacement block (three lines out, one line in) read correctly instead of
// shifting the two columns apart.
//
// A `del` run that is NOT followed by an `add` run (a pure deletion) still pairs row-by-row —
// holes on the right — for the same reason.
type SplitRow = { left?: Hunk; right?: Hunk; ctx?: Hunk }

function splitRows(hunks: Hunk[]): SplitRow[] {
  const out: SplitRow[] = []
  let i = 0
  while (i < hunks.length) {
    const h = hunks[i]
    // Anything that is not an add or a del is context: the Hunk model has exactly three kinds, and
    // the unified view below reads them the same way (its sign falls through to a space).
    if (h.kind !== 'add' && h.kind !== 'del') { out.push({ ctx: h }); i++; continue }
    if (h.kind === 'add') { out.push({ right: h }); i++; continue }
    const dels: Hunk[] = []
    while (i < hunks.length && hunks[i].kind === 'del') dels.push(hunks[i++])
    const adds: Hunk[] = []
    while (i < hunks.length && hunks[i].kind === 'add') adds.push(hunks[i++])
    for (let k = 0; k < Math.max(dels.length, adds.length); k++) {
      out.push({ left: dels[k], right: adds[k] })
    }
  }
  return out
}

export function DiffLines({ hunks, lineNumbers, findings, renderFinding, split = false }: {
  hunks: Hunk[]
  lineNumbers: boolean
  // Findings are placed inline, right under the line they are about — a review comment
  // belongs next to the code it questions.
  findings?: PlacedFinding[]
  renderFinding?: (item: PlacedFinding, orphan: boolean) => ReactNode
  /** 左右 layout (see splitRows). The caller decides: the pane follows the reader's toggle, a tool
   *  card's fragment diff stays 上下 (it lives in a column a third of the window wide). */
  split?: boolean
}) {
  const draw = renderFinding
  // Which findings land under which hunk line: a finding names a NEW-side line (the
  // file as it is now), falling back to the old side for a deleted file.
  const byLine = new Map<number, PlacedFinding[]>()
  for (const item of findings || []) {
    const key = item.finding.line
    byLine.set(key, [...(byLine.get(key) || []), item])
  }
  const placed = new Set<FindingVO>()

  // take collects the findings of a hunk line and marks them placed, so the same one cannot come
  // back through the other side of a 左右 row (a finding names one LINE NUMBER, and a paired del/add
  // can legitimately carry the same number on both sides) nor through the orphan list at the end.
  const take = (h?: Hunk): PlacedFinding[] => {
    if (!h) return []
    const here = h.newLine ? byLine.get(h.newLine) : h.kind === 'del' ? byLine.get(h.oldLine) : undefined
    const fresh = (here || []).filter((item) => !placed.has(item.finding))
    for (const item of fresh) placed.add(item.finding)
    return fresh
  }

  // one line of code, on the side it belongs to. The 上下 view draws left+right merged (that is what
  // a unified diff IS): it passes the line's own kind through, and its line-number column carries
  // both coordinates. The 左右 view gives each cell ONE coordinate — the old side's number on the
  // left, the new side's on the right — because that is what the column is showing.
  const line = (h: Hunk, side?: 'left' | 'right') => {
    const numbers = !lineNumbers ? [] : side === 'left' ? [h.oldLine] : side === 'right' ? [h.newLine] : [h.oldLine, h.newLine]
    return (
      <div className={`diff-line is-${h.kind}`}>
        {numbers.map((n, i) => <span key={i} className="diff-ln">{n || ''}</span>)}
        {/* The sign carries the meaning; the colour only reinforces it. */}
        <span className="diff-sign">{h.kind === 'add' ? '+' : h.kind === 'del' ? '−' : ' '}</span>
        <span className="diff-text">{h.text === '' ? ' ' : h.text}</span>
      </div>
    )
  }
  // A cell with no line on this side (a pure deletion's right half, an addition's left): the row has
  // to keep its height and its columns aligned with every other row, so it is a real box, empty.
  const blank = () => (
    <div className="diff-line is-blank">
      {lineNumbers ? <span className="diff-ln" /> : null}
      <span className="diff-sign" />
    </div>
  )

  // 左右 mode: the two columns ARE the two files, so each one scrolls as a COLUMN rather than as a
  // grid of independent rows — dragging one left cell sideways moves every left cell with it, and the
  // pairing stays true for the whole file instead of only for the row under the pointer.
  //
  // The listener sits on each CELL (React's onScroll on an element is attached to that element, and
  // this is exactly why: a capture-phase listener on the body looked equivalent and caught NOTHING in
  // this engine — measured in the diff-layout smoke, where only the dragged row moved). The mirror
  // writes the same value back into the other cells and stops there: assigning a scrollLeft that is
  // already set fires no further event.
  const mirrorScroll = (e: React.UIEvent<HTMLDivElement>) => {
    const src = e.currentTarget
    const group = src.classList.contains('is-left') ? 'is-left' : 'is-right'
    const body = src.closest('.diff-body')
    if (!body) return
    for (const el of body.querySelectorAll<HTMLElement>(`.diff-side.${group}`)) {
      if (el !== src) el.scrollLeft = src.scrollLeft
    }
  }

  return (
    <div className={`diff-body${split ? ' is-split' : ''}`}>
      {split
        ? splitRows(hunks).map((row, i) => {
          // The right side FIRST: a finding names a new-side line, so when a paired del/add happen
          // to carry the same number, the new-side line is the one it is about.
          const here = [...take(row.right), ...take(row.left)]
          return (
            <div key={i}>
              {/* The row is the pair; each cell is one file's line. A context line is drawn on both
                  sides rather than stretched across, so either column reads as one whole file. */}
              <div className="diff-split-row">
                <div className="diff-side is-left" onScroll={mirrorScroll}>{row.ctx || row.left ? line((row.ctx || row.left)!, 'left') : blank()}</div>
                <div className="diff-side is-right" onScroll={mirrorScroll}>{row.ctx || row.right ? line((row.ctx || row.right)!, 'right') : blank()}</div>
              </div>
              {/* A finding spans the whole row instead of sitting in a half-width column: it is a
                  note about the pair, and a review comment in a 150px cell is unreadable. Which cell
                  it belongs to is already said by the new-side anchor it carries. */}
              {here.map((item, j) => <Fragment key={j}>{draw ? draw(item, false) : null}</Fragment>)}
            </div>
          )
        })
        : hunks.map((h, i) => {
          const here = take(h)
          return (
            <div key={i}>
              {line(h)}
              {here.map((item, j) => <Fragment key={j}>{draw ? draw(item, false) : null}</Fragment>)}
            </div>
          )
        })}
      {/* A finding whose line is not in the diff (the reviewer saw a wider window, or the
          file changed after the review) still has to be readable. */}
      {(findings || []).filter((item) => !placed.has(item.finding))
        .map((item, j) => <Fragment key={`x${j}`}>{draw ? draw(item, true) : null}</Fragment>)}
    </div>
  )
}

// SEVERITY_META is the review's three levels: 🐛 bug / ⚠️ warn / 💡 info, matching the
// severities ReportFinding accepts.
const SEVERITY_META: Record<string, { icon: string; label: string }> = {
  bug: { icon: '🐛', label: 'bug' },
  warn: { icon: '⚠️', label: 'warn' },
  info: { icon: '💡', label: 'info' },
}

// CATEGORY_META names the review's five perspectives (ReportFinding's schema fixes them).
// The value comes from the model, so an unknown one falls back to the raw string: a finding
// is never worth losing to a label lookup.
const CATEGORY_META: Record<string, string> = {
  Correctness: '正确性',
  Quality: '代码质量',
  Efficiency: '性能',
  Security: '安全',
  Maintainability: '可维护性',
}

// PlacedFinding is a finding together with its index in the panel's payload. Findings
// carry no id of their own, so that index is what the draft is keyed by.
type PlacedFinding = { finding: FindingVO; idx: number }

// ── Picking findings into a message (P3) ────────────────────────────────────────
// A review's findings are a menu, not a command: the reader ticks the ones the agent
// should act on, may add a note to any of them, and sends the list as an ordinary user
// message. The panel keeps the reader's EDITS only — an untouched finding has no entry at
// all and is described by defaultPick. That is what keeps a payload arriving (or being
// replaced) after the panel opened from needing a sync step.

// FindingPick is one finding's share of the draft: whether it goes into the message, and
// what the reader added to it.
type FindingPick = { checked: boolean; comment: string }

// PRIMARY_ROOT_LABEL names the session's own working directory, for the surfaces that have to say
// which tree a file is in: the roots panel's word for it, and the same line the review prompt
// draws when it writes "(primary)" beside a directory name — so the app does not end up with two
// vocabularies for one fact.
const PRIMARY_ROOT_LABEL = '主目录'

// defaultPick: 🐛 and ⚠️ say something is wrong, so they arrive ticked; 💡 is usually
// "consider…" and waits to be opted into.
export function defaultPick(severity: string): FindingPick {
  return { checked: severity !== 'info', comment: '' }
}

// findingAnchor is the coordinate a message quotes — the same `path:line` the panel shows,
// in FILE coordinates rather than the fragment-relative ones on a tool card. That is why
// the list can only be built here (and why P1 kept Hunk.OldLine/NewLine around).
function findingAnchor(f: FindingVO): string {
  if (!f.line) return f.path
  return f.endLine && f.endLine > f.line ? `${f.path}:${f.line}-${f.endLine}` : `${f.path}:${f.line}`
}

// buildFindingMessage renders the picked findings as the message that goes to the agent.
// The anchor leads, because that is what the agent will act on; the rest is indented
// under it.
export function buildFindingMessage(items: { finding: FindingVO; pick: FindingPick }[]): string {
  const lines = [`请按以下评审意见修改（共 ${items.length} 条）：`, '']
  items.forEach(({ finding, pick }, i) => {
    lines.push(`${i + 1}. ${findingAnchor(finding)}［${finding.severity}］${finding.text}`)
    if (finding.suggestion) lines.push(`   建议：${finding.suggestion}`)
    if (pick.comment.trim()) lines.push(`   补充：${pick.comment.trim()}`)
  })
  return lines.join('\n')
}

function FindingRow({ item, pick, onPick, orphan }: {
  item: PlacedFinding
  pick: FindingPick
  onPick: (idx: number, next: FindingPick) => void
  orphan?: boolean
}) {
  const finding = item.finding
  const meta = SEVERITY_META[finding.severity] || SEVERITY_META.info
  return (
    // data-finding-idx is the jump target: the arrows find a row by the finding's index in
    // the payload, which is stable across renders and unique across the whole pane.
    <div className={`finding is-${finding.severity}${orphan ? ' is-orphan' : ''}${pick.checked ? '' : ' is-unpicked'}`}
      data-finding-idx={item.idx}>
      {/* The tick is the whole contract with the message: only what is ticked is sent. */}
      <label className="finding-pick" title={pick.checked ? '取消：不写进发给 Tachi 的清单' : '选中：写进发给 Tachi 的清单'}>
        <input type="checkbox" checked={pick.checked}
          onChange={(e) => onPick(item.idx, { ...pick, checked: e.target.checked })} />
      </label>
      <span className="finding-sev" title={meta.label}>{meta.icon}</span>
      {finding.category ? (
        <span className="finding-cat" title={`评审维度：${finding.category}`}>
          {CATEGORY_META[finding.category] || finding.category}
        </span>
      ) : null}
      <span className="finding-body">
        <span className="finding-text"><InlineMd text={finding.text} /></span>
        {finding.suggestion ? <span className="finding-fix">建议：<InlineMd text={finding.suggestion} /></span> : null}
        {orphan ? <span className="finding-orphan">（{findingAnchor(finding)}，不在以上差异行内）</span> : null}
        {/* Only a ticked finding asks for a note: this is where the reader says how they
            want it fixed, or that this spot should be left alone. */}
        {pick.checked ? (
          <CommentField value={pick.comment} onChange={(v) => onPick(item.idx, { ...pick, comment: v })} />
        ) : null}
      </span>
    </div>
  )
}

// CommentField is the optional note under a ticked finding. It grows with what is typed:
// one line covers the usual "先别动这里", and a fixed one-line box would hide the rest.
function CommentField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const ref = useRef<HTMLTextAreaElement>(null)
  const grow = useCallback(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${el.scrollHeight}px`
  }, [])
  useEffect(grow, [grow, value])
  return (
    <textarea ref={ref} className="finding-comment" rows={1} value={value}
      placeholder="补充说明（可选）：想怎么改，或者哪里别动"
      onChange={(e) => onChange(e.target.value)} />
  )
}

// DiffBlock is the tool card's fragment diff.
export const DiffBlock = memo(function DiffBlock({ change }: { change: FileChangeVO }) {
  const [open, setOpen] = useState(false)
  const hunks = change.hunks || []
  const folded = !open && hunks.length > DIFF_FOLD_LINES
  const shown = folded ? hunks.slice(0, DIFF_FOLD_LINES) : hunks

  return (
    <div className="diff-block">
      <div className="diff-head">
        <span className="diff-path" title={change.path}>{change.path}</span>
        <DiffCounts added={change.added} removed={change.removed} />
        {/* A fragment diff can only show one block against another, so a call that
            replaced every occurrence says so instead of implying paired changes. */}
        {change.replaceAll ? <span className="diff-note">多处替换，整段对照</span> : null}
      </div>
      <DiffLines hunks={shown} lineNumbers={false} />
      {folded ? (
        <button type="button" className="diff-more" onClick={() => setOpen(true)}>
          还有 {hunks.length - DIFF_FOLD_LINES} 行，展开
        </button>
      ) : null}
    </div>
  )
})

function DiffCounts({ added, removed }: { added: number; removed: number }) {
  return (
    <>
      {added > 0 ? <span className="diff-count is-add">+{added}</span> : null}
      {removed > 0 ? <span className="diff-count is-del">−{removed}</span> : null}
    </>
  )
}

// fileKey is a diff file's identity, for React keys, for the fold state and for the file index's
// jump: (root, oldPath, path) — never the path alone. A session can carry additional roots, two of
// them can hold the same relative path, and with the path alone clicking one root's file would
// unfold the other root's. \n cannot appear in a path, so the join is unambiguous.
function fileKey(f: FileDiffVO): string {
  return (f.root || '') + '\n' + (f.oldPath || '') + f.path
}

// fileTree is the root a diff file lives in: its OWN, falling back to the pane's — a session can
// carry additional roots and a path under one of them is relative to THAT root, so using the
// pane's (the primary) resolves a same-named file in the primary root. This is the one place that
// fallback is written: fileAbsPath and the group's root badge both read it from here.
function fileTree(file: FileDiffVO, root: string): string {
  return file.root || root
}

// fileAbsPath is where a diff file lives on disk (see fileTree for why it is not simply the
// pane's root plus the path).
function fileAbsPath(file: FileDiffVO, root: string): string {
  const tree = fileTree(file, root)
  return tree && !file.path.startsWith('/') ? `${tree}/${file.path}` : file.path
}

// fileRootName is which tree a diff file is in, in the words the rest of the app uses: an
// additional root is named by its directory (the backend's label), the primary by 主目录 — the
// roots panel's own word for it, and the distinction the review prompt draws when it writes
// "(primary)" next to a directory name. Leaving the primary bare only works while there is
// nothing to compare it with; in a multi-root diff the bare row is the one you cannot place.
//
// These three live at module level because the file index and the file GROUP both need them: two
// copies is how the list comes to name a different tree than the diff under it.
function fileRootName(file: FileDiffVO, root: string, multiRoot: boolean): string {
  return file.rootLabel || (multiRoot && root ? PRIMARY_ROOT_LABEL : '')
}

// FileGroup is one working directory's slice of a diff, for the file index.
type FileGroup = {
  // tree is the absolute root every file here lives in — the group's identity (two roots can hold
  // the same relative path, so it is never the label).
  tree: string
  label: string
  files: FileDiffVO[]
}

// fileGroups buckets a diff's files by the tree they live in, keeping the order the backend
// emitted them in — deliberately: that is the order the diff BODY renders the groups in, so the
// index and the content read the same way down. (The order itself is the backend's business and
// is not always primary-first: the checkpoint path emits whatever order the roots' diffs arrive
// in, which is why nothing here sorts.)
//
// The index is a LIST the reader scans to pick a tree, and a bare path in a list that mixes two
// trees is the same ambiguity the root label exists to remove — one step worse, since here there
// is no diff body next to the row to disambiguate it. So a multi-root diff gets one section per
// working directory (`multiRoot` decides, not the number of sections: a diff where only an
// additional root changed has ONE section, and its rows still need to say which tree that is).
// A single-root diff gets no headers at all — a lone 「主目录」 over the only tree there is would
// be a label with nothing to be told apart from.
function fileGroups(files: FileDiffVO[], root: string, multiRoot: boolean): FileGroup[] {
  const groups: FileGroup[] = []
  const byTree = new Map<string, FileGroup>()
  for (const f of files) {
    const tree = fileTree(f, root)
    let g = byTree.get(tree)
    if (!g) {
      // Every file of a root carries the same label (the backend derives it per root), so the
      // first one names the section.
      g = { tree, label: fileRootName(f, root, multiRoot), files: [] }
      byTree.set(tree, g)
      groups.push(g)
    }
    g.files.push(f)
  }
  return groups
}

// JUMP_HIGHLIGHT_MS is how long the row an arrow landed on stays marked. Long enough to
// find it after the scroll settles, short enough not to look like a selection — the tick
// box is what expresses "this one", and a second, stronger-looking state would compete.
const JUMP_HIGHLIGHT_MS = 1400

// jumpRank is a finding's position along the ↑/↓ walk: the FILE's index in the diff first
// (the order the groups render in), then the line inside it. Derived from the data rather
// than read off the DOM, because a folded file's rows are not in the DOM at all — and the
// arrows must still be able to reach them.
function jumpRank(f: FindingVO, fileOrder: Map<string, number>, root: string): [number, number] {
  for (const [path, i] of fileOrder) {
    if (findingMatchesFile(root, f.path, path)) return [i, f.line || 0]
  }
  // A file the diff does not show sits in the 其它文件 group, i.e. after every real group.
  return [fileOrder.size, f.line || 0]
}

// DiffFindingsPane is the 意见 + diff view: the working-tree diff of the reviewed files, with
// the review's findings anchored on the lines they name, plus the picker that turns them into
// one ordinary message.
//
// It renders as a PANE, not as an overlay. The findings, the diff they point at and the
// report that explains them belong side by side in the side-channel panel — and an overlay
// covering the conversation is exactly what this design set out to remove
// (docs/2026-09-12-desktop-oneoff-panel-design.md §5.3).
export function DiffFindingsPane({ diff, loading, findings, note, report, hasPaths, runKey, diffError, expandAll, fileList, hscroll, onSend, onOpenReport, onRerun }: {
  diff: TurnDiffVO | null
  loading: boolean
  findings: FindingVO[]
  // The run this payload belongs to (sessionId/run name). The draft and the walk are per run:
  // see the reset effect below.
  runKey: string
  // One line explaining an empty findings list: "no review yet" and "a review found nothing"
  // are different facts, and a silent pane tells the reader neither.
  note?: string
  // The report's path, when the run wrote one: the 报告 pane renders its content, and this
  // only wears it as a tooltip.
  report?: string
  // Whether a diff was even asked for. A run opened from the switcher carries no file set
  // (a review's scope lives with the turn that started it), and saying so beats an empty box.
  hasPaths: boolean
  // The fold's default when a file has no findings. False for the panel, where the diff is
  // context around the review and a wall of it buries the findings; TRUE for TurnDiffOverlay,
  // where the diff IS the content and folding it would hand the reader a list of file headers
  // with nothing to read (measured: `.diff-ln` count 0 in the smoke run — the line numbers the
  // chip promises were behind a fold nobody asked for).
  expandAll?: boolean
  // Show the file INDEX down the left of the diff — one row per changed file, and clicking one
  // jumps to it (see jumpToFile). On for the overlay, where the pane is the width of a lightbox
  // and a diff of twenty files is otherwise a wall to scroll through looking for one; off for the
  // side-channel panel, which is a column the reader drags to taste (a 210px index there would
  // take a third of it and leave the diff unreadable) and which already has the ↑/↓ walk.
  fileList?: boolean
  // ONE horizontal scroller for the whole surface, pinned to the pane's bottom (the overlay).
  // Without it, long lines scroll inside the box they are drawn in — a file's body (上下) or a
  // column's cell (左右) — which in a window-sized surface leaves the reader hunting a bar at the
  // bottom of every file, and in 左右 finds none at all (see the CSS: per-row bars are hidden
  // there). Off for the side panel: it is a column the reader drags, with no room to widen, and
  // there the inner scroller is exactly what keeps the pane from growing.
  hscroll?: boolean
  // Why the diff could not be read ("" when it was). An empty pane must not have to guess.
  diffError?: string
  // Sends the picked findings as an ordinary user message. Absent only when there is
  // nowhere to send them, and then the pane is read-only.
  onSend?: (text: string) => void
  onOpenReport?: () => void
  // Runs the review again over the same files. Present only for a run that recorded a scope
  // (a scoped review) — re-running after a fix is the natural next step from here, and the
  // record is what makes "the same files" answerable.
  onRerun?: () => void
}) {
  const counts: Record<string, number> = { bug: 0, warn: 0, info: 0 }
  for (const f of findings) counts[f.severity] = (counts[f.severity] || 0) + 1

  // What this diff IS, in the backend's own words (TurnDiffVO.Source): a turn's frozen changes, or
  // the whole working tree against HEAD. The headings and the 「不在…差异里」 badge have to say
  // which, because a whole-tree review's findings must not be read as "this turn's" — and deriving
  // it here, from the answer, keeps the pane from claiming a scope the diff does not have.
  const wholeTree = diff?.source === 'head'

  // The draft is the reader's EDITS, keyed by the finding's index in the payload; an
  // untouched finding has no entry and falls back to defaultPick. Nothing has to be seeded
  // when findings arrive, and nothing has to be reconciled when they are replaced.
  const [edits, setEdits] = useState<Record<number, FindingPick>>({})
  const picks = findings.map((f, i) => edits[i] ?? defaultPick(f.severity))
  const items = findings.map((finding, idx) => ({ finding, idx }))
  const onPick = useCallback((idx: number, next: FindingPick) => {
    setEdits((prev) => ({ ...prev, [idx]: next }))
  }, [])
  // "全选 / 全不选" writes every entry out explicitly instead of leaning on the defaults:
  // the reader said so, and it has to survive a later change of what the defaults are.
  const setAll = useCallback((checked: boolean) => {
    const next: Record<number, FindingPick> = {}
    findings.forEach((f, i) => { next[i] = { ...defaultPick(f.severity), checked } })
    setEdits(next)
  }, [findings])
  const picked = items.map((it) => ({ finding: it.finding, pick: picks[it.idx] })).filter((x) => x.pick.checked)
  // A finding can name a file the turn did not touch — the reviewer looked wider than the
  // diff scope. It has no file group to sit under, and dropping it would leave the header
  // counting findings nobody can read or tick, so it gets a group of its own.
  const outsideItems = items.filter((it) => !(diff?.files || []).some(
    (f) => findingMatchesFile(f.root || diff?.root || '', it.finding.path, f.path)))
  // Does this diff come from a session with MORE than one workspace root? A file that names a root
  // is proof enough (the backend labels only an additional one), and it is the fact EVERY row
  // needs: with one row labelled and the next bare, the bare one reads as belonging to no tree at
  // all — which is exactly the question the labels exist to answer. Single-root diffs (the
  // overwhelmingly common case) are untouched.
  const multiRoot = (diff?.files || []).some((f) => !!f.rootLabel)
  // The layout switch in the head; the groups below subscribe to the same store, so one click moves
  // every file in the pane (and the other surface's pane, see useDiffSplit).
  const split = useDiffSplit()  // The report is a file like any other, and the 报告 pane renders it with the app's own
  // markdown renderer — but it is a PANE of this panel, not another overlay stacked on top.
  const reportButton = report ? (
    <button type="button" className="diff-open" title={report}
      onClick={() => onOpenReport?.()}>报告</button>
  ) : null

  // ── Folding ─────────────────────────────────────────────────────────────────
  // fileOpen is a map of exceptions to a DERIVED default: in the panel a file with findings is
  // open (the review is what the reader came for), one without is folded (its diff is context,
  // not news) — and a pane whose diff has no findings at all (TurnDiffOverlay) passes
  // `expandAll`, because there the diff is the only content there is. Nothing has to be seeded
  // when the payload arrives, and a re-run's findings cannot fight a stored default — the same
  // shape the picking draft uses.
  const [fileOpen, setFileOpen] = useState<Record<string, boolean>>({})
  // The index is offered only when there is something to CHOOSE between: with one file it is a
  // column that says nothing and takes 210px from the diff — and a single-file diff is the common
  // case. (The pane's width cap follows the same flag, or a one-file diff would keep the extra
  // room the index never claimed.)
  const showFileList = !!fileList && (diff?.files || []).length > 1
  // A file's identity is (root, oldPath, path) — `fileKey`, which the GROUP also wears as a data
  // attribute so the file index can jump to it. Module level because of exactly that: the fold
  // state, the React key and the jump anchor have to be the same string, or the index unfolds one
  // root's file and scrolls to the other's.
  const keyOf = fileKey
  // Anchoring judges each group against ITS OWN root: that is what makes an absolute finding
  // path resolve to the right one of two same-named files. (A finding that names a bare
  // relative path is still ambiguous between such roots — the reviewer is told the roots for
  // exactly that reason, and the panel shows it under each file that answers to the name.)
  const findingsOf = (f: FileDiffVO) =>
    items.filter((it) => findingMatchesFile(f.root || diff?.root || '', it.finding.path, f.path))
  const isOpen = (f: FileDiffVO) => fileOpen[keyOf(f)] ?? (expandAll || findingsOf(f).length > 0)
  // The key of the group a finding was rendered under ("" when the diff does not show it):
  // that is what a jump has to unfold before the row exists to scroll to.
  const keyForFinding = (f: FindingVO) => {
    const files = diff?.files || []
    // Exact root match first: a finding that named an absolute path belongs to the one group
    // whose root prefixes it, and that is the group a jump has to unfold.
    const file = files.find((g) => findingMatchesFile(g.root || diff?.root || '', f.path, g.path))
    return file ? keyOf(file) : ''
  }

  // ── Jumping between findings (the ↑/↓ pair in the header) ───────────────────
  const paneRef = useRef<HTMLDivElement>(null)
  // The head, measured for its height (see the --diff-head-h block below).
  const headRef = useRef<HTMLDivElement>(null)
  // The walk order and the current position. Both are derived from the payload rather than
  // from the DOM, so a folded file's findings are still reachable (see jumpRank).
  const fileOrder = new Map<string, number>()
  ;(diff?.files || []).forEach((f, i) => fileOrder.set(f.path, i))
  const jumpOrder = [...items, ...outsideItems]
    .sort((a, b) => {
      const [fa, la] = jumpRank(a.finding, fileOrder, diff?.root || '')
      const [fb, lb] = jumpRank(b.finding, fileOrder, diff?.root || '')
      return fa - fb || la - lb || a.idx - b.idx
    })
    .map((it) => it.idx)
  const [jumpPos, setJumpPos] = useState<number | null>(null)
  const lastJumpRef = useRef<number | null>(null)
  // pendingJump drives the actual scroll: it is set by the click and consumed by the effect
  // below, i.e. AFTER React has committed the unfold the jump may have asked for. `at` makes
  // two jumps to the same finding two different states, so the second one still runs.
  const [pendingJump, setPendingJump] = useState<{ idx: number; at: number } | null>(null)

  // ── The space the pinned head occupies, for everything that hangs below it ──
  // The head is PINNED (CSS), so two things below it need its height: the file index pins at
  // `--diff-head-h` rather than 0 (a `top: 0` index slides UNDER the head and loses its first
  // rows), and a jumped-to file stops `--diff-head-h + 10px` down rather than behind the head.
  //
  // What is published is the head's BOX plus the margin below it, not just the box: the head
  // paints that 12px gap in its own background (so scrolled rows cannot show through it — see the
  // CSS), and an index pinned at the box's bottom edge would have its first row under that paint.
  // Measured rather than guessed, because the head WRAPS (title, root, note, 重新评审) and neither
  // number is a constant. Written straight to the pane's style instead of into state, for the
  // reason the panel's width drag gives: a re-render per resize would re-render every diff row.
  //
  // A LAYOUT effect, not an effect: the pinned column reads this variable through CSS, so a value
  // that arrived one paint late would leave it pinned at the `0px` fallback for that frame — the
  // index tucked under the head — and the measurement that follows (a driver, a reader's eye) can
  // catch exactly that frame. Layout effects run after the DOM is updated but BEFORE paint, which
  // is where a compensated layout belongs (the same rule the fold toggle follows).
  useLayoutEffect(() => {
    const head = headRef.current
    const pane = paneRef.current
    if (!head || !pane || !fileList) return
    // The scroller this pane lives in, whatever it is: the overlay's `.viewer-stage`, the side
    // panel's `.oneoff-body`. It is what the index's room is measured against (see --diff-room-h
    // below), and the two layouts scroll in different elements — which is exactly why the value is
    // measured here instead of being spelled `100vh` in the stylesheet.
    const scroller = pane.parentElement
    const publish = () => {
      const gap = parseFloat(getComputedStyle(head).marginBottom) || 0
      const h = Math.round(head.getBoundingClientRect().height + gap)
      pane.style.setProperty('--diff-head-h', `${h}px`)
      // Where a jumped-to file's top comes to rest: the CSS `scroll-margin-top` and the index's
      // own "which file am I on" rule both read this, so the jump and the mark cannot disagree.
      pane.style.setProperty('--diff-land', `${h + DIFF_FILE_JUMP_GAP}px`)
      // …and the room the file index may occupy: the visible height minus the head. The index's
      // column is floored at this, so its drag strip spans the whole column rather than only the
      // rows — a two-row index used to give a two-row grab target (issue #31).
      //
      // The scroller's own padding comes off too: `clientHeight` includes it, and a column 18px
      // taller than the visible area (the panel body's padding) stops being pinned exactly that
      // much before the bottom — the file list drifts up in the last stretch of the scroll.
      const cs = scroller ? getComputedStyle(scroller) : null
      const pad = cs ? (parseFloat(cs.paddingTop) || 0) + (parseFloat(cs.paddingBottom) || 0) : 0
      const room = Math.round((scroller?.clientHeight || 0) - pad - h)
      if (room > 0) pane.style.setProperty('--diff-room-h', `${room}px`)
    }
    publish()
    // A ResizeObserver, not a window listener: the height changes when the CONTENT wraps too (a
    // longer root path, a longer note), which no window event covers. Its callback riding the
    // rendering cycle is fine here — if it never arrives, the value published above still holds,
    // and the only thing a stale one costs is a few pixels of pin offset.
    const ro = new ResizeObserver(publish)
    ro.observe(head)
    // The room changes when the SCROLLER resizes (a window resize, the panel's width drag), which is
    // a different element from the head.
    if (scroller) ro.observe(scroller)
    return () => {
      ro.disconnect()
      pane.style.removeProperty('--diff-head-h')
      pane.style.removeProperty('--diff-room-h')
    }
  }, [fileList, diff])

  // ── ONE horizontal scroller for the whole pane (the overlay) ───────────────
  // Every box that can scroll sideways stops showing a bar of its own in the overlay (see the CSS)
  // and is driven from a single bar pinned to the pane's bottom instead: `hboxes` is all of them —
  // a file's body in 上下, a row's cell in 左右 — and the bar's LENGTH is the widest overflow among
  // them, so it is exactly as long as the longest line and no longer.
  //
  // Why not let each box keep its own: in a window-sized surface a bar at the bottom of every FILE
  // is a bar the reader has to go find, and in 左右 there is none at all (one per row would be a
  // wall of them). What the reader gets instead is the bar where they look for one, moving the
  // whole diff together — in 左右 that means both columns at once, which is also what keeps the
  // pairing honest while they read.
  const hbar = useRef<HTMLDivElement>(null)
  const hboxes = () => Array.from(paneRef.current?.querySelectorAll<HTMLElement>('.diff-body, .diff-side') || [])
  const hdrag = (x: number) => { for (const el of hboxes()) el.scrollLeft = x }
  const [hmax, setHmax] = useState(0)
  useLayoutEffect(() => {
    if (!hscroll) return
    // Reads layout, so it belongs in a layout effect (before paint) for the same reason the head's
    // height does. Deps: what changes the boxes — the payload, the fold, the layout. A window
    // resize does NOT (the boxes' clientWidth is the pane's), which is what the observer below is for.
    const measure = () => {
      const max = hboxes().reduce((m, el) => Math.max(m, el.scrollWidth - el.clientWidth), 0)
      setHmax(max)
      // Re-apply where the bar already is: a fold or a layout switch rebuilds the boxes at 0, and a
      // reader who had scrolled away should not be snapped back to the left edge.
      hdrag(hbar.current?.scrollLeft || 0)
    }
    measure()
    const ro = new ResizeObserver(measure)
    if (paneRef.current) ro.observe(paneRef.current)
    return () => ro.disconnect()
  }, [hscroll, diff, split, fileOpen])

  // ── The index's width (the handle between the list and the diff) ───────────
  // The same bargain the side panel's width strikes, for the same reason: what is SHOWN is the
  // reader's width clamped to the room the pane has, and the number under the pointer during a
  // drag is written straight to the element instead of into state (a state update per pointermove
  // re-renders the whole pane — every diff row — before the browser has laid the new width out).
  // The number goes back to React once, on release.
  const [width, setWidth] = useState(fileListWidth)
  const [resizing, setResizing] = useState(false)
  const listRef = useRef<HTMLElement>(null)
  const dragPxRef = useRef<number | null>(null)
  // A drag still in flight when the overlay closes would never remove its window listeners.
  const detachRef = useRef<(() => void) | null>(null)
  useEffect(() => () => detachRef.current?.(), [])
  // A render that happens mid-drag must not put React's older number back on screen.
  useLayoutEffect(() => {
    if (dragPxRef.current !== null && listRef.current) listRef.current.style.width = `${dragPxRef.current}px`
  })
  const paneWidth = () => paneRef.current?.clientWidth || 0
  const shownWidth = clampFileListWidth(width, paneWidth())

  // Rows hold their own handlers; these live here because both the drag and the arrow keys commit
  // through the same place (module memory + state), so a width set one way is remembered the other.
  const commitWidth = (px: number) => {
    fileListWidth = clampFileListWidth(px, paneWidth())
    setWidth(fileListWidth)
  }
  const startResize = (e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault()
    const start = { x: e.clientX, w: shownWidth }
    dragPxRef.current = shownWidth
    setResizing(true)
    // Listeners attach HERE rather than in an effect keyed on `resizing`: React commits that state
    // asynchronously, and a fast drag delivers its first moves before such an effect could run (the
    // side panel's drag learned this — a synthetic down+move sent back to back moved nothing).
    const widthAt = (clientX: number) => clampFileListWidth(start.w + (clientX - start.x), paneWidth())
    const move = (ev: PointerEvent) => {
      const px = widthAt(ev.clientX)
      dragPxRef.current = px
      if (listRef.current) listRef.current.style.width = `${px}px`
    }
    const finish = (ev: PointerEvent) => {
      const px = widthAt(ev.clientX)
      dragPxRef.current = null
      detach()
      setResizing(false)
      commitWidth(px)
    }
    const detach = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', finish)
      window.removeEventListener('pointercancel', finish)
      document.body.classList.remove('is-resizing')
      detachRef.current = null
    }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', finish)
    window.addEventListener('pointercancel', finish)
    document.body.classList.add('is-resizing')
    detachRef.current = detach
  }
  // The handle is a real control (role=separator), so arrow keys resize too.
  const onResizeKey = (e: React.KeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowLeft' ? -DIFF_FILELIST_KEY_STEP : e.key === 'ArrowRight' ? DIFF_FILELIST_KEY_STEP : 0
    if (!step) return
    e.preventDefault()
    commitWidth(shownWidth + step)
  }

  // ── Jumping to a file (the index on the left) ──────────────────────────────
  // The same two steps as the finding walk, for the same reason: a FOLDED file has no rows to
  // scroll to, so it is unfolded first and the scroll waits for that commit (pendingFile). The
  // landing is marked with the same flash a finding gets — in a diff this long, "did it move?"
  // is otherwise a question about 3000 pixels of identical-looking hunks.
  const [pendingFile, setPendingFile] = useState<{ key: string; at: number } | null>(null)
  // The (root, path) key of the file the reader is on — the index's highlight, driven by the click
  // and then by the scroll (see the effect below).
  const [currentKey, setCurrentKey] = useState('')
  const jumpToFile = (f: FileDiffVO) => {
    const key = fileKey(f)
    setFileOpen((prev) => ({ ...prev, [key]: true }))
    setCurrentKey(key)
    setPendingFile({ key, at: Date.now() })
  }

  // ── Which file the reader is looking at (the index's own highlight) ─────────
  // Set by a click, and then kept honest by the SCROLL: a pinned index whose highlight only moves
  // when clicked reports the reader's last jump rather than the file in front of them, and on a
  // diff this long the two part ways with one flick of the wheel.
  //
  // The rule is stated in the SAME terms as the jump, which is the whole point: a jumped-to file's
  // top stops on the line `.diff-file`'s `scroll-margin-top` names (below the pinned head), and the
  // current file is the LAST group whose top has reached that line. Reading it off the CSS
  // (`scrollMarginTop`) rather than hard-coding the number is what keeps the two in step: with a
  // measured-line rule of my own — "the first group whose bottom is still below the head" — the
  // previous file's bottom lands a few pixels UNDER the line after every jump, so clicking a row
  // marked the row above it. One line, one definition, no second constant to drift.
  //
  // The one exception is the END of the scroll, where the last file wins: a file too short to reach
  // the line (the usual shape of the last one) would otherwise never be marked while the reader is
  // looking straight at it — and clicking its row would mark the file above it, the same bug by
  // another route.
  //
  // Recomputed on a frame, not per scroll event: a wheel emits dozens per frame and each one would
  // otherwise walk every group. setCurrentKey with an unchanged key is a no-op for React, so a
  // scroll that stays inside one file re-renders nothing.
  useEffect(() => {
    if (!fileList) return
    const pane = paneRef.current
    const scroller = scrollableAncestor(pane)
    if (!pane || !scroller) return
    let raf = 0
    const measure = () => {
      raf = 0
      const groups = Array.from(pane.querySelectorAll<HTMLElement>('.diff-file'))
      if (groups.length === 0) return
      // The landing line, in viewport coordinates: where a jumped-to group's top comes to rest.
      const margin = parseFloat(getComputedStyle(pane).getPropertyValue('--diff-land')) || 0
      // +1 for rounding: a group sitting exactly on the line counts as having reached it, and the
      // next group's top is far below it, so nothing is decided by the odd pixel.
      const line = scroller.getBoundingClientRect().top + margin + 1
      let key = ''
      let last = ''
      let first = ''
      for (const g of groups) {
        // An empty key is the 其它文件 group (findings with no diff of their own): it is not a file
        // the index can point at, so it neither becomes current nor counts as the first or last one.
        const k = g.dataset.fileKey || ''
        if (!k) continue
        if (!first) first = k
        last = k
        if (g.getBoundingClientRect().top <= line) key = k
      }
      // Two ends, both of them real: nothing has reached the line yet (the diff is scrolled to its
      // TOP, where the first file's top sits below the line because the scroller has padding of its
      // own), or nothing is left to reach it (the scroll is at its END and the last file is too
      // short to climb to the line). Without these, the highlight simply disappears in both places —
      // which is what the reader saw as "clicked a file and nothing is marked".
      if (!key) key = first
      const atEnd = scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - 1
      if (atEnd && last) key = last
      setCurrentKey(key)
    }
    const onScroll = () => { if (!raf) raf = requestAnimationFrame(measure) }
    measure()
    // window + capture, not the scroller itself: `scroll` does not bubble, so a capture listener
    // at the top is the one place that hears this pane's scroller — whichever box that turns out
    // to be in the surface it is mounted in (the overlay's stage, the panel's body).
    window.addEventListener('scroll', onScroll, { capture: true, passive: true })
    return () => {
      window.removeEventListener('scroll', onScroll, { capture: true })
      if (raf) cancelAnimationFrame(raf)
    }
  }, [fileList, diff])

  useEffect(() => {
    if (!pendingFile) return
    // Found by attribute rather than by a selector built from the key: the key holds a path, and
    // a quote or a backslash in it would have to be escaped to survive `querySelector`.
    const groups = paneRef.current?.querySelectorAll<HTMLElement>('.diff-file') || []
    const el = Array.from(groups).find((g) => g.dataset.fileKey === pendingFile.key)
    if (!el) return
    // NOT smooth: an index click is meant to cross the whole diff (that is what it is for), and
    // landing instantly is what makes the flash readable as "you are here" rather than as the
    // tail of an animation. It also keeps the jump independent of the motion preference, which
    // `scroll-behavior` does not consult.
    el.scrollIntoView({ block: 'start' })
    el.classList.add('is-jump-target')
    const timer = window.setTimeout(() => el.classList.remove('is-jump-target'), JUMP_HIGHLIGHT_MS)
    return () => { window.clearTimeout(timer); el.classList.remove('is-jump-target') }
  }, [pendingFile])

  // The draft and the walk belong to the RUN whose payload they describe. Switching runs in
  // the switcher, or re-running a review, replaces the findings — and BOTH indexes point at
  // findings that no longer exist (the tick would follow a position onto an unrelated row,
  // and the walk would report a position it never took). Keyed by the run rather than by the
  // findings themselves: a LIVE run's record grows finding by finding, and a reset per
  // arrival would wipe the reader's edits while they are still writing them.
  useEffect(() => {
    setEdits({})
    lastJumpRef.current = null
    setJumpPos(null)
  }, [runKey])

  const jumpTo = (step: 1 | -1) => {    if (jumpOrder.length === 0) return
    const from = lastJumpRef.current === null ? null : jumpOrder.indexOf(lastJumpRef.current)
    // First press: ↓ starts at the first finding, ↑ at the last, so either arrow can enter
    // the walk from nothing. Afterwards the walk CLAMPS at the ends rather than wrapping —
    // a list this short is easier to trust when the ends are ends.
    const at = from === null
      ? (step === 1 ? 0 : jumpOrder.length - 1)
      : Math.max(0, Math.min(jumpOrder.length - 1, from + step))
    const idx = jumpOrder[at]
    lastJumpRef.current = idx
    setJumpPos(at)
    // The target may live in a folded file, whose rows are not rendered yet: unfold it in the
    // same update, and let the effect scroll once React has committed that.
    const target = [...items, ...outsideItems].find((it) => it.idx === idx)
    const key = target ? keyForFinding(target.finding) : ''
    if (key) setFileOpen((prev) => (prev[key] === true ? prev : { ...prev, [key]: true }))
    setPendingJump({ idx, at: Date.now() })
  }

  useEffect(() => {
    if (!pendingJump) return
    const el = paneRef.current?.querySelector<HTMLElement>(`[data-finding-idx="${pendingJump.idx}"]`)
    if (!el) return
    el.scrollIntoView({ block: 'center', behavior: 'smooth' })
    el.classList.add('is-jump-target')
    const timer = window.setTimeout(() => el.classList.remove('is-jump-target'), JUMP_HIGHLIGHT_MS)
    return () => { window.clearTimeout(timer); el.classList.remove('is-jump-target') }
  }, [pendingJump])

  return (
    <div className={`viewer-doc is-diff${showFileList ? ' has-filelist' : ''}`} ref={paneRef} tabIndex={-1}
      // Alt+↑/↓ walks the findings from anywhere inside the pane (a click on a tick box is
      // enough to put focus here) — the buttons are the pointer's way to the same walk.
      onKeyDown={(e) => {
        if (!e.altKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown')) return
        e.preventDefault()
        jumpTo(e.key === 'ArrowDown' ? 1 : -1)
      }}>
      <div className="diff-panel-head" ref={headRef}>
        <span className="diff-panel-title">{wholeTree ? '全部改动' : '本轮改动'}</span>
        {diff?.root ? <span className="diff-panel-root" title={diff.root}>{diff.root}</span> : null}
        <span className="diff-panel-note">
          {diff?.source === 'checkpoint' ? '与这一轮开始时的快照对比' : '与 git HEAD 对照'} · 行号为文件真实行号
        </span>
        {/* The layout switch. It sits in the head rather than in the sticky bar: it is a reading
            setting you touch once, and the bar is already carrying the two things a review needs
            all the time (what is picked, where you are in the findings).
            aria-pressed rather than a radio pair: these are two buttons that each state whether the
            current layout IS them, which is what a screen reader needs to read the pair. */}
        <span className="diff-layout" role="group" aria-label="diff 布局">
          <button type="button" className={`diff-layout-btn${split ? '' : ' is-on'}`}
            aria-pressed={!split} title="上下：改动的两个版本逐行对照，长行整行横着看"
            onClick={() => setDiffSplit(false)}>上下</button>
          <button type="button" className={`diff-layout-btn${split ? ' is-on' : ''}`}
            aria-pressed={split} title="左右：旧的一列、新的一列，一行一行对齐着看"
            onClick={() => setDiffSplit(true)}>左右</button>
        </span>
        {onRerun ? (
          <button type="button" className="diff-open" title="按同一批文件再评审一次（改动还可以更新）"
            onClick={onRerun}>重新评审</button>
        ) : null}
      </div>
      {/* The note is shown even when there are no findings: "no review yet" and
          "a review found nothing" are different facts, and a silent panel tells you
          neither. The report button sits next to it, because a report that exists is
          exactly what explains an empty findings list. */}
      {findings.length === 0 && note ? (
        <div className="diff-findings">
          <span className="diff-findings-hint">{note}</span>
          {reportButton}
        </div>
      ) : null}
      {findings.length > 0 ? (
        <div className="diff-findings">
          <span className="diff-findings-title">评审意见 {findings.length} 条</span>
          {(['bug', 'warn', 'info'] as const).filter((s) => counts[s] > 0).map((s) => (
            <span key={s} className={`finding-count is-${s}`}>{SEVERITY_META[s].icon} {counts[s]}</span>
          ))}
          <span className="diff-findings-hint">{note || '来自这次运行自己的 ReportFinding'}</span>
          {reportButton}
        </div>
      ) : null}
      {loading ? <div className="diff-panel-empty">读取中…</div> : null}
      {/* A failed read is its own line, above the empty-state explanation: "the diff could
          not be read" and "there is nothing to show" are different facts, and the pane used
          to print the second while meaning the first. */}
      {!loading && diffError ? <div className="diff-panel-empty">读取 diff 失败：{diffError}</div> : null}
      {!loading && !hasPaths ? (
        <div className="diff-panel-empty">
          这次运行没有记录文件清单 —— 打字发起的 /review 覆盖整棵树，没有可对照的文件集。
          某轮改动的完整 diff 请点那一轮自己的「完整 diff」（它不必有评审）
        </div>
      ) : null}
      {!loading && !diffError && hasPaths && diff?.note ? <div className="diff-panel-empty">{diff.note}</div> : null}
      {!loading && !diffError && hasPaths && !diff?.note && (diff?.files || []).length === 0 ? (
        <div className="diff-panel-empty">没有未提交的改动（可能已经提交）——「完整 diff」只能看工作树里未提交的差异</div>
      ) : null}
      {(diff?.files || []).length > 0 ? (
        <div className="diff-split">
          {/* The index: one row per changed file — grouped by working directory when the diff
              spans more than one — pinned while the diff scrolls (see the CSS — it is
              `position: sticky` inside the pane's OWN scroller rather than a second scroll
              container, because the find bar and the finding walk both bind to that scroller).
              Off in the panel, and absent with a single file (see showFileList). */}
          {showFileList ? (
            /* The column (list + handle) is what PINS, not the list: the handle has to travel with
               it, and the handle's height has to be the list's (see the CSS). */
            <div className="diff-filelist-col">
              <nav className="diff-filelist" aria-label="文件列表" ref={listRef} style={{ width: shownWidth }}>
                {fileGroups(diff?.files || [], diff?.root || '', multiRoot).map((g) => (
                  <Fragment key={g.tree}>
                    {/* One section per working directory, and only in a multi-root diff (see
                        fileGroups): the label the rows used to repeat on every line is said once,
                        here, and it stays pinned at the top of the list while its own rows scroll
                        past (the list is a scroll container of its own — see the CSS). */}
                    {multiRoot ? (
                      <div className="diff-filelist-group" title={g.tree}>
                        <span className="diff-filelist-group-name">{g.label}</span>
                        <span className="diff-filelist-group-count">{g.files.length}</span>
                      </div>
                    ) : null}
                    {g.files.map((f) => {
                      // The row the reader is ON: set by the click, kept honest by the scroll (see
                      // the effect above). aria-current is the same fact for a screen reader — the
                      // bar and the wash are what the eye gets, and colour is never the only carrier.
                      const key = keyOf(f)
                      const current = key === currentKey
                      return (
                        <button key={key} type="button"
                          className={`diff-filelist-row${current ? ' is-current' : ''}`}
                          aria-current={current ? 'true' : undefined}
                          title={fileAbsPath(f, diff?.root || '')} onClick={() => jumpToFile(f)}>
                          <span className="diff-filelist-path">{f.path}</span>
                          <DiffCounts added={f.added} removed={f.removed} />
                        </button>
                      )
                    })}
                  </Fragment>
                ))}
              </nav>
              {/* The grab strip between the index and the diff. Like the panel's, it is a real
                  focusable control (role=separator) — a drag is not the only way to widen a column,
                  and ArrowLeft/Right is what the keyboard gets. */}
              <div className={`diff-filelist-resizer${resizing ? ' is-dragging' : ''}`} role="separator"
                aria-orientation="vertical" aria-label="调整文件列表宽度" tabIndex={0}
                title="拖动调整文件列表宽度（← → 也可以）"
                onPointerDown={startResize} onKeyDown={onResizeKey} />
            </div>
          ) : null}
          <div className="diff-split-body">
            {(diff?.files || []).map((f) => (
              <FileDiffGroup key={keyOf(f)} file={f} root={diff?.root || ''} multiRoot={multiRoot}
                findings={findingsOf(f)}
                picks={picks} onPick={onPick} open={isOpen(f)} onToggle={() => setFileOpen((p) => ({ ...p, [keyOf(f)]: !isOpen(f) }))} />
            ))}
            {outsideItems.length > 0 ? (
              <div className="diff-file">
                <div className="diff-file-head">
                  <span className="diff-path">其它文件</span>
                  <span className="diff-badge" title={wholeTree ? '评审提到了这次改动之外的文件' : '评审提到了这一轮没有改动的文件'}>
                    {wholeTree ? '不在差异里' : '不在本轮差异里'}
                  </span>
                  <span className="finding-count is-bug" title="该文件上的评审意见">{outsideItems.length} 条意见</span>
                </div>
                {/* No code lines here, so the rows drop the code-column indent they get
                    when they sit under a line. */}
                <div className="finding-list">
                  {outsideItems.map((it) => (
                    <FindingRow key={it.idx} item={it} pick={picks[it.idx] ?? defaultPick(it.finding.severity)}
                      onPick={onPick} orphan />
                  ))}
                </div>
              </div>
            ) : null}
          </div>
        </div>
      ) : null}
      {/* ONE horizontal bar for the whole diff, pinned where the reader looks for it (see the
          hscroll block above). Only when something actually overflows: an empty 11px strip under
          every short diff would be a bar that does nothing. `aria-label` + tabIndex, because a
          scrollbar the reader cannot reach from the keyboard is a bar for mouse users only. */}
      {hscroll && hmax > 0 ? (
        <div className="diff-hscroll" ref={hbar} tabIndex={0} aria-label="横向滚动整份 diff"
          title="左右拖动横向查看长行（也可以在这里用 ← →）"
          onScroll={(e) => hdrag(e.currentTarget.scrollLeft)}>
          <div className="diff-hscroll-run" style={{ width: `calc(100% + ${hmax}px)` }} />
        </div>
      ) : null}
      {/* The way out AND the way through: the picked findings become one ordinary user message,
          and the ↑/↓ pair walks the findings. It is a STICKY bar rather than another row in the
          header for a reason the walk made concrete: the pane scrolls (that is how a long diff
          gets read) and a header scrolls away with it, so jumping down to a finding left the
          arrows off-screen and the reader had to scroll back up to jump again. The bar is the
          only thing that stays put.
          The action sits at the LEFT end: the pane scrolls vertically to read a long diff,
          and a button at the right end of a bar that wide is the one thing a reader could
          miss. The count it acts on stands next to it, the explanation stays at the right.
          The walk is separate from all of that on purpose: which findings are TICKED is a
          decision about sending, where you are in the list is not — so a pane with no send
          path (no onSend) still gets the arrows. */}
      {findings.length > 0 ? (
        <div className="diff-sendbar">
          {onSend ? (
            <>
              <button type="button" className="diff-sendbar-go" disabled={picked.length === 0}
                title={picked.length === 0 ? '先勾选至少一条意见' : '把勾选的意见发给 Tachi'}
                onClick={() => onSend(buildFindingMessage(picked))}>
                发给 Tachi
              </button>
              <span className="diff-sendbar-count">已选 <strong>{picked.length}</strong> / {findings.length} 条</span>
              <button type="button" className="diff-sendbar-link" onClick={() => setAll(true)}>全选</button>
              <button type="button" className="diff-sendbar-link" onClick={() => setAll(false)}>全不选</button>
            </>
          ) : null}
          {/* The walk, with its position: "where am I" has to be readable at the moment the
              jump lands, which is exactly when the header is not on screen. */}
          <span className="diff-jump">
            <button type="button" className="diff-jump-btn" title="上一条意见（Alt+↑）"
              aria-label="上一条意见" onClick={() => jumpTo(-1)}>↑</button>
            <button type="button" className="diff-jump-btn" title="下一条意见（Alt+↓）"
              aria-label="下一条意见" onClick={() => jumpTo(1)}>↓</button>
            {jumpPos !== null ? <span className="diff-jump-pos">{jumpPos + 1}/{jumpOrder.length}</span> : null}
          </span>
          {onSend ? <span className="diff-sendbar-hint">作为一条普通消息发出</span> : null}
        </div>
      ) : null}
    </div>
  )
}

// TurnDiffOverlay is the turn's own 「完整 diff」: what THIS turn changed, at reading size and
// with real file coordinates — the promise its chip makes is now only that ("本轮的完整改动，带
// 真实文件行号", with the baseline left to the head line below).
//
// The BASELINE is decided by the backend and printed by this pane's own head line: the turn's two
// checkpoint trees when it has a pair (Source "checkpoint", 「与这一轮开始时的快照对比」), else the
// working tree against HEAD limited to the declared paths (「与 git HEAD 对照」). That is not a
// detail to hard-code here — the turn-scoped REVIEW pane reads the same entry, so both surfaces
// show the same diff and a finding's line numbers mean the same thing in each.
//
// It is scoped to the CLICK rather than to a side-channel run: the paths arrive as a prop, the
// diff is fetched on open and dropped on close, and nothing about it is remembered. That is the
// whole difference from the panel's 意见 pane, which answers "what did that RUN review" and can
// therefore only diff the file set the run recorded (docs/2026-09-12-desktop-oneoff-panel-design.md
// §5.3) — a turn nobody has reviewed has no run at all, so routing this chip through the panel
// showed an empty column.
export function TurnDiffOverlay({ sessionId, turn, paths, onClose }: {
  sessionId: string
  // The turn whose changes these are. It is what the backend uses to find the turn's own two
  // checkpoint trees, so the panel shows what THAT turn changed — a shell command's writes
  // included — instead of every uncommitted difference in the files it names.
  turn: number
  // The files the footer's tool calls declared. Only the FALLBACK scope: the backend uses it
  // when the checkpoints have no pair for the turn (feature off, missing git, over its guard).
  paths: string[]
  onClose: () => void
}) {
  const [diff, setDiff] = useState<TurnDiffVO | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  // The caller's array is rebuilt on every render of the bubble that owns the chip, so the
  // fetch is keyed by the paths' CONTENT: a new array holding the same files must not re-ask.
  const files = paths.join('\n')

  useEffect(() => {
    const list = files ? files.split('\n') : []
    setDiff(null)
    setError('')
    // Nothing to ask about only when there is neither a turn nor a file list. A turn whose
    // changes came from a SHELL COMMAND declares no paths at all — and that is exactly the
    // case this panel now exists for, so bailing out on an empty list would hide it.
    if (list.length === 0 && turn === 0) {
      setLoading(false)
      return
    }
    let alive = true
    setLoading(true)
    AgentService.GetTurnChanges(sessionId, turn, list)
      .then((d) => { if (alive) setDiff(d || null) })
      .catch((e) => {
        if (!alive) return
        setDiff(null)
        // Read back for the reader instead of dropping it: an empty pane and a failed read
        // look identical, and the reason is the only thing that tells them apart.
        setError(String(e))
      })
      .finally(() => { if (alive) setLoading(false) })
    return () => { alive = false }
  }, [sessionId, turn, files])

  // The count comes from the ANSWER, not from the paths we asked with: with a checkpoint pair
  // the file set is the turn's own, and it can be larger than what the tool calls declared
  // (that is the whole point) — or smaller, for a file the tool touched but never changed.
  const count = diff?.files ? diff.files.length : (files ? files.split('\n').length : 0)
  const source = diff?.source === 'checkpoint' ? '与这一轮开始时的快照对比' : '与 git HEAD 对照'
  // A NOTE is an answer about the scope ("this turn changed nothing", "there is nothing readable
  // for it"), and the pane already renders it in its empty state. Treating that as "no scope was
  // recorded" would contradict the backend's own sentence with a second, wrong one.
  const hasScope = count > 0 || !!diff?.note
  return (
    // findings=[] on purpose: this surface is about the WORKING TREE, not about a review.
    // Everything else (the head, the per-file fold, 预览/打开) is the panel's own rendering, which
    // is what keeps the two diffs from drifting apart.
    // expandAll: with no findings to fold AROUND, every file opens — the diff is the content here,
    // not context for a review.
    // hscroll: this surface is the whole window, so the long lines get ONE bar at its bottom
    // instead of one per file (and none at all in 左右) — see the pane's hscroll block.
    //
    // selectionSource='': a diff is text worth asking about (「这段为什么这么改」), and it belongs
    // to no single file — so the source line is left out of the message, exactly as it is for a
    // paragraph selected in the transcript.
    <ViewerOverlay label={`本轮改动 — ${count} 个文件（${source}）`} onClose={onClose} stageClass="is-doc"
      overlayClass="is-flush"
      findable
      selectionSource=""
      controls={<CloseButton onClose={onClose} />}>
      <DiffFindingsPane diff={diff} loading={loading} findings={[]} runKey={`${sessionId}\n${turn}\n${files}`}
        hasPaths={hasScope} diffError={error} expandAll fileList hscroll />
    </ViewerOverlay>
  )
}

// FileDiffGroup is one file in the panel: its header (with the badges that explain
// what happened to it) and, when unfolded, its hunks.
//
// The fold is CONTROLLED by the pane: which files start folded is a rule about the review
// (a file with findings opens, one without stays shut), and the ↑/↓ walk has to be able to
// unfold a file before it can scroll to a finding inside it. Both need one owner.
function FileDiffGroup({ file, root, multiRoot, findings, picks, onPick, open, onToggle }: {
  file: FileDiffVO
  root: string
  multiRoot: boolean
  findings: PlacedFinding[]
  picks: FindingPick[]
  onPick: (idx: number, next: FindingPick) => void
  open: boolean
  onToggle: () => void
}) {
  // The file's own root wins over the panel's: a session can carry additional roots, and a
  // path under one of them is relative to THAT root (see fileAbsPath/fileRootName — shared with
  // the file index, so the list cannot name a different tree than the group it jumps to).
  const abs = fileAbsPath(file, root)
  const rootName = fileRootName(file, root, multiRoot)
  // The reader's layout (see useDiffSplit): the FILE is where the toggle applies — a tool card's
  // fragment diff stays 上下, because it lives in a column a third of the window wide.
  const split = useDiffSplit()
  // The file's own content, the way every other file in this app is shown — the same
  // PreviewFile machinery behind the attachment cards.
  const [peek, setPeek] = useState(false)
  return (
    <div className={`diff-file${open ? '' : ' is-folded'}`} data-file-key={fileKey(file)}>
      <div className="diff-file-head">
        <button type="button" className="diff-file-toggle" aria-expanded={open}
          title={open ? '收起这个文件的差异' : '展开这个文件的差异'}
          onClick={onToggle}><Caret open={open} /></button>
        <span className="diff-path" title={abs}>{file.path}</span>
        {file.oldPath && file.oldPath !== file.path ? <span className="diff-badge" title={`原路径 ${file.oldPath}`}>重命名</span> : null}
        {file.created ? <span className="diff-badge is-new">新增</span> : null}
        {file.deleted ? <span className="diff-badge is-gone">删除</span> : null}
        {file.binary ? <span className="diff-badge">二进制</span> : null}
        {/* The frozen diff was taken at the turn's end; if the file has moved since (another
            turn, the user, a commit), these hunks are no longer what is on disk — and 预览/打开
            would show text that does not match them. Saying so is the difference between a
            historical view and a wrong one. */}
        {/* Which tree this file came from. Every row says it as soon as one of them has to
            (see rootName above): a single-root diff is unchanged, and in a multi-root one no row
            is left bare. */}
        {rootName ? (
          <span className="diff-badge" title={fileTree(file, root)}>{rootName}</span>
        ) : null}
        {file.changedSince ? (
          <span className="diff-badge" title="这个文件在这一轮之后又被改过：这里显示的是当时的内容，不是磁盘上的现在">之后又改过</span>
        ) : null}
        {findings.length > 0 ? <span className="finding-count is-bug" title="该文件上的评审意见">{findings.length} 条意见</span> : null}
        <DiffCounts added={file.added} removed={file.removed} />
        {/* Two ways into the file, both borrowed from the attachment card: preview it
            here, or hand it to the system. */}
        <button type="button" className="diff-open" title="在这里预览" onClick={() => setPeek(true)}>预览</button>
        <button type="button" className="diff-open" title="用系统默认应用打开"
          onClick={() => { AgentService.OpenPath(abs).catch(() => {}) }}>打开</button>
      </div>
      {peek ? <FilePreviewOverlay path={abs} name={file.path.split('/').pop()} onClose={() => setPeek(false)} /> : null}
      {/* A folded file keeps its header (path, badges, the finding count and the two ways
          into the file) and drops the hunks — including the findings that sit on them. */}
      {!open ? null : file.binary ? (
        <div className="diff-panel-empty">二进制文件，git 不逐行比较</div>
      ) : file.hunks?.length ? (
        <DiffLines hunks={file.hunks} lineNumbers findings={findings} split={split}
          renderFinding={(item, orphan) => (
            <FindingRow item={item} pick={picks[item.idx] ?? defaultPick(item.finding.severity)}
              onPick={onPick} orphan={orphan} />
          )} />
      ) : (
        <div className="diff-panel-empty">没有内容改动（可能是权限或重命名）</div>
      )}
    </div>
  )
}
