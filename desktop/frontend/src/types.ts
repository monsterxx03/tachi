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
}

export const STATUS_META: Record<string, { dot: string; desc: string }> = {
  idle: { dot: '●', desc: '空闲' }, thinking: { dot: '◐', desc: '思考中' }, tool_running: { dot: '◑', desc: '执行工具' },
  busy: { dot: '◒', desc: '处理中' }, error: { dot: '▲', desc: '出错' },
}

export const THINKING_LEVELS = ['default', 'none', 'low', 'medium', 'high', 'xhigh', 'max']
// PAGE_SIZE is the number of raw session messages loaded per "page" when
// switching to a session or scrolling up for older history.
export const PAGE_SIZE = 100

export type { SessionMessage }
