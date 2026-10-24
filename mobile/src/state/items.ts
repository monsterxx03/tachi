import type { PendingAsk, SessionMessage, Snapshot } from '../api/types';
import type { Live, ToolRun } from './chat';

/**
 * 消息列表要画的东西，从快照算出来。**纯函数**：好单测，也不该拖着 React。
 *
 * 折叠的判据是**一轮**（两条用户消息之间），不是「相邻」：真实记录里两个 tool call 之间
 * 一定夹着 tool_result，还常常夹着 thinking 和**内容为空的 assistant**（模型一次调用的
 * 记录）。按相邻判断的话，这些空记录把每一段都切成一个工具，永远折不起来 —— 手机上看到
 * 的就是「每个 tool call 都露在外面」。
 *
 * 折进去的是这一轮的**过程**，和桌面的 turnView 一样：思考、成功的工具调用、以及模型在
 * 中途写的话（一次调用之间的小结），按原来的先后排在一起，点开照那个顺序展开。
 *
 * **只有这一轮最后写的那段话留在外面**（桌面的 conclusion）：它就是「这一轮的答案」。留着
 * 每一段正文的话，一个跑了二十步的回合会在过程行下面堆出十几段话 —— 那些是过程，不是答案。
 * 失败的那一步也留在外面 —— 出错要看得见。
 */
export type ToolPair = { key: string; msg: SessionMessage; result?: SessionMessage };

/** 过程里的一步，保持记录里的先后：思考、工具调用、中间写的话，按发生的顺序排 */
export type ProcessStep =
  | { kind: 'thinking'; key: string; text: string }
  /** 中间写的话：本轮最后一段之外的所有正文都折起来 */
  | { kind: 'note'; key: string; msg: SessionMessage }
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
  /** 这一轮结论之外还没定稿的东西：Live 只画**正在写的最后一段**，其余已经折进过程行了 */
  | { kind: 'live'; key: string; text: string };

/** 系统提醒不进消息流（见 reminder.ts） */
function hidden(m: SessionMessage): boolean {
  return m.role === 'reminder';
}

/**
 * 一步的过程折成什么：只有一步、而且它不是「中间写的话」时才直接露出来（一张工具卡 /
 * 一条思考）。
 *
 * 一条 note 必须是折起来的：它能走到这里，就说明这一轮还有别的正文段（最后那段已经当结论
 * 留在外面了）—— 单独把它露在外面，就是另外那几段被折、它自己没有道理地露着。
 *
 * 实时那一轮走的是同一个函数 —— 「跑的时候逐步看，停下来才折」会让画面在轮次结束时
 * 整段跳一下，而且跑的过程中读者根本不需要每一步各占一行。
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
    if (step.kind === 'thinking') {
      return {
        kind: 'msg',
        key: step.key,
        msg: { index: -1, role: 'assistant', content: '', thinking: step.text },
      };
    }
  }
  return { kind: 'process', key, steps: [...steps], running };
}

/** 工具调用那一段的 msg / result（实时那一轮没有记录，只能现场拼） */
function liveToolStep(tool: ToolRun): Extract<ProcessStep, { kind: 'tool' }> {
  const key = `live-${tool.id}`;
  return {
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
  };
}

/**
 * 实时那一轮拆成「折起来的」和「露在外面的」两半 —— 判据和历史那一半完全一样：
 * 最后写的一段话是这一轮的答案，之前的一切都是过程。交付物（成功的 SendFile）钉在尾部。
 */
function liveTurn(live: Live): { steps: ProcessStep[]; conclusion: string; files: Item[] } {
  const steps: ProcessStep[] = [];
  const files: Item[] = [];
  let conclusion = '';
  let conclusionAt = -1;

  live.parts.forEach((p, i) => {
    if (p.type === 'tool') {
      const step = liveToolStep(p.tool);
      // 成功的 SendFile 就是那个文件本身，不是过程 —— 和历史里一个规矩
      if (p.tool.name === 'SendFile' && p.tool.result !== undefined && !p.tool.isError) {
        files.push({ kind: 'msg', key: step.key, msg: step.pair.msg, result: step.pair.result });
        return;
      }
      steps.push(step);
      return;
    }
    if (p.type === 'thinking') {
      steps.push({ kind: 'thinking', key: `live-t${i}`, text: p.text });
      return;
    }
    steps.push({
      kind: 'note',
      key: `live-n${i}`,
      msg: { index: -1, role: 'assistant', content: p.text },
    });
    conclusionAt = steps.length - 1;
    conclusion = p.text;
  });

  // 最后一段话是这一轮的结论：从过程里拿出来（沿用的还是那条 note，index 之类的信息都在）
  if (conclusionAt >= 0) steps.splice(conclusionAt, 1);
  return { steps, conclusion, files };
}

export function buildItems(snapshot: Snapshot): Item[] {
  const results = new Map<string, SessionMessage>();
  for (const m of snapshot.messages) {
    if (m.role === 'tool_result' && m.toolCallId) results.set(m.toolCallId, m);
  }

  const items: Item[] = [];
  // 这一轮露在外面的东西，按顺序：用户消息、失败的工具、提示行……过程行插在 anchor 处
  let turn: Item[] = [];
  // 过程行插在这一轮的第几个位置 —— 用户消息之后（它是这一轮的开头），不是整条列表的最前
  let anchor = 0;
  // 这一轮折起来的过程：思考、成功的工具、中间写的话，按记录里的先后
  let steps: ProcessStep[] = [];
  // 这一轮的**交付物**（SendFile 递过来的文件），按发出的先后，钉在本轮结尾。
  // 它们不是过程：折进「N 步」里等于把「把 X 发给我」的答案藏在一个要展开的行后面
  // （桌面的 turnView 也是这么分的，见它的 attachments 那一栏）。
  let files: Item[] = [];

  const flushTurn = () => {
    // 这一轮**最后写的那段话**是答案，留在外面（桌面的 conclusion）。其余正文都是过程。
    // 走完才知道哪段是最后一段，所以正文先都当过程收着，收尾时再取出来。
    let conclusion: Item | null = null;
    for (let i = steps.length - 1; i >= 0; i--) {
      const step = steps[i];
      if (step.kind !== 'note') continue;
      steps.splice(i, 1);
      conclusion = { kind: 'msg', key: step.key, msg: step.msg };
      break;
    }
    const folded = foldItem(steps, `proc-${steps[0]?.key ?? ''}`);
    // 过程行永远在这一轮的开头：先看到「这一轮做了什么」，再看到答案（桌面同序）
    if (folded) turn.splice(anchor, 0, folded);
    if (conclusion) turn.push(conclusion);
    items.push(...turn, ...files);
    turn = [];
    anchor = 0;
    steps = [];
    files = [];
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

    // 思考进过程，和工具、正文按记录里的先后排在一起
    if (m.role === 'assistant' && m.thinking) {
      steps.push({ kind: 'thinking', key: `t${key}`, text: m.thinking });
    }
    // 正文先当过程收着：收尾时才把这一轮最后一段拿出来当结论（见 flushTurn）
    if (m.role === 'assistant' && (m.content ?? '').trim()) {
      steps.push({ kind: 'note', key, msg: m.thinking ? { ...m, thinking: undefined } : m });
      return;
    }
    // 交付物不是过程：一个成功的 SendFile 就是这个文件本身，露在外面（见 files 的注释）
    if (m.role === 'tool_call' && m.toolName === 'SendFile' && !result?.isError) {
      files.push({ kind: 'msg', key, msg: m, result });
      return;
    }
    if (m.role === 'tool_call' && !result?.isError) {
      steps.push({ kind: 'tool', key, pair: { key, msg: m, result } });
      return;
    }
    if (hidden(m)) return;
    // 思考已经收进过程里了：只剩思考、没有正文的 assistant 不该再占一行
    if (m.role === 'assistant') return;
    turn.push({ kind: 'msg', key, msg: m, result });
    // 过程行排在这一轮的提问之后
    if (m.role === 'user') anchor = turn.length;
  });
  flushTurn();

  // 实时那一轮**不在这里**：它每个字都在变，混进来就会让整份列表每次流式都重建，
  // 而列表单元是按引用判断「要不要重画」的，重建一次就等于整列重画一遍。它由
  // buildLiveTail 单独给，待答项由 buildAskItems 单独给，两者都挂在列表头部
  // （见 Chat.tsx）—— 历史这一半因此只在回合结束、翻页时变。
  return items;
}

/**
 * 正在跑的那一轮要画的东西：过程（思考 + 工具 + 中间写的话）折成一行，最后写的那段话
 * 单独一条，交付的文件钉在尾部。
 *
 * **跑着的时候就开始折**，那一行上的步数随着工具一个个结束往上走。露在外面的是正在写的
 * 字 —— 那才是读者要看的；跑过的话进了过程行。与 buildItems 分开是因为它跟着每个字变，
 * 而历史不跟着变。
 */
export function buildLiveTail(live: Live): Item[] {
  const { steps, conclusion, files } = liveTurn(live);
  const items: Item[] = [];
  const folded = foldItem(steps, 'live-process', live.active);
  if (folded) items.push(folded);
  if (conclusion || live.active) items.push({ kind: 'live', key: 'live', text: conclusion });
  items.push(...files);
  return items;
}

/**
 * 待答项：读者现在唯一该做的事，画在**视觉最底部**——实时那一轮在它上面，
 * 正在跑的内容永远不能把卡片挤到看不见的地方。
 */
export function buildAskItems(snapshot: Snapshot): Item[] {
  return snapshot.asks.map((ask, i) => ({ kind: 'msg', key: `ask${i}`, msg: snapshot.messages[0], ask }));
}
