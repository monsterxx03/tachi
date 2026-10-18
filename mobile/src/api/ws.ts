import type { Pairing } from './client';
import type { ServerEvent } from './types';

export type WsStatus = 'connecting' | 'open' | 'closed';

export interface WsOptions {
  onEvent: (ev: ServerEvent) => void;
  onStatus?: (s: WsStatus) => void;
  /** 重连成功时触发：上层要重读一次快照（记录是权威的，流只延长尾巴） */
  onReconnected?: () => void;
}

const HEARTBEAT_MS = 20_000;

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

  /** 订阅一个会话；已经连着的话就换订阅 */
  subscribe(sessionId: string): void {
    this.sessionId = sessionId;
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.send({ type: 'open', sessionId });
      return;
    }
    this.connect();
  }

  private connect(): void {
    if (this.closed) return;
    this.opts.onStatus?.('connecting');
    const ws = new WebSocket(this.url());
    this.ws = ws;

    ws.onopen = () => {
      this.retry = 0;
      this.opts.onStatus?.('open');
      if (this.sessionId) this.send({ type: 'open', sessionId: this.sessionId });
      if (this.reconnects > 0) this.opts.onReconnected?.();
      this.heartbeat = setInterval(() => this.send({ type: 'ping' }), HEARTBEAT_MS);
    };

    ws.onmessage = (e: WebSocketMessageEvent) => {
      try {
        const ev = JSON.parse(String(e.data)) as ServerEvent;
        if ((ev as { type: string }).type === 'pong') return;
        this.opts.onEvent(ev);
      } catch {
        // 解析不了的单条事件丢掉就行，不用把整条连接带走
      }
    };

    ws.onerror = () => {
      // 交给 onclose 去重连
    };

    ws.onclose = () => {
      this.clearHeartbeat();
      this.ws = null;
      this.opts.onStatus?.('closed');
      if (this.closed) return;
      // 退避重连：1s、2s、4s…最多 10s
      this.reconnects += 1;
      const wait = Math.min(1000 * 2 ** this.retry, 10_000);
      this.retry += 1;
      this.timer = setTimeout(() => this.connect(), wait);
    };
  }

  private send(v: unknown): void {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(v));
    }
  }

  private clearHeartbeat(): void {
    if (this.heartbeat) {
      clearInterval(this.heartbeat);
      this.heartbeat = null;
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
