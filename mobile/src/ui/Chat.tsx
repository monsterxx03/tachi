import { Feather } from '@expo/vector-icons';
import { useCallback, useEffect, useMemo, useRef, useState, type ComponentRef } from 'react';
import {
  ActivityIndicator,
  FlatList,
  type NativeScrollEvent,
  type NativeSyntheticEvent,
  Modal,
  Platform,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import type { FileSource } from '../api/client';
import { pickDocument, pickPhoto, type PickedFile } from '../api/pick';
import type {
  AgentPanelRow,
  AttachmentRef,
  PendingAsk,
  PendingMessage,
  SendMode,
  SessionMessage,
  SessionSummary,
  Snapshot,
  UploadedFile,
} from '../api/types';
import type { Live } from '../state/mobile';
import { buildAskItems, buildItems, buildLiveTail, type Item, type ProcessStep } from '../state/items';
import { agentRef, hashQuery, hashRefAt, matchAgents, matchSessions, sessionRef, spliceRef } from './refs';
import { EdgeBackZone, useEdgeBack } from './edgeBack';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withRepeat,
  withTiming,
} from 'react-native-reanimated';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';
import { MessageView, Thinking, ToolCard } from './Message';
import { Markdown } from './Markdown';
import { RenameDialog } from './RenameDialog';

/**
 * 离底部多远才算「滚上去读历史了」。一屏的一半上下：比这更近时往回滑两下就到底了，
 * 浮出胶囊反而挡消息。
 */
const STAY_AWAY = 320;
/** 两次点击间隔在这以内算双击（回到底部） */
const DOUBLE_TAP = 300;

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
 * 停止按钮：和桌面那个一模一样 —— 一圈红色在转，中间一个小方块。
 *
 * 桌面把「正在生成、点这里停」做成了一个转动的圆环（`chat.css` 的 `.stop-btn`），因为一个静止的
 * 方块读起来像「发一条」；手机这里跟着同一个形状，就不必再写一行「打断」的文字去解释它。
 * 环转在 UI 线程上（reanimated 的共享值），JS 线程正被这一轮的事件占着也照样匀速。
 */
function StopRing({ t }: { t: ThemeTokens }) {
  const spin = useSharedValue(0);
  useEffect(() => {
    spin.value = withRepeat(withTiming(360, { duration: 800, easing: Easing.linear }), -1, false);
  }, [spin]);
  const ring = useAnimatedStyle(() => ({ transform: [{ rotate: `${spin.value}deg` }] }));
  return (
    <View style={[styles.stopBtn, { borderColor: t.redLine, backgroundColor: t.panel }]}>
      <Animated.View
        style={[styles.stopRing, { borderTopColor: t.red, borderRightColor: t.redLine }, ring]}
      />
      <View style={[styles.stopSquare, { backgroundColor: t.red }]} />
    </View>
  );
}

/** 输入框里挂着的一个待发文件：传完的带 file，正在传的没有，失败的那条带 error。 */
type Attach = { key: string; name: string; size?: number; file?: UploadedFile; error?: string };

/** 待发送那一条：往上划够这么远 = 现在就发，往下划够这么远 = 删掉 */
const PENDING_SWIPE = 26;

/** `#s:` 候选最多列几行：手机屏幕就那么大，够不着的那几条不如让读者多打一个字。 */
const REF_PICK_LIMIT = 6;

/**
 * 待发送队列里的一条。**上滑 = 现在就发这一条**（正在跑就先停这一轮，见 state 的
 * `sendPending`），**下滑 = 从队列里删掉**。右边那个 × 留着：手势要有人解释，它不用。
 *
 * 跟手时整行换底色（上 = 发出去，下 = 删掉），松手前就知道会发生什么；松手过了阈值就顺着
 * 那个方向滑出去再落地，没过就弹回来。
 */
function PendingRow({
  text,
  onSendNow,
  onDrop,
}: {
  text: string;
  onSendNow: () => void;
  onDrop: () => void;
}) {
  const { tokens: t } = useTheme();
  const ty = useSharedValue(0);
  // -1 = 往上是「发」，1 = 往下是「删」，0 = 没动
  const dir = useSharedValue(0);

  const pan = useMemo(
    () =>
      Gesture.Pan()
        // 竖着 10px 才认领；横着先动 16px 就放弃（这里没有横向的事要做）
        .activeOffsetY([-10, 10])
        .failOffsetX([-16, 16])
        .onUpdate((e) => {
          ty.value = e.translationY;
          dir.value = e.translationY < 0 ? -1 : 1;
        })
        .onEnd((e) => {
          const send = ty.value < -PENDING_SWIPE || e.velocityY < -700;
          const drop = ty.value > PENDING_SWIPE || e.velocityY > 700;
          if (send) {
            ty.value = withTiming(-64, { duration: 150, easing: Easing.out(Easing.quad) }, () => {
              runOnJS(onSendNow)();
            });
            return;
          }
          if (drop) {
            ty.value = withTiming(64, { duration: 150, easing: Easing.out(Easing.quad) }, () => {
              runOnJS(onDrop)();
            });
            return;
          }
          ty.value = withTiming(0, { duration: 140, easing: Easing.out(Easing.cubic) });
          dir.value = 0;
        })
        // 被别处接管：归位，别停在半路
        .onFinalize((_e, success) => {
          if (success) return;
          ty.value = withTiming(0, { duration: 120, easing: Easing.out(Easing.cubic) });
          dir.value = 0;
        }),
    [dir, onDrop, onSendNow, ty],
  );

  const style = useAnimatedStyle(() => ({
    transform: [{ translateY: ty.value }],
    opacity: 1 - Math.min(1, Math.abs(ty.value) / 70),
    backgroundColor: dir.value === 0 ? 'transparent' : dir.value < 0 ? t.accentSoft : t.redWash,
  }));

  return (
    <GestureDetector gesture={pan}>
      <Animated.View
        style={[styles.pendingItem, styles.pendingRow, style]}
        accessibilityHint="上滑立即发送，下滑删掉"
      >
        <Text style={{ color: t.dim, fontSize: 12.5, flex: 1 }} numberOfLines={2}>
          {text}
        </Text>
        <Pressable onPress={onDrop} hitSlop={8}>
          <Feather name="x" size={13} color={t.faint} />
        </Pressable>
      </Animated.View>
    </GestureDetector>
  );
}

/** 回形针展开的三个入口之一。 */
function AttachItem({ icon, label, onPress }: { icon: keyof typeof Feather.glyphMap; label: string; onPress: () => void }) {
  const { tokens: t } = useTheme();
  return (
    <Pressable
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={label}
      style={({ pressed }) => [styles.attachItem, pressed && { backgroundColor: t.wash }]}
    >
      <Feather name={icon} size={15} color={t.dim} />
      <Text style={{ color: t.dim, fontSize: 12.5 }}>{label}</Text>
    </Pressable>
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
  onUpload,
  onQueue,
  onDropPending,
  onSendPending,
  onClearPending,
  onSendPendingNow,
  onStop,
  sessions,
  currentId,
  loadAgents,
}: {
  running: boolean;
  waiting: string;
  pending: PendingMessage[];
  onSend: (text: string, mode: SendMode, files?: UploadedFile[]) => Promise<boolean>;
  onUpload: (f: PickedFile) => Promise<{ file?: UploadedFile; error?: string }>;
  onQueue: (text: string, files?: UploadedFile[]) => void;
  onDropPending: (index: number) => void;
  /** 队列里的一条「现在就发」（上滑） */
  onSendPending: (index: number) => void;
  onClearPending: () => void;
  onSendPendingNow: () => void;
  onStop: () => void;
  /** `#s:` 的候选：所有对话 */
  sessions: SessionSummary[];
  /** 正在写的这一段：引用自己不会展开出任何有用的东西，所以它不出现在候选里 */
  currentId: string;
  /** `#a:` 的候选：向那台电脑要一次节点面板（一次会话里不会变） */
  loadAgents: () => Promise<AgentPanelRow[]>;
}) {
  const { tokens: t } = useTheme();
  const [text, setText] = useState('');
  // 光标所在的那个 `#` 查询；null = 没有候选面板。起止点都记下来：插入时要把
  // `#s:abc` 整段换掉，而不只是接在后面。
  const [refPick, setRefPick] = useState<{ start: number; end: number; query: string } | null>(null);
  const caretRef = useRef(0);
  // 文本的镜像：onSelectionChange 里的闭包可能是上一个 render 的，读 state 会晚一拍。
  const textRef = useRef('');
  // 待发的文件：传完的带 file，正在传的没有，失败的那条带 error（读者可以删掉它再来一次）
  const [files, setFiles] = useState<Attach[]>([]);
  const [menu, setMenu] = useState(false);
  const inputRef = useRef<ComponentRef<typeof TextInput>>(null);
  // 已经为「字还没提交」重试过一次；防止真的空着也反复失焦
  const retried = useRef(false);
  const submitRef = useRef<() => void>(() => {});
  const short = waiting === '等待你的授权' ? '等你授权' : waiting === '等待你的回答' ? '等你回答' : '';
  const uploading = files.some((f) => !f.file && !f.error);
  const ready = files.filter((f): f is Attach & { file: UploadedFile } => !!f.file);
  // 能发的东西：一句话，或者至少一个传完的文件（只发一张图、什么也不说是合法的）
  const canSend = !!text.trim() || ready.length > 0;

  // ── `#s:` 补全 ────────────────────────────────────────────────────────────
  // 一条对话引用就是一段文本 `#s:<id>`：展开发生在桌面那一侧（agent/atfile），手机只负责
  // 把拼写写对。所以「什么算引用」必须和 desktop/frontend/src/lib.ts、agent/atfile 同一条
  // 规则（都在 refs.ts 里，和它一起被测）。
  const syncPick = useCallback((value: string, caret: number) => {
    const ref = hashRefAt(value, caret);
    if (!ref || hashQuery(ref.query).kind === 'none') {
      setRefPick(null);
      return;
    }
    setRefPick({ start: ref.start, end: caret, query: ref.query });
  }, []);

  const pickKind = refPick ? hashQuery(refPick.query) : { kind: 'none' as const };
  const rest = 'rest' in pickKind ? pickKind.rest : '';
  // 节点候选取一次就够：一次会话里节点不会变，而那一趟要等网络（那是打开 `#a:` 的人在等）。
  // 失败就空着 —— 手打 `#a:<身份>` 仍然照常可用，展开在桌面那一侧。
  const [agents, setAgents] = useState<AgentPanelRow[]>([]);
  const agentsAsked = useRef(false);
  useEffect(() => {
    if (pickKind.kind !== 'agents' || agentsAsked.current) return;
    agentsAsked.current = true;
    void loadAgents()
      .then((rows) => setAgents(rows))
      .catch(() => setAgents([]));
  }, [pickKind.kind, loadAgents]);

  // 两个命名空间各列各的：`#s:` 是其它对话（手机手里就有），`#a:` 是节点（要问一次那台电脑）。
  // 还没敲完前缀（`#`、`#a`、`#s`）时两组都可以列 —— 那几行就是「# 能干什么」的答案。
  const sessionRows = useMemo(
    () => (pickKind.kind === 'sessions' || pickKind.kind === 'candidates'
      ? matchSessions(sessions, rest, currentId, REF_PICK_LIMIT)
      : []),
    [pickKind.kind, rest, sessions, currentId],
  );
  const agentRows = useMemo(
    () => (pickKind.kind === 'agents' ? matchAgents(agents, rest, REF_PICK_LIMIT) : []),
    [pickKind.kind, rest, agents],
  );

  const insertRef = useCallback(
    (id: string, kind: 'session' | 'agent') => {
      if (!refPick) return;
      const ref = kind === 'agent' ? agentRef(id) : sessionRef(id);
      const out = spliceRef(textRef.current, refPick.start, refPick.end, ref);
      textRef.current = out.value;
      caretRef.current = out.caret;
      setText(out.value);
      setRefPick(null);
      // 光标跟着落到那个补上的空格后面。不落的话这次插入还停在引用里，面板会立刻再弹回来。
      requestAnimationFrame(() =>
        inputRef.current?.setNativeProps({ selection: { start: out.caret, end: out.caret } }),
      );
    },
    [refPick],
  );

  // 挑到一个文件就先把它画成一条 chip，再传：上传要几秒，而这段时间里屏幕上是空的，
  // 读起来就是「点了没反应」。传完 chip 才变成能发的；失败的那条留在原地说为什么。
  const add = useCallback(
    async (picked: PickedFile | null) => {
      if (!picked) return;
      const key = `${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
      setFiles((list) => [...list, { key, name: picked.name, size: picked.size }]);
      const { file, error } = await onUpload(picked);
      setFiles((list) =>
        list.map((f) =>
          f.key === key
            ? file
              ? { ...f, file, name: file.name, size: file.size }
              : { ...f, error: error || '没能传上去' }
            : f,
        ),
      );
    },
    [onUpload],
  );

  // 挑文件本身。取消（或没给权限）回 null，那是读者的选择，不是错误；但**真的出错**
  // 必须留一行字：这里原先是 `.catch(() => null)`，于是「点了没反应」连个理由都没有。
  const openPicker = useCallback(
    async (source: 'library' | 'camera' | 'document') => {
      try {
        const picked = source === 'document' ? await pickDocument() : await pickPhoto(source);
        await add(picked);
      } catch {
        const key = `${Date.now()}-pick`;
        const name = source === 'document' ? '文件' : '照片';
        setFiles((list) => [...list, { key, name, error: '打不开选择器' }]);
      }
    },
    [add],
  );

  // 三个入口都在这层浮出的 Modal 里。**要等它收完再开系统选择器**：iOS 上一个
  // controller 还在 dismiss 的时候去 present 另一个，会静默失败 —— 相机那条路因为
  // 先 await 了一次权限，恰好躲过了这一帧，所以「拍照能开、照片和文件没反应」。
  // onDismiss 只在 iOS 上有意义；Android 的选择器是另一个 Activity，直接开。
  const pendingPick = useRef<'library' | 'camera' | 'document' | null>(null);
  const pick = (source: 'library' | 'camera' | 'document') => {
    setMenu(false);
    if (Platform.OS === 'ios') {
      pendingPick.current = source;
      return;
    }
    void openPicker(source);
  };

  const submit = () => {
    setMenu(false);
    const v = text.trim();
    // 还有文件在路上：现在发出去就是把它弄丢，而屏幕上那句话已经发出去了 —— 挡下来。
    if (uploading) return;
    if (!v && ready.length === 0) {
      // 空着按发送 = 停掉这一轮（正在跑的时候才有意义）
      if (running) {
        onStop();
        return;
      }
      // 字在屏幕上、`text` 却是空的：中文输入法还在组合里（拼音没选字），点击先让它
      // 提交，state 要下一帧才跟上。不补这一下的话，第一次点击只是把拼音提交掉，
      // 消息发不出去 —— 表现就是「发送要点两次」。
      if (inputRef.current?.isFocused() && !retried.current) {
        retried.current = true;
        inputRef.current.blur();
        requestAnimationFrame(() => submitRef.current());
      }
      return;
    }
    retried.current = false;
    const sent = ready.map((f) => f.file);
    if (running) {
      onQueue(v, sent);
      textRef.current = '';
      setText('');
      setRefPick(null);
      setFiles([]);
      return;
    }
    // 不在跑：服务端没收下就把字还给读者（拒绝的原因在顶部横幅里）—— 文件也留着，重发还是那几个
    void onSend(v, 'send', sent).then((ok) => {
      if (ok) {
        textRef.current = '';
        setText('');
        setRefPick(null);
        setFiles([]);
      }
    });
  };
  submitRef.current = submit;

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
            <PendingRow
              key={`${i}-${p.text.slice(0, 8)}`}
              text={[p.text, ...p.files.map((f) => `📎 ${f.name}`)].filter(Boolean).join('\n')}
              onSendNow={() => onSendPending(i)}
              onDrop={() => onDropPending(i)}
            />
          ))}
        </View>
      ) : null}

      {files.length > 0 ? (
        <View style={[styles.attachBar, styles.column]}>
          {files.map((f) => (
            <View
              key={f.key}
              style={[styles.fileChip, { backgroundColor: t.inputBg, borderColor: f.error ? t.red : t.border }]}
            >
              <Feather
                name={f.error ? 'alert-circle' : f.file?.kind === 'image' ? 'image' : 'file'}
                size={12}
                color={f.error ? t.red : t.faint}
              />
              <Text style={{ color: f.error ? t.red : t.dim, fontSize: 12, flexShrink: 1 }} numberOfLines={1}>
                {f.error ? `${f.name}：${f.error}` : f.name}
              </Text>
              {/* 正在传的那条没有 ×：它已经在路上了，删掉 chip 也停不下这次请求 */}
              {f.file || f.error ? (
                <Pressable onPress={() => setFiles((list) => list.filter((x) => x.key !== f.key))} hitSlop={8}>
                  <Feather name="x" size={12} color={t.faint} />
                </Pressable>
              ) : (
                <ActivityIndicator size="small" color={t.faint} />
              )}
            </View>
          ))}
        </View>
      ) : null}

      {/* 三个入口是一层浮出的选择，不是常驻的一行：点空白处、按返回键、端走任意一项都会收起来。
          之前它挂在输入框上面不走，读者挑完照片回到对话，那排字还留在那儿，看着像输入框多了一行。 */}
      <Modal
        visible={menu}
        transparent
        animationType="fade"
        onRequestClose={() => setMenu(false)}
        onDismiss={() => {
          const source = pendingPick.current;
          pendingPick.current = null;
          if (source) void openPicker(source);
        }}
      >
        <Pressable style={[styles.menuBackdrop, styles.column]} onPress={() => setMenu(false)}>
          <View style={[styles.menuSheet, { backgroundColor: t.panel, borderColor: t.border }]}>
            <AttachItem icon="image" label="照片" onPress={() => pick('library')} />
            <AttachItem icon="camera" label="拍照" onPress={() => pick('camera')} />
            <AttachItem icon="file-text" label="文件" onPress={() => pick('document')} />
          </View>
        </Pressable>
      </Modal>

      {running ? (
        <View style={[styles.runHint, styles.column]}>
          <View style={[styles.runDot, { backgroundColor: t.amber }]} />
          <Text style={{ color: t.faint, fontSize: 11.5 }} numberOfLines={1}>
            {short ? `停在这里${short}` : '正在跑 · 发送会排队'}
          </Text>
        </View>
      ) : null}
      {/* `#s:` 的候选。它挂在输入行上面而不是浮成一个覆盖层：手机上覆盖层会把正在写的那句话
          挡住，而读者做选择时正需要看那句话。 */}
      {sessionRows.length > 0 || agentRows.length > 0 ? (
        <View style={[styles.refBar, styles.column, { borderColor: t.border, backgroundColor: t.inputBg }]}>
          <Text style={{ color: t.faint, fontSize: 10.5 }}>
            {agentRows.length > 0 ? '引用一台节点（派活用）' : '引用另一段对话'}
          </Text>
          {sessionRows.map((row) => (
            <Pressable
              key={row.id}
              onPress={() => insertRef(row.id, 'session')}
              accessibilityRole="button"
              accessibilityLabel={`引用 ${row.title || row.id}`}
              style={({ pressed }) => [styles.refRow, pressed && { backgroundColor: t.wash }]}
            >
              <Feather name="corner-up-left" size={12} color={t.faint} />
              <Text style={{ color: t.dim, fontSize: 12.5, flex: 1 }} numberOfLines={1}>
                {row.title || '（无标题）'}
              </Text>
              <Text style={{ color: t.faint, fontSize: 11 }}>{row.id.slice(0, 8)}</Text>
            </Pressable>
          ))}
          {agentRows.map((row) => (
            <Pressable
              key={row.id}
              onPress={() => insertRef(row.id, 'agent')}
              accessibilityRole="button"
              accessibilityLabel={`引用节点 ${row.label || row.name || row.id}`}
              style={({ pressed }) => [styles.refRow, pressed && { backgroundColor: t.wash }]}
            >
              <Feather name="cpu" size={12} color={t.faint} />
              <Text style={{ color: t.dim, fontSize: 12.5, flex: 1 }} numberOfLines={1}>
                {row.label || row.name || row.id.slice(0, 8)}
              </Text>
              <Text style={{ color: t.faint, fontSize: 11 }}>{row.reach || ''}</Text>
            </Pressable>
          ))}
        </View>
      ) : null}
      <View style={[styles.inputRow, styles.column]}>
        <Pressable
          onPress={() => setMenu((m) => !m)}
          accessibilityRole="button"
          accessibilityLabel="发送文件"
          style={({ pressed }) => [styles.attachBtn, pressed && { backgroundColor: t.wash }]}
        >
          <Feather name="paperclip" size={17} color={menu ? t.accent2 : t.faint} />
        </Pressable>
        <View style={[styles.inputWrap, { backgroundColor: t.inputBg, borderColor: t.border }]}>
          <TextInput
            ref={inputRef}
            value={text}
            onChangeText={(v) => {
              const prev = textRef.current;
              textRef.current = v;
              setText(v);
              // 多出来的那几个字落在光标处：按这个差推进光标，末尾追加（最常见）才准；
              // 删除时往回收，准确位置由紧接着的 onSelectionChange 补上。
              const caret = Math.min(v.length, caretRef.current + Math.max(0, v.length - prev.length));
              caretRef.current = caret;
              syncPick(v, caret);
            }}
            // 光标移动（含打字之后）都要重算：补全面板跟着光标走，而不是跟着输入框的末尾。
            onSelectionChange={(e) => {
              const caret = e.nativeEvent.selection.start;
              caretRef.current = caret;
              syncPick(textRef.current, caret);
            }}
            placeholder="补一句…"
            placeholderTextColor={t.faint}
            style={[styles.input, { color: t.text }]}
            multiline
            // 回车 = 发送，不换行。两套平台各认一个开关：原生认 submitBehavior（多行输入框的
            // Return 从「插一个换行」变成「提交」，焦点不丢 —— 连发几句不用每次重新点输入框）；
            // react-native-web 还只认老的 blurOnSubmit，而它在多行输入框上正是同一个意思。
            // 想换行仍然有 Shift+Enter（web）。
            submitBehavior="submit"
            blurOnSubmit={Platform.OS === 'web' ? true : undefined}
            onSubmitEditing={() => submitRef.current()}
          />
        </View>
        {/* 运行中、又没有话要发：这颗按钮的意思就是「停」。它长成桌面那个转动的红环，
            而不是一个方块 + 上面一行「打断」—— 那个字在手机上是纯解释，形状本身已经说明白了。 */}
        {running && !canSend ? (
          <Pressable onPress={onStop} accessibilityRole="button" accessibilityLabel="停止">
            <StopRing t={t} />
          </Pressable>
        ) : (
          <Pressable
            onPress={submit}
            // 只有图标：读屏和自动化都要靠这个名字认它
            accessibilityRole="button"
            accessibilityLabel="发送"
            style={({ pressed }) => [styles.sendBtn, { backgroundColor: pressed || !canSend ? t.accentStrong : t.accent }]}
          >
            <Feather name="arrow-up" size={17} color={t.onAccent} />
          </Pressable>
        )}
      </View>
    </View>
  );
}

/**
 * 一轮的过程：一行概括，点开按记录里的先后展开（桌面的「N 步」那一行的手机版）。
 *
 * 折进去的是思考、成功的工具调用、以及模型在中途写的话；只有这一轮最后写的那段留在
 * 外面（它是答案），失败的那一步也留在外面。展开时顺序就是它们真正发生的顺序 —— 所以
 * 这里不要按种类分组。
 */
function ProcessFold({ steps, running }: { steps: ProcessStep[]; running?: boolean }) {
  const { tokens: t } = useTheme();
  const [open, setOpen] = useState(false);
  const tools = steps.filter((s): s is Extract<ProcessStep, { kind: 'tool' }> => s.kind === 'tool');
  const thinkings = steps.filter((s): s is Extract<ProcessStep, { kind: 'thinking' }> => s.kind === 'thinking');
  const notes = steps.filter((s) => s.kind === 'note');
  const names = tools.map((x) => x.pair.msg.toolName ?? '').filter(Boolean);
  const unique = [...new Set(names)];
  // 只有中间写的话、没有工具也没有思考：说的就是「这一段是过程，不是答案」
  const label =
    tools.length > 0
      ? `${tools.length} 步`
      : thinkings.length > 0
        ? `思考 ${thinkings.length} 段`
        : `${notes.length} 段过程说明`;
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
            step.kind === 'tool' ? (
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
            ) : (
              <View key={step.key} style={{ paddingHorizontal: 8 }}>
                {step.kind === 'thinking' ? <Thinking text={step.text} /> : <Markdown text={step.msg.content} />}
              </View>
            ),
          )}
        </View>
      ) : null}
    </View>
  );
}

/**
 * 活气泡：这一轮**正在写的那一段话**。之前写的已经由 items.ts 折成过程那一行了 ——
 * 两边各画一套的话，跑着的时候正文一段段堆在外面、停下来才折，画面会在轮次结束时整段跳。
 */
function LiveBubble({ text }: { text: string }) {
  if (!text) return null;
  return (
    <View style={styles.live}>
      <Markdown text={text} />
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
  onUpload,
  onStop,
  onLoadOlder,
  olderLoading,
  onEdgeBack,
  onRename,
  pending,
  onQueue,
  onDropPending,
  onSendPending,
  onClearPending,
  onSendPendingNow,
  sessions,
  loadAgents,
  machine,
}: {
  snapshot: Snapshot;
  /** 全部对话，`#s:` 的候选就来自这里 —— 手机手里本来就有这份列表，不必为补全再问一次服务端。 */
  sessions: SessionSummary[];
  /** `#a:` 的候选：节点身份只在那台电脑上，所以要问一次（见 `loadPanels`） */
  loadAgents: () => Promise<AgentPanelRow[]>;
  /**
   * 这一段在哪台电脑上（手机上给它起的名字）。
   *
   * 首页把几台机器混在一列里，点进来之后必须能一眼确认「我在哪台上说话」—— 同一段标题
   * 在两台机器上都可能存在，而这一屏能做的事（跑什么、读哪个目录）完全由那台决定。
   */
  machine?: string;
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
  onSend: (text: string, mode: SendMode, files?: UploadedFile[]) => Promise<boolean>;
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
  /** 改这一段对话的名字（和列表里长按一行是同一个写入方、同一个对话框） */
  onRename: (id: string, title: string) => void;
  /** 待发送队列（运行中发的消息，连着挂在上面的文件）；渲染在输入框上方 */
  pending: PendingMessage[];
  onQueue: (text: string, files?: UploadedFile[]) => void;
  /** 把一个本地文件传给那台电脑；答案里是它存下来的名字，或者为什么没成 */
  onUpload: (f: PickedFile) => Promise<{ file?: UploadedFile; error?: string }>;
  onDropPending: (index: number) => void;
  /** 队列里的一条「现在就发」（待发送那一行往上划） */
  onSendPending: (index: number) => void;
  onClearPending: () => void;
  onSendPendingNow: () => void;
}) {
  const { tokens: t } = useTheme();
  const s = snapshot.session;
  // 三块分开算，只有历史进列表数据。
  //
  // 实时那一轮每个字都在变；它跟着进数组的话，整份数组每个字都重建一次，而列表单元是按
  // 引用判断要不要重画的（RN 的单元是 PureComponent，renderItem 就在它的 props 里）——
  // 一次重建就是整列重画，长会话里每来一段文字都要把可见消息的 markdown 重新解析一遍。
  // 所以历史只依赖 snapshot，实时那一轮和待答项挂在列表头部（见 ListHeaderComponent）。
  const history = useMemo(() => buildItems(snapshot), [snapshot]);
  // FlatList 是 inverted 的：数据倒过来喂，视觉上第一条在底部
  const reversed = useMemo(() => history.slice().reverse(), [history]);
  const liveTail = useMemo(() => buildLiveTail(live), [live]);
  const asks = useMemo(() => buildAskItems(snapshot), [snapshot]);

  // 会变的东西走 ref：renderItem 的引用必须永久稳定，否则上层每帧新建的箭头函数
  // 会把它变成一个每帧都换引用的新函数，memo 和 PureComponent 就全都失效了。
  const env = useRef({
    t,
    fileSource,
    onDecide,
    onAnswer,
    onDecline,
    onOpenFile,
    attachments: snapshot.attachments,
  });
  env.current = {
    t,
    fileSource,
    onDecide,
    onAnswer,
    onDecline,
    onOpenFile,
    attachments: snapshot.attachments,
  };

  const renderBody = useCallback((item: Item) => {
    const e = env.current;
    if (item.kind === 'process') return <ProcessFold steps={item.steps} running={item.running} />;
    // 实时那一条只在列表头部渲染：它要每帧的 live，进了这里就等于把 live 塞进依赖
    if (item.kind === 'live') return null;
    return (
      <MessageView
        msg={item.msg}
        ask={item.ask}
        toolResult={item.result}
        running={item.running}
        attachments={e.attachments}
        fileSource={e.fileSource}
        onDecide={e.onDecide}
        onAnswer={e.onAnswer}
        onDecline={e.onDecline}
        onOpenFile={e.onOpenFile}
      />
    );
  }, []);
  const renderRow = useCallback(({ item }: { item: Item }) => renderBody(item), [renderBody]);
  // 一页在飞的时候不要再要一页：滚动事件一秒能来几十次，而服务端那一页是真的要读盘的
  const wantOlder = () => {
    if (!snapshot.hasMore || olderLoading) return;
    onLoadOlder();
  };

  const back = useEdgeBack(onEdgeBack);
  // inverted 列表：offset 0 就是**最新那条**，越往上翻越大。所以「回到底部」= 滚到 0。
  const listRef = useRef<FlatList<Item>>(null);
  // 滚离底部时浮出那个胶囊。只有跨过阈值的那一帧过 JS —— 滚动事件一秒几十条，
  // 每条都 setState 的话，正是这段代码想避开的「整列重画」。
  const [away, setAway] = useState(false);
  const awayRef = useRef(false);
  const toBottom = useCallback(() => {
    listRef.current?.scrollToOffset({ offset: 0, animated: true });
  }, []);
  const onScroll = useCallback((e: NativeSyntheticEvent<NativeScrollEvent>) => {
    const next = e.nativeEvent.contentOffset.y > STAY_AWAY;
    if (next === awayRef.current) return;
    awayRef.current = next;
    setAway(next);
  }, []);
  // 换一段对话要归位：列表数据换了、偏移也从 0 重新开始，那颗胶囊不该留在屏上
  useEffect(() => {
    awayRef.current = false;
    setAway(false);
  }, [s.id]);
  // 发出去就跳回底部：读者刚说完话，等的是下面那条回复
  const send = useCallback(
    async (text: string, mode: SendMode, files?: UploadedFile[]) => {
      const ok = await onSend(text, mode, files);
      if (ok) toBottom();
      return ok;
    },
    [onSend, toBottom],
  );
  // 顶栏：双击回到底部、长按改名。两件事挂在同一个 Pressable 上不会互相误伤 ——
  // RN 里长按触发之后 onPress 就不再来了。
  const [renaming, setRenaming] = useState<{ id: string; title: string } | null>(null);
  const tapAt = useRef(0);
  const onPressTitle = useCallback(() => {
    const now = Date.now();
    if (now - tapAt.current < DOUBLE_TAP) {
      tapAt.current = 0;
      toBottom();
      return;
    }
    tapAt.current = now;
  }, [toBottom]);

  return (
      <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable
          onPress={onToggleList ?? onBack}
          style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}
        >
          <Feather name={onToggleList ? 'menu' : 'chevron-left'} size={onToggleList ? 19 : 22} color={t.dim} />
        </Pressable>
        {/* 标题这一块是双击回到底部、长按改名的地方（见 onPressTitle）。它本来就不是按钮，
            单击不做任何事，所以这两个手势不会盖掉谁的点击。 */}
        <Pressable
          onPress={onPressTitle}
          onLongPress={() => setRenaming({ id: s.id, title: s.title })}
          delayLongPress={400}
          accessibilityHint="双击回到底部，长按改名"
          style={styles.headInfo}
        >
          {/* 标题旁边说明这段对话属于哪个渠道：答复发在那边，而这一屏只能读和补充。 */}
          <View style={styles.headTitleRow}>
            <Text style={[styles.headTitle, { color: t.title }]} numberOfLines={1}>
              {s.title}
            </Text>
            {s.channelSession ? (
              <View style={[styles.headTag, { borderColor: t.border }]}>
                <Text style={[styles.headTagText, { color: t.faint }]} numberOfLines={1}>
                  {s.channelName || '渠道'}
                </Text>
              </View>
            ) : null}
          </View>
          <Text style={[styles.headSub, { color: t.faint }]} numberOfLines={1}>
            {machine ? <Text style={{ color: t.accent2 }}>{machine}</Text> : null}
            {machine ? ' · ' : ''}
            {s.projectName ? `${s.projectName} · ` : ''}
            {s.readOnly ? '来自别的任务' : s.running ? '正在跑' : '空闲'}
          </Text>
        </Pressable>
        <StatePill session={s} t={t} />
      </View>

      <MetaStrip snapshot={snapshot} live={live} onOpenInfo={onOpenInfo} onOpenModels={onOpenModels} />

      <View style={styles.listWrap}>
      <FlatList
        ref={listRef}
        style={styles.list}
        data={reversed}
        keyExtractor={(it) => it.key}
        contentContainerStyle={[styles.messages, styles.column]}
        // inverted：最新的一条在底部，偏移 0 就是「贴底」。这样打开会话自然停在最新处，
        // 往上翻出来的一页接在数组末尾（视觉上是顶部），画面一动不动 —— 不用去补偿滚动位置。
        inverted
        onScroll={onScroll}
        scrollEventThrottle={16}
        // 手指在消息上往上滑就收键盘（iOS 跟手收，Android 滑一下就收）。少了这一条，
        // 键盘会一直占着下半屏，只能点别处或按返回 —— 而读历史正是要滑的。
        keyboardDismissMode={Platform.OS === 'ios' ? 'interactive' : 'on-drag'}
        // 键盘弹着时点消息里的卡片要能点开，别被「先收键盘」吃掉这一下
        keyboardShouldPersistTaps="handled"
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
        ListHeaderComponent={
          // 列表头部在 inverted 列表里就是**视觉最底部**：先是实时那一轮（过程一行在正文
          // 上面），再往下是待答项 —— 待答项永远最靠底，正在跑的内容不能把它挤上去。
          liveTail.length || asks.length ? (
            <View>
              {liveTail.map((it) => {
                if (it.kind === 'live') return <LiveBubble key={it.key} text={it.text} />;
                if (it.kind === 'process')
                  return <ProcessFold key={it.key} steps={it.steps} running={it.running} />;
                // 剩下的是这一轮交出来的文件卡：用和历史同一个渲染，附件信息在环境里
                return <View key={it.key}>{renderBody(it)}</View>;
              })}
              {asks.map((it) => (
                <View key={it.key}>{renderBody(it)}</View>
              ))}
            </View>
          ) : undefined
        }
        renderItem={renderRow}
      />

      {/* 滚上去读历史时，最新那条在屏幕外面 —— 这一段对话还在跑也好、刚跑完也好，
          滑回去要滑很久。胶囊只在真滚远了才出现（判据见 STAY_AWAY）。 */}
      {away ? (
        <Pressable
          onPress={toBottom}
          accessibilityRole="button"
          accessibilityLabel="回到底部"
          style={({ pressed }) => [
            styles.jump,
            { backgroundColor: pressed ? t.washStrong : t.panel, borderColor: t.border },
          ]}
        >
          <Feather name="arrow-down" size={13} color={t.dim} />
          <Text style={{ color: t.dim, fontSize: 11.5 }}>回到底部</Text>
          {s.running ? <View style={[styles.jumpDot, { backgroundColor: t.amber }]} /> : null}
        </Pressable>
      ) : null}
      </View>

      {s.readOnly ? (
        <View style={[styles.readOnlyNote, { borderTopColor: t.border }]}>
          <Text style={{ color: t.faint, fontSize: 12.5 }}>这段对话由别的任务占用，只能查看</Text>
        </View>
      ) : (
        <Composer
          running={s.running}
          waiting={s.waiting}
          pending={pending}
          onSend={send}
          onUpload={onUpload}
          onQueue={onQueue}
          onDropPending={onDropPending}
          onSendPending={onSendPending}
          onClearPending={onClearPending}
          onSendPendingNow={onSendPendingNow}
          onStop={onStop}
          sessions={sessions}
          currentId={s.id}
          loadAgents={loadAgents}
        />
      )}

      {/* 长按顶栏标题打开它。和列表里长按一行是同一个写入方 —— 改完 sessions_changed
          回来，这一段的标题跟着换，桌面上那一行也换。 */}
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
        <EdgeBackZone gesture={back.gesture} />
      </Animated.View>
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
  back: { width: 34, height: 34, borderRadius: 10, alignItems: 'center', justifyContent: 'center' },
  headInfo: { flex: 1, minWidth: 0, paddingHorizontal: 4 },
  headTitle: { fontSize: 14.5, fontWeight: '600', flexShrink: 1 },
  headTitleRow: { flexDirection: 'row', alignItems: 'center', gap: 6 },
  headTag: { borderWidth: 1, borderRadius: 20, paddingHorizontal: 6, paddingVertical: 1 },
  headTagText: { fontSize: 10.5 },
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
  // 列表与那个浮出的胶囊共用一块：胶囊绝对定位在它的底部（= 输入框上方）
  listWrap: { flex: 1 },
  list: { flex: 1 },
  jump: {
    position: 'absolute',
    bottom: 10,
    alignSelf: 'center',
    flexDirection: 'row',
    alignItems: 'center',
    gap: 5,
    borderWidth: StyleSheet.hairlineWidth,
    borderRadius: 20,
    paddingHorizontal: 11,
    paddingVertical: 6,
    shadowColor: '#000',
    shadowOpacity: 0.16,
    shadowRadius: 10,
    shadowOffset: { width: 0, height: 4 },
    elevation: 4,
  },
  jumpDot: { width: 5, height: 5, borderRadius: 2.5 },
  olderHead: { flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 7, paddingVertical: 10 },
  // 平板那种宽屏下，一行别横跨整屏 —— 读一句话要来回甩头
  column: { width: '100%', maxWidth: 760, alignSelf: 'center' },
  live: { paddingVertical: 4 },
  liveText: { fontSize: 15, lineHeight: 25 },
  think: { borderLeftWidth: 2, paddingLeft: 10, marginVertical: 6, opacity: 0.9 },
  tool: { borderWidth: 1, borderRadius: 10, marginVertical: 7, overflow: 'hidden' },
  toolHead: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingHorizontal: 10, paddingVertical: 9 },
  toolName: { fontSize: 12.5, fontWeight: '600' },
  toolSum: { flex: 1, fontSize: 12, fontFamily: 'monospace' },
  toolBody: { borderTopWidth: StyleSheet.hairlineWidth, padding: 10 },

  composer: { borderTopWidth: StyleSheet.hairlineWidth, paddingTop: 6, paddingBottom: 8 },
  pendingBar: { paddingHorizontal: 12, paddingBottom: 6 },
  pendingHead: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingBottom: 5, paddingTop: 2 },
  pendingItem: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 3 },
  // 上滑 / 下滑时整行换底色，圆角让它看起来是「一条」而不是一段
  pendingRow: { borderRadius: 8 },
  runHint: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingHorizontal: 16, paddingBottom: 6 },
  runDot: { width: 5, height: 5, borderRadius: 2.5 },
  inputRow: { flexDirection: 'row', alignItems: 'center', gap: 9, paddingHorizontal: 12 },
  refBar: { marginHorizontal: 12, marginBottom: 8, paddingVertical: 6, paddingHorizontal: 10, borderWidth: 1, borderRadius: 12, gap: 2 },
  refRow: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 5, paddingHorizontal: 2, borderRadius: 8 },
  attachBtn: { width: 32, height: 32, borderRadius: 16, alignItems: 'center', justifyContent: 'center' },
  attachBar: { paddingHorizontal: 12, paddingBottom: 6, gap: 5 },
  fileChip: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 6,
    borderWidth: StyleSheet.hairlineWidth,
    borderRadius: 12,
    paddingHorizontal: 9,
    paddingVertical: 5,
  },
  menuBackdrop: { flex: 1, justifyContent: 'flex-end', backgroundColor: 'rgba(0,0,0,0.28)', paddingHorizontal: 12, paddingBottom: 20 },
  menuSheet: {
    flexDirection: 'row',
    gap: 6,
    borderWidth: StyleSheet.hairlineWidth,
    borderRadius: 14,
    padding: 8,
    // 浮在输入框之下那一块之上的阴影：不与页面的其它层共用，只有它自己会浮起来
    shadowColor: '#000',
    shadowOpacity: 0.18,
    shadowRadius: 14,
    shadowOffset: { width: 0, height: 6 },
    elevation: 6,
  },
  attachItem: { flexDirection: 'row', alignItems: 'center', gap: 6, paddingHorizontal: 10, paddingVertical: 7, borderRadius: 10 },
  inputWrap: { flex: 1, borderWidth: 1, borderRadius: 20, paddingHorizontal: 15, paddingVertical: 11 },
  input: { fontSize: 14, padding: 0, maxHeight: 90 },
  sendBtn: { width: 38, height: 38, borderRadius: 18, alignItems: 'center', justifyContent: 'center' },
  stopBtn: { width: 38, height: 38, borderRadius: 19, borderWidth: 1, alignItems: 'center', justifyContent: 'center' },
  // 环比按钮小一圈：桌面是 inset -4（往外扩），手机上往外扩会压到上面那行提示
  stopRing: { position: 'absolute', width: 33, height: 33, borderRadius: 17, borderWidth: 2, borderColor: 'transparent' },
  stopSquare: { width: 9, height: 9, borderRadius: 2 },
  readOnlyNote: { borderTopWidth: StyleSheet.hairlineWidth, padding: 14, alignItems: 'center' },
});
