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
    <div className={`finding is-${finding.severity}${orphan ? ' is-orphan' : ''}${pick.checked ? '' : ' is-unpicked'}`}>
      {/* The tick is the whole contract with the message: only what is ticked is sent. */}
      <label className="finding-pick" title={pick.checked ? '取消：不写进发给 agent 的清单' : '选中：写进发给 agent 的清单'}>
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

// DiffFindingsPane is the 意见 + diff view: the working-tree diff of the reviewed files, with
// the review's findings anchored on the lines they name, plus the picker that turns them into
// one ordinary message.
//
// It renders as a PANE, not as an overlay. The findings, the diff they point at and the
// report that explains them belong side by side in the side-channel panel — and an overlay
// covering the conversation is exactly what this design set out to remove
// (docs/2026-09-12-desktop-oneoff-panel-design.md §5.3).
export function DiffFindingsPane({ diff, loading, findings, note, report, hasPaths, onSend, onOpenReport, onRerun }: {
  diff: TurnDiffVO | null
  loading: boolean
  findings: FindingVO[]
  // One line explaining an empty findings list: "no review yet" and "a review found nothing"
  // are different facts, and a silent pane tells the reader neither.
  note?: string
  // The report's path, when the run wrote one: the 报告 pane renders its content, and this
  // only wears it as a tooltip.
  report?: string
  // Whether a diff was even asked for. A run opened from the switcher carries no file set
  // (a review's scope lives with the turn that started it), and saying so beats an empty box.
  hasPaths: boolean
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

  return (
    <div className="viewer-doc is-diff">
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
      {!loading && !hasPaths ? (
        <div className="diff-panel-empty">
          这次运行没有带上被评审的文件清单 —— 从被评审的那一轮点「完整 diff」进来，就会看到与 git HEAD 的对照
        </div>
      ) : null}
      {!loading && hasPaths && diff?.note ? <div className="diff-panel-empty">{diff.note}</div> : null}
      {!loading && hasPaths && !diff?.note && (diff?.files || []).length === 0 ? (
        <div className="diff-panel-empty">没有未提交的改动（可能已经提交）——「完整 diff」只能看工作树里未提交的差异</div>
      ) : null}
      {(diff?.files || []).map((f) => (
        <FileDiffGroup key={(f.oldPath || '') + f.path} file={f} root={diff?.root || ''}
          findings={items.filter((it) => findingMatchesFile(diff?.root || '', it.finding.path, f.path))}
          picks={picks} onPick={onPick} />
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
      {/* The way out: the picked findings become one ordinary user message. It is a bar
          rather than another thing in the header because the reader decides AFTER reading
          the diff, by which point the header is far above. */}
      {findings.length > 0 && onSend ? (
        <div className="diff-sendbar">
          <span className="diff-sendbar-count">已选 <strong>{picked.length}</strong> / {findings.length} 条</span>
          <button type="button" className="diff-sendbar-link" onClick={() => setAll(true)}>全选</button>
          <button type="button" className="diff-sendbar-link" onClick={() => setAll(false)}>全不选</button>
          <span className="diff-sendbar-hint">作为一条普通消息发出，之后照常在对话里继续</span>
          <button type="button" className="diff-sendbar-go" disabled={picked.length === 0}
            title={picked.length === 0 ? '先勾选至少一条意见' : '把勾选的意见发给 agent'}
            onClick={() => onSend(buildFindingMessage(picked))}>
            发给 agent
          </button>
        </div>
      ) : null}
    </div>
  )
}

// FileDiffGroup is one file in the panel: its header (with the badges that explain
// what happened to it) and its hunks.
function FileDiffGroup({ file, root, findings, picks, onPick }: {
  file: FileDiffVO
  root: string
  findings: PlacedFinding[]
  picks: FindingPick[]
  onPick: (idx: number, next: FindingPick) => void
}) {
  const abs = root && !file.path.startsWith('/') ? `${root}/${file.path}` : file.path
  // The file's own content, the way every other file in this app is shown — the same
  // PreviewFile machinery behind the attachment cards.
  const [peek, setPeek] = useState(false)
  return (
    <div className="diff-file">
      <div className="diff-file-head">
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
      {file.binary ? (
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
