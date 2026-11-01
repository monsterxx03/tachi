/**
 * 一台电脑的「身份色」。
 *
 * 颜色只由这台机器的稳定键决定（`deviceKey` = deviceId，老配对用令牌），所以同一台机器在
 * 你的 iPhone、iPad、别人的手机上都是同一个颜色 —— 颜色不是排版，是身份。
 *
 * 为什么是色相轮换而不是一份手挑的调色板：手挑的十几个颜色在五台机器里撞色的概率过半
 * （生日问题），撞了之后除了看名字没有别的办法。色相连续分布，机数再翻一倍也不会撞。
 */
const HUES = 24;

/** 32 位 FNV-1a：够短、分布均匀、不需要依赖 */
export function machineHue(key: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < key.length; i++) {
    h ^= key.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return (h % HUES) * (360 / HUES);
}

/**
 * 这台机器在某个主题下的颜色。
 *
 * 深色主题用更高的亮度、浅色主题用更低的亮度 —— 两边都保持能当正文读（不是只当装饰）。
 * 饱和度压在中段：再高会像警告色，再低和灰色分不开。
 */
export function machineColor(key: string, dark: boolean): string {
  return `hsl(${machineHue(key)}, ${dark ? 55 : 48}%, ${dark ? 70 : 38}%)`;
}
