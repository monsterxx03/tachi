import type { SessionInfo, SessionMessage } from '../bindings/github.com/monsterxx03/tachi/desktop'

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

export interface Part {
  type: 'thinking' | 'text' | 'tool'
  text?: string
  name?: string
  title?: string
  args?: string
  summary?: string
  ok?: boolean
  done?: boolean
  durationMs?: number
  toolCallId?: string
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
