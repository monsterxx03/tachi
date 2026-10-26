import type { Pairing } from './client';
import type { ServerEvent } from './types';

export type WsStatus = 'connecting' | 'open' | 'closed';

export interface WsOptions {
  onEvent: (ev: ServerEvent) => void;
  onStatus?: (s: WsStatus) => void;
  /** 重连成功时触发：上层要重读一次快照（记录是权威的，流只延长尾巴） */
  onReconnected?: () => void;
  /**
   * 每掉一次连接就触发（含心跳看门狗判死的那次）。上层的用处只有一个：重新比一遍这台电脑的
   * 地址 —— 手机换了网络（Wi-Fi ↔ 5G）时，能用的那条会变，而 socket 只会拿着旧的那条重试。
   */
  onDisconnected?: () => void;
}

const HEARTBEAT_MS = 20_000;
/**
 * 一次连接尝试最多等多久。
 *
 * 这一条是「一直显示正在连接，就是不换地址」的根因：连一个够不着的地址时，RN 的 WebSocket
 * 既不 open 也不 close（TCP 在后台慢慢重传），于是上层永远收不到「掉线」，也就永远不会去试
 * 下一条地址。10 秒还没握上手，就当这一次失败。
 */
const CONNECT_TIMEOUT_MS = 10_000;
/**
 * 一次心跳最多等多久回音。换网络时最常见的坏法是「半开」：链路没了，但 socket 既不报错也不
 * 关闭，于是 App 一直显示着连接、其实一个字都收不到。20 秒一发、10 秒没 pong 就自己判死。
 */
const PONG_TIMEOUT_MS = 10_000;

/**
 * 一条 WebSocket：订阅一个会话、收增量、断了自己接回去。
 *
 * 一次只订阅一个会话（协议里那条），所以换会话就是重新发一次 open。
 * 令牌放在查询串里 —— React Native 的 WebSocket 设不了握手头。
 */
export class MobileSocket {
  private ws: WebSocket | null = null;
  private sessionId: string | null = null;
  private heartbeat: ReturnType<typeof setInterval> | null = null;
  private pongTimer: ReturnType<typeof setTimeout> | null = null;
  private retry = 0;
  private reconnects = 0;
  private closed = false;
  private timer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private conn: Pairing,
    private opts: WsOptions,
  ) {}

  private url(): string {
    const base = this.conn.baseUrl.replace(/^http/, 'ws').replace(/\/+$/, '');
    return `${base}/v1/ws?token=${encodeURIComponent(this.conn.token)}`;
  }

  /** 把连接挂上（配对之后就该挂着：列表页也要显示连接状态，也收得到广播） */
  ensureConnected(): void {
    if (this.ws || this.closed) return;
    this.connect();
  }

  /**
   * 从后台回来时用：**重建**连接，而不是相信现有那条。
   *
   * 锁屏期间系统会挂起定时器，TCP 也可能是半开的 —— 这时 readyState 还报 OPEN，
   * `ensureConnected` 见有对象就直接返回，于是界面永远停在「连不上」，只有杀掉重进才恢复。
   * 重连一次很便宜，判断「这条还活着吗」不可靠。
   */
  forceReconnect(): void {
    if (this.closed) return;
    this.clearHeartbeat();
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    const old = this.ws;
    this.ws = null;
    if (old) {
      // 旧连接的回调会晚一步到达：onclose 里靠 ws 身份把它的重连逻辑挡掉
      try {
        old.close();
      } catch {
        // 已经关了
      }
    }
    this.retry = 0;
    this.reconnects += 1;
    this.connect();
  }

  /**
   * 订阅一个会话；已经连着的话就换订阅。传 null = 退出这段对话（回列表）：告诉电脑
   * 别再把它的流推过来 —— 一段在跑的对话每秒几十条事件，全落在一个没人看的画面上。
   */
  subscribe(sessionId: string | null): void {
    this.sessionId = sessionId;
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.send({ type: 'open', sessionId: sessionId ?? '' });
      return;
    }
    if (!sessionId) return;
    this.connect();
  }

  private connect(): void {
    if (this.closed) return;
    this.opts.onStatus?.('connecting');
    const ws = new WebSocket(this.url());
    this.ws = ws;

    // 握手超时：把它当一次「掉线」，上层才有机会换一条地址
    const handshake = setTimeout(() => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.clearPong();
      try {
        ws.close();
      } catch {
        // 本来就没连上
      }
      this.opts.onStatus?.('closed');
      if (this.closed) return;
      this.opts.onDisconnected?.();
      this.scheduleReconnect();
    }, CONNECT_TIMEOUT_MS);

    ws.onopen = () => {
      clearTimeout(handshake);
      if (this.ws !== ws) return;
      this.retry = 0;
      this.opts.onStatus?.('open');
      if (this.sessionId) this.send({ type: 'open', sessionId: this.sessionId });
      if (this.reconnects > 0) this.opts.onReconnected?.();
      this.startHeartbeat(ws);
    };

    ws.onmessage = (e: WebSocketMessageEvent) => {
      if (this.ws !== ws) return;
      try {
        const ev = JSON.parse(String(e.data)) as ServerEvent;
        if ((ev as { type: string }).type === 'pong') {
          // 还活着：把看门狗的倒计时撤掉，等下一条 ping 再起
          this.clearPong();
          return;
        }
        this.opts.onEvent(ev);
      } catch {
        // 解析不了的单条事件丢掉就行，不用把整条连接带走
      }
    };

    ws.onerror = () => {
      // 交给 onclose 去重连
    };

    ws.onclose = () => {
      clearTimeout(handshake);
      // 被 forceReconnect 换掉的旧连接：它的 onclose 不算「连接断了」
      if (this.ws !== ws) return;
      this.clearHeartbeat();
      this.ws = null;
      this.opts.onStatus?.('closed');
      if (this.closed) return;
      this.opts.onDisconnected?.();
      this.scheduleReconnect();
    };
  }

  private send(v: unknown): void {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(v));
    }
  }

  /**
   * 心跳：每 20 秒发一个 ping，并且要求 10 秒内有 pong。没有回音就自己把这条判死 ——
   * 那个「既不报错也不关闭」的半开状态，只能靠回音发现（`readyState` 一直报 OPEN）。
   */
  private startHeartbeat(ws: WebSocket): void {
    this.clearHeartbeat();
    this.heartbeat = setInterval(() => {
      if (this.ws !== ws) return;
      this.send({ type: 'ping' });
      this.clearPong();
      this.pongTimer = setTimeout(() => {
        if (this.ws !== ws) return;
        // 先把自己手里的这条清掉，它的 onclose 就不会再走一遍重连（身份对不上）
        this.ws = null;
        this.clearHeartbeat();
        try {
          ws.close();
        } catch {
          // 已经是死的
        }
        this.opts.onStatus?.('closed');
        if (this.closed) return;
        this.opts.onDisconnected?.();
        this.scheduleReconnect();
      }, PONG_TIMEOUT_MS);
    }, HEARTBEAT_MS);
  }

  /** 掉线之后的重连：1s、2s、4s…最多 10s。 */
  private scheduleReconnect(): void {
    this.reconnects += 1;
    const wait = Math.min(1000 * 2 ** this.retry, 10_000);
    this.retry += 1;
    if (this.timer) clearTimeout(this.timer);
    this.timer = setTimeout(() => this.connect(), wait);
  }

  private clearHeartbeat(): void {
    if (this.heartbeat) {
      clearInterval(this.heartbeat);
      this.heartbeat = null;
    }
    this.clearPong();
  }

  private clearPong(): void {
    if (this.pongTimer) {
      clearTimeout(this.pongTimer);
      this.pongTimer = null;
    }
  }

  close(): void {
    this.closed = true;
    this.clearHeartbeat();
    if (this.timer) clearTimeout(this.timer);
    this.ws?.close();
    this.ws = null;
  }
}
