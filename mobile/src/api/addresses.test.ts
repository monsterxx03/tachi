import { fastestPairing, nextAddress, normalizeAddresses, PROBE_TIMEOUT_MS } from './addresses';
import type { PingResult } from './client';

const LAN = 'http://192.168.71.183:8787';
const TS = 'http://100.98.239.75:8787';

/** 一个假的 /v1/ping：delay 毫秒后应答，delay = Infinity 表示永远不应（直到被 abort）。 */
function fakePing(table: Record<string, number>, answer?: PingResult) {
  return (url: string, signal?: AbortSignal) =>
    new Promise<PingResult>((resolve, reject) => {
      const delay = table[url] ?? Infinity;
      if (delay === Infinity) {
        signal?.addEventListener('abort', () => reject(new Error('aborted')));
        return;
      }
      const timer = setTimeout(() => resolve(answer ?? { ok: true, hostname: 'h' }), delay);
      signal?.addEventListener('abort', () => {
        clearTimeout(timer);
        reject(new Error('aborted'));
      });
    });
}

describe('normalizeAddresses —— 能照着试的地址表', () => {
  it('主地址排第一、去重、去尾部斜杠', () => {
    expect(
      normalizeAddresses({ baseUrl: 'http://a:1/', urls: ['http://b:2', 'http://a:1', 'http://b:2/'] }),
    ).toEqual(['http://a:1', 'http://b:2']);
  });

  it('不像地址的丢掉（空格、空串、别的 scheme）', () => {
    expect(normalizeAddresses({ baseUrl: ' ftp://x ', urls: ['', '  ', 'https://ok:3'] })).toEqual([
      'https://ok:3',
    ]);
  });

  it('一条都不剩时给空数组，调用方据此什么都不试', () => {
    expect(normalizeAddresses({ baseUrl: '', urls: [] })).toEqual([]);
  });
});

describe('fastestPairing —— 同时试，谁先应答用谁', () => {
  it('内网比 Tailscale 快时就选内网，地址表两各都留着', async () => {
    const got = await fastestPairing(
      { baseUrl: LAN, token: 't', urls: [LAN, TS] },
      PROBE_TIMEOUT_MS,
      fakePing({ [LAN]: 1, [TS]: 30 }),
    );
    expect(got?.pairing.baseUrl).toBe(LAN);
    expect(got?.urls).toEqual([LAN, TS]);
  });

  it('内网不通（在外面）时选 Tailscale —— 不必等它超时', async () => {
    const started = Date.now();
    const got = await fastestPairing(
      { baseUrl: LAN, token: 't', urls: [LAN, TS] },
      PROBE_TIMEOUT_MS,
      fakePing({ [TS]: 5 }), // 局域网那条永远不应
    );
    expect(got?.pairing.baseUrl).toBe(TS);
    // 顺序试的话这里会等到 2.5 秒超时才轮下一条
    expect(Date.now() - started).toBeLessThan(PROBE_TIMEOUT_MS / 2);
  });

  it('服务端报的地址表比手上的新时，以它为准并把它排第一', async () => {
    const newer = 'http://100.64.0.9:8787';
    const got = await fastestPairing(
      { baseUrl: LAN, token: 't' }, // 手里只有一条
      PROBE_TIMEOUT_MS,
      fakePing({ [LAN]: 1 }, { ok: true, urls: [newer, LAN] }),
    );
    expect(got?.urls).toEqual([LAN, newer]);
  });

  it('一条都不通时返回 null（调用方据此换下一条地址去试）', async () => {
    // 用一个短上限：这条测的是「全失败」这个分支，不必陪真正的 8 秒超时
    const got = await fastestPairing({ baseUrl: LAN, token: 't', urls: [TS] }, 120, fakePing({}));
    expect(got).toBeNull();
  });

  it('慢但能应答的那条也算赢（tailnet 冷启动实测要 2.6 秒）', async () => {
    const got = await fastestPairing(
      { baseUrl: LAN, token: 't', urls: [LAN, TS] },
      PROBE_TIMEOUT_MS,
      fakePing({ [TS]: 2_800 }), // 局域网那条不应答
    );
    expect(got?.pairing.baseUrl).toBe(TS);
  }, 15_000);

  it('顺序轮换：探不出结果时按已知顺序换下一条', () => {
    expect(nextAddress([LAN, TS], LAN)).toBe(TS);
    expect(nextAddress([LAN, TS], TS)).toBe(LAN);
    expect(nextAddress([LAN], LAN)).toBeNull();
  });

  it('一条地址都没有时不发请求、直接 null', async () => {
    let called = 0;
    const got = await fastestPairing({ baseUrl: '', token: 't', urls: [] }, PROBE_TIMEOUT_MS, () => {
      called += 1;
      return Promise.resolve({ ok: true });
    });
    expect(got).toBeNull();
    expect(called).toBe(0);
  });
});
