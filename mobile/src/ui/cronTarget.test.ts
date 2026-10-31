import { cronTargetText } from './cronTarget';

describe('cronTargetText', () => {
  it('says a dedicated conversation is not created yet, instead of calling it gone', () => {
    // 这个用例就是那个 bug：pending 没有标题，旧代码把「没有标题」一律读成「会话不在了」
    expect(cronTargetText({ targetKind: 'pending', targetTitle: '' })).toBe('专属会话（待创建）');
  });

  it('still says gone when the conversation really is gone', () => {
    expect(cronTargetText({ targetKind: 'missing', targetTitle: '' })).toBe('会话已删除');
  });

  it('names a bound conversation by its title', () => {
    expect(cronTargetText({ targetKind: 'session', targetTitle: '索引估值提醒' })).toBe('索引估值提醒');
    // 绑定了却查不到标题（正在读列表的那一瞬间）：说「会话」，不是「不在了」
    expect(cronTargetText({ targetKind: 'session', targetTitle: '' })).toBe('会话');
  });

  it('keeps the channel jobs on their own word', () => {
    expect(cronTargetText({ targetKind: 'channel', targetTitle: 'discord' })).toBe('渠道 · discord');
    expect(cronTargetText({ targetKind: 'channel', targetTitle: '' })).toBe('渠道');
  });

  it('treats an unknown kind as a plain conversation', () => {
    expect(cronTargetText({ targetKind: '', targetTitle: '' })).toBe('会话');
  });
});
