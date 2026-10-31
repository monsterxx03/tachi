import { Feather } from '@expo/vector-icons';
import { memo, useState } from 'react';
import { Image, Pressable, StyleSheet, Text, TextInput, View } from 'react-native';
import type { FileSource } from '../api/client';
import type { AgentQuestion, AttachmentRef, PendingAsk, SessionMessage } from '../api/types';
import { useTheme } from '../theme/ThemeContext';
import { formatMessageTime } from './time';
import { Markdown } from './Markdown';
import { stripReminders } from './reminder';

/**
 * 助手的一段回复：整段交给 Markdown（围栏里的 mermaid 也在它的 fence 规则里接管）。
 *
 * **长按归系统。** 正文是 `selectable` 的，长按弹出原生选区（在 Android 上可以拖选择柄圈一段）
 * —— 那是手机上唯一能只复制半句的办法，所以这一层不加任何自己的长按手势：包一层
 * `Pressable onLongPress` 的话，原生选区在 Android 上就被吃掉了（做过，又撤了）。
 */
function AssistantText({ text }: { text: string }) {
  const clean = stripReminders(text);
  if (!clean) return null;
  return <Markdown text={clean} />;
}

/** 工具参数从 JSON 字符串摊开，读起来像一段代码而不是一行 */
function prettyArgs(args?: string): string {
  if (!args) return '';
  try {
    return JSON.stringify(JSON.parse(args), null, 2);
  } catch {
    return args;
  }
}

/**
 * 工具调用：一行，点开看参数和结果。
 *
 * 实时那一轮和历史记录用同一个组件 —— 两边各画一套的结果是「正在跑的时候点不开、
 * 停下来才恢复正常」，那种差异没有理由存在。
 */
export function ToolCard({
  name,
  title,
  args,
  result,
  isError,
  running,
}: {
  name: string;
  title?: string;
  args?: string;
  result?: string;
  isError?: boolean;
  running?: boolean;
}) {
  const { tokens: t } = useTheme();
  const [open, setOpen] = useState(false);
  const summary = title || prettyArgs(args).split('\n')[0] || '';

  return (
    <View style={[styles.tool, { borderColor: t.border, backgroundColor: t.wash }]}>
      <Pressable onPress={() => setOpen((v) => !v)} style={styles.toolHead}>
        <Feather
          name={open ? 'chevron-up' : 'chevron-down'}
          size={13}
          color={t.faint}
          style={{ marginTop: 1 }}
        />
        {running ? <Feather name="loader" size={12} color={t.faint} style={{ marginTop: 1 }} /> : null}
        <Text style={[styles.toolName, { color: isError ? t.red : t.dim }]} numberOfLines={1}>
          {name}
        </Text>
        <Text style={[styles.toolSum, { color: t.faint }]} numberOfLines={1}>
          {summary}
        </Text>
      </Pressable>
      {open ? (
        <View style={[styles.toolBody, { borderTopColor: t.border }]}>
          {args ? (
            <Text style={{ color: t.dim, fontFamily: 'monospace', fontSize: 12 }}>{prettyArgs(args)}</Text>
          ) : null}
          {result ? (
            <Text
              style={{
                color: isError ? t.red : t.dim,
                fontFamily: 'monospace',
                fontSize: 12,
                marginTop: args ? 8 : 0,
              }}
            >
              {result.trimEnd()}
            </Text>
          ) : null}
          {!args && !result ? (
            <Text style={{ color: t.faint, fontSize: 12 }}>{running ? '还在跑…' : '没有留下参数'}</Text>
          ) : null}
        </View>
      ) : null}
    </View>
  );
}

/** 权限卡：等你的授权 */
function PermissionCard({
  preview,
  toolName,
  onDecide,
}: {
  preview: string;
  toolName: string;
  onDecide: (allow: boolean) => void;
}) {
  const { tokens: t } = useTheme();
  const [lines, ...rest] = preview.split('\n');
  const rule = rest.join(' ').trim();

  return (
    <View style={[styles.card, { borderColor: t.borderStrong, backgroundColor: t.panel }]}>
      <Text style={[styles.cardHead, { color: t.amber }]}>等待你的授权 · {toolName}</Text>
      <View style={[styles.cmd, { backgroundColor: t.cmdBg }]}>
        <Text style={{ color: t.cmdInk, fontFamily: 'monospace', fontSize: 12.5 }}>{lines}</Text>
      </View>
      {rule ? <Text style={[styles.cardNote, { color: t.faint }]}>{rule}</Text> : null}
      <View style={styles.cardActions}>
        <Pressable
          onPress={() => onDecide(true)}
          style={({ pressed }) => [
            styles.btnPrimary,
            { backgroundColor: pressed ? t.accentStrong : t.accent },
          ]}
        >
          <Text style={{ color: t.onAccent, fontSize: 13, fontWeight: '600' }}>允许一次</Text>
        </Pressable>
        <Pressable
          onPress={() => onDecide(false)}
          style={({ pressed }) => [
            styles.btnGhost,
            { borderColor: t.redLine, backgroundColor: pressed ? t.redWash : 'transparent' },
          ]}
        >
          <Text style={{ color: t.red, fontSize: 13, fontWeight: '600' }}>拒绝</Text>
        </Pressable>
      </View>
    </View>
  );
}

/** 提问卡：agent 想确认一件事 */
function QuestionCard({
  questions,
  onAnswer,
  onDecline,
}: {
  questions: AgentQuestion[];
  onAnswer: (answers: Record<string, string>) => void;
  onDecline: () => void;
}) {
  const { tokens: t } = useTheme();
  // 每一问一格：多选是**数组**，单选也走同一条路（数组里最多一个）
  const [picked, setPicked] = useState<Record<string, string[]>>({});
  const [free, setFree] = useState<Record<string, string>>({});

  const toggle = (q: AgentQuestion, opt: string) =>
    setPicked((p) => {
      const cur = p[q.question] ?? [];
      if (!q.multiSelect) return { ...p, [q.question]: cur[0] === opt ? [] : [opt] };
      return {
        ...p,
        [q.question]: cur.includes(opt) ? cur.filter((x) => x !== opt) : [...cur, opt],
      };
    });

  // 每一问都答了才让提交：少答一题，模型拿到的就是一段缺了半句的话
  const complete = questions.every(
    (q) => (picked[q.question] ?? []).length > 0 || (free[q.question] ?? '').trim() !== '',
  );

  const submit = () => {
    const answers: Record<string, string> = {};
    for (const q of questions) {
      // 形状沿用桌面：键是问题原文，值用 ", " 连接（选了选项又自己补一句 = 两段）
      const parts = [...(picked[q.question] ?? [])];
      const extra = (free[q.question] ?? '').trim();
      if (extra) parts.push(extra);
      if (parts.length > 0) answers[q.question] = parts.join(', ');
    }
    onAnswer(answers);
  };

  return (
    <View style={[styles.card, { borderColor: t.borderStrong, backgroundColor: t.panel }]}>
      <Text style={[styles.cardHead, { color: t.accent2 }]}>
        它想确认{questions.length > 1 ? `${questions.length} 件事` : '一件事'}
      </Text>
      {questions.map((q) => (
        <View key={q.question} style={{ marginTop: 10 }}>
          <Text style={[styles.question, { color: t.text, marginTop: 0 }]}>{q.question}</Text>
          {q.options?.map((opt) => {
            const on = (picked[q.question] ?? []).includes(opt);
            return (
              <Pressable
                key={opt}
                onPress={() => toggle(q, opt)}
                style={[
                  styles.option,
                  {
                    borderColor: on ? t.accentLine : t.border,
                    backgroundColor: on ? t.accentSoft : 'transparent',
                  },
                ]}
              >
                <View
                  style={[
                    styles.radio,
                    // 多选画方框，单选画圆点：让人一眼看出能不能选好几个
                    q.multiSelect ? { borderRadius: 4 } : null,
                    { borderColor: on ? t.accent : t.borderStrong },
                  ]}
                >
                  {on ? <View style={[styles.radioDot, { backgroundColor: t.accent }]} /> : null}
                </View>
                <Text style={{ color: t.text, fontSize: 13.5, flex: 1 }}>{opt}</Text>
              </Pressable>
            );
          })}
          <View style={[styles.freeInput, { backgroundColor: t.inputBg, borderColor: t.border }]}>
            <TextInput
              value={free[q.question] ?? ''}
              onChangeText={(text) => setFree((f) => ({ ...f, [q.question]: text }))}
              placeholder="或者自己写一句…"
              placeholderTextColor={t.faint}
              style={{ color: t.text, fontSize: 13, padding: 0 }}
              multiline
            />
          </View>
        </View>
      ))}
      <View style={styles.cardActions}>
        <Pressable
          disabled={!complete}
          onPress={submit}
          style={({ pressed }) => [
            styles.btnPrimary,
            {
              backgroundColor: pressed ? t.accentStrong : t.accent,
              opacity: complete ? 1 : 0.45,
            },
          ]}
        >
          <Text style={{ color: t.onAccent, fontSize: 13, fontWeight: '600' }}>回答</Text>
        </Pressable>
        <Pressable onPress={onDecline} style={styles.btnFlat}>
          <Text style={{ color: t.faint, fontSize: 13 }}>跳过</Text>
        </Pressable>
      </View>
    </View>
  );
}

/** SendFile 交付过来的东西：图片直接把缩略图铺在气泡里，文档是一张卡片 */
function FileCard({
  file,
  source,
  onOpen,
}: {
  file: AttachmentRef;
  source?: FileSource | null;
  onOpen: (id: string) => void;
}) {
  const { tokens: t } = useTheme();
  const isImage = file.mimeType.startsWith('image/');
  const sizeText = file.size > 1024 * 1024 ? `${(file.size / 1048576).toFixed(1)} MB` : `${Math.round(file.size / 1024)} KB`;

  if (isImage)
    return (
      <Pressable
        onPress={() => onOpen(file.id)}
        style={[styles.imageCard, { borderColor: t.borderStrong, backgroundColor: t.panel }]}
      >
        {source ? (
          <Image
            source={{ uri: source.url, headers: source.headers }}
            style={[styles.thumb, { backgroundColor: t.wash }]}
            resizeMode="cover"
          />
        ) : (
          <View style={[styles.imagePh, { backgroundColor: t.wash }]}>
            <Feather name="image" size={26} color={t.faint} />
          </View>
        )}
        <View style={styles.fileMeta}>
          <Text style={{ color: t.text, fontSize: 13, fontWeight: '600' }} numberOfLines={1}>
            {file.name}
          </Text>
          <Text style={{ color: t.faint, fontSize: 11, marginTop: 2 }}>
            图片 · {sizeText}
          </Text>
        </View>
      </Pressable>
    );

  return (
    <Pressable
      onPress={() => onOpen(file.id)}
      style={[styles.fileCard, { borderColor: t.borderStrong, backgroundColor: t.panel }]}
    >
      <View style={[styles.fileIcon, { backgroundColor: t.wash }]}>
        <Feather name="file-text" size={17} color={t.accent2} />
      </View>
      <View style={styles.fileMeta}>
        <Text style={{ color: t.text, fontSize: 13, fontWeight: '600' }} numberOfLines={1}>
          {file.name}
        </Text>
        <Text style={{ color: t.faint, fontSize: 11, marginTop: 2 }}>
          文本 · {sizeText}
        </Text>
      </View>
      <Feather name="chevron-right" size={16} color={t.faint} />
    </Pressable>
  );
}

/** 思考：默认折叠（它很长，而且多半不是你要找的那句） */
export function Thinking({ text }: { text: string }) {
  const { tokens: t } = useTheme();
  const [open, setOpen] = useState(false);
  return (
    <View style={[styles.think, { borderLeftColor: t.borderStrong }]}>
      <Pressable onPress={() => setOpen((v) => !v)} style={styles.thinkHead}>
        <Feather name={open ? 'chevron-down' : 'chevron-right'} size={12} color={t.faint} />
        <Text style={{ color: t.faint, fontSize: 12 }}>思考</Text>
      </Pressable>
      {open ? (
        <Text style={{ color: t.faint, fontSize: 12.5, lineHeight: 19, marginTop: 4 }}>{text}</Text>
      ) : null}
    </View>
  );
}

/** SendFile 调用参数里的那一个 path —— 记录里它永远是 {"path": "..."} */
function fileFromSendFileArgs(msg: SessionMessage): AttachmentRef | null {
  if (!msg.args) return null;
  let path = '';
  try {
    const v = JSON.parse(msg.args) as { path?: unknown };
    if (typeof v?.path === 'string') path = v.path;
  } catch {
    return null;
  }
  if (!path) return null;
  const name = path.split(/[\\/]/).pop() || path;
  const ext = name.slice(name.lastIndexOf('.') + 1).toLowerCase();
  const mime: Record<string, string> = {
    png: 'image/png',
    jpg: 'image/jpeg',
    jpeg: 'image/jpeg',
    gif: 'image/gif',
    webp: 'image/webp',
    svg: 'image/svg+xml',
    pdf: 'application/pdf',
    md: 'text/markdown',
    txt: 'text/plain',
    json: 'application/json',
  };
  return {
    id: msg.toolCallId ?? '',
    name,
    mimeType: mime[ext] ?? 'application/octet-stream',
    // 参数里没有大小：卡片照常显示名字和图标，下载留给有记录的那一条
    size: 0,
    downloadable: false,
  };
}

export const MessageView = memo(function MessageView({
  msg,
  toolResult,
  running,
  ask,
  attachments,
  fileSource,
  onDecide,
  onAnswer,
  onDecline,
  onOpenFile,
}: {
  msg: SessionMessage;
  toolResult?: SessionMessage;
  /** 工具还在跑（只有实时那一轮会给） */
  running?: boolean;
  ask?: PendingAsk;
  attachments: AttachmentRef[];
  fileSource?: (a: AttachmentRef) => FileSource | null;
  /** 带 toolId：卡片自己绑不上它，见下面的 PermissionCard 调用 */
  onDecide: (toolId: string, allow: boolean) => void;
  onAnswer: (answers: Record<string, string>) => void;
  onDecline: () => void;
  onOpenFile: (id: string) => void;
}) {
  const { tokens: t } = useTheme();

  if (ask?.kind === 'permission')
    return (
      <PermissionCard
        preview={ask.preview}
        toolName={ask.toolName}
        onDecide={(allow) => onDecide(ask.toolId, allow)}
      />
    );
  if (ask?.kind === 'question')
    return <QuestionCard questions={ask.questions} onAnswer={onAnswer} onDecline={onDecline} />;

  if (msg.role === 'user')
    return (
      <View style={[styles.userBubble, { backgroundColor: t.userBg }]}>
        <Markdown text={stripReminders(msg.displayContent ?? msg.content)} ink={t.userInk} />
        {msg.timestamp ? (
          <Text style={[styles.msgTime, { color: t.userInk, opacity: 0.55, textAlign: 'right' }]}>
            {formatMessageTime(msg.timestamp)}
          </Text>
        ) : null}
      </View>
    );

  if (msg.role === 'assistant') {
    return (
      <View>
        {msg.thinking ? <Thinking text={msg.thinking} /> : null}
        <AssistantText text={msg.content} />
        {msg.timestamp ? (
          <Text style={[styles.msgTime, { color: t.faint }]}>{formatMessageTime(msg.timestamp)}</Text>
        ) : null}
      </View>
    );
  }

  if (msg.role === 'tool_call') {
    // 后端的附件列表是权威（它有大小、可下载标志）；没有那一条时从调用参数里兜一个出来 ——
    // 否则一个刚跑完、快照还没带上附件的 SendFile 会退回成一张普通工具卡，读者看不出
    // 模型到底递了个什么文件过来。
    const file =
      attachments.find((a) => a.id === msg.toolCallId) ??
      (msg.toolName === 'SendFile' ? fileFromSendFileArgs(msg) : null);
    if (file) return <FileCard file={file} source={fileSource?.(file)} onOpen={onOpenFile} />;
    return (
      <ToolCard
        name={msg.toolName ?? ''}
        title={msg.title}
        args={msg.args}
        result={toolResult?.toolResult}
        isError={toolResult?.isError}
        running={running}
      />
    );
  }

  // tool_result 由它上面那条 tool_call 一起画
  if (msg.role === 'tool_result') return null;

  // 系统提醒不在手机上展示：它跟着某一轮被注入，原样显示只会把真正的回复挤走
  if (msg.role === 'reminder') return null;

  return null;
});

const styles = StyleSheet.create({
  body: { paddingVertical: 4 },
  assistantText: { fontSize: 15, lineHeight: 25 },
  codeBlock: { borderRadius: 8, padding: 10, marginVertical: 8 },
  // 一条消息的时刻：小、灰、贴着它的气泡 —— 它是给「这是什么时候说的」这个问题的，
  // 不该跟正文抢注意力（手机上这一屏的字本来就密）。
  msgTime: { fontSize: 10.5, marginTop: 4 },
  userBubble: {
    alignSelf: 'flex-end',
    maxWidth: '82%',
    borderRadius: 16,
    borderBottomRightRadius: 5,
    paddingHorizontal: 14,
    paddingVertical: 9,
    marginTop: 8,
    marginBottom: 10,
  },
  think: { borderLeftWidth: 2, paddingLeft: 10, marginVertical: 6, opacity: 0.9 },
  thinkHead: { flexDirection: 'row', alignItems: 'center', gap: 5 },

  tool: { borderWidth: 1, borderRadius: 10, marginVertical: 7, overflow: 'hidden' },
  toolHead: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingHorizontal: 10, paddingVertical: 9 },
  toolName: { fontSize: 12.5, fontWeight: '600' },
  toolSum: { flex: 1, fontSize: 12, fontFamily: 'monospace' },
  toolBody: { borderTopWidth: StyleSheet.hairlineWidth, padding: 10 },

  card: { borderWidth: 1, borderRadius: 14, padding: 14, marginVertical: 12 },
  cardHead: { fontSize: 12, fontWeight: '600' },
  cardNote: { fontSize: 11.5, marginTop: 8 },
  cmd: { borderRadius: 8, padding: 10, marginTop: 9 },
  cardActions: { flexDirection: 'row', alignItems: 'center', gap: 10, marginTop: 12 },
  btnPrimary: { borderRadius: 10, paddingHorizontal: 14, paddingVertical: 9 },
  btnGhost: { borderRadius: 10, borderWidth: 1, paddingHorizontal: 14, paddingVertical: 9 },
  btnFlat: { paddingHorizontal: 8, paddingVertical: 9 },

  question: { fontSize: 14, lineHeight: 21, marginTop: 9, marginBottom: 4 },
  option: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 9,
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: 11,
    paddingVertical: 10,
    marginTop: 7,
  },
  radio: { width: 15, height: 15, borderRadius: 8, borderWidth: 1.5, alignItems: 'center', justifyContent: 'center' },
  radioDot: { width: 7, height: 7, borderRadius: 4 },
  freeInput: { borderWidth: 1, borderRadius: 10, paddingHorizontal: 11, paddingVertical: 10, marginTop: 7 },

  fileCard: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 11,
    borderWidth: 1,
    borderRadius: 10,
    padding: 11,
    marginVertical: 8,
  },
  fileIcon: { width: 34, height: 34, borderRadius: 8, alignItems: 'center', justifyContent: 'center' },
  fileMeta: { flex: 1, minWidth: 0 },
  imageCard: { borderWidth: 1, borderRadius: 10, overflow: 'hidden', marginVertical: 8, maxWidth: 320 },
  thumb: { width: '100%', height: 170 },
  imagePh: { height: 130, alignItems: 'center', justifyContent: 'center' },
});
