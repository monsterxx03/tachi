import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AppState } from 'react-native';
import { api, ApiError, normalizeSnapshot, uploadCapMb, type Pairing } from '../api/client';
import { fastestPairing, nextAddress, normalizeAddresses } from '../api/addresses';
import { loadSessionCache, saveSessionCache, type SessionCache } from '../api/cache';
import { deviceKey, loadDevices, saveDevices, type Device } from '../api/storage';
import { deviceLabel } from '../api/label';
import { parseDeepLink } from '../api/link';
import type { PickedFile } from '../api/pick';
import type { AwakeState, Panels, PendingMessage, ProjectRef, SessionSummary, UploadedFile } from '../api/types';
import { MobileSocket, type WsStatus } from '../api/ws';
import { displayOf } from './attach';
import { applyEvent, emptyChat, messageOfError, type ChatState } from './chat';
import { buildOverview, waitingCount, type OverviewRow } from './overview';

/**
 * 掉线触发的「重新比地址」节流。一次掉线会连着重试好几次（1s/2s/4s…），而一次探测最长要
 * 2.5 秒 —— 不节流的话重连期间的探测会自己叠起来。
 */
const DROP_HEAL_GAP_MS = 5_000;
/** 探测全失败时换地址的节流。它比探测间隔长：换过去之后要留出重连的时间，否则会来回跳。 */
const ROTATE_GAP_MS = 12_000;

/**
 * 首页那一列多久重新问一遍每台电脑。
 *
 * 只有正在打开的那一段有实时流；别的段在不在跑、在不在等，电脑不会主动说（广播只在
 * 我们已经订阅着那台时才会收到），所以隔一会儿问一次。前台才问 —— 后台问是拿电量换
 * 一个没人看的数字。
 */
const REFRESH_MS = 15_000;
/** 聚合刷新时每台的等待上限：一台睡着的电脑不该让整列停在「正在读」15 秒 */
const REFRESH_TIMEOUT_MS = 6_000;
/** 会话缓存落盘的节流：一次刷新会把每台都写一遍，没必要每条都碰磁盘 */
const CACHE_SAVE_MS = 2_000;

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
  /** 当前这一台的会话（对话页的 `#s:` 补全、标题查找用）。首页那一列是下面的 overview */
  sessions: SessionSummary[];
  /**
   * 首页那一列：所有电脑的会话拼在一起，等你的在最上面，每行带着机器名。
   *
   * 它是从「每台最后一次读到的列表」拼出来的（`api/cache.ts`），所以某台连不上时那些行
   * 还在 —— 淡化显示，时间说明是几小时前的，而不是整台消失。
   */
  overview: OverviewRow[];
  /** 每台电脑这一轮读到了没有（键是 deviceKey）。false = 这一轮明确失败了，别把它们当成「没有会话」 */
  reachable: Record<string, boolean>;
  /** 每台电脑最后读到列表的时刻（毫秒，0 = 从没读到过） */
  syncedAt: (key: string) => number;
  providers: string[];
  /** 新建会话时可挑的项目（桌面上的那份，只读），跟着正在连的那一台走 */
  projects: ProjectRef[];
  /**
   * 某一台上能挑的项目。
   *
   * 新建会话要先选机器，而项目是**那台**的事：拿着当前这台的列表去建另一台，项目 id 在
   * 那边根本不存在。读不到就返回空数组 —— 「无项目」那条路照样能建。
   */
  projectsFor: (deviceIndex: number) => Promise<ProjectRef[]>;
  openId: string | null;
  chat: ChatState;
  error: string | null;
  /** 收起顶上那条红字（看过、或者它挂得够久了） */
  dismissError: () => void;
  /** meta 是配对页从 /v1/ping 拿到的默认名（机器名）、原始 hostname，以及那台电脑的全部地址 */
  pair: (
    p: Pairing,
    meta?: { label?: string; hostname?: string; urls?: string[]; deviceId?: string; unreachable?: boolean },
  ) => Promise<void>;
  /** 给某一台改个名字：只写这台手机上的存储，不通知那台机器（协议里没有这个端点） */
  renameDevice: (index: number, label: string) => Promise<void>;
  /**
   * 手动指定这台电脑走哪条地址；传 null = 交回自动（会重新比一次）。
   * 手动选中的那条不会被自动切换改掉，直到读者选回自动。
   */
  selectAddress: (index: number, url: string | null) => Promise<void>;
  /** 从列表里删掉一台；删的是当前那台时会顺位切到剩下的 */
  removeDevice: (index: number) => Promise<void>;
  refresh: () => Promise<void>;
  open: (id: string) => Promise<void>;
  /**
   * 打开某台电脑上的某一段：必要时先把它变成正在连的那一台（一条 socket 只连一台）。
   *
   * 对读者是「点哪段开哪段」——切换发生在背后，画面上只是那段对话打开了。
   */
  openOn: (deviceIndex: number, id: string) => Promise<void>;
  /**
   * 在某台电脑上新建一段并打开它。
   *
   * 建的时候直接用目标的地址发请求，不先切连接 —— 会话 id 要在切换之前就知道，才能像
   * 深链那样「先记下、等切过去再打开」。
   */
  createSessionOn: (
    deviceIndex: number,
    opts: { projectId?: string; workingDir?: string },
  ) => Promise<SessionSummary | null>;
  /**
   * 按设备寻址的写操作：删 / 置顶 / 改名。
   *
   * 它们直接拿那一台的地址发请求，**不切连接** —— 切过去会断掉正在跑的那一段的订阅（以及
   * 正在等的授权），就为了改一个标题，代价不成比例。
   */
  deleteSessionOn: (deviceIndex: number, id: string) => Promise<void>;
  setPinnedOn: (deviceIndex: number, id: string, pinned: boolean) => Promise<void>;
  renameSessionOn: (deviceIndex: number, id: string, title: string) => Promise<void>;
  close: () => void;
  /** App 进后台：退订当前会话（宿主据此判断「没人在看」，见 background 的注释） */
  background: () => void;
  /** 一条推送被点开（tachi://session/…）：打开那段对话，必要时先切到那台电脑 */
  handleLink: (url: string) => void;
  /**
   * true = 服务端收下了；false 时输入框把文字还给读者。运行中请用 queueMessage。
   * `files` 是已经传上去的资料（uploadFile 换回来的名字），随这条消息一起交给那台电脑。
   */
  send: (text: string, mode: 'send' | 'steer' | 'interrupt', files?: UploadedFile[]) => Promise<boolean>;
  /**
   * 把一个本地文件传给那台电脑，换回它存下来的名字。成功后由调用方把它挂在待发消息上。
   * 失败回原因（屏幕小，那句原因就是读者唯一能行动的东西），不弹顶部横幅 —— 附件是输入框
   * 自己的事，传不上去不该把整段对话的状态盖住。
   */
  uploadFile: (f: PickedFile) => Promise<{ file?: UploadedFile; error?: string }>;
  /**
   * 这一段对话的待发送队列（桌面那份「待发送」的手机版）。运行中发的消息进这里，
   * 不直接进对话 —— 否则会插进正在流的这一轮里，而且和桌面看到的不是一回事。
   */
  pending: PendingMessage[];
  /** 运行中才该用它；不在跑时调用会立刻发出去 */
  queueMessage: (text: string, files?: UploadedFile[]) => void;
  dropPending: (index: number) => void;
  clearPending: () => void;
  /**
   * 队列里的一条「现在就发」：从队列里摘掉，直接发出去 —— 正在跑就先停这一轮，停下来的
   * 那一刻先发它，队列里剩下的照旧等着。发不出去会放回队尾（读者打的那句话不能弄丢）。
   */
  sendPending: (index: number) => void;
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
  /** 这一段对话的手机推送开关（off = 别推）。全局开关在那台电脑的设置里。 */
  setNotifyOff: (off: boolean) => Promise<void>;
  /**
   * 改标题（当前打开的这一段）。
   *
   * 列表里每一行用的是 `renameSessionOn` —— 它按设备寻址，因为那一行可能在别的机器上。
   */
  renameSession: (id: string, title: string) => Promise<void>;
  /**
   * 当前对话的三个只读面板（MCP / agent / 定时任务）。失败时返回原因而不是塞进全局
   * error —— 面板读不到是这一屏的事，不该在顶上挂一条红色横幅，更不该触发重试风暴。
   */
  loadPanels: () => Promise<{ panels: Panels | null; error: string | null }>;
  /**
   * **当前这一台**的「保持唤醒」开关。null = 还没读到（没配对、那台电脑是旧版本，或者还没请求）。
   * 它是一台机器的值，和「打开着哪段对话」无关。
   */
  awake: AwakeState | null;
  /** 某一台的开关（详情页看的是任意一台，不一定正在连的那一台） */
  awakeOf: (key: string) => AwakeState | null;
  /** 读某一台（缺省 = 正在连的那一台）；详情页打开时调 */
  loadAwake: (deviceIndex?: number) => Promise<void>;
  /** 改某一台的开关 / 时长。成功返回 null，失败返回那句原因（此时会顺手重读一次真实状态）。 */
  setAwake: (deviceIndex: number, on: boolean, durationMin?: number) => Promise<string | null>;
}

export function useMobile(): MobileApi {
  const [ready, setReady] = useState(false);
  const [pairing, setPairing] = useState<Pairing | null>(null);
  const [devices, setDevices] = useState<Device[]>([]);
  const [activeDevice, setActiveDevice] = useState(0);
  const [conn, setConn] = useState<WsStatus | 'idle'>('idle');
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  /**
   * 每台电脑最后一次读到的会话列表（键是 deviceKey）。
   *
   * 首页那一列从这个对象拼出来，所以某台连不上时它上面的行还在（显示成几小时前的样子）。
   * 它也是「当前这一台」的列表的来源 —— 写入时两份一起更新（见 applySessions）。
   */
  const [cache, setCache] = useState<SessionCache>({});
  const cacheRef = useRef<SessionCache>({});
  /** 这一轮读到了没有（键同上）。只有明确失败才记 false —— 见 overview.ts 的注释 */
  const [reachable, setReachable] = useState<Record<string, boolean>>({});
  const [providers, setProviders] = useState<string[]>([]);
  const [projects, setProjects] = useState<ProjectRef[]>([]);
  const [openId, setOpenId] = useState<string | null>(null);
  const [chat, setChat] = useState<ChatState>(emptyChat);
  const [olderLoading, setOlderLoading] = useState(false);
  const [cronVersion, setCronVersion] = useState(0);
  // 「保持唤醒」按机器存：详情页可能正在看一台没在连的电脑
  const [awakeByKey, setAwakeByKey] = useState<Record<string, AwakeState>>({});
  // 待发送队列按会话存：切走再回来，没发出去的那句还在（桌面也是这么做的）
  const [pending, setPending] = useState<Record<string, PendingMessage[]>>({});
  const pendingRef = useRef<Record<string, PendingMessage[]>>({});
  // 「立即发送·打断」之后即使这一轮以 interrupted 结束，也要把队列发出去
  const forceFlush = useRef<Record<string, boolean>>({});
  // 「这一条现在就发」：那一轮停下之后先发它，队列里剩下的照旧留着
  const immediate = useRef<Record<string, PendingMessage>>({});
  // 要打开的会话在另一台电脑上：切过去之后才打开它（一条 socket 只连一台）。
  // 立刻 open 会订阅到旧那一条上，所以「先记下、等配对真的换过去再开」。
  const pendingOpen = useRef<{ sessionId: string; deviceIndex: number } | null>(null);
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

  // 列表与当前项都是 ref 的读者要用的最新值
  const devicesRef = useRef<Device[]>([]);
  devicesRef.current = devices;
  const activeDeviceRef = useRef(0);
  activeDeviceRef.current = activeDevice;

  // 会话缓存：内存立刻更新，文件两秒后落一次（一次刷新会把每台都写一遍，没必要每条都碰磁盘）
  const saveTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const scheduleCacheSave = useCallback(() => {
    if (saveTimer.current) return;
    saveTimer.current = setTimeout(() => {
      saveTimer.current = null;
      void saveSessionCache(cacheRef.current);
    }, CACHE_SAVE_MS);
  }, []);

  /** 记下某台这一轮的列表。它只影响「看」，不参与任何写入判断 */
  const patchCache = useCallback(
    (key: string, list: SessionSummary[]) => {
      const next = { ...cacheRef.current, [key]: { at: Date.now(), sessions: list } };
      cacheRef.current = next;
      setCache(next);
      scheduleCacheSave();
    },
    [scheduleCacheSave],
  );

  /**
   * 写某台电脑的会话列表：缓存（所有设备共用）与「当前这一台」的镜像一起更新。
   *
   * 两份存在的原因是它们回答不同的问题：缓存回答「首页要画什么」，`sessions` 回答「我现在
   * 打开的这台有哪些对话」（对话页的 `#s:` 补全、标题查找）。同一个写入口保证它们不会各说一套。
   */
  const applySessions = useCallback(
    (index: number, list: SessionSummary[]) => {
      const dev = devicesRef.current[index];
      if (!dev) return;
      patchCache(deviceKey(dev), list);
      if (index === activeDeviceRef.current) setSessions(list);
    },
    [patchCache],
  );

  // 每次「这一台电脑」被重新确定（启动 / 切设备 / 回前台 / 掉线）就 +1。地址探测要几百毫秒，
  // 回来时读者可能已经切到另一台了 —— 那次结果只属于上一台，不能写回去。
  const connectSeq = useRef(0);
  // 上一次「掉线后重新比地址」/「没探测结果就换地址」的时刻（各自节流）
  const dropHealAt = useRef(0);
  const rotateAt = useRef(0);

  /**
   * 校正这台电脑的地址：并发试它已知的全部地址，赢的那条记下来，并写回这台手机的存储。
   *
   * 一台电脑的地址是会变的：笔记本换到另一个网络、Tailscale 起来了/停了、DHCP 换了 IP。
   * 手机不该要求用户知道这些 —— 它自己比一次就知道现在该走哪条。
   *
   * 它**不阻塞**界面：调用方先用记住的地址连，这条在后台把结果换上去（真的不用换时什么也不做）。
   */
  /** 把「这台电脑现在走哪条地址」写进存储（下次启动从这里开始试，但仍然会并发复核）。 */
  const rememberAddress = useCallback(
    async (dev: Pairing, baseUrl: string, urls: string[], deviceId?: string) => {
      const list = devicesRef.current.slice();
      const at = list.findIndex((d) => d.token === dev.token && d.baseUrl === dev.baseUrl);
      if (at < 0) return;
      // 已经一模一样就不写：聚合刷新每十几秒会复核一次地址，每次都落盘没有意义
      const cur = list[at];
      const same =
        cur.baseUrl === baseUrl &&
        [...normalizeAddresses(cur)].sort().join('|') === [...urls].sort().join('|') &&
        (!deviceId || cur.deviceId === deviceId);
      if (same) return;
      // deviceId 只在 ping 说了的时候才写：老服务端没有这一项，不能把已经存下的值抹掉。
      list[at] = { ...cur, baseUrl, urls, ...(deviceId ? { deviceId } : {}) };
      setDevices(list);
      await saveDevices(list, activeDeviceRef.current);
    },
    [],
  );

  /**
   * 只校正「不是当前这台」的地址：赢的那条写回存储，**不动正在连的那台**。
   *
   * 聚合列表会把每一台都问一遍，失败的那台很可能只是换了网络。这里不能走 `heal` ——
   * 那个函数在地址变了会 `setPairing`，等于把正在用的连接切到别处去。
   */
  const healQuiet = useCallback(
    async (dev: Device) => {
      if (dev.pinned) return;
      const picked = await fastestPairing(dev);
      if (!picked) return;
      await rememberAddress(dev, picked.pairing.baseUrl, picked.urls, picked.ping.deviceId);
    },
    [rememberAddress],
  );

  /**
   * 把所有电脑的列表重新读一遍，拼成首页那一列。
   *
   * 并发、各带自己的超时：一台睡着的电脑不该让整列停在「正在读」。读到就更新那台的缓存；
   * 读不到就把它标成陈旧（首页淡化那一组），并在后台校正一次地址 —— 只是换了网络的机器，
   * 下一轮刷新就能回来。
   *
   * 失败**不挂全局红条**：几台里坏一台是很正常的事（人在地铁上，家里那台就是连不上），
   * 顶上一条红字会让人以为全都坏了。哪台不在，由 chip 和行上的淡化来说。
   */
  const refreshAll = useCallback(async () => {
    const list = devicesRef.current;
    if (list.length === 0) return;
    await Promise.all(
      list.map(async (d, index) => {
        const key = deviceKey(d);
        try {
          const r = await api.listSessions(d, REFRESH_TIMEOUT_MS);
          applySessions(index, r.sessions);
          setReachable((m) => (m[key] === true ? m : { ...m, [key]: true }));
        } catch {
          setReachable((m) => (m[key] === false ? m : { ...m, [key]: false }));
          void healQuiet(d);
        }
      }),
    );
  }, [applySessions, healQuiet]);

  /**
   * 校正这台电脑的地址：并发试它已知的全部地址，赢的那条记下来，并写回这台手机的存储。
   *
   * `onNothing` 是「一条都应答不了」时的出口：网络烂的时候两条路可能都来不及应答，而那时
   * 留在原地是最坏的选择（人可能已经离开家了）。调用方用它换下一条地址去试。
   *
   * 手动选定过地址的电脑（`pinned`）不做任何自动切换 —— 那是读者明确说过的意思。
   */
  const heal = useCallback(
    async (dev: Pairing, onNothing?: () => void) => {
      if (dev.pinned) {
        // 手动选过地址：自动换地址这一步不该做，但「这台是谁」仍要学 —— 否则一条推送
        // 永远认不出它，点开只会说「还没配对」。
        const picked = await fastestPairing(dev);
        const id = picked?.ping.deviceId;
        if (id && id !== dev.deviceId) {
          await rememberAddress(dev, dev.baseUrl, normalizeAddresses(dev), id);
        }
        return;
      }
      const seq = ++connectSeq.current;
      const picked = await fastestPairing(dev);
      if (seq !== connectSeq.current) return;
      if (!picked) {
        onNothing?.();
        return;
      }
      const { pairing: next, urls } = picked;
      await rememberAddress(dev, next.baseUrl, urls, picked.ping.deviceId);
      // 换了一条才重建连接：同一条地址（只是地址表变新了）不该断开正在用的那条 socket
      if (next.baseUrl !== dev.baseUrl) setPairing(next);
    },
    [rememberAddress],
  );

  /**
   * 换到下一条地址。这是「一条都应答不了」时的兜底：明知这次探不出来，也要真的去试另一条 ——
   * 只在家里存在的那条地址，人离开家之后是永远不会应答的。
   */
  const rotate = useCallback(
    async (dev: Pairing) => {
      if (dev.pinned) return;
      const urls = normalizeAddresses(dev);
      const next = nextAddress(urls, dev.baseUrl);
      if (!next) return;
      await rememberAddress(dev, next, urls);
      setPairing({ ...dev, baseUrl: next, urls });
    },
    [rememberAddress],
  );

  /**
   * 掉线之后重新比一次地址。
   *
   * 这条是「在 App 里换网络」的唯一出路：从家里的 Wi-Fi 走到 5G、或者反过来，能连的那条地址
   * 变了，而 socket 只会拿着旧的那条一直重试。启动/回前台那两处比不到这件事 —— 人一直看着屏幕
   * 的时候，App 根本没离开过前台。
   */
  const healOnDrop = useCallback(
    (dev: Pairing) => {
      if (dev.pinned) return;
      const now = Date.now();
      if (now - dropHealAt.current < DROP_HEAL_GAP_MS) return;
      dropHealAt.current = now;
      void heal(dev, () => {
        // 一条都没应答：别守着旧地址了，按顺序换下一条真的去试
        if (now - rotateAt.current < ROTATE_GAP_MS) return;
        rotateAt.current = now;
        void rotate(dev);
      });
    },
    [heal, rotate],
  );

  // 启动时读一次「记住的电脑」：列表 + 当前那一台；同时把上次看到的会话列表画出来
  useEffect(() => {
    // 缓存先读：设备簿还没出来之前，首页那一列已经有东西可画（而且离线的那几台也在）
    void loadSessionCache().then((c) => {
      cacheRef.current = c;
      setCache(c);
    });
    void loadDevices().then((book) => {
      setDevices(book.devices);
      setActiveDevice(book.active);
      // 同步给 ref：heal 可能在这一帧的渲染之前就跑完，它要用这个下标写回存储
      activeDeviceRef.current = book.active;
      const first = book.devices[book.active] ?? null;
      setPairing(first);
      setReady(true);
      // 记住的地址是「上一次赢的那条」。换了网络之后它可能已经不是最快的，甚至根本不通，
      // 所以在后台再比一次 —— 不挡着界面出来，赢了就换过去。
      if (first) void heal(first);
    });
  }, [heal]);

  /**
   * 首页那一列的节拍：设备簿一就绪先读一遍，之后每十几秒一次。
   *
   * 只有「正在打开的那一段」有实时流；别的段在不在跑、在不在等，那台电脑不会主动说 ——
   * 它的广播只发给我们订阅着的那一条 socket。所以这里定时问一次，退到后台就停。
   */
  useEffect(() => {
    if (!ready || devices.length === 0) return;
    void refreshAll();
    const timer = setInterval(() => {
      if (AppState.currentState === 'active') void refreshAll();
    }, REFRESH_MS);
    return () => clearInterval(timer);
  }, [ready, devices.length, refreshAll]);

  // 有配对就拉列表并挂上连接
  useEffect(() => {
    if (!pairing) return;
    let cancelled = false;

    void api
      .listSessions(pairing)
      .then((r) => {
        if (cancelled) return;
        applySessions(activeDeviceRef.current, r.sessions);
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
        if (cancelled) return;
        const key = deviceKey(pairing);
        setAwakeByKey((m) => ({ ...m, [key]: r.awake }));
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
          // 列表那一行的「在跑 / 在等」：没订阅那段会话的也要看到。
          // 缓存是首页那一列的来源，镜像给对话页的标题查找 —— 两份一起改。
          const index = activeDeviceRef.current;
          const dev = devicesRef.current[index];
          if (dev) {
            const key = deviceKey(dev);
            const entry = cacheRef.current[key];
            if (entry) {
              patchCache(
                key,
                entry.sessions.map((s) =>
                  s.id === ev.sessionId ? { ...s, running: ev.running, waiting: ev.waiting } : s,
                ),
              );
            }
          }
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
              applySessions(activeDeviceRef.current, r.sessions);
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
          // 广播只来我们订阅着的那一台，所以写成它的键。
          const cur = devicesRef.current[activeDeviceRef.current];
          if (cur) {
            const key = deviceKey(cur);
            setAwakeByKey((m) => ({ ...m, [key]: ev.awake }));
          }
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
          if (immediate.current[ev.from]) {
            immediate.current[ev.to] = immediate.current[ev.from];
            delete immediate.current[ev.from];
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
            // 「上滑立即发送」的那一条排在最前：这一轮正是为它停下来的，先发它；
            // 队列里剩下的照旧留着（下一轮自然结束才走）
            const one = immediate.current[id];
            if (one) {
              delete immediate.current[id];
              void sendImmediate(one);
            } else {
              const forced = forceFlush.current[id] === true;
              const queued = (pendingRef.current[id] ?? []).length > 0;
              if (queued && (ev.end === 'complete' || forced)) {
                forceFlush.current[id] = false;
                void flushPending('send');
              }
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
        void refreshAll();
      },
      // 掉线（含心跳判死）：先别急着重连到旧地址，比一遍 —— 赢的那条会通过 setPairing 换上去
      onDisconnected: () => healOnDrop(pairing),
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
  }, [pairing, reload, healOnDrop]);

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
      const at = list.indexOf(next);
      await saveDevices(list, at);
      setActiveDevice(at);
      // 同上：heal 写回存储时读的是这个 ref，不能等一次渲染
      activeDeviceRef.current = at;
      // 每一台都可能不止一条地址：连之前（重新）比一次，赢的那条会被写回存储。
      void heal(next);
      if (next.baseUrl === pairing?.baseUrl && next.token === pairing?.token) {
        // 已经是这一台：不重建连接（重连一次要重读快照，没必要）
        setPairing(next);
        return;
      }
      resetView();
      setPairing(next);
    },
    [pairing, resetView, heal],
  );

  // 同一台电脑再配一次（换了令牌或换了地址）按令牌去重：列表里不该出现两条同一个地址
  const pair = useCallback(
    async (
      p: Pairing,
      meta?: { label?: string; hostname?: string; urls?: string[]; deviceId?: string; unreachable?: boolean },
    ) => {
      const urls = normalizeAddresses({ baseUrl: p.baseUrl, urls: meta?.urls });
      const dev: Device = {
        ...p,
        baseUrl: urls[0] ?? p.baseUrl,
        urls: urls.length > 1 ? urls : undefined,
        label: meta?.label || deviceLabel({}, p.baseUrl),
        hostname: meta?.hostname,
        deviceId: meta?.deviceId || p.deviceId,
      };
      const list = devicesRef.current.slice();
      const at = list.findIndex((d) => d.token === p.token || d.baseUrl === p.baseUrl);
      if (at >= 0) list[at] = dev;
      else list.push(dev);
      await activate(list, at >= 0 ? at : list.length - 1);
      // 探测时一条地址都没应答（那台电脑没开、或眼下这个网络进不去）：设备照样存下来了，
      // 但接下去那条会话列表的请求要等自己的超时才失败 —— 这十几秒里说一句，别让人以为白填了。
      if (meta?.unreachable) {
        setError('已记下这台电脑，但现在连不上（它没开，或不在同一个网络）');
      }
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

  const selectAddress = useCallback(
    async (index: number, url: string | null) => {
      const list = devicesRef.current.slice();
      const dev = list[index];
      if (!dev) return;
      const urls = normalizeAddresses(dev);
      list[index] = {
        ...dev,
        baseUrl: url ?? dev.baseUrl,
        urls: urls.length > 1 ? urls : undefined,
        // null = 交回自动；heal 会立刻比一次，把能动的那条找回来
        pinned: url ? true : undefined,
      };
      rotateAt.current = 0;
      dropHealAt.current = 0;
      await activate(list, index);
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

  /** 下拉刷新 = 把所有电脑都问一遍（不是只问当前这台） */
  const refresh = useCallback(async () => {
    await refreshAll();
  }, [refreshAll]);

  /** 回到前台时用：挂后台期间可能什么都没收到，回来先重读一遍 */
  const reloadCurrent = useCallback(async () => {
    // 从后台回来先重建连接：锁屏期间那条多半已经死了，而它可能还报着 OPEN
    socketRef.current?.forceReconnect();
    await reload(openIdRef.current);
    if (!pairing) return;
    // 回前台最常见的变化是「网络换了」（离开家 / 回到家）：比一次才知道该走哪条地址。
    // 它不挡着下面的读取 —— 真要换地址，那次 setPairing 会自己把连接重建一遍。
    void heal(pairing);
    // 所有机器一起重读：挂后台期间别的机器上也发生了事，只重读当前这台会留下过时的列表
    await refreshAll();
  }, [reload, pairing, heal, refreshAll]);

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

  /**
   * 打开某台电脑上的某一段。
   *
   * 不是当前这一台时先切过去（一条 socket 只连一台），切换会清空画面 —— 所以先把目标记进
   * pendingOpen，等配对真的换过去之后再由下面那个 effect 打开它。会话 id 只在那台机器上
   * 有意义，在旧机器上打开同一个 id 只会得到「没有这段对话」。
   */
  const openOn = useCallback(
    async (deviceIndex: number, id: string) => {
      const list = devicesRef.current;
      if (!list[deviceIndex]) return;
      if (deviceIndex === activeDeviceRef.current) {
        await open(id);
        return;
      }
      pendingOpen.current = { sessionId: id, deviceIndex };
      await activate(list.slice(), deviceIndex);
    },
    [activate, open],
  );

  const close = useCallback(() => {
    setOpenId(null);
    setChat(emptyChat);
    // 名单页不订阅任何会话：在跑的那段每秒几十条增量，落在一个没人看的画面上就是
    // 纯负担 —— 左滑列表的每一帧都在跟它抢 JS 线程。
    socketRef.current?.subscribe(null);
  }, []);

  /**
   * App 进了后台：退订当前会话，但**不**清掉 openId。
   *
   * 宿主用「有没有手机订阅着这段对话」来判断人是不是正在看（推送的四个问题之一）。留在后台
   * 却还订阅着，等于告诉那台电脑「我还在看」—— 于是回合结束时手机不会响，而那正是用户把
   * 手机揣进口袋的时刻。回前台走 reloadCurrent，它会重新订阅。
   */
  const background = useCallback(() => {
    socketRef.current?.subscribe(null);
  }, []);

  /**
   * 一条推送被点开：打开它说的那段对话，必要时先切到它说的那台电脑。
   *
   * 顺序必须是「先切、再开」：会话 id 只在一台机器上有意义，而切设备会重建 socket。所以
   * 切的那一下先记在 pendingOpen 里，等配对真的换过去之后（同一个 commit 的 effect）再打开。
   * 找不到那台机器时说一句 —— 静默什么也不做，点通知的人只会以为坏了。
   */
  const handleLink = useCallback(
    (url: string) => {
      const target = parseDeepLink(url);
      if (!target) return;
      if (target.deviceId) {
        const list = devicesRef.current;
        const at = list.findIndex((d) => d.deviceId === target.deviceId);
        if (at < 0) {
          // 设备簿还没学到这个 id：老配对（配对时还没有这一项）、手动选过地址所以没走过
          // 自动探测，或者 App 刚重开还没来得及比一次。当前这台身上还空着的话，就是它 ——
          // 就地学会再打开，比让人对着一条点不开的通知强。
          const cur = activeDeviceRef.current;
          if (cur >= 0 && list[cur] && !list[cur].deviceId) {
            const next = list.slice();
            next[cur] = { ...next[cur], deviceId: target.deviceId };
            setDevices(next);
            void saveDevices(next, cur);
            void open(target.sessionId);
            return;
          }
          setError('这条通知来自一台还没在这部手机上配对的电脑');
          return;
        }
        if (at !== activeDeviceRef.current) {
          pendingOpen.current = { sessionId: target.sessionId, deviceIndex: at };
          void activate(list.slice(), at);
          return;
        }
      }
      void open(target.sessionId);
    },
    [open, activate],
  );

  // 切设备那一帧的 socket 重建先跑（它声明在上面），这里再打开会话。
  useEffect(() => {
    const p = pendingOpen.current;
    if (!p || !pairing) return;
    if (activeDeviceRef.current !== p.deviceIndex) return;
    pendingOpen.current = null;
    void open(p.sessionId);
  }, [pairing, open]);

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
  // 自己发的那条本地先画上（回声会带着 origin=mobile 被忽略）。
  // 气泡里是「自己的话 + 文件名」，和服务端写进记录的那一句一致（见 ./attach）。
  const echoLocal = useCallback((text: string, files: UploadedFile[] = []) => {
    const shown = displayOf(text, files.map((f) => f.name));
    setChat((st) =>
      st.snapshot
        ? {
            ...st,
            snapshot: {
              ...st.snapshot,
              messages: [
                ...st.snapshot.messages,
                { index: -1, role: 'user', content: shown, displayContent: shown },
              ],
            },
          }
        : st,
    );
  }, []);

  const send = useCallback(
    async (text: string, mode: 'send' | 'steer' | 'interrupt', files: UploadedFile[] = []): Promise<boolean> => {
      let ok = false
      await withError(async () => {
        const id = openIdRef.current;
        if (!pairing || !id) return
        await api.send(pairing, id, text, mode, files.map((f) => f.name));
        ok = true
        echoLocal(text, files);
      })
      return ok
    },
    [pairing, withError, echoLocal],
  );

  const uploadFile = useCallback(
    async (f: PickedFile): Promise<{ file?: UploadedFile; error?: string }> => {
      const id = openIdRef.current;
      if (!pairing || !id) return { error: '没有打开的对话' };
      // 明显太大的不必传完再被拒：上限是那台电脑报回来的那个数（见 uploadCapMb）
      const cap = uploadCapMb();
      if (cap > 0 && f.size && f.size > cap * 1024 * 1024) {
        return { error: `文件太大（上限 ${cap} MB）` };
      }
      try {
        const { file } = await api.uploadFile(pairing, id, f);
        return { file };
      } catch (e) {
        // 「连不上电脑」对诊断毫无帮助：底层到底说了什么一并带上（网络层的报错只有那一句能看）
        const why = e instanceof ApiError ? '' : `（${(e as Error)?.message ?? String(e)}）`;
        return { error: messageOfError(e) + why };
      }
    },
    [pairing],
  );

  // 队列的三个原子操作。pendingRef 是事件回调能看到的「最新值」——render 闭包里的
  // state 在回调里是旧的。
  const writePending = useCallback((id: string, list: PendingMessage[]) => {
    const next = { ...pendingRef.current, [id]: list };
    pendingRef.current = next;
    setPending(next);
  }, []);
  // 队列出去的时候合成一条：几段话之间空一行，文件按排队顺序连起来（附件跟着那条消息走）
  const takePending = useCallback((id: string): PendingMessage => {
    const list = pendingRef.current[id] ?? [];
    const text = list.map((p) => p.text).filter(Boolean).join('\n\n');
    const files = list.flatMap((p) => p.files);
    if (text || files.length) writePending(id, []);
    return { text, files };
  }, [writePending]);

  const queueMessage = useCallback((text: string, files: UploadedFile[] = []) => {
    const id = openIdRef.current;
    if (!id) return;
    const list = pendingRef.current[id] ?? [];
    writePending(id, [...list, { text, files }]);
  }, [writePending]);

  // 一条排队的话「现在就发」。**发不出去就放回队尾**：读者的字已经不在输入框里了，
  // 丢掉它等于把刚打的一句话吞掉。
  const sendImmediate = useCallback(
    async (item: PendingMessage) => {
      const ok = await send(item.text, 'send', item.files);
      if (!ok) queueMessage(item.text, item.files);
    },
    [send, queueMessage],
  );

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
    const { text, files } = takePending(id);
    if (!text && files.length === 0) return;
    await withError(async () => {
      await api.send(pairing, id, text, mode, files.map((f) => f.name));
      echoLocal(text, files);
    });
  }, [pairing, takePending, withError, echoLocal]);

  const stop = useCallback(async () => {
    const id = openIdRef.current;
    if (!pairing || !id) return;
    await withError(() => api.stop(pairing, id));
  }, [pairing, withError]);

  // 队列里的一条「现在就发」（上滑）：从队列里摘掉，直接发出去它。
  // 正在跑就不能直接发 —— 会撞上 turn_running。先停这一轮，idle 到了再发这一条
  // （见事件路径里的 immediate）；队列里剩下的照旧等下一轮。
  const sendPending = useCallback((index: number) => {
    const id = openIdRef.current;
    if (!id) return;
    const list = pendingRef.current[id] ?? [];
    const item = list[index];
    if (!item) return;
    writePending(id, list.filter((_, i) => i !== index));
    if (!chatRef.current.snapshot?.session.running) {
      void sendImmediate(item);
      return;
    }
    immediate.current[id] = item;
    void stop();
  }, [sendImmediate, stop, writePending]);

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

  const setNotifyOff = useCallback(
    (off: boolean) =>
      withError(async () => {
        if (pairing && openIdRef.current) await api.setNotifyOff(pairing, openIdRef.current, off);
      }),
    [pairing, withError],
  );

  /**
   * 删 / 置顶 / 改名：按设备寻址。
   *
   * 直接拿那一台的地址发请求，**不切连接** —— 切过去会断掉正在跑的那一段的订阅（以及
   * 正在等的授权），就为了改一个标题，代价不成比例。
   *
   * 广播（sessions_changed）只会来我们正在订阅的那一台，所以本地先把那一行改掉；当前这
   * 一台随后还会因为广播重读一次，两边结果一样。
   */
  const deleteSessionOn = useCallback(
    (deviceIndex: number, id: string) =>
      withError(async () => {
        const d = devicesRef.current[deviceIndex];
        if (!d) return;
        await api.deleteSession(d, id);
        const entry = cacheRef.current[deviceKey(d)];
        if (entry) applySessions(deviceIndex, entry.sessions.filter((s) => s.id !== id));
        if (deviceIndex === activeDeviceRef.current && openIdRef.current === id) {
          setOpenId(null);
          setChat(emptyChat);
        }
      }),
    [withError, applySessions],
  );

  const setPinnedOn = useCallback(
    (deviceIndex: number, id: string, pinned: boolean) =>
      withError(async () => {
        const d = devicesRef.current[deviceIndex];
        if (!d) return;
        await api.setPinned(d, id, pinned);
        const entry = cacheRef.current[deviceKey(d)];
        if (entry) {
          applySessions(
            deviceIndex,
            entry.sessions.map((s) => (s.id === id ? { ...s, pinned } : s)),
          );
        }
      }),
    [withError, applySessions],
  );

  // 改标题和置顶是同一类事：它是列表里那一行的字段（见上）。这里不先改本地那一份 ——
  // 广播马上就到，两份状态各改一次就该对不上了。
  const renameSessionOn = useCallback(
    (deviceIndex: number, id: string, title: string) =>
      withError(async () => {
        const d = devicesRef.current[deviceIndex];
        if (!d) return;
        await api.renameSession(d, id, title);
        const entry = cacheRef.current[deviceKey(d)];
        if (entry) {
          applySessions(
            deviceIndex,
            entry.sessions.map((s) => (s.id === id ? { ...s, title } : s)),
          );
        }
      }),
    [withError, applySessions],
  );

  /** 对话页顶栏长按改标题：那一段一定在正在连的那一台上 */
  const renameSession = useCallback(
    (id: string, title: string) => renameSessionOn(activeDeviceRef.current, id, title),
    [renameSessionOn],
  );

  /**
   * 在某台电脑上新建一段并打开它。
   *
   * 建的时候直接用目标的地址发请求：会话 id 要在切换之前就知道，才能像深链那样「先记下、
   * 等切过去再打开」。切换本身会清空画面，所以顺序不能反。
   */
  const createSessionOn = useCallback(
    async (
      deviceIndex: number,
      opts: { projectId?: string; workingDir?: string },
    ): Promise<SessionSummary | null> => {
      const d = devicesRef.current[deviceIndex];
      if (!d) return null;
      try {
        const r = await api.createSession(d, opts);
        setError(null);
        const entry = cacheRef.current[deviceKey(d)];
        applySessions(deviceIndex, [r.session, ...(entry?.sessions ?? []).filter((s) => s.id !== r.session.id)]);
        if (deviceIndex !== activeDeviceRef.current) {
          pendingOpen.current = { sessionId: r.session.id, deviceIndex };
          await activate(devicesRef.current.slice(), deviceIndex);
        } else {
          void open(r.session.id);
        }
        return r.session;
      } catch (e) {
        setError(messageOfError(e));
        return null;
      }
    },
    [applySessions, activate, open],
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

  const loadAwake = useCallback(async (deviceIndex?: number) => {
    const d = devicesRef.current[deviceIndex ?? activeDeviceRef.current];
    if (!d) return;
    try {
      const r = await api.awake(d);
      const key = deviceKey(d);
      setAwakeByKey((m) => ({ ...m, [key]: r.awake }));
    } catch {
      // 同上：读不到只是一张空卡片，不挂全局横幅
    }
  }, []);

  const setAwake = useCallback(
    async (deviceIndex: number, on: boolean, durationMin?: number): Promise<string | null> => {
      const d = devicesRef.current[deviceIndex];
      if (!d) return '还没有配对电脑';
      try {
        const r = await api.setAwake(d, on, durationMin);
        const key = deviceKey(d);
        setAwakeByKey((m) => ({ ...m, [key]: r.awake }));
        return null;
      } catch (e) {
        // 服务端说没成功 = 那台机器现在是关的（系统的断言被拒、或者时长不合法）。
        // 重读一次，让开关停在真实状态上，而不是读者刚点的那一下。
        void loadAwake(deviceIndex);
        return messageOfError(e);
      }
    },
    [loadAwake],
  );

  const projectsFor = useCallback(async (deviceIndex: number): Promise<ProjectRef[]> => {
    const d = devicesRef.current[deviceIndex];
    if (!d) return [];
    try {
      const r = await api.projects(d);
      return r.projects;
    } catch {
      // 读不到项目只是少一个下拉框：无项目那条路照样能建
      return [];
    }
  }, []);

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

  /** 当前这一台的开关（其余代码用；详情页用 awakeOf 按 key 取） */
  const awake = useMemo(() => {
    const d = devices[activeDevice];
    return d ? awakeByKey[deviceKey(d)] ?? null : null;
  }, [devices, activeDevice, awakeByKey]);

  const awakeOf = useCallback((key: string) => awakeByKey[key] ?? null, [awakeByKey]);

  /**
   * 首页那一列：所有电脑的会话拼在一起。
   *
   * 依赖 cache 而不是 sessions —— 后者只是正在连的那一台的镜像，连不上的机器要从缓存里来。
   */
  const overview = useMemo(
    () => buildOverview(devices, cache, reachable),
    [devices, cache, reachable],
  );
  const syncedAt = useCallback((key: string) => cacheRef.current[key]?.at ?? 0, []);

  return {
    // 顶上那条红字没有自己的出路：一次失败的发送留下的原因会一直挂在这里，直到下一次
    // 同类操作成功。读者看过了就该能收起来（App 那条横幅点一下就调它），而且没人点的时候
    // 它也会自己退场 —— 见 App.tsx 的 NoticeBar。
    dismissError: () => setError(null),
    ready,
    pairing,
    devices,
    activeDevice,
    conn,
    sessions,
    overview,
    reachable,
    syncedAt,
    providers,
    projects,
    projectsFor,
    openId,
    chat,
    error,
    pair,
    renameDevice,
    selectAddress,
    removeDevice,
    refresh,
    reloadCurrent,
    loadOlder,
    olderLoading,
    cronVersion,
    cronAction,
    open,
    openOn,
    createSessionOn,
    deleteSessionOn,
    setPinnedOn,
    renameSessionOn,
    close,
    background,
    handleLink,
    send,
    uploadFile,
    pending: openId ? pending[openId] ?? [] : [],
    queueMessage,
    dropPending,
    clearPending,
    sendPending,
    sendPendingNow,
    stop,
    decide,
    answer,
    decline,
    setProvider,
    setSkipApproval,
    setNotifyOff,
    renameSession,
    loadPanels,
    awake,
    awakeOf,
    loadAwake,
    setAwake,
  };
}

export { ApiError, normalizeSnapshot };
