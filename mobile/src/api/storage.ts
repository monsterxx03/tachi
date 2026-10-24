import { Platform } from 'react-native';
import * as SecureStore from 'expo-secure-store';
import type { Pairing } from './client';
import { hostOf } from './label';

/**
 * 配对信息（地址 + 令牌）存在哪。
 *
 * 一台手机可以同时记住几台电脑（家里 / 公司各一台），但**同时只连一台**：桌面进程是会话的
 * 宿主，两台各有一份会话，混着显示没有意义。所以这里是「列表 + 一个当前项」。
 *
 * 手机上是系统的钥匙串；跑在 web 目标上时退化成 localStorage —— 这层适配是
 * 「界面要能跑在浏览器里」的代价，也是它成立的前提（agent 靠那条路自己看界面）。
 */
const KEY = 'tachi.devices';
/** 单台时代的键：读到就迁进列表，然后删掉（否则每次启动都会多出一条） */
const LEGACY_KEY = 'tachi.pairing';

export interface Device extends Pairing {
  /** 给人看的名字：配对时取那台机器自报的名字，之后可以在设置页改（只存在这台手机上） */
  label: string;
  /**
   * /v1/ping 报上来的机器名，原名留着。
   *
   * 改名之后仍然要它：清空输入时回到的默认值就是它，排查「这条是哪台机器」也靠它。老服务端
   * 没有这个字段（协议后加的），所以是可选的。
   */
  hostname?: string;
}

export interface DeviceBook {
  devices: Device[];
  /** 当前那一台在 devices 里的下标；列表空时无意义 */
  active: number;
}

const EMPTY: DeviceBook = { devices: [], active: 0 };

export async function loadDevices(): Promise<DeviceBook> {
  try {
    const raw = await readRaw(KEY);
    if (raw) {
      const book = JSON.parse(raw) as DeviceBook;
      const devices = (book.devices ?? []).filter((d) => d && d.baseUrl && d.token);
      const active = Math.min(Math.max(book.active ?? 0, 0), Math.max(devices.length - 1, 0));
      if (devices.length > 0) return { devices, active };
    }
    const legacy = await readRaw(LEGACY_KEY);
    if (!legacy) return EMPTY;
    const p = JSON.parse(legacy) as Pairing;
    if (!p?.baseUrl || !p?.token) return EMPTY;
    const migrated: DeviceBook = { devices: [{ ...p, label: hostOf(p.baseUrl) }], active: 0 };
    // 迁移写失败也不要紧：下次启动会再迁一遍，结果一样
    await saveDevices(migrated.devices, migrated.active);
    await removeRaw(LEGACY_KEY);
    return migrated;
  } catch {
    return EMPTY;
  }
}

export async function saveDevices(devices: Device[], active: number): Promise<void> {
  if (devices.length === 0) {
    await removeRaw(KEY);
    return;
  }
  await writeRaw(KEY, JSON.stringify({ devices, active }));
}

async function readRaw(key: string): Promise<string | null> {
  if (Platform.OS === 'web') return webGet(key);
  try {
    return await SecureStore.getItemAsync(key);
  } catch {
    return null;
  }
}

async function writeRaw(key: string, raw: string): Promise<void> {
  if (Platform.OS === 'web') {
    webSet(key, raw);
    return;
  }
  await SecureStore.setItemAsync(key, raw);
}

async function removeRaw(key: string): Promise<void> {
  if (Platform.OS === 'web') {
    webSet(key, '');
    return;
  }
  try {
    await SecureStore.deleteItemAsync(key);
  } catch {
    // 已经没有了
  }
}

function webGet(key: string): string | null {
  try {
    return typeof localStorage === 'undefined' ? null : localStorage.getItem(key);
  } catch {
    return null;
  }
}

function webSet(key: string, raw: string): void {
  try {
    if (typeof localStorage === 'undefined') return;
    if (raw) localStorage.setItem(key, raw);
    else localStorage.removeItem(key);
  } catch {
    // 隐私模式下没有 localStorage，那就当没配对
  }
}
