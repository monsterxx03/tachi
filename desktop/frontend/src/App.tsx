import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
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
import { buildTurns, fmtCredit, fmtDur, fmtTime, tpsTier, actOnKey, sessionRows, rewindTargetForMessage } from './lib'
import { lastRunningAssistantIndex, turnView, type IndexedPart } from './transcript'
import {
  ContextMeter, CacheRing, ProcessStrip, UserBubble, MCPPanel, AskForm, PermissionForm,
  SettingsIcon, UsageIcon, MCPIcon, ThemeToggle, RootsPanel,
} from './components'
import { TurnPart } from './parts'
import { TurnDiffOverlay } from './diff'
import { OneOffPanel, useOneOffs, oneOffRunLabel, ONE_OFF_PANEL_DEFAULT_WIDTH } from './oneoff'
import { RewindChainOverlay } from './rewindchain'
import type { OneOffRun } from './agentEvents'
import type { OneOffVO, RewindPreviewVO, RewindTurnVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { PlanChip, PlanPanel } from './plan'
import { useTheme, useThemeHostSync } from './theme'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import type { PlanVO, SessionRootsVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

import { pushPart, finishNotice, setPartDiffs, togglePartDiff, turnDiffStat } from './transcript'
import { useSessionTranscript } from './useTranscript'
import { useAgentStatus, useSessionUsage, useAgentStream, useOneOffStream } from './agentEvents'
import { useComposer, Composer, imeActive } from './composer'

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

const AssistantBubble = memo(function AssistantBubble({ m, workDir, runningLabel, elapsedMs, onFoldToggle, ask, onAnswer, perm, onPermAnswer, onToggleDiff, onToggleAllDiffs, onOpenDiffPanel, onReviewChanges, onOpenOneOff, reviewPending, reviewDone, reviewNotice, sessionBusy }: {
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
  // Opens the side-channel panel — where a review's process, findings and report live.
  onOpenOneOff?: () => void
  reviewPending?: boolean
  // The finished review's chip text ("已评审 3 条 · 查看"). Passed as a primitive so this
  // memoized bubble keeps a stable props shape; undefined means "no review yet".
  reviewDone?: string
  reviewNotice?: string
  // The session is mid-turn, so a review has to wait its turn.
  sessionBusy?: boolean
  // Only passed while the turn is running, so finished messages keep a stable
  // props shape and stay memoized.
  runningLabel?: string
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
          {/* The strip also renders while nothing is folded yet but a call is RUNNING: that is
              the moment the live row matters most (the turn's only part is the running card),
              and hiding the row then would make a busy turn look empty. Nothing folded and
              nothing running = nothing to say, so no row at all. */}
          {view.folded.length > 0 || (m.running && view.live) ? (
            <div className="process-block">
              <ProcessStrip summary={view.summary} open={processOpen}
                onToggle={() => setProcessOpen((v) => !v)}
                foldable={view.folded.length > 0}
                live={m.running ? view.live : null} elapsedMs={elapsedMs} />
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
        {m.running ? <span className="running"><span className="typing"><i></i><i></i><i></i></span>{runningLabel ?? '正在执行…'}</span> : null}
        {!m.running && m.stopped ? <span className="stopped-note"><span className="stop-square">⏹</span> 已停止</span> : null}
        {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
        {chip.files > 0 ? (
          <div className="msg-footer">
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
            {/* The second view: the same changes against git HEAD, with real file
                line numbers. The chip above stays the light touch (it toggles the
                inline fragment diffs); this one is the full picture. */}
            <button type="button" className="diff-chip" title="与 git HEAD 对照的完整 diff（真实文件行号）"
              onClick={() => onOpenDiffPanel?.(m.turn ?? 0, diffStat.paths)}>完整 diff</button>
            {/* The review entry: ONE per turn, never per edit card — the review's scope
                is this turn's file set, and findings carry real file lines that a
                fragment card has no coordinates for.
                It is a small state machine, because a review is a side-channel run: its
                process goes to the panel, so once it is done this chip is the way TO it
                rather than another way to start one. */}
            <button type="button" className="diff-chip"
              /* Both 「评审中…」 and 「已评审 · 查看」 LOOK at a run — one to watch it work, one to
                 read it — so both stay clickable. Only STARTING a review waits for the turn to
                 finish. (Disabling on sessionBusy alone is what made the chip unclickable while
                 it was busy running the very review it was reporting on.) */
              disabled={!reviewPending && !reviewDone && sessionBusy}
              title={reviewPending ? '评审进行中（过程在右侧面板）'
                : reviewDone ? '看这次评审的过程、意见与报告（右侧面板）'
                  : sessionBusy ? '等这一轮跑完'
                    : '让 agent 只评审本轮改动的这些文件（只读；过程与意见在右侧面板）'}
              onClick={() => (reviewPending || reviewDone ? onOpenOneOff?.() : onReviewChanges?.(m.turn ?? 0, diffStat.paths, m.id))}>
              {reviewPending ? '评审中…' : (reviewDone || '评审本轮改动')}
            </button>
            {/* The refusal reason is a TOOLTIP, not a paragraph: printed in full here it
                shoved the chips around (the sentence is a whole line), and hovering is the
                honest place for the detail — 「完整 diff」 spells out the same thing. */}
            {reviewNotice ? <span className="diff-notice" title={reviewNotice}>⚠ 没有开始评审</span> : null}
          </div>
        ) : null}
        {m.summary ? (
          <div className="msg-footer">
            {m.summary.durationMs > 0 ? <span>⏱ {fmtDur(m.summary.durationMs)}</span> : null}
            {m.summary.iterations > 0 ? <span>{m.summary.iterations} iters</span> : null}
            {m.summary.cost > 0 ? <span>¥{m.summary.cost.toFixed(3)}</span> : null}
            {m.summary.credit > 0 ? <span>{fmtCredit(m.summary.credit)} 积分</span> : null}
          </div>
        ) : null}
      </div>
    </div>
  )
})

// MODE_META labels the three session modes. Each one changes what the model can do —
// chat and plan drop the destructive tools from its schema, plan additionally appends the
// plan-mode rules — so the label says the capability, not just the name.
const MODE_META: Record<string, { label: string; hint: string }> = {
  auto: { label: '自动', hint: '完整权限：可以改文件、执行命令' },
  chat: { label: '只读', hint: '只读问答：破坏性工具对模型不可见' },
  plan: { label: '计划', hint: '规划模式：只读探索 + SavePlan 产出计划，之后切回自动执行' },
}

function App() {
  const [sessions, setSessions] = useState<SessionItem[]>([])
  const [currentId, setCurrentId] = useState<string>('')
  const [menu, setMenu] = useState<{ sid: string; x: number; y: number } | null>(null)
  // The delete confirmation. `error` holds the backend's refusal (a session with a turn
  // in flight cannot be deleted) so it lands in the box that asked the question, next to
  // the button that was just pressed — a console-only failure would read as a no-op.
  const [confirmDel, setConfirmDel] = useState<{ sid: string; title: string; error?: string } | null>(null)
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
  const [showJump, setShowJump] = useState(false)
  const scrollToBottom = useCallback((force = false) => {
    // force=true is for actions where following is clearly intended (sending a message,
    // switching sessions). Otherwise the pin only happens while the user is parked at the
    // bottom.
    if (force) {
      followBottomRef.current = true
      setShowJump(false)
    }
    const el = chatRef.current
    if (el && followBottomRef.current) el.scrollTop = el.scrollHeight
  }, [])
  // Per-session transcript state — messages, running flags, the loaded window, the delta
  // buffer — has exactly one owner (useTranscript.ts), so "belongs to a session" is a
  // property of the type rather than a convention to remember.
  const {
    msgCache, runningSet, hasMore, earliestTs,
    updateSession, patchMessage, applyToSession, sealRunningSegment, injectSteerVisual,
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
  const [workDir, setWorkDir] = useState('')
  // Workspace roots (primary + additional) and the popover that manages them. The
  // popover is only mounted while open, so its own outside-click/Esc handling is
  // not running behind the scenes.
  const [roots, setRoots] = useState<SessionRootsVO | null>(null)
  const [rootsOpen, setRootsOpen] = useState(false)
  const [rootsBusy, setRootsBusy] = useState(false)
  const [rootsError, setRootsError] = useState('')
  // The turn whose 「完整 diff」 is open: the files that turn changed, with the session they
  // belong to. Owned here because the chip that opens it lives in the transcript, and closed on
  // every session switch (a diff is about the session it was taken in).
  //
  // The REVIEW's findings and its diff are the side-channel panel's business instead: the panel
  // reads them per RUN (desktop/oneoff.go collects each record's own ReportFinding calls), while
  // this overlay answers a question about a TURN — "what does the working tree look like now,
  // against HEAD" — which is answerable with no run at all.
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

  // The session's mode (P2). It is not a label but a capability switch: chat and plan hide
  // the destructive tools from the model, and plan appends the plan-mode rules to the
  // system prompt — so the select sits next to the model, and a rejected switch says why
  // instead of silently springing back.
  const [mode, setMode] = useState('auto')
  const [modeNotice, setModeNotice] = useState('')
  const refreshMode = useCallback(async () => {
    try {
      setMode((await AgentService.GetMode()) || 'auto')
    } catch { /* keep the last known mode */ }
  }, [])
  useEffect(() => { setModeNotice(''); void refreshMode() }, [currentId, refreshMode])
  const changeMode = useCallback(async (next: string) => {
    const res = await AgentService.SetMode(next)
    if (res && res !== 'ok') {
      setModeNotice(res)
      void refreshMode() // show what it actually is, not what was clicked
      return
    }
    setModeNotice('')
    setMode(next)
  }, [refreshMode])
  // A review run started from a turn's footer. Local to the app (not persisted) and keyed by
  // THE TURN WHOSE BUTTON WAS CLICKED (its message id): the button belongs to one footer, so
  // its "评审中…" state and its refusal reason belong there too. Un-scoped they appeared on
  // every turn's footer — and, because the session's messages are re-rendered on a switch,
  // on every other session's as well. A turn id settles both: a message id is unique across
  // sessions, so no bubble can show another turn's answer.
  const [reviewPending, setReviewPending] = useState<{ msgId: string } | null>(null)
  // The finished review, for the footer chip of the turn that started it. It survives the
  // pending state (which the session going idle clears) so the chip can say 已评审 N 条.
  const [reviewResult, setReviewResult] = useState<{ msgId: string; run: OneOffRun } | null>(null)
  // Which turn the run in flight belongs to, held across the result event (see startReview).
  const reviewForRef = useRef('')
  const [reviewNotice, setReviewNotice] = useState<{ msgId: string; text: string } | null>(null)
  // The chip's "+N" badge: the chip itself shows the primary path, so this is what
  // says the workspace extends beyond it.
  const extraRootCount = (roots?.additional || []).length
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  // Theme (light/dark) for the titlebar switch; useThemeHostSync mirrors the
  // active theme to Go, which owns the window colour outside the webview.
  const [theme, toggleTheme] = useTheme()
  useThemeHostSync()
  const [mcpOpen, setMcpOpen] = useState(false)
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
  // typed into the composer or a form field are never intercepted.
  const ggPendingRef = useRef(false)
  const onChatKey = useCallback((e: React.KeyboardEvent<HTMLDivElement>) => {
    const target = e.target as HTMLElement | null
    if (target?.closest('input, textarea, [contenteditable="true"]')) return
    const el = chatRef.current
    if (!el || e.metaKey || e.altKey) return

    if (e.ctrlKey && (e.key === 'u' || e.key === 'd')) {
      e.preventDefault()
      el.scrollTop += (e.key === 'd' ? 1 : -1) * el.clientHeight * 0.5
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
  }, [scrollToBottom])

  // ── Frame-batched deltas ───────────────────────────────────────────────────
  // (enqueueDelta / flushDeltas live in useTranscript.ts — the batcher buffers the
  // streamed deltas of every session, so it belongs with the transcript it feeds.)

  // Pin to the newest content after every committed update while following —
  // doing it in a layout effect means the scroll happens in the same frame the
  // content grows, so the view glides instead of lurching.
  useLayoutEffect(() => {
    const el = chatRef.current
    if (el && followBottomRef.current) el.scrollTop = el.scrollHeight
  }, [msgCache])

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
      const c = chatRef.current
      if (c && followBottomRef.current) c.scrollTop = c.scrollHeight
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // On session switch, jump straight to the newest message BEFORE paint (via
  // useLayoutEffect) so the view never briefly shows the oldest messages and
  // then snaps down. The async-load path still uses scrollToBottom() after the
  // page arrives.
  useLayoutEffect(() => {
    const el = chatRef.current
    if (el) el.scrollTop = el.scrollHeight
    // A session switch always lands at the newest message — re-arm following.
    followBottomRef.current = true
    setShowJump(false)
  }, [currentId])

  const loadMoreRef = useRef(false)
  const handleScroll = () => {
    const el = chatRef.current
    if (!el) return
    // Track bottom-proximity first: scrolling up must pause auto-follow even
    // while a page of older messages is being fetched (the early return below
    // would otherwise swallow it).
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 48
    if (nearBottom !== followBottomRef.current) {
      followBottomRef.current = nearBottom
      setShowJump(!nearBottom)
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
          if (c) c.scrollTop = c.scrollHeight - old
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
  // bottom has to stay there — the design's 「展开/收起不该让滚动位置跳」 — and without this the
  // growth switches following OFF by itself: the view slides up by the height that appeared
  // (the scroll anchor is the top of the viewport), and the scroll event that follows reads as
  // "the reader scrolled away". Called from the bubble's LAYOUT effect, so the new content is
  // already in the DOM and the pin lands in the same frame — not from a rAF or a timer, which
  // an occluded webview never delivers. A reader who had scrolled away is left where they were.
  const repinAfterFoldToggle = useCallback(() => {
    if (!followBottomRef.current) return
    followBottomRef.current = true
    const el = chatRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [])

  // How long the turn on screen has been running: the strip's live row reports it, so the reader
  // can tell "working" from "stuck". One ticker, and the wording lives in the strip itself.
  const runningIdx = isCurrentRunning ? lastRunningAssistantIndex(messages) : -1
  const runningMsg = runningIdx >= 0 ? messages[runningIdx] : undefined
  const runningElapsedMs = useElapsedMs(runningMsg?.ts, !!runningMsg)

  // The composer: the input box, the @-picker and "/" palette, the queue of messages typed
  // while a turn runs, and the answer form for a parked question. It routes what the user
  // commits (send now / queue / slash command) into the transcript store.
  const composer = useComposer({
    currentId,
    running: isCurrentRunning,
    transcript: { updateSession, applyToSession, injectSteerVisual, sealRunningSegment, markRunning, refreshRunning },
    scrollToBottom,
    chatRef,
  })

  const refreshProvider = useCallback(async () => {
    try {
      const info = await (AgentService as any).GetProviderInfo?.()
      if (info) setProviderName(info.provider)
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

  // openDiffPanel fetches the turn's files against git HEAD. It is a deliberate
  // on-demand call: git runs once per open, not once per render.
  // The review's findings and its diff are the side-channel panel's business now: it reads
  // them per RUN (desktop/oneoff.go collects each record's own ReportFinding calls) instead of
  // asking for "the newest review in the session".

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

  // startReview runs the review fork scoped to one turn's files. The run is a normal
  // turn, so its findings stream into the transcript and the diff panel picks them up
  // from there — no extra plumbing, and they survive a restart like any other message.
  // startReview runs the review fork scoped to one turn's files. It runs as a turn (the
  // session goes busy, Stop cancels it), but its process goes to the side-channel panel: the
  // conversation keeps this turn's footer as the anchor, the panel shows the rest.
  const startReview = useCallback(async (turn: number, paths: string[], msgId?: string) => {
    const sid = currentId
    setReviewNotice(null)
    // msgId pairs the run back to the turn whose chip started it. A re-run from the panel may
    // have none (a whole-tree review has no turn) — the run is worth starting either way; what
    // it cannot do is light up a footer that does not exist.
    if (msgId) {
      setReviewPending({ msgId })
      // Remember which turn this is for. The run's result arrives as an event, and by then this
      // state may already be cleared (the session going idle does that), so the pairing cannot
      // lean on reviewPending still being set.
      reviewForRef.current = msgId
    }
    try {
      const res = await AgentService.ReviewChanges(sid, turn, paths, msgId || '')
      // A refusal means no run started, so the pending state ends HERE. Success must NOT clear
      // it: this call only ever STARTS the fork (it runs on in the background), so clearing here
      // made the chip flash 「评审中…」 and fall straight back — onto 「已评审 · 查看」, because
      // the record (with its reviewed_msg) is on disk from the first line, while the run was
      // still going. Measured report: 「点击评审按钮后，立刻会变成已评审查看，但没法点击」 —
      // and it WAS unclickable, because the session is busy running the review.
      if (res) {
        setReviewPending(null)
        reviewForRef.current = ''
        // The refusal belongs to the chip that asked (if one did); a re-run from the panel has
        // nowhere to show it, and its own pane says what happened to the run.
        if (msgId) setReviewNotice({ msgId, text: res })
      }
    } catch (e) {
      setReviewPending(null)
      reviewForRef.current = ''
      if (msgId) setReviewNotice({ msgId, text: String(e) })
    }
  }, [currentId])

  // reviewDoneLabel is the footer chip's 已评审 state for a turn, from two sources:
//
//   - the run that just finished in THIS window (its count is exact — the backend tallied the
//     findings as they were called); and
//   - the records on disk, which is what survives a restart: each review records the turn it
//     was started for (agent.OneOffKeyReviewedMsg), so the pairing comes back with the session.
//     Those runs have no loaded count (counting means reading the whole record), so the chip
//     says 已评审 and the panel — which loads the run's detail when selected — shows how many.
//
// Returns undefined when this turn has never been reviewed, which is what leaves the chip as
// 「评审本轮改动」.
function reviewDoneLabel(msgId: string, result: { msgId: string; run: OneOffRun } | null, runs: OneOffVO[]): string | undefined {
  if (result?.msgId === msgId) return oneOffRunLabel(result.run)
  // A run that is STILL GOING does not count as done, even though its record is already on disk
  // with this turn's id in it: that is what made the chip claim 已评审 while the review was
  // still working. The record's own running flag is an mtime heuristic, so this is the same
  // "still writing" notion the panel uses.
  return runs.some((it) => it.reviewedMsg === msgId && !it.running) ? '已评审 · 查看' : undefined
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
    const msgId = reviewForRef.current
    if (!msgId) return // started from the composer: the notice path owns it
    reviewForRef.current = ''
    setReviewResult({ msgId, run })
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

  // openDiffPanel is the turn's 「完整 diff」 entry: the files THIS turn changed, against git
  // HEAD, in a lightbox over the conversation.
  //
  // It used to hand the job to the side-channel panel, which is scoped to a RUN — so a turn
  // nobody had reviewed opened an empty column (there is no run to show, and the panel's list
  // says so), and a reviewed one showed the SELECTED run's file set rather than this turn's.
  // The paths are the click's own: fetched on open, dropped on close, and never stored per run
  // — which is also what keeps them from becoming the stale anchor P4 removed.
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

  // refreshMCP reloads the MCP servers/tools + profile for the current session.
  const refreshMCP = useCallback(async () => {
    try {
      const sv = (await (AgentService as any).ListMCPServers?.()) || []
      setMcpServers(sv)
      const prof = await (AgentService as any).ListMCPProfiles?.()
      if (prof) setMcpProfile({ active: prof.active || '', available: prof.available || [] })
    } catch { /* ignore */ }
  }, [])
  const toggleServer = useCallback(async (name: string, enabled: boolean) => {
    setMcpLoading((p) => ({ ...p, [name]: true }))
    try { await (AgentService as any).SetMCPServerEnabled?.(name, enabled) } catch { /* ignore */ }
    refreshMCP()
    setMcpLoading((p) => ({ ...p, [name]: false }))
  }, [refreshMCP])
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

  const commitRename = useCallback(async (id: string) => {
    const t = editTitle.trim()
    setEditingId(''); setEditTitle('')
    if (!t) return
    await (AgentService as any).RenameSession?.(id, t).catch(() => {})
    setSessions((prev) => prev.map((s) => (s.id === id ? { ...s, title: t } : s)))
  }, [editTitle])

  const loadAll = useCallback(async () => {
    setLoading(true)
    const list = (await AgentService.ListSessions().catch(() => null)) || []
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
      const ns = await AgentService.NewSession().catch(() => null)
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
    setLoading(false)
    refreshProvider()
    refreshMCP()
    if (cur) refreshWorkspace(cur.id)
  }, [msgCache, openSession, setSessionPage, scrollToBottom, refreshProvider, refreshUsage, refreshMCP, refreshWorkspace, clearUsage, composer.focusInput])
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
    loadAll()
  }, [loadAll])

  const clickSession = useCallback(async (id: string) => {
    setCurrentId(id)
    setCurrentTitle(sessions.find((s) => s.id === id)?.title || 'Tachi')
    setSessions((prev) => prev.map((s) => ({ ...s, active: s.id === id })))
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
  }, [sessions, msgCache, setSessionPage, scrollToBottom, refreshRunning, refreshProvider, refreshUsage, refreshMCP, refreshWorkspace])

  const newChat = useCallback(async () => {
    const ns = await AgentService.NewSession().catch(() => null)
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
      const list = (await AgentService.ListSessions().catch(() => null)) || []
      setSessions(list.map((s) => ({ ...s, active: s.id === ns.id })))
      refreshProvider()
    }
  }, [refreshProvider, refreshWorkspace, refreshUsage, clearUsage, composer.focusInput])

  useEffect(() => { loadAll(); refreshRunning(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [])

  // Keyboard shortcuts: Cmd+/ focuses the composer, Cmd+B toggles the sidebar,
  // Keyboard shortcuts: Cmd+/ focuses the composer, Cmd+B toggles the sidebar,
  // Cmd+N starts a new session. (No native menu binds them, so the webview sees
  // the key events.)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        // Claim the gesture only when something here was actually dismissed: the side panel
        // listens for Escape too (its × promises 「关闭（Esc）」), and it treats `defaultPrevented`
        // as "somebody else used this key". An unconditional preventDefault would swallow every
        // Escape before the panel's handler could see it as unclaimed.
        if (shortcutsOpen || confirmDel || menu || rewindMenu || rewindCard || reminderModal || chain) e.preventDefault()
        setShortcutsOpen(false); setConfirmDel(null); setMenu(null); setRewindMenu(null)
        setRewindCard(null); setReminderModal(null); setChain(null)
        return
      }
      if (!e.metaKey) return
      if (e.key === '/' && !e.shiftKey) { e.preventDefault(); composer.focusInput() }
      else if (e.key.toLowerCase() === 'b') { e.preventDefault(); setSidebarCollapsed((v) => !v) }
      else if (e.key.toLowerCase() === 'n') { e.preventDefault(); newChat() }
      // The chain. Shift is what keeps it off ⌘H (macOS hides the window on that one) — and
      // off anything the app already binds.
      else if (e.shiftKey && e.key.toLowerCase() === 'h') {
        e.preventDefault()
        void openChain(currentId, sessions.find((x) => x.id === currentId)?.title || '会话')
      }
      else if (e.key === '?' || (e.shiftKey && e.code === 'Slash')) { e.preventDefault(); setShortcutsOpen((v) => !v) }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [newChat, shortcutsOpen, confirmDel, menu, rewindMenu, rewindCard, reminderModal, chain, openChain, currentId, sessions])

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
    if (!menu && !rewindMenu) return
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || t.closest('.ctx-menu')) return
      setMenu(null); setRewindMenu(null)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [menu, rewindMenu])

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
      // files did.
      const summary = p.noFiles
        ? `未还原任何文件：${p.noFiles}`
        : p.filesUnchanged
          ? '这一轮之后没有文件改动，工作区无需还原'
          : added + changed + deleted === 0 ? '文件无变化'
            : `还原 ${changed} · 删回 ${deleted} · 删除 ${added}`
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

  // stopChat aborts the running turn in the current session (backend cancels
  // the turn ctx — same mechanism as tui Ctrl+C / acp prompt cancel).
  const stopChat = useCallback(() => {
    AgentService.Stop().catch(() => {})
  }, [])

  // sessionRow renders one session's line in the sidebar. It is a function rather than an inline
  // map body because the chain folding needs it twice: once for the conversation's newest link and
  // once per session it was compacted from (see sessionRows).
  const sessionRow = (s: SessionItem, compacted: boolean) => (
    /* Row (not a <button>): it hosts a rename <input> and a context
       menu, so it takes role/tabIndex + Enter/Space instead. */
    <div key={s.id} className={`session ${s.active ? 'active' : ''}${compacted ? ' is-compacted' : ''}`}
      onClick={() => clickSession(s.id)}
      role="button" tabIndex={0} onKeyDown={actOnKey(() => clickSession(s.id))}
      onContextMenu={(e) => { e.preventDefault(); setMenu({ sid: s.id, x: e.clientX, y: e.clientY }) }}>
      {editingId === s.id ? (
        <input className="session-rename" autoFocus value={editTitle}
          onChange={(e) => setEditTitle(e.target.value)}
          onClick={(e) => e.stopPropagation()}
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
        {/* A compacted-away session says what it is: same title, older time, and it is history. */}
        {compacted ? <span className="session-tag" title="这一段对话在压缩时被摘要取代，可以回看">压缩前</span> : null}
        {new Date(s.updatedAt).toLocaleString('zh-CN', { hour12: false })}
      </div>
    </div>
  )

  // The provider picker lists names only; the selected provider's model is
  // exposed as its tooltip instead.
  const currentProvider = providers.find((p) => (p.name ?? p.Name) === providerName)
  const currentProviderModel = currentProvider?.model ?? currentProvider?.Model ?? ''

  return (
    <div className="app">
      <header className="titlebar drag-region">
        <div className="brand no-drag">
          <span className="brand-mark"><svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><defs><linearGradient id="tg1" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stopColor="var(--accent-3)" /><stop offset="1" stopColor="var(--accent-strong)" /></linearGradient></defs><rect x="1" y="1" width="14" height="14" rx="4.5" fill="url(#tg1)" /><path d="M8 3.8 L12.2 8 L8 12.2 L3.8 8 Z" fill="var(--on-accent)" opacity="0.95" /></svg></span>
          <span className="brand-name">Tachi</span>
        </div>
        <button className={`sidebar-toggle no-drag ${sidebarCollapsed ? 'is-collapsed' : ''}`} onClick={() => setSidebarCollapsed((v) => !v)} title={sidebarCollapsed ? '展开会话侧栏' : '收起会话侧栏'}>
          <svg width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"><rect x="1.5" y="2.5" width="13" height="11" rx="2"/><line x1="6" y1="2.5" x2="6" y2="13.5"/></svg>
        </button>
        <div className="titlebar-divider" />
        <div className="titlebar-title no-drag">
          {currentId ? <span className="session-id" title={currentId + '（点击复制）'} onClick={() => navigator.clipboard?.writeText(currentId).catch(() => {})}>{currentId}</span> : null}
        </div>
        <div className="titlebar-right no-drag">
          <button className={`sidebar-toggle oneoff-toggle no-drag ${oneoffOpen ? '' : 'is-collapsed'}`}
            onClick={() => setOneOffOpen(!oneoffOpen)}
            title={oneoffOpen ? '收起旁路面板' : '展开旁路面板（评审 / 提交的过程）'}>
            <svg width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"><rect x="1.5" y="2.5" width="13" height="11" rx="2"/><line x1="10" y1="2.5" x2="10" y2="13.5"/></svg>
          </button>
          <ThemeToggle theme={theme} onToggle={toggleTheme} />
        </div>
      </header>

      <div className="app-body">
        <aside className={`sidebar${sidebarCollapsed ? ' collapsed' : ''}`}>
          <button className="new-chat" onClick={newChat}><span className="new-chat-plus">＋</span> 新建会话</button>
          <nav className="session-list">
            <div className="session-section">最近</div>
            {/* One row per CONVERSATION, not per session: a compaction chain is folded into its
                newest link, with the sessions it was compacted from underneath it (closed) — see
                sessionRows. Rendering the raw list made the pre-compaction session look like a
                second, identically-titled conversation. */}
            {sessionRows(sessions).map((row) => (
              <Fragment key={row.session.id}>
                {sessionRow(row.session, false)}
                {row.compactedFrom.length > 0 ? (
                  <button type="button" className={`session-chain${openChains[row.session.id] ? ' is-open' : ''}`}
                    aria-expanded={!!openChains[row.session.id]}
                    title={openChains[row.session.id] ? '收起压缩前的会话' : '展开压缩前的会话（同一段对话的上一节）'}
                    onClick={() => setOpenChains((p) => ({ ...p, [row.session.id]: !p[row.session.id] }))}>
                    <span className="session-chain-caret">{openChains[row.session.id] ? '▾' : '▸'}</span>
                    压缩前 {row.compactedFrom.length} 节
                  </button>
                ) : null}
                {openChains[row.session.id] ? row.compactedFrom.map((s) => sessionRow(s, true)) : null}
              </Fragment>
            ))}
            {sessions.length === 0 && <div className="session-empty">暂无会话</div>}
          </nav>
          <footer className="sidebar-footer">
            {/* Both entries are placeholders: disabled (and dimmed) rather than
                clickable-looking no-ops. */}
            <button className="footer-item" disabled title="暂未实现"><span className="footer-ico"><SettingsIcon /></span> 设置</button>
            <button className="footer-item" disabled title="暂未实现"><span className="footer-ico"><UsageIcon /></span> 用量</button>
          </footer>
        </aside>

        <main className="main">
          <div className="chat-wrap">
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
                {messages.map((m) =>
                  m.role === 'user' ? (
                    <Fragment key={m.id}>
                      <UserBubble onContextMenu={(e) => { void openRewindMenu(e, m, currentId) }}>
                        {m.text}
                        <span className="user-meta">
                          {m.reminder ? <button className="reminder-head" title="系统提醒" onClick={() => setReminderModal(m.reminder || '')}><span className="reminder-ico">!</span></button> : null}
                          {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
                        </span>
                      </UserBubble>
                    </Fragment>
                  ) : (
                    <AssistantBubble
                      key={m.id}
                      m={m}
                      workDir={workDir}
                      runningLabel={m.running ? (state.status === 'thinking' ? '正在思考…' : '正在执行…') : undefined}
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
                      onOpenOneOff={() => setOneOffOpen(true)}
                      reviewPending={reviewPending?.msgId === m.id}
                      reviewDone={reviewDoneLabel(m.id, reviewResult, oneoff.items)}
                      reviewNotice={reviewNotice?.msgId === m.id ? reviewNotice.text : undefined}
                      sessionBusy={sessionBusy}
                    />
                  ),
                )}
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
            <Composer api={composer} running={isCurrentRunning} onStop={stopChat} />
            <div className="composer-status">
              <div className="work-dir-wrap popover-anchor">
                {rootsOpen ? (
                  <RootsPanel roots={roots} error={rootsError} busy={rootsBusy}
                    onPickPrimary={() => pickWorkDir(currentId)}
                    onAdd={() => addRoots(currentId)}
                    onRemove={(p) => removeRoot(currentId, p)}
                    onClose={() => setRootsOpen(false)} />
                ) : null}
                <button type="button" className="work-dir" aria-expanded={rootsOpen}
                  title={workDir ? `工作区目录：${workDir}（点击管理）` : '工作区目录（点击管理）'}
                  onClick={() => { setRootsError(''); setRootsOpen((v) => !v) }}>
                  <span className="work-dir-ico">⌂</span>{workDir || '未设置工作目录'}
                  {extraRootCount > 0 ? <span className="work-dir-count" title={`${extraRootCount} 个附加目录`}>+{extraRootCount}</span> : null}
                </button>
              </div>
              {/* The plan (P1). Only present when this session has saved one: an always-
                  visible chip that usually says "no plan" is a chip that trains you to
                  ignore it. */}
              {plan?.steps?.length ? (
                <div className="plan-wrap popover-anchor">
                  {planOpen ? <PlanPanel plan={plan} workDir={workDir} mode={mode}
                    onSelect={(p) => void refreshPlan(p)} onDelete={deletePlan}
                    onSwitchMode={changeMode} onClose={() => setPlanOpen(false)} /> : null}
                  <PlanChip plan={plan} open={planOpen} onToggle={() => setPlanOpen((v) => !v)} />
                </div>
              ) : null}
              {tps > 0 ? <span className={`usage-tps tps-${tpsTier(tps)}`} title="当前输出速率">{tps}/s</span>
                : lastTps > 0 ? <span className="usage-tps tps-paused" title="最近输出速率">{lastTps}/s</span>
                : null}
              <div className="provider-picker">
                {/* Mode: what this session may do. It sits with the model because the two
                    together describe the turn about to run. */}
                <select className="provider-select mode-select" value={mode}
                  title={MODE_META[mode]?.hint}
                  onChange={(e) => void changeMode(e.target.value)}>
                  {(['auto', 'chat', 'plan'] as const).map((m) =>
                    <option key={m} value={m}>{MODE_META[m].label}</option>)}
                </select>
                {modeNotice ? <span className="mode-notice" title={modeNotice}>⚠</span> : null}
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
                  await (AgentService as any).SetThinkingLevel?.(lv)
                  setThinkingLevel(lv)
                }}>
                  {THINKING_LEVELS.map((l) => <option key={l} value={l}>{l}</option>)}
                </select>
                <ContextMeter sessionId={currentId} estimate={ctxEstimate} window={ctxWindow} />
                <span className="usage-meta">
                  {hasCacheHit ? <CacheRing rate={cacheHitRate} /> : null}
                  {cost > 0 ? <span className="usage-cost" title="当前会话成本">¥{cost.toFixed(3)}</span> : null}
                  {credit > 0 ? <span className="usage-credit" title="当前会话积分">{fmtCredit(credit)} 积分</span> : null}
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
                      onClose={() => setMcpOpen(false)}
                      onToggleServer={toggleServer}
                      onToggleTool={toggleTool}
                      onToggleProfile={toggleProfile}
                    />
                  )}
                </span>
              </div>
            </div>
          </footer>
        </main>
        {oneoffOpen ? <OneOffPanel api={oneoff} workDir={workDir} busy={isCurrentRunning} width={oneoffWidth}
          onResizeCommit={commitOneOffWidth} onSend={sendFindings}
          onRerun={(turn, paths, msgId) => void startReview(turn, paths, msgId)} onClose={() => setOneOffOpen(false)} /> : null}
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
          <button className="ctx-item" role="menuitem" onClick={() => { void openChain(menu.sid, sessions.find((x) => x.id === menu.sid)?.title || '会话') }}>回退链…</button>
          <button className="ctx-item" role="menuitem" onClick={() => { AgentService.OpenSessionDir(menu.sid).catch(() => {}); setMenu(null) }}>打开会话目录</button>
          <button className="ctx-item" role="menuitem" onClick={() => { setEditingId(menu.sid); setEditTitle(sessions.find((x) => x.id === menu.sid)?.title || ''); setMenu(null) }}>重命名</button>
          <button className="ctx-item danger" role="menuitem" disabled={runningSet.has(menu.sid)}
            title={runningSet.has(menu.sid) ? '会话正在运行，请先停止这一轮' : undefined}
            onClick={() => { const t = sessions.find((x) => x.id === menu.sid)?.title || ''; setConfirmDel({ sid: menu.sid, title: t }); setMenu(null) }}>删除</button>
        </div>
      )}
      {confirmDel && (
        <div className="confirm-overlay" onClick={() => setConfirmDel(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
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
      {shortcutsOpen && (
        <div className="confirm-overlay" onClick={() => setShortcutsOpen(false)}>
          <div className="confirm-box shortcuts-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">快捷键</div>
            <div className="shortcut-section">全局</div>
            <div className="shortcut-row"><kbd>⌘ /</kbd><span>聚焦输入框</span></div>
            <div className="shortcut-row"><kbd>⌘ N</kbd><span>新建会话</span></div>
            <div className="shortcut-row"><kbd>⌘ ⇧ H</kbd><span>回退链（本会话的可回退点）</span></div>
            <div className="shortcut-row"><kbd>⌘ B</kbd><span>折叠 / 展开侧栏</span></div>
            <div className="shortcut-row"><kbd>⌘ ?</kbd><span>显示本快捷键列表</span></div>
            <div className="shortcut-section">旁路面板 / diff / 文件预览</div>
            <div className="shortcut-row"><kbd>⌘ F</kbd><span>在本页查找（Enter / Shift+Enter 上下，Esc 关闭）</span></div>
            <div className="shortcut-section">消息区（聚焦时）</div>
            <div className="shortcut-row"><kbd>G</kbd><span>跳到最新消息</span></div>
            <div className="shortcut-row"><kbd>gg</kbd><span>回到顶部</span></div>
            <div className="shortcut-row"><kbd>Ctrl U / D</kbd><span>上 / 下翻半页</span></div>
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
    </div>
  )
}

// humanize renders a token count as a compact human-friendly number, e.g.
// 128512 → "128.5K", 2000000 → "2M". Used for the context-ring tooltip.
// Token counts never reach the G scale (context windows max out around M), so
// only K/M are emitted; anything above M is shown verbatim as a fallback.
export default App
