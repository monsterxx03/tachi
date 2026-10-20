import { Feather } from '@expo/vector-icons';
import { useState } from 'react';
import { ActivityIndicator, Pressable, ScrollView, StyleSheet, Text, TextInput, View } from 'react-native';
import type { ProjectRef } from '../api/types';
import { GestureDetector } from 'react-native-gesture-handler';
import Animated from 'react-native-reanimated';
import { useTheme } from '../theme/ThemeContext';
import { useEdgeBack } from './edgeBack';

/**
 * 新建一段对话。项目是可选的；选择「无项目」时可以改工作目录 —— 手机上没有文件选择器，
 * 所以这里就是一个文本框，`~` 由电脑那边展开成它的主目录。留空就是 `~/workspace`。
 */
export function NewSession({
  projects,
  busy,
  onClose,
  onCreate,
}: {
  projects: ProjectRef[];
  busy: boolean;
  onClose: () => void;
  onCreate: (opts: { projectId?: string; workingDir?: string }) => void;
}) {
  const { tokens: t } = useTheme();
  const back = useEdgeBack(onClose);
  // '' = 无项目（普通会话），否则就是项目 id
  const [projectId, setProjectId] = useState('');
  const [workingDir, setWorkingDir] = useState('~/workspace');

  const pick = (id: string) => setProjectId(id);

  return (
    <GestureDetector gesture={back.gesture}>
      <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onClose} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <Text style={[styles.title, { color: t.title }]}>新建会话</Text>
      </View>

      <ScrollView contentContainerStyle={styles.body} keyboardShouldPersistTaps="handled">
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
        </View>

        {projectId === '' ? (
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
          disabled={busy}
          onPress={() =>
            onCreate(projectId === '' ? { workingDir: workingDir.trim() } : { projectId })
          }
          style={({ pressed }) => [
            styles.btn,
            { backgroundColor: pressed || busy ? t.accentStrong : t.accent, opacity: busy ? 0.8 : 1 },
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
      </Animated.View>
    </GestureDetector>
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
  back: { width: 34, height: 34, borderRadius: 9, alignItems: 'center', justifyContent: 'center' },
  title: { fontSize: 15, fontWeight: '600' },
  body: { padding: 16, gap: 12 },
  card: { borderWidth: 1, borderRadius: 12, padding: 13 },
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
  btn: { borderRadius: 11, paddingVertical: 14, alignItems: 'center' },
});
