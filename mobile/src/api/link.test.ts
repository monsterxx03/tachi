import { parseDeepLink } from './link';

describe('parseDeepLink', () => {
  it('要点：会话和机器都要认出来', () => {
    expect(parseDeepLink('tachi://session/abc?device=d1')).toEqual({ sessionId: 'abc', deviceId: 'd1' });
  });

  it('老推送没有 device：只在当前这台电脑上找', () => {
    expect(parseDeepLink('tachi://session/abc')).toEqual({ sessionId: 'abc', deviceId: '' });
  });

  it('参数顺序与转义都不影响', () => {
    expect(parseDeepLink('tachi://session/a%20b?x=1&device=d%2F1')).toEqual({
      sessionId: 'a b',
      deviceId: 'd/1',
    });
  });

  it('别的链接一律不认', () => {
    expect(parseDeepLink('https://example.com/session/abc')).toBeNull();
    expect(parseDeepLink('tachi://sessions/abc')).toBeNull();
    expect(parseDeepLink('tachi://session/')).toBeNull();
    expect(parseDeepLink('')).toBeNull();
  });
});
