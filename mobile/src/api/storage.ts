import { Platform } from 'react-native';
import * as SecureStore from 'expo-secure-store';
import type { Pairing } from './client';

/**
 * 配对信息（地址 + 令牌）存在哪。
 *
 * 手机上是系统的钥匙串；跑在 web 目标上时退化成 localStorage —— 这层适配是
 * 「界面要能跑在浏览器里」的代价，也是它成立的前提（agent 靠那条路自己看界面）。
 */
const KEY = 'tachi.pairing';

export async function loadPairing(): Promise<Pairing | null> {
  try {
    const raw = Platform.OS === 'web' ? webGet() : await SecureStore.getItemAsync(KEY);
    if (!raw) return null;
    const p = JSON.parse(raw) as Pairing;
    return p.baseUrl && p.token ? p : null;
  } catch {
    return null;
  }
}

export async function savePairing(p: Pairing | null): Promise<void> {
  const raw = p ? JSON.stringify(p) : '';
  if (Platform.OS === 'web') {
    webSet(raw);
    return;
  }
  if (p) await SecureStore.setItemAsync(KEY, raw);
  else await SecureStore.deleteItemAsync(KEY);
}

function webGet(): string | null {
  try {
    return typeof localStorage === 'undefined' ? null : localStorage.getItem(KEY);
  } catch {
    return null;
  }
}

function webSet(raw: string): void {
  try {
    if (typeof localStorage === 'undefined') return;
    if (raw) localStorage.setItem(KEY, raw);
    else localStorage.removeItem(KEY);
  } catch {
    // 隐私模式下没有 localStorage，那就当没配对
  }
}
