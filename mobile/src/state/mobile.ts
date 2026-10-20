import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError, normalizeSnapshot, type Pairing } from '../api/client';
import { deviceLabel, loadDevices, saveDevices, type Device } from '../api/storage';
import type { Panels, ProjectRef, SessionSummary } from '../api/types';
import { MobileSocket, type WsStatus } from '../api/ws';
import { applyEvent, emptyChat, messageOfError, type ChatState } from './chat';

// 这一层只管副作用（连接、存储、请求）；事件的归并是纯函数，在 ./chat 里，单独测
export { applyEvent, emptyChat, emptyLive, messageOfError } from './chat';
export type { ChatState, Live, ToolRun } from './chat';

export interface MobileApi {
  ready: boolean;
  pairing: Pairing | null;
  /** 这台手机记住的电脑（家里 / 公司各一台），同一时刻只连其中一台 */
  devices: Device[];
  /** devices 里当前那一台的下标 */
  activeDevice: number;
  conn: WsStatus | 'idle';
  sessions: SessionSummary[];
  providers: string[];
  /** 新建会话时可挑的项目（桌面上的那份，只读） */
  projects: ProjectRef[];
  openId: string | null;
  chat: ChatState;
  error: string | null;
  /** label 是配对页从 /v1/ping 拿到的名字（+ 主机） */
  pair: (p: Pairing, label?: string) => Promise<void>;
  /** 换一台电脑：断开这一条、清掉画面，再连过去 */
  switchDevice: (index: number) => Promise<void>;
  /** 从列表里删掉一台；删的是当前那台时会顺位切到剩下的 */
  removeDevice: (index: number) => Promise<void>;
  unpair: () => Promise<void>;
  refresh: () => Promise<void>;
  open: (id: string) => Promise<void>;
  close: () => void;
  /** true = 服务端收下了；false 时输入框把文字还给读者 */
  send: (text: string, mode: 'send' | 'steer' | 'interrupt') => Promise<boolean>;
  /** 回到前台时用：重读当前会话的快照与列表（App 挂后台期间可能什么都不知道） */
  reloadCurrent: () => Promise<void>;
  /** 往上翻一页更早的历史；还有没有更早的看 snapshot.hasMore */
  loadOlder: () => Promise<void>;
  /** 一页更早的历史正在读（顶部那条提示靠它，也靠它挡住重复请求） */
  olderLoading: boolean;
  decide: (toolId: string, decision: 'allow_once' | 'deny') => Promise<void>;
  answer: (answers: Record<string, string>) => Promise<void>;
  decline: () => Promise<void>;
  setProvider: (name: string) => Promise<void>;
  setSkipApproval: (skip: boolean) => Promise<void>;
  /** 删一段对话；服务端会广播 sessions_changed，列表自己回到一致 */
  deleteSession: (id: string) => Promise<void>;
  /** 置顶 / 取消置顶。位置由服务端广播的 sessions_changed 落到每一块屏上。 */
  setPinned: (id: string, pinned: boolean) => Promise<void>;
  /** 新建一段对话；成功时返回新的一行（失败返回 null，原因在 error 上） */
  createSession: (opts: { projectId?: string; workingDir?: string }) => Promise<SessionSummary | null>;
  /**
   * 当前对话的三个只读面板（MCP / agent / 定时任务）。失败时返回原因而不是塞进全局
   * error —— 面板读不到是这一屏的事，不该在顶上挂一条红色横幅，更不该触发重试风暴。
   */
  loadPanels: () => Promise<{ panels: Panels | null; error: string | null }>;
}

export function useMobile(): MobileApi {
  const [ready, setReady] = useState(false);
  const [pairing, setPairing] = useState<Pairing | null>(null);
  const [devices, setDevices] = useState<Device[]>([]);
  const [activeDevice, setActiveDevice] = useState(0);
  const [conn, setConn] = useState<WsStatus | 'idle'>('idle');
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [providers, setProviders] = useState<string[]>([]);
  const [projects, setProjects] = useState<ProjectRef[]>([]);
  const [openId, setOpenId] = useState<string | null>(null);
  const [chat, setChat] = useState<ChatState>(emptyChat);
  const [olderLoading, setOlderLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const socketRef = useRef<MobileSocket | null>(null);
  // 事件回调里要读到最新的会话 id，用 ref 避开闭包
  const openIdRef = useRef<string | null>(null);
  openIdRef.current = openId;
  // 翻页要读「画面上最旧的那条」，那只有渲染后的 chat 里有
  const chatRef = useRef<ChatState>(chat);
  chatRef.current = chat;
  // 一页正在飞的时候不要再来一页：滚动事件一秒能来几十次
  const pagingRef = useRef(false);

  const reload = useCallback(
    async (id: string | null) => {
      if (!pairing || !id) return;
      // 快照只能从 WS 的 open 拿（协议里那条：记录是权威的，重读快照 —— 它带着
      // 待答项、状态、用量；HTTP 的 page 只管历史）
      socketRef.current?.subscribe(id);
    },
    [pairing],
  );

  // 启动时读一次「记住的电脑」：列表 + 当前那一台
  useEffect(() => {
    void loadDevices().then((book) => {
      setDevices(book.devices);
      setActiveDevice(book.active);
      setPairing(book.devices[book.active] ?? null);
      setReady(true);
    });
  }, []);
  // 列表与当前项都是 ref 的读者要用的最新值
  const devicesRef = useRef<Device[]>([]);
  devicesRef.current = devices;

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

    // 新建会话时能挑的项目
    void api
      .projects(pairing)
      .then((r) => {
        if (!cancelled) setProjects(r.projects);
      })
      .catch(() => {
        // 同上：没有项目可选时「无项目」那条路照样能用
      });

    const sock = new MobileSocket(pairing, {
      onEvent: (ev) => {
        if (ev.type === 'session_state') {
          // 列表那一行的「在跑 / 在等」：没订阅那段会话的也要看到
          setSessions((list) =>
            list.map((s) =>
              s.id === ev.sessionId ? { ...s, running: ev.running, waiting: ev.waiting } : s,
            ),
          );
          return;
        }
        if (ev.type === 'sessions_changed') {
          // 会话列表是广播的：桌面新建 / 删除 / 重命名 / 置顶，或另一台手机做了这些，这里重读一次。
          // 打开着的那段也要跟着改：标题是那一行的全部身份，重读列表却不修快照的话，
          // 桌面上改了名字的对话在这里一直显示旧名（issue #57）。
          void api
            .listSessions(pairing)
            .then((r) => {
              setSessions(r.sessions);
              const id = openIdRef.current;
              if (!id) return;
              const row = r.sessions.find((s) => s.id === id);
              if (!row) {
                // 在别处被删了：停在一条不存在的对话上没有意义
                setOpenId(null);
                setChat(emptyChat);
                return;
              }
              setChat((st) =>
                st.snapshot
                  ? {
                      ...st,
                      snapshot: {
                        ...st.snapshot,
                        session: {
                          ...st.snapshot.session,
                          title: row.title,
                          projectName: row.projectName,
                          pinned: row.pinned,
                          readOnly: row.readOnly,
                          heldByTask: row.heldByTask,
                        },
                      },
                    }
                  : st,
              );
            })
            .catch(() => undefined);
          return;
        }
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

  /** 断开当前这一屏：换电脑 / 删设备之后都不该留着上一台的会话 */
  const resetView = useCallback(() => {
    setOpenId(null);
    setChat(emptyChat);
    setSessions([]);
    setProviders([]);
    setProjects([]);
    setError(null);
  }, []);

  const activate = useCallback(
    async (list: Device[], index: number) => {
      setDevices(list);
      setActiveDevice(index);
      if (list.length === 0) {
        await saveDevices([], 0);
        resetView();
        setPairing(null);
        return;
      }
      const next = list[index] ?? list[0];
      await saveDevices(list, list.indexOf(next));
      setActiveDevice(list.indexOf(next));
      if (next.baseUrl === pairing?.baseUrl && next.token === pairing?.token) {
        // 已经是这一台：不重建连接（重连一次要重读快照，没必要）
        setPairing(next);
        return;
      }
      resetView();
      setPairing(next);
    },
    [pairing, resetView],
  );

  // 同一台电脑再配一次（换了令牌）按地址去重：列表里不该出现两条同一个地址
  const pair = useCallback(
    async (p: Pairing, label?: string) => {
      const dev: Device = { ...p, label: label || deviceLabel('', p.baseUrl) };
      const list = devicesRef.current.slice();
      const at = list.findIndex((d) => d.baseUrl === p.baseUrl);
      if (at >= 0) list[at] = dev;
      else list.push(dev);
      await activate(list, at >= 0 ? at : list.length - 1);
    },
    [activate],
  );

  const switchDevice = useCallback(
    async (index: number) => {
      const list = devicesRef.current;
      if (index < 0 || index >= list.length) return;
      await activate(list.slice(), index);
    },
    [activate],
  );

  const removeDevice = useCallback(
    async (index: number) => {
      const list = devicesRef.current.slice();
      if (index < 0 || index >= list.length) return;
      list.splice(index, 1);
      if (list.length === 0) {
        await activate([], 0);
        return;
      }
      // 删的是前面某一台时，当前那台的下标要往前挪一格
      const next = index < activeDevice ? activeDevice - 1 : Math.min(activeDevice, list.length - 1);
      await activate(list, next);
    },
    [activate, activeDevice],
  );

  const unpair = useCallback(async () => {
    await removeDevice(activeDevice);
  }, [removeDevice, activeDevice]);

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
    // 从后台回来先重建连接：锁屏期间那条多半已经死了，而它可能还报着 OPEN
    socketRef.current?.forceReconnect();
    await reload(openIdRef.current);
    if (!pairing) return;
    try {
      const r = await api.listSessions(pairing);
      setSessions(r.sessions);
    } catch {
      // 下一次刷新再说
    }
  }, [reload, pairing]);

  /**
   * 上滑翻页：游标是画面上最旧一条的记录序号（协议里那条：不是时间戳 —— 时间戳会撞）。
   * 合并之后 hasMore 用这一页自己的答案，否则到了最早一页还会一直请求。
   */
  const loadOlder = useCallback(async () => {
    const id = openIdRef.current;
    const snap = chatRef.current.snapshot;
    if (!pairing || !id || !snap || !snap.hasMore || pagingRef.current) return;
    const first = snap.messages.find((m) => m.index >= 0);
    if (!first) return;
    pagingRef.current = true;
    setOlderLoading(true);
    try {
      const r = await api.page(pairing, id, first.index);
      setChat((st) => {
        if (!st.snapshot) return st;
        const seen = new Set(st.snapshot.messages.map((m) => m.index));
        const older = r.messages.filter((m) => !seen.has(m.index));
        return {
          ...st,
          snapshot: {
            ...st.snapshot,
            messages: [...older, ...st.snapshot.messages],
            hasMore: r.hasMore,
          },
        };
      });
      setError(null);
    } catch (e) {
      setError(messageOfError(e));
    } finally {
      pagingRef.current = false;
      setOlderLoading(false);
    }
  }, [pairing]);

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

  // send answers whether the message actually went out. The composer needs that answer to
  // keep the reader's words on a refusal: the server's reason is on screen (the banner), and a
  // box that emptied itself leaves them retyping what it just explained.
  const send = useCallback(
    async (text: string, mode: 'send' | 'steer' | 'interrupt'): Promise<boolean> => {
      let ok = false
      await withError(async () => {
        const id = openIdRef.current;
        if (!pairing || !id) return
        await api.send(pairing, id, text, mode);
        ok = true
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
      })
      return ok
    },
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

  /** 删一段对话。删除成功后服务端会广播 sessions_changed，列表靠那条事件回到一致。 */
  const deleteSession = useCallback(
    (id: string) =>
      withError(async () => {
        if (!pairing) return;
        await api.deleteSession(pairing, id);
        if (openIdRef.current === id) {
          setOpenId(null);
          setChat(emptyChat);
        }
      }),
    [pairing, withError],
  );

  /** 置顶 / 取消置顶。列表的次序由服务端广播的 sessions_changed 重新读出来。 */
  const setPinned = useCallback(
    (id: string, pinned: boolean) =>
      withError(async () => {
        if (pairing) await api.setPinned(pairing, id, pinned);
      }),
    [pairing, withError],
  );

  const loadPanels = useCallback(async (): Promise<{ panels: Panels | null; error: string | null }> => {
    const id = openIdRef.current;
    if (!pairing || !id) return { panels: null, error: '没有打开着的会话' };
    try {
      const r = await api.panels(pairing, id);
      return { panels: r.panels, error: null };
    } catch (e) {
      // 路由不存在的 404 ≠ 会话不存在（那个会回 no_session）：是那台电脑上的桌面端旧了
      const error =
        e instanceof ApiError && e.code === 'http_404'
          ? '这台电脑上还没有面板，桌面端可能是旧版本'
          : messageOfError(e);
      return { panels: null, error };
    }
  }, [pairing]);

  // 新建会话：服务端建的是真正的那一段（记录在电脑上），这里只把它插进列表并打开。
  // 两件事都由 sessions_changed 再确认一次 —— 但它可能比这个响应先到，所以本地也插一行，
  // 插的时候按 id 去重。
  const createSession = useCallback(
    async (opts: { projectId?: string; workingDir?: string }): Promise<SessionSummary | null> => {
      if (!pairing) return null;
      try {
        const r = await api.createSession(pairing, opts);
        setError(null);
        setSessions((list) => [r.session, ...list.filter((s) => s.id !== r.session.id)]);
        return r.session;
      } catch (e) {
        setError(messageOfError(e));
        return null;
      }
    },
    [pairing],
  );

  return {
    ready,
    pairing,
    devices,
    activeDevice,
    conn,
    sessions,
    providers,
    projects,
    openId,
    chat,
    error,
    pair,
    switchDevice,
    removeDevice,
    unpair,
    refresh,
    reloadCurrent,
    loadOlder,
    olderLoading,
    open,
    close,
    send,
    decide,
    answer,
    decline,
    setProvider,
    setSkipApproval,
    deleteSession,
    setPinned,
    createSession,
    loadPanels,
  };
}

export { ApiError, normalizeSnapshot };
