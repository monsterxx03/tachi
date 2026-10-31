import { Feather } from '@expo/vector-icons';
import { memo, useCallback, useState } from 'react';
import { Alert, FlatList, Pressable, RefreshControl, StyleSheet, Text, View } from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from 'react-native-reanimated';
import type { SessionSummary } from '../api/types';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';
import { usePageSwipe } from './edgeBack';
import { formatUpdatedAt } from './time';
import { RenameDialog } from './RenameDialog';

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
 * **手势在 UI 线程解析**（gesture-handler + reanimated），不是 PanResponder：后者每一帧都要
 * 过一次 JS 线程，而这块屏的 JS 线程随时在被会话事件占着 —— 表现就是「跟不上手指、一划就
 * 弹回去」。`activeOffsetX` 让横向的意图归这一行、纵向归列表，两者不再互相抢。
 */
function SwipeRow({ children, actions }: { children: React.ReactNode; actions: SwipeAction[] }) {
  const { tokens: t } = useTheme();
  const reveal = actions.length * ACTION_WIDTH;
  // 0 = 关着，-reveal = 全开。手势与动画都只碰这两个共享值，不经过 React。
  const tx = useSharedValue(0);
  const opened = useSharedValue(false);
  // 收起时那层动作仍在树里（它就在内容底下），对读屏和自动化来说等于一排看不见的按钮 ——
  // 所以收起时把整层从可达性树里摘掉（这条要 React 知道，所以只有它过 JS）。
  const [revealed, setRevealed] = useState(false);

  const pan = Gesture.Pan()
    // **只认往左**（单个负数只设那一端）。写成数组 `[-8, 8]` 的话 `dx > 8` 也激活：往右那
    // 一划会先被这一行接住、然后一动不动，整页的「右滑进设置」就永远轮不到（这一行在
    // 列表里铺满整屏，等于把整页手势废掉了）。
    .activeOffsetX(-8)
    .failOffsetY([-14, 14])
    .onUpdate((e) => {
      const base = opened.value ? -reveal : 0;
      tx.value = Math.max(-reveal, Math.min(0, base + e.translationX));
    })
    .onEnd(() => {
      const open = tx.value < -reveal / 2;
      opened.value = open;
      // 松手吸附：按「还剩多远」定快慢，短距离一下就到位，整段划开才用满时长
      const remaining = Math.min(1, Math.abs((open ? -reveal : 0) - tx.value) / reveal);
      tx.value = withTiming(open ? -reveal : 0, {
        duration: 90 + Math.round(110 * remaining),
        easing: Easing.out(Easing.cubic),
      });
      runOnJS(setRevealed)(open);
    })
    // 手势被别处接管（比如列表滚动赢了）：收回去，不要停在半路
    .onFinalize((_e, success) => {
      if (success) return;
      opened.value = false;
      tx.value = withTiming(0, { duration: 120, easing: Easing.out(Easing.cubic) });
      runOnJS(setRevealed)(false);
    });

  const style = useAnimatedStyle(() => ({ transform: [{ translateX: tx.value }] }));

  const close = () => {
    opened.value = false;
    tx.value = withTiming(0, { duration: 120, easing: Easing.out(Easing.cubic) });
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
      <GestureDetector gesture={pan}>
        <Animated.View style={[{ backgroundColor: t.sid }, style]}>{children}</Animated.View>
      </GestureDetector>
    </View>
  );
}

export function SessionList({
  sessions,
  currentId,
  conn,
  deviceName,
  onOpen,
  onSettings,
  onNew,
  onRefresh,
  error,
  onDelete,
  onPin,
  onRename,
  onForward,
}: {
  sessions: SessionSummary[];
  currentId: string | null;
  conn: string;
  /** 当前那台电脑在手机上的名字（用户可以改）；空 = 还没配对信息，退回「这台电脑」 */
  deviceName?: string;
  onOpen: (id: string) => void;
  onSettings: () => void;
  onNew: () => void;
  onRefresh: () => void;
  /** 连上了但读不到东西时的那句话（null = 一切正常） */
  error?: string | null;
  onDelete: (id: string) => void;
  onPin: (id: string, pinned: boolean) => void;
  /** 改标题；写的是那台电脑上的记录，成功之后 sessions_changed 让每一块屏都跟着改 */
  onRename: (id: string, title: string) => void;
  /**
   * 整页往右推：进设置页（选电脑的那一屏）。**只在窄屏传** —— 宽屏时列表是常驻侧栏，
   * 把一条侧栏推走没有意义。
   */
  onForward?: () => void;
}) {
  const { tokens: t } = useTheme();
  const [refreshing, setRefreshing] = useState(false);
  // 长按打开的那个对话框。草稿在 RenameDialog 里：没按「保存」之前，那台电脑上的标题一个字都不动
  const [renaming, setRenaming] = useState<SessionSummary | null>(null);
  const online = conn === 'open';

  // 整页往右推 = 进设置页（选电脑的那一屏）。Android 上左缘那一划归系统的返回手势，
  // 所以这条只能铺满整页 —— 阈值的写法见 ./edgeBack 的 usePageSwipe（给错一个数字，这一屏
  // 的点击、长按、行左滑就全被它吃掉了）
  const page = usePageSwipe(onForward);

  const askRename = useCallback((s: SessionSummary) => setRenaming(s), []);

  return (
    <GestureDetector gesture={page.gesture}>
      <Animated.View style={[styles.root, { backgroundColor: t.sid }, page.style]}>
      <View style={styles.head}>
        <View style={styles.headText}>
          <Text style={[styles.brand, { color: t.title }]}>会话</Text>
          <View style={styles.subRow}>
            <View style={[styles.onlineDot, { backgroundColor: online ? t.green : t.amber }]} />
            <Text style={[styles.sub, { color: t.faint }]} numberOfLines={1}>
              {online
                ? `已连接 · ${deviceName || '这台电脑'}`
                : conn === 'connecting'
                ? '正在连接电脑…'
                : '连不上电脑'}
            </Text>
          </View>
          {/* 「已连接」和「读得到东西」是两件事：WebSocket 通了不代表 HTTP 请求回来了。
              这里只有真的有问题时才出现，平时不占地方。 */}
          {error ? (
            <Pressable
              onPress={onRefresh}
              style={({ pressed }) => [
                styles.errLine,
                { backgroundColor: pressed ? t.redWash : 'transparent' },
              ]}
            >
              <Text style={{ color: t.red, fontSize: 11.5 }} numberOfLines={2}>
                {error} · 点这里重试
              </Text>
            </Pressable>
          ) : null}
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
        renderItem={({ item }) => (
          <SessionRow
            s={item}
            selected={item.id === currentId}
            onOpen={onOpen}
            onDelete={onDelete}
            onPin={onPin}
            onRename={askRename}
          />
        )}
      />

      {/* 长按一行改名字。标题是那台电脑上的记录，这里只递交一个词：保存之后由 sessions_changed
          让每一块屏重读 —— 桌面上那一行不会停在旧名字上，这一段也不会各留一份。
          对话框与对话顶栏长按标题用的是同一个（./RenameDialog）。 */}
      {renaming ? (
        <RenameDialog
          initial={renaming.title}
          onCancel={() => setRenaming(null)}
          onSave={(title) => {
            const id = renaming.id;
            setRenaming(null);
            onRename(id, title);
          }}
        />
      ) : null}
      </Animated.View>
    </GestureDetector>
  );
}

/**
 * 一行。**memo 过**：外层的聊天状态每秒会变好几次（流式文字、tps、排队），Shell 一重渲染
 * 这个列表就跟着重渲染；不 memo 的话，正在左滑的那一行每一帧都在被别人的事件挤占 JS 线程
 * —— 看上去就是「一点都不丝滑」。传进来的都是稳定引用或原始值，所以只有这一行自己的
 * 状态变了才会重画。
 */
const SessionRow = memo(function SessionRow({
  s,
  selected,
  onOpen,
  onDelete,
  onPin,
  onRename,
}: {
  s: SessionSummary;
  selected: boolean;
  onOpen: (id: string) => void;
  onDelete: (id: string) => void;
  onPin: (id: string, pinned: boolean) => void;
  /** 长按一行：改名字。左滑只有两个动作，第三个放不下，也不该挤掉「删除」的宽度 */
  onRename: (s: SessionSummary) => void;
}) {
  const { tokens: t } = useTheme();
  const badge = badgeOf(s);

  const actions: SwipeAction[] = [
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
  ];

  return (
    <SwipeRow actions={actions}>
      <Pressable
        onPress={() => onOpen(s.id)}
        onLongPress={() => onRename(s)}
        // 长按要「按住一会儿」才算，不是一碰就弹：这一屏的主要动作是打开会话
        delayLongPress={400}
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
          {s.channelSession ? (
            <View style={[styles.sourcePill, { borderColor: t.border }]}>
              <Text style={[styles.sourcePillText, { color: t.faint }]} numberOfLines={1}>
                {s.channelName || '渠道'}
              </Text>
            </View>
          ) : null}
          {/* 有定时任务写进这段对话：一个闹钟（多于一个时带条数）。它和「渠道」不是一回事 ——
              这里的话是别的东西在替它说话，而标题上看不出来。 */}
          {(s.cron ?? 0) > 0 ? (
            <View
              style={[styles.sourcePill, styles.cronPill, { borderColor: t.border }]}
              accessibilityLabel={s.cron === 1 ? '有 1 个定时任务' : `有 ${s.cron} 个定时任务`}
            >
              <Feather name="clock" size={10} color={t.faint} />
              {(s.cron ?? 0) > 1 ? (
                <Text style={[styles.sourcePillText, { color: t.faint }]}>{s.cron}</Text>
              ) : null}
            </View>
          ) : null}
          {badge ? <Badge {...badge} t={t} /> : null}
        </View>
        <Text style={[styles.rowSub, { color: t.faint }]} numberOfLines={1}>
          {s.projectName ? `${s.projectName} · ` : ''}
          {formatUpdatedAt(s.updatedAt)}
        </Text>
      </Pressable>
    </SwipeRow>
  );
});

const styles = StyleSheet.create({
  root: { flex: 1 },
  head: { flexDirection: 'row', alignItems: 'flex-start', gap: 10, paddingTop: 14, paddingHorizontal: 16, paddingBottom: 8 },
  headText: { flex: 1, minWidth: 0 },
  brand: { fontSize: 25, fontWeight: '600', letterSpacing: -0.4 },
  subRow: { flexDirection: 'row', alignItems: 'center', gap: 6, marginTop: 4 },
  onlineDot: { width: 6, height: 6, borderRadius: 3 },
  sub: { fontSize: 12, flexShrink: 1 },
  errLine: { paddingHorizontal: 14, paddingVertical: 8, borderRadius: 8, marginTop: 6 },
  gear: { width: 34, height: 34, borderRadius: 10, borderWidth: 1, alignItems: 'center', justifyContent: 'center', marginTop: 2 },

  listContent: { paddingHorizontal: 8, paddingTop: 4, paddingBottom: 16 },
  swipeWrap: { borderRadius: 10, overflow: 'hidden', marginVertical: 2 },
  actionLayer: { position: 'absolute', right: 0, top: 0, bottom: 0, flexDirection: 'row' },
  actionBtn: { width: ACTION_WIDTH, alignItems: 'center', justifyContent: 'center' },
  row: { borderRadius: 10, borderWidth: 1, borderColor: 'transparent', paddingTop: 12, paddingHorizontal: 12, paddingBottom: 11 },
  rowTop: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  rowTitle: { flex: 1, minWidth: 0, fontSize: 14.5, fontWeight: '600' },
  rowSub: { marginTop: 4, fontSize: 12 },
  sourcePill: { borderWidth: 1, borderRadius: 20, paddingHorizontal: 6, paddingVertical: 1, maxWidth: 88 },
  // 闹钟那一条：图标 + 条数并排，比渠道那个窄很多
  cronPill: { flexDirection: 'row', alignItems: 'center', gap: 3, paddingHorizontal: 5, maxWidth: undefined },
  sourcePillText: { fontSize: 10.5 },

  pill: { flexDirection: 'row', alignItems: 'center', gap: 5, borderWidth: 1, borderColor: 'transparent', borderRadius: 20, paddingHorizontal: 7, paddingVertical: 2 },
  pillText: { fontSize: 10.5 },
  liveDot: { width: 5, height: 5, borderRadius: 2.5 },
});
