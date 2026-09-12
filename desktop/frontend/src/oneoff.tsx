// The side-channel panel (third column): what a one-off run (/review, /commit) did, kept
// out of the conversation on purpose.
//
// The panel is a READER, not a second transcript. A record is replayed through the same
// messages (Go: buildSessionMessages) and the same parts (parts.tsx) the conversation uses,
// so a tool card here shows the same fragment diff and a reply the same markdown — there is
// no second rendering to keep in step.
//
// Design: docs/2026-09-12-desktop-oneoff-panel-design.md §5

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { AgentService, type OneOffDetailVO, type OneOffRequestVO, type OneOffVO, type TurnDiffVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { OneOffRun } from './agentEvents'
import { buildTurns, fmtDur, fmtTime } from './lib'
import { TurnPart } from './parts'
import { DiffFindingsPane } from './diff'
import { MarkdownBlock } from './markdown'
import { UserBubble } from './components'
import { CloseButton } from './viewer'

// ONE_OFF_LIVE_POLL_MS is how often a RUNNING run's record is re-read. The record is written
// line by line as the run goes, so this is a read of a file that is still growing: fast
// enough to watch it work, slow enough that re-parsing a few hundred KB costs nothing.
const ONE_OFF_LIVE_POLL_MS = 1200

// The panel's width and its bounds. Mirrored by Go (desktop/uitheme.go's
// oneOffPanelMinWidth/MaxWidth), which refuses to store a width outside them — a preference
// file is editable by hand, and a number the layout cannot honour is worse than the default.
export const ONE_OFF_PANEL_DEFAULT_WIDTH = 420
const ONE_OFF_PANEL_MIN_WIDTH = 320
const ONE_OFF_PANEL_MAX_WIDTH = 1000
// The conversation never gives up more than this, whatever the window size: the panel is a
// side channel, and a side channel that swallows the main thing is a bug.
const CHAT_MIN_WIDTH = 480
// One arrow-key press on the handle (a drag is not the only way to resize).
const ONE_OFF_PANEL_KEY_STEP = 24

// panelRoom is the widest the panel may be shown: the window, less the sidebar — the other
// column that never shrinks — less CHAT_MIN_WIDTH for the conversation, capped at
// ONE_OFF_PANEL_MAX_WIDTH. Read from the DOM rather than assumed from the CSS, so a folded
// sidebar hands its room back. In a window too narrow to honour both minimums the panel's own
// floor wins: something has to give there, and the conversation is what yields.
function panelRoom(): number {
  if (typeof window === 'undefined') return ONE_OFF_PANEL_MAX_WIDTH
  const sidebar = document.querySelector('.sidebar')
  const used = sidebar ? sidebar.getBoundingClientRect().width : 0
  const room = window.innerWidth - used - CHAT_MIN_WIDTH
  return Math.max(ONE_OFF_PANEL_MIN_WIDTH, Math.min(ONE_OFF_PANEL_MAX_WIDTH, room))
}

// clampPanelWidth limits a requested width to the room there is. The result is DISPLAY-ONLY:
// what gets stored stays the width the reader chose, so a narrow window limits what is shown
// without overwriting the choice — and widening the window gives the chosen width back.
function clampPanelWidth(px: number, room: number): number {
  return Math.max(ONE_OFF_PANEL_MIN_WIDTH, Math.min(room, Math.round(px)))
}

// oneOffKindLabel names a run in the reader's words. The kind is the recorded one
// (review / review-round-N / commit), which is the same string the usage ledger uses.
function oneOffKindLabel(kind: string): string {
  const rounds = /^review-round-(\d+)$/.exec(kind)
  if (rounds) return `评审 第 ${rounds[1]} 轮`
  if (kind === 'review') return '评审'
  if (kind === 'commit') return '提交'
  return kind
}

// oneOffDoneLabel is the ONE LINE the conversation keeps about a side-channel run: what it
// was and where its process went. It is the whole price the transcript pays for a review.
export function oneOffDoneLabel(run: OneOffRun): string {
  const what = oneOffKindLabel(run.kind) || '旁路运行'
  if (run.phase === 'error') {
    return run.interrupted ? `⏹ ${what}已停止 · 过程在右侧面板` : `⚠ ${what}失败：${run.error || '见日志'}`
  }
  const findings = run.findings ? ` · ${run.findings} 条意见` : ''
  return `⤴ ${what}已完成${findings} · 过程与报告在右侧面板`
}

// oneOffRunLabel is the footer chip's state text for the turn a review was started from.
export function oneOffRunLabel(run: { findings: number; interrupted?: boolean }): string {
  if (run.interrupted) return '评审已停止 · 查看'
  return run.findings ? `已评审 ${run.findings} 条 · 查看` : '已评审 · 未报问题'
}

// oneOffLabel is one switcher entry: what it was, when it started, and the two facts a
// reader picks it by (which model, how many findings).
function oneOffLabel(it: OneOffVO, busy: boolean): string {
  const bits = [oneOffKindLabel(it.kind), fmtTime(it.startedAt) || '—']
  if (it.model) bits.push(it.model)
  if (it.findings) bits.push(`${it.findings} 条意见`)
  if (it.running && busy) bits.push('运行中')
  return bits.join(' · ')
}

export type OneOffApi = ReturnType<typeof useOneOffs>

// OneOffTab is which pane the panel is showing. The findings/diff pane and the report pane
// exist for the runs that have them; a run that only produced prose shows 过程 alone.
export type OneOffTab = 'flow' | 'findings' | 'report'

// useOneOffs owns one session's side-channel runs: the list, which one is selected, and the
// selected run's replay. The list is refetched when the panel opens (a run may have
// finished while it was closed); switching runs loads a replay without touching the list,
// so the reader's place in the menu survives.
//
// A LIVE run is followed by polling its record rather than by consuming the run's events:
// the record is written as the run goes, it is the only source of content (the backend sends
// lifecycle facts only — see agentEvents.ts), and one reader cannot disagree with itself.
export function useOneOffs(sessionId: string, open: boolean, live: OneOffRun | null) {
  const [items, setItems] = useState<OneOffVO[]>([])
  const [note, setNote] = useState('')
  const [selected, setSelected] = useState('')
  const [detail, setDetail] = useState<OneOffDetailVO | null>(null)
  const [listLoading, setListLoading] = useState(false)
  const [detailLoading, setDetailLoading] = useState(false)
  const [error, setError] = useState('')
  // requests: the lazily fetched prompt text, keyed by the request's Seq. A record keeps
  // its prompts per call (over half of its bytes), so they are fetched only when asked for.
  const [requests, setRequests] = useState<Record<number, OneOffRequestVO | 'loading'>>({})
  // tab: which pane is showing. It starts on 过程 (what the run DID); 意见 is a click on the tab.
  const [tab, setTab] = useState<OneOffTab>('flow')
  const [diff, setDiff] = useState<TurnDiffVO | null>(null)
  const [diffLoading, setDiffLoading] = useState(false)
  // Why the diff could not be read. Kept apart from `diff === null` because those are two
  // different facts — "no changes to show" and "the read failed" — and an empty pane cannot
  // tell the reader which one it is looking at.
  const [diffError, setDiffError] = useState('')
  // The report's text, tagged with the run it was read for. The text is a FILE fetched by name,
  // and the 报告 pane stays open across a switch in the switcher — so a cache without the run's
  // key would print one run's report under another run's file line, and refuse to refetch.
  const [report, setReport] = useState<{ key: string; text: string } | null>(null)

  // selectedRef mirrors the selection so a reply can be matched against it: a load for a run
  // the reader has already left must not land on top of the new one (the poll makes this
  // reachable, not just the switching effect).
  const selectedRef = useRef('')
  selectedRef.current = selected

  const refresh = useCallback(async (sid: string): Promise<OneOffVO[]> => {
    if (!sid) {
      setItems([]); setNote('还没有会话'); setSelected(''); setDetail(null)
      return []
    }
    setListLoading(true)
    setError('')
    try {
      const res = await AgentService.ListOneOffs(sid)
      const list = res?.items || []
      setItems(list)
      setNote(res?.note || '')
      // Keep the reader's selection across a refresh; fall back to the newest run when it
      // is gone (or when this is the first open).
      setSelected((prev) => (prev && list.some((it) => it.name === prev) ? prev : (list[0]?.name || '')))
      return list
    } catch (e) {
      setItems([]); setNote(''); setError('读取旁路记录失败：' + String(e)); setSelected('')
      return []
    } finally {
      setListLoading(false)
    }
  }, [])

  // loadDetail reads one run. `silent` is what the live poll passes: a poll must not flip the
  // panel into its "读取记录中…" state once a second.
  const loadDetail = useCallback(async (name: string, silent = false) => {
    if (!name) return
    if (!silent) setDetailLoading(true)
    try {
      const d = await AgentService.LoadOneOff(sessionId, name)
      if (selectedRef.current === name) setDetail(d)
    } catch (e) {
      if (selectedRef.current === name) setError('读取旁路记录失败：' + String(e))
    } finally {
      if (!silent) setDetailLoading(false)
    }
  }, [sessionId])

  useEffect(() => {
    if (!open) return
    void refresh(sessionId)
  }, [open, sessionId, refresh])

  useEffect(() => {
    if (!open || !sessionId || !selected) {
      setDetail(null)
      return
    }
    setRequests({})
    void loadDetail(selected)
  }, [open, sessionId, selected, loadDetail])

  // A run started: re-read the menu (its record exists from the first line) and select it, so
  // the panel is already showing the run the reader just launched.
  useEffect(() => {
    if (!open || !live) return
    void (async () => {
      const list = await refresh(sessionId)
      if (list[0]) setSelected(list[0].name)
    })()
  }, [open, live?.at, sessionId, refresh])

  // While it runs, follow the record. ONE_OFF_LIVE_POLL_MS is fast enough to watch the run
  // work and slow enough that re-reading a few hundred KB costs nothing.
  useEffect(() => {
    if (!open || !live) return
    const t = window.setInterval(() => { void loadDetail(selectedRef.current, true) }, ONE_OFF_LIVE_POLL_MS)
    return () => window.clearInterval(t)
  }, [open, live, loadDetail])

  // It ended: one last read. The final poll may have caught the record mid-write, and the
  // tail of a run is exactly what a reader came for.
  const wasLive = useRef(false)
  useEffect(() => {
    if (!open) return
    if (wasLive.current && !live) {
      void refresh(sessionId)
      void loadDetail(selectedRef.current, true)
    }
    wasLive.current = !!live
  }, [open, live, sessionId, refresh, loadDetail])

  // Fetching is a side effect, so it must NOT live inside a state updater: React may invoke an
  // updater more than once for the same update (StrictMode does, and concurrent re-basing can),
  // and both runs would fire a request — the `prev[seq]` guard cannot help, since both see the
  // same pre-update state. The in-flight set is what makes one click one request.
  const inflight = useRef<Set<number>>(new Set())
  useEffect(() => { inflight.current = new Set() }, [sessionId, selected])
  const loadRequest = useCallback((seq: number) => {
    if (inflight.current.has(seq)) return
    inflight.current.add(seq)
    setRequests((prev) => (prev[seq] ? prev : { ...prev, [seq]: 'loading' }))
    // The reply belongs to the run that was showing when the row was clicked: a reader who
    // switches mid-fetch must not get the old prompt attached to the new run.
    const asked = selected
    const settle = (vo: OneOffRequestVO) => {
      inflight.current.delete(seq)
      if (selectedRef.current !== asked) return
      setRequests((prev) => ({ ...prev, [seq]: vo }))
    }
    AgentService.LoadOneOffRequest(sessionId, asked, seq)
      .then(settle)
      .catch((e) => settle({ seq, notice: String(e) } as OneOffRequestVO))
  }, [sessionId, selected])

  // Everything cached per run is tagged with the SESSION as well as the run — a record's name is
  // a timestamp within its session, so two sessions can hold the same one.
  const runKey = detail ? `${sessionId}/${detail.header.name}` : ''

  // The files a diff can be built from: the scope the run recorded. One key, so the effect
  // below cannot fire on every render — and so switching runs re-reads instead of showing the
  // previous run's diff.
  const pathsKey = useMemo(() => (detail?.header.paths || []).join('\n'), [detail])
  const diffRunRef = useRef('')
  useEffect(() => {
    if (tab !== 'findings' || !runKey) return
    const files = pathsKey ? pathsKey.split('\n') : []
    if (files.length === 0) {
      setDiff(null)
      setDiffError('')
      diffRunRef.current = ''
      return
    }
    const key = `${runKey}\n${pathsKey}`
    if (diffRunRef.current === key) return
    diffRunRef.current = key
    let alive = true
    setDiffLoading(true)
    setDiffError('')
    AgentService.GetTurnDiff(sessionId, files)
      .then((d) => { if (alive) setDiff(d || null) })
      .catch((e) => {
        if (!alive) return
        setDiff(null)
        // Read back for the reader instead of dropping it: an empty pane and a failed read
        // look identical, and the reason is the only thing that tells them apart.
        setDiffError(String(e))
        diffRunRef.current = '' // a retry (reopening the pane) may try again
      })
      .finally(() => { if (alive) setDiffLoading(false) })
    return () => { alive = false }
  }, [tab, sessionId, runKey, pathsKey])

  // openReport renders the run's report in place: a file read through the same PreviewFile the
  // attachment preview uses, then the app's markdown renderer. No overlay — this panel is where
  // the report belongs.
  const openReport = useCallback(async () => {
    setTab('report')
    const file = detail?.header.report
    if (!file || !runKey || report?.key === runKey) return
    try {
      const vo = await AgentService.PreviewFile(file, true)
      setReport({ key: runKey, text: vo?.text || '' })
    } catch (e) {
      setReport({ key: runKey, text: '无法读取报告：' + String(e) })
    }
  }, [detail?.header.report, runKey, report])

  // Only the shown run's report, so keeping the pane open across a switch is safe: a key that
  // does not match means "not fetched yet" and the pane asks for it again.
  const reportText = report && runKey && report.key === runKey ? report.text : null

  // Memoized: App destructures this object and derives a callback from it that reaches a
  // memoized message bubble, so a fresh identity on every render would break that memo and
  // re-render the whole transcript on every streaming delta.
  const refreshCurrent = useCallback(() => void refresh(sessionId), [refresh, sessionId])
  return useMemo(() => ({
    items, note, selected, detail, listLoading, detailLoading, error, requests, live,
    select: setSelected, refresh: refreshCurrent, loadRequest, tab, setTab, pathsKey, diff,
    diffLoading, diffError, reportText, openReport, runKey,
  }), [
    items, note, selected, detail, listLoading, detailLoading, error, requests, live,
    refreshCurrent, loadRequest, tab, pathsKey, diff, diffLoading, diffError, reportText, openReport, runKey,
  ])
}

// OneOffPanel is the third column: the run's process, its findings against the diff they came
// from, and its report — three panes over one record, so the reader never has to hold two
// overlays in their head.
export function OneOffPanel({ api, workDir, busy, width, onResizeCommit, onSend, onRerun, onClose }: {
  api: OneOffApi
  // Resolves relative paths inside the replayed parts (markdown images, attachment cards).
  workDir: string
  // Whether the session is mid-turn right now. The backend's per-record "running" flag is
  // an mtime heuristic (a record has no end marker), so a run that finished seconds ago
  // still looks in-flight; the session's own state is the precise half of that pair.
  busy: boolean
  // The width the reader CHOSE, owned by App because it is persisted there
  // (desktop_ui.json). What the panel shows is derived from it — see shownWidth below.
  width: number
  // Called once when a drag ends (and on each arrow-key step): that is what gets written.
  onResizeCommit: (px: number) => void
  // Sends the picked findings to the agent as one ordinary user message — the panel's way
  // out, shared with the composer's own path.
  onSend?: (text: string) => void
  // Runs another review over the same files (see DiffFindingsPane's onRerun). The paths and
  // the reviewed message come from the run's own record, which is what makes a re-run from a
  // historical entry as ordinary as one from the chip.
  onRerun?: (paths: string[], reviewedMsg: string) => void
  onClose: () => void
}) {
  const { items, note, selected, detail, listLoading, detailLoading, error, requests, live, select, refresh, loadRequest, tab, setTab, pathsKey, diff, diffLoading, reportText, openReport } = api
  // The replay is rebuilt from the record's messages — the same input shape the transcript
  // feeds buildTurns, so the turns and their parts are the conversation's own.
  const turns = useMemo(() => (detail?.messages ? buildTurns(detail.messages) : []), [detail])
  // The tabs a run actually has: a commit has no findings and no diff, so offering the pane
  // would be offering an empty box. 过程 is always there — it is what the run did.
  const isReview = !!detail && detail.header.kind.startsWith('review')
  const findings = detail?.findings || []
  const tabs: { key: OneOffTab; label: string }[] = [
    { key: 'flow', label: '过程' },
    ...(isReview ? [{ key: 'findings' as const, label: findings.length ? `意见 ${findings.length}` : '意见' }] : []),
    ...(detail?.header.report ? [{ key: 'report' as const, label: '报告' }] : []),
  ]
  const shown = tabs.some((t) => t.key === tab) ? tab : 'flow'

  // ── Resizing ──────────────────────────────────────────────────────────────
  // A drag listens on the WINDOW, not on the handle: a pointer that leaves the 6px handle
  // mid-drag must keep resizing (that is what dragging means), and window listeners also work
  // for synthetic pointer events, where setPointerCapture refuses a pointer it never saw go
  // down.
  //
  // What is SHOWN and what is STORED are two different numbers. The reader's choice lives in
  // App (and on disk); the room there is for it is measured here, and re-measured when the
  // window resizes or the sidebar folds. So a narrow spell limits the panel without
  // overwriting the choice — and widening the window again gives the chosen width back.
  const [room, setRoom] = useState(() => panelRoom())
  useEffect(() => {
    const measure = () => setRoom(panelRoom())
    window.addEventListener('resize', measure)
    // Folding the sidebar frees a column's worth of room without resizing the window.
    const sidebar = document.querySelector('.sidebar')
    const ro = sidebar && typeof ResizeObserver !== 'undefined' ? new ResizeObserver(measure) : null
    if (sidebar && ro) ro.observe(sidebar)
    return () => { window.removeEventListener('resize', measure); ro?.disconnect() }
  }, [])
  // The width under the pointer while a drag is in flight; null means "the reader's own width,
  // as far as the window allows".
  //
  // The IN-FLIGHT width is NOT React state, and that is the whole point: the width lives on the
  // panel, so a state update per pointermove re-rendered the whole panel — the 意见 pane's diff
  // included, a couple of hundred rows and more on a real review — before the browser had even
  // laid the new width out. That is what 「左右拖动时很卡」 was. The drag now writes the width
  // straight to the element (one style write per move; the browser still lays out once per frame)
  // and hands the number back to React on release, where it is committed and persisted.
  const [dragging, setDragging] = useState(false)
  const panelRef = useRef<HTMLElement | null>(null)
  // The drag's own value, so a render that happens mid-drag (a streamed delta, a findings update,
  // a window resize re-measuring the room) cannot put React's older number back on screen.
  const dragPxRef = useRef<number | null>(null)
  useLayoutEffect(() => {
    if (dragPxRef.current !== null && panelRef.current) {
      panelRef.current.style.width = `${dragPxRef.current}px`
    }
  })
  // What React renders is the STORED width clamped to the room — unchanged while a drag is in
  // flight (the drag owns the element then, and re-asserts its own value after every commit).
  const shownWidth = clampPanelWidth(width, room)

  // Esc closes the panel — the × says exactly that (CloseButton's title, the same string every
  // viewer shows), and nothing used to listen here: the viewers' Esc handling lives in
  // ViewerOverlay, which this COLUMN is not. It is listened for on the window because the
  // gesture must work wherever the reader's attention is inside the panel — and WebKit does not
  // focus a div on click, so by the time they press it the focus may well be on the body.
  //
  // Two rules keep it from stealing other people's keys:
  //   - an Escape somebody has already claimed is not ours (`defaultPrevented`: the composer uses
  //     Esc to leave the input — where /review leaves the focus — the @-picker and "/" palette to
  //     close, App's menus and modals to dismiss). A modal surface that is up claims it even
  //     earlier: ViewerOverlay runs in the capture phase and stops the event there, so a viewer
  //     over the panel closes first, by construction;
  //   - a focused FIELD inside the panel owns the first one: it blurs, so a half-typed note is
  //     not destroyed by the same key that closes the pane, and the next press closes.
  // The second rule is ViewerOverlay's, verbatim; the first is what makes it safe for a column
  // that shares the screen with the composer, unlike a modal.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      const el = document.activeElement
      if (el instanceof HTMLElement && panelRef.current?.contains(el) &&
        (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT' || el.isContentEditable)) {
        e.preventDefault()
        el.blur()
        return
      }
      e.preventDefault()
      onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const dragRef = useRef<{ x: number; w: number } | null>(null)
  const detachRef = useRef<(() => void) | null>(null)
  // The panel can be closed mid-drag (⌘: the toggle) — then nothing would ever remove them.
  useEffect(() => () => detachRef.current?.(), [])

  const startDrag = (e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault()
    dragRef.current = { x: e.clientX, w: shownWidth }
    dragPxRef.current = shownWidth
    setDragging(true)
    // The listeners attach HERE, synchronously, rather than in an effect keyed on the drag
    // state: React commits that state asynchronously, and a fast drag delivers its first moves
    // before such an effect could run — the drag would start with a dead first frame (measured:
    // a synthetic down+move sent back to back moved nothing at all).
    const widthAt = (clientX: number) => {
      const d = dragRef.current
      return d ? clampPanelWidth(d.w + (d.x - clientX), room) : null
    }
    const move = (ev: PointerEvent) => {
      const px = widthAt(ev.clientX)
      if (px === null) return
      dragPxRef.current = px
      // The only thing this needs to change IS a width, so change it here rather than through a
      // re-render — see the note on `dragging` above.
      if (panelRef.current) panelRef.current.style.width = `${px}px`
    }
    const finish = (ev: PointerEvent) => {
      const px = widthAt(ev.clientX)
      dragRef.current = null
      dragPxRef.current = null
      detach()
      setDragging(false)
      // The choice goes back to React (and to disk) here, once — the render that follows writes
      // the same number it was given, and the drag never had a state update of its own.
      if (px !== null) onResizeCommit(px)
    }
    const detach = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', finish)
      window.removeEventListener('pointercancel', finish)
      // While dragging the pointer is over text: keep the selection from forming, and keep the
      // resize cursor even where the handle is not under the pointer.
      document.body.classList.remove('is-resizing')
      detachRef.current = null
    }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', finish)
    window.addEventListener('pointercancel', finish)
    document.body.classList.add('is-resizing')
    detachRef.current = detach
  }
  // Arrow keys resize too: a drag is not the only way to do this, and the handle is a real
  // focusable control (role=separator) rather than a decoration.
  const onHandleKey = (e: React.KeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowLeft' ? ONE_OFF_PANEL_KEY_STEP : e.key === 'ArrowRight' ? -ONE_OFF_PANEL_KEY_STEP : 0
    if (!step) return
    e.preventDefault()
    onResizeCommit(clampPanelWidth(shownWidth + step, room))
  }

  return (
    <aside className="oneoff-panel" aria-label="旁路运行" ref={panelRef} style={{ width: shownWidth }}>
      {/* The grab strip on the panel's edge. It sits INSIDE the panel because the panel clips
          its own overflow, and it carries the separator semantics a screen reader needs. */}
      <div className={`oneoff-resizer${dragging ? ' is-dragging' : ''}`} role="separator"
        aria-orientation="vertical" aria-label="调整旁路面板宽度" tabIndex={0}
        title="拖动调整宽度（← → 也可以）"
        onPointerDown={startDrag} onKeyDown={onHandleKey} />
      <div className="oneoff-head">
        <span className="oneoff-title">旁路</span>
        <select className="oneoff-select" value={selected} disabled={items.length === 0}
          title="本会话的所有旁路运行（评审、提交）"
          onChange={(e) => select(e.target.value)}>
          {items.length === 0 ? <option value="">（没有记录）</option> : null}
          {items.map((it) => <option key={it.name} value={it.name}>{oneOffLabel(it, busy)}</option>)}
        </select>
        <button type="button" className="oneoff-icon" title="重新读取" onClick={refresh}>⟳</button>
        <CloseButton onClose={onClose} />
      </div>

      {/* One run, three panes. The tabs only appear for the panes this run has: a commit has
          no findings and no report, and offering an empty box is worse than not offering it. */}
      {detail && !detail.notice && tabs.length > 1 ? (
        <div className="oneoff-tabs">
          {tabs.map((t) => (
            <button key={t.key} type="button"
              className={`oneoff-tab${shown === t.key ? ' active' : ''}`}
              onClick={() => (t.key === 'report' ? openReport() : setTab(t.key))}>
              {t.label}
            </button>
          ))}
        </div>
      ) : null}

      <div className="oneoff-body">
        {error ? <div className="oneoff-empty">{error}</div> : null}
        {!error && items.length === 0 ? <div className="oneoff-empty">{listLoading ? '读取中…' : note}</div> : null}
        {!error && items.length > 0 && detailLoading ? <div className="oneoff-empty">读取记录中…</div> : null}
        {!error && detail?.notice ? <div className="oneoff-empty">{detail.notice}</div> : null}

        {detail && !detail.notice ? (
          <>
            <div className="oneoff-meta">
              {live ? <span className="oneoff-live" title="这个运行还在进行中，面板每秒重读它的记录">● 运行中</span> : null}
              <span>{oneOffKindLabel(detail.header.kind)}</span>
              {detail.header.provider || detail.header.model
                ? <span>{[detail.header.provider, detail.header.model].filter(Boolean).join(' / ')}</span> : null}
              {detail.header.startedAt ? <span>开始 {fmtTime(detail.header.startedAt)}</span> : null}
              {detail.header.findings ? <span>{detail.header.findings} 条意见</span> : null}
            </div>

            {shown === 'flow' ? (
              <>
                {turns.map((m) => (
              m.role === 'user'
                ? <UserBubble key={m.id}>{m.text}</UserBubble>
                : (
                  <div className="msg msg-assistant oneoff-turn" key={m.id}>
                    <div className="msg-content">
                      <div className="turn-parts">
                        {(m.parts || []).map((p, i) => <TurnPart key={i} part={p} workDir={workDir} />)}
                      </div>
                    </div>
                  </div>
                )
            ))}

            {detail.requests?.length ? (
              <div className="oneoff-reqs">
                <div className="oneoff-req-title">本次请求 {detail.requests.length} 次（懒加载正文）</div>
                {detail.requests.map((r) => {
                  const full = requests[r.seq]
                  return (
                    <div className="oneoff-req" key={r.seq}>
                      <button type="button" className="oneoff-req-row" onClick={() => loadRequest(r.seq)}>
                        <span className="oneoff-req-seq">#{r.seq}</span>
                        <span className="oneoff-req-tools">{r.tools?.length ? `${r.tools.length} 个工具` : ''}</span>
                        <span className="oneoff-req-dur">{r.durationMs ? fmtDur(r.durationMs) : ''}</span>
                        {r.timestamp ? <span className="oneoff-req-ts">{fmtTime(r.timestamp)}</span> : null}
                      </button>
                      {full === 'loading' ? <div className="oneoff-empty">读取中…</div> : null}
                      {full && full !== 'loading' ? (
                        full.notice
                          ? <div className="oneoff-empty">{full.notice}</div>
                          : (
                            <>
                              {full.userPrompt ? (
                                <pre className="oneoff-pre" title="这一轮的用户消息（评审的作用域就在里面）">{full.userPrompt}</pre>
                              ) : null}
                              {full.systemPrompt ? (
                                <details className="oneoff-sys">
                                  <summary>system prompt</summary>
                                  <pre className="oneoff-pre">{full.systemPrompt}</pre>
                                </details>
                              ) : null}
                            </>
                          )
                      ) : null}
                    </div>
                  )
                })}
              </div>
                ) : null}
              </>
            ) : null}

            {/* The findings of THIS run against the diff they came from. The diff only exists
                when the caller knew the reviewed files (a review's own chip does); a run opened
                from the switcher says so instead of showing an empty box. */}
            {shown === 'findings' ? (
              <DiffFindingsPane diff={diff} loading={diffLoading} findings={findings} runKey={api.runKey}
                diffError={api.diffError}
                note={detail.header.findings ? undefined
                  : detail.header.report ? '评审写了报告，但没有记录结构化意见 —— 意见只在报告正文里'
                    : '这次评审没有报告问题'}
                report={detail.header.report} hasPaths={pathsKey.length > 0}
                onSend={onSend} onOpenReport={openReport}
                /* Only a run that recorded its scope can be re-run over "the same files": a
                   typed /review covers the whole tree and has nothing to name. */
                onRerun={detail.header.paths?.length && onRerun
                  ? () => onRerun(detail.header.paths || [], detail.header.reviewedMsg || '')
                  : undefined} />
            ) : null}

            {/* The report, in place: the narrative behind the findings, rendered with the app's
                own markdown pipeline rather than in another overlay. */}
            {shown === 'report' ? (
              reportText === null ? (
                <div className="oneoff-empty">读取报告…</div>
              ) : reportText === '' ? (
                <div className="oneoff-empty">报告是空的</div>
              ) : (
                <>
                  <div className="oneoff-meta">
                    <span title={detail.header.report}>{(detail.header.report || '').split('/').pop()}</span>
                    <button type="button" className="oneoff-req-row oneoff-open-report" title="用系统默认应用打开"
                      onClick={() => { if (detail.header.report) AgentService.OpenPath(detail.header.report).catch(() => {}) }}>
                      打开
                    </button>
                  </div>
                  <MarkdownBlock text={reportText} workDir={workDir} />
                </>
              )
            ) : null}
          </>
        ) : null}
      </div>
    </aside>
  )
}
