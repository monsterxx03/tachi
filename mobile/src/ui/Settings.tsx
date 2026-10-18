import { Feather } from '@expo/vector-icons';
import { Pressable, StyleSheet, Text, View } from 'react-native';
import type { Pairing as PairingInfo } from '../api/client';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeMode } from '../theme/tokens';

const MODES: { value: ThemeMode; label: string }[] = [
  { value: 'system', label: '跟随系统' },
  { value: 'light', label: '白天' },
  { value: 'dark', label: '黑夜' },
];

export function Settings({
  pairing,
  conn,
  onUnpair,
  onBack,
}: {
  pairing: PairingInfo | null;
  conn: string;
  onUnpair: () => void;
  onBack: () => void;
}) {
  const { tokens: t, mode, setMode } = useTheme();

  return (
    <View style={[styles.root, { backgroundColor: t.bg }]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onBack} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <Text style={[styles.title, { color: t.title }]}>设置</Text>
      </View>

      <View style={styles.body}>
        <Text style={[styles.section, { color: t.faint }]}>连接</Text>
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>状态</Text>
            <Text style={[styles.v, { color: conn === 'open' ? t.green : t.amber }]}>
              {conn === 'open' ? '已连接' : conn === 'connecting' ? '正在连接…' : '连不上'}
            </Text>
          </View>
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>地址</Text>
            <Text style={[styles.v, styles.mono, { color: t.dim }]}>{pairing?.baseUrl ?? '未配对'}</Text>
          </View>
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
          <Text style={{ color: t.red, fontSize: 13.5, fontWeight: '600' }}>解除配对</Text>
        </Pressable>
      </View>
    </View>
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
});
