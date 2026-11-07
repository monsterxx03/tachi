// 远端机器页：桌面作为另一台宿主的客户端。
//
// 一件事先定下来：**一次只看一台机器**。切到哪台，左列就只列那台的会话 —— 两边的会话
// id 不一样、同名标题也会撞，混在一个列表里「我这一点按在哪台机器上」会一直含糊。
//
// 能力是收窄的，而且收窄的方式是**不画**：文件树、diff/评审、rewind/分支、设置写入、
// MCP 编辑、@ 引用、一次性命令 —— 这些在远端都不出现，而不是留一个灰色按钮让人反复点。
// 第一次连上一台机器时说一次它不能做什么，之后可以在页面头部回看。
//
// 数据全程走手机那套接口（见 desktop/remote_*.go）：一条 WebSocket 订阅一段会话，
// 记录是权威的，实时流只延长尾巴 —— 所以一轮结束时重读一次快照，而不是自己拼历史。

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Dialogs, Events } from '@wailsio/runtime'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type {
  CronJobVO, RemoteAgentsVO, RemoteAskVO, RemoteAttachmentVO, RemoteContextPartVO, RemoteCronRowVO, RemoteHostVO, RemoteMCPRowVO, RemoteMessageVO, RemotePanelsVO, RemoteProviderVO, RemoteSessionVO, RemoteSnapshotVO, RemoteUploadVO,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { Question } from '../bindings/github.com/monsterxx03/tachi/agent/tools'
import { actOnKey, buildTurns, copyText, fmtCredit, fmtDur, fmtShare, fmtTime, humanize, imeActive, tpsTier } from './lib'
import { IS_MAC, MOD_KEY, isMod } from './platform'
import type { Message, PermissionDecision, PermissionRequest } from './types'
import type { SessionMessage } from './types'
import { AgentsIcon, ApprovalSelect, AskForm, CacheRing, Caret, ContextRing, MCPIcon, PermissionForm, PinIcon, ProcessStrip, ProviderPicker, ToolCard, UserBubble } from './components'
import { ComposerControls, ComposerStatus, PendingBar } from './composer'
import { CronPanel } from './cron'
import { usePopoverPosition } from './popover'
import { TurnPart } from './parts'
import { appendPartDelta, finishNotice, finishToolPart, processLiveText, pushPart, turnView, updateToolPart, type IndexedPart } from './transcript'
import { Icon, ICON_MD, ICON_SM, ICON_XS } from './icons'
import { ArrowLeft, Clock, Paperclip, PenLine, Plus, Trash2 } from 'lucide-react'

// The machine this window was last looking at. It is a UI preference, not state: it only has
// to survive a restart, and localStorage is the place for "only the page reads it".
const LAST_HOST_KEY = 'tachi.remoteHost'

// 一条提示停留多久。它和窗口里那个操作提示同一条规矩：一句「已保存到…」或一次拒绝都只说明
// 刚刚发生了什么，没有理由一直压在对话上面。点一下仍然可以立刻收掉。
const NOTICE_MS = 4000

// The protocol's refusal codes, in the words a reader can act on. They are the SAME sentences
// the phone shows (mobile/src/screens/…) — one protocol, one meaning; this table is only where
// this frontend keeps its copy.
const REMOTE_ERRORS: Record<string, string> = {
  unauthorized: '令牌已失效，在远端机器上重新配对',
  no_session: '这段对话已经不在了',
  turn_running: '它正在跑；等它结束，或者按停止',
  not_running: '这一轮已经结束了，直接发送',
  steer_unsupported: '这一轮不接受插话，等它结束或停止',
  no_such_pending_request: '已经在别处回答过了',
  read_only: '这段对话由别的任务占用，只能看',
  held_by_task: '有一项远端任务正在使用它',
  channel_offline: '那台机器现在没有跑那个渠道',
  channel_command: '这类命令要在原来的渠道里发',
  command_desktop_only: '这类命令要在那台机器的桌面窗口里运行',
  command_refused: '这条命令没能执行',
  agent_unavailable: '那台机器还没配好模型',
  no_project: '项目已经不在了（或目录不可用）',
  bad_working_dir: '那个目录用不了',
  cron_disabled: '定时任务在那台机器的设置里被关掉了',
  not_desktop: '这个任务不归那台机器管',
  bad_request: '请求不对（内容不能为空）',
}

function remoteErrorText(code: string): string {
  if (!code) return ''
  return REMOTE_ERRORS[code] || code
}

// hostAddr is the line under a machine's name: the address it will dial FIRST, without the
// scheme. The version beside it used to be what stood there, and it is a constant ("0.1") in
// every build — so the row said the same thing twice and answered nothing. WHERE a machine is
// reached is the fact that tells two rows apart (LAN vs tailnet) and the one that goes stale.
function hostAddr(h: RemoteHostVO): string {
  const u = h.urls?.[0] || ''
  if (!u) return ''
  try {
    return new URL(u).host
  } catch {
    return u.replace(/^https?:\/\//, '')
  }
}

// pairPayload turns the add dialog's two fields into the one line the backend pairs from. It
// reads JSON as well as copied prose, and an address typed WITHOUT a scheme ("nas:8787") is the
// common case — a bare host:port would not be picked out of that prose, so it gets one here.
// http is the default: a headless host prints an http address for the LAN, and a reader who
// serves it over TLS types the whole URL.
function pairPayload(addr: string, token: string): string {
  const trimmed = addr.trim().replace(/\/+$/, '')
  const url = /^https?:\/\//i.test(trimmed) ? trimmed : 'http://' + trimmed
  return JSON.stringify({ urls: [url], token: token.trim() })
}

// ── The live turn ────────────────────────────────────────────────────────────
// A turn that is still being written. It is ONE Message so the pieces keep their real order
// (thinking, a tool card, more prose) through the same helpers the local transcript uses.
// When the turn ends the snapshot replaces it — the record is the truth.

function openLive(): Message {
  return { id: 'remote-live', role: 'assistant', running: true, parts: [], ts: new Date().toISOString() }
}

// A turn that has started but has not produced anything yet (the window was opened mid-turn).
// The strip is still the status line, so it has to exist with nothing in it.
function emptyLiveMessage(): Message {
  return { id: 'remote-live-empty', role: 'assistant', running: true, parts: [] }
}

function livePart(m: Message | null, fn: (m: Message) => Message): Message {
  return fn(m ?? openLive())
}

type RemoteLive = { msg: Message; state: string; error: string }

// QueuedMsg is one message waiting for a steer point. The attachments travel WITH the text they
// were attached to — a queue item is one message, and the files it names belong to it.
type QueuedMsg = { text: string; attachments: string[] }

// queueText / queueFiles: what «立即发送» and the idle flush hand over is ONE message made of the
// queue. Text is joined the way the native composer joins it; attachments are the union (each name
// is already stored on the other machine, so duplicates only matter if the same file is queued twice).
function queueText(items: QueuedMsg[]): string {
  return items.map((q) => q.text).filter((t) => t !== '').join('\n\n')
}
function queueFiles(items: QueuedMsg[]): string[] {
  const out: string[] = []
  for (const it of items) out.push(...it.attachments)
  return out
}
// pendingDisplay is one row of the queue bar: the text, or the files when the message is files only.
function pendingDisplay(q: QueuedMsg): string {
  if (q.text) return q.text
  return q.attachments.map((n) => '📎 ' + n).join('  ')
}

// useElapsed ticks once a second while a remote turn is running. The strip's number answers
// "is it working or stuck"; without it a long tool call looks the same as a hung one.
function useElapsed(ts: string | undefined, running: boolean): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!running) return
    setNow(Date.now())
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [running])
  if (!running || !ts) return 0
  const start = Date.parse(ts)
  return Number.isNaN(start) ? 0 : Math.max(0, now - start)
}

// ── The other machine's three panels ──────────────────────────────────────────
// MCP and Agents are read-only lists of their own rather than the native panels: the protocol
// hands over COUNTS for MCP (connected, how many of a server's tools this conversation loaded)
// and a one-line summary per node, so mounting the full panels would draw empty expanders and a
// 刷新 button with nothing behind it. The CSS classes are the native ones, so they LOOK the same.
// The scheduled jobs DO reuse the native CronPanel: its data is complete, and 跑一次 / 暂停 /
// 删除 are three actions the protocol already offers.

function RemoteMCPPanel({ rows, loading, onClose, floating }: {
  rows: RemoteMCPRowVO[]
  loading: boolean
  onClose: () => void
  floating: ReturnType<typeof usePopoverPosition>
}) {
  return (
    <div className="popover-panel mcp-panel" ref={floating.panelRef} style={floating.style}>
      <div className="mcp-head">
        <span className="mcp-head-text">
          <span className="mcp-title">MCP 服务器</span>
          <span className="mcp-total">{rows.filter((r) => r.connected).length}/{rows.length} 已连接 · 只读</span>
        </span>
        <button className="mcp-close" onClick={onClose} title="关闭" aria-label="关闭">✕</button>
      </div>
      <div className="mcp-body">
        {loading && rows.length === 0 ? <div className="mcp-empty">正在读取…</div> : null}
        {!loading && rows.length === 0 ? <div className="mcp-empty">那台机器上没有 MCP 服务器</div> : null}
        {rows.map((r) => (
          <div className="mcp-server" key={r.name}>
            <div className="mcp-server-row" style={{ cursor: 'default' }}>
              <span className={`mcp-server-state ${r.connected ? 'on' : 'off'}`}>{r.connected ? '●' : '○'}</span>
              <span className="mcp-server-name">{r.name}</span>
              <span className="mcp-toolcount">
                {r.needsAuth ? '需要授权' : r.connected ? `${r.loaded}/${r.tools} 工具` : '未连接'}
              </span>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

function RemoteAgentsPanel({ panel, loading, onClose, floating }: {
  panel: RemoteAgentsVO | null
  loading: boolean
  onClose: () => void
  floating: ReturnType<typeof usePopoverPosition>
}) {
  const [open, setOpen] = useState<Record<string, boolean>>({})
  const peers = panel?.peers || []
  return (
    <div className="popover-panel mcp-panel agents-panel" ref={floating.panelRef} style={floating.style}>
      <div className="mcp-head">
        <span className="mcp-head-text">
          <span className="mcp-title">Agents</span>
          <span className="mcp-total">
            {panel?.anyChecked ? `${panel.checkedAt} · 只读` : '还没检查过'}
          </span>
        </span>
        <button className="mcp-close" onClick={onClose} title="关闭" aria-label="关闭">✕</button>
      </div>
      <div className="mcp-body">
        {loading && peers.length === 0 ? <div className="mcp-empty">正在读取…</div> : null}
        {!loading && peers.length === 0 ? <div className="mcp-empty">那台机器上没有可委派的节点</div> : null}
        {peers.map((p) => {
          const key = p.id || p.name
          const isOpen = !!open[key]
          const up = p.reach === '可用'
          return (
            <div className="mcp-server" key={key}>
              <div className="mcp-server-row" onClick={() => setOpen((prev) => ({ ...prev, [key]: !isOpen }))}>
                <span className={`mcp-server-state ${up ? 'on' : ''}`}>{up ? '●' : '·'}</span>
                <span className="mcp-server-name">{p.name}</span>
                {p.label && p.label !== p.name ? <span className="agents-id">{p.label}</span> : null}
                <span className="mcp-toolcount">{p.reach}</span>
                <span className="mcp-caret"><Caret open={isOpen} /></span>
              </div>
              {isOpen ? (
                <div className="agents-detail">
                  <div className="agents-line"><span className="agents-k">地址</span><span className="agents-v agents-mono">{p.url}</span></div>
                  <div className="agents-line"><span className="agents-k">身份</span><span className="agents-v agents-mono">{p.id ? p.id.slice(0, 8) : '未记录'}</span></div>
                  {p.note ? <div className="agents-line"><span className="agents-k">备注</span><span className="agents-v">{p.note}</span></div> : null}
                  {p.skills?.length ? <div className="agents-line"><span className="agents-k">自称会</span><span className="agents-v">{p.skills.join('、')}</span></div> : null}
                  {p.err ? <div className="agents-line"><span className="agents-k">原因</span><span className="agents-v">{p.err}</span></div> : null}
                </div>
              ) : null}
            </div>
          )
        })}
      </div>
    </div>
  )
}

// RemoteContext is the context ring, fed by what the OTHER machine told us. It is not the native
// ContextMeter: that one asks this app's AgentService for the breakdown when it opens, which here
// would describe a conversation on the wrong machine. The protocol already carries the total, the
// window and the buckets (with labels but without the native colour keys), so the ring reuses the
// same SVG and the popover lists the buckets plainly.
function RemoteContext({ ctx }: { ctx: { estimate: number; window: number; parts: RemoteContextPartVO[] } }) {
  const [open, setOpen] = useState(false)
  const pos = usePopoverPosition(open)
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t || pos.isInside(t) || t.closest('.ctx-btn')) return
      setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [open, pos.isInside])
  if (ctx.window <= 0 || ctx.estimate <= 0) return null
  const pct = Math.min(100, (ctx.estimate / ctx.window) * 100)
  return (
    <span className="popover-anchor" ref={pos.anchorRef}>
      {open ? (
        <div className="popover-panel ctx-panel" ref={pos.panelRef} style={pos.style}>
          <div className="ctx-head">
            <span className="ctx-title">上下文占用</span>
            <span className="ctx-total">{humanize(ctx.estimate)} / {humanize(ctx.window)}</span>
          </div>
          <div className="ctx-sub">{pct.toFixed(1)}% 的上下文窗口</div>
          {ctx.parts.length > 0 ? (
            <div className="ctx-rows">
              {ctx.parts.map((p, i) => (
                <div className="ctx-row" key={`${p.label}-${i}`}>
                  <span className="ctx-name">{p.label}</span>
                  <span className="ctx-tokens">{humanize(p.tokens)}</span>
                  <span className="ctx-pct">{fmtShare(p.tokens / Math.max(1, ctx.estimate))}</span>
                </div>
              ))}
            </div>
          ) : null}
          {/* One caveat that belongs to this view: the number is the LAST thing that machine
              measured — the protocol has no per-call context event, so it moves at turn ends. */}
          <div className="ctx-foot">来自那台机器最近一次测量；一轮结束时重读</div>
        </div>
      ) : null}
      <button type="button" className="ctx-btn" aria-expanded={open}
        aria-label={`上下文 ${pct.toFixed(1)}%`}
        title={`上下文 ${pct.toFixed(1)}%（${humanize(ctx.estimate)} / ${humanize(ctx.window)}）`}
        onClick={() => setOpen((v) => !v)}>
        <ContextRing estimate={ctx.estimate} window={ctx.window} />
      </button>
    </span>
  )
}

// RemoteTurn renders ONE completed turn the way the local transcript does: thinking, tool
// calls and mid-turn prose fold into one strip (turnView), the conclusion stays visible, and
// the things that must never be hidden — a failed call, the call a permission card is parked
// on, the call AskUserQuestion is waiting on — stay outside the fold whether or not it is open.
//
// It is the same splitter and the same strip the native transcript uses, so the two views
// answer "how much happened" with the same words instead of one showing a flat list of steps.
function RemoteTurn({ m, workDir, running, perm, questions, attachments, onDownload, stat, onPermAnswer, onAskAnswer, onAskDecline, onFold }: {
  m: Message
  workDir: string
  running?: boolean
  perm?: PermissionRequest | null
  questions?: Question[]
  attachments?: RemoteAttachmentVO[]
  onDownload?: (att: RemoteAttachmentVO) => void
  /** 这一轮的耗时与迭代次数。只有刚跑完的那一轮有：历史记录里协议不保存它们。 */
  stat?: { durationMs: number; iterations: number }
  onPermAnswer?: (toolId: string, decision: PermissionDecision) => void
  onAskAnswer?: (answers: Record<string, string>) => void
  onAskDecline?: () => void
  onFold?: () => void
}) {
  const [open, setOpen] = useState(false)
  const view = turnView(m.parts, {
    permissionToolCallId: perm?.toolId,
    pendingAsk: !!(questions && questions.length > 0),
  })
  const elapsedMs = useElapsed(m.ts, !!running)
  // The fold changes the height of a bottom-pinned transcript, so the pin has to be re-applied
  // in the SAME layout pass (never with rAF: an occluded window stops delivering frames).
  const settled = useRef(false)
  useLayoutEffect(() => {
    if (!settled.current) {
      settled.current = true
      return
    }
    onFold?.()
  }, [open])

  let permShown = false
  let askShown = false
  const renderPart = (it: IndexedPart, key: string) => {
    const p = it.part
    if (perm && onPermAnswer && !permShown && p.type === 'tool' && !p.done && p.toolCallId === perm.toolId) {
      permShown = true
      return <PermissionForm key={key} perm={{ req: perm }} onAnswer={(d) => onPermAnswer(perm.toolId, d)} />
    }
    if (questions && questions.length > 0 && onAskAnswer && !askShown && p.type === 'tool' && !p.done && p.name === 'AskUserQuestion') {
      askShown = true
      return <AskForm key={key} questions={questions} onSubmit={onAskAnswer} onCancel={() => onAskDecline?.()} />
    }
    // A SendFile card points at a path on the OTHER machine; the local file card would open
    // nothing (or the wrong file). When the snapshot knows the attachment (it lists every delivery
    // with the file's size), the row is a download; otherwise it says what it is and stops there.
    if (p.type === 'tool' && p.name === 'SendFile') {
      const att = attachments?.find((a) => a.id === p.toolCallId)
      if (att) {
        const can = att.downloadable && !!onDownload
        return (
          <button key={key} type="button" className="remote-file-row" disabled={!can}
            title={can ? '下载到本机' : '那个文件已经不在了，或者超出了那台机器的上限'}
            onClick={() => { if (can) onDownload?.(att) }}>
            <Icon as={Paperclip} size={ICON_XS} />
            <span className="remote-file-row-name">{att.name}</span>
            <span className="remote-file-row-action">{can ? '下载' : '不可下载'}</span>
          </button>
        )
      }
      return <ToolCard key={key} name="SendFile" title={p.title || p.args || ''} args="" summary="文件在那台机器上"
        ok={!!p.ok} done={p.done} durationMs={p.durationMs} />
    }
    return <TurnPart key={key} part={p} workDir={workDir} />
  }
  const statText = stat
    ? [stat.durationMs > 0 ? `⏱ ${fmtDur(stat.durationMs)}` : '', stat.iterations > 0 ? `${stat.iterations} iters` : '']
      .filter(Boolean).join(' · ')
    : ''
  const footer = (view.conclusion || statText)
    ? (
      <div className="msg-footer">
        {statText ? <span className="msg-ts" title="这一轮的耗时与迭代次数（只有刚结束的那一轮有）">{statText}</span> : null}
        {m.ts ? <span className="msg-ts">{fmtTime(m.ts)}</span> : null}
      </div>
    )
    : null
  return (
    <div className="msg msg-assistant">
      <div className="msg-avatar"><img src="/agent-avatar.png" alt="" draggable={false} /></div>
      <div className="msg-content">
        <div className="turn-parts">
          {view.folded.length > 0 || running ? (
            <div className="process-block">
              <ProcessStrip summary={view.summary} open={open} onToggle={() => setOpen((v) => !v)}
                foldable={view.folded.length > 0} running={running} elapsedMs={elapsedMs}
                status={running ? processLiveText(view.live, view.phase, view.unfinished) : undefined} />
              {open && view.folded.length > 0 ? (
                <div className="process-timeline">
                  {view.folded.map((it) => renderPart(it, `f${it.index}`))}
                </div>
              ) : null}
            </div>
          ) : null}
          {[...view.exposed, ...(view.conclusion ? [view.conclusion] : [])]
            .sort((a, b) => a.index - b.index)
            .map((it) => renderPart(it, `v${it.index}`))}
          {view.attachments.map((it) => renderPart(it, `a${it.index}`))}
        </div>
        {footer}
      </div>
    </div>
  )
}

// turnEndText: what a finished turn says when it did not end cleanly. A user's stop is not an
// error — same distinction the backend makes — so only a real failure gets the ⚠ line.
function turnEndText(ev: Record<string, unknown>): string {
  const end = String(ev.end || '')
  if (end === 'interrupted') return '这一轮被停掉了'
  if (end === 'error') return '这一轮出错了'
  return ''
}

export function RemotePage({ onBack }: { onBack: () => void }) {
  const [hosts, setHosts] = useState<RemoteHostVO[]>([])
  const [hostID, setHostID] = useState('')
  const [sessions, setSessions] = useState<RemoteSessionVO[]>([])
  const [listError, setListError] = useState('')
  const [loadingList, setLoadingList] = useState(false)

  const [openID, setOpenID] = useState('')
  const [snapshot, setSnapshot] = useState<RemoteSnapshotVO | null>(null)
  const [messages, setMessages] = useState<RemoteMessageVO[]>([])
  const [hasMore, setHasMore] = useState(false)
  const [loadingMore, setLoadingMore] = useState(false)
  const [openError, setOpenError] = useState('')
  const [opening, setOpening] = useState(false)

  const [live, setLive] = useState<RemoteLive | null>(null)
  const [asks, setAsks] = useState<RemoteAskVO[]>([])
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  // flashNotice shows one line and takes it away by itself. Every notice goes through it, so a
  // sentence can never stay on screen forever just because nobody clicked it.
  const noticeTimer = useRef(0)
  const flashNotice = useCallback((text: string) => {
    setNotice(text)
    window.clearTimeout(noticeTimer.current)
    if (!text) return
    noticeTimer.current = window.setTimeout(() => setNotice(''), NOTICE_MS)
  }, [])
  useEffect(() => () => window.clearTimeout(noticeTimer.current), [])
  const [connected, setConnected] = useState(true)
  // 后端在退避重连时报到第几次；0 = 没在重连（断开后放弃，或还没断过）。
  const [reconnectAttempt, setReconnectAttempt] = useState(0)

  const [providers, setProviders] = useState<RemoteProviderVO[]>([])
  const [model, setModel] = useState('')
  // 这段对话在那台机器上的数字：成本 / 积分 / 缓存命中、上下文、实时速率，以及刚结束那一轮的
  // 耗时与迭代次数。它们和窗口标题栏里那五个是同一组事实，来源不同（协议只有会话级的 cost
  // 事件和每轮结束时的 turn 事件）。
  const [usage, setUsage] = useState({ cost: 0, credit: 0, cacheHitRate: 0 })
  const [ctx, setCtx] = useState<{ estimate: number; window: number; parts: RemoteContextPartVO[] }>({ estimate: 0, window: 0, parts: [] })
  const [tps, setTps] = useState({ now: 0, last: 0 })
  const [turnStat, setTurnStat] = useState<{ durationMs: number; iterations: number } | null>(null)
  // 对面那台机器的三个面板（按会话读）：MCP 连接状态、可委派节点、定时任务。
  const [panels, setPanels] = useState<RemotePanelsVO | null>(null)
  const [panelsLoading, setPanelsLoading] = useState(false)
  const [panelNotice, setPanelNotice] = useState('')
  const [openPanel, setOpenPanel] = useState<'mcp' | 'agents' | 'cron' | ''>('')
  const mcpPop = usePopoverPosition(openPanel === 'mcp')
  const agentsPop = usePopoverPosition(openPanel === 'agents')
  const cronPop = usePopoverPosition(openPanel === 'cron')
  const [skipApproval, setSkipApproval] = useState(false)

  const [draft, setDraft] = useState('')
  const [sending, setSending] = useState(false)
  // 已经传到那台机器、还没随消息发出去的文件。名字才是发送时要递的东西（协议如此）。
  const [files, setFiles] = useState<RemoteUploadVO[]>([])
  const [uploading, setUploading] = useState(false)
  // The steer queue. It is LOCAL, exactly like the native composer's: text typed while a turn
  // runs is not sent and forgotten — it is held here, one item is handed over at the next tool
  // boundary (a `tool_result`), and whatever is left is sent as the next message when the turn
  // ends. Sending them straight through would also be consumed at a boundary, but the protocol
  // has no "the agent is parked at a steer point" event, so a client that fires immediately
  // cannot show the reader what is still waiting — and cannot put it back when the turn refuses.
  const [pending, setPending] = useState<QueuedMsg[]>([])
  const pendingRef = useRef<QueuedMsg[]>([])
  const setQueue = useCallback((next: QueuedMsg[]) => {
    pendingRef.current = next
    setPending(next)
  }, [])
  const steeringRef = useRef(false)
  const busyRef = useRef(false)
  busyRef.current = busy

  const [addOpen, setAddOpen] = useState(false)
  // 配对只需要两样东西，所以是两行、不是一个粘贴框：地址和令牌是那两个问题的答案，读者
  // 照着无头宿主打印的那两行抄，不用先弄清楚「整段要粘多少」。
  const [pairAddr, setPairAddr] = useState('')
  const [pairToken, setPairToken] = useState('')
  const [addError, setAddError] = useState('')
  const [adding, setAdding] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState('')
  // 右键菜单、删除确认、两处就地改名（会话 / 机器）——与原生侧栏同一套手势。
  const [menu, setMenu] = useState<{ kind: 'session' | 'host'; id: string; x: number; y: number } | null>(null)
  const [confirmDel, setConfirmDel] = useState<{ sid: string; title: string; cron: number; error?: string } | null>(null)
  const confirmBoxRef = useRef<HTMLDivElement>(null)
  const [editingId, setEditingId] = useState('')
  const [editTitle, setEditTitle] = useState('')
  const [renamingHost, setRenamingHost] = useState('')
  const [hostName, setHostName] = useState('')
  const [creating, setCreating] = useState(false)
  const creatingRef = useRef(false)

  // Refs read by the event handler: it is installed once, and must always answer about the
  // machine/session on screen NOW, not the one that was open when it was registered.
  const hostRef = useRef('')
  const openRef = useRef('')
  hostRef.current = hostID
  openRef.current = openID

  useLayoutEffect(() => {
    if (confirmDel) confirmBoxRef.current?.focus()
  }, [confirmDel])

  const scrollerRef = useRef<HTMLDivElement>(null)
  const stickRef = useRef(true)
  const inputRef = useRef<HTMLTextAreaElement>(null)

  const host = hosts.find((h) => h.id === hostID)
  const current = sessions.find((s) => s.id === openID)
  const workDir = snapshot?.workspace.primary || ''
  const title = current?.title || snapshot?.session.title || ''

  // ── Machines ───────────────────────────────────────────────────────────────
  const loadHosts = useCallback(async () => {
    const list = (await AgentService.ListRemoteHosts().catch(() => null)) || []
    setHosts(list)
    return list
  }, [])

  useEffect(() => {
    void (async () => {
      const list = await loadHosts()
      const remembered = localStorage.getItem(LAST_HOST_KEY) || ''
      const pick = list.find((h) => h.id === remembered) || list[0]
      if (pick) setHostID(pick.id)
    })()
  }, [loadHosts])

  const refreshSessions = useCallback(async (id: string) => {
    if (!id) return
    setLoadingList(true)
    const [list, why] = await AgentService.RemoteSessions(id).catch(() => [null, '连不上'] as [null, string])
    setLoadingList(false)
    if (why) {
      setListError(remoteErrorText(why))
      return
    }
    setListError('')
    setSessions(list || [])
  }, [])

  // refreshPanels re-reads the three panels for the conversation on screen. They are a READ on
  // the other machine (it builds nothing, it forces no probe), so the page can ask freely — and
  // `cron_changed` is the event that says the answer moved.
  const refreshPanels = useCallback(async () => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) {
      setPanels(null)
      return
    }
    setPanelsLoading(true)
    const [vo, why] = await AgentService.RemotePanels(hostNow, id).catch(() => [null, '读不到面板'] as [null, string])
    setPanelsLoading(false)
    if (why || !vo) {
      if (openPanel !== '') setPanelNotice(remoteErrorText(why || '读不到面板'))
      return
    }
    setPanels(vo)
  }, [openPanel])

  useEffect(() => {
    if (!openID) {
      setPanels(null)
      return
    }
    void refreshPanels()
  }, [openID, refreshPanels])

  // Switching machines closes the old subscription and drops everything that belonged to it:
  // a session id only means something on the machine it came from.
  const selectHost = useCallback((id: string) => {
    if (id === hostRef.current) return
    const old = hostRef.current
    if (old) AgentService.RemoteDisconnect(old).catch(() => {})
    hostRef.current = id
    setHostID(id)
    localStorage.setItem(LAST_HOST_KEY, id)
    setSessions([])
    setOpenID('')
    setSnapshot(null)
    setMessages([])
    setLive(null)
    setAsks([])
    setQueue([])
    flashNotice('')
    setListError('')
    setProviders([])
    setConnected(true)
    setUsage({ cost: 0, credit: 0, cacheHitRate: 0 })
    setCtx({ estimate: 0, window: 0, parts: [] })
    setTps({ now: 0, last: 0 })
    setTurnStat(null)
    setReconnectAttempt(0)
  }, [])

  useEffect(() => {
    if (!hostID) return
    setConnected(true)
    void refreshSessions(hostID)
    void (async () => {
      const [list] = await AgentService.RemoteProviders(hostID).catch(() => [null, ''] as [null, string])
      setProviders(list || [])
    })()
  }, [hostID, refreshSessions])

  useEffect(() => () => {
    const id = hostRef.current
    if (id) AgentService.RemoteDisconnect(id).catch(() => {})
  }, [])

  // ── Opening a conversation ────────────────────────────────────────────────
  const applySnapshot = useCallback((snap: RemoteSnapshotVO) => {
    setSnapshot(snap)
    setMessages(snap.messages || [])
    setHasMore(snap.hasMore)
    setAsks(snap.asks || [])
    setModel(snap.model)
    setSkipApproval(snap.skipApproval)
    setBusy(snap.state.status === 'busy')
    setLive(null)
    setConnected(true)
    flashNotice('')
  }, [])

  const openSession = useCallback(async (id: string) => {
    const hostNow = hostRef.current
    if (!hostNow || !id) return
    setQueue([])
    setOpening(true)
    setOpenError('')
    flashNotice('')
    setLive(null)
    openRef.current = id
    setOpenID(id)
    const [snap, why] = await AgentService.RemoteOpenSession(hostNow, id).catch(() => [null, '连不上这台机器'] as [null, string])
    setOpening(false)
    if (why || !snap) {
      setOpenError(remoteErrorText(why || ''))
      setSnapshot(null)
      setMessages([])
      return
    }
    if (openRef.current !== id) return // 读者已经点了别的会话
    applySnapshot(snap)
  }, [applySnapshot])

  // newSession creates a conversation ON the machine being driven. It asks that machine for its
  // own default working directory (the phone's new-session endpoint), so nothing from this
  // window's folders leaks across; the row it answers with is opened right away — the list
  // refresh and this open are two reads, and the reader should not wait for both to see the
  // conversation they just asked for.
  const newSession = useCallback(async () => {
    const hostNow = hostRef.current
    if (!hostNow || creatingRef.current) return
    creatingRef.current = true
    setCreating(true)
    flashNotice('')
    const [row, why] = await AgentService.RemoteNewSession(hostNow).catch(() => [null, '连不上这台机器'] as [null, string])
    creatingRef.current = false
    setCreating(false)
    if (why || !row) {
      flashNotice(remoteErrorText(why || ''))
      return
    }
    await refreshSessions(hostNow)
    void openSession(row.id)
  }, [openSession, refreshSessions])

  // commitRename: 会话改名。名字是那台机器上的事实（它也广播 sessions_changed），这里先把
  // 行改掉，免得读者等一个来回。
  const commitRename = useCallback(async (id: string) => {
    const title = editTitle.trim()
    setEditingId('')
    setEditTitle('')
    if (!title) return
    const hostNow = hostRef.current
    if (!hostNow) return
    const why = await AgentService.RemoteRenameSession(hostNow, id, title).catch((e) => String(e))
    if (why && why !== 'ok') {
      flashNotice(remoteErrorText(why))
      return
    }
    setSessions((prev) => prev.map((s) => (s.id === id ? { ...s, title } : s)))
  }, [editTitle])

  // commitHostRename: 机器的名字只存在这一侧（手机也是这么做的），所以它不会去问那台机器。
  const commitHostRename = useCallback(async (id: string) => {
    const name = hostName.trim()
    setRenamingHost('')
    setHostName('')
    if (!name) return
    const [vo, why] = await AgentService.RenameRemoteHost(id, name).catch(() => [null, '没改成功'] as [null, string])
    if (why || !vo) {
      flashNotice(remoteErrorText(why || ''))
      return
    }
    setHosts((prev) => prev.map((h) => (h.id === id ? vo : h)))
  }, [hostName])

  // deleteSession deletes a conversation on the other machine. What the reader is handed next is
  // computed BEFORE the list changes (the row below, else the row above) — the native rule — and
  // only applies when the deleted conversation was the one on screen.
  const deleteSession = useCallback(async (id: string) => {
    const hostNow = hostRef.current
    if (!hostNow) return
    const idx = sessions.findIndex((s) => s.id === id)
    const successor = idx >= 0 ? (sessions[idx + 1]?.id || sessions[idx - 1]?.id || '') : ''
    const why = await AgentService.RemoteDeleteSession(hostNow, id).catch((e) => String(e))
    if (why && why !== 'ok') {
      setConfirmDel((c) => (c ? { ...c, error: remoteErrorText(why) } : c))
      return
    }
    setConfirmDel(null)
    await refreshSessions(hostNow)
    if (openRef.current !== id) return
    if (successor) {
      void openSession(successor)
    } else {
      openRef.current = ''
      setOpenID('')
      setSnapshot(null)
      setMessages([])
      setLive(null)
      setAsks([])
      setQueue([])
    }
  }, [openSession, refreshSessions, sessions, setQueue])

  // togglePin: 置顶是列表的事实（服务端按置顶优先排序并广播列表），这里只把行先改掉，
  // 顺序仍以重读回来的那份为准。
  const togglePin = useCallback(async (sid: string, pinned: boolean) => {
    const hostNow = hostRef.current
    if (!hostNow) return
    const why = await AgentService.RemoteSetSessionPinned(hostNow, sid, pinned).catch((e) => String(e))
    if (why && why !== 'ok') {
      flashNotice(remoteErrorText(why))
      return
    }
    setSessions((prev) => prev.map((s) => (s.id === sid ? { ...s, pinned } : s)))
    void refreshSessions(hostNow)
  }, [refreshSessions])

  // toggleNotify flips this conversation's push switch on the other machine. Only the open
  // conversation has a known value (the snapshot carries it); the event `session_prefs` keeps it
  // in step when another screen or the phone changes it.
  const toggleNotify = useCallback(async (sid: string, off: boolean) => {
    const hostNow = hostRef.current
    if (!hostNow) return
    const why = await AgentService.RemoteSetSessionNotify(hostNow, sid, off).catch((e) => String(e))
    if (why && why !== 'ok') {
      flashNotice(remoteErrorText(why))
      return
    }
    setSnapshot((prev) => (prev ? { ...prev, notifyOff: off } : prev))
  }, [])

  // askDeleteCurrent is ⌘⇧⌫: the keyboard form of the row's 删除, for the conversation on screen.
  // It never deletes anything itself — the box it opens is the row's box, so a refusal (a turn in
  // flight) lands in the same place.
  const askDeleteCurrent = useCallback(() => {
    const id = openRef.current
    if (!id) return
    const row = sessions.find((s) => s.id === id)
    setConfirmDel({ sid: id, title: row?.title || snapshot?.session.title || '未命名会话', cron: row?.cron || 0 })
  }, [sessions, snapshot])

  // reload re-reads the snapshot of the conversation on screen. That is the reconcile step the
  // protocol asks for (a record is the truth): after a turn, on lag, and after a moved session.
  const reload = useCallback(async () => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) return
    const [snap, why] = await AgentService.RemoteOpenSession(hostNow, id).catch(() => [null, '连不上'] as [null, string])
    if (why || !snap) {
      flashNotice(remoteErrorText(why || '连不上这台机器'))
      return
    }
    if (openRef.current !== id) return
    applySnapshot(snap)
  }, [applySnapshot])

  const loadOlder = useCallback(async () => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id || loadingMore || !hasMore || messages.length === 0) return
    const before = messages[0].index
    setLoadingMore(true)
    const [page, why] = await AgentService.RemotePage(hostNow, id, before, 100).catch(() => [null, '连不上'] as [null, string])
    setLoadingMore(false)
    if (why || !page) {
      flashNotice(remoteErrorText(why || '连不上这台机器'))
      return
    }
    if (openRef.current !== id) return
    setMessages((prev) => [...(page.messages || []), ...prev])
    setHasMore(page.hasMore)
    stickRef.current = false // 往上翻的时候不要把人拽回底部
  }, [hasMore, loadingMore, messages])

  // steerNext hands ONE queued message over at a tool boundary. The protocol has no steer-point
  // event, so the boundary we can see is the tool result — the same moment the native composer's
  // steer_check fires. One at a time: the agent's steer channel holds exactly one value, and a
  // second write would replace the first.
  const steerNext = useCallback(async () => {
    if (steeringRef.current) return
    const queue = pendingRef.current
    if (queue.length === 0 || !busyRef.current || !hostRef.current || !openRef.current) return
    const [first, ...rest] = queue
    steeringRef.current = true
    setQueue(rest)
    const why = await AgentService.RemoteSend(hostRef.current, openRef.current, first.text, 'steer', first.attachments).catch((e) => String(e))
    steeringRef.current = false
    if (why && why !== 'ok') {
      // 这一轮刚结束（或不受理插话）：放回队首，让它按普通消息发出去。
      setQueue([first, ...pendingRef.current])
      if (why !== 'not_running' && why !== 'steer_unsupported') flashNotice(remoteErrorText(why))
    }
  }, [setQueue])

  // flushQueue sends whatever is still queued as the NEXT message, when the turn ended on its
  // own. Same rule as the native composer: a stopped or failed turn leaves the queue for the
  // reader to review, drop, or send — it must not be swallowed by the turn's ending.
  const flushQueue = useCallback(async () => {
    const items = pendingRef.current
    if (items.length === 0 || !hostRef.current || !openRef.current) return
    setQueue([])
    const why = await AgentService.RemoteSend(hostRef.current, openRef.current, queueText(items), 'send', queueFiles(items)).catch((e) => String(e))
    if (why && why !== 'ok') {
      setQueue(items)
      flashNotice(remoteErrorText(why))
    }
  }, [setQueue])

  // sendPendingNow is 「立即发送 · 打断」: interrupt this turn and hand the queued text over as
  // the next one (mode=interrupt is the protocol's StopAndSend).
  const sendPendingNow = useCallback(async () => {
    const items = pendingRef.current
    if (items.length === 0 || !hostRef.current || !openRef.current) return
    setQueue([])
    const mode = busyRef.current ? 'interrupt' : 'send'
    const why = await AgentService.RemoteSend(hostRef.current, openRef.current, queueText(items), mode, queueFiles(items)).catch((e) => String(e))
    if (why && why !== 'ok') {
      setQueue(items)
      flashNotice(remoteErrorText(why))
    }
  }, [setQueue])

  // ── Live events ───────────────────────────────────────────────────────────
  useEffect(() => {
    const off = Events.On('remote:event', (event) => {
      const ev = event?.data as Record<string, any> | null
      if (!ev || ev.hostId !== hostRef.current) return
      const onScreen = !ev.sessionId || ev.sessionId === openRef.current
      switch (ev.type) {
        case 'disconnected':
          setConnected(false)
          setBusy(false)
          flashNotice('连接断开了：' + (ev.message || ''));
          return
        case 'sessions_changed':
          void refreshSessions(hostRef.current)
          return
        case 'cron_changed':
          void refreshPanels()
          return
        case 'session_state': {
          const id = String(ev.sessionId || '')
          const running = !!ev.running
          const waiting = String(ev.waiting || '')
          setSessions((prev) => prev.map((s) => (s.id === id ? { ...s, running, waiting } : s)))
          if (id === openRef.current) setBusy(running)
          return
        }
        case 'lagged':
          void reload()
          return
      }
      if (!onScreen) return
      switch (ev.type) {
        case 'text':
          setLive((l) => ({ msg: livePart(l?.msg ?? null, (m) => appendPartDelta(m, 'text', String(ev.text || ''))), state: l?.state || '', error: '' }))
          return
        case 'thinking':
          setLive((l) => ({ msg: livePart(l?.msg ?? null, (m) => appendPartDelta(m, 'thinking', String(ev.text || ''))), state: l?.state || '', error: '' }))
          return
        case 'tool_start':
          setLive((l) => ({
            msg: livePart(l?.msg ?? null, (m) => pushPart(m, {
              type: 'tool', name: String(ev.toolName || ''), title: String(ev.title || ''),
              args: '', summary: '执行中…', ok: true, done: false,
              toolCallId: String(ev.toolId || ''), expand: !!ev.autoExpand,
            })),
            state: l?.state || '', error: '',
          }))
          return
        case 'tool_args':
          setLive((l) => l ? ({
            ...l,
            msg: updateToolPart(l.msg, String(ev.toolId || ''), '', '', String(ev.args || '')),
          }) : l)
          return
        case 'tool_result':
          setLive((l) => l ? ({
            ...l,
            msg: finishToolPart(l.msg, String(ev.toolId || ''), '', String(ev.result || '') + (ev.truncated ? '\n…（远端截断）' : ''), !ev.isError),
          }) : l)
          // 工具边界：一张卡刚结束，正是插话点。队列里的话在这里交出去。
          void steerNext()
          return
        case 'state': {
          const st = ev.state || {}
          setBusy(st.status === 'busy')
          return
        }
        case 'ask':
          if (ev.ask) setAsks((prev) => {
            const a = ev.ask as RemoteAskVO
            return prev.some((x) => x.toolId === a.toolId) ? prev : [...prev, a]
          })
          return
        case 'ask_answered':
          setAsks((prev) => prev.filter((a) => a.toolId !== String(ev.toolId || '')))
          return
        case 'user_message': {
          // 另一块屏（或这一条自己的回声）发来的话。不做乐观气泡：记录是权威的，回声就是
          // 那条记录到达的第一个时刻。
          const text = String(ev.displayText || ev.text || '')
          if (!text) return
          setMessages((prev) => [...prev, { index: -1 - prev.length, role: 'user', content: String(ev.text || text), displayContent: text }])
          stickRef.current = true
          return
        }
        case 'session_prefs':
          if (ev.provider) setModel(String(ev.provider))
          setSkipApproval(!!ev.skipApproval)
          setSnapshot((prev) => (prev ? { ...prev, notifyOff: !!ev.notifyOff } : prev))
          return
        case 'compact_start':
          setLive((l) => ({
            msg: livePart(l?.msg ?? null, (m) => ({ ...m, parts: [...(m.parts || []), { type: 'notice', label: '正在压缩上下文…', done: false }] })),
            state: l?.state || '', error: '',
          }))
          return
        case 'compact_done': {
          const label = ev.error ? '压缩失败：' + String(ev.error) : '上下文已压缩'
          setLive((l) => l ? ({ ...l, msg: finishNotice(l.msg, label) }) : l)
          return
        }
        case 'turn': {
          const end = turnEndText(ev)
          if (end) flashNotice(end)
          // 这一轮的数字：耗时与迭代次数。历史记录里没有它们（协议不存），所以只留给最近这一轮，
          // 贴在最后一张回答卡上 —— 与其给每一轮编一个假数字，不如说清楚只有刚跑完的那一轮有。
          if (typeof ev.durationMs === 'number' || typeof ev.iterations === 'number') {
            setTurnStat({ durationMs: Number(ev.durationMs) || 0, iterations: Number(ev.iterations) || 0 })
          }
          setTps((t) => ({ now: 0, last: t.last }))
          return
        }
        case 'cost':
          setUsage({
            cost: Number(ev.cost) || 0,
            credit: Number(ev.credit) || 0,
            cacheHitRate: Number(ev.cacheHitRate) || 0,
          })
          return
        case 'tps':
          setTps({ now: Number(ev.tps) || 0, last: Number(ev.lastTps) || 0 })
          return
        case 'error':
          flashNotice(String(ev.message || '出错了'))
          setBusy(false)
          return
        case 'idle':
          setBusy(false)
          setTps((t) => ({ now: 0, last: t.last }))
          // 自然结束：队里还剩的话作为下一条消息发出去（被停掉/出错的那一轮不吞队列，
          // 与本地输入框同一条规则）。
          if (!ev.reason || ev.reason === 'complete') void flushQueue()
          // 这一轮结束了：重读一次快照，把实时拼出来的那一份换成记录里的。
          void reload()
          void refreshSessions(hostRef.current)
          return
        case 'session_moved':
          if (ev.to) {
            openRef.current = String(ev.to)
            setOpenID(String(ev.to))
            void reload()
            void refreshSessions(hostRef.current)
          }
          return
      }
    })
    return () => off?.()
  }, [applySnapshot, flushQueue, refreshPanels, reload, refreshSessions, steerNext])

  // ── Scroll ────────────────────────────────────────────────────────────────
  const onScroll = useCallback(() => {
    const el = scrollerRef.current
    if (!el) return
    stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
    if (el.scrollTop < 40 && hasMore && !loadingMore) void loadOlder()
  }, [hasMore, loadOlder, loadingMore])

  useLayoutEffect(() => {
    const el = scrollerRef.current
    if (el && stickRef.current) el.scrollTop = el.scrollHeight
  }, [messages, live, asks, opening, openError])

  // ── Sending ───────────────────────────────────────────────────────────────
  const send = useCallback(async () => {
    const text = draft.trim()
    const uploaded = files
    if ((!text && uploaded.length === 0) || !hostRef.current || !openRef.current) return
    const msg: QueuedMsg = { text, attachments: uploaded.map((f) => f.name) }
    setDraft('')
    setFiles([])
    flashNotice('')
    // 正在跑：进队列，等工具边界。空闲：直接开这一轮。
    if (busyRef.current) {
      setQueue([...pendingRef.current, msg])
      stickRef.current = true
      inputRef.current?.focus()
      return
    }
    setSending(true)
    const why = await AgentService.RemoteSend(hostRef.current, openRef.current, msg.text, 'send', msg.attachments).catch((e) => String(e))
    setSending(false)
    if (why && why !== 'ok') {
      setQueue([...pendingRef.current, msg])
      flashNotice(remoteErrorText(why))
      return
    }
    stickRef.current = true
    inputRef.current?.focus()
  }, [draft, files, setQueue])

  // attachFiles uploads local files to the session on the other machine. The bytes are read HERE
  // (the picker names paths on this machine) and land in that session's own uploads directory;
  // what comes back is a NAME, which is all the send request will ever cite.
  const attachFiles = useCallback(async () => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id || uploading) return
    let picked: string | string[] = ''
    try {
      picked = await Dialogs.OpenFile({ Title: '发送文件到这台机器上的这段会话', AllowsMultipleSelection: true })
    } catch {
      return
    }
    const paths = Array.isArray(picked) ? picked : picked ? [picked] : []
    if (paths.length === 0) return
    setUploading(true)
    const added: RemoteUploadVO[] = []
    for (const p of paths) {
      const [vo, why] = await AgentService.RemoteUploadFile(hostNow, id, p).catch(() => [null, '上传失败'] as [null, string])
      if (why || !vo) {
        flashNotice(remoteErrorText(why || '上传失败'))
        break
      }
      added.push(vo)
    }
    setUploading(false)
    if (added.length > 0) setFiles((prev) => [...prev, ...added])
  }, [uploading])

  // downloadAttachment takes a file the other machine's agent handed over and saves it under this
  // machine's Downloads — then shows it, because "download" is a deliberate click whose result
  // the reader expects to lay eyes on.
  const downloadAttachment = useCallback(async (att: RemoteAttachmentVO) => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) return
    const [path, why] = await AgentService.RemoteDownloadFile(hostNow, id, att.id, att.name)
      .catch(() => ['', '没下载下来'] as [string, string])
    if (why || !path) {
      flashNotice(remoteErrorText(why || '没下载下来'))
      return
    }
    flashNotice(`已保存到 ${path}`)
    await AgentService.RevealPath(path).catch(() => {})
  }, [])

  // 定时任务的三个动作：跑一次 / 暂停 / 删除。都走对面那台机器自己的写入方，做完重读面板；
  // 它还会广播 cron_changed，所以别处的改动这里也能跟上。
  const runCronNow = useCallback(async (job: CronJobVO) => {
    const hostNow = hostRef.current
    if (!hostNow) return
    setPanelNotice('')
    const why = await AgentService.RemoteRunCronJob(hostNow, job.id).catch((e) => String(e))
    if (why && why !== 'ok') setPanelNotice(remoteErrorText(why))
    void refreshPanels()
  }, [refreshPanels])

  const toggleCronJob = useCallback(async (job: CronJobVO) => {
    const hostNow = hostRef.current
    if (!hostNow) return
    setPanelNotice('')
    const why = await AgentService.RemotePauseCronJob(hostNow, job.id, job.status === 'active').catch((e) => String(e))
    if (why && why !== 'ok') setPanelNotice(remoteErrorText(why))
    void refreshPanels()
  }, [refreshPanels])

  const deleteCronJob = useCallback(async (job: CronJobVO) => {
    const hostNow = hostRef.current
    if (!hostNow) return
    setPanelNotice('')
    const why = await AgentService.RemoteDeleteCronJob(hostNow, job.id).catch((e) => String(e))
    if (why && why !== 'ok') setPanelNotice(remoteErrorText(why))
    void refreshPanels()
  }, [refreshPanels])

  // The native panel's data shape, filled from the wire's rows. The fields the remote side does
  // not send (prompt, schedule expression, next-run error, timestamps) stay empty — the panel
  // only reads what it is given, and an invented value would read as a fact.
  const cronJobs: CronJobVO[] = useMemo(() => (panels?.cron || []).map((j: RemoteCronRowVO) => ({
    id: j.id, name: j.name, schedule: '', scheduleText: j.scheduleText, type: j.type,
    prompt: '', status: j.status, notify: '', timezone: '',
    targetKind: j.targetKind, targetTitle: j.targetTitle,
    nextRun: j.nextRun || undefined,
    lastRunStatus: j.lastRunStatus, lastRunError: j.lastRunError,
    running: j.running, createdAt: '', updatedAt: '',
  } as CronJobVO)), [panels])

  const stop = useCallback(async () => {
    if (!hostRef.current || !openRef.current) return
    const why = await AgentService.RemoteStop(hostRef.current, openRef.current).catch((e) => String(e))
    if (why && why !== 'ok') flashNotice(remoteErrorText(why))
  }, [])

  const answerPermission = useCallback(async (toolId: string, decision: PermissionDecision) => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) return
    const why = await AgentService.RemoteAnswerPermission(hostNow, id, toolId, decision).catch((e) => String(e))
    if (why && why !== 'ok') {
      flashNotice(remoteErrorText(why))
      return
    }
    setAsks((prev) => prev.filter((a) => a.toolId !== toolId))
  }, [])

  const answerQuestion = useCallback(async (answers: Record<string, string>) => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) return
    const ask = asks.find((a) => a.kind === 'question')
    const why = await AgentService.RemoteAnswerQuestion(hostNow, id, answers).catch((e) => String(e))
    if (why && why !== 'ok') {
      flashNotice(remoteErrorText(why))
      return
    }
    if (ask) setAsks((prev) => prev.filter((a) => a.toolId !== ask.toolId))
  }, [asks])

  const declineQuestion = useCallback(async () => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) return
    const why = await AgentService.RemoteDeclineQuestion(hostNow, id).catch((e) => String(e))
    if (why && why !== 'ok') flashNotice(remoteErrorText(why))
  }, [])

  const pickProvider = useCallback(async (name: string) => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) return
    const why = await AgentService.RemoteSetProvider(hostNow, id, name).catch((e) => String(e))
    if (why && why !== 'ok') flashNotice(remoteErrorText(why))
    else setModel(name)
  }, [])

  const setRemoteSkipApproval = useCallback(async (next: boolean) => {
    const hostNow = hostRef.current
    const id = openRef.current
    if (!hostNow || !id) return
    const why = await AgentService.RemoteSetSkipApproval(hostNow, id, next).catch((e) => String(e))
    if (why && why !== 'ok') flashNotice(remoteErrorText(why))
    else setSkipApproval(next)
  }, [])

  // A draft that grows with its content, up to a point. The same shape the local composer
  // uses, minus the drag handle: on this screen the box is a plain box.
  useLayoutEffect(() => {
    const el = inputRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = Math.min(el.scrollHeight, 200) + 'px'
  }, [draft])

  // ── Adding a machine ──────────────────────────────────────────────────────
  const addMachine = useCallback(async () => {
    const addr = pairAddr.trim()
    const token = pairToken.trim()
    if (!addr || !token) return
    setAdding(true)
    setAddError('')
    const [vo, why] = await AgentService.AddRemoteHost(pairPayload(addr, token)).catch(() => [null, '配对失败'] as [null, string])
    setAdding(false)
    if (why || !vo) {
      setAddError(remoteErrorText(why || '') || '配对失败')
      return
    }
    setPairAddr('')
    setPairToken('')
    setAddOpen(false)
    await loadHosts()
    selectHost(vo.id)
  }, [loadHosts, pairAddr, pairToken, selectHost])

  const closeAdd = useCallback(() => {
    setAddOpen(false)
    setAddError('')
  }, [])

  const removeMachine = useCallback(async (id: string) => {
    const why = await AgentService.RemoveRemoteHost(id).catch((e) => String(e))
    if (why && why !== 'ok') {
      flashNotice(why)
      return
    }
    setConfirmDelete('')
    const list = await loadHosts()
    if (id === hostRef.current) {
      setHostID('')
      hostRef.current = ''
      setSessions([])
      setOpenID('')
      setSnapshot(null)
      setMessages([])
      openRef.current = ''
      setLive(null)
      if (list[0]) selectHost(list[0].id)
    }
  }, [loadHosts, selectHost])

  // Esc goes back to the conversation, the same contract the settings page has. A field
  // inside keeps the first press (a half-typed draft is not destroyed by the key that
  // leaves the page).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      // 先关这一页自己打开的东西（菜单、确认框、改名框），都没有才回对话 —— 和原生窗口里
      // 「Esc 先收掉浮层」同一条规矩。
      if (menu) { e.preventDefault(); setMenu(null); return }
      if (confirmDel) { e.preventDefault(); setConfirmDel(null); return }
      // 配对弹窗盖在页面上：Esc 先收它，而不是连它带页面一起退掉。
      if (addOpen) { e.preventDefault(); closeAdd(); return }
      if (editingId || renamingHost) {
        e.preventDefault()
        setEditingId(''); setEditTitle(''); setRenamingHost(''); setHostName('')
        return
      }
      if (openPanel) {
        e.preventDefault()
        setOpenPanel('')
        return
      }
      const el = document.activeElement
      if (el instanceof HTMLElement && (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT')) {
        el.blur()
        return
      }
      e.preventDefault()
      onBack()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [addOpen, closeAdd, confirmDel, editingId, menu, onBack, openPanel, renamingHost])

  // The two read-only panels dismiss on a press outside, the same contract the native popovers
  // have. CronPanel registers its own (it also has a two-step delete to unwind first).
  useEffect(() => {
    if (openPanel !== 'mcp' && openPanel !== 'agents') return
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t) return
      if (t.closest('.popover-panel') || t.closest('.mcp-btn') || t.closest('.agents-btn')) return
      setOpenPanel('')
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [openPanel])

  // ⌘N creates a conversation on the machine on screen, the same gesture the window has for its
  // own. The app-level shortcut handler deliberately ignores the remote view (its keys belong to
  // this page), so this is the only listener for it while the page is up.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (imeActive(e)) return
      // ⌘⇧⌫ opens the delete confirmation for the conversation on screen — the same key and the
      // same box the window uses for its own.
      if (isMod(e) && e.shiftKey && (e.key === 'Backspace' || e.key === 'Delete')) {
        e.preventDefault()
        askDeleteCurrent()
        return
      }
      if (!isMod(e) || e.shiftKey || e.altKey) return
      if (e.key.toLowerCase() !== 'n') return
      e.preventDefault()
      void newSession()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [askDeleteCurrent, newSession])

  // ── What the transcript shows ─────────────────────────────────────────────
  const turns = useMemo(() => buildTurns(messages as unknown as SessionMessage[]), [messages])

  // Which card carries the last turn's numbers: the newest ANSWER. History has no per-turn
  // duration (the protocol stores none), so only that one can honestly show it.
  const lastAssistantIndex = useMemo(() => {
    for (let i = turns.length - 1; i >= 0; i--) if (turns[i].role === 'assistant') return i
    return -1
  }, [turns])

  const hostLabel = host ? host.name : '选一台机器'
  const askPermission = asks.find((a) => a.kind === 'permission')
  const askQuestions = asks.find((a) => a.kind === 'question')
  const questions: Question[] = useMemo(() => (askQuestions?.questions || []).map((q) => ({
    question: q.question,
    header: '',
    options: (q.options || []).map((label) => ({ label, description: '' })),
    multi_select: !!q.multiSelect,
  })), [askQuestions])

  const perm: PermissionRequest | null = askPermission ? {
    sessionId: openID,
    toolId: askPermission.toolId,
    toolName: askPermission.toolName || '',
    kind: (askPermission.permissionKind || 'bash') as PermissionRequest['kind'],
    preview: askPermission.preview || '',
  } : null

  // The parked call is INSIDE the turn that is waiting, so its card is replaced by the form
  // (the native transcript's rule). The standalone form below is only the fallback for a wait
  // whose call is not in the loaded window — losing a card that can be answered is worse than
  // showing it twice in the one case it would happen.
  const visibleParts = [
    ...(turns.length ? turns[turns.length - 1].parts || [] : []),
    ...(live?.msg.parts || []),
  ]
  const permInline = !!perm && visibleParts.some((p) => p.type === 'tool' && p.toolCallId === perm.toolId)
  const askInline = questions.length > 0 && visibleParts.some((p) => p.type === 'tool' && p.name === 'AskUserQuestion' && !p.done)

  // Opening or closing a fold changes the transcript's height; a reader pinned to the bottom
  // stays pinned. The child calls this from its own layout effect, i.e. after the DOM changed.
  const scrollIfPinned = useCallback(() => {
    const el = scrollerRef.current
    if (el && stickRef.current) el.scrollTop = el.scrollHeight
  }, [])

  return (
    <div className="remote-page">
      <aside className="remote-side">
        <div className={`window-strip${IS_MAC ? '' : ' drag-region'}`} aria-hidden="true" />
        <div className="remote-side-top">
          <button className="btn ghost remote-back" onClick={onBack} title="返回对话（Esc）">
            <Icon as={ArrowLeft} size={ICON_MD} />
            <span>返回</span>
          </button>
          <span className="remote-side-title">远端机器</span>
        </div>

        <button className="new-chat remote-new-chat" onClick={() => void newSession()}
          disabled={!hostID || creating} title={`在这台机器上新建会话（${MOD_KEY} N）`}>
          <Icon as={PenLine} size={ICON_MD} />
          <span>{creating ? '新建中…' : '新会话'}</span>
        </button>

        <div className="remote-session-head">
          <span>会话</span>
          {loadingList ? <span className="remote-session-loading">…</span> : null}
        </div>
        <nav className="remote-sessions">
          {listError ? <div className="remote-list-error">{listError}</div> : null}
          {!listError && sessions.length === 0 && !loadingList && hostID ? (
            <div className="remote-empty">这台机器上没有会话</div>
          ) : null}
          {sessions.map((s) => (
            <div key={s.id} className={`remote-session${s.id === openID ? ' is-on' : ''}`}
              role="button" tabIndex={0}
              onClick={() => void openSession(s.id)}
              onKeyDown={actOnKey(() => void openSession(s.id))}
              onContextMenu={(e) => { e.preventDefault(); setMenu({ kind: 'session', id: s.id, x: e.clientX, y: e.clientY }) }}>
              {editingId === s.id ? (
                <input className="session-rename" autoFocus value={editTitle}
                  onChange={(e) => setEditTitle(e.target.value)}
                  onClick={(e) => e.stopPropagation()}
                  onBlur={() => void commitRename(s.id)}
                  onKeyDown={(e) => {
                    if (imeActive(e)) return
                    if (e.key === 'Enter') { e.stopPropagation(); void commitRename(s.id) }
                    else if (e.key === 'Escape') { e.stopPropagation(); setEditingId(''); setEditTitle('') }
                  }} />
              ) : (
                <span className="remote-session-title"
                  onDoubleClick={() => { setEditingId(s.id); setEditTitle(s.title || ''); setMenu(null) }}>
                  {s.title || '未命名会话'}
                </span>
              )}
              <span className="remote-session-meta">
                <span>{fmtTime(s.updatedAt)}</span>
                {s.waiting ? <span className="wait-dot" title={s.waiting} /> : null}
                {s.running ? <span className="spin-dot" title="运行中" /> : null}
                {s.readOnly || s.heldByTask ? <span className="session-tag">只读</span> : null}
                {s.channelSession ? <span className="session-tag">{s.channelName || '渠道'}</span> : null}
                {s.pinned ? <span className="session-pin" title="已置顶：排在最前"><PinIcon /></span> : null}
              </span>
            </div>
          ))}
        </nav>

        {/* 机器选择器贴在左栏底部：这一页的主角是那台机器上的会话，换机器比换会话少得多，
            但它必须随时够得着（换机器换的是整列会话，藏进菜单里就找不到回本机的路）。 */}
        <div className="remote-hosts">
          <div className="remote-hosts-head">
            <span>机器</span>
            <button className="remote-add" onClick={() => setAddOpen(true)}>
              <Icon as={Plus} size={ICON_XS} />
              添加机器
            </button>
          </div>
          <div className="remote-host-list">
            {hosts.map((h) => (
              <div key={h.id} className={`remote-host${h.id === hostID ? ' is-on' : ''}`}
                onContextMenu={(e) => { e.preventDefault(); setMenu({ kind: 'host', id: h.id, x: e.clientX, y: e.clientY }) }}>
                {renamingHost === h.id ? (
                  <input className="remote-host-rename" autoFocus value={hostName}
                    onChange={(e) => setHostName(e.target.value)}
                    onBlur={() => void commitHostRename(h.id)}
                    onKeyDown={(e) => {
                      if (imeActive(e)) return
                      if (e.key === 'Enter') { e.stopPropagation(); void commitHostRename(h.id) }
                      else if (e.key === 'Escape') { e.stopPropagation(); setRenamingHost(''); setHostName('') }
                    }} />
                ) : (
                  <button className="remote-host-pick" onClick={() => selectHost(h.id)}
                    title={h.urls?.join('\n') || h.id}>
                    <span className="remote-host-name"
                      onDoubleClick={() => { setRenamingHost(h.id); setHostName(h.name || h.deviceId.slice(0, 8)); setMenu(null) }}>
                      {h.name || h.deviceId.slice(0, 8)}
                    </span>
                    <span className="remote-host-meta">
                      {hostAddr(h)}{h.reachable ? ' · 刚刚应答过' : ''}
                    </span>
                  </button>
                )}
                {confirmDelete === h.id ? (
                  <span className="remote-host-confirm">
                    删掉？<button className="btn ghost" onClick={() => setConfirmDelete('')}>取消</button>
                    <button className="btn ghost" onClick={() => void removeMachine(h.id)}>删除</button>
                  </span>
                ) : (
                  <button className="remote-host-del" title="从这台机器上删掉它的配对信息"
                    onClick={() => setConfirmDelete(h.id)}>
                    <Icon as={Trash2} size={ICON_SM} />
                  </button>
                )}
              </div>
            ))}
          </div>
        </div>
      </aside>

      <main className="remote-main">
        <header className="remote-header">
          <div className="remote-head-text">
            <span className="remote-head-host">{hostLabel}{current?.projectName ? ` · ${current.projectName}` : ''}</span>
            <span className="remote-head-title">{title || '远端对话'}</span>
          </div>
          <div className="remote-head-actions">
            {openID ? (
              <div className="titlebar-metrics remote-metrics">
                {tps.now > 0 ? <span className={`usage-tps tps-${tpsTier(tps.now)}`} title="当前输出速率">{tps.now}/s</span>
                  : tps.last > 0 ? <span className="usage-tps tps-paused" title="最近输出速率">{tps.last}/s</span>
                  : null}
                <RemoteContext ctx={ctx} />
                <span className="usage-meta">
                  {usage.cacheHitRate > 0 ? <CacheRing rate={usage.cacheHitRate} /> : null}
                  {usage.cost > 0 ? <span className="usage-cost" title="那台机器上这段对话的成本">¥{usage.cost.toFixed(3)}</span> : null}
                  {usage.credit > 0 ? <span className="usage-credit" title="那台机器上这段对话的积分">{fmtCredit(usage.credit)} 积分</span> : null}
                </span>
              </div>
            ) : null}
            {!connected && reconnectAttempt > 0 ? (
              <span className="remote-reconnecting">重连中…（{reconnectAttempt}）</span>
            ) : null}
            {!connected && reconnectAttempt === 0 ? (
              <button className="btn ghost" onClick={() => void reload()}>重连</button>
            ) : null}
          </div>
        </header>

        {openError ? <div className="remote-open-error">{openError}</div> : null}
        {notice ? <div className="remote-notice" onClick={() => flashNotice('')}>{notice}</div> : null}

        <div className="chat-wrap remote-chat-wrap">
          <div className="chat" ref={scrollerRef} onScroll={onScroll}>
          <div className="chat-content">
            {loadingMore ? <div className="remote-empty">正在读更早的记录…</div> : null}
            {!hasMore && messages.length > 0 ? <div className="remote-empty">已经到最早了</div> : null}
            {!openID ? (
              <div className="remote-welcome">
                <div className="remote-welcome-title">选一段对话</div>
                <div className="remote-welcome-sub">
                  这是一个收窄的客户端：会话、对话、审批和模型都能用；
                  文件树、diff 与评审、回退和分支、设置只在那台机器的桌面窗口里。
                </div>
              </div>
            ) : null}
            {opening ? <div className="remote-empty">正在打开…</div> : null}
            {turns.map((m, mi) => m.role === 'user'
              ? <UserBubble key={m.id}><span className="remote-user-text">{(m.text || '').split('\n').map((line, i) => (
                <span key={i}>{line}{i < (m.text || '').split('\n').length - 1 ? <br /> : null}</span>
              ))}</span></UserBubble>
              : <RemoteTurn key={m.id} m={m} workDir={workDir}
                perm={perm} questions={questions}
                attachments={snapshot?.attachments || []} onDownload={(att) => void downloadAttachment(att)}
                stat={mi === lastAssistantIndex ? turnStat ?? undefined : undefined}
                onPermAnswer={(toolId, decision) => void answerPermission(toolId, decision)}
                onAskAnswer={(answers) => void answerQuestion(answers)}
                onAskDecline={() => void declineQuestion()}
                onFold={scrollIfPinned} />)}
            {live || busy ? (
              <RemoteTurn m={live?.msg || emptyLiveMessage()} running={busy} workDir={workDir}
                perm={perm} questions={questions}
                attachments={snapshot?.attachments || []} onDownload={(att) => void downloadAttachment(att)}
                onPermAnswer={(toolId, decision) => void answerPermission(toolId, decision)}
                onAskAnswer={(answers) => void answerQuestion(answers)}
                onAskDecline={() => void declineQuestion()}
                onFold={scrollIfPinned} />
            ) : null}
            {perm && !permInline ? (
              <PermissionForm perm={{ req: perm }}
                onAnswer={(decision) => void answerPermission(perm.toolId, decision)} />
            ) : null}
            {askQuestions && questions.length > 0 && !askInline ? (
              <AskForm questions={questions}
                onSubmit={(answers) => void answerQuestion(answers)}
                onCancel={() => void declineQuestion()} />
            ) : null}
          </div>
          </div>
        </div>

        {openID ? (
          <div className="composer remote-composer">
            <PendingBar running={busy} items={pending.map(pendingDisplay)} sendingNow={!busy && sending}
              onSendNow={() => void sendPendingNow()}
              onClear={() => setQueue([])}
              onDrop={(i) => setQueue(pending.filter((_, j) => j !== i))} />
            {files.length > 0 ? (
              <div className="remote-files">
                {files.map((f) => (
                  <span className="remote-file-chip" key={f.name}>
                    <Icon as={Paperclip} size={ICON_XS} />
                    <span className="remote-file-name" title={f.name}>{f.name}</span>
                    <button className="remote-file-del" title="不发了" onClick={() => setFiles((prev) => prev.filter((x) => x.name !== f.name))}>✕</button>
                  </span>
                ))}
              </div>
            ) : null}
            <div className="composer-box">
              <div className="composer-row">
                <div className="composer-input-wrap">
                  <textarea
                    ref={inputRef}
                    className="composer-input"
                    rows={1}
                    placeholder={busy ? '正在运行…Enter 加入待发送' : '发给这台机器上的对话（Enter 发送，Shift+Enter 换行）'}
                    value={draft}
                    onChange={(e) => setDraft(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter' && !e.shiftKey) {
                        if (imeActive(e)) return
                        e.preventDefault()
                        void send()
                      }
                    }}
                  />
                </div>
                <ComposerControls running={busy} canSend={!!draft.trim() || files.length > 0} sendingNow={!busy && sending}
                  onSend={() => void send()} onStop={() => void stop()}>
                  <button className="mic-btn remote-attach" title="发送文件到这台机器"
                    aria-label="发送文件" disabled={uploading || !openID} onClick={() => void attachFiles()}>
                    <Icon as={Paperclip} size={ICON_SM} />
                  </button>
                </ComposerControls>
              </div>
            </div>
            <ComposerStatus>
              <div className="provider-picker">
                <ProviderPicker providers={providers} value={model} onPick={(name) => void pickProvider(name)} />
                <ApprovalSelect skip={skipApproval} disabled={!openID || !!current?.readOnly}
                  onChange={(skip) => void setRemoteSkipApproval(skip)} />
                {/* 对面那台机器的三个面板，位置与原生状态栏一致：模型/审批之后。只读的两个
                    （MCP、Agents）各自有一份小渲染；定时任务复用同一个 CronPanel。 */}
                <span className="popover-anchor" ref={agentsPop.anchorRef}>
                  <button className="agents-btn" title="Agents（那台机器可委派的节点）"
                    onClick={() => setOpenPanel((v) => (v === 'agents' ? '' : 'agents'))}>
                    <span className="agents-ico"><AgentsIcon /></span>
                    <span className="agents-count">
                      {(panels?.agents.peers || []).filter((p) => p.reach === '可用').length || ''}
                    </span>
                  </button>
                  {openPanel === 'agents' ? (
                    <RemoteAgentsPanel panel={panels?.agents || null} loading={panelsLoading}
                      onClose={() => setOpenPanel('')} floating={agentsPop} />
                  ) : null}
                </span>
                <span className="popover-anchor" ref={mcpPop.anchorRef}>
                  <button className="mcp-btn" title="MCP 服务器 / 工具（只读）"
                    onClick={() => setOpenPanel((v) => (v === 'mcp' ? '' : 'mcp'))}>
                    <span className="mcp-ico"><MCPIcon /></span>
                    <span className="mcp-count">{(panels?.mcp || []).filter((r) => r.connected).length || ''}</span>
                  </button>
                  {openPanel === 'mcp' ? (
                    <RemoteMCPPanel rows={panels?.mcp || []} loading={panelsLoading}
                      onClose={() => setOpenPanel('')} floating={mcpPop} />
                  ) : null}
                </span>
                <span className="popover-anchor" ref={cronPop.anchorRef}>
                  <button className="cron-btn" title="定时任务（可跑一次 / 暂停 / 删除）"
                    onClick={() => setOpenPanel((v) => (v === 'cron' ? '' : 'cron'))}>
                    <span className="cron-ico"><Icon as={Clock} size={ICON_XS} /></span>
                    {cronJobs.length ? <span className="cron-btn-count">{cronJobs.length}</span> : null}
                  </button>
                  {openPanel === 'cron' ? (
                    <CronPanel jobs={cronJobs} loading={panelsLoading} scheduled notice={panelNotice}
                      onClose={() => { setOpenPanel(''); setPanelNotice('') }}
                      onTogglePaused={(j) => void toggleCronJob(j)}
                      onRunNow={(j) => void runCronNow(j)}
                      onDelete={(j) => void deleteCronJob(j)}
                      floating={cronPop} readonly />
                  ) : null}
                </span>
              </div>
            </ComposerStatus>
          </div>
        ) : null}
      </main>

      {menu && (() => {
        const row = menu.kind === 'session' ? sessions.find((s) => s.id === menu.id) : undefined
        const knownNotify = menu.kind === 'session' && menu.id === openID && !!snapshot
        return (
        <div className="ctx-menu" role="menu" style={{ left: menu.x, top: menu.y }} onMouseLeave={() => setMenu(null)}>
          {menu.kind === 'session' ? (
            <>
              <button className="ctx-item" role="menuitem"
                onClick={() => { void togglePin(menu.id, !row?.pinned); setMenu(null) }}>
                {row?.pinned ? '取消置顶' : '置顶'}
              </button>
              {/* The push switch lives on that machine's session record; only the OPEN conversation's
                  value is known here (the snapshot carries it), so the item is absent elsewhere rather
                  than guessing a label. */}
              {knownNotify ? (
                <button className="ctx-item" role="menuitem"
                  onClick={() => { void toggleNotify(menu.id, !snapshot!.notifyOff); setMenu(null) }}>
                  {snapshot!.notifyOff ? '恢复手机推送' : '不推送到手机'}
                </button>
              ) : null}
              <button className="ctx-item" role="menuitem"
                onClick={() => { copyText(menu.id); setMenu(null) }}>复制 session id</button>
            </>
          ) : null}
          <button className="ctx-item" role="menuitem"
            onClick={() => {
              if (menu.kind === 'session') {
                const r = sessions.find((s) => s.id === menu.id)
                setEditingId(menu.id)
                setEditTitle(r?.title || '')
              } else {
                const h = hosts.find((x) => x.id === menu.id)
                setRenamingHost(menu.id)
                setHostName(h?.name || h?.deviceId.slice(0, 8) || '')
              }
              setMenu(null)
            }}>重命名</button>
          <button className="ctx-item danger" role="menuitem"
            disabled={menu.kind === 'session' && !!sessions.find((s) => s.id === menu.id)?.running}
            title={menu.kind === 'session' && sessions.find((s) => s.id === menu.id)?.running ? '会话正在运行，请先停止这一轮' : undefined}
            onClick={() => {
              if (menu.kind === 'session') {
                const row = sessions.find((s) => s.id === menu.id)
                setConfirmDel({ sid: menu.id, title: row?.title || '', cron: row?.cron || 0 })
              } else {
                setConfirmDelete(menu.id)
              }
              setMenu(null)
            }}>删除</button>
        </div>
        )
      })()}

      {addOpen && (
        <div className="confirm-overlay" onClick={closeAdd}>
          <div className="confirm-box remote-pair-box" role="dialog" aria-label="添加远端机器"
            onClick={(e) => e.stopPropagation()}>
            <div className="confirm-msg">添加一台远端机器</div>
            <div className="confirm-sub">在远端主机上启动无头宿主，它启动时会打印地址和令牌。</div>
            <div className="form-row">
              <label className="form-label" htmlFor="pair-addr">地址</label>
              <input id="pair-addr" className="form-input mono" autoFocus value={pairAddr}
                placeholder="http://192.168.1.9:8787"
                onChange={(e) => setPairAddr(e.target.value)}
                onKeyDown={(e) => {
                  if (imeActive(e)) return
                  if (e.key === 'Enter') { e.preventDefault(); void addMachine() }
                }} />
              <div className="form-hint">不带 http:// 也行，默认按 http 连。</div>
            </div>
            <div className="form-row">
              <label className="form-label" htmlFor="pair-token">令牌</label>
              <input id="pair-token" className="form-input mono" value={pairToken}
                placeholder="那台机器打印的 token"
                onChange={(e) => setPairToken(e.target.value)}
                onKeyDown={(e) => {
                  if (imeActive(e)) return
                  if (e.key === 'Enter') { e.preventDefault(); void addMachine() }
                }} />
            </div>
            {addError ? <div className="confirm-error">⚠ {addError}</div> : null}
            <div className="confirm-actions">
              <button className="btn ghost" onClick={closeAdd}>取消</button>
              <button className="btn" disabled={adding || !pairAddr.trim() || !pairToken.trim()}
                onClick={() => void addMachine()}>
                {adding ? '连接中…' : '连接'}
              </button>
            </div>
          </div>
        </div>
      )}

      {confirmDel && (
        <div className="confirm-overlay" onClick={() => setConfirmDel(null)}>
          {/* The box TAKES FOCUS (see confirmBoxRef): Enter has to mean 确认 here, and the caret is
              usually in the composer when ⌘⇧⌫ is pressed — a box that leaves it there would send a
              message instead. */}
          <div className="confirm-box" role="dialog" aria-label="删除会话" tabIndex={-1} ref={confirmBoxRef}
            onClick={(e) => e.stopPropagation()}
            onKeyDown={(e) => {
              if (imeActive(e)) return
              if (e.key === 'Enter') { e.preventDefault(); void deleteSession(confirmDel.sid) }
            }}>
            <div className="confirm-msg">删除会话「{confirmDel.title || '未命名会话'}」？</div>
            {confirmDel.cron ? (
              <div className="confirm-sub">这个会话的 {confirmDel.cron} 个定时任务也会一并删除。</div>
            ) : null}
            <div className="confirm-sub">此操作不可恢复。</div>
            {confirmDel.error ? <div className="confirm-error">⚠ {confirmDel.error}</div> : null}
            <div className="confirm-actions">
              <button className="btn ghost" onClick={() => setConfirmDel(null)}>取消</button>
              <button className="btn danger" onClick={() => void deleteSession(confirmDel.sid)}>删除</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
