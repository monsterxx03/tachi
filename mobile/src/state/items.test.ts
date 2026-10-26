import type { SessionMessage, Snapshot } from '../api/types';
import { emptyLive, type Live } from './chat';
import { buildAskItems, buildItems, buildLiveTail, type Item } from './items';

/** 一段最小快照；消息按参数给 */
function snap(messages: SessionMessage[]): Snapshot {
  return {
    session: {
      id: 's1',
      title: 't',
      updatedAt: '刚刚',
      running: false,
      waiting: '',
      readOnly: false,
      heldByTask: false,
    },
    messages,
    hasMore: false,
    state: { status: 'idle', label: '空闲', detail: '', waiting: '' },
    asks: [],
    model: 'p',
    skipApproval: false,
    workspace: { primary: '/tmp', additional: [] },
    usage: { cost: 0 },
    context: { estimate: 0, window: 0, parts: [] },
    attachments: [],
  };
}

let next = 0;
function user(text: string): SessionMessage {
  return { index: next++, role: 'user', content: text };
}
function assistant(text: string, thinking?: string): SessionMessage {
  return { index: next++, role: 'assistant', content: text, thinking };
}
function call(id: string, name = 'Bash'): SessionMessage {
  return { index: next++, role: 'tool_call', content: '', toolName: name, toolCallId: id, title: id };
}
function resultOf(id: string, isError = false): SessionMessage {
  return { index: next++, role: 'tool_result', content: '', toolCallId: id, toolResult: 'ok', isError };
}

beforeEach(() => {
  next = 0;
});

/**
 * 一屏的全部项，按时间顺序（旧 → 新）：历史、实时那一轮、待答项。
 *
 * 三块在渲染时是分开算的（历史、实时、待答各一个纯函数，见 items.ts）—— 历史不跟着
 * 每个字重建，是这次拆开的全部原因。这里拼回一条线，让断言的语义和拆分前一致。
 */
function screen(s: Snapshot, live: Live): Item[] {
  return [...buildItems(s), ...buildLiveTail(live), ...buildAskItems(s)];
}

describe('buildItems —— 一轮的过程折成一行', () => {
  it('一轮里成功的工具收成一行，失败的那一步留在原地', () => {
    const items = buildItems(
      snap([
        user('看一下接口'),
        assistant('我先扫一遍'),
        call('a'),
        resultOf('a'),
        call('b'),
        resultOf('b'),
        call('bad'),
        resultOf('bad', true),
        call('c'),
        resultOf('c'),
        assistant('13 个端点都过了 guard'),
      ])
    );

    const kinds = items.map((i) => (i.kind === 'msg' ? i.msg.role : i.kind));
    // 用户 / 折叠（中间那句「我先扫一遍」也在里面）/ 失败的卡片 / 结尾 —— 不再是一个工具一行
    expect(kinds).toEqual(['user', 'process', 'tool_call', 'assistant']);

    const fold = items.find((i) => i.kind === 'process');
    if (fold?.kind !== 'process') throw new Error('没有折叠项');
    expect(fold.steps.map((s) => (s.kind === 'tool' ? s.pair.msg.toolCallId : s.kind))).toEqual([
      'note',
      'a',
      'b',
      'c',
    ]);
    // 露在外面的只有最后那段（这一轮的答案）
    const prose = items.filter((i) => i.kind === 'msg' && i.msg.role === 'assistant');
    expect(prose.map((i) => (i.kind === 'msg' ? i.msg.content : ''))).toEqual(['13 个端点都过了 guard']);
  });

  it('中间夹着 thinking 和内容为空的 assistant，仍然折在一起（真实记录就是这个形状）', () => {
    const items = buildItems(
      snap([
        user('改一下'),
        call('a'),
        resultOf('a'),
        // 投影把 thinking 记录并进后面那条 assistant：内容是空的，只有思考
        assistant('', 'Let me read the relevant sections'),
        call('b'),
        resultOf('b'),
        // 纯粹的空调子（既没有正文也没有思考）：不该占一行
        assistant(''),
        call('c'),
        resultOf('c'),
        assistant('改完了'),
      ])
    );

    const folds = items.filter((i) => i.kind === 'process');
    expect(folds).toHaveLength(1);
    if (folds[0].kind !== 'process') throw new Error('类型不对');
    // 按记录里的先后：工具 a → 思考 → 工具 b → 工具 c（空 assistant 不占位）
    expect(folds[0].steps.map((s) => s.kind)).toEqual(['tool', 'thinking', 'tool', 'tool']);

    // 只剩思考、没有正文的 assistant 不再单独占一行（它已经在过程里了）
    const empties = items.filter(
      (i) => i.kind === 'msg' && i.msg.role === 'assistant' && !i.msg.content,
    );
    expect(empties).toHaveLength(0);
  });

  it('两条用户消息之间各折各的，不跨轮', () => {
    const items = buildItems(
      snap([
        user('第一轮'),
        call('a'),
        resultOf('a'),
        call('b'),
        resultOf('b'),
        assistant('第一轮完了'),
        user('第二轮'),
        call('c'),
        resultOf('c'),
        call('d'),
        resultOf('d'),
        assistant('第二轮完了'),
      ])
    );

    const folds = items.filter((i) => i.kind === 'process');
    expect(folds).toHaveLength(2);
    const ids = (f: (typeof folds)[number]) =>
      f.kind === 'process' ? f.steps.map((s) => (s.kind === 'tool' ? s.pair.msg.toolCallId : 'th')) : [];
    expect(ids(folds[0])).toEqual(['a', 'b']);
    expect(ids(folds[1])).toEqual(['c', 'd']);
  });

  it('只有思考、没有工具的一轮：折成「思考 N 段」那一条，展开是原文', () => {
    const items = buildItems(
      snap([
        user('想想'),
        assistant('', '第一段思考'),
        assistant('', '第二段思考'),
        assistant('结论是这样'),
      ])
    );
    // 思考折成一条 + 正文
    expect(items.map((i) => i.kind)).toEqual(['msg', 'process', 'msg']);
    const fold = items[1];
    if (fold.kind !== 'process') throw new Error('没有过程项');
    expect(fold.steps.map((s) => (s.kind === 'thinking' ? s.text : ''))).toEqual([
      '第一段思考',
      '第二段思考',
    ]);
  });

  it('一轮里写了两段话、没有工具：前一段是过程，后一段才是答案', () => {
    const items = buildItems(snap([user('讲讲'), assistant('先说背景'), assistant('结论是这些')]));
    expect(items.map((i) => i.kind)).toEqual(['msg', 'process', 'msg']);
    const fold = items[1];
    if (fold.kind !== 'process') throw new Error('没有过程项');
    expect(fold.steps.map((s) => (s.kind === 'note' ? s.msg.content : s.kind))).toEqual(['先说背景']);
    const answer = items[2];
    expect(answer.kind === 'msg' ? answer.msg.content : '').toBe('结论是这些');
  });

  it('SendFile 不折进过程：它是交付物，露在本轮结尾', () => {
    const items = buildItems(
      snap([user('把周报发我'), call('a', 'SendFile'), resultOf('a'), assistant('发你了')]),
    );
    // 过程那一行不该出现：折起来等于把「文件在哪」的答案藏在要展开的行后面
    expect(items.some((i) => i.kind === 'process')).toBe(false);
    // 文件卡片排在本轮最后，即使模型在它之后又说了一句
    const last = items[items.length - 1];
    expect(last.kind).toBe('msg');
    expect(last.kind === 'msg' ? last.msg.toolName : '').toBe('SendFile');
    expect(items.length).toBe(3);
  });

  it('只有一个工具时照旧画成一张卡（折一行没有意义）', () => {
    const items = buildItems(snap([user('跑一下'), call('a'), resultOf('a')]));
    expect(items.map((i) => i.kind)).toEqual(['msg', 'msg']);
    expect(items[1].kind === 'msg' && items[1].msg.role).toBe('tool_call');
  });

  it('正在跑的那一轮也折：思考与工具收成一行，标着在跑，步数跟着工具走', () => {
    const live: Live = {
      ...emptyLive,
      active: true,
      parts: [
        { type: 'thinking', text: '想一想' },
        { type: 'tool', tool: { id: 'a', name: 'Read', title: 'a.go', result: 'ok' } },
        { type: 'tool', tool: { id: 'b', name: 'Grep', title: 'x' } },
        { type: 'text', text: '我先看看' },
      ],
    };
    const items = screen(snap([user('扫一遍')]), live);

    // 过程一行 + 正在写的正文
    expect(items.map((i) => i.kind)).toEqual(['msg', 'process', 'live']);
    const fold = items[1];
    if (fold.kind !== 'process') throw new Error('没有过程项');
    expect(fold.running).toBe(true);
    expect(fold.steps.map((s) => (s.kind === 'tool' ? s.pair.msg.toolCallId : 'thinking'))).toEqual([
      'thinking',
      'a',
      'b',
    ]);
    // 还没回来的那条工具：展开时应当还是「在跑」的样子
    const last = fold.steps[2];
    expect(last.kind === 'tool' && last.pair.result).toBeUndefined();
  });

  it('跑的时候只有一个工具：照旧露成一张卡，但标着在跑', () => {
    const live: Live = {
      ...emptyLive,
      active: true,
      parts: [{ type: 'tool', tool: { id: 'a', name: 'Bash', title: 'ls' } }],
    };
    const items = screen(snap([user('跑一下')]), live);
    const card = items[1];
    expect(card.kind).toBe('msg');
    if (card.kind !== 'msg') throw new Error('类型不对');
    expect(card.msg.role).toBe('tool_call');
    expect(card.running).toBe(true);
  });

  it('跑着的时候也只露正在写的那一段：中间写过的话已经折进过程行', () => {
    const live: Live = {
      ...emptyLive,
      active: true,
      parts: [
        { type: 'text', text: '我先扫一遍' },
        { type: 'tool', tool: { id: 'a', name: 'Read', title: 'a.go', result: 'ok' } },
        { type: 'tool', tool: { id: 'b', name: 'Grep', title: 'x', result: 'ok' } },
        { type: 'text', text: '扫码这件事有三个入口' },
      ],
    };
    const items = screen(snap([user('看一下')]), live);

    // 过程一行 + 正在写的那一段：不把「我先扫一遍」也堆在下面
    expect(items.map((i) => i.kind)).toEqual(['msg', 'process', 'live']);
    const fold = items[1];
    if (fold.kind !== 'process') throw new Error('没有过程项');
    expect(fold.steps.map((s) => s.kind)).toEqual(['note', 'tool', 'tool']);
    const tail = items[2];
    expect(tail.kind === 'live' ? tail.text : '').toBe('扫码这件事有三个入口');
  });

  it('跑着的时候交出来的文件也钉在尾部，不折进过程', () => {
    const live: Live = {
      ...emptyLive,
      active: true,
      parts: [
        { type: 'tool', tool: { id: 'a', name: 'Read', title: 'a.go', result: 'ok' } },
        {
          type: 'tool',
          tool: { id: 'f', name: 'SendFile', title: '周报.pdf', args: '{"path":"/tmp/周报.pdf"}', result: '已发送' },
        },
        { type: 'text', text: '发你了' },
      ],
    };
    const items = buildLiveTail(live);
    // 只有一个工具时它照旧露成一张卡（单步不值得折），文件卡排在最后
    expect(items.map((i) => i.kind)).toEqual(['msg', 'live', 'msg']);
    const file = items[2];
    expect(file.kind === 'msg' ? file.msg.toolName : '').toBe('SendFile');
  });

  it('待答项永远在最后：正在跑的内容不能把卡片挤上去', () => {
    const s = snap([user('问一下'), call('a'), resultOf('a')]);
    s.asks = [{ kind: 'question', toolId: 'a', questions: [{ question: '放哪一层？', options: ['甲', '乙'] }] }];
    const live: Live = { ...emptyLive, active: true, parts: [{ type: 'text', text: '正在写' }] };
    const items = screen(s, live);
    const last = items[items.length - 1];
    expect(last.kind === 'msg' ? last.ask?.toolId : '').toBe('a');
  });

  it('待答项排在消息之后', () => {
    const s = snap([user('删掉它'), call('a'), resultOf('a')]);
    s.asks = [{ kind: 'permission', toolId: 'a', toolName: 'Bash', permissionKind: 'bash', preview: 'rm -rf x' }];
    const items = screen(s, emptyLive);
    const last = items[items.length - 1];
    expect(last.kind).toBe('msg');
    expect(last.kind === 'msg' ? last.ask?.toolId : '').toBe('a');
  });
});
