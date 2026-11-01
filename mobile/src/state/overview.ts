import type { SessionCache } from '../api/cache';
import { defaultLabel } from '../api/label';
import type { Device } from '../api/storage';
import { deviceKey } from '../api/storage';
import type { SessionSummary } from '../api/types';

/**
 * 首页那一列里的一行：一段对话 + 它属于哪台电脑。
 *
 * 分机器之后「这一段在哪台上」和标题一样重要——同一份列表里混着几台机器，不知道哪条在
 * 哪台，就没法判断点进去会发生什么。`device` 是设备簿里的下标，只在当前这一份列表里有效，
 * 所以每次渲染重算，不存进任何长期状态。
 */
export interface OverviewRow {
  session: SessionSummary;
  /** 那台电脑的稳定键（见 storage.ts 的 deviceKey） */
  key: string;
  /** devices 里的下标（删除一台之后会挪，所以别拿它当身份） */
  device: number;
  /** 给人看的机器名 */
  machine: string;
  /** 这一行来自缓存：那台这一轮没读到，状态可能是几小时前的 */
  stale: boolean;
}

export type Section = 'waiting' | 'running' | 'rest';

/** 分组次序就是这一列的阅读次序：先处理等你的，再看在跑的，最后才是历史 */
export const SECTION_ORDER: Section[] = ['waiting', 'running', 'rest'];

export function sectionOf(s: SessionSummary): Section {
  if (s.waiting) return 'waiting';
  if (s.running) return 'running';
  return 'rest';
}

function updatedAt(s: SessionSummary): number {
  const ms = Date.parse(s.updatedAt);
  return Number.isNaN(ms) ? 0 : ms;
}

/**
 * 把所有电脑的会话拼成一列。
 *
 * 排序按三段（等你的 > 在跑的 > 其余）——**等你的不看时间**：三天前的一段在等你回答，也要
 * 浮到最上面，否则「等我在动作」这件事就白做了。同一段里置顶的在前，再按最近更新。
 *
 * `reachable` 是「这一轮读到了没有」：只有明确失败的才标成陈旧。还没刷新的（键不存在）
 * 不标——启动时列表会先画缓存再刷新，先给每台都打上「连不上」会闪一下。
 */
export function buildOverview(
  devices: Device[],
  cache: SessionCache,
  reachable: Record<string, boolean>,
): OverviewRow[] {
  const rows: OverviewRow[] = [];
  devices.forEach((d, index) => {
    const key = deviceKey(d);
    const entry = cache[key];
    if (!entry || entry.sessions.length === 0) return;
    const stale = reachable[key] === false;
    // 用户在这台手机上改过的名字优先；没改过就用机器自报的名字（或者地址）
    const machine = d.label || defaultLabel(d);
    for (const session of entry.sessions) {
      rows.push({ session, key, device: index, machine, stale });
    }
  });
  return rows.sort(compare);
}

function compare(a: OverviewRow, b: OverviewRow): number {
  const sa = SECTION_ORDER.indexOf(sectionOf(a.session));
  const sb = SECTION_ORDER.indexOf(sectionOf(b.session));
  if (sa !== sb) return sa - sb;
  // 同一段里，新鲜的在前面 —— 一台连不上的机器上「等你授权」可能是几小时前的，那时人早
  // 处理完了。它仍然留在「等待」组里（chip 上的数字也没丢），但不该盖住真正新鲜的等待。
  if (a.stale !== b.stale) return a.stale ? 1 : -1;
  if (Boolean(a.session.pinned) !== Boolean(b.session.pinned)) return a.session.pinned ? -1 : 1;
  const ta = updatedAt(a.session);
  const tb = updatedAt(b.session);
  if (ta !== tb) return tb - ta;
  // 时间撞在一起时按机器与会话 id 排：否则每次刷新都可能换一下前后，行会跳
  return a.key.localeCompare(b.key) || a.session.id.localeCompare(b.session.id);
}

/** 每台电脑有几段在等你 —— 顶上那排 chip 上的小圆点和数字用它 */
export function waitingCount(cache: SessionCache, key: string): number {
  const entry = cache[key];
  if (!entry) return 0;
  let n = 0;
  for (const s of entry.sessions) if (s.waiting) n++;
  return n;
}
