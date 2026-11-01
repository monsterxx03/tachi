import { File, Paths } from 'expo-file-system';
import { Platform } from 'react-native';
import { readWebStorage, writeWebStorage } from './storage';
import type { SessionSummary } from './types';

/**
 * 每台电脑最近一次读到的会话列表。
 *
 * 它回答的是「我上次看到它的时候，它上面还有哪些对话」——某台连不上时首页仍然显示这些行
 * （淡化，并标明是几小时前的），否则读者会以为那台机器上的对话没了。它只用于显示：任何
 * 写入（新建、改名、删除）都不从这里读，也不写回这里以外的任何地方。
 *
 * 存在手机本地而不是钥匙串：一份列表几十行，超出 SecureStore 在 Android 上的单值上限。
 */
export interface DeviceSessions {
  /** 最后一次成功读到的时刻（毫秒） */
  at: number;
  sessions: SessionSummary[];
}

/** 键是 `deviceKey`（见 storage.ts），不是设备簿里的下标 */
export type SessionCache = Record<string, DeviceSessions>;

const FILE = 'session-cache.json';
const WEB_KEY = 'tachi.sessionCache';
/** 只留最近这些行：它是「上次看到的列表」，不是一份记录，越攒越大没有意义 */
const MAX_ROWS = 120;

function trim(cache: SessionCache): SessionCache {
  const out: SessionCache = {};
  for (const [key, v] of Object.entries(cache)) {
    out[key] = { at: v.at, sessions: v.sessions.slice(0, MAX_ROWS) };
  }
  return out;
}

export async function loadSessionCache(): Promise<SessionCache> {
  try {
    const raw = await readRaw();
    if (!raw) return {};
    const parsed = JSON.parse(raw) as SessionCache;
    const out: SessionCache = {};
    for (const [key, v] of Object.entries(parsed ?? {})) {
      // 半截的记录（升级过、或者手动改坏了）跳过，不要让它把整份缓存带塌
      if (!v || typeof v.at !== 'number' || !Array.isArray(v.sessions)) continue;
      out[key] = { at: v.at, sessions: v.sessions };
    }
    return out;
  } catch {
    return {};
  }
}

/** 写失败不影响任何主流程：那只是少一份离线快照，下一轮刷新会再写一次 */
export async function saveSessionCache(cache: SessionCache): Promise<void> {
  try {
    await writeRaw(JSON.stringify(trim(cache)));
  } catch {
    // 磁盘满、权限、web 隐私模式 —— 都只是没有离线快照
  }
}

async function readRaw(): Promise<string | null> {
  if (Platform.OS === 'web') return readWebStorage(WEB_KEY);
  const file = new File(Paths.document, FILE);
  if (!file.exists) return null;
  return await file.text();
}

async function writeRaw(raw: string): Promise<void> {
  if (Platform.OS === 'web') {
    writeWebStorage(WEB_KEY, raw);
    return;
  }
  const file = new File(Paths.document, FILE);
  await file.write(raw);
}
