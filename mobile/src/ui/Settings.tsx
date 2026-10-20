import { Feather } from '@expo/vector-icons';
import { Alert, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';
import type { Pairing as PairingInfo } from '../api/client';
import type { Device } from '../api/storage';
import { GestureDetector } from 'react-native-gesture-handler';
import Animated from 'react-native-reanimated';
import { useTheme } from '../theme/ThemeContext';
import { useEdgeBack } from './edgeBack';
import type { ThemeMode } from '../theme/tokens';

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
  onAddDevice,
  onUnpair,
  onBack,
}: {
  pairing: PairingInfo | null;
  devices: Device[];
  activeDevice: number;
  conn: string;
  onSwitchDevice: (index: number) => void;
  onRemoveDevice: (index: number) => void;
  onAddDevice: () => void;
  onUnpair: () => void;
  onBack: () => void;
}) {
  const { tokens: t, mode, setMode } = useTheme();
  const back = useEdgeBack(onBack);

  return (
    <GestureDetector gesture={back.gesture}>
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
            return (
              <Pressable
                key={`${d.baseUrl}-${i}`}
                onPress={() => (on ? undefined : onSwitchDevice(i))}
                style={[styles.device, { borderColor: on ? t.accentLine : t.border, backgroundColor: on ? t.accentSoft : 'transparent' }]}
              >
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Text style={{ color: t.text, fontSize: 13.5 }} numberOfLines={1}>
                    {d.label || d.baseUrl}
                  </Text>
                  <Text style={{ color: t.faint, fontSize: 11, marginTop: 2, fontFamily: 'monospace' }} numberOfLines={1}>
                    {on
                      ? `${d.baseUrl} · ${conn === 'open' ? '已连接' : conn === 'connecting' ? '正在连接…' : '连不上'}`
                      : d.baseUrl}
                  </Text>
                </View>
                {on ? <Feather name="check" size={14} color={t.accent2} /> : null}
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
            同时只连一台：会话在电脑上，切过去看到的是那台电脑的会话。地址 {pairing?.baseUrl ?? '—'}
          </Text>
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
      </ScrollView>
      </Animated.View>
    </GestureDetector>
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
  back: { width: 34, height: 34, borderRadius: 9, alignItems: 'center', justifyContent: 'center' },
  title: { fontSize: 15, fontWeight: '600' },
  body: { padding: 16 },
  section: { fontSize: 11.5, marginTop: 14, marginBottom: 8 },
  card: { borderWidth: 1, borderRadius: 12, padding: 13 },
  kv: { flexDirection: 'row', alignItems: 'baseline', gap: 12, paddingVertical: 7 },
  k: { fontSize: 12.5, width: 48 },
  v: { fontSize: 12.5, flex: 1 },
  mono: { fontFamily: 'monospace', fontSize: 11.5 },
  seg: { flexDirection: 'row', gap: 8 },
  segItem: { borderWidth: 1, borderRadius: 9, paddingHorizontal: 12, paddingVertical: 8 },
  danger: { borderWidth: 1, borderRadius: 11, paddingVertical: 13, alignItems: 'center', marginTop: 26 },
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
