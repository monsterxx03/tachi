import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { Alert, Pressable, ScrollView, StyleSheet, Text, TextInput, View } from 'react-native';
import Animated from 'react-native-reanimated';
import { defaultLabel, hostOf } from '../api/label';
import type { Device } from '../api/storage';
import { deviceKey } from '../api/storage';
import type { AwakeState } from '../api/types';
import { useTheme } from '../theme/ThemeContext';
import { machineColor } from './machineColor';
import { Switch } from './controls';
import { PairingQR } from './PairingQR';
import { EdgeBackZone, useEdgeBack } from './edgeBack';

/** 菜单/卡片上的时长选项。0 = 不自动关 —— 与桌面托盘里那组一致。 */
const AWAKE_DURATIONS = [30, 60, 180, 360, 480, 0];

/**
 * 一台电脑的每一条地址，当前在用的那条排第一。
 *
 * 旧数据（或者只连过一条地址的电脑）没有 urls，退回 baseUrl —— 所以这里永远至少有一样东西，
 * 而「等 N 条地址」那种缩写不再出现：读者要能看到完整的 URL 才能自己抄去手输。
 */
function addressLines(d: Device): string[] {
  const all = d.urls && d.urls.length > 0 ? d.urls : [d.baseUrl];
  return [d.baseUrl, ...all.filter((u) => u !== d.baseUrl)];
}

/** 这条地址是哪一类网络：读者要能一眼判断「出门之后它还通不通」 */
function kindOf(url: string): string {
  const host = hostOf(url).split(':')[0];
  if (/^100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\./.test(host)) return 'Tailscale';
  if (/^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(host)) return '局域网';
  if (host === 'localhost' || host.startsWith('127.')) return '本机';
  return '公网';
}

/** 分钟的读法：和桌面菜单里那行「唤醒时长」用同一套说法（两边看的是同一个值）。 */
function awakeDurationText(minutes: number): string {
  if (minutes === 0) return '不自动关';
  if (minutes % 60 === 0) return `${minutes / 60} 小时`;
  if (minutes > 60) return `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分`;
  return `${minutes} 分钟`;
}

/** 「到 18:20 自动关」里的那个时刻，按这台手机自己的时区。 */
function awakeUntilText(until: string): string {
  const t = new Date(until);
  if (Number.isNaN(t.getTime())) return until;
  return `${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`;
}

/**
 * 一台电脑自己的那一页。
 *
 * 这里放的全是「这一台」的事：它在哪、现在通不通、叫什么、别睡、以及把它从这台手机上删掉。
 * 分机器之前这些和别的电脑挤在同一屏里（平铺），现在一屏只回答一台 —— 地址不用再藏进
 * 「用哪条地址」那个展开里，一列全摊开。
 */
export function DeviceDetail({
  device,
  online,
  conn,
  syncedAt,
  onBack,
  onRemove,
  onRename,
  onSelectAddress,
  awake,
  onLoadAwake,
  onSetAwake,
}: {
  device: Device;
  /** 这一轮读到了没有（false 才是真的连不上） */
  online: boolean;
  /** 正在连的那一台才有 WebSocket 状态；别的那几台这里一定是空 */
  conn: string;
  /** 这台最后一次读到列表的时刻（毫秒，0 = 从没读到） */
  syncedAt: number;
  onBack: () => void;
  onRemove: () => void;
  /** 改这一台在手机上的显示名；只写本地，不通知那台机器 */
  onRename: (label: string) => void;
  /** 手动指定这台电脑走哪条地址；null = 交回自动 */
  onSelectAddress: (url: string | null) => void;
  /** 这一台的「保持唤醒」；null = 还没读到（那台电脑是旧版本，或者请求还没回来） */
  awake: AwakeState | null;
  onLoadAwake: () => void;
  /** 开关 / 改时长。返回失败的那句话，成功返回 null。 */
  onSetAwake: (on: boolean, durationMin?: number) => Promise<string | null>;
}) {
  const { tokens: t, isDark } = useTheme();
  const back = useEdgeBack(onBack);
  const [editingName, setEditingName] = useState(false);
  const [draft, setDraft] = useState(device.label);
  const [showingQR, setShowingQR] = useState(false);
  const [awakeCustom, setAwakeCustom] = useState('');
  const [awakeErr, setAwakeErr] = useState<string | null>(null);
  const [awakeBusy, setAwakeBusy] = useState(false);

  // 进这一页读一次：开关是一台机器的值，可能刚在桌面的托盘上被改过。
  useEffect(() => {
    onLoadAwake();
  }, [onLoadAwake]);

  // 服务端回来的时长就是输入框的初值（「不自动关」= 0 显示成空，让「什么都没填」和它一致）。
  useEffect(() => {
    if (awake) setAwakeCustom(awake.durationMin > 0 ? String(awake.durationMin) : '');
  }, [awake]);

  async function applyAwake(on: boolean, minutes?: number) {
    setAwakeBusy(true);
    setAwakeErr(await onSetAwake(on, minutes));
    setAwakeBusy(false);
  }

  async function applyCustomDuration() {
    const n = Number(awakeCustom.trim());
    if (!Number.isFinite(n) || n === 0) {
      // 空框 = 不自动关：那是时长的一种，不是错误。
      await applyAwake(awake?.on ?? false, 0);
      return;
    }
    await applyAwake(awake?.on ?? false, Math.round(n));
  }

  // 清空输入 = 回到默认名（那台机器自报的名字，或者地址）。这条规则让「忘了原来叫什么」
  // 有出路，也免得一个空名字在列表里留下一条看不见的条目。
  function commitName() {
    const wanted = draft.trim();
    onRename(wanted || defaultLabel(device));
    setEditingName(false);
  }

  const urls = addressLines(device);
  const auto = !device.pinned;
  // 「已连接」只对正在连的那一台有意义；别的机器在线就是「在线」，它此刻没有 socket 连着
  const status = !online
    ? syncedAt > 0
      ? `连不上 · ${lastSeen(syncedAt)}看过`
      : '连不上'
    : conn === 'open'
      ? '已连接'
      : '在线';

  return (
    <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onBack} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <View style={styles.headInfo}>
          <View style={{ flexDirection: 'row', alignItems: 'center', gap: 7 }}>
            {/* 和首页那一行同一个颜色：这台机器在哪儿都是这个色 */}
            <View style={{ width: 8, height: 8, borderRadius: 4, backgroundColor: machineColor(deviceKey(device), isDark) }} />
            <Text style={[styles.title, { color: t.title, flexShrink: 1 }]} numberOfLines={1}>
              {device.label}
            </Text>
          </View>
          <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }} numberOfLines={1}>
            {status}
          </Text>
        </View>
      </View>

      <ScrollView contentContainerStyle={styles.body} keyboardShouldPersistTaps="handled">
        <Text style={[styles.section, { color: t.faint }]}>连接</Text>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          {/* 自动：交给它自己比。下面那几条是手动锁 —— 读者明确说过「就走这条」之后，
              自动切换不再动它（见 addresses.ts 的 pinned）。 */}
          <Pressable
            onPress={() => onSelectAddress(null)}
            style={[styles.addrRow, { borderBottomColor: t.border }]}
          >
            <View style={{ flex: 1, minWidth: 0 }}>
              <Text style={{ color: auto ? t.accent2 : t.text, fontSize: 13.5, fontWeight: auto ? '600' : '400' }}>
                自动
              </Text>
              <Text style={{ color: t.faint, fontSize: 11, marginTop: 3 }}>
                哪个通就用哪个，换网络也不用管
              </Text>
            </View>
            {auto ? <Feather name="check" size={15} color={t.accent2} /> : null}
          </Pressable>
          {urls.map((url, i) => {
            const on = device.pinned && url === device.baseUrl;
            return (
              <Pressable
                key={url}
                onPress={() => onSelectAddress(on ? null : url)}
                style={[
                  styles.addrRow,
                  { borderBottomColor: t.border },
                  i === urls.length - 1 && { borderBottomWidth: 0 },
                ]}
              >
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Text
                    style={{
                      color: on ? t.accent2 : t.text,
                      fontSize: 13,
                      fontFamily: 'monospace',
                      fontWeight: on ? '600' : '400',
                    }}
                    numberOfLines={1}
                  >
                    {hostOf(url)}
                  </Text>
                  <Text style={{ color: t.faint, fontSize: 11, marginTop: 3 }}>{kindOf(url)}</Text>
                </View>
                {on ? <Feather name="check" size={15} color={t.accent2} /> : null}
              </Pressable>
            );
          })}
          <Text style={{ color: t.faint, fontSize: 11, lineHeight: 17, paddingVertical: 9 }}>
            {device.pinned
              ? '现在锁在一条地址上，它不会被自动换掉。选「自动」交回给它自己判断。'
              : urls.length > 1
                ? '两条都通时用局域网；出门它会自己换到 Tailscale。'
                : '只有这一条地址。换了网络连不上时，回来这里核对一下。'}
          </Text>
        </View>

        <Pressable
          onPress={() => setShowingQR(true)}
          style={({ pressed }) => [
            styles.linkRow,
            { borderColor: t.border, backgroundColor: pressed ? t.wash : t.panel },
          ]}
        >
          <Feather name="grid" size={16} color={t.accent2} />
          <Text style={{ color: t.text, fontSize: 13.5, flex: 1 }}>出示配对码</Text>
          <Feather name="chevron-right" size={16} color={t.faint} />
        </Pressable>

        <Text style={[styles.section, { color: t.faint }]}>这台电脑</Text>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={{ flexDirection: 'row', alignItems: 'center', gap: 10 }}>
            <View style={{ flex: 1, minWidth: 0 }}>
              <Text style={{ color: t.text, fontSize: 13.5 }}>保持唤醒</Text>
              <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 3, lineHeight: 17 }}>
                不让这台电脑因为闲置而睡。屏幕照常会黑，合上盖子也还是会睡。
              </Text>
            </View>
            {/* 关掉的时候开关就是关的：这个值属于那台机器，不在这里猜 */}
            <Switch
              on={awake?.on ?? false}
              disabled={awake == null || awakeBusy || !online}
              onPress={() => void applyAwake(!(awake?.on ?? false))}
              t={t}
            />
          </View>

          {awake == null ? (
            <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 10 }}>
              {online ? '这台电脑还没有这个开关，桌面端可能是旧版本' : '这台电脑现在连不上，读不到它的开关'}
            </Text>
          ) : (
            <>
              <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 14 }}>时长</Text>
              <View style={[styles.seg, { flexWrap: 'wrap', marginTop: 8 }]}>
                {AWAKE_DURATIONS.map((m) => {
                  const on = (awake.durationMin || 0) === m;
                  return (
                    <Pressable
                      key={m}
                      disabled={awakeBusy}
                      onPress={() => void applyAwake(awake.on, m)}
                      style={[
                        styles.segItem,
                        on && { backgroundColor: t.accentSoft, borderColor: t.accentLine },
                        { borderColor: on ? t.accentLine : t.border },
                      ]}
                    >
                      <Text style={{ color: on ? t.accent2 : t.dim, fontSize: 12.5 }}>
                        {awakeDurationText(m)}
                      </Text>
                    </Pressable>
                  );
                })}
              </View>
              <View style={{ flexDirection: 'row', alignItems: 'center', gap: 8, marginTop: 10 }}>
                <Text style={{ color: t.faint, fontSize: 11.5 }}>自定义</Text>
                <TextInput
                  value={awakeCustom}
                  onChangeText={setAwakeCustom}
                  keyboardType="number-pad"
                  returnKeyType="done"
                  onSubmitEditing={() => void applyCustomDuration()}
                  placeholder="分钟，空 = 不自动关"
                  placeholderTextColor={t.faint}
                  style={[styles.nameInput, { width: 150, color: t.text, borderColor: t.borderStrong, backgroundColor: t.bg }]}
                />
                <Pressable
                  disabled={awakeBusy}
                  onPress={() => void applyCustomDuration()}
                  style={({ pressed }) => [
                    styles.segItem,
                    { borderColor: t.accentLine, backgroundColor: pressed ? t.wash : t.accentSoft },
                  ]}
                >
                  <Text style={{ color: t.accent2, fontSize: 12.5 }}>确定</Text>
                </Pressable>
              </View>
              <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 10, lineHeight: 17 }}>
                {awake.on
                  ? awake.until
                    ? `现在开着，${awakeUntilText(awake.until)} 自动关`
                    : '现在开着，不自动关'
                  : '现在关着：电脑按它自己的设置到点就睡'}
              </Text>
              {awakeErr ? <Text style={{ color: t.red, fontSize: 11.5, marginTop: 6 }}>{awakeErr}</Text> : null}
            </>
          )}
        </View>

        <Text style={[styles.section, { color: t.faint }]}>名字</Text>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          {editingName ? (
            <TextInput
              value={draft}
              onChangeText={setDraft}
              autoFocus
              autoCorrect={false}
              autoCapitalize="none"
              returnKeyType="done"
              onSubmitEditing={commitName}
              onBlur={commitName}
              placeholder={defaultLabel(device)}
              placeholderTextColor={t.faint}
              style={[styles.nameInput, { color: t.text, borderColor: t.borderStrong, backgroundColor: t.bg }]}
            />
          ) : (
            <Pressable
              onPress={() => {
                setDraft(device.label);
                setEditingName(true);
              }}
              style={{ flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 4 }}
            >
              <Text style={{ color: t.text, fontSize: 13.5, flex: 1 }} numberOfLines={1}>
                {device.label}
              </Text>
              <Feather name="edit-2" size={14} color={t.faint} />
            </Pressable>
          )}
          <Text style={{ color: t.faint, fontSize: 11, marginTop: 8, lineHeight: 17 }}>
            只存在这台手机上。清空会回到它自报的名字（{defaultLabel(device)}）。
          </Text>
        </View>

        <Pressable
          onPress={() =>
            Alert.alert('删除这台电脑？', `${device.label}\n\n只是这台手机不再连它，电脑本身不受影响。`, [
              { text: '取消', style: 'cancel' },
              { text: '删除', style: 'destructive', onPress: onRemove },
            ])
          }
          style={({ pressed }) => [styles.danger, { borderColor: t.redLine, backgroundColor: pressed ? t.redWash : 'transparent' }]}
        >
          <Text style={{ color: t.red, fontSize: 13.5, fontWeight: '600' }}>删除这台电脑</Text>
        </Pressable>
      </ScrollView>

      <PairingQR
        pairing={showingQR ? device : null}
        onClose={() => setShowingQR(false)}
      />
      <EdgeBackZone gesture={back.gesture} />
    </Animated.View>
  );
}

/** 「4 小时前」——这台最后一次读到列表的时间，用来说「这些行可能多旧」 */
function lastSeen(at: number): string {
  const mins = Math.max(0, Math.round((Date.now() - at) / 60000));
  if (mins < 1) return '刚刚';
  if (mins < 60) return `${mins} 分钟前`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours} 小时前`;
  return `${Math.round(hours / 24)} 天前`;
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 6,
    height: 58,
    paddingHorizontal: 8,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  back: { width: 34, height: 34, borderRadius: 10, alignItems: 'center', justifyContent: 'center' },
  headInfo: { flex: 1, minWidth: 0, paddingHorizontal: 4 },
  title: { fontSize: 15, fontWeight: '600' },
  body: { padding: 12, paddingBottom: 24 },
  section: { fontSize: 11.5, fontWeight: '600', paddingHorizontal: 6, paddingTop: 16, paddingBottom: 8, letterSpacing: 0.4 },
  card: { borderWidth: 1, borderRadius: 12, paddingHorizontal: 12, paddingVertical: 2 },
  addrRow: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 11, borderBottomWidth: StyleSheet.hairlineWidth },
  linkRow: { flexDirection: 'row', alignItems: 'center', gap: 10, borderWidth: 1, borderRadius: 12, paddingHorizontal: 13, paddingVertical: 14, marginTop: 10 },
  seg: { flexDirection: 'row', gap: 7 },
  segItem: { height: 30, borderWidth: 1, borderRadius: 15, paddingHorizontal: 11, alignItems: 'center', justifyContent: 'center' },
  nameInput: { borderWidth: 1, borderRadius: 10, paddingHorizontal: 12, paddingVertical: 9, fontSize: 13.5, marginTop: 8 },
  danger: {
    marginTop: 22,
    height: 44,
    borderWidth: 1,
    borderRadius: 12,
    alignItems: 'center',
    justifyContent: 'center',
  },
});
