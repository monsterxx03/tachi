import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { Alert, Pressable, ScrollView, StyleSheet, Text, TextInput, View } from 'react-native';
import type { Pairing as PairingInfo } from '../api/client';
import type { Device } from '../api/storage';
import { recentRequests, type ReqRecord } from '../api/client';
import type { AwakeState } from '../api/types';
import { defaultLabel } from '../api/label';
import Animated from 'react-native-reanimated';
import { Switch } from './controls';
import { useTheme } from '../theme/ThemeContext';
import { PairingQR } from './PairingQR';
import { EdgeBackZone, useEdgeBack } from './edgeBack';
import type { ThemeMode } from '../theme/tokens';

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

const MODES: { value: ThemeMode; label: string }[] = [
  { value: 'system', label: '跟随系统' },
  { value: 'light', label: '白天' },
  { value: 'dark', label: '黑夜' },
];

export function Settings({
  pairing,
  devices,
  activeDevice,
  conn,
  onSwitchDevice,
  onRemoveDevice,
  onRenameDevice,
  onSelectAddress,
  onAddDevice,
  onUnpair,
  onBack,
  awake,
  onLoadAwake,
  onSetAwake,
}: {
  pairing: PairingInfo | null;
  devices: Device[];
  activeDevice: number;
  conn: string;
  onSwitchDevice: (index: number) => void;
  onRemoveDevice: (index: number) => void;
  /** 手动指定这台电脑走哪条地址；null = 交回自动 */
  onSelectAddress: (index: number, url: string | null) => void;
  /** 改这一台在手机上的显示名；只写本地，不通知那台机器 */
  onRenameDevice: (index: number, label: string) => void;
  onAddDevice: () => void;
  onUnpair: () => void;
  onBack: () => void;
  /** 这台电脑的「保持唤醒」开关；null = 还没读到（没配对，或那台电脑是旧版本） */
  awake: AwakeState | null;
  onLoadAwake: () => void;
  /** 开关 / 改时长。返回失败的那句话，成功返回 null。 */
  onSetAwake: (on: boolean, durationMin?: number) => Promise<string | null>;
}) {
  const { tokens: t, mode, setMode } = useTheme();
  const back = useEdgeBack(onBack);
  // 改名是行内编辑，不弹一个新页面：这一屏本来就短，改的是一个字段。
  const [editing, setEditing] = useState<number | null>(null);
  // 哪一台展开了「用哪条地址」的选择（null = 都收着）
  const [picking, setPicking] = useState<number | null>(null);
  // 正在出示配对码的那一台（换新手机时扫它，不必走到电脑跟前）。
  const [showingQR, setShowingQR] = useState<number | null>(null);
  // 诊断：最近几次请求的去向与结果。手机上没有控制台，这一行是唯一能读到的证据。
  const [reqs, setReqs] = useState<ReqRecord[]>([]);
  useEffect(() => {
    const t = setInterval(() => setReqs(recentRequests()), 1000);
    return () => clearInterval(t);
  }, []);
  const [draft, setDraft] = useState('');
  // 「保持唤醒」：自定义时长是一个输入框，输入到一半不是合法数字，所以单独存草稿。
  const [awakeCustom, setAwakeCustom] = useState('');
  const [awakeErr, setAwakeErr] = useState<string | null>(null);
  const [awakeBusy, setAwakeBusy] = useState(false);

  // 进这一屏读一次：开关是一台机器的值，可能刚刚在桌面的托盘上被改过。
  // 依赖是那个稳定的回调本身：写成 `() => onLoadAwake()` 之外的东西（比如每次渲染新建一个箭头），
  // 这一屏会在每次 setState 之后再读一次，转成一个自己喂自己的轮询。
  useEffect(() => {
    void onLoadAwake();
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
  function commit(index: number, d: Device) {
    const wanted = draft.trim();
    onRenameDevice(index, wanted || defaultLabel(d));
    setEditing(null);
  }

  return (
      <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onBack} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <Text style={[styles.title, { color: t.title }]}>设置</Text>
      </View>

      <ScrollView contentContainerStyle={styles.body}>
        <Text style={[styles.section, { color: t.faint }]}>电脑</Text>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          {devices.length === 0 ? (
            <Text style={{ color: t.faint, fontSize: 12.5 }}>还没配对过电脑</Text>
          ) : null}
          {devices.map((d, i) => {
            const on = i === activeDevice;
            const renaming = editing === i;
            return (
              <Pressable
                key={`${d.baseUrl}-${i}`}
                onPress={() => (renaming || on ? undefined : onSwitchDevice(i))}
                style={[styles.device, { borderColor: on ? t.accentLine : t.border, backgroundColor: on ? t.accentSoft : 'transparent' }]}
              >
                <View style={{ flex: 1, minWidth: 0 }}>
                  {renaming ? (
                    <TextInput
                      value={draft}
                      onChangeText={setDraft}
                      autoFocus
                      autoCorrect={false}
                      autoCapitalize="none"
                      returnKeyType="done"
                      onSubmitEditing={() => commit(i, d)}
                      placeholder={defaultLabel(d)}
                      placeholderTextColor={t.faint}
                      style={[styles.nameInput, { color: t.text, borderColor: t.borderStrong, backgroundColor: t.bg }]}
                    />
                  ) : (
                    <Text style={{ color: t.text, fontSize: 13.5 }} numberOfLines={1}>
                      {d.label || d.baseUrl}
                    </Text>
                  )}
                  {/* 每一条地址都列出来，完整的。上面那行是「这台电脑叫什么」，这里是「它在哪」——
                      缩成一条加个「等 N 条」，读者就没法自己判断该抄哪条去手输。 */}
                  {addressLines(d).map((line, k) => (
                    <Text
                      key={line}
                      style={{
                        color: k === 0 && on ? t.accent2 : t.faint,
                        fontSize: 11,
                        marginTop: k === 0 ? 2 : 1,
                        fontFamily: 'monospace',
                      }}
                      numberOfLines={1}
                    >
                      {line}
                    </Text>
                  ))}
                  {on ? (
                    <Text style={{ color: t.faint, fontSize: 11, marginTop: 1 }}>
                      {conn === 'open' ? '已连接' : conn === 'connecting' ? '正在连接…' : '连不上'}
                      {d.pinned ? ' · 手动选的地址' : ''}
                    </Text>
                  ) : null}

                  {/* 「点那一行地址」太隐蔽了（试过的人找不到），所以给它一个明说的入口。 */}
                  {!renaming && addressLines(d).length > 1 ? (
                    <Pressable
                      hitSlop={6}
                      onPress={() => setPicking(picking === i ? null : i)}
                      style={{ flexDirection: 'row', alignItems: 'center', gap: 4, marginTop: 6 }}
                    >
                      <Feather
                        name={picking === i ? 'chevron-up' : 'chevron-down'}
                        size={12}
                        color={t.accent2}
                      />
                      <Text style={{ color: t.accent2, fontSize: 11 }}>
                        {picking === i ? '收起' : d.pinned ? '换个地址' : '用哪条地址'}
                      </Text>
                    </Pressable>
                  ) : null}
                  {/* 「出示配对码」：换新手机时扫这一张，等于扫电脑上那一张。和上面那条一样是
                      一行小字，挂在同一列里 —— 右侧那两颗按钮（改名 / 删除）是对这一行的操作，
                      而这一条是对「另一台手机」的动作，混进去会读成同一类事。 */}
                  {!renaming ? (
                    <Pressable
                      hitSlop={6}
                      onPress={() => setShowingQR(i)}
                      style={{ flexDirection: 'row', alignItems: 'center', gap: 4, marginTop: 6 }}
                    >
                      <Feather name="grid" size={12} color={t.accent2} />
                      <Text style={{ color: t.accent2, fontSize: 11 }}>出示配对码</Text>
                    </Pressable>
                  ) : null}
                  {picking === i ? (
                    <View style={{ marginTop: 6, gap: 2 }}>
                      {[null, ...addressLines(d)].map((opt) => {
                        const checked = opt ? d.baseUrl === opt : !d.pinned;
                        return (
                          <Pressable
                            key={opt ?? 'auto'}
                            onPress={() => {
                              setPicking(null);
                              onSelectAddress(i, opt);
                            }}
                            style={{ flexDirection: 'row', alignItems: 'center', gap: 6, paddingVertical: 5 }}
                          >
                            <Feather
                              name={checked ? 'check-circle' : 'circle'}
                              size={13}
                              color={checked ? t.accent2 : t.borderStrong}
                            />
                            <Text style={{ color: checked ? t.text : t.faint, fontSize: 12 }}>
                              {opt ?? '自动（跟着网络走）'}
                            </Text>
                          </Pressable>
                        );
                      })}
                    </View>
                  ) : null}
                </View>
                {renaming ? (
                  <>
                    <Pressable hitSlop={8} onPress={() => commit(i, d)} style={styles.trash}>
                      <Feather name="check" size={15} color={t.accent2} />
                    </Pressable>
                    <Pressable hitSlop={8} onPress={() => setEditing(null)} style={styles.trash}>
                      <Feather name="x" size={15} color={t.faint} />
                    </Pressable>
                  </>
                ) : (
                  <>
                    {on ? <Feather name="check" size={14} color={t.accent2} /> : null}
                    <Pressable
                      hitSlop={8}
                      onPress={() => {
                        setDraft(d.label);
                        setEditing(i);
                      }}
                      style={styles.trash}
                    >
                      <Feather name="edit-2" size={14} color={t.faint} />
                    </Pressable>
                    <Pressable
                      hitSlop={8}
                      onPress={() =>
                        Alert.alert('删掉这台电脑？', d.label || d.baseUrl, [
                          { text: '取消', style: 'cancel' },
                          { text: '删掉', style: 'destructive', onPress: () => onRemoveDevice(i) },
                        ])
                      }
                      style={styles.trash}
                    >
                      <Feather name="x" size={15} color={t.faint} />
                    </Pressable>
                  </>
                )}
              </Pressable>
            );
          })}
          <Pressable
            onPress={onAddDevice}
            style={({ pressed }) => [
              styles.addDevice,
              { borderColor: t.borderStrong, backgroundColor: pressed ? t.wash : 'transparent' },
            ]}
          >
            <Feather name="plus" size={15} color={t.accent2} />
            <Text style={{ color: t.accent2, fontSize: 13 }}>添加电脑</Text>
          </Pressable>
          <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 10, lineHeight: 17 }}>
            同时只连一台。名字只存在这台手机上，改它不会动那台机器。
          </Text>
          {reqs.length > 0 ? (
            <View style={{ marginTop: 8, gap: 1 }}>
              <Text style={{ color: t.faint, fontSize: 10.5 }}>最近的请求</Text>
              {reqs.map((q) => (
                <Text
                  key={`${q.at}-${q.path}`}
                  style={{ color: q.outcome === 'ok' ? t.faint : t.red, fontSize: 10, fontFamily: 'monospace' }}
                  numberOfLines={1}
                >
                  {q.path.replace('/v1', '')} · {q.base.replace(/^https?:\/\//, '')} · {q.outcome} · {q.ms}ms
                </Text>
              ))}
            </View>
          ) : null}
        </View>

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
              disabled={!pairing || awake == null || awakeBusy}
              onPress={() => void applyAwake(!(awake?.on ?? false))}
              t={t}
            />
          </View>

          {awake == null ? (
            <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 10 }}>
              {pairing ? '这台电脑还没有这个开关，桌面端可能是旧版本' : '配对一台电脑之后才能用'}
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
                  style={[
                    styles.nameInput,
                    {
                      width: 150,
                      color: t.text,
                      borderColor: t.borderStrong,
                      backgroundColor: t.bg,
                    },
                  ]}
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
              {awakeErr ? (
                <Text style={{ color: t.red, fontSize: 11.5, marginTop: 6 }}>{awakeErr}</Text>
              ) : null}
            </>
          )}
        </View>

        <Text style={[styles.section, { color: t.faint }]}>外观</Text>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={styles.seg}>
            {MODES.map((m) => {
              const on = mode === m.value;
              return (
                <Pressable
                  key={m.value}
                  onPress={() => setMode(m.value)}
                  style={[
                    styles.segItem,
                    on && { backgroundColor: t.accentSoft, borderColor: t.accentLine },
                    { borderColor: on ? t.accentLine : t.border },
                  ]}
                >
                  <Text style={{ color: on ? t.accent2 : t.dim, fontSize: 12.5 }}>{m.label}</Text>
                </Pressable>
              );
            })}
          </View>
          <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 10 }}>
            只影响这台手机，和电脑上的那份设置无关。
          </Text>
        </View>

        <Pressable
          onPress={onUnpair}
          style={({ pressed }) => [
            styles.danger,
            { borderColor: t.redLine, backgroundColor: pressed ? t.redWash : 'transparent' },
          ]}
        >
          <Text style={{ color: t.red, fontSize: 13.5, fontWeight: '600' }}>解除当前这一台的配对</Text>
        </Pressable>
      <PairingQR
        pairing={showingQR !== null ? devices[showingQR] ?? null : null}
        onClose={() => setShowingQR(null)}
      />
      </ScrollView>
        <EdgeBackZone gesture={back.gesture} />
      </Animated.View>
  );
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
  title: { fontSize: 15, fontWeight: '600' },
  body: { padding: 16 },
  section: { fontSize: 11.5, marginTop: 14, marginBottom: 8 },
  card: { borderWidth: 1, borderRadius: 10, padding: 13 },
  kv: { flexDirection: 'row', alignItems: 'baseline', gap: 12, paddingVertical: 7 },
  k: { fontSize: 12.5, width: 48 },
  v: { fontSize: 12.5, flex: 1 },
  mono: { fontFamily: 'monospace', fontSize: 11.5 },
  seg: { flexDirection: 'row', gap: 8 },
  segItem: { borderWidth: 1, borderRadius: 10, paddingHorizontal: 12, paddingVertical: 8 },
  danger: { borderWidth: 1, borderRadius: 10, paddingVertical: 13, alignItems: 'center', marginTop: 26 },
  device: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 9,
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: 12,
    paddingVertical: 10,
    marginBottom: 8,
  },
  trash: { padding: 4 },
  nameInput: {
    borderWidth: 1,
    borderRadius: 7,
    paddingHorizontal: 8,
    paddingVertical: 5,
    fontSize: 13.5,
  },
  addDevice: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 7,
    borderWidth: 1,
    borderRadius: 10,
    paddingVertical: 11,
  },
});
