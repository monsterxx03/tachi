import { Feather } from '@expo/vector-icons';
import { Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';
import type { Snapshot } from '../api/types';
import { useTheme } from '../theme/ThemeContext';

/** 分项条的四档：同一个色系由深到浅，别用语义色（绿/琥珀说的是「好/警告」，这里说的是「占比」） */
function partColors(t: ReturnType<typeof useTheme>['tokens']): string[] {
  return [t.accent, t.accent2, t.accentLine, t.accentSoft];
}

function CtxBar({ parts, total, t }: { parts: { label: string; tokens: number }[]; total: number; t: ReturnType<typeof useTheme>['tokens'] }) {
  const colors = partColors(t);
  return (
    <>
      <View style={[styles.ctxBar, { backgroundColor: t.wash }]}>
        <View style={styles.ctxFill}>
          {parts.map((p, i) => (
            <View
              key={p.label}
              style={{ height: 8, width: `${total > 0 ? (p.tokens / total) * 100 : 0}%`, backgroundColor: colors[i % colors.length] }}
            />
          ))}
        </View>
      </View>
      <View style={styles.ctxRows}>
        {parts.map((p, i) => (
          <View key={p.label} style={styles.ctxRow}>
            <View style={{ width: 7, height: 7, borderRadius: 2, backgroundColor: colors[i % colors.length] }} />
            <Text style={[styles.ctxLabel, { color: t.faint }]}>{p.label}</Text>
            <Text style={[styles.ctxNum, { color: t.dim }]}>{p.tokens.toLocaleString()}</Text>
          </View>
        ))}
      </View>
    </>
  );
}

function Switch({ on, disabled, onPress, t }: { on: boolean; disabled?: boolean; onPress: () => void; t: ReturnType<typeof useTheme>['tokens'] }) {
  return (
    <Pressable
      disabled={disabled}
      onPress={onPress}
      style={[
        styles.switch,
        {
          backgroundColor: on ? t.amberWash : t.washStrong,
          borderColor: on ? t.amberLine : t.borderStrong,
          opacity: disabled ? 0.4 : 1,
        },
      ]}
    >
      <View style={[styles.knob, { left: on ? 20 : 2, backgroundColor: on ? t.amber : t.switchOff }]} />
    </Pressable>
  );
}

export function SessionInfo({
  snapshot,
  providers,
  onClose,
  onPickModel,
  onToggleSkip,
}: {
  snapshot: Snapshot;
  providers: string[];
  onClose: () => void;
  onPickModel: (name: string) => void;
  onToggleSkip: (skip: boolean) => void;
}) {
  const { tokens: t } = useTheme();
  const s = snapshot.session;
  const ratio = snapshot.context.window > 0 ? snapshot.context.estimate / snapshot.context.window : 0;
  const readOnly = s.readOnly;

  return (
    <View style={[styles.root, { backgroundColor: t.bg }]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onClose} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <Text style={[styles.title, { color: t.title }]}>会话信息</Text>
      </View>

      <ScrollView contentContainerStyle={styles.body}>
        {/* 上下文 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={styles.cardHead}>
            <Text style={[styles.cardTitle, { color: t.text }]}>上下文</Text>
            <Text style={[styles.cardNum, { color: t.dim }]}>
              {snapshot.context.estimate.toLocaleString()} / {snapshot.context.window.toLocaleString()}
              <Text style={{ color: t.faint }}> · {Math.round(ratio * 100)}%</Text>
            </Text>
          </View>
          <CtxBar parts={snapshot.context.parts} total={snapshot.context.estimate} t={t} />
        </View>

        {/* 工作区 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>工作区</Text>
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>主目录</Text>
            <Text style={[styles.v, styles.mono, { color: t.dim }]}>{snapshot.workspace.primary || '—'}</Text>
          </View>
          {snapshot.workspace.additional.map((d) => (
            <View key={d} style={styles.kv}>
              <Text style={[styles.k, { color: t.faint }]}>附加</Text>
              <Text style={[styles.v, styles.mono, { color: t.dim }]}>{d}</Text>
            </View>
          ))}
          <Text style={[styles.hint, { color: t.faint }]}>改目录是电脑上的事，手机只读。</Text>
        </View>

        {/* 模型 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>模型</Text>
          {providers.map((name) => {
            const on = name === snapshot.model;
            return (
              <Pressable
                key={name}
                disabled={readOnly}
                onPress={() => onPickModel(name)}
                style={[
                  styles.modelRow,
                  {
                    borderColor: on ? t.accentLine : t.border,
                    backgroundColor: on ? t.accentSoft : 'transparent',
                    opacity: readOnly ? 0.55 : 1,
                  },
                ]}
              >
                <Text style={{ color: t.text, fontSize: 13.5, flex: 1 }}>{name}</Text>
                {on ? <Feather name="check" size={14} color={t.accent2} /> : null}
              </Pressable>
            );
          })}
          <Text style={[styles.hint, { color: t.faint }]}>只影响这段对话，不改电脑上的默认。</Text>
        </View>

        {/* 审批 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={styles.switchRow}>
            <View style={{ flex: 1 }}>
              <Text style={[styles.cardTitle, { color: t.text }]}>跳过审批</Text>
              <Text style={[styles.hint, { color: t.faint }]}>
                命中 ask 规则的命令会直接执行，不再停下来问。只影响这段对话。
              </Text>
            </View>
            <Switch on={snapshot.skipApproval} disabled={readOnly} onPress={() => onToggleSkip(!snapshot.skipApproval)} t={t} />
          </View>
          {readOnly ? (
            <Text style={[styles.hint, { color: t.faint, marginTop: 8 }]}>这段对话由别的任务占用，改不了。</Text>
          ) : null}
        </View>

        {/* 用量 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>用量</Text>
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>这段对话</Text>
            <Text style={[styles.v, { color: t.dim }]}>${snapshot.usage.cost.toFixed(3)}</Text>
          </View>
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>速度</Text>
            <Text style={[styles.v, { color: t.dim }]}>52 tok/s</Text>
          </View>
        </View>
      </ScrollView>
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
  body: { padding: 16, gap: 12 },
  card: { borderWidth: 1, borderRadius: 12, padding: 13 },
  cardHead: { flexDirection: 'row', alignItems: 'baseline', justifyContent: 'space-between' },
  cardTitle: { fontSize: 13.5, fontWeight: '600' },
  cardNum: { fontSize: 12, fontVariant: ['tabular-nums'] },
  ctxBar: { height: 8, borderRadius: 5, overflow: 'hidden', marginTop: 11, marginBottom: 13 },
  ctxFill: { flexDirection: 'row', height: '100%' },
  ctxRows: { gap: 7 },
  ctxRow: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  ctxLabel: { fontSize: 11.5, flex: 1 },
  ctxNum: { fontSize: 11.5, fontVariant: ['tabular-nums'] },
  kv: { flexDirection: 'row', alignItems: 'baseline', gap: 12, paddingTop: 10 },
  k: { fontSize: 12, width: 52 },
  v: { fontSize: 12.5, flex: 1, lineHeight: 18 },
  mono: { fontFamily: 'monospace', fontSize: 11.5 },
  hint: { fontSize: 11.5, lineHeight: 17, marginTop: 8 },
  modelRow: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: 12,
    paddingVertical: 11,
    marginTop: 8,
  },
  switchRow: { flexDirection: 'row', alignItems: 'flex-start', gap: 12 },
  switch: { width: 44, height: 26, borderRadius: 20, borderWidth: 1, justifyContent: 'center' },
  knob: { position: 'absolute', width: 20, height: 20, borderRadius: 10 },
});
