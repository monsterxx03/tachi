import type { CronPanelRow } from '../api/types';

/**
 * 一条定时任务「跑在哪儿」的那半句话。
 *
 * `targetTitle` 只有**已经存在**的会话才有值，所以「取不到标题」其实是两种完全不同的事：
 *
 * - `pending`：专属会话还没创建（第一次运行才建），一切都正常，只是还没到那一步；
 * - `missing`：它曾经有、现在没了，任务确实需要人管。
 *
 * 把两者混成一句「会话不在了」，就是让人以为自己的任务坏了去删它 —— 而这正是它被写成
 * 一个函数、而不是行内一个 `||` 的原因。四种值见 mobile-protocol.md 的 panels 那条。
 */
export function cronTargetText(row: Pick<CronPanelRow, 'targetKind' | 'targetTitle'>): string {
  switch (row.targetKind) {
    case 'pending':
      return '专属会话（待创建）';
    case 'missing':
      return '会话已删除';
    case 'channel':
      return row.targetTitle ? `渠道 · ${row.targetTitle}` : '渠道';
    default:
      return row.targetTitle || '会话';
  }
}
