import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Dialogs, Events } from '@wailsio/runtime'
import {
  AgentService,
  AgentStatus,
  type AgentState,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import {
  STATUS_META,
  THINKING_LEVELS,
  PAGE_SIZE,
  AT_MAX_RESULTS,
  AT_SEARCH_DEBOUNCE_MS,
  type AgentEvent,
  type AtMatch,
  type AtPickerState,
  type Message,
  type Part,
  type SessionItem,
} from './types'
import { buildTurns, fmtCredit, fmtDur, fmtTime, tpsTier, actOnKey, atRefAt, countAtRefs, insertRefText, replaceRefText } from './lib'
import {
  ContextMeter, CacheRing, ThinkingPart, NoticePart, UserBubble, CommandPicker, ToolCard, MCPPanel, AtFilePicker, AskForm,
  SettingsIcon, UsageIcon, MCPIcon, ThemeToggle, RootsPanel,
} from './components'
import { FileCard, fileFromSendFileArgs } from './filepreview'
import { DiffPanel } from './diff'
import { PlanChip, PlanPanel } from './plan'
import { MarkdownBlock } from './markdown'
import { useTheme, useThemeHostSync } from './theme'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import type { CommandVO, FileChangeVO, PlanVO, ReviewFindingsVO, SessionRootsVO, TurnDiffVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

// ── Ordered turn parts ──────────────────────────────────────────────────────
// A live turn accumulates the SAME ordered `parts` array a rebuilt transcript
// has (see buildTurns): deltas extend the trailing part of their own kind, a
// tool start appends a part, its result closes that part. Without this the
// live view sorted everything into fixed buckets (all tools, then all text),
// so a multi-step turn rendered in the wrong order until a restart rebuilt it.
function appendPartDelta(m: Message, type: 'thinking' | 'text', delta: string): Message {
  if (!delta) return m
  const parts = [...(m.parts || [])]
  const last = parts[parts.length - 1]
  if (last && last.type === type) parts[parts.length - 1] = { ...last, text: (last.text || '') + delta }
  else parts.push({ type, text: delta })
  return { ...m, parts }
}

function pushPart(m: Message, p: Part): Message {
  return { ...m, parts: [...(m.parts || []), p] }
}

// finishToolPart closes the newest still-running call of the named tool.
function finishToolPart(m: Message, name: string, summary: string, ok: boolean, durationMs?: number): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i]
    if (p.type === 'tool' && !p.done && p.name === name) {
      parts[i] = { ...p, summary, ok, done: true, durationMs }
      break
    }
  }
  return { ...m, parts }
}

// finishNotice completes the newest in-flight notice (auto-compaction pushes one
// when it starts and fills in its outcome here). A notice that never had a start
// — the window was opened while a compaction was already running — is appended
// as an already-finished one rather than dropped.
function finishNotice(m: Message, label: string, summary?: string): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    if (parts[i].type === 'notice' && parts[i].done === false) {
      parts[i] = { ...parts[i], label, summary, done: true }
      return { ...m, parts }
    }
  }
  return { ...m, parts: [...parts, { type: 'notice', label, summary, done: true }] }
}

// updateToolPart fills in the human-readable title/args (and the derived change) for
// the newest in-flight call, pushed by the separate agent:tool event. That event
// fires twice — once when the model starts the call (args still empty) and once with
// the complete arguments right before execution — so the second patch is what
// actually brings the diff in.
function updateToolPart(m: Message, name: string, title: string, args: string, change?: FileChangeVO | null): Message {
  const parts = [...(m.parts || [])]
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i]
    if (p.type === 'tool' && !p.done && p.name === name) {
      parts[i] = { ...p, title, args, change: change ?? p.change }
      break
    }
  }
  return { ...m, parts }
}

// setPartDiffs sets diffOpen on every diff-carrying part of ONE message: the turn
// footer's chip opens/closes the whole turn at once. value undefined = toggle each
// part on its own current state... which is why the chip passes an explicit value.
function setPartDiffs(m: Message, value: boolean): Message {
  return {
    ...m,
    parts: (m.parts || []).map((p) => (p.type === 'tool' && p.change ? { ...p, diffOpen: value } : p)),
  }
}

// togglePartDiff flips one card's diff (the per-card entry).
function togglePartDiff(m: Message, index: number): Message {
  return {
    ...m,
    parts: (m.parts || []).map((p, i) => (i === index && p.type === 'tool' ? { ...p, diffOpen: !p.diffOpen } : p)),
  }
}

// turnDiffStat aggregates a turn's changes for the footer chip: files deduped by path
// (the same file edited twice counts once), fragment line counts summed, and whether
// the turn ran shell commands at all — whose changes never show up in a diff derived
// from tool arguments.
function turnDiffStat(parts: Part[] | undefined): { files: number; added: number; removed: number; shell: boolean; paths: string[] } {
  const byPath = new Map<string, { added: number; removed: number }>()
  let shell = false
  for (const p of parts || []) {
    if (p.type !== 'tool') continue
    if (p.name === 'Bash') shell = true
    if (!p.change || !p.done || !p.ok) continue
    const cur = byPath.get(p.change.path) || { added: 0, removed: 0 }
    byPath.set(p.change.path, { added: cur.added + (p.change.added || 0), removed: cur.removed + (p.change.removed || 0) })
  }
  let added = 0
  let removed = 0
  for (const v of byPath.values()) {
    added += v.added
    removed += v.removed
  }
  return { files: byPath.size, added, removed, shell, paths: [...byPath.keys()] }
}

// closeOpenToolParts marks every unfinished call as aborted — used when a turn
// is stopped/errors and no tool_result will ever arrive.
function closeOpenToolParts(parts: Part[] | undefined, summary: string): Part[] {
  return (parts || []).map((p) => (p.type === 'tool' && !p.done ? { ...p, ok: false, done: true, summary } : p))
}

function lastRunningAssistantIndex(list: Message[]): number {
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].role === 'assistant' && list[i].running) return i
  }
  return -1
}

// imeActive reports whether a key event belongs to an IME composition (Chinese
// / Japanese / Korean candidate selection). Enter while composing confirms a
// candidate — treating it as "submit" is the classic IME bug. keyCode 229 is
// the fallback some WebKit builds report instead of setting isComposing.
function imeActive(e: { nativeEvent?: { isComposing?: boolean }; keyCode?: number }): boolean {
  return !!e.nativeEvent?.isComposing || e.keyCode === 229
}

// ── Memoized transcript pieces ──────────────────────────────────────────────
// Streaming replaces only the message being written (its siblings keep the
// same object references), so memoization here means every earlier turn — and
// its already-parsed markdown — is skipped on each animation frame. Without
// this, a long session re-parsed the whole transcript dozens of times per
// second, which is what made output feel jumpy.

// The markdown itself is rendered by MarkdownBlock and the attachment cards by
// FileCard (see markdown.tsx / filepreview.tsx): this file only decides which
// piece a turn part turns into.

const TurnPart = memo(function TurnPart({ part, workDir, onToggleDiff }: { part: Part; workDir: string; onToggleDiff?: () => void }) {
  if (part.type === 'thinking') return <ThinkingPart text={part.text || ''} />
  if (part.type === 'notice') return <NoticePart part={part} />
  if (part.type === 'tool') {
    // A SendFile call IS the attachment — show the file card rather than a raw
    // tool card (covers the live turn and reloaded history alike). workDir
    // resolves the relative paths a model sometimes writes.
    if (part.name === 'SendFile') {
      const file = fileFromSendFileArgs(part.args || '')
      if (file) return <FileCard file={file} workDir={workDir} />
    }
    return <ToolCard name={part.name || ''} title={part.title} args={part.args} summary={part.summary || ''} ok={!!part.ok}
      done={part.done} change={part.change} diffOpen={part.diffOpen} durationMs={part.durationMs}
      defaultExpanded={part.expand} onToggleDiff={onToggleDiff} />
  }
  return <MarkdownBlock text={part.text || ''} workDir={workDir} />
})

const AssistantBubble = memo(function AssistantBubble({ m, workDir, runningLabel, ask, onAnswer, onToggleDiff, onToggleAllDiffs, onOpenDiffPanel, onReviewChanges, reviewPending, reviewNotice, sessionBusy }: {
  m: Message
  workDir: string
  // Diff interaction: one card at a time, or the whole turn from the footer chip.
  onToggleDiff?: (partIndex: number) => void
  onToggleAllDiffs?: (value: boolean) => void
  // Opens the working-tree diff panel for this turn's files (git-backed, real line
  // numbers) — the authoritative view behind the fragment diffs.
  onOpenDiffPanel?: (paths: string[]) => void
  // The turn-level review: one click, scoped to exactly this turn's files.
  // The clicked turn's message id travels with the request, so the run's state can be shown
  // on the very footer that started it (and on no other).
  onReviewChanges?: (paths: string[], msgId?: string) => void
  reviewPending?: boolean
  reviewNotice?: string
  // The session is mid-turn, so a review has to wait its turn.
  sessionBusy?: boolean
  // Only passed while the turn is running, so finished messages keep a stable
  // props shape and stay memoized.
  runningLabel?: string
  // Pending AskUserQuestion questions for THIS session: the form replaces the
  // tool card that asked them, so they appear in the transcript where they belong.
  ask?: Question[] | null
  onAnswer?: (answers: Record<string, string> | null) => void
}) {
  // Render the form in place of the pending AskUserQuestion card. Any other
  // unfinished card of the same tool is left alone; the loop only ever asks one
  // question set at a time, so the first match is the one waiting.
  // The turn's changes: what the footer chip summarizes and what "expand all" acts on.
  const diffStat = turnDiffStat(m.parts)
  const diffParts = (m.parts || []).filter((p) => p.type === 'tool' && p.change && p.done && p.ok)
  const allDiffsOpen = diffParts.length > 0 && diffParts.every((p) => p.diffOpen)

  let askShown = false
  const parts = (m.parts || []).map((p, i) => {
    if (ask && onAnswer && !askShown && p.type === 'tool' && !p.done && p.name === 'AskUserQuestion') {
      askShown = true
      return <AskForm key={i} questions={ask} onSubmit={onAnswer} onCancel={() => onAnswer(null)} />
    }
    return <TurnPart key={i} part={p} workDir={workDir} onToggleDiff={onToggleDiff ? () => onToggleDiff(i) : undefined} />
  })
  return (
    <div className="msg msg-assistant">
      <div className="msg-avatar"><img src="/agent-avatar.png" alt="" draggable={false} /></div>
      <div className="msg-content">
        <div className="turn-parts">{parts}</div>
        {/* Fallback: a pending ask with no matching tool card (e.g. the card was
            closed by an interruption) still has to be answerable. */}
        {ask && onAnswer && !askShown && m.running ? (
          <AskForm questions={ask} onSubmit={onAnswer} onCancel={() => onAnswer(null)} />
        ) : null}
        {m.running ? <span className="running"><span className="typing"><i></i><i></i><i></i></span>{runningLabel ?? '正在执行…'}</span> : null}
        {!m.running && m.stopped ? <span className="stopped-note"><span className="stop-square">⏹</span> 已停止</span> : null}
        {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
        {diffStat.files > 0 ? (
          <div className="msg-footer">
            <button type="button" className="diff-chip"
              title={`本次工具调用在片段内新增/删除的行数（不是 git numstat）${diffStat.shell ? '；本轮还跑了 shell 命令，那些改动不会出现在 diff 里' : ''}`}
              onClick={() => onToggleAllDiffs?.(!allDiffsOpen)}>
              🧾 {diffStat.files} files
              {diffStat.added > 0 ? <span className="diff-count is-add">+{diffStat.added}</span> : null}
              {diffStat.removed > 0 ? <span className="diff-count is-del">−{diffStat.removed}</span> : null}
              {diffStat.shell ? <span className="diff-chip-shell">· 含 shell</span> : null}
            </button>
            {/* The second view: the same changes against git HEAD, with real file
                line numbers. The chip above stays the light touch (it toggles the
                inline fragment diffs); this one is the full picture. */}
            <button type="button" className="diff-chip" title="与 git HEAD 对照的完整 diff（真实文件行号）"
              onClick={() => onOpenDiffPanel?.(diffStat.paths)}>完整 diff</button>
            {/* The review entry: ONE per turn, never per edit card — the review's scope
                is this turn's file set, and findings carry real file lines that a
                fragment card has no coordinates for. */}
            <button type="button" className="diff-chip"
              disabled={reviewPending || sessionBusy}
              title={reviewPending ? '评审进行中…' : sessionBusy ? '等这一轮跑完' : '让 agent 只评审本轮改动的这些文件（只读；意见会落在 diff 面板里）'}
              onClick={() => onReviewChanges?.(diffStat.paths, m.id)}>
              {reviewPending ? '评审中…' : '评审本轮改动'}
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
  const [confirmDel, setConfirmDel] = useState<{ sid: string; title: string } | null>(null)
  const [shortcutsOpen, setShortcutsOpen] = useState(false)
  const [reminderModal, setReminderModal] = useState<string | null>(null)
  const [editingId, setEditingId] = useState('')
  const [editTitle, setEditTitle] = useState('')
  const [, setCurrentTitle] = useState<string>('Tachi')
  const [msgCache, setMsgCache] = useState<Record<string, Message[]>>({})
  const [runningSet, setRunningSet] = useState<Set<string>>(new Set())
  const [state, setState] = useState<AgentState>({ status: AgentStatus.StatusIdle, label: '空闲', detail: '就绪' })
  const [input, setInput] = useState('')
  const [loading, setLoading] = useState(false)
  const [providers, setProviders] = useState<any[]>([])
  const [providerName, setProviderName] = useState('')
  const [thinkingLevel, setThinkingLevel] = useState('none')
  const [ctxEstimate, setCtxEstimate] = useState(0)
  const [ctxWindow, setCtxWindow] = useState(0)
  const [cost, setCost] = useState(0)
  const [credit, setCredit] = useState(0)
  const [cacheHitRate, setCacheHitRate] = useState(0)
  const [hasCacheHit, setHasCacheHit] = useState(false)
  const [workDir, setWorkDir] = useState('')
  // Workspace roots (primary + additional) and the popover that manages them. The
  // popover is only mounted while open, so its own outside-click/Esc handling is
  // not running behind the scenes.
  const [roots, setRoots] = useState<SessionRootsVO | null>(null)
  const [rootsOpen, setRootsOpen] = useState(false)
  const [rootsBusy, setRootsBusy] = useState(false)
  const [rootsError, setRootsError] = useState('')
  // Working-tree diff panel (P2): opened from a turn's footer, fetched on demand.
  const [diffPanelOpen, setDiffPanelOpen] = useState(false)
  const [diffPanelData, setDiffPanelData] = useState<TurnDiffVO | null>(null)
  const [diffPanelLoading, setDiffPanelLoading] = useState(false)
  // The panel belongs to the session it was opened for: its diff, its findings, and the
  // paths behind 预览/打开 all come from there. Switching sessions closes it rather than
  // leaving another session's changes on screen — and it is also what keeps 发给 agent
  // (P3) from sending into a session the findings never came from.
  useEffect(() => { setDiffPanelOpen(false) }, [currentId])

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
  const [reviewNotice, setReviewNotice] = useState<{ msgId: string; text: string } | null>(null)
  // The chip's "+N" badge: the chip itself shows the primary path, so this is what
  // says the workspace extends beyond it.
  const extraRootCount = (roots?.additional || []).length
  const [tps, setTps] = useState(0)
  const [lastTps, setLastTps] = useState(0)
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false)
  // Theme (light/dark) for the titlebar switch; useThemeHostSync mirrors the
  // active theme to Go, which owns the window colour outside the webview.
  const [theme, toggleTheme] = useTheme()
  useThemeHostSync()
  const [mcpOpen, setMcpOpen] = useState(false)
  const [mcpServers, setMcpServers] = useState<any[]>([])
  const [mcpLoading, setMcpLoading] = useState<Record<string, boolean>>({})
  const [mcpProfile, setMcpProfile] = useState<{ active: string; available: string[] }>({ active: '', available: [] })
  // Per-session message pagination: how much of the session history is loaded
  // on the client, and the oldest loaded raw-message timestamp (used as the
  // "load earlier" cursor).
  const [hasMore, setHasMore] = useState<Record<string, boolean>>({})
  const [earliestTs, setEarliestTs] = useState<Record<string, string>>({})
  // Pending queue: messages the user sends while a turn is still running. They
  // are NOT executed immediately — they sit here (each removable, the whole
  // queue clearable) until the next steer point injects them into the running
  // turn, or the turn ends and a natural completion auto-sends them. Keyed by
  // session so queued input follows its own conversation.
  const [pending, setPending] = useState<Record<string, string[]>>({})
  const pendingRef = useRef<Record<string, string[]>>({})
  // sendingNow guards the "立即发送" action against double-clicks while the
  // backend is still stopping the previous turn.
  const [sendingNow, setSendingNow] = useState(false)
  // Auto-follow state: true while the view is parked at the bottom. Scrolling
  // up flips it off (and surfaces the "jump to latest" button) so incoming
  // streaming output no longer yanks the transcript back down under the user.
  const followBottomRef = useRef(true)
  const [showJump, setShowJump] = useState(false)
  // IME composition state for the composer. Some WebKit builds fire
  // compositionend BEFORE the Enter keydown that commits the candidate, so the
  // ref is cleared on the next macrotask — that keeps the guard active for the
  // committing Enter without swallowing a later, genuine Enter-to-send.
  const composingRef = useRef(false)
  const chatRef = useRef<HTMLDivElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)
  const messages = msgCache[currentId] || []
  const pendMsgs = pending[currentId] || []

  // ── @-file completion ─────────────────────────────────────────────────────
  // Typing "@" opens a fuzzy picker over the session's working directory.
  // Accepting a match splices a reference into the text; the reference is
  // expanded backend-side at turn start (agent/atfile), so the transcript keeps
  // showing the raw text the user typed. The trigger rule in atRefAt mirrors
  // agent/atfile.IsRefBoundary — the popup must offer exactly what the backend
  // will expand.
  const [at, setAt] = useState<AtPickerState | null>(null)
  // Slash commands the backend supports (fetched once: the set is static per
  // build) and the "/" palette's highlight. The palette is derived from the
  // input below rather than stored, so there is no way for it to go stale.
  const [cmdList, setCmdList] = useState<CommandVO[]>([])
  const [cmdDismissed, setCmdDismissed] = useState<string | null>(null)
  const [cmdIdx, setCmdIdx] = useState(0)
  // Query of the last scheduled search — null (not "") means "none yet": the
  // empty query is a real query (it lists the working directory), so "" cannot
  // double as the sentinel or the very first "@" would never be searched.
  const atQueryRef = useRef<string | null>(null)
  const atSeqRef = useRef(0)                     // stale-response guard
  const atTimerRef = useRef<number | null>(null)
  // inputRef mirrors the composer text for event listeners (Wails file drops)
  // that must not re-subscribe on every keystroke.
  const inputRef = useRef(input)
  useEffect(() => { inputRef.current = input }, [input])
  // atStateRef mirrors the picker state for the same reason (see the file-drop
  // listener: a drop with the picker open must replace the reference being
  // typed, not append after it).
  const atStateRef = useRef<AtPickerState | null>(null)
  useEffect(() => { atStateRef.current = at }, [at])

  const closeAt = useCallback(() => {
    atQueryRef.current = null
    atSeqRef.current++
    if (atTimerRef.current !== null) {
      window.clearTimeout(atTimerRef.current)
      atTimerRef.current = null
    }
    setAt(null)
  }, [])

  // scheduleAtSearch runs the (debounced) backend search for a query.
  const scheduleAtSearch = useCallback((query: string) => {
    const sid = currentId
    atQueryRef.current = query
    if (atTimerRef.current !== null) window.clearTimeout(atTimerRef.current)
    const seq = ++atSeqRef.current
    atTimerRef.current = window.setTimeout(() => {
      atTimerRef.current = null
      // Clear the spinner on every path — a rejected (or even synchronously
      // throwing) call must not leave the picker stuck on "搜索中…".
      const applyResult = (items: AtMatch[]) => {
        if (seq !== atSeqRef.current) return // superseded by a newer query
        setAt((p) => (p && p.query === query ? { ...p, items, idx: 0, loading: false } : p))
      }
      try {
        AgentService.SearchFiles(sid, query, AT_MAX_RESULTS)
          .then((res) => applyResult(res || []))
          .catch(() => applyResult([]))
      } catch {
        applyResult([])
      }
    }, AT_SEARCH_DEBOUNCE_MS)
  }, [currentId])

  // syncAtRef recomputes the picker from the composer's value + caret. Called on
  // every text/selection change, which is also how it closes: no reference under
  // the caret means no picker.
  const syncAtRef = useCallback((value: string, caret: number) => {
    const ref = atRefAt(value, caret)
    if (!ref) {
      closeAt()
      return
    }
    const count = countAtRefs(value)
    setAt((prev) => (prev && prev.start === ref.start && prev.query === ref.query
      ? { ...prev, count }
      : { start: ref.start, query: ref.query, items: [], idx: 0, loading: true, count }))
    if (atQueryRef.current !== ref.query) scheduleAtSearch(ref.query)
  }, [closeAt, scheduleAtSearch])

  // insertAtCaret splices text into the composer at the caret, keeping the focus
  // and the caret after the inserted text.
  const insertAtCaret = useCallback((text: string, trailingSpace: boolean, resync: boolean) => {
    const caret = composerRef.current?.selectionStart ?? inputRef.current.length
    const ins = insertRefText(inputRef.current, caret, text, trailingSpace)
    setInput(ins.value)
    requestAnimationFrame(() => {
      const node = composerRef.current
      if (!node) return
      node.focus()
      node.setSelectionRange(ins.caret, ins.caret)
      if (resync) syncAtRef(ins.value, ins.caret)
    })
  }, [syncAtRef])

  // acceptAt REPLACES the reference being typed with the picked path — it must
  // not splice at the caret, or the "@query" the user was typing survives and
  // the text ends up with a stray "@" (counted as a second reference). A
  // directory keeps the picker open on the new prefix so the user can drill in.
  const acceptAt = useCallback((match?: AtMatch) => {
    if (!match || !at) return
    const dir = !!match.isDir
    // The backend decides the reference form (relative under the primary root,
    // absolute under an additional one); Path is only what the row displays.
    const text = (match.ref || '@' + match.path) + (dir ? '/' : ' ')
    const ins = replaceRefText(inputRef.current, at.start, text)
    atQueryRef.current = null // whatever follows is a fresh query
    setAt(null)
    setInput(ins.value)
    requestAnimationFrame(() => {
      const node = composerRef.current
      if (!node) return
      node.focus()
      node.setSelectionRange(ins.caret, ins.caret)
      if (dir) syncAtRef(ins.value, ins.caret)
    })
  }, [at, syncAtRef])

  // A session switch changes the working directory, so any open picker is stale.
  useEffect(() => { closeAt() }, [currentId, closeAt])

  // ── AskUserQuestion ───────────────────────────────────────────────────────
  // The agent parks the turn and waits for answers; the questions arrive as an
  // event and are answered through AgentService.AnswerQuestion (the TUI answers
  // the same channel via RespondToAskUser). Pending questions are kept per
  // session so a background session's question never hijacks the foreground UI.
  const [asks, setAsks] = useState<Record<string, { toolId: string; questions: Question[] }>>({})
  const clearAsk = useCallback((sid: string) => {
    setAsks((prev) => {
      if (!prev[sid]) return prev
      const next = { ...prev }
      delete next[sid]
      return next
    })
  }, [])

  // answerQuestion answers (or declines, with null) the pending AskUserQuestion.
  const answerQuestion = useCallback((sid: string, answers: Record<string, string> | null) => {
    clearAsk(sid)
    // nil answers = the user declined; the model is told the question went
    // unanswered instead of being handed a made-up choice.
    AgentService.AnswerQuestion(sid, answers, null).catch(() => {})
  }, [clearAsk])

  // answerCurrent is the stable callback the transcript form uses for the
  // session on screen.
  const answerCurrent = useCallback((a: Record<string, string> | null) => answerQuestion(currentId, a), [currentId, answerQuestion])

  // patchMessage applies a transform to ONE message of the current session (by id):
  // the shape a transcript-local interaction needs, so the message map stays the
  // single source of truth for what is expanded.
  const patchMessage = useCallback((msgId: string, fn: (m: Message) => Message) => {
    setMsgCache((prev) => {
      const list = prev[currentId]
      if (!list) return prev
      return { ...prev, [currentId]: list.map((m) => (m.id === msgId ? fn(m) : m)) }
    })
  }, [currentId])

  // Note: pending questions are NOT cleared on session switch — the agent is
  // still parked, so switching back must show the same form. They are cleared
  // when the turn ends (see the agent:idle / agent:error listeners below).

  // Native file drops arrive from Go (the webview cannot read dropped paths):
  // resolve them into @-references and splice them in at the caret.
  useEffect(() => {
    const off = Events.On('agent:filedrop', (event) => {
      const d = event.data as { paths?: string[] } | undefined
      if (!d?.paths?.length) return
      const paths = d.paths
      ;(async () => {
        try {
          const files = (await AgentService.ResolveDroppedPaths(currentId, paths)) || []
          const refs = files.map((f) => f.ref).filter(Boolean) as string[]
          if (!refs.length) return
          const batch = refs.join(' ')
          const open = atStateRef.current
          closeAt()
          if (open) {
            // Drop while the picker is open: replace the "@query" being typed.
            const ins = replaceRefText(inputRef.current, open.start, batch + ' ')
            setInput(ins.value)
            requestAnimationFrame(() => {
              const node = composerRef.current
              node?.focus()
              node?.setSelectionRange(ins.caret, ins.caret)
            })
            return
          }
          insertAtCaret(batch, true, false)
        } catch { /* ignore */ }
      })()
    })
    return () => off?.()
  }, [currentId, closeAt, insertAtCaret])

  const scrollToBottom = useCallback((force = false) => {
    // force=true is for actions where following is clearly intended (sending a
    // message, switching sessions). Otherwise the pin only happens while the
    // user is parked at the bottom.
    if (force) {
      followBottomRef.current = true
      setShowJump(false)
    }
    const el = chatRef.current
    if (el && followBottomRef.current) el.scrollTop = el.scrollHeight
  }, [])

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
  // Token deltas arrive far faster than the display refreshes. Batching them
  // into a single state update per animation frame caps markdown parsing and
  // layout at ~60/s instead of once per token. Non-delta events flush the
  // buffer first, so the transcript order stays exactly as it happened.
  const deltaQueueRef = useRef<{ sid: string; type: 'thinking' | 'text'; delta: string }[]>([])
  const deltaRafRef = useRef<number | null>(null)
  const flushDeltas = useCallback(() => {
    if (deltaRafRef.current !== null) {
      cancelAnimationFrame(deltaRafRef.current)
      deltaRafRef.current = null
    }
    const q = deltaQueueRef.current
    if (!q.length) return
    deltaQueueRef.current = []
    setMsgCache((prev) => {
      // Group per session (order preserved within a session) so background
      // sessions batched in the same frame don't clobber each other.
      const groups = new Map<string, { type: 'thinking' | 'text'; delta: string }[]>()
      for (const it of q) {
        const g = groups.get(it.sid)
        if (g) g.push(it)
        else groups.set(it.sid, [it])
      }
      let out = prev
      for (const [sid, items] of groups) {
        let list = prev[sid] || []
        let idx = lastRunningAssistantIndex(list)
        if (idx < 0) {
          // Same reason as applyToSession's openIfMissing: deltas can arrive before the
          // frontend has placed the placeholder for the turn producing them.
          const ts = Date.now()
          list = [...list, { id: `a-${ts}-open`, role: 'assistant', running: true, parts: [], ts: new Date(ts).toISOString() }]
          prev = { ...prev, [sid]: list }
          out = prev
          idx = list.length - 1
        }
        let cur = list[idx]
        for (const it of items) cur = appendPartDelta(cur, it.type, it.delta)
        if (cur === list[idx]) continue
        const next = [...list]
        next[idx] = cur
        if (out === prev) out = { ...prev }
        out[sid] = next
      }
      return out
    })
  }, [])
  const enqueueDelta = useCallback((sid: string, type: 'thinking' | 'text', delta: string) => {
    if (!delta) return
    deltaQueueRef.current.push({ sid, type, delta })
    if (deltaRafRef.current === null) deltaRafRef.current = requestAnimationFrame(flushDeltas)
  }, [flushDeltas])

  // Pin to the newest content after every committed update while following —
  // doing it in a layout effect means the scroll happens in the same frame the
  // content grows, so the view glides instead of lurching.
  useLayoutEffect(() => {
    const el = chatRef.current
    if (el && followBottomRef.current) el.scrollTop = el.scrollHeight
  }, [msgCache])

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
            setHasMore((p) => ({ ...p, [currentId]: false }))
            return
          }
          const more = buildTurns(page.messages)
          setMsgCache((prev) => ({ ...prev, [currentId]: [...more, ...(prev[currentId] || [])] }))
          setEarliestTs((p) => ({ ...p, [currentId]: page.messages[0].timestamp }))
          setHasMore((p) => ({ ...p, [currentId]: !!page.hasMore }))
        } catch { /* ignore */ }
        requestAnimationFrame(() => requestAnimationFrame(() => {
          const c = chatRef.current
          if (c) c.scrollTop = c.scrollHeight - old
          loadMoreRef.current = false
        }))
      })()
    }
  }

  const refreshRunning = useCallback(async () => {
    const list = (await AgentService.RunningSessions().catch(() => null)) || []
    setRunningSet(new Set(list))
  }, [])
  const setSessionMsgs = useCallback((id: string, fn: (l: Message[]) => Message[]) => {
    setMsgCache((prev) => ({ ...prev, [id]: fn(prev[id] || []) }))
  }, [])

  // ── Pending queue / steer ─────────────────────────────────────────────────
  // Queue ops keep pendingRef in sync so event listeners (which can only see
  // the latest values through refs, not stale render closures) always act on
  // the freshest queue.
  const enqueuePending = useCallback((sid: string, text: string) => {
    const next = { ...pendingRef.current, [sid]: [...(pendingRef.current[sid] || []), text] }
    pendingRef.current = next
    setPending(next)
  }, [])
  const dropPending = useCallback((sid: string, idx: number) => {
    const list = pendingRef.current[sid] || []
    const next = { ...pendingRef.current, [sid]: list.filter((_, i) => i !== idx) }
    pendingRef.current = next
    setPending(next)
  }, [])
  const clearPending = useCallback((sid: string) => {
    if (!(pendingRef.current[sid] || []).length) return
    const next = { ...pendingRef.current, [sid]: [] }
    pendingRef.current = next
    setPending(next)
  }, [])
  // takePending joins and clears the queue, returning the combined text ("" if
  // empty) — the single drain point for steer injection / send-now / auto-flush.
  const takePending = useCallback((sid: string) => {
    const text = (pendingRef.current[sid] || []).join('\n\n')
    if (text) {
      const next = { ...pendingRef.current, [sid]: [] }
      pendingRef.current = next
      setPending(next)
    }
    return text
  }, [])

  // The current session is producing output when its run is in-flight, or the
  // status bar is in a busy state (covers the simulated-turn fallback too).
  const isCurrentRunning = runningSet.has(currentId) ||
    state.status === 'thinking' || state.status === 'tool_running' || state.status === 'busy'

  // sendText appends the user message + a running assistant placeholder and
  // starts a backend turn. Shared by the composer, the queue's "send now" and
  // the auto-flush after a naturally completed turn.
  const sendText = useCallback((raw: string) => {
    const text = raw.trim()
    if (!text) return
    const sid = currentId
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    setSessionMsgs(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: tsStr },
      { id: `a-${ts}`, role: 'assistant', running: true, parts: [], ts: tsStr },
    ])
    AgentService.SendMessage(text).catch(() => {})
    setRunningSet((prev) => new Set(prev).add(sid))
    scrollToBottom(true)
  }, [currentId, setSessionMsgs, scrollToBottom])

  // sealRunningSegment finalizes the turn's currently-streaming assistant segment.
  //
  // An interrupted turn's terminal event (error / ExitReason=cancelled) is applied to the
  // newest RUNNING assistant — so if the next message's placeholder is already in the
  // transcript when it arrives, the flag lands on the wrong message. That is exactly what
  // "立即发送" used to produce: the brand-new placeholder read 已停止, and the reply that
  // followed had no running message to attach to and was dropped. Sealing first leaves the
  // incoming event with nothing to hit; a segment that rendered nothing is removed rather
  // than left as an empty bubble.
  const sealRunningSegment = useCallback((sid: string) => {
    setMsgCache((prev) => {
      const list = prev[sid]
      if (!list) return prev
      const back = [...list].reverse().findIndex((m) => m.role === 'assistant' && m.running)
      if (back < 0) return prev
      const idx = list.length - 1 - back
      const seg = list[idx]
      const out = [...list]
      if (!(seg.parts && seg.parts.length)) out.splice(idx, 1)
      else out[idx] = { ...seg, running: false, stopped: true }
      return { ...prev, [sid]: out }
    })
  }, [])

  // applyToSession patches the newest message of the given role in a session.
  // Every streaming handler funnels through it (text/tool deltas and the
  // auto-compaction notices), so a live transcript is only ever mutated in one
  // place — and it keys purely off the session ID the backend sent, never off
  // currentId, so a background session still updates correctly.
  // applyToSession patches the newest message of the given role in a session.
  //
  // openIfMissing is for STREAM events (a delta, a tool call): if the session has no running
  // assistant, they open one. A turn can start before the frontend has placed its
  // placeholder — exactly what "[立即发送]" does, because the backend begins the next turn
  // while the stop call is still on its way back — and without this every delta of that turn
  // was dropped on the floor ("the reply never appeared"). Terminal events (turn_complete,
  // error) deliberately do NOT open one: they exist to close a turn, so creating a message
  // for them would turn an interrupted turn into a phantom empty bubble.
  const applyToSession = useCallback((sid: string, role: 'user' | 'assistant', fn: (m: Message) => Message, openIfMissing = false) => {
    setMsgCache((prev) => {
      const list = prev[sid] || []
      const target = role === 'user'
        ? [...list].reverse().find((m) => m.role === 'user')
        : [...list].reverse().find((m) => m.role === 'assistant' && m.running)
      if (!target) {
        if (!openIfMissing || role !== 'assistant') return prev
        const ts = Date.now()
        const fresh: Message = { id: `a-${ts}-open`, role: 'assistant', running: true, parts: [], ts: new Date(ts).toISOString() }
        return { ...prev, [sid]: [...list, fn(fresh)] }
      }
      return { ...prev, [sid]: list.map((m) => (m.id === target.id ? fn(m) : m)) }
    })
  }, [])

  // runCommand sends a slash command. It renders exactly like a message — user
  // bubble plus a running assistant placeholder, so the command's streamed output
  // has somewhere to land — but the backend dispatches it instead of starting a
  // chat turn. A non-empty result means the command never ran (unknown, or the
  // session is busy): that is shown as a notice rather than an empty reply.
  const runCommand = useCallback((text: string) => {
    const sid = currentId
    const ts = Date.now()
    const tsStr = new Date().toISOString()
    setSessionMsgs(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: tsStr },
      { id: `a-${ts}`, role: 'assistant', running: true, parts: [], ts: tsStr },
    ])
    setRunningSet((prev) => new Set(prev).add(sid))
    scrollToBottom(true)
    AgentService.RunCommand(text).then((refusal) => {
      if (!refusal) return
      applyToSession(sid, 'assistant', (m) => ({ ...finishNotice(m, refusal), running: false }))
      setRunningSet((prev) => { const next = new Set(prev); next.delete(sid); return next })
    }).catch(() => {
      applyToSession(sid, 'assistant', (m) => ({ ...finishNotice(m, '命令执行失败'), running: false }))
      setRunningSet((prev) => { const next = new Set(prev); next.delete(sid); return next })
    })
  }, [currentId, setSessionMsgs, scrollToBottom, applyToSession])

  // acceptCommand completes the palette's highlighted name into the input,
  // leaving a trailing space so arguments follow naturally. Completing rather
  // than sending is deliberate: "/rev" + Enter should not run the wrong command.
  const acceptCommand = useCallback((cmd?: CommandVO) => {
    if (!cmd) return
    setCmdIdx(0)
    setInput('/' + cmd.name + ' ')
    requestAnimationFrame(() => composerRef.current?.focus())
  }, [])

  // The "/" palette: open while the input is a bare command name still being
  // typed (a space means arguments follow, an exact name means the command is
  // complete). Derived from the input rather than stored, so it can never
  // disagree with what would actually be dispatched; Esc dismisses the palette
  // for the current query only, so typing on reopens it.
  const cmdQuery = input.startsWith('/') && !/\s/.test(input.slice(1)) ? input.slice(1) : null
  const cmdMatches = cmdQuery === null ? [] : cmdList.filter((c) => c.name.startsWith(cmdQuery.toLowerCase()))
  const cmdOpen = cmdQuery !== null && cmdDismissed !== cmdQuery && !cmdList.some((c) => c.name === cmdQuery)

  // send routes the composer text: while a turn is running the message goes to
  // the pending queue (to be steered in at the next tool boundary) instead of
  // starting a second, competing turn.
  const send = useCallback(() => {
    const text = input.trim()
    if (!text) return
    closeAt()
    setInput('')
    // A leading "/" is a command, never a message: the backend owns the list and
    // answers with a notice when it does not know the name.
    if (text.startsWith('/')) {
      runCommand(text)
      return
    }
    if (isCurrentRunning) {
      enqueuePending(currentId, text)
      return
    }
    sendText(text)
  }, [input, isCurrentRunning, currentId, enqueuePending, sendText, closeAt, runCommand])

  // sendPendingNow is the pending bar's primary action: stop the current turn
  // and send the queued text as a fresh user turn right away. The user bubble
  // + assistant placeholder are shown immediately; if stopping the turn times
  // out on the backend the text is put back into the queue for another try.
  // sendPendingNow is the queue's "[立即发送]": interrupt the running turn and send the
  // queued text as the next one.
  //
  // Two things have to be true at once, and the ORDER is what makes them so:
  //   1. the interrupted turn's segment is sealed BEFORE the new message is placed — its
  //      terminal event (error / interrupted) targets "the newest running assistant", so a
  //      message placed first would be the one marked 已停止 (and its own reply dropped);
  //   2. the assistant placeholder is NOT placed here at all — the stream events open it
  //      (see applyToSession's openIfMissing), because the backend starts the next turn while
  //      the stop call is still on its way back, so its first delta can beat this function.
  // Only the user's bubble goes in now: it has to stay above the reply.
  const sendPendingNow = useCallback(async () => {
    const sid = currentId
    const text = takePending(sid)
    if (!text.trim()) return
    setSendingNow(true)

    if (!isCurrentRunning) {
      // Nothing to interrupt: the plain send path places both bubbles and owns the IPC.
      sendText(text)
      setSendingNow(false)
      return
    }

    sealRunningSegment(sid)
    const ts = Date.now()
    setSessionMsgs(sid, (prev) => [...prev,
      { id: `u-${ts}`, role: 'user', text, ts: new Date(ts).toISOString() },
    ])
    setRunningSet((prev) => new Set(prev).add(sid))
    scrollToBottom(true)

    const requeue = () => {
      // Nothing was sent, so the segment we just opened must be closed again — and the text
      // belongs back in the queue rather than in the transcript as if it had gone out.
      sealRunningSegment(sid)
      enqueuePending(sid, text)
      refreshRunning()
    }
    try {
      const ret = await AgentService.StopAndSend(text)
      if (ret && ret !== 'ok') requeue()
    } catch {
      requeue()
    }
    setSendingNow(false)
  }, [currentId, isCurrentRunning, takePending, setSessionMsgs, enqueuePending, scrollToBottom,
      sealRunningSegment, refreshRunning, sendText])

  // injectSteerVisual splits the streaming assistant segment so the queued
  // user text lands AFTER the tool work already shown and BEFORE the reply
  // that continues after the steer point: finalize the running segment, append
  // the user bubble, open a fresh running placeholder for the rest of the turn
  // (subsequent deltas/tool events target the newest running assistant). When
  // the segment has rendered nothing yet the bubble is inserted before it.
  const injectSteerVisual = useCallback((sid: string, text: string, ts: string) => {
    setMsgCache((prev) => {
      const list = prev[sid] || []
      let ai = -1
      for (let i = list.length - 1; i >= 0; i--) {
        if (list[i].role === 'assistant' && list[i].running) { ai = i; break }
      }
      if (ai < 0) return prev
      const seg = list[ai]
      const u: Message = { id: `u-${Date.now()}-steer`, role: 'user', text, ts }
      if (!(seg.parts && seg.parts.length)) {
        const out = [...list]
        out.splice(ai, 0, u)
        return { ...prev, [sid]: out }
      }
      const out = [...list]
      out[ai] = { ...seg, running: false }
      out.push(u, { id: `a-${Date.now()}-steer`, role: 'assistant', running: true, parts: [], ts })
      return { ...prev, [sid]: out }
    })
    // Respect the follow state here too: a steered message landing mid-history
    // must not yank a user who is reading older output back to the bottom.
    scrollToBottom()
  }, [scrollToBottom])

  // answerSteer replies to the agent's steer_check for a session: queued text
  // is drained, shown in the transcript and injected; otherwise the empty
  // string unblocks the agent loop without steering.
  const answerSteer = useCallback((sid: string) => {
    const text = takePending(sid)
    if (!text) {
      AgentService.Steer(sid, '').catch(() => {})
      return
    }
    injectSteerVisual(sid, text, new Date().toISOString())
    AgentService.Steer(sid, text).catch(() => {})
  }, [takePending, injectSteerVisual])

  const refreshProvider = useCallback(async () => {
    try {
      const info = await (AgentService as any).GetProviderInfo?.()
      if (info) { setProviderName(info.provider); setCtxEstimate(info.contextEstimate || 0); setCtxWindow(info.contextWindow || 0) }
      const lv = await (AgentService as any).GetThinkingLevel?.()
      if (lv) setThinkingLevel(lv)
    } catch { /* ignore */ }
  }, [])

  // refreshCost fetches the current session's cumulative cost/credit ("积分")
  // from the backend's usage ledger (counterpart to TUI's statusbar cost).
  const refreshCost = useCallback(async (id: string) => {
    try {
      const u = await (AgentService as any).GetSessionUsage?.(id)
      // Applied unconditionally: a missing payload means "nothing recorded", and guarding it
      // with `if (u)` is what let one session's numbers survive into the next.
      setCost(u?.cost || 0); setCredit(u?.credit || 0)
      setCacheHitRate(u?.cacheHitRate || 0); setHasCacheHit(!!u?.hasCacheHit)
    } catch { /* ignore: a failed fetch is not evidence of zero */ }
  }, [])

  // clearUsage blanks the whole session-scoped usage row (cost, credit, cache ring). It is
  // the immediate half of refreshCost: the fetch confirms the zeros, and this makes sure the
  // row never shows another session's numbers in the meantime — a brand-new session used to
  // inherit the previous one's cache rate while having sent nothing at all.
  const clearUsage = useCallback(() => {
    setCost(0); setCredit(0); setCacheHitRate(0); setHasCacheHit(false)
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
  // Review findings come from the backend, which reads them out of the session's newest
  // review transcript: a review is a one-off run, so its findings live in that file
  // rather than in the conversation history — and that is also what makes them survive
  // a restart.
  const [reviewFindings, setReviewFindings] = useState<ReviewFindingsVO | null>(null)

  // A review is a turn like any other: when the session stops running, it is over.
  const sessionBusy = state.status !== 'idle' && state.status !== 'error'
  useEffect(() => {
    // Only the session that started the review can finish it: clearing whenever ANY session
    // is idle would drop the state of a review still running in another one.
    if (reviewPending && !sessionBusy) setReviewPending(null)
  }, [reviewPending, sessionBusy])

  // startReview runs the review fork scoped to one turn's files. The run is a normal
  // turn, so its findings stream into the transcript and the diff panel picks them up
  // from there — no extra plumbing, and they survive a restart like any other message.
  const startReview = useCallback(async (paths: string[], msgId?: string) => {
    const sid = currentId
    if (!msgId) return
    setReviewNotice(null)
    setReviewPending({ msgId })
    try {
      const res = await AgentService.ReviewChanges(sid, paths)
      setReviewPending(null)
      if (res) setReviewNotice({ msgId, text: res })
    } catch (e) {
      setReviewPending(null)
      setReviewNotice({ msgId, text: String(e) })
    }
  }, [currentId])

  // sendFindings is how the diff panel leaves: the picked findings become one ordinary
  // user message. It rides the composer's route instead of a channel of its own — while a
  // turn is running the message queues for the next steer point, because startTurn refuses
  // a busy session (so sending directly would drop the text in silence). The panel closes
  // on its way out; the reply then streams into the transcript behind it.
  const sendFindings = useCallback((text: string) => {
    setDiffPanelOpen(false)
    if (isCurrentRunning) {
      enqueuePending(currentId, text)
      return
    }
    sendText(text)
  }, [currentId, isCurrentRunning, enqueuePending, sendText])

  const openDiffPanel = useCallback(async (paths: string[]) => {
    setDiffPanelOpen(true)
    setDiffPanelData(null)
    setDiffPanelLoading(true)
    try {
      const [diff, findings] = await Promise.all([
        AgentService.GetTurnDiff(currentId, paths),
        AgentService.GetReviewFindings(currentId),
      ])
      setDiffPanelData(diff || null)
      setReviewFindings(findings || null)
    } catch {
      setDiffPanelData(null)
      setReviewFindings(null)
    } finally {
      setDiffPanelLoading(false)
    }
  }, [currentId])

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
          setMsgCache((prev) => ({ ...prev, [cur!.id]: buildTurns(page.messages) }))
          setHasMore((p) => ({ ...p, [cur!.id]: !!page.hasMore }))
          setEarliestTs((p) => ({ ...p, [cur!.id]: page.messages[0]?.timestamp }))
        }
      }
      refreshCost(cur.id)
      scrollToBottom(true)
    } else {
      const ns = await AgentService.NewSession().catch(() => null)
      if (ns) {
        setCurrentId(ns.id); setCurrentTitle(ns.title || 'Tachi')
        setMsgCache((p) => ({ ...p, [ns.id]: [] }))
        setHasMore((p) => ({ ...p, [ns.id]: false }))
        setEarliestTs((p) => ({ ...p, [ns.id]: '' }))
        clearUsage(); setTps(0); setLastTps(0)
        refreshCost(ns.id)
        refreshWorkspace(ns.id)
      }
    }
    setSessions(list.map((s) => ({ ...s, active: s.id === cur?.id })))
    setLoading(false)
    refreshProvider()
    refreshMCP()
    if (cur) refreshWorkspace(cur.id)
  }, [msgCache, scrollToBottom, refreshProvider, refreshCost, refreshMCP, refreshWorkspace, clearUsage])
  const confirmDelete = useCallback(async (id: string) => {
    await (AgentService as any).DeleteSession?.(id).catch(() => {})
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
        setMsgCache((prev) => ({ ...prev, [id]: buildTurns(page.messages) }))
        setHasMore((p) => ({ ...p, [id]: !!page.hasMore }))
        setEarliestTs((p) => ({ ...p, [id]: page.messages[0]?.timestamp }))
      }
      setLoading(false)
    }
    setTps(0); setLastTps(0)
    scrollToBottom(true)
    refreshRunning()
    refreshCost(id)
    refreshProvider()
    refreshMCP()
    refreshWorkspace(id)
  }, [sessions, msgCache, scrollToBottom, refreshRunning, refreshProvider, refreshCost, refreshMCP, refreshWorkspace])

  const newChat = useCallback(async () => {
    const ns = await AgentService.NewSession().catch(() => null)
    if (ns) {
      setCurrentId(ns.id); setCurrentTitle(ns.title || 'Tachi')
      setMsgCache((prev) => ({ ...prev, [ns.id]: [] }))
      setHasMore((p) => ({ ...p, [ns.id]: false }))
      setEarliestTs((p) => ({ ...p, [ns.id]: '' }))
      clearUsage(); setTps(0); setLastTps(0)
      refreshCost(ns.id)
      refreshWorkspace(ns.id)
      const list = (await AgentService.ListSessions().catch(() => null)) || []
      setSessions(list.map((s) => ({ ...s, active: s.id === ns.id })))
      refreshProvider()
    }
  }, [refreshProvider, refreshWorkspace, refreshCost, clearUsage])

  useEffect(() => { loadAll(); refreshRunning(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [])

  // The slash commands the desktop supports. Fetched once — the set is baked into
  // the build — and used both by the "/" palette and to decide what a lead-in "/"
  // means when sending.
  useEffect(() => {
    AgentService.ListCommands().then((list) => setCmdList(list || [])).catch(() => {})
  }, [])

  // Keyboard shortcuts: Cmd+/ focuses the composer, Cmd+B toggles the sidebar,
  // Cmd+N starts a new session. (No native menu binds them, so the webview sees
  // the key events.)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { setShortcutsOpen(false); setConfirmDel(null); setMenu(null); setReminderModal(null); return }
      if (!e.metaKey) return
      if (e.key === '/' && !e.shiftKey) { e.preventDefault(); composerRef.current?.focus() }
      else if (e.key.toLowerCase() === 'b') { e.preventDefault(); setSidebarCollapsed((v) => !v) }
      else if (e.key.toLowerCase() === 'n') { e.preventDefault(); newChat() }
      else if (e.key === '?' || (e.shiftKey && e.code === 'Slash')) { e.preventDefault(); setShortcutsOpen((v) => !v) }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [newChat])

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

  useEffect(() => {
    const off = Events.On('agent:state', (event) => { setState(event.data as AgentState) })
    AgentService.GetState().then((s) => setState(s)).catch(() => {})
    return () => off?.()
  }, [])

  useEffect(() => {
    const off = Events.On('agent:cost', (event) => {
      const d = event.data as { sessionId: string; cost: number; credit: number; cacheHitRate?: number; hasCacheHit?: boolean }
      if (d.sessionId === currentId) { setCost(d.cost || 0); setCredit(d.credit || 0); if (d.cacheHitRate != null) setCacheHitRate(d.cacheHitRate); setHasCacheHit(!!d.hasCacheHit) }
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const off = Events.On('agent:tps', (event) => {
      const d = event.data as { sessionId: string; tps: number; lastTps?: number }
      if (d.sessionId !== currentId) return
      if (d.tps > 0) { setTps(d.tps); setLastTps(0) }
      else { setTps(0); if (d.lastTps) setLastTps(d.lastTps) }
    })
    return () => off?.()
  }, [currentId])

  // Questions arrive while the turn is parked; the form renders in the
  // transcript (see AssistantBubble), so pull the view down to it.
  useEffect(() => {
    const off = Events.On('agent:ask', (event) => {
      const d = event.data as { sessionId?: string; toolId?: string; questions?: Question[] } | undefined
      if (!d?.sessionId || !d.questions?.length) return
      setAsks((prev) => ({ ...prev, [d.sessionId as string]: { toolId: d.toolId || '', questions: d.questions as Question[] } }))
      // The form renders in the transcript, so follow it into view — but only
      // while the user is already parked at the bottom: yanking someone who is
      // reading history down to a form would be exactly the kind of
      // interruption this UI should avoid.
      if (d.sessionId === currentId) scrollToBottom()
    })
    return () => off?.()
  }, [currentId, scrollToBottom])

  useEffect(() => {
    const off = Events.On('agent:error', (event) => {
      const d = event.data as { sessionId: string; error: string; interrupted?: boolean }
      clearAsk(d.sessionId) // the turn is over: nothing can still be waiting
      if (d.sessionId !== currentId) return
      const interrupted = !!d.interrupted
      setMsgCache((prev) => {
        const list = prev[currentId] || []
        // The turn being concluded is the newest assistant message. Unlike a
        // running-scan, this still works when agent:event has already flipped
        // running off (listener order is not guaranteed). Mutations are
        // idempotent so double-handling is harmless.
        const target = [...list].reverse().find((m) => m.role === 'assistant')
        if (!target) return prev
        const conclude = (m: Message): Message => {
          // After a stop no tool_result event arrives for in-flight calls, so
          // close their cards; a real error is surfaced as a trailing text
          // part (the transcript renders parts in order — there is no
          // separate error field to append to).
          const parts = interrupted
            ? closeOpenToolParts(m.parts, '已中断')
            : [...(m.parts || []), { type: 'text' as const, text: '⚠ ' + (d.error || '出错') }]
          return { ...m, running: false, stopped: interrupted || m.stopped, parts }
        }
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? conclude(m) : m)) }
      })
    })
    return () => off?.()
  }, [currentId, clearAsk])

  useEffect(() => {
    const off = Events.On('agent:turn', (event) => {
      const d = event.data as { sessionId: string; durationMs: number; iterations: number; cost: number; credit: number }
      if (d.sessionId !== currentId) return
      setMsgCache((prev) => {
        const list = prev[currentId] || []
        const target = [...list].reverse().find((m) => m.role === 'assistant')
        if (!target) return prev
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? { ...m, summary: d } : m)) }
      })
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const off = Events.On('agent:tool', (event) => {
      const t = event.data as { name: string; title: string; args: string; change?: FileChangeVO | null }
      setMsgCache((prev) => {
        const list = prev[currentId] || []
        const target = [...list].reverse().find((m) => m.role === 'assistant' && m.running)
        if (!target) return prev
        return { ...prev, [currentId]: list.map((m) => (m.id === target.id ? updateToolPart(m, t.name, t.title, t.args, t.change) : m)) }
      })
    })
    return () => off?.()
  }, [currentId])

  useEffect(() => {
    const off = Events.On('agent:event', (event) => {
      const d = event.data as { sessionId: string; event: AgentEvent }
      const { sessionId, event: ev } = d
      // Deltas ride the frame batcher; every other event must be applied in
      // order, so flush pending text/thinking first.
      if (ev.Type !== 'thinking_delta' && ev.Type !== 'text_delta') flushDeltas()
      switch (ev.Type) {
        case 'thinking_delta': enqueueDelta(sessionId, 'thinking', ev.ThinkingDelta || ''); break
        case 'text_delta': enqueueDelta(sessionId, 'text', ev.TextDelta); break
        case 'tool_call_start': applyToSession(sessionId, 'assistant', (m) => pushPart(m, { type: 'tool', name: ev.ToolName, title: '', args: '', summary: '执行中…', ok: true, done: false, expand: ev.ToolAutoExpand }), true); break
        case 'tool_result': applyToSession(sessionId, 'assistant', (m) => finishToolPart(m, ev.ToolName, ev.ToolResult, !ev.ToolIsError, ev.ToolDuration ? Math.round(ev.ToolDuration / 1e6) : undefined), true); break
        case 'turn_complete': applyToSession(sessionId, 'assistant', (m) => ({ ...m, running: false })); break
        case 'steer_check':
          // The agent finished a round of tool calls and is parked, waiting
          // for pending user input to steer the next LLM call. Queued text is
          // drained, shown in the transcript and injected; an empty queue
          // replies "" to unblock the loop without steering.
          answerSteer(sessionId)
          break
        case 'error': {
          const interrupted = ev.Result?.ExitReason === 'interrupted' || ev.Result?.ExitReason === 'cancelled'
          applyToSession(sessionId, 'assistant', (m) => ({ ...m, running: false, stopped: interrupted || m.stopped }))
          break
        }
      }
      // These two are backend round-trips over IPC and used to run for EVERY
      // delta — the single biggest cause of stutter while streaming. They only
      // carry new information at turn boundaries.
      if (ev.Type === 'turn_complete' || ev.Type === 'error') {
        refreshRunning()
        refreshProvider()
      }
    })
    return () => off?.()
  }, [currentId, applyToSession, refreshRunning, refreshProvider, answerSteer, flushDeltas, enqueueDelta])

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
    const moveKey = <T,>(rec: Record<string, T>): Record<string, T> => {
      if (!(from in rec)) return rec
      const { [from]: kept, ...rest } = rec
      return { ...rest, [to]: kept }
    }
    const dropKey = <T,>(rec: Record<string, T>): Record<string, T> => {
      if (!(from in rec) && !(to in rec)) return rec
      const { [from]: dropped, [to]: replaced, ...rest } = rec
      return rest
    }
    setPending(moveKey)
    pendingRef.current = moveKey(pendingRef.current)
    setAsks(moveKey)
    setMsgCache(dropKey)
    setHasMore(dropKey)
    setEarliestTs(dropKey)
    setRunningSet((prev) => (prev.has(from) ? new Set([...prev].map((id) => (id === from ? to : id))) : prev))
    setCurrentId(to)
    // The compacted child is a new row in the sidebar (the parent stays as the
    // pre-compaction history, exactly as in the TUI). `active` is frontend state,
    // so it is re-derived rather than fetched.
    setSessions((prev) => prev.map((s) => ({ ...s, active: s.id === to })))
    void AgentService.ListSessions()
      .then((list) => { if (list) setSessions(list.map((s) => ({ ...s, active: s.id === to }))) })
      .catch(() => {})
    setTps(0); setLastTps(0)
    void (async () => {
      setLoading(true)
      const page: any = await (AgentService as any).LoadSession?.(to, PAGE_SIZE).catch(() => null)
      setMsgCache((prev) => ({ ...prev, [to]: page?.messages ? buildTurns(page.messages) : [] }))
      setHasMore((p) => ({ ...p, [to]: !!page?.hasMore }))
      setEarliestTs((p) => ({ ...p, [to]: page?.messages?.[0]?.timestamp }))
      setLoading(false)
      scrollToBottom(true)
    })()
    refreshRunning()
    refreshCost(to)
    refreshProvider()
    refreshMCP()
    refreshWorkspace(to)
  }, [refreshRunning, refreshProvider, refreshCost, refreshMCP, refreshWorkspace, scrollToBottom])

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

  // agent:idle fires after a turn goroutine has fully exited (running already
  // reset on the backend). A NATURAL completion auto-sends whatever the user
  // queued while the turn ran (same drain semantics as TUI's TurnComplete);
  // stopped/errored turns leave the queue for the user to review, drop or
  // send manually.
  useEffect(() => {
    const off = Events.On('agent:idle', (event) => {
      const d = event.data as { sessionId: string; reason: string; current?: boolean }
      clearAsk(d.sessionId) // the turn is over: nothing can still be waiting
      // `current` comes from the backend (was this the displayed session?) rather
      // than being compared against this closure's currentId: an auto-compaction
      // switch can move the session between the two events, and React state read
      // through a stale closure would then silently drop the queued message.
      if (!d.current || d.reason !== 'complete') return
      const queued = pendingRef.current[d.sessionId] || []
      if (queued.length === 0) return
      sendText(takePending(d.sessionId))
    })
    return () => off?.()
  }, [sendText, takePending, clearAsk])

  // stopChat aborts the running turn in the current session (backend cancels
  // the turn ctx — same mechanism as tui Ctrl+C / acp prompt cancel).
  const stopChat = useCallback(() => {
    AgentService.Stop().catch(() => {})
  }, [])

  const meta = STATUS_META[state.status as string] ?? STATUS_META.idle
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
          <ThemeToggle theme={theme} onToggle={toggleTheme} />
          <div className="status-badge"><span className={`dot dot-${state.status}`}>{meta.dot}</span><span className="status-label">{state.label}</span></div>
        </div>
      </header>

      <div className="app-body">
        <aside className={`sidebar${sidebarCollapsed ? ' collapsed' : ''}`}>
          <button className="new-chat" onClick={newChat}><span className="new-chat-plus">＋</span> 新建会话</button>
          <nav className="session-list">
            <div className="session-section">最近</div>
            {sessions.map((s) => (
              /* Row (not a <button>): it hosts a rename <input> and a context
                 menu, so it takes role/tabIndex + Enter/Space instead. */
              <div key={s.id} className={`session ${s.active ? 'active' : ''}`} onClick={() => clickSession(s.id)}
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
                <div className="session-meta">{runningSet.has(s.id) ? <span className="spin-dot" title="运行中" /> : null}{new Date(s.updatedAt).toLocaleString('zh-CN', { hour12: false })}</div>
              </div>
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
            {loading ? <div className="chat-loading">加载会话…</div> : (
              <>
                {messages.length === 0 && (
                  <div className="welcome"><div className="welcome-mark">◆</div><div className="welcome-title">你好，我是 Tachi</div><div className="welcome-sub">在下方输入问题开始对话。左侧可切换或新建会话。</div></div>
                )}
                {messages.map((m) =>
                  m.role === 'user' ? (
                    <Fragment key={m.id}>
                      <UserBubble>
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
                      ask={m.running ? (asks[currentId]?.questions || null) : null}
                      onAnswer={answerCurrent}
                      onToggleDiff={(i) => patchMessage(m.id, (msg) => togglePartDiff(msg, i))}
                      onToggleAllDiffs={(v) => patchMessage(m.id, (msg) => setPartDiffs(msg, v))}
                      onOpenDiffPanel={openDiffPanel}
                      onReviewChanges={startReview}
                      reviewPending={reviewPending?.msgId === m.id}
                      reviewNotice={reviewNotice?.msgId === m.id ? reviewNotice.text : undefined}
                      sessionBusy={sessionBusy}
                    />
                  ),
                )}
              </>
            )}
            </div>
            {diffPanelOpen ? (
              <DiffPanel diff={diffPanelData} loading={diffPanelLoading} findings={reviewFindings?.findings || []}
                findingsNote={reviewFindings?.note} findingsReport={reviewFindings?.report}
                onClose={() => setDiffPanelOpen(false)}
                onSend={sendFindings} />
            ) : null}
            {showJump && (
              <button className="jump-latest" onClick={() => scrollToBottom(true)} title="回到最新消息">
                <span className="jump-ico">↓</span>回到最新
              </button>
            )}
          </div>

          <footer className="composer">
            {pendMsgs.length > 0 && (
              <div className="pending-bar">
                <div className="pending-head">
                  <span className="pending-title">
                    {isCurrentRunning ? '⏸ 待发送 · 会在本轮工具调用结束后自动插入' : '待发送消息'}
                  </span>
                  <span className="pending-actions">
                    <button className="pending-send" disabled={sendingNow} onClick={sendPendingNow}>
                      {sendingNow ? '正在停止…' : isCurrentRunning ? '立即发送 · 打断' : '发送'}
                    </button>
                    <button className="pending-clear" onClick={() => clearPending(currentId)} title="清空待发送队列">清空</button>
                  </span>
                </div>
                {pendMsgs.map((t, i) => (
                  <div className="pending-item" key={`${i}-${t.slice(0, 8)}`}>
                    <span className="pending-bullet">·</span>
                    <span className="pending-text" title={t}>{t}</span>
                    <button className="pending-del" title="撤回该条" onClick={() => dropPending(currentId, i)}>✕</button>
                  </div>
                ))}
              </div>
            )}
            <div className="composer-box" data-file-drop-target="true">
              {cmdOpen && (
                <CommandPicker
                  items={cmdMatches}
                  selected={Math.min(cmdIdx, Math.max(0, cmdMatches.length - 1))}
                  onPick={(i) => acceptCommand(cmdMatches[i])}
                />
              )}
              {at && (
                <AtFilePicker
                  query={at.query}
                  items={at.items}
                  selected={at.idx}
                  loading={at.loading}
                  refCount={at.count}
                  onPick={(i) => acceptAt(at.items[i])}
                  onHover={(i) => setAt((p) => (p ? { ...p, idx: i } : p))}
                />
              )}
              <div className="composer-input-wrap">
                <textarea className="composer-input" ref={composerRef} value={input}
                  onChange={(e) => { setInput(e.target.value); syncAtRef(e.target.value, e.target.selectionStart ?? e.target.value.length) }}
                  onSelect={(e) => { const el = e.currentTarget; syncAtRef(el.value, el.selectionStart ?? el.value.length) }}
                  onBlur={() => closeAt()}
                  onCompositionStart={() => { composingRef.current = true }}
                  onCompositionEnd={() => { window.setTimeout(() => { composingRef.current = false }, 0) }}
                  onKeyDown={(e) => {
                    const ime = composingRef.current || imeActive(e)
                    // While the @-picker is open it owns the navigation keys;
                    // an IME composing (candidate selection) owns them first.
                    if (!ime && at) {
                      // ↑↓ and Ctrl+N/Ctrl+P both move the highlight (Ctrl+N/P
                      // matches the TUI's keymap, and preventDefault keeps
                      // Cocoa's own Ctrl+N/P caret movement out of the way).
                      const down = e.key === 'ArrowDown' || (e.ctrlKey && e.key.toLowerCase() === 'n')
                      const up = e.key === 'ArrowUp' || (e.ctrlKey && e.key.toLowerCase() === 'p')
                      if (down || up) {
                        e.preventDefault()
                        const delta = down ? 1 : -1
                        setAt((p) => (p && p.items.length
                          ? { ...p, idx: Math.max(0, Math.min(p.items.length - 1, p.idx + delta)) }
                          : p))
                        return
                      }
                      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
                        // Tab/Enter accept the highlighted match; with no match,
                        // Enter still sends (Tab just dismisses the picker).
                        if (at.items.length > 0) { e.preventDefault(); acceptAt(at.items[at.idx]); return }
                        if (e.key === 'Tab') { e.preventDefault(); closeAt(); return }
                      }
                      if (e.key === 'Escape') { e.preventDefault(); closeAt(); return }
                    }
                    // The "/" palette owns the navigation keys while it is open
                    // (the @-picker cannot be open at the same time: one is
                    // triggered by "@", the other by a leading "/").
                    if (!ime && cmdOpen) {
                      const down = e.key === 'ArrowDown' || (e.ctrlKey && e.key.toLowerCase() === 'n')
                      const up = e.key === 'ArrowUp' || (e.ctrlKey && e.key.toLowerCase() === 'p')
                      if (down || up) {
                        e.preventDefault()
                        const delta = down ? 1 : -1
                        setCmdIdx((i) => Math.max(0, Math.min(cmdMatches.length - 1, i + delta)))
                        return
                      }
                      if (e.key === 'Tab') { e.preventDefault(); acceptCommand(cmdMatches[Math.min(cmdIdx, cmdMatches.length - 1)]); return }
                      if (e.key === 'Escape') { e.preventDefault(); setCmdDismissed(cmdQuery); return }
                      // Enter completes rather than sends while the name is still
                      // partial: "/rev" + Enter must not run the wrong command.
                      if (e.key === 'Enter' && !e.shiftKey && cmdMatches.length > 0) {
                        e.preventDefault()
                        acceptCommand(cmdMatches[Math.min(cmdIdx, cmdMatches.length - 1)])
                        return
                      }
                    }
                    // Esc hands the focus to the message area (the Vim-like keys
                    // live there). Not listed in the shortcut sheet on purpose:
                    // it is a focus affordance, not a feature shortcut.
                    if (e.key === 'Escape') { e.preventDefault(); chatRef.current?.focus(); return }
                    if (e.key !== 'Enter' || e.shiftKey) return
                    // Enter while an IME is composing confirms the candidate
                    // (选词), it must not send the message.
                    if (ime) return
                    e.preventDefault()
                    send()
                  }}
                  placeholder={isCurrentRunning ? '正在运行…输入后 Enter 将加入待发送，在工具间隙自动插入' : '发送消息给 Tachi…（Enter 发送，Shift+Enter 换行，@ 引用文件）'} />
                {isCurrentRunning && (
                  <button className="stop-btn" title="停止生成" onClick={stopChat} aria-label="停止生成">
                    <svg viewBox="0 0 24 24" width="10" height="10" fill="currentColor"><rect x="7" y="7" width="10" height="10" rx="1.5" /></svg>
                  </button>
                )}
              </div>
              <div className="composer-actions">
                <button className="send-btn" onClick={send} disabled={!input.trim() || sendingNow}>
                  <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"><line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" /></svg>
                  <span>{isCurrentRunning ? '排队' : '发送'}</span>
                </button>
              </div>
            </div>
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
      </div>
      {menu && (
        <div className="ctx-menu" role="menu" style={{ left: menu.x, top: menu.y }} onMouseLeave={() => setMenu(null)}>
          <button className="ctx-item" role="menuitem" onClick={() => { setEditingId(menu.sid); setEditTitle(sessions.find((x) => x.id === menu.sid)?.title || ''); setMenu(null) }}>重命名</button>
          <button className="ctx-item danger" role="menuitem" onClick={() => { const t = sessions.find((x) => x.id === menu.sid)?.title || ''; setConfirmDel({ sid: menu.sid, title: t }); setMenu(null) }}>删除</button>
        </div>
      )}
      {confirmDel && (
        <div className="confirm-overlay" onClick={() => setConfirmDel(null)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">删除会话「{confirmDel.title || '未命名会话'}」？</div>
            <div className="confirm-sub">此操作不可恢复。</div>
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setConfirmDel(null)}>取消</button>
              <button className="btn danger" onClick={() => { const id = confirmDel.sid; setConfirmDel(null); confirmDelete(id) }}>删除</button>
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
            <div className="shortcut-row"><kbd>⌘ B</kbd><span>折叠 / 展开侧栏</span></div>
            <div className="shortcut-row"><kbd>⌘ ?</kbd><span>显示本快捷键列表</span></div>
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
    </div>
  )
}

// humanize renders a token count as a compact human-friendly number, e.g.
// 128512 → "128.5K", 2000000 → "2M". Used for the context-ring tooltip.
// Token counts never reach the G scale (context windows max out around M), so
// only K/M are emitted; anything above M is shown verbatim as a fallback.
export default App
