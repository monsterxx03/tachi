/**
 * 推送通知点开来的是哪个会话、在哪台电脑上。
 *
 * 链接形状（桌面侧 `pkg/push` 的 DeepLink 拼的就是它）：
 *
 *   tachi://session/<会话 id>?device=<机器 id>
 *
 * 为什么要带机器 id：手机可以记住好几台电脑，而会话 id 只在一台机器上有意义。拿同一个
 * id 去另一台机器上打开，轻则「这段对话已经不在了」，重则打开一段同 id 的、不相干的对话。
 * 机器 id 是那台电脑自己生成的（`machine.json`），配对时手机就存下了，改名字、换网络都不影响。
 *
 * 解析失败返回 null：一条点不开的通知不该让 App 停在半路。
 */
export interface DeepLink {
  sessionId: string;
  /** 老推送没有这一项时是空串：那就只在当前这台电脑上找 */
  deviceId: string;
}

const SHAPE = /^tachi:\/\/session\/([^/?#]+)(?:\?([^#]*))?/i;

export function parseDeepLink(url: string): DeepLink | null {
  const m = SHAPE.exec(url.trim());
  if (!m) return null;
  const sessionId = safeDecode(m[1]);
  if (!sessionId) return null;
  const deviceId = safeDecode(/(?:^|&)device=([^&]*)/.exec(m[2] ?? '')?.[1] ?? '');
  return { sessionId, deviceId };
}

function safeDecode(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}
