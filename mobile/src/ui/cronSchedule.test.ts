import { cronPlanText, cronWhenText } from './cronSchedule';

// 固定一个「现在」：断言说的是规则（今天 / 哪天 / 哪个钟点），不是今天几号
const now = new Date('2026-10-31T20:00:00+08:00');

describe('cronPlanText', () => {
  it('names the schedule of a repeating job, then where it runs', () => {
    expect(
      cronPlanText({ type: 'recurring', scheduleText: '每个工作日 15:30', targetKind: 'session', targetTitle: '索引估值提醒' }),
    ).toBe('每个工作日 15:30 · 索引估值提醒');
  });

  // 这条就是那个 issue：一次性任务的 scheduleText 是「30 13 1 11 *」——「11 月 1 日 13:30
  // 跑一次」在 cron 语法里的写法，摊在屏幕上只会让人以为它每年都要跑一遍
  it('never shows the cron expression of an oneshot job', () => {
    expect(
      cronPlanText({ type: 'oneshot', scheduleText: '30 13 1 11 *', targetKind: 'session', targetTitle: '上线清单' }),
    ).toBe('上线清单');
  });

  it('still says where an oneshot runs, and says nothing at all when there is nowhere', () => {
    expect(
      cronPlanText({ type: 'oneshot', scheduleText: '30 13 1 11 *', targetKind: 'channel', targetTitle: 'weixin' }),
    ).toBe('渠道 · weixin');
    expect(
      cronPlanText({ type: 'oneshot', scheduleText: '30 13 1 11 *', targetKind: 'pending', targetTitle: '' }),
    ).toBe('专属会话（待创建）');
  });

  it('reads a missing type as oneshot, the way the scheduler defaults it', () => {
    expect(cronPlanText({ type: '', scheduleText: '30 13 1 11 *', targetKind: 'session', targetTitle: '上线清单' })).toBe(
      '上线清单',
    );
    // 反向：知道是周期性的任务照旧给计划，别为了干净把有用的话删了
    expect(cronPlanText({ type: 'recurring', scheduleText: '每天 04:00', targetKind: 'session', targetTitle: '日志' })).toBe(
      '每天 04:00 · 日志',
    );
  });
});

describe('cronWhenText', () => {
  it('tells an oneshot job the exact moment it will run, not “下次”', () => {
    expect(cronWhenText({ type: 'oneshot', running: false, nextRun: '2026-11-01T13:30:00+08:00' }, now)).toBe(
      '11月1日 13:30 跑一次',
    );
    expect(cronWhenText({ type: 'oneshot', running: false, nextRun: '2026-10-31T23:10:00+08:00' }, now)).toBe(
      '今天 23:10 跑一次',
    );
  });

  // 暂停的一次性任务没有下次运行（暂停 = 从调度器里摘掉）。说「不会再跑」是错的：恢复就会跑
  it('does not claim a paused oneshot will never run', () => {
    expect(cronWhenText({ type: 'oneshot', running: false, nextRun: '' }, now)).toBe('只跑一次');
  });

  it('leaves the repeating job on “下次”', () => {
    expect(cronWhenText({ type: 'recurring', running: false, nextRun: '2026-11-01T15:30:00+08:00' }, now)).toBe(
      '下次 11-01 15:30',
    );
    expect(cronWhenText({ type: 'recurring', running: false, nextRun: '' }, now)).toBe('不会自动跑');
  });

  it('says it is running above everything else', () => {
    expect(cronWhenText({ type: 'oneshot', running: true, nextRun: '2026-11-01T13:30:00+08:00' }, now)).toBe('正在跑');
    expect(cronWhenText({ type: 'recurring', running: true, nextRun: '2026-11-01T15:30:00+08:00' }, now)).toBe('正在跑');
  });
});
