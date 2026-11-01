import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';
import Animated from 'react-native-reanimated';
import { recentRequests, type ReqRecord } from '../api/client';
import { hostOf } from '../api/label';
import { type Device, deviceKey } from '../api/storage';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeMode } from '../theme/tokens';
import { EdgeBackZone, useEdgeBack } from './edgeBack';

const MODES: { value: ThemeMode; label: string }[] = [
  { value: 'system', label: '跟随系统' },
  { value: 'light', label: '白天' },
  { value: 'dark', label: '黑夜' },
];

/**
 * 这台手机记住的电脑。
 *
 * 它以前是「设置」那一屏：电脑、添加、保持唤醒、外观、解除配对全平铺在一起。分机器之后
 * 每台都有自己的地址、开关和名字，所以上面那些「某一台的事」挪进了它自己的页面（见
 * DeviceDetail）；这一屏只回答「我配过哪些电脑、它们现在通不通」，外加只属于这台手机的
 * 外观。顶上的 + 就是原来那个「添加电脑」。
 */
export function Devices({
  devices,
  reachable,
  syncedAt,
  onAddDevice,
  onOpen,
  onBack,
}: {
  devices: Device[];
  /** 这一轮读到没有（键是 deviceKey） */
  reachable: Record<string, boolean>;
  /** 这台最后一次读到列表的时刻（毫秒，0 = 从没读到） */
  syncedAt: (key: string) => number;
  onAddDevice: () => void;
  onOpen: (index: number) => void;
  onBack: () => void;
}) {
  const { tokens: t, mode, setMode } = useTheme();
  const back = useEdgeBack(onBack);
  // 诊断：最近几次请求的去向与结果。手机上没有控制台，这一行是唯一能读到的证据。
  const [reqs, setReqs] = useState<ReqRecord[]>([]);
  useEffect(() => {
    const timer = setInterval(() => setReqs(recentRequests()), 1000);
    return () => clearInterval(timer);
  }, []);

  const offline = devices.filter((d) => reachable[deviceKey(d)] === false).length;

  return (
    <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onBack} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <View style={styles.headInfo}>
          <Text style={[styles.title, { color: t.title }]}>电脑</Text>
          <View style={{ flexDirection: 'row', alignItems: 'center', gap: 6, marginTop: 3 }}>
            <View style={[styles.dot, { backgroundColor: offline > 0 ? t.amber : t.green }]} />
            <Text style={{ color: t.faint, fontSize: 11.5 }} numberOfLines={1}>
              {devices.length} 台 · {devices.length - offline} 台在线
            </Text>
          </View>
        </View>
        <Pressable
          onPress={onAddDevice}
          style={({ pressed }) => [styles.add, { borderColor: t.border, backgroundColor: pressed ? t.wash : 'transparent' }]}
          accessibilityLabel="添加电脑"
        >
          <Feather name="plus" size={18} color={t.faint} />
        </Pressable>
      </View>

      <ScrollView contentContainerStyle={styles.body}>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          {devices.length === 0 ? (
            <Text style={{ color: t.faint, fontSize: 12.5, paddingVertical: 14 }}>还没配对过电脑</Text>
          ) : null}
          {devices.map((d, i) => {
            const key = deviceKey(d);
            const dead = reachable[key] === false;
            const seen = syncedAt(key);
            return (
              <Pressable
                key={key}
                onPress={() => onOpen(i)}
                style={({ pressed }) => [
                  styles.device,
                  { borderBottomColor: t.border },
                  i === devices.length - 1 && { borderBottomWidth: 0 },
                  pressed && { backgroundColor: t.wash },
                ]}
              >
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Text style={{ color: t.text, fontSize: 13.5 }} numberOfLines={1}>
                    {d.label || d.baseUrl}
                  </Text>
                  <Text
                    style={{ color: t.faint, fontSize: 11, marginTop: 3, fontFamily: 'monospace' }}
                    numberOfLines={1}
                  >
                    {hostOf(d.baseUrl)}
                  </Text>
                  <View style={{ flexDirection: 'row', alignItems: 'center', gap: 6, marginTop: 4 }}>
                    <View style={[styles.dot, { backgroundColor: dead ? 'transparent' : t.green, borderWidth: dead ? 1 : 0, borderColor: t.faint }]} />
                    <Text style={{ color: dead ? t.faint : t.dim, fontSize: 11 }}>
                      {dead ? (seen > 0 ? `连不上 · ${lastSeen(seen)}看过` : '连不上') : '在线'}
                    </Text>
                  </View>
                </View>
                <Feather name="chevron-right" size={16} color={t.faint} />
              </Pressable>
            );
          })}
        </View>
        <Text style={[styles.hint, { color: t.faint }]}>
          名字只存在这台手机上，改它不会动那台机器。点一台进去可以看地址、让它别睡，或者删掉它。
        </Text>

        <Text style={[styles.section, { color: t.faint }]}>这台手机</Text>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={{ paddingVertical: 12 }}>
            <Text style={{ color: t.text, fontSize: 13.5 }}>外观</Text>
            <View style={[styles.seg, { marginTop: 10 }]}>
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
        </View>

        {reqs.length > 0 ? (
          <View style={{ marginTop: 14, gap: 2, paddingHorizontal: 4 }}>
            <Text style={{ color: t.faint, fontSize: 10.5 }}>最近的请求</Text>
            {reqs.map((q, i) => (
              <Text
                // 同一毫秒里可能连着两条同样的请求：只用 at+path 会撞 key，React 每次都警告
                key={`${i}-${q.at}-${q.path}`}
                style={{ color: q.outcome === 'ok' ? t.faint : t.red, fontSize: 10, fontFamily: 'monospace' }}
                numberOfLines={1}
              >
                {q.path.replace('/v1', '')} · {q.base.replace(/^https?:\/\//, '')} · {q.outcome} · {q.ms}ms
              </Text>
            ))}
          </View>
        ) : null}
      </ScrollView>
      <EdgeBackZone gesture={back.gesture} />
    </Animated.View>
  );
}

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
  add: { width: 34, height: 34, borderRadius: 10, borderWidth: 1, alignItems: 'center', justifyContent: 'center' },
  headInfo: { flex: 1, minWidth: 0, paddingHorizontal: 4 },
  title: { fontSize: 15, fontWeight: '600' },
  dot: { width: 6, height: 6, borderRadius: 3 },
  body: { padding: 12, paddingBottom: 24 },
  card: { borderWidth: 1, borderRadius: 12, paddingHorizontal: 12 },
  device: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
    paddingVertical: 13,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  hint: { fontSize: 11.5, lineHeight: 17, paddingHorizontal: 6, paddingTop: 10 },
  section: { fontSize: 11.5, fontWeight: '600', paddingHorizontal: 6, paddingTop: 18, paddingBottom: 8, letterSpacing: 0.4 },
  seg: { flexDirection: 'row', gap: 7, flexWrap: 'wrap' },
  segItem: { height: 30, borderWidth: 1, borderRadius: 15, paddingHorizontal: 12, alignItems: 'center', justifyContent: 'center' },
});
