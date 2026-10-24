import { useCallback, useEffect, useRef, useState } from 'react';
import { api, ApiError, normalizeSnapshot, type Pairing } from '../api/client';
import { loadDevices, saveDevices, type Device } from '../api/storage';
import { deviceLabel } from '../api/label';
import type { AwakeState, Panels, ProjectRef, SessionSummary } from '../api/types';
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
  /** meta 是配对页从 /v1/ping 拿到的默认名（机器名）与原始 hostname */
  pair: (p: Pairing, meta?: { label?: string; hostname?: string }) => Promise<void>;
  /** 换一台电脑：断开这一条、清掉画面，再连过去 */
  switchDevice: (index: number) => Promise<void>;
  /** 给某一台改个名字：只写这台手机上的存储，不通知那台机器（协议里没有这个端点） */
  renameDevice: (index: number, label: string) => Promise<void>;
  /** 从列表里删掉一台；删的是当前那台时会顺位切到剩下的 */
  removeDevice: (index: number) => Promise<void>;
  unpair: () => Promise<void>;
  refresh: () => Promise<void>;
  open: (id: string) => Promise<void>;
  close: () => void;
  /** true = 服务端收下了；false 时输入框把文字还给读者。运行中请用 queueMessage。 */
  send: (text: string, mode: 'send' | 'steer' | 'interrupt') => Promise<boolean>;
  /**
   * 这一段对话的待发送队列（桌面那份「待发送」的手机版）。运行中发的消息进这里，
   * 不直接进对话 —— 否则会插进正在流的这一轮里，而且和桌面看到的不是一回事。
   */
  pending: string[];
  /** 运行中才该用它；不在跑时调用会立刻发出去 */
  queueMessage: (text: string) => void;
  dropPending: (index: number) => void;
  clearPending: () => void;
  /** 「立即发送 · 打断」：停掉这一轮，等它真的结束了再把队列发出去 */
  sendPendingNow: () => void;
  /** 只打断，不发队列 */
  stop: () => Promise<void>;
  /** 回到前台时用：重读当前会话的快照与列表（App 挂后台期间可能什么都不知道） */
  reloadCurrent: () => Promise<void>;
  /** 往上翻一页更早的历史；还有没有更早的看 snapshot.hasMore */
  loadOlder: () => Promise<void>;
  /** 一页更早的历史正在读（顶部那条提示靠它，也靠它挡住重复请求） */
  olderLoading: boolean;
  /** 定时任务表的版本号：另一块屏改了任务就 +1，面板据此重读 */
  cronVersion: number;
  /** 跑一次 / 暂停 / 恢复 / 删除；成功返回 null，失败返回那句原因 */
  cronAction: (id: string, action: 'run' | 'pause' | 'resume' | 'delete') => Promise<string | null>;
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
  /**
   * 这台电脑的「保持唤醒」开关。null = 还没读到（没配对、那台电脑是旧版本，或者还没请求）。
   * 它是一台机器的值，和「打开着哪段对话」无关。
   */
  awake: AwakeState | null;
  /** 读一次；设置页打开时调 */
  loadAwake: () => Promise<void>;
  /** 开关 / 改时长。成功返回 null，失败返回那句原因（此时会顺手重读一次真实状态）。 */
  setAwake: (on: boolean, durationMin?: number) => Promise<string | null>;
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
  const [cronVersion, setCronVersion] = useState(0);
  const [awake, setAwakeState] = useState<AwakeState | null>(null);
  // 待发送队列按会话存：切走再回来，没发出去的那句还在（桌面也是这么做的）
  const [pending, setPending] = useState<Record<string, string[]>>({});
  const pendingRef = useRef<Record<string, string[]>>({});
  // 「立即发送·打断」之后即使这一轮以 interrupted 结束，也要把队列发出去
  const forceFlush = useRef<Record<string, boolean>>({});
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
    // 这台电脑的开关跟着配对走：换了电脑就先把上一台的值丢掉，别一秒钟里拿别人的状态去画。
    setAwakeState(null);
    if (!pairing) return;
    let cancelled = false;

    void api
      .listSessions(pairing)
      .then((r) => {
        if (cancelled) return;
        setSessions(r.sessions);
        // 连上了就把上一次的「连不上」撤掉 —— 那条横幅不能一直挂着
        setError(null);
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

    // 这台电脑的「保持唤醒」开关（一台机器一个值，所以跟着配对走，不跟着会话）
    void api
      .awake(pairing)
      .then((r) => {
        if (!cancelled) setAwakeState(r.awake);
      })
      .catch(() => {
        // 读不到就只是卡片留空：那台电脑可能还是旧版本，不值得打断这一屏
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

    // 流式文字按帧合并。一条 delta 一次 setState 的话，服务端发多快就渲染多快（一秒
    // 几十次很常见），而一帧的可见结果只取决于这一帧攒下的全部文字 —— 攒到下一帧一次性
    // 吐出去，渲染次数就从「跟服务端同速」变成「跟屏幕同速」。
    const streamBuf = { text: '', thinking: '' };
    let streamFrame: number | null = null;
    // 攒着的字属于哪一段对话。换了会话（或从列表进另一段）之后，上一段没吐完的字
    // 绝不能落到新画面上——那会凭空长出一段没发生过的正文。
    let streamOwner: string | null = null;
    const dropStream = () => {
      if (streamFrame !== null) {
        cancelAnimationFrame(streamFrame);
        streamFrame = null;
      }
      streamBuf.text = '';
      streamBuf.thinking = '';
    };
    const flushStream = () => {
      if (streamFrame !== null) {
        cancelAnimationFrame(streamFrame);
        streamFrame = null;
      }
      const { text, thinking } = streamBuf;
      if (!text && !thinking) return;
      streamBuf.text = '';
      streamBuf.thinking = '';
      setChat((st) => {
        let next = st;
        if (thinking) next = applyEvent(next, { type: 'thinking', text: thinking }, 'mobile').state;
        if (text) next = applyEvent(next, { type: 'text', text }, 'mobile').state;
        return next;
      });
    };
    const queueStream = (kind: 'text' | 'thinking', text: string) => {
      if (streamOwner !== openIdRef.current) {
        dropStream();
        streamOwner = openIdRef.current;
      }
      streamBuf[kind] += text;
      if (streamFrame !== null) return;
      streamFrame = requestAnimationFrame(() => {
        streamFrame = null;
        flushStream();
      });
    };

    const sock = new MobileSocket(pairing, {
      onEvent: (ev) => {
        // 文字先攒着。攒批只在「这一帧里全是文字」时是安全的：快照、工具边界、回合
        // 结束这些事件必须看见它们之前的全部文字，所以下面先 flush 再处理。
        if (ev.type === 'text' || ev.type === 'thinking') {
          if (!openIdRef.current) return;
          queueStream(ev.type, ev.text);
          return;
        }
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
        if (ev.type === 'awake_changed') {
          // 一台机器的开关，谁改的都落在这里：桌面的托盘、另一台手机、以及到点自己关的时候。
          // 带着完整状态，照它画即可 —— 不重读，也就不会有两块屏各说一套的窗口。
          setAwakeState(ev.awake);
          return;
        }
        if (ev.type === 'cron_changed') {
          // 定时任务表是广播的：窗口、另一台手机或调度器自己改了，就让面板重读一次
          setCronVersion((v) => v + 1);
          return;
        }
        if (ev.type === 'session_moved') {
          // 压缩成功把这轮对话写进了子会话，桌面在同一刻换的轨（agent:session_switched）。
          // 订阅是按 id 走的：不跟着搬，画面就停在压缩点、状态永远「在跑」，而 idle 早已
          // 发往子会话了。还没发出去的队列也跟着搬 —— 那是同一段对话里读者排的话。
          const carried = pendingRef.current[ev.from] ?? [];
          if (carried.length > 0) {
            const moved = {
              ...pendingRef.current,
              [ev.to]: [...(pendingRef.current[ev.to] ?? []), ...carried],
            };
            delete moved[ev.from];
            pendingRef.current = moved;
            setPending(moved);
          }
          if (forceFlush.current[ev.from]) {
            delete forceFlush.current[ev.from];
            forceFlush.current[ev.to] = true;
          }
          if (openIdRef.current === ev.from) {
            openIdRef.current = ev.to;
            setOpenId(ev.to);
            // 不清画面：子会话的快照马上就到（mergeSnapshot 见到 id 变了会整段换掉），
            // 先清空的话这一瞬 snapshot 是 undefined，Composer 会以为「没在跑」——读者
            // 正好在这几十毫秒里按发送，就会直发撞上 turn_running。
            socketRef.current?.subscribe(ev.to);
            // 这一轮的 idle 发往新 id，而换订阅晚一步 —— 队列得在这里补发，规则和 idle
            // 那条一样：自然结束就发，被打断/出错就留着让读者决定（「立即发送·打断」
            // 按过的话，打断也算数）。
            const queued = (pendingRef.current[ev.to] ?? []).length > 0;
            const forced = forceFlush.current[ev.to] === true;
            if (queued && (ev.end === 'complete' || forced)) {
              forceFlush.current[ev.to] = false;
              void flushPending('send');
            }
          }
          return;
        }
        // 工具边界：桌面在这一刻把队列「插」进正在跑的轮次（steer）。
        if (ev.type === 'tool_result') {
          const id = openIdRef.current;
          if (id && (pendingRef.current[id] ?? []).length > 0) void flushPending('steer');
        }
        // 一轮真的结束了：自然结束就把队列当成新一轮发出去；被打断/出错则留着让读者决定
        // （桌面的规则一样）。「立即发送·打断」按过的话，打断也算数。
        if (ev.type === 'idle') {
          const id = openIdRef.current;
          if (id) {
            const forced = forceFlush.current[id] === true;
            const queued = (pendingRef.current[id] ?? []).length > 0;
            if (queued && (ev.end === 'complete' || forced)) {
              forceFlush.current[id] = false;
              void flushPending('send');
            }
          }
        }
        if (ev.type === 'lagged') {
          // 丢掉的那段文字已经不可信了：重读会给出权威版本，攒着的旧字混进去只会造出
          // 一段没发生过的正文
          dropStream();
          void reload(openIdRef.current);
          return;
        }
        // 没有打开的会话：这些都是上一段的尾巴，别为一个没人看的画面重画
        if (!openIdRef.current) return;
        // 这个事件要看见它之前的全部文字（快照、工具边界、回合结束都在此列）
        flushStream();
        setChat((st) => {
          const { state, needsReload } = applyEvent(st, ev, 'mobile');
          if (needsReload) void reload(openIdRef.current);
          return state;
        });
      },
      onStatus: (st) => {
        setConn(st);
        // 连接一恢复，之前那条「连不上电脑」就不该再挂着
        if (st === 'open') setError(null);
      },
      onReconnected: () => {
        void reload(openIdRef.current);
        void api.listSessions(pairing).then((r) => setSessions(r.sessions));
      },
    });
    socketRef.current = sock;
    sock.ensureConnected();

    return () => {
      cancelled = true;
      // 卸载后那一帧的 flush 没有意义（还会对着已经离开的画面 setState）
      dropStream();
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
    async (p: Pairing, meta?: { label?: string; hostname?: string }) => {
      const dev: Device = {
        ...p,
        label: meta?.label || deviceLabel({}, p.baseUrl),
        hostname: meta?.hostname,
      };
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

  // 改名是本地的事：devices 是这台手机自己的簿子，那台机器并不知道（也不需要知道）。
  // 它刻意不走 activate —— 改名不该断开并重连一条正在用的 WebSocket。
  const renameDevice = useCallback(
    async (index: number, label: string) => {
      const list = devicesRef.current.slice();
      const at = list[index];
      if (!at) return;
      const name = label.trim();
      if (!name || name === at.label) return;
      list[index] = { ...at, label: name };
      setDevices(list);
      await saveDevices(list, activeDevice);
    },
    [activeDevice],
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
    // 名单页不订阅任何会话：在跑的那段每秒几十条增量，落在一个没人看的画面上就是
    // 纯负担 —— 左滑列表的每一帧都在跟它抢 JS 线程。
    socketRef.current?.subscribe(null);
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
  // 自己发的那条本地先画上（回声会带着 origin=mobile 被忽略）
  const echoLocal = useCallback((text: string) => {
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
  }, []);

  const send = useCallback(
    async (text: string, mode: 'send' | 'steer' | 'interrupt'): Promise<boolean> => {
      let ok = false
      await withError(async () => {
        const id = openIdRef.current;
        if (!pairing || !id) return
        await api.send(pairing, id, text, mode);
        ok = true
        echoLocal(text);
      })
      return ok
    },
    [pairing, withError, echoLocal],
  );

  // 队列的三个原子操作。pendingRef 是事件回调能看到的「最新值」——render 闭包里的
  // state 在回调里是旧的。
  const writePending = useCallback((id: string, list: string[]) => {
    const next = { ...pendingRef.current, [id]: list };
    pendingRef.current = next;
    setPending(next);
  }, []);
  const takePending = useCallback((id: string): string => {
    const text = (pendingRef.current[id] ?? []).join('\n\n');
    if (text) writePending(id, []);
    return text;
  }, [writePending]);

  const queueMessage = useCallback((text: string) => {
    const id = openIdRef.current;
    if (!id) return;
    const list = pendingRef.current[id] ?? [];
    writePending(id, [...list, text]);
  }, [writePending]);

  const dropPending = useCallback((index: number) => {
    const id = openIdRef.current;
    if (!id) return;
    const list = (pendingRef.current[id] ?? []).filter((_, i) => i !== index);
    writePending(id, list);
  }, [writePending]);

  const clearPending = useCallback(() => {
    const id = openIdRef.current;
    if (!id) return;
    writePending(id, []);
  }, [writePending]);

  // 队列出去的时候按「新一轮」发；插话只在工具边界由下面的事件路径走。
  const flushPending = useCallback(async (mode: 'send' | 'steer'): Promise<void> => {
    const id = openIdRef.current;
    if (!pairing || !id) return;
    const text = takePending(id);
    if (!text) return;
    await withError(async () => {
      await api.send(pairing, id, text, mode);
      echoLocal(text);
    });
  }, [pairing, takePending, withError, echoLocal]);

  const stop = useCallback(async () => {
    const id = openIdRef.current;
    if (!pairing || !id) return;
    await withError(() => api.stop(pairing, id));
  }, [pairing, withError]);

  // 「立即发送 · 打断」：先停这一轮，等 idle 到了再把队列发出去（桌面同一条路）。
  // 直接在这里发会撞上 turn_running —— 那一轮还没真的结束。
  const sendPendingNow = useCallback(() => {
    const id = openIdRef.current;
    if (!id) return;
    // 没在跑（比如上一轮被打断后队列还留着）：直接当成新一轮发出去
    if (!chatRef.current.snapshot?.session.running) {
      void flushPending('send');
      return;
    }
    forceFlush.current[id] = true;
    void stop();
  }, [stop, flushPending]);

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

  // 三个动作走同一层：错误就地还给面板，不挂全局横幅（面板自己有位置说）
  const cronAction = useCallback(
    async (id: string, action: 'run' | 'pause' | 'resume' | 'delete'): Promise<string | null> => {
      if (!pairing) return '还没有配对电脑';
      try {
        if (action === 'run') await api.cronRun(pairing, id);
        else if (action === 'delete') await api.cronDelete(pairing, id);
        else await api.cronPause(pairing, id, action === 'pause');
        return null;
      } catch (e) {
        return messageOfError(e);
      }
    },
    [pairing],
  );

  const loadAwake = useCallback(async () => {
    if (!pairing) return;
    try {
      const r = await api.awake(pairing);
      setAwakeState(r.awake);
    } catch {
      // 同上：读不到只是一张空卡片，不挂全局横幅
    }
  }, [pairing]);

  const setAwake = useCallback(
    async (on: boolean, durationMin?: number): Promise<string | null> => {
      if (!pairing) return '还没有配对电脑';
      try {
        const r = await api.setAwake(pairing, on, durationMin);
        setAwakeState(r.awake);
        return null;
      } catch (e) {
        // 服务端说没成功 = 那台机器现在是关的（系统的断言被拒、或者时长不合法）。
        // 重读一次，让开关停在真实状态上，而不是读者刚点的那一下。
        void loadAwake();
        return messageOfError(e);
      }
    },
    [pairing, loadAwake],
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
    renameDevice,
    removeDevice,
    unpair,
    refresh,
    reloadCurrent,
    loadOlder,
    olderLoading,
    cronVersion,
    cronAction,
    open,
    close,
    send,
    pending: openId ? pending[openId] ?? [] : [],
    queueMessage,
    dropPending,
    clearPending,
    sendPendingNow,
    stop,
    decide,
    answer,
    decline,
    setProvider,
    setSkipApproval,
    deleteSession,
    setPinned,
    createSession,
    loadPanels,
    awake,
    loadAwake,
    setAwake,
  };
}

export { ApiError, normalizeSnapshot };
