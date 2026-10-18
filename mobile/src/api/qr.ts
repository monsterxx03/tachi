import type { Pairing } from './client';

/**
 * 电脑上那个二维码里装的东西（协议里定的形状）：
 *
 *   {"v":1,"url":"http://192.168.1.10:8787","token":"…"}
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

  const v = parsed as { v?: unknown; url?: unknown; token?: unknown };
  if (v.v !== 1) return null;
  if (typeof v.url !== 'string' || typeof v.token !== 'string') return null;

  const baseUrl = v.url.trim().replace(/\/+$/, '');
  const token = v.token.trim();
  if (!/^https?:\/\/[^/]+/.test(baseUrl)) return null;
  if (!token) return null;

  return { baseUrl, token };
}
