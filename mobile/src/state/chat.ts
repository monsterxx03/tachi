import { ApiError } from '../api/client';
import type { ServerEvent, SessionMessage, Snapshot } from '../api/types';

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
export function applyEvent(
  st: ChatState,
  ev: ServerEvent,
  selfOrigin: string,
): { state: ChatState; needsReload: boolean } {
  const same = (s: ChatState) => ({ state: s, needsReload: false });

  switch (ev.type) {
    case 'snapshot':
      return same({ snapshot: ev.snapshot, live: emptyLive });

    case 'text':
      return same({ ...st, live: { ...st.live, active: true, text: st.live.text + ev.text } });

    case 'thinking':
      return same({ ...st, live: { ...st.live, active: true, thinking: st.live.thinking + ev.text } });

    case 'tool_start':
      return same({
        ...st,
        live: {
          ...st.live,
          active: true,
          tools: [...st.live.tools, { id: ev.toolId, name: ev.toolName, title: ev.title }],
        },
      });

    case 'tool_result':
      return same({
        ...st,
        live: {
          ...st.live,
          tools: st.live.tools.map((t) =>
            t.id === ev.toolId ? { ...t, result: ev.result, isError: ev.isError } : t,
          ),
        },
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

    // 这几样都要重读一次快照：记录是权威的，实时流只负责延长尾巴
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
      case 'agent_unavailable':
        return '这台电脑还没配好模型，先在电脑上配置';
    }
    return `出错了（${e.code}）`;
  }
  return '连不上电脑（没开机、或不在同一个网络）';
}
