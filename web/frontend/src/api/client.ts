// Thin fetch wrapper for the Tachi web API.
//
// Auth flow:
//   - The backend requires an X-Api-Key header when config.web.api_key is set.
//   - The key is persisted in a cookie (tachi_api_key) so it survives reloads.
//   - Any 401 response throws an AuthError; the app-level ApiKeyGate catches
//     it and shows the key entry screen.

import type {
  APIRequest,
  GlobalOneOffs,
  Message,
  OneOffEvents,
  SessionDetail,
  SessionListPage,
  SessionMeta,
  UsageSummary,
} from '../types/api'

export const API_KEY_COOKIE = 'tachi_api_key'

export class AuthError extends Error {
  constructor() {
    super('unauthorized')
    this.name = 'AuthError'
  }
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export function getApiKey(): string {
  const m = document.cookie.match(new RegExp(`(?:^|; )${API_KEY_COOKIE}=([^;]*)`))
  return m ? decodeURIComponent(m[1]) : ''
}

export function setApiKey(key: string): void {
  // Simple persistence; secure flag off since this is a localhost-only console.
  document.cookie = `${API_KEY_COOKIE}=${encodeURIComponent(key)}; path=/; max-age=31536000; SameSite=Lax`
}

export function clearApiKey(): void {
  document.cookie = `${API_KEY_COOKIE}=; path=/; max-age=0`
}

interface RequestOptions {
  method?: string
  body?: unknown
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  const key = getApiKey()
  if (key) headers['X-Api-Key'] = key
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'

  const resp = await fetch(path, {
    method: opts.method ?? 'GET',
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  })

  if (resp.status === 401 || resp.status === 403) {
    throw new AuthError()
  }
  if (!resp.ok) {
    let msg = `HTTP ${resp.status}`
    try {
      const data = (await resp.json()) as { error?: string }
      if (data.error) msg = data.error
    } catch {
      /* keep status-only message */
    }
    throw new ApiError(resp.status, msg)
  }
  return (await resp.json()) as T
}

// ── typed endpoints ────────────────────────────────────────────────────────

export const api = {
  listSessions: (params?: { limit?: number; cursor?: string }) => {
    const q = new URLSearchParams()
    if (params?.limit != null) q.set('limit', String(params.limit))
    if (params?.cursor) q.set('cursor', params.cursor)
    const qs = q.toString()
    return request<SessionListPage>(`/api/sessions${qs ? `?${qs}` : ''}`)
  },

  getSession: (id: string) =>
    request<SessionDetail>(`/api/sessions/${encodeURIComponent(id)}`),

  listSessionOneOffs: (id: string) =>
    request<{ oneoffs: import('../types/api').OneOffSummary[] }>(
      `/api/sessions/${encodeURIComponent(id)}/oneoff`,
    ),

  getOneOff: (id: string, file: string) =>
    request<OneOffEvents>(
      `/api/sessions/${encodeURIComponent(id)}/oneoff/${encodeURIComponent(file)}`,
    ),

  globalOneOffs: () => request<GlobalOneOffs>('/api/oneoff'),

  usage: () => request<UsageSummary>('/api/usage'),

  transcriptUrl: (id: string) => `/api/sessions/${encodeURIComponent(id)}/transcript`,
}

// Convenience: fetch a full session's message list alone (used by the
// inspector timeline; meta arrives in the same payload).
export async function fetchSessionMessages(id: string): Promise<{
  meta: SessionMeta
  messages: Message[]
  apiRequests: APIRequest[]
  oneoffs: import('../types/api').OneOffSummary[]
}> {
  const detail = await api.getSession(id)
  return {
    meta: detail.meta,
    messages: detail.messages,
    apiRequests: detail.api_requests,
    oneoffs: detail.oneoffs,
  }
}