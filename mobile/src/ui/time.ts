/**
 * 会话列表那一行的「最近一次是什么时候」。
 *
 * 服务端发的是 **RFC3339**（`mobileSummary` 里 `UpdatedAt.Format(time.RFC3339)`），而手机上
 * 原样画出来是 `2026-10-31T18:26:24+08:00` —— 一行里最长的那截，却不是读者要看的那件事。
 * 所以这里把它折成手机上惯用的说法。
 *
 * **认不出来的字符串原样返回**，不猜：mockapi 给的就是「昨天 / 2 小时前 / 3 分钟前」这种
 * 现成的话（e2e 断言的是它们），老一点的服务端也可能发别的。格式化只发生在确认它是时间戳的时候。
 */

/** 带时区的完整时间戳（`Z` 或 `+08:00`）：没有时区的「本地时间」按什么读都说不清，不认 */
const TIMESTAMP = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})$/;

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;

function pad(n: number): string {
  return n < 10 ? `0${n}` : String(n);
}

function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

/** `now` 只给测试用；调用方一个参数就够 */
export function formatUpdatedAt(raw: string, now: Date = new Date()): string {
  if (!raw || !TIMESTAMP.test(raw)) return raw;
  const t = new Date(raw);
  const ms = t.getTime();
  if (Number.isNaN(ms)) return raw;

  const diff = now.getTime() - ms;
  // 差一点点就到「未来」的那种（对表偏差）也算刚刚，不然会显示成负数的分钟
  if (diff < MINUTE) return '刚刚';
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)} 分钟前`;
  if (sameDay(t, now)) return `${pad(t.getHours())}:${pad(t.getMinutes())}`;
  // 「昨天」按日历算，不按 24 小时前：跨夏令时、以及刚过零点的那几分钟都对得上
  const yesterday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
  if (sameDay(t, yesterday)) return '昨天';
  if (t.getFullYear() === now.getFullYear()) return `${t.getMonth() + 1}月${t.getDate()}日`;
  return `${t.getFullYear()}年${t.getMonth() + 1}月${t.getDate()}日`;
}
