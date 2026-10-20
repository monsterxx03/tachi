import { Feather } from '@expo/vector-icons';
import { useRef, useState } from 'react';
import {
  Alert,
  Animated,
  FlatList,
  PanResponder,
  Pressable,
  RefreshControl,
  StyleSheet,
  Text,
  View,
} from 'react-native';
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

/** 左滑露出来的那一截：每个动作一个按钮的宽度 */
const ACTION_WIDTH = 84;

export interface SwipeAction {
  label: string;
  icon: 'bookmark' | 'trash-2';
  tone: 'accent' | 'red';
  onPress: () => void;
}

/**
 * 一行可以左滑露出动作（置顶 / 删除）。
 *
 * 只在横向移动超过 8px 时接管手势 —— 轻点仍然落到里面的 Pressable 上，
 * 否则点开一段对话这件事会被滑动手势吃掉。
 */
function SwipeRow({ children, actions }: { children: React.ReactNode; actions: SwipeAction[] }) {
  const { tokens: t } = useTheme();
  const tx = useRef(new Animated.Value(0)).current;
  const opened = useRef(false);
  // 收起时那层动作仍在树里（它就在内容底下），对读屏和自动化来说等于一排看不见的按钮 ——
  // 所以收起时把整层从可达性树里摘掉。
  const [revealed, setRevealed] = useState(false);
  const reveal = actions.length * ACTION_WIDTH;

  const pan = useRef(
    PanResponder.create({
      onMoveShouldSetPanResponder: (_e, g) => Math.abs(g.dx) > 8 && Math.abs(g.dx) > Math.abs(g.dy) * 1.5,
      onPanResponderMove: (_e, g) => {
        const base = opened.current ? -reveal : 0;
        tx.setValue(Math.max(-reveal, Math.min(0, base + g.dx)));
      },
      onPanResponderRelease: (_e, g) => {
        const open = (opened.current ? -reveal : 0) + g.dx < -reveal / 2;
        opened.current = open;
        setRevealed(open);
        Animated.spring(tx, {
          toValue: open ? -reveal : 0,
          useNativeDriver: false,
          bounciness: 0,
        }).start();
      },
    }),
  ).current;

  const close = () => {
    Animated.spring(tx, { toValue: 0, useNativeDriver: false, bounciness: 0 }).start();
    opened.current = false;
    setRevealed(false);
  };

  return (
    <View style={styles.swipeWrap}>
      <View
        style={[styles.actionLayer, { width: reveal, backgroundColor: t.wash }]}
        importantForAccessibility={revealed ? 'auto' : 'no-hide-descendants'}
        accessibilityElementsHidden={!revealed}
      >
        {actions.map((a) => {
          const color = a.tone === 'red' ? t.red : t.accent2;
          return (
            <Pressable
              key={a.label}
              onPress={() => {
                close();
                a.onPress();
              }}
              style={[styles.actionBtn, { backgroundColor: a.tone === 'red' ? t.redWash : t.accentSoft }]}
            >
              <Feather name={a.icon} size={16} color={color} />
              <Text style={{ color, fontSize: 11, marginTop: 3 }}>{a.label}</Text>
            </Pressable>
          );
        })}
      </View>
      <Animated.View style={{ transform: [{ translateX: tx }], backgroundColor: t.sid }} {...pan.panHandlers}>
        {children}
      </Animated.View>
    </View>
  );
}

export function SessionList({
  sessions,
  currentId,
  conn,
  onOpen,
  onSettings,
  onNew,
  onRefresh,
  onDelete,
  onPin,
}: {
  sessions: SessionSummary[];
  currentId: string | null;
  conn: string;
  onOpen: (id: string) => void;
  onSettings: () => void;
  onNew: () => void;
  onRefresh: () => void;
  onDelete: (id: string) => void;
  onPin: (id: string, pinned: boolean) => void;
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
          onPress={onNew}
          style={({ pressed }) => [
            styles.gear,
            { borderColor: t.border, backgroundColor: pressed ? t.wash : 'transparent' },
          ]}
        >
          <Feather name="plus" size={18} color={t.faint} />
        </Pressable>
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
            <SwipeRow
              actions={[
                {
                  label: s.pinned ? '取消置顶' : '置顶',
                  icon: 'bookmark',
                  tone: 'accent',
                  onPress: () => onPin(s.id, !s.pinned),
                },
                {
                  label: '删除',
                  icon: 'trash-2',
                  tone: 'red',
                  onPress: () =>
                    Alert.alert('删除这段对话？', s.title, [
                      { text: '取消', style: 'cancel' },
                      { text: '删除', style: 'destructive', onPress: () => onDelete(s.id) },
                    ]),
                },
              ]}
            >
              <Pressable
                onPress={() => onOpen(s.id)}
                style={({ pressed }) => [
                  styles.row,
                  selected && { backgroundColor: t.accentSoft, borderColor: t.accentLine },
                  !selected && pressed && { backgroundColor: t.wash },
                ]}
              >
                <View style={styles.rowTop}>
                  {s.pinned ? <Feather name="bookmark" size={12} color={t.faint} style={{ marginTop: 1 }} /> : null}
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
            </SwipeRow>
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
  swipeWrap: { borderRadius: 12, overflow: 'hidden', marginVertical: 2 },
  actionLayer: { position: 'absolute', right: 0, top: 0, bottom: 0, flexDirection: 'row' },
  actionBtn: { width: ACTION_WIDTH, alignItems: 'center', justifyContent: 'center' },
  row: { borderRadius: 12, borderWidth: 1, borderColor: 'transparent', paddingTop: 12, paddingHorizontal: 12, paddingBottom: 11 },
  rowTop: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  rowTitle: { flex: 1, minWidth: 0, fontSize: 14.5, fontWeight: '600' },
  rowSub: { marginTop: 4, fontSize: 12 },

  pill: { flexDirection: 'row', alignItems: 'center', gap: 5, borderWidth: 1, borderColor: 'transparent', borderRadius: 20, paddingHorizontal: 7, paddingVertical: 2 },
  pillText: { fontSize: 10.5 },
  liveDot: { width: 5, height: 5, borderRadius: 2.5 },
});
