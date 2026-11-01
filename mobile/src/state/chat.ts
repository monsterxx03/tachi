import { Platform } from 'react-native';
import { ApiError } from '../api/client';
import type { ServerEvent, SessionMessage, Snapshot } from '../api/types';

/** 正在跑的这一轮：文字在流、工具在转。轮次一结束就清掉，改用记录（记录是权威的）。 */
export interface ToolRun {
  id: string;
  name: string;
  title: string;
  /** 工具参数（JSON 字符串），tool_args 事件补上 */
  args?: string;
  result?: string;
  isError?: boolean;
  /** 用户自己点名的调用（桌面的 /sh）：卡片默认摊开，见 mobile-protocol.md 的 tool_start */
  autoExpand?: boolean;
}

/**
 * 实时那一轮的一段。**顺序就是发生的顺序**：思考和正文一段段接在尾部（同类合并），工具
 * 调用当场插进去（见 appendDelta）。
 *
 * 为什么不是「一个字符串 + 一组工具」：模型在一轮里会写一段、调工具、再写一段，全部拼进
 * 一个字符串就丢了这个先后，画面上只剩「所有文字挤在过程行下面」（桌面端的 parts 数组
 * 是同一个模型，见 desktop 的 appendPartDelta）。
 */
export type LivePart =
  | { type: 'thinking'; text: string }
  | { type: 'text'; text: string }
  | { type: 'tool'; tool: ToolRun };

export interface Live {
  active: boolean;
  parts: LivePart[];
  /** 这一轮的实时速度（协议里的 tps 事件）；还没收到过就是 undefined */
  tps?: number;
  /** 工具边界 / 轮次结束后冻结下来的上一次速度 —— 停下来时显示它 */
  lastTps?: number;
}

export const emptyLive: Live = { active: false, parts: [] };

/** 同一类的一段接着上一段的尾部，否则另起一段：中间夹了思考或工具就是新的一段 */
function appendDelta(parts: LivePart[], type: 'thinking' | 'text', delta: string): LivePart[] {
  if (!delta) return parts;
  const last = parts[parts.length - 1];
  if (last && last.type === type) return [...parts.slice(0, -1), { type, text: last.text + delta }];
  return [...parts, { type, text: delta }];
}

/** 工具事件按 call id 找到它自己那一段；找不到就什么都不动（别为一个不认识的事件新起一段） */
function patchTool(parts: LivePart[], id: string, patch: Partial<ToolRun>): LivePart[] {
  return parts.map((p) => (p.type === 'tool' && p.tool.id === id ? { ...p, tool: { ...p.tool, ...patch } } : p));
}

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
      // 快照是整个会话的权威状态，实时那一轮要清掉 —— 但最后一次实测速度留着：
      // 它就是这段对话最近的速度，清掉的话每轮结束都退回「—」。
      return same({ snapshot: mergeSnapshot(st.snapshot, ev.snapshot), live: { ...emptyLive, lastTps: st.live.lastTps } });

    case 'text':
      return same({ ...st, live: { ...st.live, active: true, parts: appendDelta(st.live.parts, 'text', ev.text) } });

    case 'thinking':
      return same({
        ...st,
        live: { ...st.live, active: true, parts: appendDelta(st.live.parts, 'thinking', ev.text) },
      });

    case 'tool_start':
      return same({
        ...st,
        live: {
          ...st.live,
          active: true,
          parts: [
            ...st.live.parts,
            {
              type: 'tool',
              tool: { id: ev.toolId, name: ev.toolName, title: ev.title, autoExpand: ev.autoExpand },
            },
          ],
        },
      });

    case 'tps':
      return same({ ...st, live: { ...st.live, tps: ev.tps, lastTps: ev.lastTps || st.live.lastTps } });

    case 'tool_args':
      return same({
        ...st,
        live: { ...st.live, parts: patchTool(st.live.parts, ev.toolId, { args: ev.args }) },
      });

    case 'tool_result':
      return same({
        ...st,
        live: { ...st.live, parts: patchTool(st.live.parts, ev.toolId, { result: ev.result, isError: ev.isError }) },
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

    case 'session_prefs': {
      // 这几个开关是共享状态：谁改的都落到这里，这块屏立刻显示同一组值。
      if (!st.snapshot) return same(st);
      return same({
        ...st,
        snapshot: {
          ...st.snapshot,
          model: ev.provider,
          skipApproval: ev.skipApproval,
          notifyOff: ev.notifyOff,
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

/**
 * 新快照落到已有画面上：**已经翻出来的更早消息留着**。
 *
 * 快照只带最近一百条（协议里那条：记录是权威的，但快照只够画一屏）。用户上滑翻过更早的
 * 历史之后，一次轮次结束、一次重连都会重读快照 —— 直接替换等于把翻出来的东西又收回去，
 * 滚动位置也会跳。所以这里把比新快照更早的那些合并回去，按记录序号去重。
 *
 * hasMore 跟着**合并后**最旧的那条走：新快照的 hasMore 说的是「比我这一屏更早的还有」，
 * 而我们手里可能已经有更早的了，那时该用旧的答案。
 */
export function mergeSnapshot(prev: Snapshot | null, snap: Snapshot): Snapshot {
  if (!prev || prev.session.id !== snap.session.id) return snap;
  const oldest = snap.messages.length > 0 ? snap.messages[0].index : Number.MAX_SAFE_INTEGER;
  const older = prev.messages.filter((m) => m.index >= 0 && m.index < oldest);
  if (older.length === 0) return snap;
  const seen = new Set(older.map((m) => m.index));
  const merged = [...older, ...snap.messages.filter((m) => !seen.has(m.index))];
  return { ...snap, messages: merged, hasMore: prev.hasMore };
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
      case 'unknown_command':
        return '没有这个命令';
      case 'command_desktop_only':
        // 一次性命令（/review、/commit…）的输出在电脑那块侧面板里，手机上无处可看
        return '这类命令要在电脑上运行';
      case 'command_refused':
        return '这条命令没能执行，在电脑上试试';
      case 'no_project':
        return '这个项目已经不在了（或它的目录不可用）';
      case 'bad_working_dir':
        return '这个目录用不了，换一个路径试试';
      case 'create_failed':
        return '没能建出这段对话，在电脑上看看日志';
      case 'pin_failed':
        return '置顶没保存成功，再试一次';
      case 'no_job':
        return '这个定时任务已经不在了';
      case 'cron_disabled':
        return '定时任务在电脑的设置里被关掉了';
      case 'not_desktop':
        return '这个任务不归这台电脑管';
      case 'cron_failed':
        return '操作没成功，在电脑上看看日志';
      case 'awake_failed':
        // 电脑的系统（或它的桌面环境）不肯接这条「别睡」的禁令 —— 不是手机的错。
        return '这台电脑没能保持唤醒';
    }
    return `出错了（${e.code}）`;
  }
  // iOS 上还有第三种原因，而且它看起来和另外两种一模一样：本地网络权限被拒了。
  // 那一句话要能把人送到正确的位置（设置 → 隐私与安全性 → 本地网络）。
  return Platform.OS === 'ios'
    ? '连不上电脑（没开机、不在同一网络，或没给本地网络权限）'
    : '连不上电脑（没开机、或不在同一个网络）';
}
