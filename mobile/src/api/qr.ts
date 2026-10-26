import type { Pairing } from './client';
import { normalizeAddresses } from './addresses';

/**
 * 电脑上那个二维码里装的东西（协议里定的形状）：
 *
 *   {"v":1,"url":"http://192.168.1.10:8787","urls":["…","http://100.98.239.75:8787"],"token":"…"}
 *
 * `url` 是主地址（旧的桌面端只写这一个，所以它必须留着），`urls` 是这台电脑的全部地址 —— 家里
 * 局域网的与 Tailscale 的。手机把它们一起记住，连的时候并发试、谁先应答用谁。
 *
 * 只认这一种形状：扫码扫到别的东西（网页、名片、另一个 App 的码）时返回 null，
 * 让调用方继续扫，而不是拿半截数据去连一个错的地方。
 */
export function parsePairingQR(raw: string): Pairing | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null) return null;

  const v = parsed as { v?: unknown; url?: unknown; urls?: unknown; token?: unknown };
  if (v.v !== 1) return null;
  if (typeof v.url !== 'string' || typeof v.token !== 'string') return null;

  const token = v.token.trim();
  if (!token) return null;

  // urls 是后加的：旧的桌面端没有它，那就只有 url 这一条。垃圾项在 normalizeAddresses 里被丢掉，
  // 所以这里不为了「码不干净」而整个拒掉一个能用的配对。
  const urls = Array.isArray(v.urls) ? v.urls.filter((u): u is string => typeof u === 'string') : [];
  const addresses = normalizeAddresses({ baseUrl: v.url, urls });
  const [baseUrl, ...rest] = addresses;
  if (!baseUrl) return null;

  return rest.length > 0 ? { baseUrl, token, urls: addresses } : { baseUrl, token };
}
