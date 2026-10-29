import { ping, type Pairing, type PingResult } from './client';

/**
 * 一台电脑的地址可能不止一条：家里的局域网地址（快，但只有同一个 Wi-Fi 下能用）和 Tailscale
 * 的 100.x 地址（慢一点，但在任何地方都能用）。手机不去猜自己现在在哪儿 —— 它**同时**试所有
 * 地址，谁先应答就用谁。
 *
 * 顺序试的坏处很具体：人在外面时，局域网那条必然要等到超时才轮到下一条，于是每次打开 App
 * 都要先白等一次超时。并发试的总耗时就是「最快那条的往返」，在内网是几毫秒。
 */

/**
 * 一条地址最多等多久。
 *
 * 这个数字不能小气：手机在蜂窝上连一台 Wi-Fi 不太好的机器时，tailnet 那条**冷启动**实测要
 * 2.6 秒（TCP + Tailscale 打洞都在这一步里）；定成 2.5 秒的结果是「每次都被自己掐断」，
 * 于是每一轮都判定「哪儿都不通」而原地不动 —— 看起来就是死活不切换。
 */
export const PROBE_TIMEOUT_MS = 8000;

/**
 * 换到下一条地址：按已知顺序轮过去。
 *
 * 探测全失败时用它兜底 —— 「探不出结果」不等于「这条路不存在」（网络烂的时候它只是没来得及
 * 应答）。人已经离开家了，守着那条只在家里存在的地址才是真的没救。
 */
export function nextAddress(urls: string[], current: string): string | null {
  if (urls.length < 2) return null;
  const at = urls.indexOf(current);
  return urls[(at + 1) % urls.length] ?? null;
}

/**
 * 把一条配对里的地址整理成「可以照着试」的列表：主地址第一、去重、去掉尾部斜杠、丢掉不像
 * http(s) 的东西。
 *
 * 服务端下发的是它自己算出来的 URL，但手输和扫描这两条路都可能带进来一个多一个斜杠或者空格的
 * 值 —— 那种值直接拼进请求就是一条必然失败的探测，而失败的原因看起来会像「那台电脑没开」。
 */
export function normalizeAddresses(p: { baseUrl: string; urls?: string[] }): string[] {
  const out: string[] = [];
  for (const raw of [p.baseUrl, ...(p.urls ?? [])]) {
    const url = (raw ?? '').trim().replace(/\/+$/, '');
    if (!/^https?:\/\/[^/]+/.test(url)) continue;
    if (out.includes(url)) continue;
    out.push(url);
  }
  return out;
}

export interface Picked {
  /** 用赢的那条地址拼出来的配对（地址表已经按「赢家第一」整理过） */
  pairing: Pairing;
  /** 这台电脑当前的地址表：服务端报的（可能比手上的新），没有就是试过的这批 */
  urls: string[];
  /** 赢的那次 ping 的答复 */
  ping: PingResult;
}

/** 配对页探测的结论：无论通不通，都要给出一份**可以存下来**的配对。 */
export interface ProbeOutcome {
  pairing: Pairing;
  /** 应答的那台机器的自我介绍；null = 一条都没应答（照样记下来，只是没有名字可抄） */
  ping: PingResult | null;
}

/**
 * 配对页那一次探测。
 *
 * 它不回答「能不能配对」——**能**，配对只要地址和令牌。它只回答两件顺带的事：这台电脑现在
 * 走哪条地址（并发试全部，谁先应答用谁，见 `fastestPairing`），以及它叫什么名字。
 *
 * 一条都不应答时仍然返回一份带完整地址表的配对：地址可能只是**此刻**不通（那台电脑还没开、
 * 或眼下这个网络进不去），记下来之后手机会自己继续找路（`heal` / 换下一条）。否则读者对着
 * 一个填好的配对页干等一次系统超时，最后什么也没留下。
 */
export async function probePairing(
  pairing: Pairing,
  timeoutMs: number = PROBE_TIMEOUT_MS,
  probe: (url: string, signal?: AbortSignal) => Promise<PingResult> = ping,
): Promise<ProbeOutcome> {
  const addresses = normalizeAddresses(pairing);
  const picked = await fastestPairing(pairing, timeoutMs, probe);
  if (picked) return { pairing: picked.pairing, ping: picked.ping };
  return {
    pairing: {
      ...pairing,
      baseUrl: addresses[0] ?? pairing.baseUrl,
      urls: addresses.length > 1 ? addresses : undefined,
    },
    ping: null,
  };
}

/**
 * 并发试这台电脑的全部地址，第一个应答的胜出；一条都不通时返回 null（调用方按原来的地址继续，
 * 界面上的「连不上」说的就是这件事）。
 *
 * 服务端在 ping 里也报一份地址表，所以赢的那次会**顺手带回最新的一批**：桌面换过网络、多了
 * Tailscale 地址，手机不用重新扫码就知道。
 */
export async function fastestPairing(
  pairing: Pairing,
  timeoutMs: number = PROBE_TIMEOUT_MS,
  probe: (url: string, signal?: AbortSignal) => Promise<PingResult> = ping,
): Promise<Picked | null> {
  const addresses = normalizeAddresses(pairing);
  if (addresses.length === 0) return null;

  return new Promise<Picked | null>((resolve) => {
    let settled = false;
    let failed = 0;
    const controls = addresses.map(() => new AbortController());

    addresses.forEach((url, i) => {
      const abort = () => controls[i].abort();
      const timer = setTimeout(abort, timeoutMs);
      probe(url, controls[i].signal)
        .then((answer) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          // 输的那几条不必再等：它们的回答已经没人要了
          controls.forEach((c, j) => {
            if (j !== i) c.abort();
          });
          const urls = normalizeAddresses({ baseUrl: url, urls: answer.urls ?? addresses });
          resolve({ pairing: { ...pairing, baseUrl: url, urls }, urls, ping: answer });
        })
        .catch(() => {
          clearTimeout(timer);
          if (settled) return;
          failed += 1;
          if (failed === addresses.length) {
            settled = true;
            resolve(null);
          }
        });
    });
  });
}
