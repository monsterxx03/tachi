/**
 * 系统提醒（`<system-reminder>…</system-reminder>`）不进手机的消息流。
 *
 * 它跟着某一轮被注入，桌面会把它折成正文边上一个小标记；手机上没有人会点开它，
 * 原样显示只会把真正的回复挤走。抽出来丢掉，只留正文。
 */
export function stripReminders(content: string): string {
  return content.replace(/<system-reminder>[\s\S]*?<\/system-reminder>/g, '').trim();
}
