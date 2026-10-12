import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Dialogs, Events } from '@wailsio/runtime'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import {
  THINKING_LEVELS,
  PAGE_SIZE,
  type Message,
  type PermissionDecision,
  type PermissionRequest,
  type SessionItem,
} from './types'
import { buildTurns, copyText, fmtCredit, fmtDur, fmtTime, imeActive, tpsTier, actOnKey, sessionGroups, sessionGroupKey, rewindTargetForMessage, latestSessionOf, newestOf, looseMembers, nextSessionAfter, LOOSE_LABEL, LOOSE_GROUP_KEY, managedLabel, managedReadOnlyNote, type SessionRow, type SessionGroup } from './lib'
import { lastRunningAssistantIndex, lastAssistantIndex, turnView, answerTextOf, processLiveText, type IndexedPart } from './transcript'
import {
  ContextMeter, CacheRing, ProcessStrip, UserBubble, TurnMenu, MCPPanel, AgentsPanel, AskForm, PermissionForm,
  SettingsIcon, UsageIcon, MCPIcon, AgentsIcon, PinIcon, ThemeToggle, RootsPanel, Caret, MoreRows, type TurnMenuItem,
} from './components'
import { TurnPart } from './parts'
import { TurnDiffOverlay } from './diff'
import { OneOffPanel, useOneOffs, oneOffRunLabel, ONE_OFF_PANEL_DEFAULT_WIDTH } from './oneoff'
import { RewindChainOverlay } from './rewindchain'
import { ProjectPalette, SessionPalette, type ProjectChoice } from './palette'
import type { OneOffRun } from './agentEvents'
import type { OneOffVO, PeerBoardVO, ProjectVO, RewindPreviewVO, RewindTurnVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { PlanChip, PlanPanel } from './plan'
import { SettingsPage } from './settings'
import { FindBar, useFind } from './find'
import { useSidebarWidth } from './sidebar'
import { bumpFontScale, reconcileFontScale, resetFontScale } from './fontsize'
import { useTheme, useThemeHostSync } from './theme'
import { isMod, IS_MAC, MOD_KEY } from './platform'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import type { PlanVO, SessionRootsVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

import { pushPart, finishNotice, setPartDiffs, togglePartDiff, turnDiffStat } from './transcript'
import { useSessionTranscript } from './useTranscript'

import { useAgentStatus, useSessionUsage, useAgentStream, useOneOffStream, useWorkspaceChanged, useMCPAuth, useMCPReady } from './agentEvents'
import { useComposer, Composer } from './composer'
// CHAT_FIND_SCOPE is what ⌘F may find in the CONVERSATION: the reader's own words and the model's
// prose, and nothing else. Most of a session's text is tool arguments and tool output — a search over
// the whole transcript is a search over file dumps (issue #35). Positive rather than a skip list, for
// find.tsx's reason: a card type added later must not join the search by accident.
//
// The two containers are stable because they are the app's "this is prose" markers: a user bubble's
// content, and MarkdownBlock's root (every text part and every turn conclusion). Thinking rows,
// notices, process strips, tool cards, file cards and forms all live outside them.
const CHAT_FIND_SCOPE = '.msg-user .msg-content, .assistant-text'

// How many rows of ONE sidebar group are rendered at first, and how much each arrival at that
// group's tail adds (see the window state in App, and MoreRows in components.tsx). A constant rather
// than a prop or a setting: the point is to paint the newest screenful and nothing beyond it, and a
// number a reader could tune would only be a way to make the sidebar slow again.
const SIDEBAR_WINDOW_ROWS = 30

// groupKey is the sidebar's key for ONE group: the project's own id, or LOOSE_GROUP_KEY for the
// bucket. Module-level because it is pure AND because the row window's grow callback is memoized on
// it: a per-render closure here would re-create that callback, and the reveal effect — which lists
// it as a dependency — would then run on every render.
function groupKey(g: SessionGroup<SessionItem>): string {
  return g.project?.id || LOOSE_GROUP_KEY
}

// ── Memoized transcript pieces ──────────────────────────────────────────────
// Streaming replaces only the message being written (its siblings keep the
// same object references), so memoization here means every earlier turn — and
// its already-parsed markdown — is skipped on each animation frame. Without
// this, a long session re-parsed the whole transcript dozens of times per
// second, which is what made output feel jumpy.

// The markdown, the tool cards and the attachment cards are rendered by their own
// modules (markdown.tsx / components.tsx / filepreview.tsx); WHICH piece a part turns
// into is decided in parts.tsx, shared with the side-channel panel so a replayed run
// and a live turn render identically.

// useElapsedMs ticks once a second while a turn is running and reports how long it has
// been going. The number is the difference between "working" and "stuck": a strip whose
// step count has not moved is indistinguishable from a hung one without it. One ticker
// per running turn, and the turn's strip is its only reader.
function useElapsedMs(ts: string | undefined, running: boolean): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!running) return
    // Align the clock on the edge, not a second later: `now` was last written by the previous
    // turn (or by mount), so the first frame would report an elapsed time derived from a stale
    // reading.
    setNow(Date.now())
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [running])
  if (!running || !ts) return 0
  const start = Date.parse(ts)
  return Number.isNaN(start) ? 0 : Math.max(0, now - start)
}

const AssistantBubble = memo(function AssistantBubble({ m, workDir, elapsedMs, onFoldToggle, ask, onAnswer, perm, onPermAnswer, onToggleDiff, onToggleAllDiffs, onOpenDiffPanel, onReviewChanges, onReviewAll, onOpenOneOff, reviewPending, reviewDone, reviewNotice, allReviewPending, allReviewDone, allReviewNotice, allReviewFiles, sessionBusy }: {
  m: Message
  workDir: string
  // Diff interaction: one card at a time, or the whole turn from the footer chip.
  onToggleDiff?: (partIndex: number) => void
  onToggleAllDiffs?: (value: boolean) => void
  // Opens the working-tree diff panel for this turn's files (git-backed, real line
  // numbers) — the authoritative view behind the fragment diffs.
  onOpenDiffPanel?: (turn: number, paths: string[]) => void
  // The turn-level review: one click, scoped to exactly this turn's files.
  // The clicked turn's message id travels with the request, so the run's state can be shown
  // on the very footer that started it (and on no other).
  onReviewChanges?: (turn: number, paths: string[], msgId?: string) => void
  // Starts a review of the WHOLE working tree against HEAD. Passed only for the LATEST turn: that
  // subject is the tree as it is now, so it belongs to the turn the reader has just watched rather
  // than to every turn in the transcript (the turn-scoped entry above is the opposite — its
  // subject is frozen in that turn's checkpoints).
  onReviewAll?: (msgId: string) => void
  // The number of uncommitted files in the working tree, and this entry's state. allReviewFiles is
  // BOTH its gate (>0 = there is something to review) and, on a turn that changed nothing of its
  // own, the only thing the row can say — see the footer.
  allReviewFiles?: number
  allReviewPending?: boolean
  allReviewDone?: string
  allReviewNotice?: string
  // Opens the side-channel panel — where a review's process, findings and report live.
  onOpenOneOff?: () => void
  reviewPending?: boolean
  // The finished review's chip text ("已评审 3 条 · 查看"). Passed as a primitive so this
  // memoized bubble keeps a stable props shape; undefined means "no review yet".
  reviewDone?: string
  reviewNotice?: string
  // The session is mid-turn, so a review has to wait its turn.
  sessionBusy?: boolean
  // How long this turn has been running, for the strip's live row (0 = not running).
  elapsedMs?: number
  // Called AFTER a fold toggle's layout lands (see the layout effect below and
  // repinAfterFoldToggle): lets the owner keep a bottom-following reader pinned across the
  // layout change the toggle caused.
  onFoldToggle?: () => void
  // Pending AskUserQuestion questions for THIS session: the form replaces the
  // tool card that asked them, so they appear in the transcript where they belong.
  ask?: Question[] | null
  onAnswer?: (answers: Record<string, string> | null) => void
  // A bash permission request for THIS session: the confirm card replaces the tool
  // card whose call is waiting (matched by toolId, below).
  perm?: { req: PermissionRequest; busy?: boolean; err?: string } | null
  onPermAnswer?: (decision: PermissionDecision) => void
}) {
  // Render the form in place of the pending AskUserQuestion card. Any other
  // unfinished card of the same tool is left alone; the loop only ever asks one
  // question set at a time, so the first match is the one waiting.
  // Whether this turn's process timeline is open. Presentational and per turn: it is not
  // persisted, so a reloaded conversation starts folded (the design's "展开状态是展示态").
  const [processOpen, setProcessOpen] = useState(false)
  // The turn's changes: what the footer chip summarizes and what "expand all" acts on.
  const diffStat = turnDiffStat(m.parts)
  // The turn's own numbers, when the backend has them (they come from the turn's checkpoint
  // trees, so they include what NO tool declared). Falling back is not a failure mode to
  // hide: the chip says which source it used, because the two count different things.
  //
  // A checkpoint answer WITHOUT numbers is not an answer: the turn's end state can be refused
  // (a guard tripped on what the turn itself created, git failed), which arrives as a note and
  // zeroes. Taking it as the chip's data would blank the footer — and with it the two entries
  // that live in it (完整 diff / 评审本轮改动) — for a turn whose changes the tool calls DID
  // declare. So the numbers decide the source, and the note rides along to say why.
  const cp = m.changes && !m.changes.note ? m.changes : null
  const chip = cp
    ? { files: cp.files, added: cp.added, removed: cp.removed, source: 'checkpoint' as const, note: '' }
    : { files: diffStat.files, added: diffStat.added, removed: diffStat.removed, source: 'tools' as const, note: m.changes?.note || '' }
  const diffParts = (m.parts || []).filter((p) => p.type === 'tool' && p.change && p.done && p.ok)
  const allDiffsOpen = diffParts.length > 0 && diffParts.every((p) => p.diffOpen)
  // The chip's tooltip: where the numbers came from, and what clicking does. The second half
  // matters because the two can differ — a turn whose changes came from a SHELL command has no
  // fragment diffs to unfold, and its numbers are exactly the ones no fragment ever counted.
  const chipTitle = (chip.source === 'checkpoint'
    ? '本轮改动，来自检查点：与这一轮开始时的快照对比（真实 numstat，包含 shell 命令的改动）'
    : `本轮改动来自工具调用：只是片段内的行数，不是 git numstat${chip.note ? `；检查点没有给出这一轮的数字（${chip.note}）` : ''}${diffStat.shell ? '；本轮还跑了 shell 命令，那些改动不会出现在这里' : ''}`)
    + (diffParts.length === 0 ? '；点击打开完整 diff' : '；点击展开本轮所有片段 diff')

  // Whether this footer carries the 「评审所有改动」 entry. onReviewAll is passed only for the LATEST
  // turn (see the message list), and the count is its gate: 0 uncommitted files is nothing to
  // review, and offering it anyway would be a chip that can only refuse. The two RUN states are
  // added to the gate so a run that has started stays reachable — and visible — even if the tree
  // stops looking dirty under it (a /commit, or a count that came back empty for its own reasons).
  const showAllReview = !!onReviewAll && ((allReviewFiles ?? 0) > 0 || !!allReviewPending || !!allReviewDone)

  // 「复制回答」 confirms itself where it was pressed (the copy button on a code block does the
  // same). One timer, cleared on unmount: a session switch can take this bubble away mid-flash.
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const t = window.setTimeout(() => setCopied(false), 1200)
    return () => window.clearTimeout(t)
  }, [copied])

  // The menu's entries: what this turn can be ASKED for. Built here rather than inline in the JSX
  // so an entry the turn does not have is simply absent — a disabled control is a control that has
  // to explain itself, and 完整 diff on a turn that changed nothing would be exactly that.
  //
  // The labels keep their scope (本轮改动 vs 所有改动), the note carries the RUN state, and the
  // order runs from "this turn's own work" to "the tree as it is now" — the same order the two
  // review entries had on the footer.
  const menuItems: TurnMenuItem[] = []
  if (chip.files > 0) {
    menuItems.push({
      key: 'diff', label: '完整 diff',
      // The baseline is not this entry's promise: a turn with a checkpoint pair is read from its own
      // two trees (frozen — a shell command's writes are in it, and a commit since does not empty
      // it), and only a turn without one falls back to the working tree against HEAD. So the tooltip
      // names both and points at the overlay's own head line, the one place that knows which answer
      // came back — a promise made here would be a second, guessing surface.
      title: '本轮的完整改动，带真实文件行号（浮层顶部会说明这一次与什么对照：有检查点的是这一轮自己的两个快照，没有的是 git HEAD）',
      onClick: () => onOpenDiffPanel?.(m.turn ?? 0, diffStat.paths),
    })
    menuItems.push({
      key: 'review-turn', label: '评审本轮改动',
      // The note is the WHOLE state the old chip showed, 「已评审 1 条 · 查看」 included: the entry
      // still names its scope, and the state after it says both what happened and what clicking
      // does (it opens the run). 评审中… is clickable too, for the same reason it always was — that
      // is how the reader watches the run work.
      note: reviewPending ? '评审中…' : (reviewDone || ''),
      disabled: !reviewPending && !reviewDone && !!sessionBusy,
      title: reviewPending ? '评审进行中（过程在右侧面板）'
        : reviewDone ? '看这次评审的过程、意见与报告（右侧面板）'
          : sessionBusy ? '等这一轮跑完' : '让 agent 只评审本轮改动的这些文件（只读；过程与意见在右侧面板）',
      onClick: () => (reviewPending || reviewDone ? onOpenOneOff?.() : onReviewChanges?.(m.turn ?? 0, diffStat.paths, m.id)),
    })
  }
  if (showAllReview) {
    menuItems.push({
      key: 'review-all', label: '评审所有改动',
      note: allReviewPending ? '评审中…' : (allReviewDone || ''),
      disabled: !allReviewPending && !allReviewDone && !!sessionBusy,
      title: allReviewPending ? '评审进行中（过程在右侧面板）'
        : allReviewDone ? '看这次评审的过程、意见与报告（右侧面板）'
          : sessionBusy ? '等这一轮跑完' : '让 agent 评审工作树里所有未提交的改动（与 git HEAD 对照，只读；过程与意见在右侧面板）',
      onClick: () => (allReviewPending || allReviewDone ? onOpenOneOff?.() : onReviewAll?.(m.id)),
    })
  }
  // The turn's own numbers are a STAT row, not an entry: they are read once, in passing — which is
  // why they are in the menu at all instead of buying a second line under every answer.
  if (m.summary) {
    const bits: string[] = []
    if (m.summary.durationMs > 0) bits.push(`⏱ ${fmtDur(m.summary.durationMs)}`)
    if (m.summary.iterations > 0) bits.push(`${m.summary.iterations} iters`)
    if (m.summary.cost > 0) bits.push(`¥${m.summary.cost.toFixed(3)}`)
    if (m.summary.credit > 0) bits.push(`${fmtCredit(m.summary.credit)} 积分`)
    if (bits.length) menuItems.push({ key: 'detail', label: `本轮 ${bits.join(' · ')}`, title: '这一轮的耗时、迭代次数与费用', stat: true })
  }
  // What the trigger says: the state of a review, while there is one to report — a run started from
  // this footer must stay visible on the footer that started it, and its entry is behind the menu
  // now. Otherwise the dots, which are the "there is more here" mark.
  const menuLabel = (reviewPending || allReviewPending) ? '评审中…'
    : (reviewDone || allReviewDone) ? '已评审' : '⋯'

  let askShown = false
  let permShown = false
  // One part → one piece of the transcript, with the two substitutions that put a blocking
  // form IN the flow: a parked permission replaces the card of the call that is waiting
  // (matched by tool CALL ID, not by name — the model emits a whole batch of calls before
  // any of them runs, so "the newest unfinished Bash card" is usually a different call),
  // and a pending AskUserQuestion replaces the card that asked.
  const renderPart = (it: IndexedPart, key: string) => {
    const p = it.part
    if (perm && onPermAnswer && !permShown && p.type === 'tool' && !p.done && p.toolCallId === perm.req.toolId) {
      permShown = true
      return <PermissionForm key={key} perm={perm} onAnswer={onPermAnswer} />
    }
    if (ask && onAnswer && !askShown && p.type === 'tool' && !p.done && p.name === 'AskUserQuestion') {
      askShown = true
      return <AskForm key={key} questions={ask} onSubmit={onAnswer} onCancel={() => onAnswer(null)} />
    }
    // onToggleDiff carries the part's ORIGINAL index: the footer chip opens every diff of
    // the turn, folded ones included.
    return <TurnPart key={key} part={p} workDir={workDir} onToggleDiff={onToggleDiff ? () => onToggleDiff(it.index) : undefined} />
  }
  // The turn's process is folded into one strip (see turnView): the row says how much
  // happened and whether anything failed, the conclusion stays visible, and the parts that
  // must never be hidden — failures, the call that is running, a blocking form's call —
  // stay outside the fold whether or not it is open.
  const view = turnView(m.parts, { permissionToolCallId: perm?.req.toolId, pendingAsk: !!ask })
  // The fold's own layout change, on the frame it lands in. A layout effect (not an effect, not
  // a rAF): the pin then happens before the browser paints the new height, so the view never
  // gets a frame to slide up in — and it does not depend on rAF being delivered, which an
  // occluded app window does not guarantee. The first run is the mount, which has no layout
  // change to compensate for.
  const foldSettled = useRef(false)
  useLayoutEffect(() => {
    if (!foldSettled.current) {
      foldSettled.current = true
      return
    }
    onFoldToggle?.()
  }, [processOpen])
  return (
    <div className="msg msg-assistant">
      <div className="msg-avatar"><img src="/agent-avatar.png" alt="" draggable={false} /></div>
      <div className="msg-content">
        <div className="turn-parts">
          {/* The strip renders for the whole turn: while it RUNS it is the turn's one status line
              (a call in flight, or what the model is doing), and once it is over it is the summary
              of what happened. A turn that is still running its first call has nothing FOLDED yet,
              and that is exactly when the status matters most — hiding the row then made a busy
              turn look empty, and it is also why this row now replaces the second status line that
              used to sit under the prose being written (one turn, one status). */}
          {view.folded.length > 0 || m.running ? (
            <div className="process-block">
              <ProcessStrip summary={view.summary} open={processOpen}
                onToggle={() => setProcessOpen((v) => !v)}
                foldable={view.folded.length > 0}
                running={!!m.running}
                status={processLiveText(view.live, view.phase, view.unfinished)}
                elapsedMs={elapsedMs} />
              {processOpen && view.folded.length > 0
                ? <div className="process-timeline">{view.folded.map((it) => renderPart(it, `f${it.index}`))}</div>
                : null}
            </div>
          ) : null}
          {/* Exposed parts and the conclusion, in their ORIGINAL order. The strip summarizes
              the whole turn and goes first, but everything visible keeps the sequence the
              parts have: pinning the conclusion last would move a mid-turn preamble BELOW the
              call that follows it (the live window of every text-then-tool turn). */}
          {[...view.exposed, ...(view.conclusion ? [view.conclusion] : [])]
            .sort((a, b) => a.index - b.index)
            .map((it) => renderPart(it, `v${it.index}`))}
          {/* The turn's DELIVERABLES: the files a SendFile call handed over, pinned here rather
              than where they happened. They are not process — folding them put the answer to
              "把 X 发给我" behind a row the reader had to open — so they are grouped at the tail,
              after the conclusion, in the order they were sent. See turnView's rule. */}
          {view.attachments.length > 0 ? (
            <div className="turn-files">
              {view.attachments.map((it) => renderPart(it, `a${it.index}`))}
            </div>
          ) : null}
        </div>
        {/* Fallback: a pending ask with no matching tool card (e.g. the card was
            closed by an interruption) still has to be answerable. */}
        {ask && onAnswer && !askShown && m.running ? (
          <AskForm questions={ask} onSubmit={onAnswer} onCancel={() => onAnswer(null)} />
        ) : null}
        {/* Same fallback for a parked permission: a turn must never be left waiting on a
            card that is not on screen. */}
        {perm && onPermAnswer && !permShown && m.running ? (
          <PermissionForm perm={perm} onAnswer={onPermAnswer} />
        ) : null}
        {/* The turn's running state is the strip's (see above) — there is deliberately no second
            status row here. It used to sit exactly here, under the prose being written, saying
            正在思考…/正在执行… while the strip above named the call in flight: two rows, one fact,
            and the reader's eye is on THIS one while the answer streams. */}
        {!m.running && m.stopped ? <span className="stopped-note"><span className="stop-square">⏹</span> 已停止</span> : null}
        {chip.files > 0 || showAllReview || !!m.ts ? (
          <div className="msg-footer">
            {/* The answer is the thing a reader takes away from a turn, and copying it used to mean
                selecting across the prose by hand — the tool cards and notices that surround it make
                that a careful drag. It copies the MODEL'S OWN PROSE (transcript.ts's answerTextOf),
                never the arguments or the outputs: "copy this" is about what it said.
                Not offered while the turn runs: there is no finished answer to take yet. */}
            {!m.running ? (
              <button type="button" className="footer-btn" title="复制这一轮的回答文字（只含模型的正文，不含工具参数与工具输出）"
                onClick={() => { copyText(answerTextOf(m.parts)); setCopied(true) }}>
                {copied ? '已复制' : '复制回答'}
              </button>
            ) : null}
            {/* The turn's changes stay a FIRST-CLASS entry rather than moving into the menu: it is
                the one thing here a reader scans the transcript FOR, and its numbers are the reason
                the entry exists. Clicking it is still the light touch — it unfolds this turn's
                fragment diffs (or opens the full diff when there are no fragments to unfold). */}
            {chip.files > 0 ? (
              <button type="button" className="diff-chip"
                title={chipTitle}
                onClick={() => {
                  // Nothing to unfold: the turn's changes came from a shell command (or from a
                  // form the fragments cannot express), so the only place they are readable is the
                  // full diff. Toggling here would open the fold onto an empty list.
                  if (diffParts.length === 0) {
                    onOpenDiffPanel?.(m.turn ?? 0, diffStat.paths)
                    return
                  }
                  // The diffs live inside their tool cards, and those cards sit in the fold:
                  // opening every diff without opening the fold would look like a dead click.
                  if (!allDiffsOpen) setProcessOpen(true)
                  onToggleAllDiffs?.(!allDiffsOpen)
                }}>
                🧾 {chip.files} files
                {chip.added > 0 ? <span className="diff-count is-add">+{chip.added}</span> : null}
                {chip.removed > 0 ? <span className="diff-count is-del">−{chip.removed}</span> : null}
                {chip.source === 'tools' && diffStat.shell ? <span className="diff-chip-shell">· 含 shell</span> : null}
                {chip.source === 'tools' ? <span className="diff-chip-shell">· 来自工具调用</span> : null}
              </button>
            ) : null}
            {/* The entries about the turn's work: the full diff, the two review scopes, and the
                turn's own numbers. They were a row of chips — two of them whole sentences about
                review scope — under every answer in the transcript. The trigger carries the review
                STATE, because the entry that says 评审中… is behind the menu and a run started here
                must still be reported here. */}
            {menuItems.length > 0 ? (
              <TurnMenu items={menuItems} label={menuLabel}
                title="更多：本轮的完整 diff、评审，以及这一轮的耗时与费用" />
            ) : null}
            {/* A turn that changed nothing of its OWN still carries the whole-tree review entry,
                because the working tree's changes (from earlier turns) are exactly what it reviews —
                and without this row they would be unreachable from the transcript. The menu item
                then has no 🧾 chip to explain itself, so the count does the explaining: a bare
                「评审所有改动」 under a turn that changed nothing invites the reader to ask
                "review what?". */}
            {showAllReview && chip.files === 0 ? (
              <span className="diff-hint">工作树里还有 {allReviewFiles} 个未提交的改动（来自更早的轮次）</span>
            ) : null}
            {/* A refused review reports itself OUTSIDE the menu: it is the answer to "I clicked
                review and nothing happened", and it must be readable without opening anything. */}
            {reviewNotice ? <span className="diff-notice" title={reviewNotice}>⚠ 没有开始评审</span> : null}
            {allReviewNotice && allReviewNotice !== reviewNotice
              ? <span className="diff-notice" title={allReviewNotice}>⚠ 没有开始评审</span> : null}
            {/* When it happened, on the row that already exists because of it. */}
            {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
          </div>
        ) : null}
      </div>
    </div>
  )
})

function App() {
  const [sessions, setSessions] = useState<SessionItem[]>([])
  const [currentId, setCurrentId] = useState<string>('')
  // The session whose row the sidebar has to bring into view, for the one render that needs it (see
  // the two effects below). '' = nothing pending. It is the session ON SCREEN, set by the effect
  // that watches `currentId` — not by whoever happened to switch it.
  const [revealRow, setRevealRow] = useState('')
  // The scrolling box the rows live in, so the reveal can find them without asking the document
  // (the transcript has rows of its own — see .session-list in layout.css).
  const sessionListRef = useRef<HTMLElement | null>(null)
  const [menu, setMenu] = useState<{ sid: string; x: number; y: number } | null>(null)
  // The delete confirmation. `error` holds the backend's refusal (a session with a turn
  // in flight cannot be deleted) so it lands in the box that asked the question, next to
  // the button that was just pressed — a console-only failure would read as a no-op.
  const [confirmDel, setConfirmDel] = useState<{ sid: string; title: string; error?: string } | null>(null)
  // The delete box's element. Focus is TAKEN here explicitly rather than with `autoFocus`: that is an
  // attribute, and WebKit honours it on form controls — on a div it does nothing, which is exactly the
  // trap this box has to avoid (its Enter means 确认; left in the composer, the same key sends a message).
  const confirmBoxRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    if (confirmDel) confirmBoxRef.current?.focus()
  }, [confirmDel])
  // The rewind chain: this session's rewind points, from the checkpoint manifest. It is a
  // second ENTRY to the same card (the bubble's menu is the first), not a second rewind path —
  // hence `turns` here is only what the list renders, and a row's click goes through
  // openRewindCard like the menu item does.
  const [chain, setChain] = useState<{
    sid: string; title: string; turns: RewindTurnVO[]; blocked?: string; position?: number
  } | null>(null)
  // Rewind: the bubble's own menu (with the session's checkpoint boundaries, fetched when
  // it opens) and the confirmation card that shows what would be restored and — more to
  // the point — what would be DELETED, before anything moves.
  const [rewindMenu, setRewindMenu] = useState<{ x: number; y: number; msg: Message; sid: string; turns: RewindTurnVO[] | null } | null>(null)
  const [rewindCard, setRewindCard] = useState<{ sid: string; turn: number; preview: RewindPreviewVO; error?: string } | null>(null)
  const [shortcutsOpen, setShortcutsOpen] = useState(false)
  // ⌘P / ⌘K: the two palettes. Neither is about the session on screen — they are the way to GET
  // to one — so they hold no per-opening state here (each palette owns its query, its highlight
  // and, for ⌘P, its refusal notice; see palette.tsx). Only ONE is ever open: they are the same
  // box in the same place, so ⌘K closes ⌘P and vice versa.
  const [paletteOpen, setPaletteOpen] = useState(false)
  // ⌘K: the session palette. It reads the list the sidebar already renders — no fetch — and picks
  // a conversation by TITLE; ⌘P is the project one.
  const [sessionPaletteOpen, setSessionPaletteOpen] = useState(false)
  const [reminderModal, setReminderModal] = useState<string | null>(null)
  const [editingId, setEditingId] = useState('')
  const [editTitle, setEditTitle] = useState('')
  // Which folded compaction chains the reader opened, by the chain head's session id. Closed by
  // default — the chain is one conversation, and its history is a click away, not the thing the
  // sidebar is for. Not persisted, like every other disclosure state.
  const [openChains, setOpenChains] = useState<Record<string, boolean>>({})
  const [, setCurrentTitle] = useState<string>('Tachi')
  // Auto-follow state: true while the view is parked at the bottom. Scrolling up flips it
  // off (and surfaces the "jump to latest" button) so incoming streaming output no longer
  // yanks the transcript back down under the user. Declared up here because the transcript
  // store below scrolls through it (a steered message follows the same rule).
  const chatRef = useRef<HTMLDivElement>(null)
  // The transcript's content box (see the JSX): what the follow-the-bottom observer watches.
  const chatContentRef = useRef<HTMLDivElement>(null)
  const followBottomRef = useRef(true)
  // The scroll position the app last accounted for: the one its own pins wrote, or the one the
  // last scroll event reported. handleScroll reads the reader's movement against it (see there).
  const lastTopRef = useRef(0)
  const [showJump, setShowJump] = useState(false)

  // pinBottom puts the newest message in view — and RECORDS where that landed, so the scroll event
  // the browser fires for this very assignment can never be read as the reader moving the view.
  const pinBottom = useCallback(() => {
    const el = chatRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
    lastTopRef.current = el.scrollTop
  }, [])

  const scrollToBottom = useCallback((force = false) => {
    // force=true is for actions where following is clearly intended (sending a message,
    // switching sessions). Otherwise the pin only happens while the user is parked at the
    // bottom.
    if (force) {
      followBottomRef.current = true
      setShowJump(false)
    }
    if (followBottomRef.current) pinBottom()
  }, [pinBottom])

  // scrollByHalfPage is THE page-turn: Ctrl+U/Ctrl+D in the conversation, the same pair inside the
  // composer (composer.tsx claims it there, since a keydown born in the input box never reaches the
  // chat area's handler), and the badge's short up/down press (desktop/remote.go turns that gesture
  // into a chat:scroll event) — one function, so "翻页" cannot mean two different things depending
  // on where the finger is.
  const scrollByHalfPage = useCallback((delta: number) => {
    const el = chatRef.current
    if (!el) return
    el.scrollTop += delta * el.clientHeight * 0.5
  }, [])

  // …and this is the device's end of it: the badge reports a gesture, the host (remote.go) says
  // which way, and the scrolling itself is the same half page as above. A scroll the reader did
  // not perform is still a scroll AWAY from the bottom, so handleScroll ends following here too —
  // which is what "翻回去读上文" should do.
  useEffect(() => {
    const off = Events.On('chat:scroll', (event) => {
      const delta = Number((event.data as { delta?: number } | undefined)?.delta ?? 0)
      if (delta !== 0) scrollByHalfPage(Math.sign(delta))
    })
    return () => off?.()
  }, [scrollByHalfPage])

  // Per-session transcript state — messages, running flags, the loaded window, the delta
  // buffer — has exactly one owner (useTranscript.ts), so "belongs to a session" is a
  // property of the type rather than a convention to remember.
  const {
    msgCache, runningSet, hasMore, earliestTs,
    updateSession, patchMessage, applyToSession, sealRunningSegment, injectSteerVisual,
    holdLocal, releaseLocal,
    setSessionPage, prependSessionPage, markHistoryEnd, openSession, rekeySession,
    refreshRunning, markRunning, enqueueDelta, flushDeltas,
  } = useSessionTranscript(currentId, scrollToBottom)
  // The displayed session's own status (useAgentStatus keeps one per conversation), plus this
  // frontend's own running set.
  const state = useAgentStatus(currentId)
  // The statusbar's numbers (cost/credit/缓存环/速率/上下文环) belong to the session on
  // screen; they are cleared before the next session's ledger is read (see useSessionUsage).
  const {
    cost, credit, cacheHitRate, hasCacheHit, tps, lastTps, ctxEstimate, ctxWindow,
    refresh: refreshUsage, clear: clearUsage, resetRate: resetTps,
  } = useSessionUsage(currentId)
  const [loading, setLoading] = useState(false)
  const [providers, setProviders] = useState<any[]>([])
  const [providerName, setProviderName] = useState('')
  const [thinkingLevel, setThinkingLevel] = useState('none')
  // The levels the active provider's endpoint accepts: from the backend
  // (GetProviderInfo), falling back to the full list until a session answers.
  const [thinkingLevels, setThinkingLevels] = useState<string[]>(THINKING_LEVELS)
  const [workDir, setWorkDir] = useState('')
  // Workspace roots (primary + additional) and the popover that manages them. The
  // popover is only mounted while open, so its own outside-click/Esc handling is
  // not running behind the scenes.
  const [roots, setRoots] = useState<SessionRootsVO | null>(null)
  const [rootsOpen, setRootsOpen] = useState(false)
  const [rootsBusy, setRootsBusy] = useState(false)
  const [rootsError, setRootsError] = useState('')
  // Projects: the sidebar's groups (design §7.1). Fetched, never derived from the sessions —
  // a project with no members yet still has a header, and its name is joined in at render
  // time so a rename needs no session write.
  const [projects, setProjects] = useState<ProjectVO[]>([])
  const [projectMenu, setProjectMenu] = useState<{ id: string; x: number; y: number } | null>(null)
  // Renaming happens in the header itself, like a session's row.
  const [editingProject, setEditingProject] = useState<{ id: string; name: string } | null>(null)
  // A refused project rename (empty or duplicate name) is shown under the header's own input:
  // the session rows swallow their rename errors because the backend cannot refuse one, and
  // this one can.
  const [projectNotice, setProjectNotice] = useState<{ id: string; msg: string } | null>(null)
  // The project editor: 新建项目 (§7.3) and 编辑项目 (§7.2) are ONE form — the second is the
  // first with the fields filled in — so a project can be created and later corrected without
  // two different dialogs disagreeing about what a valid root is.
  const [projectForm, setProjectForm] = useState<{
    mode: 'create' | 'edit'; id?: string; name: string; primary: string; additional: string[]; error?: string
  } | null>(null)
  // Deleting a project DETACHES its members, so the box says how many sessions that is.
  const [confirmDelProject, setConfirmDelProject] = useState<{ id: string; name: string; count: number; error?: string } | null>(null)
  // Which groups are OPEN, by group key (a project id, or LOOSE_GROUP_KEY for the 无项目 bucket).
  // EMPTY means every group is folded, and that is the startup state: with several projects the
  // sidebar opens as a list of headers, and a project created later starts folded too — because the
  // default IS the empty map, not a seeded one. Memory only, like openChains: a reopened window is
  // folded again.
  //
  // The direction matters: a set of COLLAPSED ids would need every project pre-filled at load
  // time, and the ones that arrive later (a new project, a list still loading) would leak open.
  const [expandedProjects, setExpandedProjects] = useState<Record<string, boolean>>({})
  // focusGroup decides which ONE group is open: the one holding the session on screen. Every way of
  // landing in a session — a click on a row, ⌘K / ⌘P, a creation, the session restored at boot —
  // asks for that group, and the others fold. The list is then a list of that conversation's
  // neighbours: the reader who has just moved elsewhere is not reading the project they left, and a
  // sidebar that keeps the old group open spends the screen on rows nobody is looking at.
  //
  // It REPLACES the map rather than adding a key, which is the whole rule. Two consequences worth
  // stating: a group the reader opened by hand (they may open a second one to look around) survives
  // until their next switch — opening a group is their own doing, and nothing here closes one that
  // no switch touched; and 收起你正坐着的那个组 stays their call, because folding a group is not a
  // switch and this function only runs on one.
  //
  // It takes a KEY, not a project id: the 无项目 bucket is a group on the same terms.
  const focusGroup = useCallback((key?: string) => {
    if (!key) return
    setExpandedProjects((p) => (p[key] && Object.keys(p).length === 1 ? p : { [key]: true }))
  }, [])
  // Bringing the row of the session ON SCREEN into view is the sidebar's own job: "the active row"
  // and "a row the reader can see" are two different things, and every way of switching sessions can
  // land on the second one — ⌘K / ⌘P pick a conversation wherever it sits, a creation puts the new
  // row at the HEAD of its group while the 无项目 bucket is at the FOOT of the list, a delete hands
  // the reader to a neighbour, and compaction moves the conversation to a brand-new row. A click on a
  // row never needs it (the pointer is already there) and still gets the same call, because the rule
  // is one rule: the switch is what asks, not the gesture that caused it.
  //
  // It is a REQUEST rather than a scroll at the moment of the switch (the row may not exist yet — the
  // list is fetched a beat later, and a folded group has to open first): `revealRow` holds the id
  // until a render actually HAS that row, then the effect below scrolls and clears it. Clearing on
  // the FOUND row is what keeps this honest — an ordinary re-render (a streamed delta, a title
  // arriving, a list refresh) must never move the list under the reader's hands.
  useEffect(() => {
    if (currentId) setRevealRow(currentId)
  }, [currentId])

  // The sidebar renders a WINDOW of each group rather than the whole group: a conversation list
  // grows without bound (this machine's own is 100+ project-less sessions), and the reader is
  // looking at the newest handful — the rest is scroll nobody asked to paint. `rowWindow` is how
  // many rows of ONE group are shown, keyed the way the groups are (project id, or the bucket's own
  // key), memory only, and it only ever GROWS within a session: folding is expandedProjects'
  // business (see toggleGroup), and a reader who has scrolled to the end of a group did not ask
  // for it to be re-hidden. `MoreRows` (components.tsx) is the line that grows it.
  //
  // The grouping is memoized because the reveal effect below reads it: `sessionGroups` builds a
  // fresh array every call, and a dependency that changes on every render would run that effect (and
  // its DOM query) on every render — during a turn, on every streamed frame.
  const groups = useMemo(() => sessionGroups(sessions, projects), [sessions, projects])
  // The 无项目 bucket's own count: a project carries its member count in its record, the bucket has
  // no record of its own to ask, so it counts the sessions the sidebar files under it. That is
  // `looseMembers` — the same membership ⌘P's 无项目 row shows — so the two surfaces cannot report
  // two different sizes for the one group (and it is memoized because this renders during a turn,
  // on every streamed frame, over a list with no upper bound).
  const looseCount = useMemo(() => looseMembers(sessions, projects).length, [sessions, projects])
  const [rowWindow, setRowWindow] = useState<Record<string, number>>({})
  const shownRows = (g: SessionGroup<SessionItem>) => {
    const n = rowWindow[groupKey(g)] ?? SIDEBAR_WINDOW_ROWS
    return g.rows.length > n ? g.rows.slice(0, n) : g.rows
  }
  const growRows = useCallback((g: SessionGroup<SessionItem>) => {
    setRowWindow((w) => {
      const key = groupKey(g)
      return { ...w, [key]: (w[key] ?? SIDEBAR_WINDOW_ROWS) + SIDEBAR_WINDOW_ROWS }
    })
  }, [])

  // The scroll itself. `groups` and `expandedProjects` are dependencies because between them they
  // are what makes the row EXIST (the list arrives inside `groups`; opening the group is the other
  // half) and `rowWindow` is the third, for the same reason one layer down. Without them a switch
  // into a folded group, or past its window, would find no row — and the pending request would sit
  // there until something unrelated happened to change the list. `growRows` is memoized on nothing,
  // so it never asks for a run of its own.
  useEffect(() => {
    if (!revealRow) return
    const row = Array.from(sessionListRef.current?.querySelectorAll<HTMLElement>('.session') || [])
      .find((el) => el.dataset.sid === revealRow)
    if (!row) {
      // Not rendered YET, and there are two reasons — both answered here, or the request sits until
      // something unrelated changes the list:
      //   • the group is FOLDED. Growing cannot help (a folded group renders no rows at all), so
      //     nothing happens and the reader has the header's ● marker, which names the session.
      //   • the row is beyond its group's WINDOW. A row that was never rendered is not a row this
      //     effect can scroll to, so the window grows — the same rule as opening the group, one
      //     layer down. Every way of switching lands here: ⌘K / ⌘P pick the OLDEST conversation in
      //     the list, a delete hands over to a neighbour, compaction makes a brand-new row.
      const owner = groups.find((g) => g.rows.some((r) => r.session.id === revealRow))
      if (owner && (!owner.project || expandedProjects[owner.project.id])) growRows(owner)
      return
    }
    setRevealRow('')
    // 'nearest' and not 'center': a row already on screen must not move the list at all, and the
    // reader's own scroll position is not this feature's to take.
    row.scrollIntoView({ block: 'nearest' })
  }, [revealRow, groups, expandedProjects, rowWindow, growRows])

  // bootOpenedGroup makes the boot's exception a STARTUP fact: loadAll also runs after a delete,
  // and re-applying it there would re-open a group the reader had just collapsed.
  const bootOpenedGroup = useRef(false)
  // The turn whose 「完整 diff」 is open: the files that turn changed, with the session they
  // belong to. Owned here because the chip that opens it lives in the transcript, and closed on
  // every session switch (a diff is about the session it was taken in).
  //
  // The REVIEW's findings and its diff are the side-channel panel's business instead: the panel
  // reads them per RUN (desktop/oneoff.go collects each record's own ReportFinding calls), while
  // this overlay answers a question about a TURN — "what did this turn change, with real line
  // numbers" — which is answerable with no run at all. WHICH baseline answers it (the turn's own
  // two checkpoint trees, or the working tree against HEAD limited to the declared files) is the
  // backend's call, and the overlay's head line is where the reader learns which one came back.
  const [turnDiff, setTurnDiff] = useState<{ sessionId: string; turn: number; paths: string[] } | null>(null)
  useEffect(() => { setTurnDiff(null) }, [currentId])

  // The plan panel (P1): this session's newest plan. Read from disk on demand — the
  // agent:plan event only says "re-read it", so the panel can never drift from the file
  // that IS the document. Same lifetime as the diff panel: closed and re-read when the
  // session changes.
  const [plan, setPlan] = useState<PlanVO | null>(null)
  const [planOpen, setPlanOpen] = useState(false)
  const refreshPlan = useCallback(async (path = '') => {
    if (!currentId) { setPlan(null); return }
    try {
      setPlan(await AgentService.GetPlan(currentId, path) || null)
    } catch {
      setPlan(null)
    }
  }, [currentId])
  useEffect(() => { setPlanOpen(false); void refreshPlan() }, [currentId, refreshPlan])
  useEffect(() => {
    const off = Events.On('agent:plan', () => { void refreshPlan() })
    return () => off?.()
  }, [refreshPlan])

  // deletePlan removes one of this session's plans, then re-reads: the list shrinks, and
  // if the plan being shown was the one deleted the newest remaining takes its place
  // (the backend validates the path against this session's files — see DeletePlan).
  const deletePlan = useCallback(async (path: string) => {
    await AgentService.DeletePlan(currentId, path)
    void refreshPlan()
  }, [currentId, refreshPlan])

  // A review run started from a turn's footer. Local to the app (not persisted) and keyed by
  // THE TURN WHOSE BUTTON WAS CLICKED (its message id) — and by WHICH of that turn's two entries
  // it was (ReviewSlot.whole): one footer now carries 「评审本轮改动」 and 「评审所有改动」, and state
  // keyed by the turn alone would have them report each other's runs. Un-scoped, the pending state
  // and its refusal reason also appeared on every other turn's footer — and, because the session's
  // messages are re-rendered on a switch, on every other session's as well. A message id is unique
  // across sessions, so no bubble can show another turn's answer.
  const [reviewPending, setReviewPending] = useState<ReviewSlot | null>(null)
  // The finished review, for the footer chip of the turn that started it. It survives the
  // pending state (which the session going idle clears) so the chip can say 已评审 N 条.
  const [reviewResult, setReviewResult] = useState<(ReviewSlot & { run: OneOffRun }) | null>(null)
  // Which slot the run in flight belongs to, held across the result event (see startReview).
  const reviewForRef = useRef<ReviewSlot | null>(null)
  const [reviewNotice, setReviewNotice] = useState<(ReviewSlot & { text: string }) | null>(null)
  // The chip's "+N" badge: the chip itself shows the primary path, so this is what
  // says the workspace extends beyond it.
  const extraRootCount = (roots?.additional || []).length
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  // The session list's width: the reader drags its edge, and the choice is remembered across
  // restarts (desktop_ui.json). See sidebar.tsx — the number under the pointer during a drag is NOT
  // state, and what is shown is the chosen width clamped to the room the window has.
  const sidebarW = useSidebarWidth(sidebarCollapsed)
  // Which of the two top-level views is on screen. It is App's state, not a route, and the chat
  // view is HIDDEN rather than unmounted while settings is up: a conversation holds more local
  // state than a route could rebuild (collapsed project groups, the composer's draft, the
  // transcript's scroll offset), and coming back from a settings page must not cost any of it.
  const [view, setView] = useState<'chat' | 'settings'>('chat')
  // ⌘F over the conversation. It is the LOWEST host in find.tsx's stack (the chat mounts first), so
  // any overlay or panel on top keeps the key for itself — the rule that lets a preview or a review's
  // pane search its own text. No root while the settings page is up: that view COVERS the chat (the
  // body is only inert), and a bar opening behind it would be a search nobody can see.
  const chatFind = useFind(view === 'chat' ? chatRef : null, CHAT_FIND_SCOPE)
  // Theme (light/dark) for the titlebar switch; useThemeHostSync mirrors the
  // active theme to Go, which owns the window colour outside the webview.
  const [theme, toggleTheme] = useTheme()
  useThemeHostSync()
  const [mcpOpen, setMcpOpen] = useState(false)
  // The node board: which machines this one may delegate to, and what each says about itself. The
  // data is read from the backend and NOT cached here — every field the panel shows is a fact about
  // a moment (who answered just now), and a copy kept across turns is exactly what the refresh
  // button exists to rule out.
  const [agentsOpen, setAgentsOpen] = useState(false)
  const [agentsBoard, setAgentsBoard] = useState<PeerBoardVO | null>(null)
  const [agentsBusy, setAgentsBusy] = useState(false)
  // The side-channel panel (third column): /review and /commit run as one-off forks, and
  // their process belongs here rather than in the conversation — see
  // docs/2026-09-12-desktop-oneoff-panel-design.md. The panel is a reader over the same
  // records the findings panel reads, so it needs no live stream of its own until P2.
  const [oneoffOpen, setOneoffOpen] = useState(false)
  // What the backend reports about the session's side-channel runs: `live` while one is
  // running, `result` once it ended (see agentEvents.ts). The panel needs the first, the
  // conversation's one-line anchor the second.
  const oneoffRun = useOneOffStream(currentId)
  const oneoff = useOneOffs(currentId, oneoffOpen, oneoffRun.live)

  // The panel's open/closed state is a desktop preference (desktop_ui.json): "I keep it open"
  // has to survive a restart like every other layout choice. Read once at mount — the render is
  // driven by the state above, the file is only what it is restored FROM.
  const [oneoffWidth, setOneoffWidth] = useState(ONE_OFF_PANEL_DEFAULT_WIDTH)
  useEffect(() => {
    AgentService.GetUIState()
      .then((st) => {
        if (st?.oneOffPanelOpen) setOneoffOpen(true)
        // 0 = never dragged. Anything the layout cannot honour is refused by the backend too.
        if (st?.oneOffPanelWidth) setOneoffWidth(st.oneOffPanelWidth)
      })
      .catch(() => { /* an unreadable preference just means the defaults */ })
  }, [])
  const setOneOffOpen = useCallback((open: boolean) => {
    setOneoffOpen(open)
    // Best effort, like every other uiState write: a failure costs the next launch the
    // panel's position, nothing more.
    AgentService.SetOneOffPanelOpen(open).catch(() => {})
  }, [])
  // The width follows the pointer during a drag (state only) and is written when the drag ends:
  // one small file write per gesture instead of one per frame.
  const commitOneOffWidth = useCallback((px: number) => {
    setOneoffWidth(px)
    AgentService.SetOneOffPanelWidth(px).catch(() => {})
  }, [])
  const [mcpServers, setMcpServers] = useState<any[]>([])
  const [mcpLoading, setMcpLoading] = useState<Record<string, boolean>>({})
  const [mcpProfile, setMcpProfile] = useState<{ active: string; available: string[] }>({ active: '', available: [] })
  // Per-session message pagination (how much of a session's history is loaded, and the
  // "load earlier" cursor) lives in the transcript store with the messages it describes.
  const messages = msgCache[currentId] || []

  // Vim-like keys while the message area has focus: G jumps to the newest
  // message, gg returns to the top, Ctrl+U / Ctrl+D scroll half a page. Keys
  // typed into the composer or a form field are never intercepted here — the
  // composer claims Ctrl+U / Ctrl+D for ITSELF (see composer.tsx: the caret is
  // usually there when you want to look back up), and G/gg stay plain letters
  // that must reach whatever is being typed into.
  const ggPendingRef = useRef(false)
  const onChatKey = useCallback((e: React.KeyboardEvent<HTMLDivElement>) => {
    const target = e.target as HTMLElement | null
    if (target?.closest('input, textarea, [contenteditable="true"]')) return
    const el = chatRef.current
    if (!el || e.metaKey || e.altKey) return

    if (e.ctrlKey && (e.key === 'u' || e.key === 'd')) {
      e.preventDefault()
      scrollByHalfPage(e.key === 'd' ? 1 : -1)
      return
    }
    if (e.ctrlKey) return
    if (e.key === 'G') { e.preventDefault(); scrollToBottom(true); return } // newest message + re-arm following
    if (e.key === 'g') {
      e.preventDefault()
      if (ggPendingRef.current) {
        ggPendingRef.current = false
        el.scrollTop = 0
      } else {
        ggPendingRef.current = true
        window.setTimeout(() => { ggPendingRef.current = false }, 600)
      }
    }
  }, [scrollToBottom, scrollByHalfPage])

  // ── Frame-batched deltas ───────────────────────────────────────────────────
  // (enqueueDelta / flushDeltas live in useTranscript.ts — the batcher buffers the
  // streamed deltas of every session, so it belongs with the transcript it feeds.)

  // Pin to the newest content after every committed update while following —
  // doing it in a layout effect means the scroll happens in the same frame the
  // content grows, so the view glides instead of lurching.
  useLayoutEffect(() => {
    if (followBottomRef.current) pinBottom()
  }, [msgCache, pinBottom])

  // …and pin again when the content changes height WITHOUT a message update.
  //
  // Messages are not the only thing that grows a transcript: a mermaid diagram renders
  // asynchronously (lazy import + debounced render), an image or attachment card gets its
  // height when the file loads, a tool card expands on click. All of that happens after the
  // pin above, and with the scroll anchor being the TOP of the viewport the visible content
  // slides up by exactly the height that appeared — until the next delta pins it back down
  // ("先向上飘，再跳到底"). Watching the content box is what makes following mean "the newest
  // message is in view", whatever moved it.
  useEffect(() => {
    const el = chatContentRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(() => {
      if (followBottomRef.current) pinBottom()
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [pinBottom])

  // On session switch, jump straight to the newest message BEFORE paint (via
  // useLayoutEffect) so the view never briefly shows the oldest messages and
  // then snaps down. The async-load path still uses scrollToBottom() after the
  // page arrives.
  useLayoutEffect(() => {
    pinBottom()
    // A session switch always lands at the newest message — re-arm following.
    followBottomRef.current = true
    setShowJump(false)
  }, [currentId, pinBottom])

  const loadMoreRef = useRef(false)
  const handleScroll = () => {
    const el = chatRef.current
    if (!el) return
    // Following is a statement about the READER, so only the reader ends it — and the one thing a
    // reader does that leaves the bottom is move the viewport UP (scrollTop decreases).
    //
    // This used to be answered from the distance to the bottom ("more than 48px away = the reader
    // scrolled away"), which cannot tell a reader from a transcript that GREW: content arriving
    // below the viewport leaves scrollTop exactly where it was and pushes the bottom away, so the
    // scroll event the app's own pin had queued was read as a departure. Following then switched
    // OFF and the view rested short of the newest message, with 回到最新消息 showing — and it was
    // intermittent, because it needs that growth to land before the queued event is delivered.
    // pinBottom records where each pin lands, so a scroll event it caused reads as no movement.
    const top = el.scrollTop
    const movedUp = top < lastTopRef.current - 1
    lastTopRef.current = top
    if (followBottomRef.current) {
      if (movedUp) {
        followBottomRef.current = false
        setShowJump(true)
      }
    } else if (el.scrollHeight - top - el.clientHeight < 48) {
      // Back at the bottom (by wheel, by 回到最新消息, by G): following re-arms.
      followBottomRef.current = true
      setShowJump(false)
    }
    if (loadMoreRef.current || loading) return
    // Load older messages from the backend when the user scrolls to the top and
    // more history is known to exist.
    if (el.scrollTop <= 40 && hasMore[currentId] && earliestTs[currentId]) {
      loadMoreRef.current = true
      const old = el.scrollHeight
      const before = earliestTs[currentId]
      ;(async () => {
        try {
          const page: any = await (AgentService as any).LoadSessionMore?.(currentId, before, PAGE_SIZE)
          if (!page || !page.messages || page.messages.length === 0) {
            markHistoryEnd(currentId)
            return
          }
          prependSessionPage(currentId, {
            messages: buildTurns(page.messages),
            hasMore: !!page.hasMore,
            earliestTs: page.messages[0].timestamp,
          })
        } catch { /* ignore */ }
        requestAnimationFrame(() => requestAnimationFrame(() => {
          const c = chatRef.current
          if (c) {
            // Put the same content back at the top of the viewport, and record where that landed:
            // this is an app-initiated move, not the reader's (see handleScroll).
            c.scrollTop = c.scrollHeight - old
            lastTopRef.current = c.scrollTop
          }
          loadMoreRef.current = false
        }))
      })()
    }
  }

  // The session on screen is producing output when its own run is in-flight, or its own status
  // is busy — the latter is what covers the simulated-turn fallback, which never marks the run
  // busy. BOTH terms are scoped to currentId: the desktop runs every session's turn in its own
  // goroutine, so the neighbour's spinner must never answer for this one.
  const isCurrentRunning = runningSet.has(currentId) ||
    state.status === 'thinking' || state.status === 'tool_running' || state.status === 'busy'

  // A turn's fold toggle grows (or shrinks) that turn IN PLACE. A reader who is following the
  // bottom has to stay there — the design's 「展开/收起不该让滚动位置跳」 — and the pin goes through
  // pinBottom so the growth itself cannot end following (that is handleScroll's job to get right,
  // and this keeps the two consistent). Called from the bubble's LAYOUT effect, so the new content
  // is already in the DOM and the pin lands in the same frame — not from a rAF or a timer, which
  // an occluded webview never delivers. A reader who had scrolled away is left where they were.
  const repinAfterFoldToggle = useCallback(() => {
    if (!followBottomRef.current) return
    followBottomRef.current = true
    pinBottom()
  }, [pinBottom])

  // How long the turn on screen has been running: the strip's live row reports it, so the reader
  // can tell "working" from "stuck". One ticker, and the wording lives in the strip itself.
  const runningIdx = isCurrentRunning ? lastRunningAssistantIndex(messages) : -1
  const runningMsg = runningIdx >= 0 ? messages[runningIdx] : undefined
  const runningElapsedMs = useElapsedMs(runningMsg?.ts, !!runningMsg)

  // Which turn is "the latest" — the one footer that carries 「评审所有改动」 (see the message list),
  // and the id the run records so its chip can report back after a restart.
  const latestIdx = lastAssistantIndex(messages)
  const latestTurnMsgId = latestIdx >= 0 ? messages[latestIdx].id : ''

  // The composer: the input box, the @-picker and "/" palette, the queue of messages typed
  // while a turn runs, and the answer form for a parked question. It routes what the user
  // commits (send now / queue / slash command) into the transcript store.
  const composer = useComposer({
    currentId,
    running: isCurrentRunning,
    transcript: { updateSession, applyToSession, injectSteerVisual, sealRunningSegment, markRunning, refreshRunning, holdLocal, releaseLocal },
    scrollToBottom,
    chatRef,
  })

  const refreshProvider = useCallback(async () => {
    try {
      const info = await (AgentService as any).GetProviderInfo?.()
      if (info) setProviderName(info.provider)
      // The provider decides which levels exist (a declared thinking_efforts
      // set); an empty/absent answer keeps the full list.
      const lvs = info?.thinkingLevels
      if (Array.isArray(lvs) && lvs.length > 0) setThinkingLevels(lvs)
      const lv = await (AgentService as any).GetThinkingLevel?.()
      if (lv) setThinkingLevel(lv)
    } catch { /* ignore */ }
  }, [])

  // refreshWorkspace loads a session's workspace context: the primary directory
  // (bash's cwd, relative-path base) and the full root set the picker and the
  // prompt use. Both come from the session meta, so they move together.
  const refreshWorkspace = useCallback(async (id: string) => {
    try {
      const w = await (AgentService as any).GetSessionWorkingDir?.(id)
      setWorkDir(w || '')
    } catch { /* ignore */ }
    try {
      setRoots((await AgentService.GetSessionRoots(id)) || null)
    } catch { setRoots(null) }
  }, [])

  // refreshProjects re-reads the project table for the sidebar's groups. It is a separate call
  // from the session list on purpose: the two answer different questions, and a project with no
  // members must still render its header.
  const refreshProjects = useCallback(async () => {
    try {
      setProjects((await AgentService.ListProjects()) || [])
    } catch { /* keep what we have */ }
  }, [])

  // A project write moves the ground under sessions the frontend already rendered (design
  // §7.4). Everything it shows was PULLED, so the backend's own write is not visible until the
  // reader re-reads: the group headers and their names, the rows' membership, and — for the
  // session on screen — its chip and roots panel. This is that re-read.
  useWorkspaceChanged(useCallback(() => {
    void refreshProjects()
    void AgentService.ListSessions()
      .then((list) => { if (list) setSessions(list.map((s) => ({ ...s, active: s.id === currentId }))) })
      .catch(() => {})
    if (currentId) void refreshWorkspace(currentId)
  }, [refreshProjects, refreshWorkspace, currentId]))

  // saveProject runs the create/edit form. Both paths go through the same two backend calls
  // (CreateProject / SetProjectRoots) and the same refusal string, which is why the form can be
  // one component: its job is to collect paths, not to decide what is valid.
  const saveProject = useCallback(async () => {
    const f = projectForm
    if (!f) return
    const res = f.mode === 'create'
      ? await AgentService.CreateProject(f.name, f.primary, f.additional).catch((e) => String(e))
      : await AgentService.SetProjectRoots(f.id || '', f.primary, f.additional).catch((e) => String(e))
    if (res !== 'ok') {
      setProjectForm((prev) => (prev ? { ...prev, error: res || '保存失败' } : prev))
      return
    }
    // A rename that came with an edit: the form carries both, and RenameProject is the only
    // writer of a name. Only asked when it actually changed.
    if (f.mode === 'edit' && f.id) {
      const before = projects.find((p) => p.id === f.id)
      if (before && before.name !== f.name) {
        const rres = await AgentService.RenameProject(f.id, f.name).catch((e) => String(e))
        if (rres !== 'ok') {
          setProjectForm((prev) => (prev ? { ...prev, error: rres || '重命名失败' } : prev))
          return
        }
      }
    }
    setProjectForm(null)
    // The backend also emits agent:workspace_changed, but this form OWNS the fact it just
    // changed something — waiting for the event to come back would leave the dialog's own
    // commit invisible for a frame.
    await refreshProjects()
    if (currentId) await refreshWorkspace(currentId)
    const list = (await AgentService.ListSessions().catch(() => null)) || []
    setSessions(list.map((s) => ({ ...s, active: s.id === currentId })))
  }, [projectForm, projects, refreshProjects, refreshWorkspace, currentId])

  // openProjectEditor opens the create form, or the edit form prefilled from a project.
  const openProjectEditor = useCallback((p?: ProjectVO) => {
    setProjectMenu(null)
    setProjectForm(p
      ? {
        mode: 'edit', id: p.id, name: p.name, primary: p.workingDir,
        additional: (p.additionalDirs || []).map((r) => r.path),
      }
      : { mode: 'create', name: '', primary: '', additional: [] })
  }, [])

  const commitProjectRename = useCallback(async (id: string, name: string) => {
    const trimmed = name.trim()
    // Nothing typed is the editor's own cancel (Escape, or blur with an empty box), not a
    // refusal to report: 项目名不能为空 would be answering a keystroke that never meant to commit.
    if (!trimmed) { setEditingProject(null); setProjectNotice(null); return }
    const res = await AgentService.RenameProject(id, trimmed).catch((e) => String(e))
    if (res !== 'ok') {
      // The editor stays open with the reason under it: a refused rename that merely closes
      // reads as "the app ignored me", and these refusals are actionable (empty / duplicate).
      setProjectNotice({ id, msg: res || '重命名失败' })
      return
    }
    setEditingProject(null)
    setProjectNotice(null)
    await refreshProjects()
  }, [refreshProjects])

  // deleteProject confirms first (§6.2 lists the affected session count), then detaches.
  const deleteProject = useCallback(async (id: string) => {
    const res = await AgentService.DeleteProject(id).catch((e) => String(e))
    if (res !== 'ok') {
      setConfirmDelProject((prev) => (prev && prev.id === id ? { ...prev, error: res || '删除失败' } : prev))
      return
    }
    setConfirmDelProject(null)
    await refreshProjects()
    if (currentId) await refreshWorkspace(currentId)
    const list = (await AgentService.ListSessions().catch(() => null)) || []
    setSessions(list.map((s) => ({ ...s, active: s.id === currentId })))
  }, [refreshProjects, refreshWorkspace, currentId])

  // The project form's two pickers. Same Wails dialog and the same shape as the session's own
  // (single-select for the primary, multi-select for the rest) — validation lives in the
  // backend in both cases, so the form never has to decide what a valid root is.
  const pickProjectPrimary = useCallback(async () => {
    try {
      const picked: string | string[] = await Dialogs.OpenFile({
        CanChooseDirectories: true,
        CanChooseFiles: false,
        CanCreateDirectories: true,
        AllowsMultipleSelection: false,
        Title: '选择项目主目录',
        Directory: projectForm?.primary || undefined,
      })
      if (typeof picked !== 'string' || !picked) return
      setProjectForm((prev) => (prev ? { ...prev, primary: picked, error: undefined } : prev))
    } catch { /* ignore */ }
  }, [projectForm?.primary])

  const addProjectRoots = useCallback(async () => {
    try {
      const picked: string | string[] = await Dialogs.OpenFile({
        CanChooseDirectories: true,
        CanChooseFiles: false,
        CanCreateDirectories: false,
        AllowsMultipleSelection: true,
        Title: '添加项目附加目录（可多选）',
        Directory: projectForm?.primary || undefined,
      })
      const dirs = Array.isArray(picked) ? picked : picked ? [picked] : []
      if (dirs.length === 0) return
      // Duplicates are dropped here only so the list the user sees matches what they picked;
      // the backend normalizes (and drops the primary itself) on save.
      setProjectForm((prev) => (prev
        ? { ...prev, additional: Array.from(new Set([...prev.additional, ...dirs])), error: undefined }
        : prev))
    } catch { /* ignore */ }
  }, [projectForm?.primary])

  // pickWorkDir opens a native folder picker (seeded at the session's current
  // working directory) and applies the chosen directory to the session. It stays
  // SINGLE-select, and separate from "add directory": which folder is primary is
  // never something to guess from the order of a multi-selection.
  const pickWorkDir = useCallback(async (id: string) => {
    try {
      const picked: string | string[] = await Dialogs.OpenFile({
        CanChooseDirectories: true,
        CanChooseFiles: false,
        CanCreateDirectories: true,
        AllowsMultipleSelection: false,
        Title: '选择主工作目录',
        Directory: workDir || undefined,
      })
      if (typeof picked !== 'string' || !picked) return
      const res = await (AgentService as any).SetSessionWorkingDir?.(id, picked).catch((e: unknown) => String(e))
      if (res && res !== 'ok') setRootsError(res)
      setWorkDir(picked)
      // A directory that just became the primary stops being an additional root, so
      // the list is re-read rather than patched.
      await refreshWorkspace(id)
    } catch { /* ignore */ }
  }, [workDir, refreshWorkspace])

  // addRoots appends additional workspace roots through a MULTI-select picker (the
  // Wails dialog returns a list when AllowsMultipleSelection is set). Validation —
  // absolute, no whitespace, exists, is a directory — lives in the backend, and its
  // message is what the popover shows.
  const addRoots = useCallback(async (id: string) => {
    setRootsError('')
    setRootsBusy(true)
    try {
      const picked: string | string[] = await Dialogs.OpenFile({
        CanChooseDirectories: true,
        CanChooseFiles: false,
        CanCreateDirectories: false,
        AllowsMultipleSelection: true,
        Title: '添加工作区目录（可多选）',
        Directory: workDir || undefined,
      })
      const dirs = Array.isArray(picked) ? picked : picked ? [picked] : []
      if (dirs.length === 0) return
      const res = await AgentService.AddSessionRoots(id, dirs).catch((e) => String(e))
      if (res !== 'ok') setRootsError(res || '添加失败')
      await refreshWorkspace(id)
    } catch (e) {
      setRootsError(String(e))
    } finally {
      setRootsBusy(false)
    }
  }, [workDir, refreshWorkspace])

  // A review is over when its RUN is over — and the session's busy flag cannot decide that: between
  // the click and the fork going busy the session is still idle, so a "clear while idle" rule fired
  // in exactly that window and dropped the pending state before the run had even started. That rule
  // was put back for one control run, and the chip's observed sequence went
  // 评审本轮改动 → 已评审 1 条 · 查看 (the middle state never appears) against
  // 评审中… → 已评审 1 条 · 查看 with this effect deciding — i.e. the reader's report
  // 「立刻会变成已评审查看，但没法点击」. The run's own end event is the fact; it is paired to the
  // turn through reviewForRef, which is why the clearing lives in the effect below.
  // The DISPLAYED session's busy flag (useAgentStatus is keyed by session id — a neighbour's
  // turn must not disable this session's chip).
  const sessionBusy = state.status !== 'idle' && state.status !== 'error'

  // runReview starts one review fork and pairs it back to the footer that asked for it. The
  // scope itself is the caller's call (a thunk), because the two entries ask different questions
  // — one turn's own two checkpoint trees, or the whole working tree against HEAD — while
  // everything around the ask is the same.
  //
  // The run is a normal turn as far as the backend is concerned (the session goes busy, Stop
  // cancels it), but its process goes to the side-channel panel: the conversation keeps this
  // turn's footer as the anchor, the panel shows the rest.
  //
  // `slot` is null when there is no footer to light up (a re-run from the panel of a run that
  // recorded no turn). The run is worth starting either way; what it cannot do is report back.
  const runReview = useCallback(async (slot: ReviewSlot | null, start: (sid: string) => Promise<string>) => {
    const sid = currentId
    setReviewNotice(null)
    if (slot) {
      setReviewPending(slot)
      // Remember which slot this is for. The run's result arrives as an event, and by then this
      // state may already be cleared (the session going idle does that), so the pairing cannot
      // lean on reviewPending still being set.
      reviewForRef.current = slot
    }
    try {
      const res = await start(sid)
      // A refusal means no run started, so the pending state ends HERE. Success must NOT clear
      // it: this call only ever STARTS the fork (it runs on in the background), so clearing here
      // made the chip flash 「评审中…」 and fall straight back — onto 「已评审 · 查看」, because
      // the record (with its reviewed_msg) is on disk from the first line, while the run was
      // still going. Measured report: 「点击评审按钮后，立刻会变成已评审查看，但没法点击」 —
      // and it WAS unclickable, because the session is busy running the review.
      if (res) {
        setReviewPending(null)
        reviewForRef.current = null
        // The refusal belongs to the chip that asked (if one did); a re-run from the panel has
        // nowhere to show it, and its own pane says what happened to the run.
        if (slot) setReviewNotice({ ...slot, text: res })
      }
    } catch (e) {
      setReviewPending(null)
      reviewForRef.current = null
      if (slot) setReviewNotice({ ...slot, text: String(e) })
    }
  }, [currentId])

  // startReview is the turn-scoped entry: the files this turn changed, read from its own two
  // checkpoint trees when they have a pair (which is why a review of already-committed changes
  // still has something to read).
  const startReview = useCallback((turn: number, paths: string[], msgId?: string) => {
    void runReview(msgId ? { msgId, whole: false } : null,
      (sid) => AgentService.ReviewChanges(sid, turn, paths, msgId || ''))
  }, [runReview])

  // startReviewAll is the working-tree entry: everything uncommitted against HEAD, from any turn.
  // It reads that list from the backend at click time rather than taking it from the footer —
  // the footer's count is a hint that was true a moment ago, and the run has to name what it
  // actually reviewed.
  const startReviewAll = useCallback((msgId: string) => {
    void runReview(msgId ? { msgId, whole: true } : null, (sid) => AgentService.ReviewAllChanges(sid, msgId))
  }, [runReview])

  // wholeTreeFiles is how many files the working tree holds uncommitted against HEAD: the gate of
  // the 「评审所有改动」 entry (>0 means there is something to review) AND its evidence when the turn
  // carrying it changed nothing of its own — the row then has nothing else to say with.
  //
  // Asked when the session comes to REST, not on every render: it is a git walk per root, and the
  // answer only moves when a turn ends, when the reader switches sessions, or when a side-channel
  // run ends (a /commit commits exactly what the entry was offering to review). While a turn is
  // running the last answer is left in place rather than cleared — the entry stays on screen,
  // disabled, the way 评审本轮改动 does, instead of blinking out and back once per turn.
  const [wholeTreeFiles, setWholeTreeFiles] = useState(0)
  useEffect(() => {
    if (!currentId || isCurrentRunning) return
    let alive = true
    AgentService.CountWorkingTreeChanges(currentId)
      .then((n) => { if (alive) setWholeTreeFiles(n || 0) })
      .catch(() => { if (alive) setWholeTreeFiles(0) })
    return () => { alive = false }
  }, [currentId, isCurrentRunning, oneoffRun.result])

  // ReviewSlot identifies one review entry on one turn's footer: WHICH turn asked (its message id,
// unique across sessions) and WHICH of the two scopes that turn offers. Both halves are needed —
// a turn has one entry of each, and every piece of per-entry state (pending, the finished run, the
// refusal reason) is keyed by the pair.
type ReviewSlot = { msgId: string; whole: boolean }

// reviewDoneLabel is the footer chip's 已评审 state for a turn, from two sources:
//
//   - the run that just finished in THIS window (its count is exact — the backend tallied the
//     findings as they were called); and
//   - the records on disk, which is what survives a restart: each review records the turn it
//     was started for (agent.OneOffKeyReviewedMsg), so the pairing comes back with the session.
//     Those runs have no loaded count (counting means reading the whole record), so the chip
//     says 已评审 and the panel — which loads the run's detail when selected — shows how many.
//
// The slot carries WHICH entry is being asked about (its turn's own changes, or the whole working
// tree): a turn has one of each, and a matcher that ignored that would have yesterday's
// whole-tree run answer for today's turn-scoped chip.
//
// Returns undefined when this slot has never been reviewed, which is what leaves the chip as
// 「评审本轮改动」/「评审所有改动」.
function reviewDoneLabel(slot: ReviewSlot, result: (ReviewSlot & { run: OneOffRun }) | null, runs: OneOffVO[]): string | undefined {
  if (result && result.msgId === slot.msgId && result.whole === slot.whole) return oneOffRunLabel(result.run)
  // A run that is STILL GOING does not count as done, even though its record is already on disk
  // with this turn's id in it: that is what made the chip claim 已评审 while the review was
  // still working. The record's own running flag is an mtime heuristic, so this is the same
  // "still writing" notion the panel uses.
  return runs.some((it) => it.reviewedMsg === slot.msgId && !!it.wholeTree === slot.whole && !it.running)
    ? '已评审 · 查看' : undefined
}

// ── The conversation's one line about a side-channel run ─────────────────────
  // A review's process no longer streams into the transcript, so what is left here is the
  // anchor: the turn's footer says how it ended and opens the panel, and a typed /review or
  // /commit closes its placeholder bubble with a single line. Design §5.4.
  //
  // The result pairs with the turn that started it through reviewForRef rather than through
  // reviewPending: the two states change at different moments (this event ends the pending one),
  // and the pairing must survive everything that re-renders in between.
  useEffect(() => {
    const run = oneoffRun.result
    if (!run || run.kind !== 'review') return
    const slot = reviewForRef.current
    if (!slot) return // started from the composer: the notice path owns it
    reviewForRef.current = null
    setReviewResult({ ...slot, run })
    // The run is over, so the chip leaves 评审中… — the end EVENT is what says so (see the note
    // above about the idle-flag rule that used to clear it too early).
    setReviewPending(null)
  }, [oneoffRun.result])

  // A typed command's placeholder is a bubble the composer opened; hand the result to it so
  // it can become one line. `at` guards against re-firing on unrelated re-renders.
  const noticedRunRef = useRef(0)
  useEffect(() => {
    const run = oneoffRun.result
    if (!run || run.at === noticedRunRef.current) return
    noticedRunRef.current = run.at
    composer.noticeCommandResult(run)
  }, [oneoffRun.result, composer])

  // A run started → show it. The reader just launched it, and this is where its output goes;
  // nothing else opens the panel on its own.
  useEffect(() => {
    if (oneoffRun.live) setOneOffOpen(true)
  }, [oneoffRun.live?.at])

  // openDiffPanel is the turn's 「完整 diff」 entry: what THIS turn changed, in a lightbox over the
  // conversation, with real file line numbers. The baseline is the backend's choice, stated on the
  // pane's own head line: the turn's two checkpoint trees when it has a pair (exact — a shell
  // command's writes are in it — and frozen, so a commit since does not empty it), otherwise the
  // working tree against HEAD limited to the tool-declared paths.
  //
  // It used to hand the job to the side-channel panel, which is scoped to a RUN — so a turn
  // nobody had reviewed opened an empty column (there is no run to show, and the panel's list
  // says so), and a reviewed one showed the SELECTED run's file set rather than this turn's.
  // The paths are the click's own: fetched on open, dropped on close, and never stored per run
  // — which is also what keeps them from becoming the stale anchor P4 removed, and why git runs
  // once per open rather than once per render.
  const openDiffPanel = useCallback((turn: number, paths: string[]) => {
    setTurnDiff({ sessionId: currentId, turn, paths })
  }, [currentId])

  // sendFindings is how the panel's findings leave: the picked ones become one ordinary user
  // message. It rides the composer's route (submit) instead of a channel of its own — while
  // a turn is running the message queues for the next steer point, because startTurn refuses
  // a busy session (so sending directly would drop the text in silence). The reply then
  // streams into the transcript, and the panel stays where it is.
  const sendFindings = useCallback((text: string) => {
    composer.submit(text)
  }, [composer])

  const removeRoot = useCallback(async (id: string, path: string) => {
    setRootsError('')
    setRootsBusy(true)
    try {
      const res = await AgentService.RemoveSessionRoot(id, path).catch((e) => String(e))
      if (res !== 'ok') setRootsError(res || '移除失败')
      await refreshWorkspace(id)
    } finally {
      setRootsBusy(false)
    }
  }, [refreshWorkspace])

  // refreshMCP reloads the MCP servers/tools + profile for the current session. It is also
  // what an authorization's "ok" event triggers (useMCPAuth): the panel holds each row's
  // connected flag and tool list by value, so the row would otherwise keep saying 需要授权
  // after the server came up.
  const refreshMCP = useCallback(async () => {
    try {
      const sv = (await (AgentService as any).ListMCPServers?.()) || []
      setMcpServers(sv)
      const prof = await (AgentService as any).ListMCPProfiles?.()
      if (prof) setMcpProfile({ active: prof.active || '', available: prof.available || [] })
    } catch { /* ignore */ }
  }, [])

  // loadAgents reads the board as it stands (no waiting on the network) — what a panel opened for
  // the first time needs, since nobody has asked those nodes anything yet.
  const loadAgents = useCallback(async () => {
    try {
      setAgentsBoard(await AgentService.PeerBoard())
    } catch { /* leave the panel empty rather than claiming anything */ }
  }, [])

  // refreshAgents is the panel's 刷新: it asks every node NOW (the backend drops its cached cards)
  // and waits for the answers, which is why the button says 检查中… while it runs.
  const refreshAgents = useCallback(async () => {
    setAgentsBusy(true)
    try {
      setAgentsBoard(await AgentService.RefreshPeerBoard())
    } catch { /* keep what the panel already shows */ }
    finally { setAgentsBusy(false) }
  }, [])

  // Opening the panel reads once; it does not poll. What makes a reading current is the button.
  useEffect(() => {
    if (agentsOpen) void loadAgents()
  }, [agentsOpen, loadAgents])
  const { auth: mcpAuth, setAuth: setMCPAuth, clearAuth: clearMCPAuth } = useMCPAuth(refreshMCP)
  // ...and once more when the backend says its pool is settled: a panel that was already open
  // while the servers were still coming up would otherwise keep the empty snapshot it read.
  useMCPReady(refreshMCP)
  const toggleServer = useCallback(async (name: string, enabled: boolean) => {
    setMcpLoading((p) => ({ ...p, [name]: true }))
    // The answer is a string, not a boolean: the backend says WHY it refused, and
    // discarding it is what made 启用 look like a no-op on a server that needs
    // authorization (the one failure the user can do something about). It lands in the
    // same per-server slot the authorization progress uses, so the row shows the reason
    // exactly where it shows the fix.
    const res = await (AgentService as any).SetMCPServerEnabled?.(name, enabled).catch((e: unknown) => String(e))
    if (res && res !== 'ok') setMCPAuth(name, { phase: 'error', message: res, authUrl: '' })
    else clearMCPAuth(name)
    refreshMCP()
    setMcpLoading((p) => ({ ...p, [name]: false }))
  }, [refreshMCP, setMCPAuth, clearMCPAuth])
  // authorizeServer starts the OAuth flow. The call returns as soon as the flow is
  // running — the progress (and the URL to open) arrives as agent:mcp_auth events — so the
  // only thing to do with its answer is to show a refusal.
  const authorizeServer = useCallback(async (name: string) => {
    setMCPAuth(name, { phase: 'starting', message: '正在启动授权流程…', authUrl: '' })
    const res = await (AgentService as any).AuthorizeMCPServer?.(name).catch((e: unknown) => String(e))
    if (res && res !== 'ok') setMCPAuth(name, { phase: 'error', message: res, authUrl: '' })
  }, [setMCPAuth])
  // completeMCPAuth hands back the redirect URL the browser could not deliver (or the user
  // copied faster than it arrived).
  const completeMCPAuth = useCallback(async (name: string, url: string) => {
    const res = await (AgentService as any).CompleteMCPServerAuth?.(name, url).catch((e: unknown) => String(e))
    if (res && res !== 'ok') setMCPAuth(name, { phase: 'error', message: res, authUrl: '' })
    refreshMCP()
  }, [refreshMCP, setMCPAuth])
  const toggleTool = useCallback(async (name: string, enabled: boolean) => {
    setMcpLoading((p) => ({ ...p, [name]: true }))
    try { await (AgentService as any).SetMCPToolEnabled?.(name, enabled) } catch { /* ignore */ }
    refreshMCP()
    setMcpLoading((p) => ({ ...p, [name]: false }))
  }, [refreshMCP])
  const toggleProfile = useCallback(async (name: string) => {
    if (name === mcpProfile.active) return
    setMcpLoading((p) => ({ ...p, __profile__: true }))
    try { await (AgentService as any).SetMCPProfile?.(name) } catch { /* ignore */ }
    refreshMCP()
    setMcpLoading((p) => ({ ...p, __profile__: false }))
  }, [mcpProfile.active, refreshMCP])

  // The panel's data is re-read EVERY time it opens, and that is not a nicety: the snapshot
  // it renders can be taken while the servers are still coming up. The manager registers a
  // connection as soon as its own handshake and tools/list succeed, but the shared pool is
  // filled only after EVERY server has answered (ConnectAll waits for the slowest one), so
  // "已连接 + 池子里还没有工具" is a real window — seconds wide when one server is slow. A
  // panel opened on a snapshot from inside it showed a connected server with nothing to
  // expand, forever, because nothing re-read afterwards.
  useEffect(() => {
    if (mcpOpen) refreshMCP()
  }, [mcpOpen, refreshMCP])

  const commitRename = useCallback(async (id: string) => {
    const t = editTitle.trim()
    setEditingId(''); setEditTitle('')
    if (!t) return
    await (AgentService as any).RenameSession?.(id, t).catch(() => {})
    setSessions((prev) => prev.map((s) => (s.id === id ? { ...s, title: t } : s)))
  }, [editTitle])

  // togglePin pins or unpins a session (the row's 置顶 / 取消置顶).
  //
  // The local list is updated FIRST: the row moves to the head of its own group on the very next
  // render (sessionGroups sorts pinned-first), so the click lands immediately instead of after a
  // round trip. The backend call is then the record of it — the flag lives in meta.json, which is
  // what makes the order survive a restart — and a refusal puts the row back where it was rather
  // than leaving the UI claiming something the disk does not say.
  const togglePin = useCallback(async (sid: string, pinned: boolean) => {
    setSessions((prev) => prev.map((s) => (s.id === sid ? { ...s, pinned } : s)))
    const res = await (AgentService as any).SetSessionPinned?.(sid, pinned).catch((e: unknown) => String(e))
    if (res && res !== 'ok') setSessions((prev) => prev.map((s) => (s.id === sid ? { ...s, pinned: !pinned } : s)))
  }, [])

  const loadAll = useCallback(async () => {
    setLoading(true)
    const list = (await AgentService.ListSessions().catch(() => null)) || []
    void refreshProjects()
    let cur = await AgentService.CurrentSession().catch(() => null)
    if (!cur && list.length > 0) cur = list[0]
    if (cur) {
      setCurrentId(cur.id); setCurrentTitle(cur.title || 'Tachi')
      if (!msgCache[cur.id]) {
        const page: any = await (AgentService as any).LoadSession?.(cur.id, PAGE_SIZE)
        if (page?.messages) {
          setSessionPage(cur.id, { messages: buildTurns(page.messages), hasMore: !!page.hasMore, earliestTs: page.messages[0]?.timestamp || '' })
        }
      }
      refreshUsage(cur.id)
      scrollToBottom(true)
    } else {
      const ns = await AgentService.NewSession('').catch(() => null)
      if (ns) {
        setCurrentId(ns.id); setCurrentTitle(ns.title || 'Tachi')
        openSession(ns.id)
        clearUsage(); resetTps()
        // A session created at launch is an empty conversation too, so the caret starts in
        // the composer (the welcome text below asks the user to type there).
        composer.focusInput()
        refreshUsage(ns.id)
        refreshWorkspace(ns.id)
      }
    }
    setSessions(list.map((s) => ({ ...s, active: s.id === cur?.id })))
    // The ONE group the boot opens: the one holding the session it just resumed. Every other
    // project stays folded (the empty-map default) — this is the single exception the "the active
    // row must be visible" rule needs, since at boot the reader has chosen nothing yet.
    if (cur && !bootOpenedGroup.current) {
      bootOpenedGroup.current = true
      focusGroup(cur.projectId || LOOSE_GROUP_KEY)
    }
    setLoading(false)
    refreshProvider()
    refreshMCP()
    if (cur) refreshWorkspace(cur.id)
  }, [msgCache, openSession, setSessionPage, scrollToBottom, refreshProvider, refreshUsage, refreshMCP, refreshWorkspace, clearUsage, composer.focusInput, focusGroup])
  // openRewindMenu asks the backend where this session's turns begin and opens the
  // bubble's menu. The list is fetched per open rather than cached: a running turn keeps
  // adding boundaries, and a stale list would resolve a bubble to the wrong turn.
  const openRewindMenu = useCallback(async (e: React.MouseEvent, m: Message, sid: string) => {
    e.preventDefault()
    const turns: RewindTurnVO[] = (await AgentService.RewindTurns(sid).catch(() => null)) || []
    setRewindMenu({ x: e.clientX, y: e.clientY, msg: m, sid, turns })
  }, [])

  // openRewindCard asks what the rewind would do and shows it. Nothing is touched here —
  // the preview is a read, and the card is where the reader agrees to it.
  //
  // It takes (sid, turn) rather than a resolved menu target because there are two ways in — the
  // bubble's menu and the chain — and they must land on the SAME card: one rewind, one set of
  // questions (what is restored, what is deleted, what cannot be undone).
  const openRewindCard = useCallback(async (sid: string, turn: number) => {
    setRewindMenu(null)
    let preview: RewindPreviewVO
    try {
      preview = await AgentService.PreviewRewind(sid, turn)
    } catch (e) {
      // A preview that cannot be read still opens the card: a menu item that closes the
      // menu and shows nothing is indistinguishable from a broken button, and the reason
      // is exactly what the reader needs.
      preview = { turn, target: turn, blocked: `读取回退预览失败：${String(e)}` } as RewindPreviewVO
    }
    setRewindCard({ sid, turn, preview })
  }, [])

  // openChain lists the session's rewind points. The list comes from the manifest, so it covers
  // every turn — including the ones whose opening record is not on screen — and opening it runs
  // no git at all (the per-turn numbers were recorded when each turn ended).
  //
  // A chain with NOTHING to do (no checkpoints at all) still opens: the empty state says why,
  // and 「点了没反应」 is the one outcome a list must never have.
  const openChain = useCallback(async (sid: string, title: string) => {
    setMenu(null)
    const chainVO = await AgentService.RewindChain(sid).catch(() => null)
    setChain({
      sid, title, turns: chainVO?.turns || [], blocked: chainVO?.blocked || '',
      // -1 = the position is unknown (the call failed); 0 is a real one (a rewind to the first
      // turn leaves no records at all), so the two must not be collapsed.
      position: chainVO?.position ?? -1,
    })
  }, [])

  // confirmRewind runs it. A refusal (a running turn, a pruned checkpoint, an
  // unavailable snapshot) lands in the card that asked, next to the button that was
  // pressed — the backend has the last word, so it is shown verbatim.
  const confirmRewind = useCallback(async (card: { sid: string; turn: number }) => {
    const res = await AgentService.ApplyRewind(card.sid, card.turn).catch((e) => String(e))
    if (res === 'ok') { setRewindCard(null); return }
    setRewindCard((c) => (c ? { ...c, error: res } : c))
  }, [])

  const clickSession = useCallback(async (id: string) => {
    setCurrentId(id)
    setCurrentTitle(sessions.find((s) => s.id === id)?.title || 'Tachi')
    setSessions((prev) => prev.map((s) => ({ ...s, active: s.id === id })))
    // Landing in a session decides the one open group (see focusGroup), and it is asked of the
    // session's RECORD rather than of the id that was picked: a conversation whose project is gone
    // is grouped under 无项目, and this is the same rule the grouping uses (sessionGroupKey).
    //
    // Every switch funnels through here — the row's own click, both palettes' picks, and the
    // successor a delete hands the reader to — so the rule cannot be missing from one of them.
    const arrived = sessions.find((s) => s.id === id)
    if (arrived) focusGroup(sessionGroupKey(arrived, projects))
    // Always sync the backend's active session (cheap) so provider/thinking
    // selectors reflect this session even when its messages are already cached.
    await (AgentService as any).ActivateSession?.(id).catch(() => {})
    if (!msgCache[id]) {
      setLoading(true)
      const page: any = await (AgentService as any).LoadSession?.(id, PAGE_SIZE)
      if (page?.messages) {
        setSessionPage(id, { messages: buildTurns(page.messages), hasMore: !!page.hasMore, earliestTs: page.messages[0]?.timestamp || '' })
      }
      setLoading(false)
    }
    resetTps()
    scrollToBottom(true)
    refreshRunning()
    refreshUsage(id)
    refreshProvider()
    refreshMCP()
    refreshWorkspace(id)
  }, [sessions, projects, focusGroup, msgCache, setSessionPage, scrollToBottom, refreshRunning, refreshProvider, refreshUsage, refreshMCP, refreshWorkspace])

  // confirmDelete deletes for real and hands the reader to the NEXT session — the row below the
  // deleted one inside its own group, or the row above when it was that group's last (nextSessionAfter).
  // Two rules make that safe. The successor is computed BEFORE the reload, because `sessions` still
  // holds the deleted row then and that is the only moment "the row below it" is computable at all.
  // And the selection moves ONLY when the deleted session was the one on screen: deleting some other
  // row must not yank the reader out of the conversation they are reading.
  //
  // An "" answer (nothing left to pick, or a row no group claims) keeps the old behaviour: whatever
  // the reload selects, including the session it creates when the list has run empty.
  //
  // It sits AFTER clickSession because it now calls it — a useCallback's dependency array is read
  // while the component renders, so naming a const declared below would be a TDZ error.
  const confirmDelete = useCallback(async (id: string) => {
    const res = await (AgentService as any).DeleteSession?.(id).catch(() => null)
    // "ok" (or a missing binding) is the only success. Anything else — in practice the
    // backend refusing because a turn is still running — keeps the box open with the
    // reason in it, and does NOT reload the list (nothing changed).
    if (res && res !== 'ok') {
      setConfirmDel((prev) => (prev && prev.sid === id ? { ...prev, error: res } : prev))
      return
    }
    setConfirmDel(null)
    const successor = id === currentId ? nextSessionAfter(sessions, projects, id) : ''
    await loadAll()
    if (successor) void clickSession(successor)
  }, [loadAll, clickSession, sessions, projects, currentId])

  // Ending the execution a node is running in this session.
  //
  // The banner is where the reader already is — it is the thing that explains this session — and
  // the button only appears while something is actually running, a state the session list carries
  // (heldByTask, read from the retention record by ListSessions). The backend refuses politely
  // when the state it saw is already gone, so a stale button is a sentence, not an error.
  const [stopNote, setStopNote] = useState('')
  useEffect(() => { setStopNote('') }, [currentId])
  const endSessionTask = useCallback(async (id: string) => {
    setStopNote('')
    const res = await (AgentService as any).EndSessionTask?.(id).catch(() => null)
    if (res && res !== 'ok') {
      setStopNote(res)
      return
    }
    await loadAll()
  }, [loadAll])

  // newChat creates a session. projectID is "" for the sidebar's own button; a project group's
  // ＋ passes its id, which is the only way a session is ever BOUND (design §6.1) — the backend
  // writes the snapshot and project_id, so nothing here needs to know about roots.
  const newChat = useCallback(async (projectID = '') => {
    const ns = await AgentService.NewSession(projectID).catch(() => null)
    if (ns) {
      setCurrentId(ns.id); setCurrentTitle(ns.title || 'Tachi')
      openSession(ns.id)
      clearUsage(); resetTps()
      // The new session is an empty conversation, so the caret belongs in the composer and
      // the user can start typing. It goes here rather than at the end of the function for
      // the same reason it exists at all: the click that created the session (or ⌘N) leaves
      // focus on the button, and this is the last place that must own focus — the refreshes
      // below are pure reads and must not race it.
      composer.focusInput()
      refreshUsage(ns.id)
      refreshWorkspace(ns.id)
      void refreshProjects()
      const list = (await AgentService.ListSessions().catch(() => null)) || []
      setSessions(list.map((s) => ({ ...s, active: s.id === ns.id })))
      // The group the new session ACTUALLY landed in opens, asked of the record just read back: a
      // project that cannot drive a session produces a 无项目 one (the backend binds nothing), and
      // focusing the requested project's group would fold the only group holding a row — the one
      // the ＋ just made. The requested id is the fallback for a list read that came back without
      // it, because the group it names is where the session was meant to be.
      const created = list.find((s) => s.id === ns.id)
      focusGroup(created ? sessionGroupKey(created, projects) : projectID || LOOSE_GROUP_KEY)
      refreshProvider()
    }
  }, [refreshProvider, refreshWorkspace, refreshProjects, refreshUsage, clearUsage, composer.focusInput, focusGroup, projects])

  // newChatLikeCurrent makes another conversation like the one on screen: if that session belongs
  // to a project, its successor starts INSIDE it — the project is what supplies the workspace, the
  // extra roots and the skills, so a project-less successor would silently drop all three while
  // looking like a fresh start.
  //
  // Membership is resolved exactly the way the sidebar groups it (sessionGroups): only a project
  // that still exists counts, so a session the reader sees under 无项目 (its project was deleted,
  // or lost out of projects.json) makes a session there too, instead of one bound to an id that no
  // longer names anything.
  //
  // It is the ONE rule behind both 「再来一个」 entry points — ⌘N and the menu bar's 「新会话」.
  const newChatLikeCurrent = useCallback(() => {
    const pid = sessions.find((x) => x.id === currentId)?.projectId
    void newChat(pid && projects.some((p) => p.id === pid) ? pid : '')
  }, [sessions, projects, currentId, newChat])

  // askDeleteCurrent opens the delete confirmation for the session ON SCREEN — the keyboard form of
  // the row's right-click 「删除」. It never deletes anything itself: the box that comes up is the
  // same one the row opens, so the backend's refusal (a session with a turn in flight) lands in the
  // same place, and the reader's answer is the same button.
  //
  // The title comes from the LIST rather than from the titlebar's own derivation further down (that
  // const is declared after this in the body, and reading it from here would be a TDZ error): a
  // session not listed yet shows 未命名会话, which is the words its row would carry too.
  const askDeleteCurrent = useCallback(() => {
    if (!currentId) return
    setConfirmDel({ sid: currentId, title: sessions.find((s) => s.id === currentId)?.title || '未命名会话' })
  }, [currentId, sessions])

  // The menu bar's 「新会话」 arrives as ui:new_session (desktop/tray.go): the shell brings the
  // window forward and asks, and the session is made HERE — one creation path, shared with the
  // sidebar's ＋ and ⌘N.
  //
  // The settings page gets out of the way first, which ⌘N does not do (it is gated on that page
  // instead): a tray click is an explicit 「我要新会话」, and taking the session behind an opaque
  // page would leave the user looking at nothing having happened. Switching back to the chat is
  // that page's own way out, which is why nothing here needs to ask.
  useEffect(() => {
    const off = Events.On('ui:new_session', () => {
      setView('chat')
      newChatLikeCurrent()
    })
    return () => off?.()
  }, [newChatLikeCurrent])

  useEffect(() => { loadAll(); refreshRunning(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [])

  // focusAfterPalette puts the caret in the composer once the palette is gone — the two palettes are
  // the entry points that close a dialog on their way to a session, and the box's own input is what
  // holds focus while it is open, so a focus taken DURING the pick is dropped with the input when React
  // removes it (focus falls back to <body> and the next keystroke goes nowhere). The timeout is what
  // gets behind that commit: a discrete keydown flushes its state updates before the browser runs the
  // callback, so this is ordered after the unmount rather than racing it.
  const focusAfterPalette = useCallback(() => {
    window.setTimeout(() => composer.focusInput(), 0)
  }, [composer.focusInput])

  // pickProject answers the ⌘P palette (see palette.tsx): open the chosen row's most recent
  // conversation, or make one. It returns "" when it acted — the palette then closes — or the
  // reason to show IN the palette instead of closing it, because a refusal that dismisses the box
  // reads as success.
  //
  // Two kinds of row, and each has its own rule about creating:
  //   • a PROJECT opens its existing sessions as usual (those fall back to their own snapshot by
  //     design), and refuses to create when its primary directory is gone: the backend's NewSession
  //     falls back to the last used workspace and binds nothing when a project does not drive, so
  //     Enter on 「新建会话」 would quietly produce a 无项目 session — i.e. the row below it.
  //   • the 无项目 BUCKET can refuse nothing: its newest session is opened, and when it has none the
  //     session made is the project-less one (newChat('')), the same one the sidebar's ＋ and ⌘⇧N
  //     make. Nothing about it can be unusable, since it needs no workspace of anyone's.
  //
  // The way IN also decides which group is open — but it does not do that here: opening the group
  // the reader lands in belongs to the two functions that land them (clickSession for an existing
  // conversation, newChat for a new one), and this entry point is one of the several that reach
  // them. A second copy here is how ⌘P's group comes to disagree with the row's own click.
  //
  // A refusal expands nothing, because nothing was switched.
  const pickProject = useCallback((c: ProjectChoice) => {
    if (c.kind === 'loose') {
      const latest = newestOf(looseMembers(sessions, projects))
      if (latest) {
        void clickSession(latest.id)
        focusAfterPalette()
        return ''
      }
      void newChat()
      focusAfterPalette()
      return ''
    }
    const p = c.project
    const latest = latestSessionOf(sessions, p.id)
    if (latest) {
      void clickSession(latest.id)
      focusAfterPalette()
      return ''
    }
    if (!p.rootsUsable) {
      return `「${p.name}」的主目录已不可用，不能在这里新建会话；请先在项目组上右键「编辑目录」修好它`
    }
    void newChat(p.id)
    focusAfterPalette()
    return ''
  }, [sessions, projects, clickSession, newChat, focusAfterPalette])

  // pickSession answers the ⌘K palette (see palette.tsx): switch to the chosen conversation.
  // Nothing can refuse it — ⌘P's reason string exists for the projects that cannot be opened into
  // anything — so the palette closes on the pick, and the caret goes to the composer for the same
  // reason it does there.
  //
  // Which group that opens is clickSession's business (⌘K means 「带我去那次对话」, and the switch is
  // the switch) — this palette picks the conversation and nothing else.
  const pickSession = useCallback((s: SessionItem) => {
    void clickSession(s.id)
    focusAfterPalette()
  }, [clickSession, focusAfterPalette])

  // The app body is INERT while the settings page is up: the page is fixed, opaque and covers the
  // window (settings.css), so what is behind it needs to leave the tab order and the accessibility
  // tree — and nothing else. NOT `visibility: hidden`: WebKit does not paint a hidden subtree and
  // does not reliably resume painting the runs inside it that carry a `text-overflow: ellipsis`, so
  // the sidebar's session titles came back BLANK after every visit to the settings page until
  // something repainted the row (issue #27). `inert` leaves the paint alone, which is also what
  // keeps the transcript's scroll offset and the composer's draft alive across the round trip.
  const appBodyRef = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    appBodyRef.current?.toggleAttribute('inert', view === 'settings')
  }, [view])

  // Keyboard shortcuts: Cmd+/ focuses the composer, Cmd+B toggles the sidebar,
  // Cmd+N starts a new session like the one on screen, Cmd+Shift+N one that belongs to no
  // project, Cmd+P searches projects, Cmd+K searches conversations. (No native menu binds
  // them, so the webview sees the key events.)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // The settings page has no conversation shortcuts. Gating here rather than leaving it to
      // focus is deliberate: the chat view is still MOUNTED (inert), so its windows-level
      // listeners are alive, and ⌘N on the settings page must not quietly start a session
      // behind it. Escape is gated with the rest — the page's own dismissals are its own.
      if (view === 'settings') return
      if (e.key === 'Escape') {
        // Claim the gesture only when something here was actually dismissed: the side panel
        // listens for Escape too (its × promises 「关闭（Esc）」), and it treats `defaultPrevented`
        // as "somebody else used this key". An unconditional preventDefault would swallow every
        // Escape before the panel's handler could see it as unclaimed.
        if (shortcutsOpen || confirmDel || menu || rewindMenu || rewindCard || reminderModal || chain
          || projectMenu || projectForm || confirmDelProject || editingProject || paletteOpen
          || sessionPaletteOpen) e.preventDefault()
        setShortcutsOpen(false); setConfirmDel(null); setMenu(null); setRewindMenu(null)
        setRewindCard(null); setReminderModal(null); setChain(null)
        // The project surfaces dismiss with the same gesture. The rename editor is last: it is
        // the one the user is typing in, so a stray Escape there closes the editor, not the
        // window's other layers.
        setProjectMenu(null); setConfirmDelProject(null); setEditingProject(null)
        setPaletteOpen(false); setSessionPaletteOpen(false)
        if (projectForm) setProjectForm(null)
        return
      }
      if (!isMod(e)) return
      // A palette OWNS the box while it is up — on a platform whose shortcut modifier IS Ctrl
      // (everywhere but macOS) its list navigation is Ctrl+N / Ctrl+P (listNav), which is the very
      // key ⌘N / ⌘P ride: without this, Ctrl+N inside the box would move the highlight AND start a
      // session behind it, and Ctrl+P would close the palette it was navigating. macOS needs no
      // such rule — its navigation is Ctrl while its shortcuts are ⌘ — which is why the collision
      // only ever showed up on Linux. ⌘K stays out of it: no list key is named k, so its toggle
      // still closes the box (see the two branches below).
      const paletteUp = (paletteOpen || sessionPaletteOpen) && !IS_MAC
      if (e.key === '/' && !e.shiftKey) { e.preventDefault(); composer.focusInput() }
      else if (e.key === ',') { e.preventDefault(); setView('settings') }
      // The reading size. ⌘= / ⌘⇧= and ⌘- / ⌘⇧- are the same press on a US layout (the shifted
      // forms type + and _), and ⌘0 returns to the stylesheet's own size. The keys reach the page
      // only because the app menu no longer carries Wails' View → Zoom items, which magnified the
      // WHOLE window (see desktop/menu.go).
      else if (e.key === '=' || e.key === '+') { e.preventDefault(); bumpFontScale(1) }
      else if (e.key === '-' || e.key === '_') { e.preventDefault(); bumpFontScale(-1) }
      else if (e.key === '0') { e.preventDefault(); resetFontScale() }
      else if (e.key.toLowerCase() === 'b') { e.preventDefault(); setSidebarCollapsed((v) => !v) }
      // The two palettes are the same box in the same place, so opening one closes the other —
      // and each is a toggle, so its own key closes it again.
      else if (e.key.toLowerCase() === 'p' && !e.shiftKey && !paletteUp) {
        e.preventDefault(); setSessionPaletteOpen(false); setPaletteOpen((v) => !v)
      }
      else if (e.key.toLowerCase() === 'k' && !e.shiftKey) {
        e.preventDefault(); setPaletteOpen(false); setSessionPaletteOpen((v) => !v)
      }
      else if (e.key.toLowerCase() === 'n' && !e.shiftKey && !paletteUp) {
        e.preventDefault()
        newChatLikeCurrent()
      }
      // ⌘⇧N is the other half of ⌘N: 「随便开一个新的」, with NO project — the session behind it may
      // supply a workspace that is not what this conversation is about, and until now the only way
      // to get a project-less session was the sidebar's own ＋ (which is at the foot of the list,
      // and that is not a keyboard gesture). Shift is what makes it a different command rather than
      // a spelling of ⌘N; the two are the same key on purpose, so the pair reads as one idea.
      else if (e.shiftKey && e.key.toLowerCase() === 'n' && !paletteUp) {
        e.preventDefault()
        void newChat()
      }
      // ⌘⇧⌫ deletes the session on screen — through its confirmation, always (see askDeleteCurrent).
      // Delete is accepted beside Backspace for the keyboards that have a forward-delete key: it is
      // the same gesture on those, and nothing else in the app binds it.
      else if (e.shiftKey && (e.key === 'Backspace' || e.key === 'Delete')) {
        e.preventDefault()
        askDeleteCurrent()
      }
      // The chain. Shift is what keeps it off ⌘H (macOS hides the window on that one) — and
      // off anything the app already binds.
      else if (e.shiftKey && e.key.toLowerCase() === 'h') {
        e.preventDefault()
        void openChain(currentId, sessions.find((x) => x.id === currentId)?.title || '会话')
      }
      // Voice input: the same toggle the microphone button calls, so the two entry points cannot
      // disagree about what a press means. `repeat` is checked on this one and nowhere else because
      // this key STARTS AND STOPS a recording: holding it down would flip the microphone on and off
      // for as long as the key is held (⌘B repeating just toggles a sidebar).
      else if (e.shiftKey && !e.repeat && e.key.toLowerCase() === 'm') {
        if (!composer.voiceAvailable) return
        e.preventDefault()
        composer.toggleVoice()
      }
      else if (e.key === '?' || (e.shiftKey && e.code === 'Slash')) { e.preventDefault(); setShortcutsOpen((v) => !v) }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [newChat, newChatLikeCurrent, askDeleteCurrent, shortcutsOpen, confirmDel, menu, rewindMenu, rewindCard, reminderModal, chain, openChain, currentId, sessions,
    projectMenu, projectForm, confirmDelProject, editingProject, paletteOpen, sessionPaletteOpen, view,
    // Listed because it is captured by value: availability arrives once, after mount, and a closure
    // from the first render would hold `false` forever (⌘⇧M would be a dead key). `composer` itself
    // is deliberately absent — every method this handler calls is a stable useCallback.
    composer.voiceAvailable])

  // A context menu is dismissed the way every other popover here is: a press outside it
  // (this effect), or Escape (above). `onMouseLeave` alone is NOT a dismissal — it only
  // fires once the pointer has been INSIDE the menu, and the menu is drawn at the click
  // point and animates in, so a right-click can leave it on screen with no gesture that
  // closes it (measured from use: 「右键点空白处关不掉，一直显示着」).
  //
  // `mousedown` rather than `click`: the menu must be gone before the press lands on
  // whatever is underneath it, and a press that lands INSIDE the menu is left alone —
  // the entry's own click closes it after doing its work.
  useEffect(() => {
    if (!menu && !rewindMenu && !projectMenu) return
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || t.closest('.ctx-menu')) return
      setMenu(null); setRewindMenu(null); setProjectMenu(null)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [menu, rewindMenu, projectMenu])

  // The reading size's two stores are settled once, at mount: the boot script has already painted
  // the step this webview remembers, and the file takes over when it has none (see fontsize.ts).
  useEffect(() => {
    reconcileFontScale()
  }, [])

  useEffect(() => {
    const loadProv = async () => {
      try {
        const ps = (await (AgentService as any).ListProviders?.()) || []
        setProviders(ps)
        const info = await (AgentService as any).GetProviderInfo?.()
        if (info) { setProviderName(info.provider) }
      } catch { /* ignore */ }
    }
    loadProv()
  }, [])

  // The generated title arrives as an event; the sidebar renders a list fetched at
  // load/switch, so the row has to be patched in place or it keeps saying 未命名会话.
  const setSessionTitle = useCallback((sid: string, title: string) => {
    setSessions((prev) => prev.map((s) => (s.id === sid ? { ...s, title } : s)))
  }, [])

  // Everything the running turn reports about itself, applied to the transcript
  // (agent:event / agent:tool / agent:turn / agent:error — see agentEvents.ts). The steer
  // reply and the question cleanup come from the composer: both are about the queue/form
  // that lives there.
  useAgentStream({
    currentId,
    updateSession, applyToSession, enqueueDelta, flushDeltas, refreshRunning,
    refreshProvider, answerSteer: composer.answerSteer, clearAsk: composer.clearAsk,
    onSessionTitle: setSessionTitle,
  })

  // moveSession follows a conversation that auto-compaction moved into a new
  // session. It is deliberately NOT a cache rename: the compacted session is a
  // different conversation — it begins at the summary, not at the messages that
  // were summarised — so the old transcript is dropped and the new session is
  // loaded from disk like any other. Carrying the transcript over left the
  // pre-compaction messages visible under the new session, which is exactly what
  // a user scrolling up must not see.
  //
  // What DOES move is what belongs to the conversation rather than to the view:
  // the queue of messages typed while the turn ran, and a pending question. The
  // metrics (context estimate, cost/credit including the cache-hit ring) are
  // re-read for the new session — they are per-session on the backend, and the
  // child has its own ledger from here on.
  const moveSession = useCallback((from: string, to: string) => {
    if (!from || !to || from === to) return
    // The queue of messages typed while the turn ran and its pending question are the
    // conversation's, so they follow it; the transcript's half of the same move (drop the
    // parent's window, carry the running flag over) is the store's own rule — see rekeySession
    // in useTranscript.ts.
    composer.rekeySessions(from, to)
    rekeySession(from, to)
    setCurrentId(to)
    // The compacted child is a new row in the sidebar (the parent stays as the
    // pre-compaction history, exactly as in the TUI). `active` is frontend state,
    // so it is re-derived rather than fetched.
    setSessions((prev) => prev.map((s) => ({ ...s, active: s.id === to })))
    void AgentService.ListSessions()
      .then((list) => { if (list) setSessions(list.map((s) => ({ ...s, active: s.id === to }))) })
      .catch(() => {})
    resetTps()
    void (async () => {
      setLoading(true)
      const page: any = await (AgentService as any).LoadSession?.(to, PAGE_SIZE).catch(() => null)
      setSessionPage(to, {
        messages: page?.messages ? buildTurns(page.messages) : [],
        hasMore: !!page?.hasMore,
        earliestTs: page?.messages?.[0]?.timestamp || '',
      })
      setLoading(false)
      scrollToBottom(true)
    })()
    refreshRunning()
    refreshUsage(to)
    refreshProvider()
    refreshMCP()
    refreshWorkspace(to)
  }, [rekeySession, setSessionPage, refreshRunning, refreshProvider, refreshUsage, refreshMCP, refreshWorkspace, scrollToBottom])

  // Auto-compaction: when the token estimate crosses the configured threshold
  // the agent compacts in-flight and moves the conversation into a new (child)
  // session. The notices carry Go's wording — the TUI and ACP show the same
  // event — and the switch arrives once the stream has settled, because a
  // mid-turn re-key would move the transcript out from under the running
  // message.
  useEffect(() => {
    const offStart = Events.On('agent:compact_start', (event) => {
      const d = event.data as { sessionId: string }
      applyToSession(d.sessionId, 'assistant', (m) => pushPart(m, { type: 'notice', label: '正在压缩对话历史…', done: false }))
    })
    const offDone = Events.On('agent:compact_done', (event) => {
      const d = event.data as { sessionId: string; error?: string; reason?: string; oldCount?: number; summary?: string }
      // A user stop and the compact timeout are not failures worth alarming
      // about: the first is what the stop button is for, the second is retried
      // by the agent loop on its next iteration.
      const label = d.reason === 'cancelled' ? '对话压缩已终止'
        : d.reason === 'timeout' ? '对话压缩超时，稍后重试'
        : d.error ? `对话压缩失败：${d.error}`
        : `对话已压缩（旧消息数 ${d.oldCount ?? 0} 条）`
      applyToSession(d.sessionId, 'assistant', (m) => finishNotice(m, label, d.summary))
    })
    const offSwitch = Events.On('agent:session_switched', (event) => {
      const d = event.data as { previousId: string; sessionId: string }
      moveSession(d.previousId, d.sessionId)
    })
    return () => { offStart?.(); offDone?.(); offSwitch?.() }
  }, [applyToSession, moveSession])

  // A completed rewind moves the conversation backwards in place. The transcript is
  // reloaded from disk rather than patched: the tail is gone, and only the session's own
  // records know what the remaining history looks like. The prompt that started the
  // rewound turn goes back into the composer, so the reader can edit and re-send it —
  // which is the point of going back to it.
  useEffect(() => {
    const off = Events.On('agent:rewound', async (event) => {
      const d = event.data as { sessionId: string; preview: RewindPreviewVO }
      const page: any = await (AgentService as any).LoadSession?.(d.sessionId, PAGE_SIZE)
      if (page?.messages) {
        setSessionPage(d.sessionId, {
          messages: buildTurns(page.messages), hasMore: !!page.hasMore,
          earliestTs: page.messages[0]?.timestamp || '',
        })
      }
      const p = d.preview || ({} as RewindPreviewVO)
      let changed = 0, deleted = 0, added = 0
      for (const r of p.roots || []) {
        changed += r.changed?.length || 0
        deleted += r.deleted?.length || 0
        added += r.added?.length || 0
      }
      // The three outcomes of the workspace half, said out loud in the same words the
      // card used — a rewind whose files did not move must never read as one whose
      // files did. The root note rides along: the tree that was restored is not
      // always the one on screen, and the notice is the last thing the reader sees.
      const summary = (p.noFiles
        ? `未还原任何文件：${p.noFiles}`
        : p.filesUnchanged
          ? '这一轮之后没有文件改动，工作区无需还原'
          : added + changed + deleted === 0 ? '文件无变化'
            : `还原 ${changed} · 删回 ${deleted} · 删除 ${added}`)
        + (p.rootMismatch ? `（${p.rootMismatch}）` : '')
      // Appended as its own message rather than through applyToSession: after a rewind the
      // transcript can be EMPTY (every turn was undone), and applyToSession only attaches to
      // the newest RUNNING assistant — so the one moment the notice matters most is the one
      // moment it would be dropped.
      updateSession(d.sessionId, (list) => [...list, {
        id: `notice-${Date.now()}`, role: 'assistant',
        parts: [{ type: 'notice', label: `已回退到第 ${p.turn} 轮之前`, summary, done: true }],
      }])
      if (p.userText) composer.setInput(p.userText)
    })
    return () => { off?.() }
  }, [updateSession, setSessionPage, composer])

  // toggleGroup opens/closes a sidebar group — a project's, or the 无项目 bucket's, which is why it
  // takes a group KEY and not a project id. The header owns the click; the caret button keeps its
  // own handler (stopping propagation) so a pointer already aimed at the caret still toggles exactly
  // once, while a click meant for ＋ or the rename box never toggles at all.
  const toggleGroup = useCallback((key: string) => {
    setExpandedProjects((p) => ({ ...p, [key]: !p[key] }))
  }, [])

  // activeInGroup returns the session the CURRENT conversation is, when it hangs in this group —
  // a pre-compaction link counts, since a folded chain hides it exactly like the group does. It
  // is what the collapsed header's marker names; nothing else reads it.
  const activeInGroup = (g: { rows: SessionRow<SessionItem>[] }): SessionItem | null => {
    for (const row of g.rows) {
      if (row.session.active) return row.session
      const inChain = row.compactedFrom.find((s) => s.active)
      if (inChain) return inChain
    }
    return null
  }

  // stopChat aborts the running turn in the current session (backend cancels
  // the turn ctx — same mechanism as tui Ctrl+C / acp prompt cancel).
  const stopChat = useCallback(() => {
    AgentService.Stop().catch(() => {})
  }, [])

  // sessionRow renders one session's line in the sidebar. It is a function rather than an inline
  // map body because the chain folding needs it twice: once for the conversation's newest link and
  // once per session it was compacted from (see sessionRows). `data-sid` is the row's own handle,
  // the only way to find one row among rows that carry the same classes (see the revealRow effect).
  const sessionRow = (s: SessionItem, compacted: boolean) => (
    /* Row (not a <button>): it hosts a rename <input> and a context
       menu, so it takes role/tabIndex + Enter/Space instead. */
    <div key={s.id} className={`session ${s.active ? 'active' : ''}${compacted ? ' is-compacted' : ''}`}
      data-sid={s.id}
      onClick={() => clickSession(s.id)}
      role="button" tabIndex={0} onKeyDown={actOnKey(() => clickSession(s.id))}
      onContextMenu={(e) => { e.preventDefault(); setMenu({ sid: s.id, x: e.clientX, y: e.clientY }) }}>
      {editingId === s.id ? (
        <input className="session-rename" autoFocus value={editTitle}
          onChange={(e) => setEditTitle(e.target.value)}
          onClick={(e) => e.stopPropagation()}
          /* Losing focus ends the rename: clicking anywhere else is the same decision as Enter
             ("done"), while Escape already means "never mind". A box that waits for a key kept
             the row in edit mode after the reader had moved on, so the NEXT click landed in a
             text box instead of on whatever it aimed at. An empty box is just an exit. */
          onBlur={() => commitRename(s.id)}
          onKeyDown={(e) => {
            // Enter/Escape during IME composition belong to the
            // candidate window, not to rename/commit.
            if (imeActive(e)) return
            if (e.key === 'Enter') { e.stopPropagation(); commitRename(s.id) }
            else if (e.key === 'Escape') { e.stopPropagation(); setEditingId(''); setEditTitle('') }
          }} />
      ) : (
        <div className="session-title" onDoubleClick={() => { setEditingId(s.id); setEditTitle(s.title || ''); setMenu(null) }}>{s.title || '未命名会话'}</div>
      )}
      <div className="session-meta">
        {runningSet.has(s.id) ? <span className="spin-dot" title="运行中" /> : null}
        {/* A pinned row says so on its own line: the list is sorted pinned-first, and a row that
            jumped to the top with no mark would read as an arbitrary order. The title states the
            RULE, so the reader learns where the pin applies (this group) without having to try it. */}
        {s.pinned ? <span className="session-pin" title="已置顶：排在本组最前（项目内的会话排在项目内）"><PinIcon /></span> : null}
        {/* A compacted-away session says what it is: same title, older time, and it is history. */}
        {compacted ? <span className="session-tag" title="这一段对话在压缩时被摘要取代，可以回看">压缩前</span> : null}
        {/* A session a subsystem owns says so too, and for a stronger reason than 压缩前: its
            title came from a REMOTE prompt, so an unmarked row reads as a conversation the
            reader had. Opening it is allowed (the transcript is worth reading) — the mark is
            what stops it being mistaken for one more row of their own. */}
        {s.kind ? (
          <span className="session-tag is-managed" title="子系统驱动的执行记录：只能查看，不能在这里继续">{managedLabel(s.kind, s.sourceNodeId, s.sourceNode)}</span>
        ) : null}
        {new Date(s.updatedAt).toLocaleString('zh-CN', { hour12: false })}
      </div>
    </div>
  )

  // groupRows renders one group's conversation rows, each with its folded chain. A function for
  // the same reason sessionRow is one: a project group wraps them in its own container (the
  // indent) and the 无项目 bucket does not, and duplicating this markup twice is how the two
  // copies would drift apart.
  const groupRows = (rows: SessionRow<SessionItem>[]) => rows.map((row) => (
    <Fragment key={row.session.id}>
      {sessionRow(row.session, false)}
      {row.compactedFrom.length > 0 ? (
        <button type="button" className={`session-chain${openChains[row.session.id] ? ' is-open' : ''}`}
          aria-expanded={!!openChains[row.session.id]}
          title={openChains[row.session.id] ? '收起压缩前的会话' : '展开压缩前的会话（同一段对话的上一节）'}
          onClick={() => setOpenChains((p) => ({ ...p, [row.session.id]: !p[row.session.id] }))}>
          <span className="session-chain-caret"><Caret open={!!openChains[row.session.id]} /></span>
          压缩前 {row.compactedFrom.length} 节
        </button>
      ) : null}
      {openChains[row.session.id] ? row.compactedFrom.map((s) => sessionRow(s, true)) : null}
      {/* A chain the fold could not reach the end of: its pre-compaction session is not in the list
          any more (deleted by hand, or swept by session cleanup's max_keep). This is a NOTE and not
          a control — there is nothing to unfold, and a button that cannot act is worse than a
          sentence — and it is the whole reason the child's record carries the parent's title: the
          link alone points at a session the row cannot show, and the row would otherwise look like
          a conversation that had never been compacted. */}
      {row.compactedMissing === null ? null : (
        <div className="session-chain-gone"
          title="这一段对话在压缩时被摘要取代；压缩前的那一节已经不在会话列表里（被删除或按 max_keep 清理掉了），无法回看">
          {row.compactedFrom.length > 0 ? '更早的压缩前会话已不在列表' : '压缩前的会话已不在列表'}
          {row.compactedMissing ? `：${row.compactedMissing}` : ''}
        </div>
      )}
    </Fragment>
  ))

  // The provider picker lists names only; the selected provider's model is
  // exposed as its tooltip instead.
  const currentProvider = providers.find((p) => (p.name ?? p.Name) === providerName)
  const currentProviderModel = currentProvider?.model ?? currentProvider?.Model ?? ''

  // What the titlebar names: the session on screen, in the sidebar's own words. Two cases are
  // deliberately NOT the same. A session that is not in the list yet (a launch, or the moment
  // between clicking a row and the list coming back) leaves the bar EMPTY rather than showing
  // 未命名会话, which would flash a name that is about to be replaced by the real one. A session
  // that IS listed with no title yet shows 未命名会话 — the same words its row carries, because a
  // conversation's name is one fact and two surfaces must not spell it differently.
  const currentSessionItem = sessions.find((s) => s.id === currentId)
  const sessionTitle = currentSessionItem ? currentSessionItem.title || '未命名会话' : ''

  // A session a SUBSYSTEM drives (today a receiving node's A2A execution — see lib.ts). It can be
  // opened — reading it is the point — but not driven: the composer is replaced by the reason,
  // because every send from here would be refused by the agent's own entry (agent/managed.go),
  // and an input box that silently cannot send is worse than no box. The refusal there is the
  // boundary; this is the courtesy that keeps a reader from walking into it.
  const managedSession = currentSessionItem?.kind ? currentSessionItem : null

  // The sidebar's grouping (see the memo above): the list renders the project groups, then the
  // plain 新建会话 button, then the 无项目 bucket — so it has to know whether that bucket exists.

  // The row the context menu is open on, resolved once: both the 置顶 item's label (置顶 /
  // 取消置顶) and its target read from it. Undefined when the row is gone (deleted while the
  // menu stood open) — then the toggle is offered for a session that no longer exists, and the
  // backend answers for it, which is the same path every other stale target takes.
  const menuSession = menu ? sessions.find((s) => s.id === menu.sid) : undefined

  return (
    // data-session-id is the ONLY place the displayed session's id is readable now that the
    // titlebar stopped showing it: the smoke drivers use it to tell "switched to another session"
    // from "still on the same one" — a title cannot do that, because 未命名会话 repeats as soon as a
    // second session exists.
    <div className="app" data-session-id={currentId}>
      {/* sidebar-folded exists for ONE reason: with the sidebar folded away, the content column
          slides under the window's own traffic lights, and the titlebar has to buy their room back
          (see the .titlebar rules in layout.css). Where there are no lights the clearance is the
          bar's own padding, so the class changes nothing — it stays on for the macOS rule. */}
      <div className={`app-body${sidebarCollapsed ? ' sidebar-folded' : ''}`} ref={appBodyRef}>
        <aside ref={sidebarW.ref as React.RefObject<HTMLElement>}
          className={`sidebar${sidebarCollapsed ? ' collapsed' : ''}${sidebarW.resizing ? ' is-resizing' : ''}`}
          style={sidebarW.width === null ? undefined : { width: sidebarW.width, minWidth: sidebarW.width }}>
          {/* The window's top strip: the traffic lights are painted onto it on macOS, and off
              macOS — where the window is frameless — it is the handle that window has no bar for.
              It stays empty either way: no brand, no controls. Whether it is a drag region is the
              platform's answer (macOS has one already, at the window level). */}
          <div className={`window-strip${IS_MAC ? '' : ' drag-region'}`} aria-hidden="true" />
          {/* Projects group the list (design §7.1). A project that exists but has no members
              yet still needs an entry point, so the sidebar opens the create form directly
              rather than hiding the feature until a session happens to be bound. This is the
              sidebar's top-level action; the ＋ that makes a project-LESS session is NOT up
              here — it belongs with the rows it makes (the 无项目 bucket's own header). */}
          <button className="new-project" onClick={() => openProjectEditor()}
            title="新建项目：一组会话共用工作区的容器">
            <span className="new-chat-plus">＋</span> 新建项目
          </button>
          <nav className="session-list" ref={sessionListRef}>
            {/* Groups, not one flat list (design §7.1): a project's sessions hang under its
                header, and everything no live project owns — including a session whose
                project_id points at a project that is gone — is the 无项目 bucket. The rows
                themselves are the same conversation rows as before (sessionRows folds a
                compaction chain INSIDE its group, because a chain shares its project). */}
            {groups.map((group) => {
              // What THIS group's window shows, and what it does not (see rowWindow above). The
              // window is per group, so a short group (a project with three sessions) is never held
              // back by the hundred-row bucket beside it.
              const key = groupKey(group)
              const open = !!expandedProjects[key]
              const shown = shownRows(group)
              const hidden = group.rows.length - shown.length
              const active = activeInGroup(group)
              // A group is either a project (a record with a name, a working directory and a member
              // count) or the 无项目 bucket — which is a group on the same terms and has none of
              // those, so the few places the two differ are each spelled out where they differ.
              const count = group.project ? group.project.sessionCount : looseCount
              return (
              <Fragment key={key}>
                {/* The WHOLE header toggles (the caret is one way to say it, not the only one):
                    role=button already covered the row for the keyboard, and a header whose only
                    live spot is a 16px caret reads as a label. The controls inside stopPropagation,
                    or ＋ would collapse the group it creates a session in.

                    The 无项目 bucket wears the same header as a project (is-loose is the whole
                    difference). It IS a group of rows like any other — it folds, it counts, and its
                    ＋ makes a row that lands in it — so a reader reads it with the same eye. What it
                    has no header for is what only a project has: a name of its own to rename, a
                    working directory, a menu to edit or delete it. */}
                <div className={`proj-head${group.project ? '' : ' is-loose'}`} role="button" tabIndex={0}
                  onClick={() => toggleGroup(key)}
                  onKeyDown={actOnKey(() => toggleGroup(key))}
                  onContextMenu={group.project ? (e) => { e.preventDefault(); setProjectMenu({ id: group.project!.id, x: e.clientX, y: e.clientY }) } : undefined}>
                  <button type="button" className="proj-toggle" aria-expanded={open}
                    title={`${open ? '收起' : '展开'}${group.project ? '该项目' : `「${LOOSE_LABEL}」`}`}
                    onClick={(e) => { e.stopPropagation(); toggleGroup(key) }}>
                    <Caret open={open} />
                  </button>
                  {group.project ? (
                    editingProject?.id === group.project.id ? (
                      <input className="project-rename" autoFocus value={editingProject.name}
                        onClick={(e) => e.stopPropagation()}
                        onChange={(e) => setEditingProject({ id: group.project!.id, name: e.target.value })}
                        onBlur={() => { if (editingProject) void commitProjectRename(group.project!.id, editingProject.name) }}
                        onKeyDown={(e) => {
                          if (imeActive(e)) return
                          if (e.key === 'Enter') { e.stopPropagation(); void commitProjectRename(group.project!.id, editingProject.name) }
                          else if (e.key === 'Escape') { e.stopPropagation(); setEditingProject(null); setProjectNotice(null) }
                        }} />
                    ) : (
                      /* No rename on double-click: the header's click toggles the group, so a
                         double-click would toggle twice, and the context menu already offers
                         重命名 (with the refusal notice next to the box). */
                      <span className="proj-name" title={`主目录：${group.project.workingDir}`}>
                        {group.project.name}
                      </span>
                    )
                  ) : (
                    /* The bucket's name is a constant, not a record — the app's one word for 「a
                       conversation that belongs to no project」, spelled the same way in the two
                       palettes (LOOSE_LABEL). */
                    <span className="proj-name" title="不属于任何项目的会话">{LOOSE_LABEL}</span>
                  )}
                  <span className="proj-count" title={`${count} 个会话`}>({count})</span>
                  {/* A COLLAPSED group hides its rows, so a reader whose conversation is in
                      there cannot see where they are — the marker says so and names the session
                      (a bare dot would say "something here", not "you are here"). Collapsed
                      only: expanded, the row's own highlight says it. */}
                  {!open && active
                    ? <span className="proj-current" title={`当前会话「${active.title || '未命名会话'}」在${group.project ? '这个项目' : `「${LOOSE_LABEL}」这一组`}里`}>●</span>
                    : null}
                  {group.project && !group.project.rootsUsable ? (
                    <span className="proj-bad" title="项目主目录不可用：成员会话暂时用自己的快照，可以编辑；修好目录后自动回到项目">目录不可用</span>
                  ) : null}
                  <button type="button" className="proj-add"
                    title={group.project ? '在该项目里新建会话' : '新建一个不属于任何项目的会话'}
                    onClick={(e) => { e.stopPropagation(); void newChat(group.project?.id ?? '') }}>＋</button>
                </div>
                {projectNotice && projectNotice.id === group.project?.id ? (
                  <div className="proj-notice">⚠ {projectNotice.msg}</div>
                ) : null}
                {!open ? null : (
                  // is-empty: an EMPTY group gets no rail (see .proj-rows in layout.css) — the rail
                  // says 「the group's rows hang here」, and there are none to hang.
                  <div className={`proj-rows${group.rows.length === 0 ? ' is-empty' : ''}${group.project ? '' : ' is-loose'}`}>
                    {group.rows.length === 0
                      // A group with no sessions yet is a normal state (a project was just made, or
                      // a group's last session was deleted): the header above is where its first
                      // session is created, and this line says why the group is empty rather than
                      // leaving the reader wondering whether the group even exists.
                      ? <div className="proj-empty">{group.project
                          ? '还没有会话，点上面的 ＋ 在这个项目里新建'
                          : '还没有会话，点上面的 ＋ 新建一个'}</div>
                      : groupRows(shown)}
                    {hidden > 0 ? <MoreRows hidden={hidden} onGrow={() => growRows(group)} /> : null}
                  </div>
                )}
              </Fragment>
              )
            })}
          </nav>
          <footer className="sidebar-footer">
            {/* 设置 opens the settings page (a whole view, not a panel — see view in App).
                用量 is still a placeholder: disabled (and dimmed) rather than a
                clickable-looking no-op. */}
            <button className="footer-item" onClick={() => setView('settings')}
              title="设置：config.yaml 里的可配置项">
              <span className="footer-ico"><SettingsIcon /></span> 设置
            </button>
            <button className="footer-item" disabled title="暂未实现"><span className="footer-ico"><UsageIcon /></span> 用量</button>
          </footer>
          {/* The grab strip along the sidebar's right edge — inside it, like the panel's, because the
              sidebar clips its own overflow and a handle hanging outside would be cut off. A real
              focusable separator: a drag is not the only way to widen the list. Absent while the
              sidebar is folded, where there is no width to drag. */}
          {sidebarCollapsed ? null : <div className={`sidebar-resizer${sidebarW.resizing ? ' is-dragging' : ''}`} {...sidebarW.handleProps} />}
        </aside>

        <main className="main">
          {/* The toolbar belongs to the CONTENT COLUMN, not to the window: the sidebar runs the
              window's full height beside it (see docs/agents/desktop.md). What it carries is
              therefore the content column's own facts — which conversation this is, and the two
              toggles that change how the column is laid out. The session ID used to sit here and
              does not any more: it is an implementation handle, and the one gesture it offered now
              lives in the session row's context menu (「复制 session id」). The TITLE is different —
              it is what the reader calls this conversation, and with the sidebar folded there was
              otherwise nothing on screen saying which one was open. */}
          <header className="titlebar drag-region">
            <button className={`sidebar-toggle no-drag ${sidebarCollapsed ? 'is-collapsed' : ''}`} onClick={() => setSidebarCollapsed((v) => !v)} title={sidebarCollapsed ? '展开会话侧栏' : '收起会话侧栏'}>
              <svg width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"><rect x="1.5" y="2.5" width="13" height="11" rx="2"/><line x1="6" y1="2.5" x2="6" y2="13.5"/></svg>
            </button>
            <div className="titlebar-title" title={sessionTitle || undefined}>{sessionTitle}</div>
            <div className="titlebar-right no-drag">
              <button className={`sidebar-toggle oneoff-toggle no-drag ${oneoffOpen ? '' : 'is-collapsed'}`}
                onClick={() => setOneOffOpen(!oneoffOpen)}
                title={oneoffOpen ? '收起旁路面板' : '展开旁路面板（评审 / 提交的过程）'}>
                <svg width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"><rect x="1.5" y="2.5" width="13" height="11" rx="2"/><line x1="10" y1="2.5" x2="10" y2="13.5"/></svg>
              </button>
              <ThemeToggle theme={theme} onToggle={toggleTheme} />
            </div>
          </header>

          {/* A read-only banner at the top of the transcript: the row in the sidebar marks the
              session, but a reader who came straight to the window never saw that row. The composer
              below is replaced by the same reason, so the two say one thing.

              It is a ROW OF THIS COLUMN — a sibling of the header and the composer — and NOT a
              child of .chat-wrap. That box is `display: flex` in its default ROW direction and holds
              the scroller plus the floats anchored to it (the jump pill, the find bar), so a second
              in-flow child there lays out BESIDE the transcript: the banner's `flex-shrink: 0` let it
              take the left half of the window and squeezed the messages into a right-hand strip. */}
          {managedSession ? (
            <div className="managed-banner" role="note">
              <span className="managed-banner-tag">{managedLabel(managedSession.kind, managedSession.sourceNodeId, managedSession.sourceNode)}</span>
              <span className="managed-banner-note">{managedReadOnlyNote(managedSession.kind!, managedSession.sourceNodeId, managedSession.sourceNode)}</span>
              {/* The escape hatch, and only while there is something to escape: a task that will
                  never end by itself (a peer that died, a state a bug left running) holds this
                  session, and the delete the reader probably wants is refused until it ends. */}
              {managedSession.heldByTask ? (
                <button className="managed-banner-stop"
                  onClick={() => void endSessionTask(managedSession.id)}
                  title="停止这一轮执行（本机所有者操作）">结束正在运行的任务</button>
              ) : null}
              {stopNote ? <span className="managed-banner-stop-note">{stopNote}</span> : null}
            </div>
          ) : null}

          <div className="chat-wrap">
            {/* The bar floats over the transcript (its own position: .find-bar.is-chat) rather than
                taking a row: it appears on a keypress, and a row appearing would shift every message
                down at the moment the reader is trying to read one. */}
            {chatFind.open ? <FindBar find={chatFind} className="is-chat" /> : null}
            <div className="chat" ref={chatRef} onScroll={handleScroll} tabIndex={0} onKeyDown={onChatKey}>
            {/* The transcript's own box, and the thing follow-the-bottom watches: the
                SCROLLPORT's box does not change when its content grows, so the observer has to
                sit on the content. See the ResizeObserver effect. */}
            <div className="chat-content" ref={chatContentRef}>
            {loading ? <div className="chat-loading">加载会话…</div> : (
              <>
                {messages.length === 0 && (
                  <div className="welcome"><div className="welcome-mark">◆</div><div className="welcome-title">你好，我是 Tachi</div><div className="welcome-sub">在下方输入问题开始对话。左侧可切换或新建会话。</div></div>
                )}
                {messages.map((m) => {
                  if (m.role === 'user') {
                    return (
                      <Fragment key={m.id}>
                        <UserBubble onContextMenu={(e) => { void openRewindMenu(e, m, currentId) }}>
                          {m.text}
                          <span className="user-meta">
                            {m.reminder ? <button className="reminder-head" title="系统提醒" onClick={() => setReminderModal(m.reminder || '')}><span className="reminder-ico">!</span></button> : null}
                            {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
                          </span>
                        </UserBubble>
                      </Fragment>
                    )
                  }
                  // 「评审所有改动」 is offered on the LATEST turn ONLY (isLatest): its subject is the
                  // working tree as it is now, so it belongs to the turn the reader has just watched
                  // rather than to every turn in the transcript. The turn-scoped entry is the
                  // opposite and stays on every turn that has one — its subject is frozen in that
                  // turn's checkpoints and does not move.
                  //
                  // The number passed with it is the working tree's uncommitted file count, which is
                  // also what makes a turn that changed NOTHING of its own carry the entry: the
                  // changes it reviews came from earlier turns, and this footer is the only place in
                  // the transcript that can say so.
                  const isLatest = m.id === latestTurnMsgId
                  return (
                    <AssistantBubble
                      key={m.id}
                      m={m}
                      workDir={workDir}
                      elapsedMs={m.running ? runningElapsedMs : undefined}
                      onFoldToggle={repinAfterFoldToggle}
                      ask={m.running ? (composer.ask?.questions || null) : null}
                      onAnswer={composer.answerCurrent}
                      perm={m.running ? composer.perm : null}
                      onPermAnswer={composer.answerPerm}
                      onToggleDiff={(i) => patchMessage(m.id, (msg) => togglePartDiff(msg, i))}
                      onToggleAllDiffs={(v) => patchMessage(m.id, (msg) => setPartDiffs(msg, v))}
                      onOpenDiffPanel={openDiffPanel}
                      onReviewChanges={startReview}
                      onReviewAll={isLatest ? startReviewAll : undefined}
                      allReviewFiles={isLatest ? wholeTreeFiles : undefined}
                      onOpenOneOff={() => setOneOffOpen(true)}
                      reviewPending={reviewPending?.msgId === m.id && !reviewPending.whole}
                      reviewDone={reviewDoneLabel({ msgId: m.id, whole: false }, reviewResult, oneoff.items)}
                      reviewNotice={reviewNotice?.msgId === m.id && !reviewNotice.whole ? reviewNotice.text : undefined}
                      allReviewPending={isLatest && reviewPending?.msgId === m.id && reviewPending.whole}
                      allReviewDone={isLatest ? reviewDoneLabel({ msgId: m.id, whole: true }, reviewResult, oneoff.items) : undefined}
                      allReviewNotice={isLatest && reviewNotice?.msgId === m.id && reviewNotice.whole ? reviewNotice.text : undefined}
                      sessionBusy={sessionBusy}
                    />
                  )
                })}
              </>
            )}
            </div>
            </div>
            {showJump && (
              <button className="jump-latest" onClick={() => scrollToBottom(true)} title="回到最新消息">
                <span className="jump-ico">↓</span>回到最新
              </button>
            )}
          </div>

          <footer className="composer">
            {managedSession ? (
              <div className="composer-readonly" role="note">
                <span className="composer-readonly-title">只读</span>
                <span className="composer-readonly-note">{managedReadOnlyNote(managedSession.kind!, managedSession.sourceNodeId, managedSession.sourceNode)}</span>
              </div>
            ) : (
              <Composer api={composer} running={isCurrentRunning} onStop={stopChat} onPageTurn={scrollByHalfPage} />
            )}
            <div className="composer-status">
              <div className="work-dir-wrap popover-anchor">
                {rootsOpen ? (
                  <RootsPanel roots={roots} error={rootsError} busy={rootsBusy}
                    onPickPrimary={() => pickWorkDir(currentId)}
                    onAdd={() => addRoots(currentId)}
                    onRemove={(p) => removeRoot(currentId, p)}
                    onEditProject={(pid) => {
                      const p = projects.find((x) => x.id === pid)
                      // Editing from the panel implies the project exists; if the group list is
                      // stale the form still opens, empty, and the backend refuses a save that
                      // names no project — better than a dead button.
                      setRootsOpen(false)
                      openProjectEditor(p)
                    }}
                    onNewSessionInProject={(pid) => { setRootsOpen(false); void newChat(pid) }}
                    onClose={() => setRootsOpen(false)} />
                ) : null}
                <button type="button" className="work-dir" aria-expanded={rootsOpen}
                  title={workDir ? `工作区目录：${workDir}（点击管理）` : '工作区目录（点击管理）'}
                  onClick={() => { setRootsError(''); setRootsOpen((v) => !v) }}>
                  <span className="work-dir-ico">⌂</span>
                  {/* A member session wears its PROJECT's name (design §7.2): the directory is
                      the project's, and naming the project is what tells the reader that
                      anything they do to it moves their neighbours too. The full path is in the
                      chip's title and in the panel. */}
                  {roots?.projectId && !roots.projectMissing
                    ? <span className="work-dir-project" title={workDir || undefined}>{roots.projectName || '项目'}</span>
                    : (workDir || '未设置工作目录')}
                  {extraRootCount > 0 ? <span className="work-dir-count" title={`${extraRootCount} 个附加目录`}>+{extraRootCount}</span> : null}
                </button>
              </div>
              {/* The plan (P1). Only present when this session has saved one: an always-
                  visible chip that usually says "no plan" is a chip that trains you to
                  ignore it. */}
              {plan?.steps?.length ? (
                <div className="plan-wrap popover-anchor">
                  {planOpen ? <PlanPanel plan={plan} workDir={workDir}
                    onSelect={(p) => void refreshPlan(p)} onDelete={deletePlan}
                    onClose={() => setPlanOpen(false)} /> : null}
                  <PlanChip plan={plan} open={planOpen} onToggle={() => setPlanOpen((v) => !v)} />
                </div>
              ) : null}
              {tps > 0 ? <span className={`usage-tps tps-${tpsTier(tps)}`} title="当前输出速率">{tps}/s</span>
                : lastTps > 0 ? <span className="usage-tps tps-paused" title="最近输出速率">{lastTps}/s</span>
                : null}
              <div className="provider-picker">
                <select className="provider-select" value={providerName}
                  title={currentProviderModel ? `模型：${currentProviderModel}` : undefined}
                  onChange={async (e) => {
                    const name = e.target.value
                    await (AgentService as any).SwitchProvider?.(name)
                    refreshProvider()
                    // A different provider is a different context window, so the ring has to
                    // re-read (it is no longer a side effect of refreshProvider — see
                    // useSessionUsage).
                    refreshUsage(currentId)
                  }}>
                  {/* Names only — the model string is long and often identical
                      to the provider name; the tooltip above keeps it reachable. */}
                  {providers.map((p) => <option key={p.name ?? p.Name} value={p.name ?? p.Name}>{(p.name ?? p.Name)}</option>)}
                </select>
                <select className="provider-select" value={thinkingLevel} onChange={async (e) => {
                  const lv = e.target.value
                  // A level the backend refuses (an endpoint that rejects it) is
                  // NOT shown as if it were set: re-read, which puts the select
                  // back on the level the session actually has.
                  const res = await (AgentService as any).SetThinkingLevel?.(lv)
                  if (res && res !== 'ok') { refreshProvider(); return }
                  setThinkingLevel(lv)
                }}>
                  {/* The current level is always drawn, even when the provider's
                      set excludes it (a level chosen before the session moved
                      to this endpoint): a select whose value has no option
                      renders blank, which would hide what is actually set. */}
                  {(thinkingLevels.includes(thinkingLevel) ? thinkingLevels : [thinkingLevel, ...thinkingLevels]).map((l) => <option key={l} value={l}>{l}</option>)}
                </select>
                <ContextMeter sessionId={currentId} estimate={ctxEstimate} window={ctxWindow} />
                <span className="usage-meta">
                  {hasCacheHit ? <CacheRing rate={cacheHitRate} /> : null}
                  {cost > 0 ? <span className="usage-cost" title="当前会话成本">¥{cost.toFixed(3)}</span> : null}
                  {credit > 0 ? <span className="usage-credit" title="当前会话积分">{fmtCredit(credit)} 积分</span> : null}
                </span>
                <span className="popover-anchor">
                  <button className="agents-btn" title="Agents（可委派的节点）" onClick={() => setAgentsOpen((v) => !v)}>
                    <span className="agents-ico"><AgentsIcon /></span>
                    {/* The count is the number of nodes that ANSWERED the last probe, not the number
                        configured: a machine that cannot delegate to anybody should read as an empty
                        badge, not as "2". */}
                    <span className="agents-count">{(agentsBoard?.peers || []).filter((p) => p.reachable).length || ''}</span>
                  </button>
                  {agentsOpen && (
                    <AgentsPanel
                      board={agentsBoard}
                      busy={agentsBusy}
                      onClose={() => setAgentsOpen(false)}
                      onRefresh={() => void refreshAgents()}
                    />
                  )}
                </span>
                <span className="popover-anchor">
                  <button className="mcp-btn" title="MCP servers / tools" onClick={() => setMcpOpen((v) => !v)}>
                    <span className="mcp-ico"><MCPIcon /></span>
                    <span className="mcp-count">{mcpServers.filter((s) => s.connected).length || ''}</span>
                  </button>
                  {/* Anchored to the button itself (see .popover-panel), like the
                      ring's breakdown: the status row's right end is where the
                      triggers are, so the panel hangs off its own trigger and
                      stays attached to the thing that opened it. */}
                  {mcpOpen && (
                    <MCPPanel
                      servers={mcpServers}
                      loading={mcpLoading}
                      profile={mcpProfile}
                      auth={mcpAuth}
                      onClose={() => setMcpOpen(false)}
                      onToggleServer={toggleServer}
                      onToggleTool={toggleTool}
                      onToggleProfile={toggleProfile}
                      onAuthorize={authorizeServer}
                      onCompleteAuth={completeMCPAuth}
                    />
                  )}
                </span>
              </div>
            </div>
          </footer>
        </main>
        {oneoffOpen ? <OneOffPanel api={oneoff} workDir={workDir} busy={isCurrentRunning} width={oneoffWidth}
          onResizeCommit={commitOneOffWidth} onSend={sendFindings}
          /* The re-run asks the SAME question the record says was asked: a whole-tree run is
             re-run as a whole-tree run (re-reading the working tree, which is the point — the
             changes may have moved since), and a turn-scoped one over that turn's files. */
          onRerun={(turn, paths, msgId, whole) => void (whole ? startReviewAll(msgId) : startReview(turn, paths, msgId))}
          onClose={() => setOneOffOpen(false)} /> : null}
      </div>
      {rewindMenu && (() => {
        // The menu is built here rather than inline so the target resolution reads in one
        // place: which turn contains this bubble, and whether it can start a rewind at all.
        const rt = rewindTargetForMessage(rewindMenu.msg, rewindMenu.turns || [])
        return (
          <div className="ctx-menu" role="menu" style={{ left: rewindMenu.x, top: rewindMenu.y }} onMouseLeave={() => setRewindMenu(null)}>
            <button className="ctx-item" role="menuitem" disabled={rt.kind !== 'ok'}
              title={rt.kind === 'steer'
                ? '插话不单独成检查点：请用本轮开头那条消息回退'
                : rt.kind === 'none' ? '这一轮没有检查点（可能已被裁剪）' : undefined}
              onClick={() => { if (rt.kind === 'ok' && rt.turn != null) void openRewindCard(rewindMenu.sid, rt.turn) }}>回退到这里</button>
          </div>
        )
      })()}
      {rewindCard && (
        <div className="confirm-overlay" onClick={() => setRewindCard(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">回退到第 {rewindCard.preview.turn} 轮之前？</div>
            <div className="confirm-sub">
              {rewindCard.preview.blocked
                ? `不能回退：${rewindCard.preview.blocked}`
                : rewindCard.preview.userText
                  ? `会撤销这一轮之后的全部工作，并把「${rewindCard.preview.userText.slice(0, 60)}${rewindCard.preview.userText.length > 60 ? '…' : ''}」放回输入框。`
                  : '会撤销这一轮之后的全部工作，并把该轮的提示词放回输入框。'}
            </div>
            {/* The workspace half is a three-way answer and the card has to say which one
                it is BEFORE the button: files to restore (the list below), nothing to
                restore because nothing wrote (a fact, not a warning), or nothing WAS
                restored (the conversation moves back and the workspace does not — the one
                outcome a reader must never discover afterwards). */}
            {!rewindCard.preview.blocked && rewindCard.preview.filesUnchanged ? (
              <div className="rewind-note">这一轮及之后没有文件改动，工作区无需还原。</div>
            ) : null}
            {!rewindCard.preview.blocked && rewindCard.preview.noFiles ? (
              <div className="confirm-error">
                ⚠ 不会还原任何文件：{rewindCard.preview.noFiles}。对话照样回退，工作区保持现状。
              </div>
            ) : null}
            {/* The tree that would be restored is not always the one on screen: the
                session may have been pointed elsewhere since that turn. Saying it here,
                above the list, is what keeps "the workspace went back" from reading as
                "the files I am looking at moved". */}
            {!rewindCard.preview.blocked && rewindCard.preview.rootMismatch ? (
              <div className="rewind-note">
                ⚠ {rewindCard.preview.rootMismatch}。回退的是那一轮所在的工作区。
              </div>
            ) : null}
            {rewindCard.preview.roots?.length ? (
              <div className="rewind-roots">
                {rewindCard.preview.roots.map((r) => (
                  <div key={r.root} className="rewind-root">
                    <div className="rewind-root-head">{r.root}</div>
                    <div className="rewind-root-counts">
                      还原 {r.changed?.length || 0} · 删回 {r.deleted?.length || 0} · <b>删除 {r.added?.length || 0}</b>
                    </div>
                    {r.added?.length ? (
                      <ul className="rewind-added">{r.added.slice(0, 8).map((f) => <li key={f}>{f}</li>)}
                        {r.added.length > 8 ? <li>…还有 {r.added.length - 8} 个</li> : null}</ul>
                    ) : null}
                  </div>
                ))}
              </div>
            ) : null}
            {rewindCard.preview.irreversible?.length ? (
              <div className="confirm-error">无法撤销：{rewindCard.preview.irreversible.join('；')}</div>
            ) : null}
            {rewindCard.error ? <div className="confirm-error">⚠ {rewindCard.error}</div> : null}
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setRewindCard(null)}>取消</button>
              <button className="btn danger" disabled={!!rewindCard.preview.blocked}
                onClick={() => { void confirmRewind(rewindCard) }}>回退</button>
            </div>
          </div>
        </div>
      )}
      {menu && (
        <div className="ctx-menu" role="menu" style={{ left: menu.x, top: menu.y }} onMouseLeave={() => setMenu(null)}>
          {/* 置顶 first: it is the item a reader reaches for while scanning the list, and the only
              one here that is about WHERE the row sits rather than what happens to it. */}
          <button className="ctx-item" role="menuitem"
            onClick={() => { void togglePin(menu.sid, !menuSession?.pinned); setMenu(null) }}>{menuSession?.pinned ? '取消置顶' : '置顶'}</button>
          <button className="ctx-item" role="menuitem" onClick={() => { void openChain(menu.sid, sessions.find((x) => x.id === menu.sid)?.title || '会话') }}>回退链…</button>
          <button className="ctx-item" role="menuitem" onClick={() => { AgentService.OpenSessionDir(menu.sid).catch(() => {}); setMenu(null) }}>打开会话目录</button>
          {/* No 「已复制」 flash: the menu is gone by the time the promise settles, and the click
              closing it IS the acknowledgement — the same bargain the titlebar's affordance made.
              copyText, not navigator.clipboard: this page is not a secure context, and the bare call
              reads as a no-op there (the clipboard stayed empty — see lib.ts). */}
          <button className="ctx-item" role="menuitem"
            onClick={() => { copyText(menu.sid); setMenu(null) }}>复制 session id</button>
          <button className="ctx-item" role="menuitem" onClick={() => { setEditingId(menu.sid); setEditTitle(sessions.find((x) => x.id === menu.sid)?.title || ''); setMenu(null) }}>重命名</button>
          <button className="ctx-item danger" role="menuitem" disabled={runningSet.has(menu.sid)}
            title={runningSet.has(menu.sid) ? '会话正在运行，请先停止这一轮' : undefined}
            onClick={() => { const t = sessions.find((x) => x.id === menu.sid)?.title || ''; setConfirmDel({ sid: menu.sid, title: t }); setMenu(null) }}>删除</button>
        </div>
      )}
      {projectMenu && (
        <div className="ctx-menu" role="menu" style={{ left: projectMenu.x, top: projectMenu.y }} onMouseLeave={() => setProjectMenu(null)}>
          <button className="ctx-item" role="menuitem"
            onClick={() => {
              const p = projects.find((x) => x.id === projectMenu.id)
              if (p) { setEditingProject({ id: p.id, name: p.name }); setProjectNotice(null) }
              setProjectMenu(null)
            }}>重命名</button>
          <button className="ctx-item" role="menuitem"
            onClick={() => openProjectEditor(projects.find((x) => x.id === projectMenu.id))}>编辑目录…</button>
          <button className="ctx-item danger" role="menuitem"
            onClick={() => {
              const p = projects.find((x) => x.id === projectMenu.id)
              setConfirmDelProject({ id: projectMenu.id, name: p?.name || '项目', count: p?.sessionCount || 0 })
              setProjectMenu(null)
            }}>删除项目…</button>
        </div>
      )}
      {confirmDelProject && (
        <div className="confirm-overlay" onClick={() => setConfirmDelProject(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">删除项目「{confirmDelProject.name}」？</div>
            {/* What the user is agreeing to, in their own terms: the sessions are NOT deleted —
                they keep their workspace and become ordinary conversations again (§6.2). */}
            <div className="confirm-sub">
              {confirmDelProject.count > 0
                ? `项目里的 ${confirmDelProject.count} 个会话会保留下来（工作区不变），只是不再由项目统一管理。`
                : '该项目还没有会话。'}
              {' '}项目本身会被移除，此操作不可恢复。
            </div>
            {confirmDelProject.error ? <div className="confirm-error">⚠ {confirmDelProject.error}</div> : null}
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setConfirmDelProject(null)}>取消</button>
              <button className="btn danger" onClick={() => { const id = confirmDelProject.id; void deleteProject(id) }}>删除项目</button>
            </div>
          </div>
        </div>
      )}
      {projectForm && (
        <div className="confirm-overlay" onClick={() => setProjectForm(null)}>
          <div className="confirm-box project-form" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">{projectForm.mode === 'create' ? '新建项目' : '编辑项目'}</div>
            <div className="confirm-sub">项目是一组会话共用的工作区：主目录加若干附加目录，项目里所有会话都跟着它走。</div>

            <div className="form-row">
              <label className="form-label" htmlFor="proj-name">名字</label>
              <input id="proj-name" className="form-input" value={projectForm.name} placeholder="留空则取主目录的名字"
                onChange={(e) => setProjectForm({ ...projectForm, name: e.target.value, error: undefined })} />
            </div>

            <div className="form-row">
              <span className="form-label">主目录</span>
              <div className="form-path">
                <span className="form-path-value" title={projectForm.primary || undefined}>
                  <bdi>{projectForm.primary || '未选择'}</bdi>
                </span>
                <button className="roots-btn" onClick={() => void pickProjectPrimary()}>
                  {projectForm.primary ? '更换…' : '选择…'}
                </button>
              </div>
            </div>

            <div className="form-row">
              <span className="form-label">附加目录</span>
              <div className="form-roots">
                {projectForm.additional.length === 0
                  ? <div className="roots-empty">还没有附加目录</div>
                  : projectForm.additional.map((p) => (
                    <div key={p} className="roots-row">
                      <span className="roots-path" title={p}><bdi>{p}</bdi></span>
                      <button className="roots-btn"
                        onClick={() => setProjectForm({
                          ...projectForm,
                          additional: projectForm.additional.filter((x) => x !== p),
                          error: undefined,
                        })}>移除</button>
                    </div>
                  ))}
                <button className="roots-add" onClick={() => void addProjectRoots()}>＋ 添加目录</button>
              </div>
            </div>

            {projectForm.error ? <div className="confirm-error">⚠ {projectForm.error}</div> : null}
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setProjectForm(null)}>取消</button>
              <button className="btn" disabled={!projectForm.primary} onClick={() => void saveProject()}>保存</button>
            </div>
          </div>
        </div>
      )}
      {confirmDel && (
        <div className="confirm-overlay" onClick={() => setConfirmDel(null)}>
          {/* The box TAKES FOCUS (see confirmBoxRef): Enter has to mean 「确认」 here, and a box that
              leaves the caret in the composer would send a message instead — which is exactly where
              the caret is when ⌘⇧⌫ is pressed. tabIndex={-1} makes a div focusable without putting it
              in the Tab order. */}
          <div className="confirm-box" role="dialog" aria-label="删除会话" tabIndex={-1} ref={confirmBoxRef}
            onClick={(e) => e.stopPropagation()}
            onKeyDown={(e) => {
              // IME composition keys belong to the candidate window, as everywhere else in the app.
              if (imeActive(e)) return
              if (e.key === 'Enter') { e.preventDefault(); confirmDelete(confirmDel.sid) }
            }}>
            <div className="confirm-msg">删除会话「{confirmDel.title || '未命名会话'}」？</div>
            <div className="confirm-sub">此操作不可恢复。</div>
            {confirmDel.error ? <div className="confirm-error">⚠ {confirmDel.error}</div> : null}
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setConfirmDel(null)}>取消</button>
              <button className="btn danger" onClick={() => { const id = confirmDel.sid; confirmDelete(id) }}>删除</button>
            </div>
          </div>
        </div>
      )}
      {paletteOpen && (
        <ProjectPalette projects={projects} sessions={sessions}
          onPick={pickProject} onClose={() => setPaletteOpen(false)} />
      )}
      {sessionPaletteOpen && (
        <SessionPalette sessions={sessions} projects={projects} activeId={currentId}
          onPick={pickSession} onClose={() => setSessionPaletteOpen(false)} />
      )}
      {shortcutsOpen && (
        <div className="confirm-overlay" onClick={() => setShortcutsOpen(false)}>
          <div className="confirm-box shortcuts-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">快捷键</div>
            {/* The rows scroll, the title and the 关闭 button do not: every binding in the app is in
                here, so the sheet outgrows a short window — and a 关闭 that has scrolled away is the
                one control the reader needs to leave. */}
            <div className="shortcut-body">
              <div className="shortcut-section">全局</div>
              <div className="shortcut-row"><kbd>{MOD_KEY} /</kbd><span>聚焦输入框</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} = / {MOD_KEY} -</kbd><span>放大 / 缩小阅读字号（{MOD_KEY} 0 复位；消息区、评审报告与预览一起变）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} ,</kbd><span>打开设置（设置页里按 Esc 返回会话）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} N</kbd><span>新建会话（当前会话属于项目时，就在该项目里新建）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} ⇧ N</kbd><span>新建会话（不属于任何项目，侧栏停在新建的那一行）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} ⇧ ⌫</kbd><span>删除当前会话（先弹确认框，按 Enter 确认）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} P</kbd><span>项目搜索（回车打开该项目最近的会话，没有就新建）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} K</kbd><span>会话搜索（按标题过滤，回车切到该会话）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} ⇧ ↑ / ↓</kbd><span>加高 / 变矮输入框（在输入框里按）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} ⇧ H</kbd><span>回退链（本会话的可回退点）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} ⇧ M</kbd><span>语音输入：开始 / 结束录音（文字进输入框，不自动发送）</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} B</kbd><span>折叠 / 展开侧栏</span></div>
              <div className="shortcut-row"><kbd>{MOD_KEY} ?</kbd><span>显示本快捷键列表</span></div>
              <div className="shortcut-section">旁路面板 / diff / 文件预览</div>
              <div className="shortcut-row"><kbd>{MOD_KEY} F</kbd><span>在本页查找（Enter / Shift+Enter 上下，Esc 关闭）</span></div>
              <div className="shortcut-section">消息区（聚焦时）</div>
              <div className="shortcut-row"><kbd>G</kbd><span>跳到最新消息</span></div>
              <div className="shortcut-row"><kbd>gg</kbd><span>回到顶部</span></div>
              <div className="shortcut-row"><kbd>Ctrl U / D</kbd><span>上 / 下翻半页（焦点在消息区或输入框时都生效）</span></div>
            </div>
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setShortcutsOpen(false)}>关闭（Esc）</button>
            </div>
          </div>
        </div>
      )}
      {reminderModal && (
        <div className="confirm-overlay" onClick={() => setReminderModal(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">系统提醒</div>
            <div className="reminder-modal-body">{reminderModal}</div>
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setReminderModal(null)}>关闭（Esc）</button>
            </div>
          </div>
        </div>
      )}
      {turnDiff ? <TurnDiffOverlay sessionId={turnDiff.sessionId} turn={turnDiff.turn} paths={turnDiff.paths}
        onClose={() => setTurnDiff(null)} /> : null}
      {chain ? <RewindChainOverlay turns={chain.turns} title={chain.title} position={chain.position ?? -1}
        onPick={(turn) => { const sid = chain.sid; setChain(null); void openRewindCard(sid, turn) }}
        onClose={() => setChain(null)} /> : null}
      {/* Last, so it is above the floating chat surfaces as well as over them by z-index (see
          settings.css): a settings page opened while one of them was up must still be the page
          on screen, and it must be the page the keyboard belongs to.

          Rendered only while it is the current view — unlike the chat view, which is hidden
          rather than unmounted. Nothing here is worth keeping across a visit, and a fresh read
          of the schema on the way in is exactly what makes the values on screen the values in
          the file. */}
      {view === 'settings' ? <SettingsPage onBack={() => setView('chat')} /> : null}
    </div>
  )
}

// humanize renders a token count as a compact human-friendly number, e.g.
// 128512 → "128.5K", 2000000 → "2M". Used for the context-ring tooltip.
// Token counts never reach the G scale (context windows max out around M), so
// only K/M are emitted; anything above M is shown verbatim as a fallback.
export default App
