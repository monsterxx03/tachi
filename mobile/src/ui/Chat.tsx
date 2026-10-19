import { Feather } from '@expo/vector-icons';
import { useMemo, useState } from 'react';
import { FlatList, Pressable, StyleSheet, Text, TextInput, View } from 'react-native';
import type { FileSource } from '../api/client';
import type { AttachmentRef, PendingAsk, SendMode, SessionMessage, Snapshot } from '../api/types';
import type { Live } from '../state/mobile';
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

  return (
    <View style={[styles.strip, { borderBottomColor: t.border, backgroundColor: t.stripBg }]}>
      <Text style={[styles.money, { color: t.dim }]}>¥{usage.cost.toFixed(3)}</Text>
      <Text style={[styles.sep, { color: t.faint }]}>·</Text>
      <View style={[styles.tps, { opacity: streaming ? 1 : 0.62 }]}>
        <View style={[styles.tpsDot, { backgroundColor: streaming ? t.green : t.faint }]} />
        <Text style={{ color: streaming ? t.dim : t.faint, fontSize: 11.5 }}>
          {live.tps ? `${Math.round(live.tps)} tok/s` : '—'}
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

/** 底部：正在跑的时候多一行提示（插话 / 打断），只读会话换成一条说明 */
function Composer({
  running,
  waiting,
  onSend,
  onStop,
}: {
  running: boolean;
  waiting: string;
  onSend: (text: string, mode: SendMode) => Promise<boolean>;
  onStop: () => void;
}) {
  const { tokens: t } = useTheme();
  const [text, setText] = useState('');
  const short = waiting === '等待你的授权' ? '等你授权' : waiting === '等待你的回答' ? '等你回答' : '';

  return (
    <View style={[styles.composer, { borderTopColor: t.border, backgroundColor: t.composerBg }]}>
      {running ? (
        <View style={[styles.runHint, styles.column]}>
          <View style={[styles.runDot, { backgroundColor: t.amber }]} />
          <Text style={{ color: t.faint, fontSize: 11.5 }} numberOfLines={1}>
            {short ? `停在这里${short}` : '正在跑，可以插话'}
          </Text>
          <View style={styles.spacer} />
          <Pressable onPress={onStop}>
            <Text style={{ color: t.accent2, fontSize: 11.5 }}>立即发送 · 打断</Text>
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
          onPress={() => {
            const v = text.trim();
            if (!v) return;
            // Only clear once the server took it: a refusal is explained by the banner, and
            // taking the reader's words with it makes them retype what they were just told.
            void onSend(v, running ? 'steer' : 'send').then((ok) => {
              if (ok) setText('');
            });
          }}
          style={({ pressed }) => [styles.sendBtn, { backgroundColor: pressed || !text.trim() ? t.accentStrong : t.accent }]}
        >
          <Feather name="arrow-up" size={17} color={t.onAccent} />
        </Pressable>
      </View>
    </View>
  );
}

/** 活气泡：这一轮正在流的东西。轮次一结束就被记录替换掉。 */
function LiveBubble({ live }: { live: Live }) {
  if (!live.active && !live.text && live.tools.length === 0) return null;
  return (
    <View style={styles.live}>
      {live.thinking ? <Thinking text={live.thinking} /> : null}
      {live.tools.map((tool) => (
        <ToolCard
          key={tool.id}
          name={tool.name}
          title={tool.title}
          args={tool.args}
          result={tool.result}
          isError={tool.isError}
          running={!tool.result}
        />
      ))}
      {live.text ? <Markdown text={live.text} /> : null}
    </View>
  );
}

type Item =
  | { kind: 'msg'; key: string; msg: SessionMessage; ask?: PendingAsk; result?: SessionMessage }
  | { kind: 'live'; key: string };

function buildItems(snapshot: Snapshot, live: Live): Item[] {
  const results = new Map<string, SessionMessage>();
  for (const m of snapshot.messages) {
    if (m.role === 'tool_result' && m.toolCallId) results.set(m.toolCallId, m);
  }
  const items: Item[] = [];
  snapshot.messages.forEach((m, i) => {
    if (m.role === 'tool_result') return; // 跟着它的 tool_call 一起画
    items.push({ kind: 'msg', key: `m${i}`, msg: m, result: m.toolCallId ? results.get(m.toolCallId) : undefined });
  });
  snapshot.asks.forEach((ask, i) => items.push({ kind: 'msg', key: `ask${i}`, msg: snapshot.messages[0], ask }));
  if (live.active || live.text || live.tools.length > 0) items.push({ kind: 'live', key: 'live' });
  return items;
}

export function Chat({
  snapshot,
  live,
  fileSource,
  onBack,
  onToggleList,
  onDecide,
  onAnswer,
  onOpenInfo,
  onOpenModels,
  onOpenFile,
  onSend,
  onStop,
}: {
  snapshot: Snapshot;
  live: Live;
  fileSource?: (a: AttachmentRef) => FileSource | null;
  onBack: () => void;
  /** 宽屏：收起 / 展开左边那栏。没有它（窄屏）时 header 显示返回键。 */
  onToggleList?: () => void;
  onDecide: (toolId: string, allow: boolean) => void;
  onAnswer: (toolId: string, text: string) => void;
  onOpenInfo: () => void;
  onOpenModels: () => void;
  onOpenFile: (id: string) => void;
  onSend: (text: string, mode: SendMode) => Promise<boolean>;
  onStop: () => void;
}) {
  const { tokens: t } = useTheme();
  const s = snapshot.session;
  const items = useMemo(() => buildItems(snapshot, live), [snapshot, live]);

  return (
    <View style={[styles.root, { backgroundColor: t.bg }]}>
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
        data={items}
        keyExtractor={(it) => it.key}
        contentContainerStyle={[styles.messages, styles.column]}
        renderItem={({ item }) =>
          item.kind === 'live' ? (
            <LiveBubble live={live} />
          ) : (
            <MessageView
              msg={item.msg}
              ask={item.ask}
              toolResult={item.result}
              attachments={snapshot.attachments}
              fileSource={fileSource}
              onDecide={(allow) => item.ask && onDecide(item.ask.toolId, allow)}
              onAnswer={(text) => item.ask && onAnswer(item.ask.toolId, text)}
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
        <Composer running={s.running} waiting={s.waiting} onSend={onSend} onStop={onStop} />
      )}
    </View>
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
  runHint: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingHorizontal: 16, paddingBottom: 6 },
  runDot: { width: 5, height: 5, borderRadius: 2.5 },
  inputRow: { flexDirection: 'row', alignItems: 'center', gap: 9, paddingHorizontal: 12 },
  inputWrap: { flex: 1, borderWidth: 1, borderRadius: 20, paddingHorizontal: 15, paddingVertical: 11 },
  input: { fontSize: 14, padding: 0, maxHeight: 90 },
  sendBtn: { width: 38, height: 38, borderRadius: 19, alignItems: 'center', justifyContent: 'center' },
  readOnlyNote: { borderTopWidth: StyleSheet.hairlineWidth, padding: 14, alignItems: 'center' },
});
