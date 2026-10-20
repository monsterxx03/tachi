import { Feather } from '@expo/vector-icons';
import { useMemo, useRef, useState } from 'react';
import {
  ActivityIndicator,
  FlatList,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import type { FileSource } from '../api/client';
import type { AttachmentRef, PendingAsk, SendMode, SessionMessage, Snapshot } from '../api/types';
import type { Live } from '../state/mobile';
import { buildItems, type Item, type ProcessStep } from '../state/items';
import { useEdgeBack } from './edgeBack';
import { GestureDetector } from 'react-native-gesture-handler';
import Animated from 'react-native-reanimated';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';
import { MessageView, Thinking, ToolCard } from './Message';
import { Markdown } from './Markdown';

/** 细条：这一段对话花了多少、跑多快、上下文占了多少、用的哪个模型 */
function MetaStrip({
  snapshot,
  live,
  onOpenInfo,
  onOpenModels,
}: {
  snapshot: Snapshot;
  live: Live;
  onOpenInfo: () => void;
  onOpenModels: () => void;
}) {
  const { tokens: t } = useTheme();
  const { usage, context, model, session } = snapshot;
  const ratio = context.window > 0 ? context.estimate / context.window : 0;
  const ctxColor = ratio >= 0.9 ? t.red : ratio >= 0.75 ? t.amber : t.faint;
  const streaming = session.running;
  // 停下来的那一轮保留最后一次速度（桌面同一条规矩：工具边界之后变暗），
  // 否则每轮结束后这里都退回「—」，看着像从没测到过。
  const tps = live.tps || live.lastTps;

  return (
    <View style={[styles.strip, { borderBottomColor: t.border, backgroundColor: t.stripBg }]}>
      <Text style={[styles.money, { color: t.dim }]}>¥{usage.cost.toFixed(3)}</Text>
      <Text style={[styles.sep, { color: t.faint }]}>·</Text>
      <View style={[styles.tps, { opacity: streaming ? 1 : 0.62 }]}>
        <View style={[styles.tpsDot, { backgroundColor: streaming ? t.green : t.faint }]} />
        <Text style={{ color: streaming ? t.dim : t.faint, fontSize: 11.5 }}>
          {tps ? `${Math.round(tps)} tok/s` : '—'}
        </Text>
      </View>
      <Text style={[styles.sep, { color: t.faint }]}>·</Text>
      <Pressable onPress={onOpenInfo} style={styles.ctx}>
        <View style={[styles.ctxBar, { backgroundColor: t.borderStrong }]}>
          <View style={[styles.ctxFill, { width: `${Math.round(ratio * 100)}%`, backgroundColor: ctxColor }]} />
        </View>
        <Text style={{ color: ctxColor, fontSize: 11.5 }}>{Math.round(ratio * 100)}%</Text>
      </Pressable>

      {snapshot.skipApproval ? (
        <View style={[styles.skipChip, { borderColor: t.amberLine, backgroundColor: t.amberWash }]}>
          <Text style={{ color: t.amber, fontSize: 10.5 }}>跳过审批</Text>
        </View>
      ) : null}

      <View style={styles.spacer} />

      {session.readOnly ? (
        <Text style={[styles.chip, { color: t.faint, borderColor: t.border }]}>{model}</Text>
      ) : (
        <Pressable
          onPress={onOpenModels}
          style={({ pressed }) => [
            styles.chipBtn,
            { borderColor: t.borderStrong, backgroundColor: pressed ? t.wash : 'transparent' },
          ]}
        >
          <Text style={{ color: t.dim, fontSize: 11.5 }}>{model}</Text>
        </Pressable>
      )}
      <Pressable onPress={onOpenInfo} style={styles.iconBtn}>
        <Feather name="more-horizontal" size={16} color={t.faint} />
      </Pressable>
    </View>
  );
}

/**
 * 底部：正在跑的时候多一行状态，发出去的话先进「待发送队列」（桌面的 pending 的手机版）。
 *
 * 运行中直接把这句发出去是错的：它会被当成插话立刻塞进正在流的这一轮，读者看到的是
 * 「已经发出去了」，而桌面那边是排队、在本轮工具调用结束后再插入。所以这里只有两件事：
 * 运行中发送 = 排队；「立即发送 · 打断」= 先停这一轮，等它真的结束了再把队列发出去。
 */
function Composer({
  running,
  waiting,
  pending,
  onSend,
  onQueue,
  onDropPending,
  onClearPending,
  onSendPendingNow,
  onStop,
}: {
  running: boolean;
  waiting: string;
  pending: string[];
  onSend: (text: string, mode: SendMode) => Promise<boolean>;
  onQueue: (text: string) => void;
  onDropPending: (index: number) => void;
  onClearPending: () => void;
  onSendPendingNow: () => void;
  onStop: () => void;
}) {
  const { tokens: t } = useTheme();
  const [text, setText] = useState('');
  const short = waiting === '等待你的授权' ? '等你授权' : waiting === '等待你的回答' ? '等你回答' : '';

  const submit = () => {
    const v = text.trim();
    if (!v) {
      // 空着按发送 = 停掉这一轮（正在跑的时候才有意义）
      if (running) onStop();
      return;
    }
    if (running) {
      onQueue(v);
      setText('');
      return;
    }
    // 不在跑：服务端没收下就把字还给读者（拒绝的原因在顶部横幅里）
    void onSend(v, 'send').then((ok) => {
      if (ok) setText('');
    });
  };

  return (
    <View style={[styles.composer, { borderTopColor: t.border, backgroundColor: t.composerBg }]}>
      {pending.length > 0 ? (
        <View style={[styles.pendingBar, styles.column, { borderBottomColor: t.border }]}>
          <View style={styles.pendingHead}>
            <Text style={{ color: t.faint, fontSize: 11.5, flex: 1 }} numberOfLines={1}>
              {running ? '待发送 · 本轮工具调用结束后自动插入' : '待发送消息'}
            </Text>
            <Pressable onPress={onSendPendingNow}>
              <Text style={{ color: t.accent2, fontSize: 11.5 }}>
                {running ? '立即发送 · 打断' : '发送'}
              </Text>
            </Pressable>
            <Pressable onPress={onClearPending}>
              <Text style={{ color: t.faint, fontSize: 11.5 }}>清空</Text>
            </Pressable>
          </View>
          {pending.map((p, i) => (
            <View key={`${i}-${p.slice(0, 8)}`} style={styles.pendingItem}>
              <Text style={{ color: t.dim, fontSize: 12.5, flex: 1 }} numberOfLines={2}>
                {p}
              </Text>
              <Pressable onPress={() => onDropPending(i)} hitSlop={8}>
                <Feather name="x" size={13} color={t.faint} />
              </Pressable>
            </View>
          ))}
        </View>
      ) : null}

      {running ? (
        <View style={[styles.runHint, styles.column]}>
          <View style={[styles.runDot, { backgroundColor: t.amber }]} />
          <Text style={{ color: t.faint, fontSize: 11.5 }} numberOfLines={1}>
            {short ? `停在这里${short}` : '正在跑 · 发送会排队'}
          </Text>
          <View style={styles.spacer} />
          <Pressable onPress={onStop}>
            <Text style={{ color: t.accent2, fontSize: 11.5 }}>打断</Text>
          </Pressable>
        </View>
      ) : null}
      <View style={[styles.inputRow, styles.column]}>
        <View style={[styles.inputWrap, { backgroundColor: t.inputBg, borderColor: t.border }]}>
          <TextInput
            value={text}
            onChangeText={setText}
            placeholder="补一句…"
            placeholderTextColor={t.faint}
            style={[styles.input, { color: t.text }]}
            multiline
          />
        </View>
        <Pressable
          onPress={submit}
          // 只有图标：读屏和自动化都要靠这个名字认它
          accessibilityRole="button"
          accessibilityLabel={running && !text.trim() ? '停止' : '发送'}
          style={({ pressed }) => [styles.sendBtn, { backgroundColor: pressed || !text.trim() ? t.accentStrong : t.accent }]}
        >
          <Feather name={running && !text.trim() ? 'square' : 'arrow-up'} size={17} color={t.onAccent} />
        </Pressable>
      </View>
    </View>
  );
}

/**
 * 一轮的过程：一行概括，点开按记录里的先后展开（桌面的「N 步」那一行的手机版）。
 *
 * 折进去的是思考与成功的工具调用；正文和失败的那一步留在外面。展开时思考在前、工具
 * 紧跟其后，顺序就是它们真正发生的顺序 —— 所以这里不要按种类分组。
 */
function ProcessFold({ steps, running }: { steps: ProcessStep[]; running?: boolean }) {
  const { tokens: t } = useTheme();
  const [open, setOpen] = useState(false);
  const tools = steps.filter((s): s is Extract<ProcessStep, { kind: 'tool' }> => s.kind === 'tool');
  const thinkings = steps.filter((s): s is Extract<ProcessStep, { kind: 'thinking' }> => s.kind === 'thinking');
  const names = tools.map((x) => x.pair.msg.toolName ?? '').filter(Boolean);
  const unique = [...new Set(names)];
  const label = tools.length > 0 ? `${tools.length} 步` : `思考 ${thinkings.length} 段`;
  const summary =
    unique.length === 0
      ? ''
      : unique.length > 2
        ? `${unique.slice(0, 2).join('、')} 等`
        : unique.join('、');

  return (
    <View style={[styles.tool, { borderColor: t.border, backgroundColor: t.wash }]}>
      <Pressable onPress={() => setOpen((v) => !v)} style={styles.toolHead}>
        <Feather name={open ? 'chevron-up' : 'chevron-down'} size={13} color={t.faint} style={{ marginTop: 1 }} />
        <Text style={[styles.toolName, { color: t.dim }]}>{label}</Text>
        {running ? <Text style={{ color: t.accent2, fontSize: 11.5 }}>· 在跑</Text> : null}
        {summary ? (
          <Text style={[styles.toolSum, { color: t.faint }]} numberOfLines={1}>
            {summary}
          </Text>
        ) : null}
      </Pressable>
      {open ? (
        <View style={{ paddingBottom: 4, paddingTop: 2 }}>
          {steps.map((step) =>
            step.kind === 'thinking' ? (
              <View key={step.key} style={{ paddingHorizontal: 8 }}>
                <Thinking text={step.text} />
              </View>
            ) : (
              <View key={step.key} style={{ paddingHorizontal: 4 }}>
                <ToolCard
                  name={step.pair.msg.toolName ?? ''}
                  title={step.pair.msg.title}
                  args={step.pair.msg.args}
                  result={step.pair.result?.toolResult}
                  isError={step.pair.result?.isError}
                  running={running === true && step.pair.result === undefined}
                />
              </View>
            ),
          )}
        </View>
      ) : null}
    </View>
  );
}

/**
 * 活气泡：这一轮**正在写的字**。思考和工具已经由 items.ts 折成过程那一行了 —— 两边各画
 * 一套的话，跑着的时候工具一个个露在外面、停下来才折，画面会在轮次结束时整段跳一下。
 */
function LiveBubble({ live }: { live: Live }) {
  if (!live.text) return null;
  return (
    <View style={styles.live}>
      <Markdown text={live.text} />
    </View>
  );
}

export function Chat({
  snapshot,
  live,
  fileSource,
  onBack,
  onToggleList,
  onDecide,
  onAnswer,
  onDecline,
  onOpenInfo,
  onOpenModels,
  onOpenFile,
  onSend,
  onStop,
  onLoadOlder,
  olderLoading,
  onEdgeBack,
  pending,
  onQueue,
  onDropPending,
  onClearPending,
  onSendPendingNow,
}: {
  snapshot: Snapshot;
  live: Live;
  fileSource?: (a: AttachmentRef) => FileSource | null;
  onBack: () => void;
  /** 宽屏：收起 / 展开左边那栏。没有它（窄屏）时 header 显示返回键。 */
  onToggleList?: () => void;
  onDecide: (toolId: string, allow: boolean) => void;
  /** 键是问题原文、值用 ", " 连接（协议里的答案形状） */
  onAnswer: (answers: Record<string, string>) => void;
  /** 「跳过」= decline，不是「交一份空答案」 */
  onDecline: () => void;
  onOpenInfo: () => void;
  onOpenModels: () => void;
  onOpenFile: (id: string) => void;
  onSend: (text: string, mode: SendMode) => Promise<boolean>;
  onStop: () => void;
  /** 上滑到头时翻一页更早的历史 */
  onLoadOlder: () => void;
  /** 一页更早的历史正在读 */
  olderLoading: boolean;
  /**
   * 从屏幕左缘往右划：窄屏=回列表，宽屏侧栏收起=把列表拉回来。
   * 只认「起点在左缘 24pt 内」的手势，否则会跟消息里的横向操作打架。
   */
  onEdgeBack?: () => void;
  /** 待发送队列（运行中发的消息）；渲染在输入框上方 */
  pending: string[];
  onQueue: (text: string) => void;
  onDropPending: (index: number) => void;
  onClearPending: () => void;
  onSendPendingNow: () => void;
}) {
  const { tokens: t } = useTheme();
  const s = snapshot.session;
  const items = useMemo(() => buildItems(snapshot, live), [snapshot, live]);
  // FlatList 是 inverted 的：数据倒过来喂，视觉上第一条在底部
  const reversed = useMemo(() => items.slice().reverse(), [items]);
  // 一页在飞的时候不要再要一页：滚动事件一秒能来几十次，而服务端那一页是真的要读盘的
  const wantOlder = () => {
    if (!snapshot.hasMore || olderLoading) return;
    onLoadOlder();
  };

  const back = useEdgeBack(onEdgeBack);
  // inverted 列表本来就贴着最新那条；这个 ref 只留给出错时手动定位，不参与常规滚动
  const listRef = useRef<FlatList<Item>>(null);

  return (
    <GestureDetector gesture={back.gesture}>
      <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable
          onPress={onToggleList ?? onBack}
          style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}
        >
          <Feather name={onToggleList ? 'menu' : 'chevron-left'} size={onToggleList ? 19 : 22} color={t.dim} />
        </Pressable>
        <View style={styles.headInfo}>
          <Text style={[styles.headTitle, { color: t.title }]} numberOfLines={1}>
            {s.title}
          </Text>
          <Text style={[styles.headSub, { color: t.faint }]} numberOfLines={1}>
            {s.projectName ? `${s.projectName} · ` : ''}
            {s.readOnly ? '来自别的任务' : s.running ? '正在跑' : '空闲'}
          </Text>
        </View>
        <StatePill session={s} t={t} />
      </View>

      <MetaStrip snapshot={snapshot} live={live} onOpenInfo={onOpenInfo} onOpenModels={onOpenModels} />

      <FlatList
        ref={listRef}
        data={reversed}
        keyExtractor={(it) => it.key}
        contentContainerStyle={[styles.messages, styles.column]}
        // inverted：最新的一条在底部，偏移 0 就是「贴底」。这样打开会话自然停在最新处，
        // 往上翻出来的一页接在数组末尾（视觉上是顶部），画面一动不动 —— 不用去补偿滚动位置。
        inverted
        onEndReached={wantOlder}
        onEndReachedThreshold={0.2}
        ListFooterComponent={
          olderLoading ? (
            <View style={styles.olderHead}>
              <ActivityIndicator size="small" color={t.faint} />
              <Text style={{ color: t.faint, fontSize: 11.5 }}>正在读更早的消息…</Text>
            </View>
          ) : snapshot.hasMore ? (
            <Pressable onPress={wantOlder} style={styles.olderHead}>
              <Feather name="chevron-up" size={13} color={t.faint} />
              <Text style={{ color: t.faint, fontSize: 11.5 }}>上滑（或点这里）看更早的消息</Text>
            </Pressable>
          ) : undefined
        }
        renderItem={({ item }) =>
          item.kind === 'live' ? (
            <LiveBubble live={live} />
          ) : item.kind === 'process' ? (
            <ProcessFold steps={item.steps} running={item.running} />
          ) : (
            <MessageView
              msg={item.msg}
              ask={item.ask}
              toolResult={item.result}
              running={item.running}
              attachments={snapshot.attachments}
              fileSource={fileSource}
              onDecide={(allow) => item.ask && onDecide(item.ask.toolId, allow)}
              onAnswer={(answers) => onAnswer(answers)}
              onDecline={() => onDecline()}
              onOpenFile={onOpenFile}
            />
          )
        }
      />

      {s.readOnly ? (
        <View style={[styles.readOnlyNote, { borderTopColor: t.border }]}>
          <Text style={{ color: t.faint, fontSize: 12.5 }}>这段对话由别的任务占用，只能查看</Text>
        </View>
      ) : (
        <Composer
          running={s.running}
          waiting={s.waiting}
          pending={pending}
          onSend={onSend}
          onQueue={onQueue}
          onDropPending={onDropPending}
          onClearPending={onClearPending}
          onSendPendingNow={onSendPendingNow}
          onStop={onStop}
        />
      )}
      </Animated.View>
    </GestureDetector>
  );
}

function StatePill({ session, t }: { session: Snapshot['session']; t: ThemeTokens }) {
  const label = session.readOnly
    ? '只读'
    : session.waiting === '等待你的授权'
      ? '等你授权'
      : session.waiting === '等待你的回答'
        ? '等你回答'
        : session.running
          ? '在跑'
          : '空闲';
  const tone =
    session.waiting !== ''
      ? { color: t.amber, borderColor: t.amberLine, backgroundColor: t.amberWash }
      : session.running
        ? { color: t.accent2, borderColor: t.accentLine, backgroundColor: t.accentSoft }
        : { color: t.faint, borderColor: t.border };
  return (
    <View style={[styles.statePill, tone]}>
      <Text style={{ color: tone.color, fontSize: 11 }}>{label}</Text>
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
  headInfo: { flex: 1, minWidth: 0, paddingHorizontal: 4 },
  headTitle: { fontSize: 14.5, fontWeight: '600' },
  headSub: { fontSize: 11.5, marginTop: 2 },
  statePill: { borderWidth: 1, borderColor: 'transparent', borderRadius: 20, paddingHorizontal: 9, paddingVertical: 3, marginRight: 4 },

  strip: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 7,
    paddingHorizontal: 14,
    paddingVertical: 6,
    minHeight: 34,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  money: { fontSize: 11.5, fontWeight: '600' },
  sep: { fontSize: 11.5, opacity: 0.45 },
  tps: { flexDirection: 'row', alignItems: 'center', gap: 5 },
  tpsDot: { width: 5, height: 5, borderRadius: 2.5 },
  ctx: { flexDirection: 'row', alignItems: 'center', gap: 5 },
  ctxBar: { width: 26, height: 4, borderRadius: 2, overflow: 'hidden' },
  ctxFill: { height: 4, borderRadius: 2 },
  skipChip: { borderWidth: 1, borderRadius: 20, paddingHorizontal: 7, paddingVertical: 2 },
  spacer: { flex: 1 },
  chip: { fontSize: 11.5, borderWidth: 1, borderRadius: 20, paddingHorizontal: 9, paddingVertical: 3 },
  chipBtn: { borderWidth: 1, borderRadius: 20, paddingHorizontal: 9, paddingVertical: 3 },
  iconBtn: { width: 26, height: 26, borderRadius: 7, alignItems: 'center', justifyContent: 'center' },

  messages: { paddingHorizontal: 16, paddingTop: 8, paddingBottom: 20 },
  olderHead: { flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 7, paddingVertical: 10 },
  // 平板那种宽屏下，一行别横跨整屏 —— 读一句话要来回甩头
  column: { width: '100%', maxWidth: 760, alignSelf: 'center' },
  live: { paddingVertical: 4 },
  liveText: { fontSize: 15, lineHeight: 25 },
  think: { borderLeftWidth: 2, paddingLeft: 10, marginVertical: 6, opacity: 0.9 },
  tool: { borderWidth: 1, borderRadius: 11, marginVertical: 7, overflow: 'hidden' },
  toolHead: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingHorizontal: 10, paddingVertical: 9 },
  toolName: { fontSize: 12.5, fontWeight: '600' },
  toolSum: { flex: 1, fontSize: 12, fontFamily: 'monospace' },
  toolBody: { borderTopWidth: StyleSheet.hairlineWidth, padding: 10 },

  composer: { borderTopWidth: StyleSheet.hairlineWidth, paddingTop: 6, paddingBottom: 8 },
  pendingBar: { paddingHorizontal: 12, paddingBottom: 6 },
  pendingHead: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingBottom: 5, paddingTop: 2 },
  pendingItem: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 3 },
  runHint: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingHorizontal: 16, paddingBottom: 6 },
  runDot: { width: 5, height: 5, borderRadius: 2.5 },
  inputRow: { flexDirection: 'row', alignItems: 'center', gap: 9, paddingHorizontal: 12 },
  inputWrap: { flex: 1, borderWidth: 1, borderRadius: 20, paddingHorizontal: 15, paddingVertical: 11 },
  input: { fontSize: 14, padding: 0, maxHeight: 90 },
  sendBtn: { width: 38, height: 38, borderRadius: 19, alignItems: 'center', justifyContent: 'center' },
  readOnlyNote: { borderTopWidth: StyleSheet.hairlineWidth, padding: 14, alignItems: 'center' },
});
