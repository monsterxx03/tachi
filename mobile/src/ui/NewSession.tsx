import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { ActivityIndicator, Pressable, ScrollView, StyleSheet, Text, TextInput, View } from 'react-native';
import type { Device } from '../api/storage';
import { deviceKey } from '../api/storage';
import type { ProjectRef } from '../api/types';
import Animated from 'react-native-reanimated';
import { useTheme } from '../theme/ThemeContext';
import { EdgeBackZone, useEdgeBack } from './edgeBack';

/**
 * 新建一段对话。项目是可选的；选择「无项目」时可以改工作目录 —— 手机上没有文件选择器，
 * 所以这里就是一个文本框，`~` 由电脑那边展开成它的主目录。留空就是 `~/workspace`。
 */
export function NewSession({
  devices,
  reachable,
  loadProjects,
  busy,
  onClose,
  onCreate,
}: {
  devices: Device[];
  /** 这一轮读到没有（键是 deviceKey）：连不上的那台仍然可以选，只是先说一声 */
  reachable: Record<string, boolean>;
  /** 某一台上能挑的项目（项目属于那台机器，不是这台手机的） */
  loadProjects: (deviceIndex: number) => Promise<ProjectRef[]>;
  busy: boolean;
  onClose: () => void;
  onCreate: (deviceIndex: number, opts: { projectId?: string; workingDir?: string }) => void;
}) {
  const { tokens: t } = useTheme();
  const back = useEdgeBack(onClose);
  // 每次都要先选建在哪台：几台机器各有各的项目和目录，默认一个很容易建错地方
  const [deviceIndex, setDeviceIndex] = useState<number | null>(null);
  const [projects, setProjects] = useState<ProjectRef[]>([]);
  const [loadingProjects, setLoadingProjects] = useState(false);
  // '' = 无项目（普通会话），否则就是项目 id
  const [projectId, setProjectId] = useState('');
  const [workingDir, setWorkingDir] = useState('~/workspace');

  const pick = (id: string) => setProjectId(id);

  // 换了一台电脑，项目列表也换了 —— 拿着上一台的项目 id 去建，在那边根本不存在
  useEffect(() => {
    if (deviceIndex == null) return;
    let dead = false;
    setProjects([]);
    setProjectId('');
    setLoadingProjects(true);
    void loadProjects(deviceIndex).then((list) => {
      if (!dead) setProjects(list);
    }).finally(() => {
      if (!dead) setLoadingProjects(false);
    });
    return () => {
      dead = true;
    };
  }, [deviceIndex, loadProjects]);

  return (
      <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onClose} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <Text style={[styles.title, { color: t.title }]}>新建会话</Text>
      </View>

      <ScrollView contentContainerStyle={styles.body} keyboardShouldPersistTaps="handled">
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>建在哪台</Text>
          {devices.map((d, i) => {
            const dead = reachable[deviceKey(d)] === false;
            return (
              <Row
                key={deviceKey(d)}
                label={d.label || d.baseUrl}
                hint={dead ? '现在连不上，建的时候要它在线' : d.baseUrl.replace(/^https?:\/\//, '')}
                on={deviceIndex === i}
                onPress={() => {
                  setDeviceIndex(i);
                  // 换机器时把「无项目 + 默认目录」重置一次，避免把上一台的输入带过去
                  setWorkingDir('~/workspace');
                }}
                t={t}
              />
            );
          })}
        </View>

        {deviceIndex == null ? (
          <Text style={[styles.hint, { color: t.faint, textAlign: 'center' }]}>
            先选一台电脑，再挑项目或工作目录。
          </Text>
        ) : (
          <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>项目</Text>
          <Row
            label="无项目"
            hint="自己指定一个工作目录"
            on={projectId === ''}
            onPress={() => pick('')}
            t={t}
          />
          {projects.map((p) => (
            <Row
              key={p.id}
              label={p.name}
              hint={p.runnable ? p.primary : `目录不在了：${p.primary}`}
              on={projectId === p.id}
              disabled={!p.runnable}
              onPress={() => pick(p.id)}
              t={t}
            />
          ))}
          <Text style={[styles.hint, { color: t.faint }]}>
            项目会带来它的工作目录与附加目录，和电脑上新建一个绑定项目的会话一样。
          </Text>
          {loadingProjects ? (
            <Text style={[styles.hint, { color: t.faint }]}>正在读这台电脑上的项目…</Text>
          ) : null}
        </View>
        )}

        {deviceIndex != null && projectId === '' ? (
          <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
            <Text style={[styles.cardTitle, { color: t.text }]}>工作目录</Text>
            <TextInput
              value={workingDir}
              onChangeText={setWorkingDir}
              autoCapitalize="none"
              autoCorrect={false}
              placeholder="~/workspace"
              placeholderTextColor={t.faint}
              style={[styles.input, { backgroundColor: t.inputBg, borderColor: t.border, color: t.text }]}
            />
            <Text style={[styles.hint, { color: t.faint }]}>
              留空就是 ~/workspace。`~` 展开成电脑上的主目录；目录不存在会替你建出来。
            </Text>
          </View>
        ) : null}

        <Pressable
          disabled={busy || deviceIndex == null}
          onPress={() =>
            deviceIndex != null &&
            onCreate(
              deviceIndex,
              projectId === '' ? { workingDir: workingDir.trim() } : { projectId },
            )
          }
          style={({ pressed }) => [
            styles.btn,
            {
              backgroundColor: pressed || busy ? t.accentStrong : t.accent,
              opacity: busy || deviceIndex == null ? 0.5 : 1,
            },
          ]}
        >
          {busy ? (
            <ActivityIndicator color={t.onAccent} size="small" />
          ) : (
            <Text style={{ color: t.onAccent, fontSize: 14, fontWeight: '600' }}>创建</Text>
          )}
        </Pressable>

        <Text style={[styles.hint, { color: t.faint, textAlign: 'center' }]}>
          会话建在电脑上，电脑也能看到它。
        </Text>
      </ScrollView>
        <EdgeBackZone gesture={back.gesture} />
      </Animated.View>
  );
}

function Row({
  label,
  hint,
  on,
  disabled,
  onPress,
  t,
}: {
  label: string;
  hint?: string;
  on: boolean;
  disabled?: boolean;
  onPress: () => void;
  t: ReturnType<typeof useTheme>['tokens'];
}) {
  return (
    <Pressable
      disabled={disabled}
      onPress={onPress}
      style={[
        styles.row,
        {
          borderColor: on ? t.accentLine : t.border,
          backgroundColor: on ? t.accentSoft : 'transparent',
          opacity: disabled ? 0.5 : 1,
        },
      ]}
    >
      <View style={{ flex: 1, minWidth: 0 }}>
        <Text style={{ color: t.text, fontSize: 13.5 }}>{label}</Text>
        {hint ? (
          <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }} numberOfLines={1}>
            {hint}
          </Text>
        ) : null}
      </View>
      {on ? <Feather name="check" size={14} color={t.accent2} /> : null}
    </Pressable>
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
  body: { padding: 16, gap: 12 },
  card: { borderWidth: 1, borderRadius: 10, padding: 13 },
  cardTitle: { fontSize: 13.5, fontWeight: '600' },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: 12,
    paddingVertical: 11,
    marginTop: 8,
  },
  input: { borderWidth: 1, borderRadius: 10, paddingHorizontal: 12, paddingVertical: 11, fontSize: 14, marginTop: 10 },
  hint: { fontSize: 11.5, lineHeight: 17, marginTop: 8 },
  btn: { borderRadius: 10, paddingVertical: 14, alignItems: 'center' },
});
