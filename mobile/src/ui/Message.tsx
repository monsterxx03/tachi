import { Feather } from '@expo/vector-icons';
import { useState } from 'react';
import { Image, Pressable, StyleSheet, Text, View } from 'react-native';
import type { FileSource } from '../api/client';
import type { AgentQuestion, AttachmentRef, PendingAsk, SessionMessage } from '../api/types';
import { useTheme } from '../theme/ThemeContext';
import { Mermaid } from './Mermaid';

/** 把回复切成「文本 / 代码块 / 图」三种块 —— 只认围栏，不做完整 markdown */
type Block =
  | { kind: 'text'; text: string }
  | { kind: 'code'; lang: string; text: string }
  | { kind: 'mermaid'; text: string };

function parseBlocks(raw: string): Block[] {
  const out: Block[] = [];
  const re = /```([\w-]*)\n([\s\S]*?)```/g;
  let last = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(raw))) {
    if (m.index > last) out.push({ kind: 'text', text: raw.slice(last, m.index) });
    const lang = m[1] ?? '';
    out.push(lang === 'mermaid' ? { kind: 'mermaid', text: m[2] } : { kind: 'code', lang, text: m[2] });
    last = re.lastIndex;
  }
  if (last < raw.length) out.push({ kind: 'text', text: raw.slice(last) });
  return out;
}

/** 行内 `code` 与换行 */
function TextBlock({ text, style }: { text: string; style: object }) {
  const { tokens: t } = useTheme();
  const lines = text.replace(/\n+$/, '').split('\n');
  return (
    <Text style={style}>
      {lines.map((line, li) => (
        <Text key={li}>
          {line.split(/(`[^`]+`)/g).map((part, pi) =>
            part.startsWith('`') && part.endsWith('`') && part.length > 2 ? (
              <Text key={pi} style={{ color: t.codeInk, backgroundColor: t.codeBg }}>
                {part.slice(1, -1)}
              </Text>
            ) : (
              <Text key={pi}>{part}</Text>
            ),
          )}
          {li < lines.length - 1 ? '\n' : ''}
        </Text>
      ))}
    </Text>
  );
}

/** 助手的一段回复 */
function AssistantText({ text }: { text: string }) {
  const { tokens: t } = useTheme();
  const blocks = parseBlocks(text);
  return (
    <View style={styles.body}>
      {blocks.map((b, i) => {
        if (b.kind === 'mermaid') return <Mermaid key={i} source={b.text} />;
        if (b.kind === 'code')
          return (
            <View key={i} style={[styles.codeBlock, { backgroundColor: t.codeBg }]}>
              <Text style={{ color: t.codeInk, fontFamily: 'monospace', fontSize: 12.5 }}>{b.text.replace(/\n$/, '')}</Text>
            </View>
          );
        return <TextBlock key={i} text={b.text} style={[styles.assistantText, { color: t.text }]} />;
      })}
    </View>
  );
}

/** 工具调用：一行，点开看结果 */
function ToolCard({ msg, result }: { msg: SessionMessage; result?: SessionMessage }) {
  const { tokens: t } = useTheme();
  const [open, setOpen] = useState(false);
  const failed = result?.isError;

  return (
    <View style={[styles.tool, { borderColor: t.border, backgroundColor: t.wash }]}>
      <Pressable onPress={() => setOpen((v) => !v)} style={styles.toolHead}>
        <Feather
          name={open ? 'chevron-up' : 'chevron-down'}
          size={13}
          color={t.faint}
          style={{ marginTop: 1 }}
        />
        <Text style={[styles.toolName, { color: failed ? t.red : t.dim }]} numberOfLines={1}>
          {msg.toolName}
        </Text>
        <Text style={[styles.toolSum, { color: t.faint }]} numberOfLines={1}>
          {msg.title || msg.args || ''}
        </Text>
      </Pressable>
      {open && result?.toolResult ? (
        <View style={[styles.toolBody, { borderTopColor: t.border }]}>
          <Text style={{ color: failed ? t.red : t.dim, fontFamily: 'monospace', fontSize: 12 }}>
            {result.toolResult.trimEnd()}
          </Text>
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
function QuestionCard({ questions, onAnswer }: { questions: AgentQuestion[]; onAnswer: (text: string) => void }) {
  const { tokens: t } = useTheme();
  const [picked, setPicked] = useState<Record<string, string>>({});
  const [free, setFree] = useState('');
  const first = questions[0];

  return (
    <View style={[styles.card, { borderColor: t.borderStrong, backgroundColor: t.panel }]}>
      <Text style={[styles.cardHead, { color: t.accent2 }]}>它想确认一件事</Text>
      <Text style={[styles.question, { color: t.text }]}>{first.question}</Text>
      {first.options?.map((opt) => {
        const on = picked[first.question] === opt;
        return (
          <Pressable
            key={opt}
            onPress={() => setPicked((p) => ({ ...p, [first.question]: opt }))}
            style={[
              styles.option,
              { borderColor: on ? t.accentLine : t.border, backgroundColor: on ? t.accentSoft : 'transparent' },
            ]}
          >
            <View style={[styles.radio, { borderColor: on ? t.accent : t.borderStrong }]}>
              {on ? <View style={[styles.radioDot, { backgroundColor: t.accent }]} /> : null}
            </View>
            <Text style={{ color: t.text, fontSize: 13.5 }}>{opt}</Text>
          </Pressable>
        );
      })}
      <View style={[styles.freeInput, { backgroundColor: t.inputBg, borderColor: t.border }]}>
        <Text
          style={{ color: free ? t.text : t.faint, fontSize: 13 }}
          onPress={() => setFree(free ? '' : '只改登录页，别的先不动')}
        >
          {free || '或者自己写一句…'}
        </Text>
      </View>
      <View style={styles.cardActions}>
        <Pressable
          disabled={!picked[first.question] && !free}
          onPress={() => onAnswer(picked[first.question] ?? free)}
          style={({ pressed }) => [
            styles.btnPrimary,
            {
              backgroundColor: pressed ? t.accentStrong : t.accent,
              opacity: picked[first.question] || free ? 1 : 0.45,
            },
          ]}
        >
          <Text style={{ color: t.onAccent, fontSize: 13, fontWeight: '600' }}>回答</Text>
        </Pressable>
        <Pressable onPress={() => onAnswer('')} style={styles.btnFlat}>
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

export function MessageView({
  msg,
  toolResult,
  ask,
  attachments,
  fileSource,
  onDecide,
  onAnswer,
  onOpenFile,
}: {
  msg: SessionMessage;
  toolResult?: SessionMessage;
  ask?: PendingAsk;
  attachments: AttachmentRef[];
  fileSource?: (a: AttachmentRef) => FileSource | null;
  onDecide: (allow: boolean) => void;
  onAnswer: (text: string) => void;
  onOpenFile: (id: string) => void;
}) {
  const { tokens: t } = useTheme();

  if (ask?.kind === 'permission')
    return (
      <PermissionCard preview={ask.preview} toolName={ask.toolName} onDecide={onDecide} />
    );
  if (ask?.kind === 'question')
    return <QuestionCard questions={ask.questions} onAnswer={onAnswer} />;

  if (msg.role === 'user')
    return (
      <View style={[styles.userBubble, { backgroundColor: t.userBg }]}>
        <TextBlock text={msg.displayContent ?? msg.content} style={{ color: t.userInk, fontSize: 15, lineHeight: 22 }} />
      </View>
    );

  if (msg.role === 'assistant') {
    return (
      <View>
        {msg.thinking ? (
          <View style={[styles.think, { borderLeftColor: t.borderStrong }]}>
            <Text style={{ color: t.faint, fontSize: 12.5, lineHeight: 19 }}>{msg.thinking}</Text>
          </View>
        ) : null}
        <AssistantText text={msg.content} />
      </View>
    );
  }

  if (msg.role === 'tool_call') {
    const file = attachments.find((a) => a.id === msg.toolCallId);
    if (file) return <FileCard file={file} source={fileSource?.(file)} onOpen={onOpenFile} />;
    return <ToolCard msg={msg} result={toolResult} />;
  }

  // tool_result 由它上面那条 tool_call 一起画
  if (msg.role === 'tool_result') return null;

  if (msg.role === 'reminder')
    return (
      <Text style={{ color: t.faint, fontSize: 12.5, marginVertical: 6 }}>{msg.content}</Text>
    );

  return null;
}

const styles = StyleSheet.create({
  body: { paddingVertical: 4 },
  assistantText: { fontSize: 15, lineHeight: 25 },
  codeBlock: { borderRadius: 8, padding: 10, marginVertical: 8 },
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

  tool: { borderWidth: 1, borderRadius: 11, marginVertical: 7, overflow: 'hidden' },
  toolHead: { flexDirection: 'row', alignItems: 'center', gap: 7, paddingHorizontal: 10, paddingVertical: 9 },
  toolName: { fontSize: 12.5, fontWeight: '600' },
  toolSum: { flex: 1, fontSize: 12, fontFamily: 'monospace' },
  toolBody: { borderTopWidth: StyleSheet.hairlineWidth, padding: 10 },

  card: { borderWidth: 1, borderRadius: 14, padding: 14, marginVertical: 12 },
  cardHead: { fontSize: 12, fontWeight: '600' },
  cardNote: { fontSize: 11.5, marginTop: 8 },
  cmd: { borderRadius: 8, padding: 10, marginTop: 9 },
  cardActions: { flexDirection: 'row', alignItems: 'center', gap: 10, marginTop: 12 },
  btnPrimary: { borderRadius: 9, paddingHorizontal: 14, paddingVertical: 9 },
  btnGhost: { borderRadius: 9, borderWidth: 1, paddingHorizontal: 14, paddingVertical: 9 },
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
    borderRadius: 12,
    padding: 11,
    marginVertical: 8,
  },
  fileIcon: { width: 34, height: 34, borderRadius: 8, alignItems: 'center', justifyContent: 'center' },
  fileMeta: { flex: 1, minWidth: 0 },
  imageCard: { borderWidth: 1, borderRadius: 12, overflow: 'hidden', marginVertical: 8, maxWidth: 320 },
  thumb: { width: '100%', height: 170 },
  imagePh: { height: 130, alignItems: 'center', justifyContent: 'center' },
});
