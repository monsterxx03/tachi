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

  // urls 是后加的字段：一台电脑既有局域网地址又有 Tailscale 地址时，一次扫描要把它们一起记住，
  // 不然手机只知道「读者当时填的那条」，换个网络就没法连。
  it('码里的全部地址都记住，主地址排第一', () => {
    const raw =
      '{"v":1,"url":"http://192.168.1.10:8787","urls":["http://100.98.239.75:8787/","http://192.168.1.10:8787"],"token":"t"}';
    expect(parsePairingQR(raw)).toEqual({
      baseUrl: 'http://192.168.1.10:8787',
      token: 't',
      urls: ['http://192.168.1.10:8787', 'http://100.98.239.75:8787'],
    });
  });

  it('urls 里的垃圾项被丢掉，能用的照旧', () => {
    const raw = '{"v":1,"url":"http://192.168.1.10:8787","urls":[42,"nope",null],"token":"t"}';
    expect(parsePairingQR(raw)).toEqual({ baseUrl: 'http://192.168.1.10:8787', token: 't' });
  });

  it('只有一条地址时不带 urls 字段（与旧形状一致）', () => {
    const got = parsePairingQR('{"v":1,"url":"http://192.168.1.10:8787","urls":[],"token":"t"}');
    expect(got).toEqual({ baseUrl: 'http://192.168.1.10:8787', token: 't' });
    expect(got?.urls).toBeUndefined();
  });
});
