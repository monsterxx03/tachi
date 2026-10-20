import { Platform } from 'react-native';
import { ApiError, normalizeSnapshot } from '../api/client';
import type { Snapshot } from '../api/types';
import { applyEvent, emptyChat, messageOfError, type ChatState } from './chat';

/** 一份最小的快照；要什么字段就在参数里覆盖 */
function snap(partial: Partial<Snapshot> = {}): Snapshot {
  return {
    session: {
      id: 's1',
      title: '清理构建目录',
      updatedAt: '刚刚',
      running: false,
      waiting: '',
      readOnly: false,
      heldByTask: false,
    },
    messages: [],
    hasMore: false,
    state: { status: 'idle', label: '空闲', detail: '', waiting: '' },
    asks: [],
    model: 'Alpha 4.5',
    skipApproval: false,
    workspace: { primary: '~/repos/tachi', additional: [] },
    usage: { cost: 0.12 },
    context: { estimate: 100, window: 1000, parts: [] },
    attachments: [],
    ...partial,
  };
}

function withSnapshot(partial: Partial<Snapshot> = {}): ChatState {
  return { snapshot: snap(partial), live: emptyChat.live };
}

describe('applyEvent —— 增量怎么落到界面上', () => {
  it('text 是一段段接起来的，不是替换', () => {
    let st = withSnapshot();
    st = applyEvent(st, { type: 'text', text: '你' }, 'mobile').state;
    st = applyEvent(st, { type: 'text', text: '好' }, 'mobile').state;
    expect(st.live.text).toBe('你好');
    expect(st.live.active).toBe(true);
  });

  it('snapshot 一来就换掉整份，并把活气泡清空', () => {
    const st = applyEvent(
      { ...withSnapshot(), live: { active: true, text: '半句话', thinking: '', tools: [] } },
      { type: 'snapshot', snapshot: snap({ messages: [{ index: 0, role: 'user', content: '喂' }] }) },
      'mobile',
    ).state;
    expect(st.snapshot?.messages).toHaveLength(1);
    expect(st.live.text).toBe('');
    expect(st.live.active).toBe(false);
  });

  it('一轮结束、出错、跟不上，都要重读快照（记录才是权威）', () => {
    for (const ev of [
      { type: 'turn', end: 'complete', durationMs: 1, iterations: 1 },
      { type: 'error', message: 'boom', interrupted: false },
      { type: 'idle', end: 'complete' },
      { type: 'lagged' },
    ] as const) {
      const { needsReload } = applyEvent(withSnapshot(), ev, 'mobile');
      expect(needsReload).toBe(true);
    }
  });

  it('待答项出现或消失，也要重读快照（它带着 asks）', () => {
    const ask = {
      type: 'ask' as const,
      ask: { kind: 'permission' as const, toolId: 't1', toolName: 'Bash', permissionKind: 'bash' as const, preview: '$ rm -rf x' },
    };
    expect(applyEvent(withSnapshot(), ask, 'mobile').needsReload).toBe(true);
    expect(applyEvent(withSnapshot(), { type: 'ask_answered', toolId: 't1' }, 'mobile').needsReload).toBe(true);
  });

  it('自己发的那条不重复画，另一块屏发的要画出来', () => {
    const mine = applyEvent(
      withSnapshot(),
      { type: 'user_message', text: '我说的', steer: false, origin: 'mobile' },
      'mobile',
    ).state;
    expect(mine.snapshot?.messages).toHaveLength(0);

    const other = applyEvent(
      withSnapshot(),
      { type: 'user_message', text: '桌面说的', steer: false, origin: 'desktop' },
      'mobile',
    ).state;
    expect(other.snapshot?.messages).toHaveLength(1);
    expect(other.snapshot?.messages[0].content).toBe('桌面说的');
  });

  it('两个会话开关被另一块屏改了，这边立刻跟着变（不用等重读）', () => {
    const st = applyEvent(
      withSnapshot(),
      { type: 'session_prefs', provider: 'smoke-alt', skipApproval: true },
      'mobile',
    ).state;
    expect(st.snapshot?.model).toBe('smoke-alt');
    expect(st.snapshot?.skipApproval).toBe(true);
  });

  it('工具结果补在它自己的那条调用上，不会新起一条', () => {
    let st = withSnapshot();
    st = applyEvent(st, { type: 'tool_start', toolId: 't1', toolName: 'Bash', title: 'du -sh build' }, 'mobile').state;
    st = applyEvent(st, { type: 'tool_start', toolId: 't2', toolName: 'Grep', title: 'login' }, 'mobile').state;
    st = applyEvent(st, { type: 'tool_result', toolId: 't1', isError: false, result: '328M', truncated: false }, 'mobile').state;

    expect(st.live.tools).toHaveLength(2);
    expect(st.live.tools[0].result).toBe('328M');
    expect(st.live.tools[1].result).toBeUndefined();
  });

  it('state 事件决定那句「在跑」是亮还是灭', () => {
    const working = applyEvent(
      withSnapshot(),
      { type: 'state', state: { status: 'working', label: '在跑', detail: '', waiting: '' } },
      'mobile',
    ).state;
    expect(working.snapshot?.session.running).toBe(true);

    // 等着你回答 = 没在跑，但确实停在那里
    const waiting = applyEvent(
      withSnapshot(),
      { type: 'state', state: { status: 'waiting', label: '等你回答', detail: '', waiting: '等待你的回答' } },
      'mobile',
    ).state;
    expect(waiting.snapshot?.session.running).toBe(false);
    expect(waiting.snapshot?.session.waiting).toBe('等待你的回答');
  });

  it('还没打开任何会话时收到事件，不会把界面弄崩', () => {
    const { state } = applyEvent(emptyChat, { type: 'text', text: '喂' }, 'mobile');
    expect(state.snapshot).toBeNull();
  });
});

describe('messageOfError —— 服务端拒绝的原因要原样变成人话', () => {
  it('协议里那几句各自有对应', () => {
    expect(messageOfError(new ApiError(401, 'unauthorized'))).toBe('令牌已失效，请在电脑上重新配对');
    expect(messageOfError(new ApiError(404, 'no_session'))).toBe('这段对话已经不在了');
    expect(messageOfError(new ApiError(409, 'turn_running'))).toBe('它正在跑；插话或打断？');
    expect(messageOfError(new ApiError(409, 'no_such_pending_request'))).toBe('已经在别处回答过了');
    expect(messageOfError(new ApiError(409, 'agent_unavailable'))).toBe('这台电脑还没配好模型，先在电脑上配置');
    expect(messageOfError(new ApiError(409, 'read_only'))).toBe('这段对话由别的任务占用');
  });

  it('没见过的码也要说得出话，不能是空白', () => {
    expect(messageOfError(new ApiError(500, 'weird'))).toContain('weird');
  });

  it('连不上和上面那些不是一回事', () => {
    const text = messageOfError(new TypeError('Network request failed'));
    expect(text.startsWith('连不上电脑')).toBe(true);
    // iOS 多一种「看起来一样」的原因：本地网络权限被拒。那句话必须把它说出来，
    // 否则读者只会去检查 Wi-Fi 和那台电脑。
    if (Platform.OS === 'ios') expect(text).toContain('本地网络权限');
  });
});

describe('normalizeSnapshot —— 空数组不能变 null', () => {
  it('null 收成空数组（服务端少写一次初始化，崩的是这里）', () => {
    const raw = {
      ...snap(),
      messages: null,
      asks: null,
      attachments: null,
      workspace: { primary: '~', additional: null },
      context: { estimate: 1, window: 2, parts: null },
    } as unknown as Snapshot;

    const fixed = normalizeSnapshot(raw);
    expect(fixed.messages).toEqual([]);
    expect(fixed.asks).toEqual([]);
    expect(fixed.attachments).toEqual([]);
    expect(fixed.workspace.additional).toEqual([]);
    expect(fixed.context.parts).toEqual([]);
  });
});
