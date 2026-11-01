import { formatMessageTime, formatNextRun, formatNextWake, formatRunAt, formatUpdatedAt } from './time';

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

describe('formatNextRun', () => {
  it('gives month-day and clock for a schedule that repeats', () => {
    expect(formatNextRun('2026-11-01T15:30:00+08:00')).toBe('11-01 15:30');
  });

  it('returns what it cannot recognise unchanged', () => {
    expect(formatNextRun('')).toBe('');
    expect(formatNextRun('明天')).toBe('明天');
  });
});

describe('formatRunAt', () => {
  const now = new Date('2026-10-31T20:00:00+08:00');

  it('says 今天 for a moment later today', () => {
    expect(formatRunAt('2026-10-31T23:10:00+08:00', now)).toBe('今天 23:10');
  });

  it('gives the date for a later day, and the year when it is another year', () => {
    // 一次性任务的时刻常在几天后、甚至明年：裸的「11-01 13:30」会被读成今天
    expect(formatRunAt('2026-11-01T13:30:00+08:00', now)).toBe('11月1日 13:30');
    expect(formatRunAt('2027-01-04T09:00:00+08:00', now)).toBe('2027年1月4日 09:00');
  });

  it('returns what it cannot recognise unchanged', () => {
    expect(formatRunAt('', now)).toBe('');
    expect(formatRunAt('下周三', now)).toBe('下周三');
  });
});

describe('formatNextWake', () => {
  // 月中，「本月」和「今年」才各有一个未来时刻可指
  const now = new Date('2026-10-10T20:00:00+08:00');

  it('今天只给钟点，本月给日子，今年给月日，再远给年月', () => {
    // 这一行里已经有时刻、标题和「最近一次」，时间只是那颗小闹钟的定语，所以能省就省
    expect(formatNextWake('2026-10-10T23:00:00+08:00', now)).toBe('23:00');
    expect(formatNextWake('2026-10-21T08:30:00+08:00', now)).toBe('21日 08:30');
    expect(formatNextWake('2026-12-01T07:05:00+08:00', now)).toBe('12月1日 07:05');
    expect(formatNextWake('2027-01-04T09:00:00+08:00', now)).toBe('2027年1月4日 09:00');
  });

  it('缺着或认不出来就是空串：徽章只说它知道的那半句', () => {
    expect(formatNextWake(undefined, now)).toBe('');
    expect(formatNextWake('', now)).toBe('');
    expect(formatNextWake('明天', now)).toBe('');
  });
});
