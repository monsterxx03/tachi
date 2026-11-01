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

/** 认得出才给 Date，认不出给 null —— 三个格式化函数共同的「原样返回」入口 */
function parseStamp(raw: string): Date | null {
  if (!raw || !TIMESTAMP.test(raw)) return null;
  const t = new Date(raw);
  return Number.isNaN(t.getTime()) ? null : t;
}

/** `now` 只给测试用；调用方一个参数就够 */
export function formatUpdatedAt(raw: string, now: Date = new Date()): string {
  const t = parseStamp(raw);
  if (!t) return raw;
  const ms = t.getTime();

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

/**
 * 消息气泡上那个时刻。
 *
 * 和上面的「最近一次是什么时候」不是同一条规矩：那里回答「多久以前」（刚刚 / 3 分钟前），
 * 这里回答「这句话是什么时候说的」——所以今天的一律给钟点，过去的给日期：一个钟点离开它
 * 的日子没有意义，而「3 分钟前」写在一条旧消息旁边只会随读者看屏幕的时间漂。
 *
 * 认不出来的字符串原样返回，与 formatUpdatedAt 同一条规矩。
 */
export function formatMessageTime(raw: string, now: Date = new Date()): string {
  const t = parseStamp(raw);
  if (!t) return raw;

  if (sameDay(t, now)) return `${pad(t.getHours())}:${pad(t.getMinutes())}`;
  if (t.getFullYear() === now.getFullYear()) return `${t.getMonth() + 1}月${t.getDate()}日`;
  return `${t.getFullYear()}年${t.getMonth() + 1}月${t.getDate()}日`;
}

/**
 * 定时任务行的「下一次」。
 *
 * 两次运行之间通常只有几小时到一天，所以「MM-DD HH:mm」够读、也最短。
 */
export function formatNextRun(raw: string): string {
  const t = parseStamp(raw);
  if (!t) return raw;
  return `${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}`;
}

/**
 * 一次性任务「哪一刻跑」。
 *
 * 和 formatNextRun 不是同一条规矩：那个时刻可能在未来的某一天、甚至明年，一个裸的
 * 「11-01 13:30」读起来就像今天 —— 而这行字右边就是它的删除按钮。所以今天说「今天」，
 * 今年内说月日，再往后补上年。
 */
export function formatRunAt(raw: string, now: Date = new Date()): string {
  const t = parseStamp(raw);
  if (!t) return raw;
  const hm = `${pad(t.getHours())}:${pad(t.getMinutes())}`;
  if (sameDay(t, now)) return `今天 ${hm}`;
  if (t.getFullYear() === now.getFullYear()) return `${t.getMonth() + 1}月${t.getDate()}日 ${hm}`;
  return `${t.getFullYear()}年${t.getMonth() + 1}月${t.getDate()}日 ${hm}`;
}
