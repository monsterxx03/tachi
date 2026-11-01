import type { CronPanelRow } from '../api/types';
import { cronTargetText } from './cronTarget';
import { formatNextRun, formatRunAt } from './time';

/**
 * 一条定时任务「按什么计划跑」的那半句话。
 *
 * 周期性任务照旧给表达式（「每个工作日 15:30」）；一次性任务**不给** —— 它那句
 * `30 13 1 11 *` 是「11 月 1 日 13:30 跑一次」在 cron 语法里的写法，把写法摊给读者看只会
 * 让人以为这任务每年都要跑一遍。哪一刻跑，由下一行讲（见 cronWhenText）。
 */
export function cronPlanText(row: Pick<CronPanelRow, 'type' | 'scheduleText' | 'targetKind' | 'targetTitle'>): string {
  const schedule = isOneshot(row) ? '' : row.scheduleText;
  return [schedule, cronTargetText(row)].filter(Boolean).join(' · ');
}

/**
 * 一条定时任务「什么时候跑」的那半句话。
 *
 * 一次性任务说具体时刻（「今天 13:30 跑一次」），因为读者要判断的是那一刻自己是否在场；
 * 周期性任务说「下次」，因为它真正的问题是「接下来还会不会自动跑」。
 */
export function cronWhenText(row: Pick<CronPanelRow, 'type' | 'running' | 'nextRun'>, now: Date = new Date()): string {
  if (row.running) return '正在跑';
  if (isOneshot(row)) {
    // 暂停的一次性任务没有下次运行（暂停 = 从调度器里摘掉），但「不会再跑」是错的：恢复就会跑
    return row.nextRun ? `${formatRunAt(row.nextRun, now)} 跑一次` : '只跑一次';
  }
  return row.nextRun ? `下次 ${formatNextRun(row.nextRun)}` : '不会自动跑';
}

/**
 * 空 type 当 oneshot 读：调度器创建任务时默认就是 oneshot（`Scheduler.Create`），所以没有这个
 * 字段的老任务也是一次性的。Go 侧把这条规则折成了 mobileJobType，两边是一条规矩。
 */
function isOneshot(row: Pick<CronPanelRow, 'type'>): boolean {
  return row.type === '' || row.type === 'oneshot';
}
