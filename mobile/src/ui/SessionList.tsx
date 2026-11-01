import { Feather } from '@expo/vector-icons';
import { memo, useCallback, useMemo, useState } from 'react';
import { Alert, FlatList, Pressable, RefreshControl, ScrollView, StyleSheet, Text, View } from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from 'react-native-reanimated';
import type { Device } from '../api/storage';
import { deviceKey } from '../api/storage';
import type { SessionSummary } from '../api/types';
import { type OverviewRow, type Section, sectionOf } from '../state/overview';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';
import { usePageSwipe } from './edgeBack';
import { machineColor } from './machineColor';
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

/** 三段的标题。放英文小写是因为它是一块「状态区」的标签，不是一句话 */
const SECTION_LABEL: Record<Section, string> = {
  waiting: '等待',
  running: '正在跑',
  rest: '其余',
};

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

/** 列表里的一项：分段标题，或者一行对话 */
type Item =
  | { kind: 'section'; section: Section; count: number }
  | { kind: 'row'; row: OverviewRow };

export function SessionList({
  rows,
  devices,
  reachable,
  currentId,
  conn,
  filterKey,
  onFilter,
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
  /** 所有电脑的会话拼成一列（`buildOverview` 的结果，顺序就是显示顺序） */
  rows: OverviewRow[];
  devices: Device[];
  /** 这一轮读到没有（键是 deviceKey）。false 的那台，行会淡化 */
  reachable: Record<string, boolean>;
  currentId: string | null;
  conn: string;
  /** 只看某一台（键是 deviceKey）；null = 全部 */
  filterKey: string | null;
  onFilter: (key: string | null) => void;
  /**
   * 打开某台电脑上的一段。切换发生在状态层（一条 socket 只连一台），这一屏只管把人送过去。
   */
  onOpen: (deviceIndex: number, id: string) => void;
  onSettings: () => void;
  onNew: () => void;
  onRefresh: () => void;
  /** 连上了但读不到东西时的那句话（null = 一切正常） */
  error?: string | null;
  onDelete: (deviceIndex: number, id: string) => void;
  onPin: (deviceIndex: number, id: string, pinned: boolean) => void;
  /** 改标题；写的是那台电脑上的记录，成功之后 sessions_changed 让每一块屏都跟着改 */
  onRename: (deviceIndex: number, id: string, title: string) => void;
  /**
   * 整页往右推：进电脑清单。**只在窄屏传** —— 宽屏时列表是常驻侧栏，
   * 把一条侧栏推走没有意义。
   */
  onForward?: () => void;
}) {
  const { tokens: t, isDark } = useTheme();
  const [refreshing, setRefreshing] = useState(false);
  // 长按打开的那个对话框。草稿在 RenameDialog 里：没按「保存」之前，那台电脑上的标题一个字都不动
  const [renaming, setRenaming] = useState<OverviewRow | null>(null);
  const online = conn === 'open';
  // 筛选的那台可能刚被删掉：那就退回到「全部」，别停在一个空列表上
  const filter = filterKey && devices.some((d) => deviceKey(d) === filterKey) ? filterKey : null;

  // 整页往右推 = 进电脑清单（左滑已经归了行上的置顶 / 删除）
  const page = usePageSwipe(onForward);

  // 每台有几段在等你 / 在跑：chip 上那个点和数字。用全部行算，不受当前筛选影响
  const { waitingByKey, runningByKey } = useMemo(() => {
    const waiting: Record<string, number> = {};
    const running: Record<string, number> = {};
    for (const r of rows) {
      if (r.session.waiting) waiting[r.key] = (waiting[r.key] ?? 0) + 1;
      if (r.session.running) running[r.key] = (running[r.key] ?? 0) + 1;
    }
    return { waitingByKey: waiting, runningByKey: running };
  }, [rows]);

  /**
   * chip 的次序：有东西等你的排最前，其次有在跑的，再是其余。
   *
   * 机器多了之后横滚会把远的那几台藏起来 —— 而最该被看见的恰恰是「这台在等你」。同一档
   * 里保持设备簿的顺序（稳定排序），否则每次刷新 chip 都会跳一下。
   */
  const chipDevices = useMemo(() => {
    // 连不上的永远排最后：它上面的一切都是几小时前的，不能占着最该被看见的位置
    const rank = (key: string) => {
      if (reachable[key] === false) return 3;
      if ((waitingByKey[key] ?? 0) > 0) return 0;
      if ((runningByKey[key] ?? 0) > 0) return 1;
      return 2;
    };
    return devices
      .map((d, index) => ({ d, index, key: deviceKey(d), n: waitingByKey[deviceKey(d)] ?? 0 }))
      .map((x) => ({ ...x, rank: rank(x.key) }))
      .sort((a, b) => a.rank - b.rank);
  }, [devices, waitingByKey, runningByKey, reachable]);

  /**
   * 三段分组。`rows` 已经按「等你 > 在跑 > 其余」排好了，所以这里只需要在切换段的时候
   * 插一个标题 —— 不重新排序，否则两处排序规则迟早会各说一套。
   */
  const items = useMemo<Item[]>(() => {
    const shown = filter ? rows.filter((r) => r.key === filter) : rows;
    const out: Item[] = [];
    let last: Section | null = null;
    let lastIndex = -1;
    for (const row of shown) {
      const sec = sectionOf(row.session);
      if (sec !== last) {
        out.push({ kind: 'section', section: sec, count: 1 });
        last = sec;
        lastIndex = out.length - 1;
      } else {
        const head = out[lastIndex];
        if (head.kind === 'section') head.count++;
      }
      out.push({ kind: 'row', row });
    }
    return out;
  }, [rows, filter]);

  const offline = devices.filter((d) => reachable[deviceKey(d)] === false).length;
  const filtered = devices.find((d) => deviceKey(d) === filter);

  const askRename = useCallback((row: OverviewRow) => setRenaming(row), []);

  const confirmDelete = useCallback(
    (row: OverviewRow) => {
      if (row.stale) {
        Alert.alert('这台电脑现在连不上', `${row.machine} 现在连不上，删除要等它回来。`);
        return;
      }
      const s = row.session;
      Alert.alert(
        '删除这段对话？',
        (s.cron ?? 0) > 0
          ? // 任务和会话一起走：后端删掉目标会话时会把绑在它上面的任务一并删掉，所以先说出来。
            `${s.title}\n\n这个会话的 ${s.cron} 个定时任务也会一并删除。`
          : s.title,
        [
          { text: '取消', style: 'cancel' },
          { text: '删除', style: 'destructive', onPress: () => onDelete(row.device, s.id) },
        ],
      );
    },
    [onDelete],
  );

  const togglePin = useCallback(
    (row: OverviewRow) => {
      if (row.stale) {
        Alert.alert('这台电脑现在连不上', `${row.machine} 现在连不上，置顶要等它回来。`);
        return;
      }
      onPin(row.device, row.session.id, !row.session.pinned);
    },
    [onPin],
  );

  return (
    <GestureDetector gesture={page.gesture}>
      <Animated.View style={[styles.root, { backgroundColor: t.sid }, page.style]}>
        <View style={styles.head}>
          <View style={styles.headText}>
            <Text style={[styles.brand, { color: t.title }]}>所有对话</Text>
            <View style={styles.subRow}>
              <View style={[styles.onlineDot, { backgroundColor: offline > 0 ? t.amber : t.green }]} />
              <Text style={[styles.sub, { color: t.faint }]} numberOfLines={1}>
                {filtered
                  ? `${filtered.label || filtered.baseUrl} · ${items.filter((i) => i.kind === 'row').length} 段对话${
                      reachable[deviceKey(filtered)] === false ? ' · 连不上' : ''
                    }`
                  : `${devices.length} 台电脑 · ${devices.length - offline} 台在线`}
              </Text>
            </View>
            {/* 「连上了」和「读得到东西」是两件事：WebSocket 通了不代表 HTTP 请求回来了。
                这里只有真的有问题时才出现，平时不占地方。 */}
            {error ? (
              <Pressable
                onPress={onRefresh}
                style={({ pressed }) => [styles.errLine, { backgroundColor: pressed ? t.redWash : 'transparent' }]}
              >
                <Text style={{ color: t.red, fontSize: 11.5 }} numberOfLines={2}>
                  {error} · 点这里重试
                </Text>
              </Pressable>
            ) : null}
          </View>
          <Pressable
            onPress={onNew}
            accessibilityLabel="新建对话"
            style={({ pressed }) => [
              styles.gear,
              { borderColor: t.border, backgroundColor: pressed ? t.wash : 'transparent' },
            ]}
          >
            <Feather name="plus" size={18} color={t.faint} />
          </Pressable>
          <Pressable
            onPress={onSettings}
            accessibilityLabel="电脑与设置"
            style={({ pressed }) => [
              styles.gear,
              { borderColor: t.border, backgroundColor: pressed ? t.wash : 'transparent' },
            ]}
          >
            <Feather name="settings" size={17} color={t.faint} />
          </Pressable>
        </View>

        {/* 一排机器：点一下就只看那一台，再点回全部。有东西等你的那台带一个琥珀点和数字。
            「全部」固定在最左边 —— 机器一多横滚会把它推出屏幕，回来就没路了。 */}
        <View style={styles.chipsRow}>
          <Pressable
            onPress={() => onFilter(null)}
            style={[
              styles.chip,
              { borderColor: 'transparent' },
              filter === null && { backgroundColor: t.accentSoft, borderColor: t.accentLine },
            ]}
          >
            <Text style={[styles.chipText, { color: filter === null ? t.accent2 : t.dim }]}>全部</Text>
          </Pressable>
          <ScrollView
            horizontal
            showsHorizontalScrollIndicator={false}
            style={styles.chips}
            contentContainerStyle={styles.chipsInner}
          >
            {chipDevices.map(({ d, key, n }) => {
              const on = filter === key;
              const dead = reachable[key] === false;
              return (
                <Pressable
                  key={key}
                  onPress={() => onFilter(on ? null : key)}
                  style={[
                    styles.chip,
                    { borderColor: t.border },
                    on && { backgroundColor: t.accentSoft, borderColor: t.accentLine },
                    dead && { opacity: 0.45 },
                  ]}
                >
                  {n > 0 ? <View style={[styles.needDot, { backgroundColor: t.amber }]} /> : null}
                  <Text
                    style={[styles.chipText, { color: machineColor(key, isDark) }]}
                    numberOfLines={1}
                  >
                    {d.label || d.baseUrl.replace(/^https?:\/\//, '')}
                  </Text>
                  {n > 0 ? <Text style={[styles.chipCount, { color: t.amber }]}>{n}</Text> : null}
                </Pressable>
              );
            })}
          </ScrollView>
        </View>

        <FlatList
          data={items}
          keyExtractor={(it) => (it.kind === 'section' ? `s:${it.section}` : `${it.row.key}:${it.row.session.id}`)}
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
          ListEmptyComponent={
            <View style={styles.empty}>
              <Text style={{ color: t.faint, fontSize: 13 }}>
                {filter ? '这台电脑上还没有对话' : '还没有对话'}
              </Text>
            </View>
          }
          renderItem={({ item }) =>
            item.kind === 'section' ? (
              <View style={styles.sect}>
                <Text style={[styles.sectText, { color: item.section === 'waiting' ? t.amber : t.faint }]}>
                  {SECTION_LABEL[item.section]}
                </Text>
                <Text style={[styles.sectCount, { color: item.section === 'waiting' ? t.amber : t.faint }]}>
                  {item.count}
                </Text>
              </View>
            ) : (
              <SessionRow
                row={item.row}
                selected={item.row.session.id === currentId}
                machine={filter === null}
                onOpen={onOpen}
                onDelete={() => confirmDelete(item.row)}
                onPin={() => togglePin(item.row)}
                onRename={askRename}
              />
            )
          }
        />

        {/* 长按一行改名字。标题是那台电脑上的记录，这里只递交一个词：保存之后由 sessions_changed
            让每一块屏重读 —— 桌面上那一行不会停在旧名字上，这一段也不会各留一份。
            对话框与对话顶栏长按标题用的是同一个（./RenameDialog）。 */}
        {renaming ? (
          <RenameDialog
            initial={renaming.session.title}
            onCancel={() => setRenaming(null)}
            onSave={(title) => {
              const row = renaming;
              setRenaming(null);
              onRename(row.device, row.session.id, title);
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
  row,
  selected,
  machine,
  onOpen,
  onDelete,
  onPin,
  onRename,
}: {
  row: OverviewRow;
  selected: boolean;
  /** 显示机器名（筛选某台时不显示：整屏都是一台，说两遍只是噪音） */
  machine: boolean;
  onOpen: (deviceIndex: number, id: string) => void;
  onDelete: () => void;
  onPin: () => void;
  /** 长按一行：改名字。左滑只有两个动作，第三个放不下，也不该挤掉「删除」的宽度 */
  onRename: (row: OverviewRow) => void;
}) {
  const { tokens: t, isDark } = useTheme();
  const s = row.session;
  const badge = badgeOf(s);

  const actions: SwipeAction[] = [
    {
      label: s.pinned ? '取消置顶' : '置顶',
      icon: 'bookmark',
      tone: 'accent',
      onPress: onPin,
    },
    {
      label: '删除',
      icon: 'trash-2',
      tone: 'red',
      onPress: onDelete,
    },
  ];

  return (
    <View style={row.stale ? styles.stale : undefined}>
      <SwipeRow actions={actions}>
        <Pressable
          onPress={() => onOpen(row.device, s.id)}
          onLongPress={() => onRename(row)}
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
          <View style={styles.rowSubRow}>
            {machine ? (
              <>
                <Text
                  style={[styles.rowMachine, { color: machineColor(row.key, isDark) }]}
                  numberOfLines={1}
                >
                  {row.machine}
                </Text>
                <Text style={[styles.rowSub, { color: t.faint }]}>·</Text>
              </>
            ) : null}
            <Text style={[styles.rowSub, { color: t.faint }]} numberOfLines={1}>
              {s.projectName ? `${s.projectName} · ` : ''}
              {formatUpdatedAt(s.updatedAt)}
            </Text>
            {row.stale ? (
              <>
                <Text style={[styles.rowSub, { color: t.faint }]}>·</Text>
                <Text style={[styles.rowSub, { color: t.faint }]}>连不上</Text>
              </>
            ) : null}
          </View>
        </Pressable>
      </SwipeRow>
    </View>
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

  chipsRow: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingLeft: 16, paddingBottom: 8 },
  // 机器的部分自己横滚；「全部」在它外面，固定不动
  chips: { flexGrow: 0, flexShrink: 1 },
  chipsInner: { gap: 7, alignItems: 'center', paddingRight: 16 },
  chip: {
    height: 30,
    borderRadius: 15,
    paddingHorizontal: 12,
    borderWidth: 1,
    borderColor: 'transparent',
    flexDirection: 'row',
    alignItems: 'center',
    gap: 5,
    maxWidth: 180,
  },
  chipText: { fontSize: 12.5 },
  chipCount: { fontSize: 11 },
  needDot: { width: 6, height: 6, borderRadius: 3 },

  listContent: { paddingHorizontal: 8, paddingTop: 2, paddingBottom: 16 },
  empty: { alignItems: 'center', paddingTop: 48 },
  sect: { flexDirection: 'row', alignItems: 'baseline', gap: 7, paddingTop: 12, paddingHorizontal: 10, paddingBottom: 4 },
  sectText: { fontSize: 11.5, fontWeight: '600', letterSpacing: 0.4 },
  sectCount: { fontSize: 11, opacity: 0.75 },

  swipeWrap: { borderRadius: 10, overflow: 'hidden', marginVertical: 2 },
  actionLayer: { position: 'absolute', right: 0, top: 0, bottom: 0, flexDirection: 'row' },
  actionBtn: { width: ACTION_WIDTH, alignItems: 'center', justifyContent: 'center' },
  row: { borderRadius: 10, borderWidth: 1, borderColor: 'transparent', paddingTop: 12, paddingHorizontal: 12, paddingBottom: 11 },
  rowTop: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  rowTitle: { flex: 1, minWidth: 0, fontSize: 14.5, fontWeight: '600' },
  rowSubRow: { flexDirection: 'row', alignItems: 'center', gap: 5, marginTop: 4 },
  rowMachine: { fontSize: 12, fontWeight: '500', maxWidth: 120 },
  rowSub: { fontSize: 12 },
  // 连不上的那台：整行淡下去。状态标记照旧画，但那可能是几小时前的
  stale: { opacity: 0.5 },
  sourcePill: { borderWidth: 1, borderRadius: 20, paddingHorizontal: 6, paddingVertical: 1, maxWidth: 88 },
  // 闹钟那一条：图标 + 条数并排，比渠道那个窄很多
  cronPill: { flexDirection: 'row', alignItems: 'center', gap: 3, paddingHorizontal: 5, maxWidth: undefined },
  sourcePillText: { fontSize: 10.5 },

  pill: { flexDirection: 'row', alignItems: 'center', gap: 5, borderWidth: 1, borderColor: 'transparent', borderRadius: 20, paddingHorizontal: 7, paddingVertical: 2 },
  pillText: { fontSize: 10.5 },
  liveDot: { width: 5, height: 5, borderRadius: 2.5 },
});
