import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError, normalizeSnapshot, type Pairing } from '../api/client';
import { loadPairing, savePairing } from '../api/storage';
import type { ServerEvent, SessionMessage, SessionSummary, Snapshot } from '../api/types';
import { MobileSocket, type WsStatus } from '../api/ws';

/** 正在跑的这一轮：文字在流、工具在转。轮次一结束就清掉，改用记录（记录是权威的）。 */
export interface ToolRun {
  id: string;
  name: string;
  title: string;
  result?: string;
  isError?: boolean;
}

export interface Live {
  active: boolean;
  text: string;
  thinking: string;
  tools: ToolRun[];
}

export const emptyLive: Live = { active: false, text: '', thinking: '', tools: [] };

export interface ChatState {
  snapshot: Snapshot | null;
  live: Live;
}

export const emptyChat: ChatState = { snapshot: null, live: emptyLive };

/**
 * 一条事件怎么落到界面上。**纯函数**：好单测，也和连接、存储那些副作用分开。
 *
 * 边界（轮次结束、重连、lagged）不在这里处理 —— 它们要重读快照，那是副作用，
 * 由调用方看到 `needsReload` 后去做。
 */
export function applyEvent(st: ChatState, ev: ServerEvent, selfOrigin: string): { state: ChatState; needsReload: boolean } {
  const same = (s: ChatState) => ({ state: s, needsReload: false });

  switch (ev.type) {
    case 'snapshot':
      return same({ snapshot: normalizeSnapshot(ev.snapshot), live: emptyLive });

    case 'text':
      return same({ ...st, live: { ...st.live, active: true, text: st.live.text + ev.text } });

    case 'thinking':
      return same({ ...st, live: { ...st.live, active: true, thinking: st.live.thinking + ev.text } });

    case 'tool_start':
      return same({
        ...st,
        live: { ...st.live, active: true, tools: [...st.live.tools, { id: ev.toolId, name: ev.toolName, title: ev.title }] },
      });

    case 'tool_result':
      return same({
        ...st,
        live: { ...st.live, tools: st.live.tools.map((t) => (t.id === ev.toolId ? { ...t, result: ev.result, isError: ev.isError } : t)) },
      });

    case 'state': {
      if (!st.snapshot) return same(st);
      const running = ev.state.status !== 'idle' && ev.state.status !== 'waiting';
      return same({
        ...st,
        snapshot: {
          ...st.snapshot,
          state: ev.state,
          session: { ...st.snapshot.session, running, waiting: ev.state.waiting },
        },
      });
    }

    case 'user_message': {
      // 另一块屏发来的才画；自己发的那条本地已经加过了
      if (!st.snapshot || ev.origin === selfOrigin) return same(st);
      const m: SessionMessage = {
        index: -1,
        role: 'user',
        content: ev.text,
        displayContent: ev.displayText ?? ev.text,
      };
      return same({ ...st, snapshot: { ...st.snapshot, messages: [...st.snapshot.messages, m] } });
    }

    // 这三样都要重读一次快照：记录是权威的，实时流只负责延长尾巴
    case 'turn':
    case 'error':
    case 'idle':
    case 'lagged':
    case 'ask':
    case 'ask_answered':
    case 'compact_done':
      return { state: st, needsReload: true };

    default:
      return same(st);
  }
}

/** 服务端那些拒绝原因翻成一句人话（协议里那张表） */
export function messageOfError(e: unknown): string {
  if (e instanceof ApiError) {
    switch (e.code) {
      case 'unauthorized':
        return '令牌已失效，请在电脑上重新配对';
      case 'no_session':
        return '这段对话已经不在了';
      case 'turn_running':
        return '它正在跑；插话或打断？';
      case 'not_running':
        return '这一轮已经结束了，直接发送？';
      case 'steer_unsupported':
        return '这轮不接受插话，等它结束或打断';
      case 'no_such_pending_request':
        return '已经在别处回答过了';
      case 'read_only':
        return '这段对话由别的任务占用';
      case 'held_by_task':
        return '有一项远端任务正在使用它';
    }
    return `出错了（${e.code}）`;
  }
  return '连不上电脑（没开机、或不在同一个网络）';
}

export interface MobileApi {
  ready: boolean;
  pairing: Pairing | null;
  conn: WsStatus | 'idle';
  sessions: SessionSummary[];
  providers: string[];
  openId: string | null;
  chat: ChatState;
  error: string | null;
  pair: (p: Pairing) => Promise<void>;
  unpair: () => Promise<void>;
  refresh: () => Promise<void>;
  open: (id: string) => Promise<void>;
  close: () => void;
  send: (text: string, mode: 'send' | 'steer' | 'interrupt') => Promise<void>;
  decide: (toolId: string, decision: 'allow_once' | 'deny') => Promise<void>;
  answer: (answers: Record<string, string>) => Promise<void>;
  decline: () => Promise<void>;
  setProvider: (name: string) => Promise<void>;
  setSkipApproval: (skip: boolean) => Promise<void>;
}

export function useMobile(): MobileApi {
  const [ready, setReady] = useState(false);
  const [pairing, setPairing] = useState<Pairing | null>(null);
  const [conn, setConn] = useState<WsStatus | 'idle'>('idle');
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [providers, setProviders] = useState<string[]>([]);
  const [openId, setOpenId] = useState<string | null>(null);
  const [chat, setChat] = useState<ChatState>(emptyChat);
  const [error, setError] = useState<string | null>(null);

  const socketRef = useRef<MobileSocket | null>(null);
  // 事件回调里要读到最新的会话 id，用 ref 避开闭包
  const openIdRef = useRef<string | null>(null);
  openIdRef.current = openId;

  const reload = useCallback(
    async (id: string | null) => {
      if (!pairing || !id) return;
      // 快照只能从 WS 的 open 拿（协议里那条：记录是权威的，重读快照 —— 它带着
      // 待答项、状态、用量；HTTP 的 page 只管历史）
      socketRef.current?.subscribe(id);
    },
    [pairing],
  );

  // 启动时读一次配对
  useEffect(() => {
    void loadPairing().then((p) => {
      setPairing(p);
      setReady(true);
    });
  }, []);

  // 有配对就拉列表并挂上连接
  useEffect(() => {
    if (!pairing) return;
    let cancelled = false;

    void api
      .listSessions(pairing)
      .then((r) => {
        if (!cancelled) setSessions(r.sessions);
      })
      .catch((e) => {
        if (!cancelled) setError(messageOfError(e));
      });

    // 可切换的模型（全局的，不随会话变）
    void api
      .providers(pairing)
      .then((r) => {
        if (!cancelled) setProviders(r.providers.map((p) => p.name));
      })
      .catch(() => {
        // 拉不到就只是列表空着，不值得打断这一屏
      });

    const sock = new MobileSocket(pairing, {
      onEvent: (ev) => {
        if (ev.type === 'lagged') {
          void reload(openIdRef.current);
          return;
        }
        setChat((st) => {
          const { state, needsReload } = applyEvent(st, ev, 'mobile');
          if (needsReload) void reload(openIdRef.current);
          return state;
        });
      },
      onStatus: setConn,
      onReconnected: () => {
        void reload(openIdRef.current);
        void api.listSessions(pairing).then((r) => setSessions(r.sessions));
      },
    });
    socketRef.current = sock;
    sock.ensureConnected();

    return () => {
      cancelled = true;
      sock.close();
      socketRef.current = null;
    };
  }, [pairing, reload]);

  const pair = useCallback(async (p: Pairing) => {
    await savePairing(p);
    setError(null);
    setPairing(p);
  }, []);

  const unpair = useCallback(async () => {
    await savePairing(null);
    setOpenId(null);
    setChat(emptyChat);
    setSessions([]);
    setPairing(null);
  }, []);

  const refresh = useCallback(async () => {
    if (!pairing) return;
    try {
      const r = await api.listSessions(pairing);
      setSessions(r.sessions);
      setError(null);
    } catch (e) {
      setError(messageOfError(e));
    }
  }, [pairing]);

  const open = useCallback(
    async (id: string) => {
      setOpenId(id);
      setChat(emptyChat);
      socketRef.current?.subscribe(id);
    },
    [],
  );

  const close = useCallback(() => {
    setOpenId(null);
    setChat(emptyChat);
  }, []);

  const withError = useCallback(async (fn: () => Promise<unknown>) => {
    try {
      await fn();
      setError(null);
    } catch (e) {
      setError(messageOfError(e));
    }
  }, []);

  const send = useCallback(
    (text: string, mode: 'send' | 'steer' | 'interrupt') =>
      withError(async () => {
        const id = openIdRef.current;
        if (!pairing || !id) return;
        await api.send(pairing, id, text, mode);
        // 自己发的那条本地先画上（回声会带着 origin=mobile 被忽略）
        setChat((st) =>
          st.snapshot
            ? {
                ...st,
                snapshot: {
                  ...st.snapshot,
                  messages: [...st.snapshot.messages, { index: -1, role: 'user', content: text, displayContent: text }],
                },
              }
            : st,
        );
      }),
    [pairing, withError],
  );

  const decide = useCallback(
    (toolId: string, decision: 'allow_once' | 'deny') =>
      withError(async () => {
        if (pairing && openIdRef.current) await api.decide(pairing, openIdRef.current, toolId, decision);
      }),
    [pairing, withError],
  );

  const answer = useCallback(
    (answers: Record<string, string>) =>
      withError(async () => {
        if (pairing && openIdRef.current) await api.answer(pairing, openIdRef.current, answers);
      }),
    [pairing, withError],
  );

  const decline = useCallback(
    () =>
      withError(async () => {
        if (pairing && openIdRef.current) await api.decline(pairing, openIdRef.current);
      }),
    [pairing, withError],
  );

  const setProvider = useCallback(
    (name: string) =>
      withError(async () => {
        if (pairing && openIdRef.current) await api.setProvider(pairing, openIdRef.current, name);
      }),
    [pairing, withError],
  );

  const setSkipApproval = useCallback(
    (skip: boolean) =>
      withError(async () => {
        if (pairing && openIdRef.current) await api.setSkipApproval(pairing, openIdRef.current, skip);
      }),
    [pairing, withError],
  );

  return {
    ready,
    pairing,
    conn,
    sessions,
    providers,
    openId,
    chat,
    error,
    pair,
    unpair,
    refresh,
    open,
    close,
    send,
    decide,
    answer,
    decline,
    setProvider,
    setSkipApproval,
  };
}
