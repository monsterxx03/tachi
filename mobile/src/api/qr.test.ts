import { parsePairingQR } from './qr';

describe('parsePairingQR —— 只认电脑上那个码', () => {
  it('正常的一张码', () => {
    expect(parsePairingQR('{"v":1,"url":"http://192.168.1.10:8787","token":"abc123"}')).toEqual({
      baseUrl: 'http://192.168.1.10:8787',
      token: 'abc123',
    });
  });

  it('地址末尾的斜杠去掉（不然拼出来是 //v1/ping）', () => {
    expect(parsePairingQR('{"v":1,"url":"http://192.168.1.10:8787/","token":"t"}')?.baseUrl).toBe(
      'http://192.168.1.10:8787',
    );
  });

  it('扫到别的东西一律不接受', () => {
    for (const raw of [
      'https://example.com',
      'hello',
      '',
      '{"v":2,"url":"http://x:1","token":"t"}', // 版本不认识
      '{"v":1,"url":"ftp://x","token":"t"}', // 不是 http
      '{"v":1,"url":"http://x:1"}', // 没有令牌
      '{"v":1,"token":"t"}', // 没有地址
      '{"v":1,"url":"http://x:1","token":"  "}', // 令牌是空白
      'null',
      '[1,2,3]',
    ]) {
      expect(parsePairingQR(raw)).toBeNull();
    }
  });
});
