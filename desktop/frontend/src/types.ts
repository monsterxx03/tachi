import type { FileChangeVO, SessionInfo, SessionMessage } from '../bindings/github.com/monsterxx03/tachi/desktop'

export interface SessionItem extends SessionInfo { active?: boolean }

export interface ToolCardData {
  name: string
  title?: string
  arguments?: string
  summary: string
  ok: boolean
  done?: boolean
  durationMs?: number
}

// AttachmentInfo is a file the agent handed over (SendFile). It is derived from
// the recorded tool call itself, so the live turn and reloaded history render
// the very same card — one path, no duplicated rendering.
export interface AttachmentInfo {
  path: string
  name: string
}

export interface Part {
  type: 'thinking' | 'text' | 'tool' | 'notice'
  // notice: a one-line event in the transcript that is not part of the
  // conversation itself (auto-compaction). `label` is the line, `summary` the
  // optional body it can unfold, `done === false` while it is still running.
  label?: string
  text?: string
  name?: string
  title?: string
  args?: string
  summary?: string
  ok?: boolean
  done?: boolean
  durationMs?: number
  toolCallId?: string
  file?: AttachmentInfo
  // change: the file change this tool call set out to make (derived in Go from the
  // call's args, never persisted). Only rendered once the part is done and ok — a
  // running or failed call changed nothing.
  change?: FileChangeVO | null
  // diffOpen: whether the card shows its diff. Owned here (not inside the card)
  // because the turn footer's chip opens/closes every diff of the turn at once.
  diffOpen?: boolean
  // expand: the card opens with its output showing. Set for tool calls the USER
  // asked for directly (the /sh slash command) — that output is the answer, so
  // folding it away would hide what was requested.
  expand?: boolean
}

export interface Message {
  id: string
  role: 'user' | 'assistant'
  text?: string
  ts?: string
  reminder?: string
  reminderCollapsed?: boolean
  parts?: Part[]             // historical assistant turn: ordered segments
  thinking?: string          // streaming turn
  thinkingCollapsed?: boolean
  tools?: ToolCardData[]     // streaming turn
  running?: boolean
  stopped?: boolean          // turn was stopped by the user (not an error)
  summary?: { durationMs: number; iterations: number; cost: number; credit: number }
}

export interface AgentEvent {
  Type: string
  TextDelta: string
  ThinkingDelta: string
  ToolName: string
  ToolResult: string
  ToolIsError: boolean
  ToolDuration?: number
  Result?: { ExitReason?: string }
  // Auto-compaction (agent's auto_compact_done): the summary that replaced the
  // old history and how many messages it replaced.
  CompactSummary?: string
  OldMsgCount?: number
  // ToolAutoExpand: display hint from the backend for user-requested tool calls
  // (see Part.expand).
  ToolAutoExpand?: boolean
  // Title: for session_title — the name the model generated for this session. It is
  // persisted in the session meta, but the sidebar renders the list it fetched, so the
  // event is the only thing that can update the row while the session is open.
  Title?: string
}

export const STATUS_META: Record<string, { dot: string; desc: string }> = {
  idle: { dot: '●', desc: '空闲' }, thinking: { dot: '◐', desc: '思考中' }, tool_running: { dot: '◑', desc: '执行工具' },
  busy: { dot: '◒', desc: '处理中' }, error: { dot: '▲', desc: '出错' },
}

export const THINKING_LEVELS = ['default', 'none', 'low', 'medium', 'high', 'xhigh', 'max']
// PAGE_SIZE is the number of raw session messages loaded per "page" when
// switching to a session or scrolling up for older history.
export const PAGE_SIZE = 100

// @-file completion. AT_MAX_RESULTS caps the picker's list (matching the TUI's
// completion limit); AT_SEARCH_DEBOUNCE_MS coalesces keystrokes into one
// backend search.
export const AT_MAX_RESULTS = 20
export const AT_SEARCH_DEBOUNCE_MS = 120

export interface AtMatch {
  path: string
  isDir: boolean
  // ref is the reference the backend wants inserted ('@' included): a match under
  // the primary root is relative, one under an additional root is absolute — a
  // relative path there would resolve against the primary and point elsewhere.
  ref?: string
  // root labels the additional root a match came from ("" = primary), so two roots
  // holding the same path are tellable apart in the list.
  root?: string
}

// AtPickerState is the composer's @-file picker: the reference being typed
// (start index + query), the matches, the highlighted row and the number of
// references already present in the input.
export interface AtPickerState {
  start: number
  query: string
  items: AtMatch[]
  idx: number
  loading: boolean
  count: number
}

export type { SessionMessage }
