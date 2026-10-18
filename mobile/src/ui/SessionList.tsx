import { Feather } from '@expo/vector-icons';
import { useState } from 'react';
import { FlatList, Pressable, RefreshControl, StyleSheet, Text, View } from 'react-native';
import type { SessionSummary } from '../api/types';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';

/** 一行右边的标记：只读 > 等你授权 > 等你回答 > 在跑（与侧栏同一套先后） */
function badgeOf(s: SessionSummary): { text: string; tone: 'mute' | 'amber' | 'live' } | null {
  if (s.readOnly) return { text: '只读', tone: 'mute' };
  if (s.waiting === '等待你的授权') return { text: '等你授权', tone: 'amber' };
  if (s.waiting === '等待你的回答') return { text: '等你回答', tone: 'amber' };
  if (s.running) return { text: '在跑', tone: 'live' };
  return null;
}

function Badge({ text, tone, t }: { text: string; tone: 'mute' | 'amber' | 'live'; t: ThemeTokens }) {
  const toneStyle =
    tone === 'amber'
      ? { color: t.amber, borderColor: t.amberLine, backgroundColor: t.amberWash }
      : tone === 'live'
        ? { color: t.accent2, borderColor: t.accentLine, backgroundColor: t.accentSoft }
        : { color: t.faint, opacity: 0.8 };
  return (
    <View style={[styles.pill, toneStyle]}>
      {tone === 'live' ? <View style={[styles.liveDot, { backgroundColor: t.accent }]} /> : null}
      <Text style={[styles.pillText, { color: toneStyle.color }]}>{text}</Text>
    </View>
  );
}

export function SessionList({
  sessions,
  currentId,
  conn,
  onOpen,
  onSettings,
  onRefresh,
}: {
  sessions: SessionSummary[];
  currentId: string | null;
  conn: string;
  onOpen: (id: string) => void;
  onSettings: () => void;
  onRefresh: () => void;
}) {
  const { tokens: t } = useTheme();
  const [refreshing, setRefreshing] = useState(false);
  const online = conn === 'open';

  return (
    <View style={[styles.root, { backgroundColor: t.sid }]}>
      <View style={styles.head}>
        <View style={styles.headText}>
          <Text style={[styles.brand, { color: t.title }]}>会话</Text>
          <View style={styles.subRow}>
            <View style={[styles.onlineDot, { backgroundColor: online ? t.green : t.amber }]} />
            <Text style={[styles.sub, { color: t.faint }]} numberOfLines={1}>
              {online ? '已连接 · 这台电脑' : conn === 'connecting' ? '正在连接电脑…' : '连不上电脑'}
            </Text>
          </View>
        </View>
        <Pressable
          onPress={onSettings}
          style={({ pressed }) => [
            styles.gear,
            { borderColor: t.border, backgroundColor: pressed ? t.wash : 'transparent' },
          ]}
        >
          <Feather name="settings" size={17} color={t.faint} />
        </Pressable>
      </View>

      <FlatList
        data={sessions}
        keyExtractor={(s) => s.id}
        contentContainerStyle={styles.listContent}
        refreshControl={
          <RefreshControl
            refreshing={refreshing}
            onRefresh={() => {
              setRefreshing(true);
              onRefresh();
              setTimeout(() => setRefreshing(false), 600);
            }}
            tintColor={t.faint}
          />
        }
        renderItem={({ item }) => {
          const s = item;
          const selected = s.id === currentId;
          const badge = badgeOf(s);
          return (
            <Pressable
              onPress={() => onOpen(s.id)}
              style={({ pressed }) => [
                styles.row,
                selected && { backgroundColor: t.accentSoft, borderColor: t.accentLine },
                !selected && pressed && { backgroundColor: t.wash },
              ]}
            >
              <View style={styles.rowTop}>
                <Text style={[styles.rowTitle, { color: t.title }]} numberOfLines={1}>
                  {s.title}
                </Text>
                {badge ? <Badge {...badge} t={t} /> : null}
              </View>
              <Text style={[styles.rowSub, { color: t.faint }]} numberOfLines={1}>
                {s.projectName ? `${s.projectName} · ` : ''}
                {s.updatedAt}
              </Text>
            </Pressable>
          );
        }}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  head: { flexDirection: 'row', alignItems: 'flex-start', gap: 10, paddingTop: 14, paddingHorizontal: 16, paddingBottom: 8 },
  headText: { flex: 1, minWidth: 0 },
  brand: { fontSize: 25, fontWeight: '600', letterSpacing: -0.4 },
  subRow: { flexDirection: 'row', alignItems: 'center', gap: 6, marginTop: 4 },
  onlineDot: { width: 6, height: 6, borderRadius: 3 },
  sub: { fontSize: 12, flexShrink: 1 },
  gear: { width: 34, height: 34, borderRadius: 9, borderWidth: 1, alignItems: 'center', justifyContent: 'center', marginTop: 2 },

  listContent: { paddingHorizontal: 8, paddingTop: 4, paddingBottom: 16 },
  row: { borderRadius: 12, borderWidth: 1, borderColor: 'transparent', paddingTop: 12, paddingHorizontal: 12, paddingBottom: 11, marginVertical: 2 },
  rowTop: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  rowTitle: { flex: 1, minWidth: 0, fontSize: 14.5, fontWeight: '600' },
  rowSub: { marginTop: 4, fontSize: 12 },

  pill: { flexDirection: 'row', alignItems: 'center', gap: 5, borderWidth: 1, borderColor: 'transparent', borderRadius: 20, paddingHorizontal: 7, paddingVertical: 2 },
  pillText: { fontSize: 10.5 },
  liveDot: { width: 5, height: 5, borderRadius: 2.5 },
});
