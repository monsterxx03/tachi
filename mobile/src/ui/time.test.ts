import { formatMessageTime, formatUpdatedAt } from './time';

// 固定一个「现在」：这些断言说的是规则（几分钟前 / 今天 / 昨天 / 更早），不是今天的日期
const now = new Date('2026-10-31T18:30:00+08:00');

describe('formatUpdatedAt', () => {
  it('几分钟以内说「刚刚」，一小时以内说多少分钟前', () => {
    expect(formatUpdatedAt('2026-10-31T18:29:30+08:00', now)).toBe('刚刚');
    expect(formatUpdatedAt('2026-10-31T18:12:00+08:00', now)).toBe('18 分钟前');
  });

  // 时钟对不齐时服务端可能给一个「未来」的时间：不能显示成「-3 分钟前」
  it('未来一点点也算刚刚', () => {
    expect(formatUpdatedAt('2026-10-31T18:33:00+08:00', now)).toBe('刚刚');
  });

  it('今天更早的时刻说钟点', () => {
    expect(formatUpdatedAt('2026-10-31T09:05:00+08:00', now)).toBe('09:05');
  });

  it('昨天、今年更早、往年各有一种说法', () => {
    expect(formatUpdatedAt('2026-10-30T23:50:00+08:00', now)).toBe('昨天');
    expect(formatUpdatedAt('2026-03-02T10:00:00+08:00', now)).toBe('3月2日');
    expect(formatUpdatedAt('2025-12-31T10:00:00+08:00', now)).toBe('2025年12月31日');
  });

  // 服务端换了说法也照原样显示：mockapi 给的就是这种现成的话，而「认不出来」不等于「坏了」
  it('不是时间戳的字符串原样返回', () => {
    expect(formatUpdatedAt('昨天', now)).toBe('昨天');
    expect(formatUpdatedAt('2 小时前', now)).toBe('2 小时前');
    expect(formatUpdatedAt('', now)).toBe('');
  });
});

describe('formatMessageTime', () => {
  const now = new Date('2026-10-31T20:00:00+08:00');

  it('gives the clock for a message from today, never “N minutes ago”', () => {
    // 一条消息的时刻是它自己的事实，不该随着读者盯着屏幕的时间变
    expect(formatMessageTime('2026-10-31T18:29:30+08:00', now)).toBe('18:29');
    expect(formatMessageTime('2026-10-31T00:05:00+08:00', now)).toBe('00:05');
  });

  it('gives the date for an older day, and the year when it is another year', () => {
    expect(formatMessageTime('2026-10-30T23:59:00+08:00', now)).toBe('10月30日');
    expect(formatMessageTime('2025-12-31T09:00:00+08:00', now)).toBe('2025年12月31日');
  });

  it('returns what it cannot recognise unchanged', () => {
    // mockapi 的种子与老服务端都可能发别的东西，猜一个时间出来比不显示更糟
    expect(formatMessageTime('刚刚', now)).toBe('刚刚');
    expect(formatMessageTime('', now)).toBe('');
  });
});
