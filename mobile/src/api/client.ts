import type {
  AttachmentRef,
  PermissionDecision,
  SendMode,
  SessionMessage,
  SessionSummary,
  Snapshot,
} from './types';

/** 服务端拒绝的原因按协议原样带出来 —— 手机屏幕小，用户能行动的就是那句话 */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
  ) {
    super(code);
  }
}

export interface Pairing {
  baseUrl: string;
  token: string;
}

async function req<T>(conn: Pairing, path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${conn.baseUrl}${path}`, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${conn.token}`,
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    let code = `http_${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body?.error) code = body.error;
    } catch {
      // 不是 JSON 就用手上的状态码
    }
    throw new ApiError(res.status, code);
  }
  return (await res.json()) as T;
}

/** 配对页用它验证地址与令牌；这个端点不要令牌 */
export async function ping(baseUrl: string): Promise<{ ok: boolean; name: string; version: string }> {
  const res = await fetch(`${baseUrl.replace(/\/+$/, '')}/v1/ping`);
  if (!res.ok) throw new ApiError(res.status, `http_${res.status}`);
  return (await res.json()) as { ok: boolean; name: string; version: string };
}

export const api = {
  listSessions: (c: Pairing) => req<{ sessions: SessionSummary[] }>(c, '/v1/sessions'),

  providers: (c: Pairing) => req<{ providers: { name: string; model: string }[] }>(c, '/v1/providers'),

  /** 向上翻页：before 是记录序号，不是时间 */
  page: (c: Pairing, id: string, before?: number, limit = 50) =>
    req<{ messages: SessionMessage[]; hasMore: boolean; session: SessionSummary }>(
      c,
      `/v1/sessions/${encodeURIComponent(id)}/page?limit=${limit}${before != null ? `&before=${before}` : ''}`,
    ),

  send: (c: Pairing, id: string, text: string, mode: SendMode) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}/messages`, {
      method: 'POST',
      body: JSON.stringify({ text, mode }),
    }),

  stop: (c: Pairing, id: string) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}/stop`, { method: 'POST' }),

  decide: (c: Pairing, id: string, toolId: string, decision: PermissionDecision) =>
    req<{ accepted: boolean }>(
      c,
      `/v1/sessions/${encodeURIComponent(id)}/permissions/${encodeURIComponent(toolId)}`,
      { method: 'POST', body: JSON.stringify({ decision }) },
    ),

  /** 答案的形状沿用桌面：键是问题原文，值是选中项用 ", " 连接 */
  answer: (c: Pairing, id: string, answers: Record<string, string>) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}/questions`, {
      method: 'POST',
      body: JSON.stringify({ answers }),
    }),

  decline: (c: Pairing, id: string) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}/questions`, {
      method: 'POST',
      body: JSON.stringify({ decline: true }),
    }),

  setProvider: (c: Pairing, id: string, name: string) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}/provider`, {
      method: 'POST',
      body: JSON.stringify({ name }),
    }),

  setSkipApproval: (c: Pairing, id: string, skip: boolean) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}/skip-approval`, {
      method: 'POST',
      body: JSON.stringify({ skip }),
    }),

  /**
   * 附件的下载地址与请求头。
   *
   * 令牌走**请求头**、不进 URL —— URL 会进日志和 WebView 的历史。图片用 Image 的
   * headers 也能带上（RN 支持），文本就正常 fetch。
   */
  file: (c: Pairing, sessionId: string, a: AttachmentRef) => ({
    url: `${c.baseUrl}/v1/sessions/${encodeURIComponent(sessionId)}/files/${encodeURIComponent(a.id)}`,
    headers: { Authorization: `Bearer ${c.token}` },
  }),
};

export type { Snapshot };

/** 附件取回时要用的地址与请求头（令牌走头，不进 URL） */
export interface FileSource {
  url: string;
  headers: Record<string, string>;
}

/**
 * 数组字段兜底：协议要求空的时候是 `[]` 而不是 `null`，但别拿渲染去赌 ——
 * 某一边少写一次初始化，崩的就是这里。
 */
export function normalizeSnapshot(s: Snapshot): Snapshot {
  return {
    ...s,
    messages: s.messages ?? [],
    asks: s.asks ?? [],
    attachments: s.attachments ?? [],
    workspace: { ...s.workspace, additional: s.workspace?.additional ?? [] },
    context: { ...s.context, parts: s.context?.parts ?? [] },
  };
}
