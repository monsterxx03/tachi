// Diff rendering: two views of the same data model.
//
//   - DiffBlock — the per-call FRAGMENT diff on a tool card. Line numbers are
//     fragment-relative there, so they are not shown (a fragment has no position in
//     the file), and a long diff folds.
//   - DiffFindingsPane — the PANE the side-channel panel shows: the reviewed files'
//     working-tree diff against git HEAD (real file coordinates, so numbers ARE shown)
//     with the review's findings anchored on the lines they name.
//
// Both render the same `Hunk` lines through DiffLines, which is what keeps the two
// views from drifting: the difference is a flag, not a second implementation.

import { Fragment, memo, useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { AgentService, type FileChangeVO, type FileDiffVO, type FindingVO, type TurnDiffVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { Hunk } from '../bindings/github.com/monsterxx03/tachi/pkg/linediff'
import { FilePreviewOverlay } from './filepreview'
import { InlineMd } from './markdown'

// DIFF_FOLD_LINES is how much of a change shows before folding.
const DIFF_FOLD_LINES = 24

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
function DiffLines({ hunks, lineNumbers, findings, renderFinding }: {
  hunks: Hunk[]
  lineNumbers: boolean
  // Findings are placed inline, right under the line they are about — a review comment
  // belongs next to the code it questions.
  findings?: PlacedFinding[]
  renderFinding?: (item: PlacedFinding, orphan: boolean) => ReactNode
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

  return (
    <div className="diff-body">
      {hunks.map((h, i) => {
        const here = h.newLine ? byLine.get(h.newLine) : h.kind === 'del' ? byLine.get(h.oldLine) : undefined
        for (const item of here || []) placed.add(item.finding)
        return (
          <div key={i}>
            <div className={`diff-line is-${h.kind}`}>
              {lineNumbers ? (
                <>
                  <span className="diff-ln">{h.oldLine || ''}</span>
                  <span className="diff-ln">{h.newLine || ''}</span>
                </>
              ) : null}
              {/* The sign carries the meaning; the colour only reinforces it. */}
              <span className="diff-sign">{h.kind === 'add' ? '+' : h.kind === 'del' ? '−' : ' '}</span>
              <span className="diff-text">{h.text === '' ? ' ' : h.text}</span>
            </div>
            {(here || []).map((item, j) => <Fragment key={j}>{draw ? draw(item, false) : null}</Fragment>)}
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
export function DiffFindingsPane({ diff, loading, findings, note, report, hasPaths, runKey, diffError, onSend, onOpenReport, onRerun }: {
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
    (f) => findingMatchesFile(diff?.root || '', it.finding.path, f.path)))
  // The report is a file like any other, and the 报告 pane renders it with the app's own
  // markdown renderer — but it is a PANE of this panel, not another overlay stacked on top.
  const reportButton = report ? (
    <button type="button" className="diff-open" title={report}
      onClick={() => onOpenReport?.()}>报告</button>
  ) : null

  // ── Folding ─────────────────────────────────────────────────────────────────
  // fileOpen is a map of exceptions to a DERIVED default: a file with findings is open (the
  // review is what the reader came for), one without is folded (its diff is context, not
  // news). Nothing has to be seeded when the payload arrives, and a re-run's findings cannot
  // fight a stored default — the same shape the picking draft uses.
  const [fileOpen, setFileOpen] = useState<Record<string, boolean>>({})
  const keyOf = (f: FileDiffVO) => (f.oldPath || '') + f.path
  const findingsOf = (path: string) => items.filter((it) => findingMatchesFile(diff?.root || '', it.finding.path, path))
  const isOpen = (f: FileDiffVO) => fileOpen[keyOf(f)] ?? findingsOf(f.path).length > 0
  // The key of the group a finding was rendered under ("" when the diff does not show it):
  // that is what a jump has to unfold before the row exists to scroll to.
  const keyForFinding = (f: FindingVO) => {
    const file = (diff?.files || []).find((g) => findingMatchesFile(diff?.root || '', f.path, g.path))
    return file ? keyOf(file) : ''
  }

  // ── Jumping between findings (the ↑/↓ pair in the header) ───────────────────
  const paneRef = useRef<HTMLDivElement>(null)
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
    <div className="viewer-doc is-diff" ref={paneRef} tabIndex={-1}
      // Alt+↑/↓ walks the findings from anywhere inside the pane (a click on a tick box is
      // enough to put focus here) — the buttons are the pointer's way to the same walk.
      onKeyDown={(e) => {
        if (!e.altKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown')) return
        e.preventDefault()
        jumpTo(e.key === 'ArrowDown' ? 1 : -1)
      }}>
      <div className="diff-panel-head">
        <span className="diff-panel-title">本轮改动</span>
        {diff?.root ? <span className="diff-panel-root" title={diff.root}>{diff.root}</span> : null}
        <span className="diff-panel-note">与 git HEAD 对照 · 行号为文件真实行号</span>
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
          这次运行没有带上被评审的文件清单 —— 从被评审的那一轮点「完整 diff」进来，就会看到与 git HEAD 的对照
        </div>
      ) : null}
      {!loading && !diffError && hasPaths && diff?.note ? <div className="diff-panel-empty">{diff.note}</div> : null}
      {!loading && !diffError && hasPaths && !diff?.note && (diff?.files || []).length === 0 ? (
        <div className="diff-panel-empty">没有未提交的改动（可能已经提交）——「完整 diff」只能看工作树里未提交的差异</div>
      ) : null}
      {(diff?.files || []).map((f) => (
        <FileDiffGroup key={keyOf(f)} file={f} root={diff?.root || ''}
          findings={items.filter((it) => findingMatchesFile(diff?.root || '', it.finding.path, f.path))}
          picks={picks} onPick={onPick} open={isOpen(f)} onToggle={() => setFileOpen((p) => ({ ...p, [keyOf(f)]: !isOpen(f) }))} />
      ))}
      {outsideItems.length > 0 ? (
        <div className="diff-file">
          <div className="diff-file-head">
            <span className="diff-path">其它文件</span>
            <span className="diff-badge" title="评审提到了这一轮没有改动的文件">不在本轮差异里</span>
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

// FileDiffGroup is one file in the panel: its header (with the badges that explain
// what happened to it) and, when unfolded, its hunks.
//
// The fold is CONTROLLED by the pane: which files start folded is a rule about the review
// (a file with findings opens, one without stays shut), and the ↑/↓ walk has to be able to
// unfold a file before it can scroll to a finding inside it. Both need one owner.
function FileDiffGroup({ file, root, findings, picks, onPick, open, onToggle }: {
  file: FileDiffVO
  root: string
  findings: PlacedFinding[]
  picks: FindingPick[]
  onPick: (idx: number, next: FindingPick) => void
  open: boolean
  onToggle: () => void
}) {
  const abs = root && !file.path.startsWith('/') ? `${root}/${file.path}` : file.path
  // The file's own content, the way every other file in this app is shown — the same
  // PreviewFile machinery behind the attachment cards.
  const [peek, setPeek] = useState(false)
  return (
    <div className={`diff-file${open ? '' : ' is-folded'}`}>
      <div className="diff-file-head">
        <button type="button" className="diff-file-toggle" aria-expanded={open}
          title={open ? '收起这个文件的差异' : '展开这个文件的差异'}
          onClick={onToggle}>{open ? '▾' : '▸'}</button>
        <span className="diff-path" title={abs}>{file.path}</span>
        {file.oldPath && file.oldPath !== file.path ? <span className="diff-badge" title={`原路径 ${file.oldPath}`}>重命名</span> : null}
        {file.created ? <span className="diff-badge is-new">新增</span> : null}
        {file.deleted ? <span className="diff-badge is-gone">删除</span> : null}
        {file.binary ? <span className="diff-badge">二进制</span> : null}
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
        <DiffLines hunks={file.hunks} lineNumbers findings={findings}
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
