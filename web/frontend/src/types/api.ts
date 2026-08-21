// Shared API types — mirror the JSON shapes in web/api.go.

export type MessageType =
  | 'user'
  | 'assistant'
  | 'thinking'
  | 'tool_call'
  | 'tool_result'
  | 'confirm'
  | 'reminder'

export interface Usage {
  input_tokens?: number
  output_tokens?: number
  cache_creation_input_tokens?: number
  cache_read_input_tokens?: number
  estimated_input_tokens?: number
}

export interface Message {
  type: MessageType
  content?: string
  name?: string
  signature?: string
  args?: unknown
  result?: string
  is_error?: boolean
  diff?: string
  tool_call_id?: string
  subagent_id?: string
  usage?: Usage
  iteration?: number
  /** Session-wide request sequence: monotonic across turns, shared with the
   *  matching api_requests record. Absent on user/reminder and legacy data. */
  seq?: number
  duration_ms?: number
  timestamp: string
}

export interface SessionMeta {
  id: string
  thread_id?: string
  title: string
  provider_name?: string
  working_dir?: string
  created_at: string
  updated_at: string
  skip_dream?: boolean
  mode?: string
  thinking_level?: string
  compacted_child_id?: string
  compacted_parent_id?: string
  compacted_parent_title?: string
}

export interface SessionSummaryItem {
  id: string
  title: string
  provider?: string
  mode?: string
  working_dir?: string
  created_at: string
  updated_at: string
  message_count: number
  tool_calls: number
  oneoff_count: number
  compacted_child_id?: string
  cost: number
  calls: number
}

/** One page of GET /api/sessions (keyset-paginated by session id). */
export interface SessionListPage {
  sessions: SessionSummaryItem[]
  /** Total number of sessions on disk (not just this page). */
  total: number
  /** ID of the last session of this page; pass as cursor for the next page. */
  next_cursor?: string
}

export interface APITool {
  name: string
  description: string
  parameters?: unknown
}

export interface APIRequest {
  timestamp: string
  iteration?: number
  /** Session-wide request sequence: monotonic across turns, shared with the
   *  matching request-bound messages. Absent on legacy data. */
  seq?: number
  system_prompt: string
  user_prompt?: string
  model?: string
  provider?: string
  thinking?: string
  /** Wall-clock duration of this API call in ms (0/absent on legacy data). */
  duration_ms?: number
  /** Precise CNY cost (¥) of this API call, from the backend usage ledger
   *  (absent on legacy data / when not resolved). */
  cost?: number
  /** Precise credit of this API call, from the same ledger row as cost
   *  (absent on legacy data / when not resolved). */
  credit?: number
  tools?: APITool[]
}

export interface OneOffSummary {
  file: string
  size: number
  kind?: string
  provider?: string
  model?: string
  started_at?: string
  system_prompt?: string
  extra?: Record<string, string>
  event_count: number
}

export interface UsageAmount {
  cost: number
  calls: number
}

export interface UsageDayStat {
  date: string
  cost: number
  calls: number
  unpriced: number
  input?: number
  output?: number
  cache?: number
}

export interface UsageSummary {
  total_cost: number
  total_calls: number
  /** Total credit across all rows (call-time snapshots; pre-upgrade rows
   *  recomputed from the current configured credit_rate). Absent on older
   *  backends. */
  total_credit?: number
  /** Session-wide token totals (all ledger kinds); absent on older backends.
   *  Input is cache-miss only; cache read/creation are separate, so the hit
   *  rate = read / (read + creation + input). */
  total_input?: number
  total_output?: number
  total_cache_read?: number
  total_cache_creation?: number
  days: UsageDayStat[]
  by_kind: Record<string, UsageAmount>
  by_model: Record<string, UsageAmount>
}

export interface SessionDetail {
  meta: SessionMeta
  messages: Message[]
  api_requests: APIRequest[]
  subagents: Record<string, Message[]>
  oneoffs: OneOffSummary[]
  usage: UsageSummary
}

export interface OneOffEvents {
  file: string
  events: unknown[]
}
