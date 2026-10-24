/**
 * 一台电脑在手机上叫什么名字。
 *
 * 这里刻意只有纯函数：名字怎么取是产品规则（机器自报的优先、老服务端怎么兜底），存到哪是
 * storage.ts 的事。分开之后规则可以单独测，不用把钥匙串拖进来。
 */

/**
 * 配对时给那一台起个名字：**机器自报的名字（hostname）优先**。
 *
 * 没有 hostname 的老服务端退回原来的拼法（产品名 + 主机）——产品名每台安装都一样，所以那个
 * 拼法只能靠地址区分；hostname 有值时才真的认得出「这是家里那台 NAS」。
 */
export function deviceLabel(r: { hostname?: string; name?: string }, baseUrl: string): string {
  const host = hostOf(baseUrl);
  const machine = cleanHostname(r.hostname);
  if (machine) return machine;
  return r.name && r.name !== host ? `${r.name} · ${host}` : host;
}

/** 用户清空输入时回到的名字（机器自报的，或者地址） */
export function defaultLabel(d: { hostname?: string; baseUrl: string }): string {
  return deviceLabel(d, d.baseUrl);
}

/** macOS 的 hostname 带 ".local"；手机列表里不需要那一截 */
function cleanHostname(hostname?: string): string {
  return (hostname ?? '').trim().replace(/\.local$/i, '');
}

/** 地址去掉协议与末尾斜杠，剩下的就是「哪台机器」 */
export function hostOf(baseUrl: string): string {
  return baseUrl.replace(/^https?:\/\//, '').replace(/\/+$/, '');
}
