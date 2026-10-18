import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError, normalizeSnapshot, type Pairing } from '../api/client';
import { loadPairing, savePairing } from '../api/storage';
import type { SessionSummary } from '../api/types';
import { MobileSocket, type WsStatus } from '../api/ws';
import { applyEvent, emptyChat, messageOfError, type ChatState } from './chat';

// 这一层只管副作用（连接、存储、请求）；事件的归并是纯函数，在 ./chat 里，单独测
export { applyEvent, emptyChat, emptyLive, messageOfError } from './chat';
export type { ChatState, Live, ToolRun } from './chat';

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
  /** 回到前台时用：重读当前会话的快照与列表（App 挂后台期间可能什么都不知道） */
  reloadCurrent: () => Promise<void>;
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

  /** 回到前台时用：挂后台期间可能什么都没收到，回来先重读一遍 */
  const reloadCurrent = useCallback(async () => {
    await reload(openIdRef.current);
    if (!pairing) return;
    try {
      const r = await api.listSessions(pairing);
      setSessions(r.sessions);
    } catch {
      // 下一次刷新再说
    }
  }, [reload, pairing]);

  const open = useCallback(async (id: string) => {
    setOpenId(id);
    setChat(emptyChat);
    socketRef.current?.subscribe(id);
  }, []);

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
                  messages: [
                    ...st.snapshot.messages,
                    { index: -1, role: 'user', content: text, displayContent: text },
                  ],
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
    reloadCurrent,
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

export { ApiError, normalizeSnapshot };
