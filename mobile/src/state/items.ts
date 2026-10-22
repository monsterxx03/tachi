import type { PendingAsk, SessionMessage, Snapshot } from '../api/types';
import type { Live } from './chat';

/**
 * 消息列表要画的东西，从快照算出来。**纯函数**：好单测，也不该拖着 React。
 *
 * 折叠的判据是**一轮**（两条用户消息之间），不是「相邻」：真实记录里两个 tool call 之间
 * 一定夹着 tool_result，还常常夹着 thinking 和**内容为空的 assistant**（模型一次调用的
 * 记录）。按相邻判断的话，这些空记录把每一段都切成一个工具，永远折不起来 —— 手机上看到
 * 的就是「每个 tool call 都露在外面」。
 *
 * 折进去的是这一轮的**过程**，和桌面的「N 步」一样：思考、成功的工具调用按原来的先后
 * 排在一起，点开照那个顺序展开。正文（模型写给人看的话）留在外面，失败的那一步也留在
 * 外面 —— 出错要看得见。
 */
export type ToolPair = { key: string; msg: SessionMessage; result?: SessionMessage };

/** 过程里的一步，保持记录里的先后：思考在前，工具调用跟着来 */
export type ProcessStep =
  | { kind: 'thinking'; key: string; text: string }
  | { kind: 'tool'; key: string; pair: ToolPair };

export type Item =
  | {
      kind: 'msg';
      key: string;
      msg: SessionMessage;
      ask?: PendingAsk;
      result?: SessionMessage;
      /** 这一张工具卡还在跑（只有实时那一轮会有） */
      running?: boolean;
    }
  /** running：这是**正在跑**的那一轮的过程，折起来的那一行要标「在跑」 */
  | { kind: 'process'; key: string; steps: ProcessStep[]; running?: boolean }
  | { kind: 'live'; key: string };

/** 系统提醒不进消息流（见 reminder.ts） */
function hidden(m: SessionMessage): boolean {
  return m.role === 'reminder';
}

/**
 * 一步的过程折成什么：只有一步就直接露出来（一张工具卡 / 一条思考），多步才收成一行。
 *
 * 实时那一轮走的是同一个函数 —— 「跑的时候逐步看，停下来才折」会让画面在轮次结束时
 * 整段跳一下，而且跑的过程中读者根本不关心每个工具各占一行。
 */
function foldItem(
  steps: ProcessStep[],
  key: string,
  running?: boolean,
): Item | null {
  if (steps.length === 0) return null;
  if (steps.length === 1) {
    const step = steps[0];
    if (step.kind === 'tool') {
      return {
        kind: 'msg',
        key: step.key,
        msg: step.pair.msg,
        result: step.pair.result,
        running: running && step.pair.result === undefined,
      };
    }
    return {
      kind: 'msg',
      key: step.key,
      msg: { index: -1, role: 'assistant', content: '', thinking: step.text },
    };
  }
  return { kind: 'process', key, steps: [...steps], running };
}

/** 正在跑的那一轮，过程还没进记录：从 live 的增量拼出同一套步骤 */
function liveProcess(live: Live): ProcessStep[] {
  const steps: ProcessStep[] = [];
  if (live.thinking) steps.push({ kind: 'thinking', key: 'live-think', text: live.thinking });
  live.tools.forEach((tool, i) => {
    const key = `live-${i}-${tool.id}`;
    steps.push({
      kind: 'tool',
      key,
      pair: {
        key,
        msg: {
          index: -1,
          role: 'tool_call',
          content: '',
          toolName: tool.name,
          toolCallId: tool.id,
          title: tool.title,
          args: tool.args,
        },
        result:
          tool.result === undefined
            ? undefined
            : {
                index: -1,
                role: 'tool_result',
                content: '',
                toolCallId: tool.id,
                toolResult: tool.result,
                isError: tool.isError,
              },
      },
    });
  });
  return steps;
}

export function buildItems(snapshot: Snapshot): Item[] {
  const results = new Map<string, SessionMessage>();
  for (const m of snapshot.messages) {
    if (m.role === 'tool_result' && m.toolCallId) results.set(m.toolCallId, m);
  }

  const items: Item[] = [];
  // 当前这一轮的过程：foldAt 是它在这一轮里的位置，steps 是这一轮的思考与工具，按先后
  let turn: Item[] = [];
  let foldAt = -1;
  let steps: ProcessStep[] = [];

  const flushTurn = () => {
    const folded = foldItem(steps, `proc${foldAt}-${steps[0]?.key ?? ''}`);
    if (folded) turn.splice(foldAt, 0, folded);
    items.push(...turn);
    turn = [];
    foldAt = -1;
    steps = [];
  };

  snapshot.messages.forEach((m, i) => {
    if (m.role === 'tool_result') return; // 跟着它的 tool_call 一起画
    // key 用**记录序号**，不是数组位置：往上翻页时前面插进一整页，按位置生成的话每一条
    // 的 key 都会变，列表就认不出「读者正看着哪条」了（滚动位置会跳）。
    // 本地先画上的那条（index < 0）还没有记录位置，用它在数组里的位置兜底。
    const key = m.index >= 0 ? `m${m.index}` : `local${i}`;
    const result = m.toolCallId ? results.get(m.toolCallId) : undefined;

    // 一条用户消息开始新一轮：先把上一轮的收尾
    if (m.role === 'user') flushTurn();

    // 思考进过程，和工具按记录里的先后排在一起；正文照旧单独渲染
    if (m.role === 'assistant' && m.thinking) {
      if (foldAt < 0) foldAt = turn.length;
      steps.push({ kind: 'thinking', key: `t${key}`, text: m.thinking });
    }
    if (m.role === 'tool_call' && !result?.isError) {
      if (foldAt < 0) foldAt = turn.length;
      steps.push({ kind: 'tool', key, pair: { key, msg: m, result } });
      return;
    }
    if (hidden(m)) return;
    // 思考已经收进过程里了：只剩思考、没有正文的 assistant 不该再占一行
    if (m.role === 'assistant' && !(m.content ?? '').trim()) return;
    turn.push({ kind: 'msg', key, msg: m.thinking ? { ...m, thinking: undefined } : m, result });
  });
  flushTurn();

  // 实时那一轮**不在这里**：它每个字都在变，混进来就会让整份列表每次流式都重建，
  // 而列表单元是按引用判断「要不要重画」的，重建一次就等于整列重画一遍。它由
  // buildLiveTail 单独给，待答项由 buildAskItems 单独给，两者都挂在列表头部
  // （见 Chat.tsx）—— 历史这一半因此只在回合结束、翻页时变。
  return items;
}

/**
 * 正在跑的那一轮要画的东西：过程（思考 + 工具）折成一行，正文单独一条。
 *
 * **跑着的时候就开始折**，那一行上的步数随着工具一个个结束往上走。正文留在外面 ——
 * 正在写的字才是读者要看的。与 buildItems 分开是因为它跟着每个字变，而历史不跟着变。
 */
export function buildLiveTail(live: Live): Item[] {
  const items: Item[] = [];
  const folded = foldItem(liveProcess(live), 'live-process', live.active);
  if (folded) items.push(folded);
  if (live.text || live.active) items.push({ kind: 'live', key: 'live' });
  return items;
}

/**
 * 待答项：读者现在唯一该做的事，画在**视觉最底部**——实时那一轮在它上面，
 * 正在跑的内容永远不能把卡片挤到看不见的地方。
 */
export function buildAskItems(snapshot: Snapshot): Item[] {
  return snapshot.asks.map((ask, i) => ({ kind: 'msg', key: `ask${i}`, msg: snapshot.messages[0], ask }));
}
