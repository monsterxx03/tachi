import type {
  AttachmentRef,
  AwakeState,
  Panels,
  PermissionDecision,
  ProjectRef,
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
  /**
   * 同一台电脑的其它地址（备用）。配对码里带着它，服务端在 `/v1/ping` 里也会再报一次 ——
   * 手机用它来判断「还有没有别的路能进这台电脑」，不需要用户知道自己在哪个网络里。
   */
  urls?: string[];
  /**
   * 地址是读者手动选的：自动切换不再动它（见 `mobile/src/state/mobile.ts` 的 heal / rotate）。
   * 只有「记住的电脑」（`Device`）会用到，临时拼出来的 Pairing 不带。
   */
  pinned?: boolean;
}

/**
 * 一次请求最多等多久。
 *
 * 没有上限的话，慢链路（Wi-Fi 差 + 蜂窝）会让一个请求永远挂着：界面停在空列表、不报错、
 * 也不重试，看起来就像「连上了但什么也没有」——比明确失败更难查。
 */
const REQUEST_TIMEOUT_MS = 15_000;

/**
 * 最近几次请求的去向与结果，给设置页上的那一行诊断用。
 *
 * 为什么值得存在：这条链路上「已连接但什么都没加载」有三种完全不同的原因 —— 请求没发出去、
 * 发出去了没人应答、答回来被丢掉。它们在屏幕上长得一模一样，而手机上没有控制台可看。留下
 * 这几条记录，就能让它变成一句能读的话。
 */
export interface ReqRecord {
  at: number;
  method: string;
  path: string;
  base: string;
  ms: number;
  outcome: string;
}

const recentReqs: ReqRecord[] = [];

export function recentRequests(): ReqRecord[] {
  return recentReqs.slice(-4);
}

function noteReq(rec: ReqRecord): void {
  recentReqs.push(rec);
  if (recentReqs.length > 12) recentReqs.shift();
}

async function req<T>(conn: Pairing, path: string, init?: RequestInit): Promise<T> {
  const started = Date.now();
  // 调用方自己的 signal 也要能生效（附件下载这类），所以两者合并
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), REQUEST_TIMEOUT_MS);
  const forwardAbort = () => ctrl.abort();
  init?.signal?.addEventListener('abort', forwardAbort);
  let res: Response;
  try {
    res = await fetch(`${conn.baseUrl}${path}`, {
      ...init,
      signal: ctrl.signal,
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${conn.token}`,
        ...(init?.headers ?? {}),
      },
    });
  } catch (e) {
    const aborted = ctrl.signal.aborted && !init?.signal?.aborted;
    noteReq({
      at: started,
      method: init?.method ?? 'GET',
      path,
      base: conn.baseUrl,
      ms: Date.now() - started,
      outcome: aborted ? '超时' : `发不出去：${(e as Error)?.message ?? e}`,
    });
    throw e;
  } finally {
    clearTimeout(timer);
    init?.signal?.removeEventListener('abort', forwardAbort);
  }
  if (!res.ok) {
    let code = `http_${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body?.error) code = body.error;
    } catch {
      // 不是 JSON 就用手上的状态码
    }
    noteReq({ at: started, method: init?.method ?? 'GET', path, base: conn.baseUrl, ms: Date.now() - started, outcome: `HTTP ${res.status}` });
    throw new ApiError(res.status, code);
  }
  const out = (await res.json()) as T;
  noteReq({ at: started, method: init?.method ?? 'GET', path, base: conn.baseUrl, ms: Date.now() - started, outcome: 'ok' });
  return out;
}

/** /v1/ping 的答案。`hostname` 是那台机器自己的名字（老服务端没有这一项）。 */
export interface PingResult {
  ok: boolean;
  /** 产品名，每台安装都一样；只用来判明「这是不是一个 Tachi」 */
  name?: string;
  /** 机器名（hostname），手机列表里默认显示它 */
  hostname?: string;
  version?: string;
  /**
   * 这台电脑当前的全部地址，主的那条在前。老服务端没有这一项 —— 那就只有手上这一条。
   * 它让「配对一次」就够了：之后那台电脑换了网络、开了 Tailscale，手机连上就能知道。
   */
  urls?: string[];
}

/** 配对页用它验证地址与令牌；这个端点不要令牌。signal 由并发探测用来放弃输掉的那几条。 */
export async function ping(baseUrl: string, signal?: AbortSignal): Promise<PingResult> {
  const res = await fetch(`${baseUrl.replace(/\/+$/, '')}/v1/ping`, { signal });
  if (!res.ok) throw new ApiError(res.status, `http_${res.status}`);
  return (await res.json()) as PingResult;
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

  /** 删一段对话。正在跑的那一轮会被拒绝（先停下再删），和桌面同一条规矩。 */
  deleteSession: (c: Pairing, id: string) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  /** 置顶 / 取消置顶。位置是列表的事实，成功之后服务端会广播 sessions_changed。 */
  setPinned: (c: Pairing, id: string, pinned: boolean) =>
    req<{ accepted: boolean }>(c, `/v1/sessions/${encodeURIComponent(id)}/pin`, {
      method: 'POST',
      body: JSON.stringify({ pinned }),
    }),

  /**
   * 新建一段对话。项目是可选的；没有项目时就按 workingDir 起一段（空 = ~/workspace，
   * "~" 由服务端展开 —— 那是那台电脑的主目录，不是手机的）。
   */
  createSession: (c: Pairing, opts: { projectId?: string; workingDir?: string }) =>
    req<{ session: SessionSummary }>(c, '/v1/sessions', {
      method: 'POST',
      body: JSON.stringify({ projectId: opts.projectId ?? '', workingDir: opts.workingDir ?? '' }),
    }),

  /** 新建会话时可选的项目 */
  projects: (c: Pairing) => req<{ projects: ProjectRef[] }>(c, '/v1/projects'),

  /**
   * 这台电脑的「保持唤醒」开关（不属于任何一段会话）。没传 durationMin 就沿用存着的偏好 ——
   * 传了的话它同时成为这次的时长。
   */
  awake: (c: Pairing) => req<{ awake: AwakeState }>(c, '/v1/awake'),

  setAwake: (c: Pairing, on: boolean, durationMin?: number) =>
    req<{ awake: AwakeState }>(c, '/v1/awake', {
      method: 'POST',
      body: JSON.stringify(durationMin == null ? { on } : { on, durationMin }),
    }),

  /** 这段对话的三个只读面板（MCP / agent / 定时任务） */
  panels: (c: Pairing, id: string) =>
    req<{ panels: Panels }>(c, `/v1/sessions/${encodeURIComponent(id)}/panels`),

  /** 定时任务：立刻跑一次 / 暂停或恢复 / 删除。三条都走桌面同一条写入路径。 */
  cronRun: (c: Pairing, id: string) =>
    req<{ accepted: boolean }>(c, `/v1/cron/${encodeURIComponent(id)}/run`, { method: 'POST' }),

  cronPause: (c: Pairing, id: string, paused: boolean) =>
    req<{ accepted: boolean }>(c, `/v1/cron/${encodeURIComponent(id)}/pause`, {
      method: 'POST',
      body: JSON.stringify({ paused }),
    }),

  cronDelete: (c: Pairing, id: string) =>
    req<{ accepted: boolean }>(c, `/v1/cron/${encodeURIComponent(id)}`, { method: 'DELETE' }),

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
