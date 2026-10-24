/**
 * 上下文那两处（总量 / 分项）的 token 数：写成「12k」「200k」而不是 12345、200000。
 *
 * 这几行是用来判断「还剩多少空间」的，读者看的是量级；一串精确到个位的数字在手机上既占地方、
 * 又要在换行时被折成两段。窗口大小（通常是 200k）和估算值用同一条规则，所以两者的比例一眼
 * 就能对上。
 */
export function humanTokens(n: number): string {
  const v = Math.max(0, Math.round(Number.isFinite(n) ? n : 0));
  if (v < 1000) return String(v);
  if (v < 10_000) return `${trimZero((v / 1000).toFixed(1))}k`;
  if (v < 1_000_000) return `${Math.round(v / 1000)}k`;
  return `${trimZero((v / 1_000_000).toFixed(1))}M`;
}

/** 去掉「12.0」那个多余的 .0 —— 整数不要拖一条尾巴。 */
function trimZero(s: string): string {
  return s.endsWith('.0') ? s.slice(0, -2) : s;
}
