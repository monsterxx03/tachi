import { Feather } from '@expo/vector-icons';
import { useEffect, useMemo, useState } from 'react';
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  useWindowDimensions,
  View,
} from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from 'react-native-reanimated';
import { api, type Pairing } from '../api/client';
import type { AttachmentRef } from '../api/types';
import { useTheme } from '../theme/ThemeContext';
import { fileKind } from './fileKind';
import { Markdown } from './Markdown';
import { ZoomableImage } from './Zoomable';

/** 1 倍就是 1 倍：捏合与双击都会让它离 1 有一点小数级的偏差 */
const BASE_SCALE = 1.01;
/** 拖过屏幕高度的这个比例就关掉，短于此弹回去 */
const DISMISS_RATIO = 0.14;
/** 甩得比这快就不用拖那么远（pt/s） */
const DISMISS_VELOCITY = 900;

/**
 * 图片那一屏：捏合、放大后拖动、双击来回**归缩放**（见 ./Zoomable），
 * **竖直往下拖 = 关掉**（相册那张图的手感）。原来只有一个返回箭头在左上角。
 *
 * 两套手势不打架，靠的是同一个 `scale`：库自己的平移在按下那一刻发现只有 1 倍就主动
 * fail（`useGestures` 的 `onTouchesDown`），所以 1 倍时纵向只剩这一条；放大之后反过来，
 * 我们这条在按下时让位 —— 放大过的图要能上下拖到边角，不能一拖就关屏。
 */
function ImagePane({
  source,
  onClose,
}: {
  source: { uri: string; headers?: Record<string, string> };
  onClose: () => void;
}) {
  const { height } = useWindowDimensions();
  const scale = useSharedValue(1);
  const ty = useSharedValue(0);

  const pan = useMemo(
    () =>
      Gesture.Pan()
        // 放大着的时候让位给库的平移：它一激活，我们就不该存在
        .onTouchesDown((_e, manager) => {
          if (scale.value > BASE_SCALE) manager.fail();
        })
        // 竖着划够 12px 才认领；横着先划够 14px 就放弃（横划归缩放/平移）
        .activeOffsetY([-12, 12])
        .failOffsetX([-14, 14])
        .onUpdate((e) => {
          // 只跟往下：往上拖没有对应的动作，停在原地
          ty.value = Math.max(0, e.translationY);
        })
        .onEnd((e) => {
          const done = ty.value > height * DISMISS_RATIO || e.velocityY > DISMISS_VELOCITY;
          if (done) {
            ty.value = withTiming(height, { duration: 160, easing: Easing.out(Easing.quad) }, () => {
              runOnJS(onClose)();
            });
            return;
          }
          ty.value = withTiming(0, { duration: 150, easing: Easing.out(Easing.cubic) });
        })
        // 被别处接管（比如双指捏合赢了）：归位，别停在半路
        .onFinalize((_e, success) => {
          if (!success) ty.value = withTiming(0, { duration: 120, easing: Easing.out(Easing.cubic) });
        }),
    [height, onClose, scale, ty],
  );

  // 拖下去的同时淡一点：让人看得出这一下是「把它送走」，不是把它挪到别处
  const style = useAnimatedStyle(() => ({
    transform: [{ translateY: ty.value }],
    opacity: 1 - Math.min(0.6, ty.value / (height * 0.7)),
  }));

  return (
    <GestureDetector gesture={pan}>
      <Animated.View style={[styles.imageHost, style]}>
        <ZoomableImage source={source} scale={scale} />
      </Animated.View>
    </GestureDetector>
  );
}

/**
 * 附件阅读器：手机上能看的，只有 agent 在这段对话里用 SendFile 交出去的那几个。
 *
 * 取文件走带令牌的下载端点，端点只认交付记录里的 id（不接受路径）——
 * 所以这里拿到的永远是"这段对话真的交付过"的东西。
 */
export function FileViewer({
  pairing,
  sessionId,
  file,
  onClose,
}: {
  pairing: Pairing;
  sessionId: string;
  file: AttachmentRef;
  onClose: () => void;
}) {
  const { tokens: t } = useTheme();
  const kind = fileKind(file);
  const isImage = kind === 'image';
  const isMarkdown = kind === 'markdown';
  // markdown 也是取回来的一段文本，只是拿去渲染而不是摊开
  const isText = kind === 'text' || isMarkdown;
  const [text, setText] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const [loading, setLoading] = useState(isText);

  const sizeText =
    file.size > 1024 * 1024 ? `${(file.size / 1048576).toFixed(1)} MB` : `${Math.round(file.size / 1024)} KB`;

  useEffect(() => {
    if (!isText) return;
    let alive = true;
    const { url, headers } = api.file(pairing, sessionId, file);
    fetch(url, { headers })
      .then((r) => {
        if (!r.ok) throw new Error(String(r.status));
        return r.text();
      })
      .then((body) => {
        if (alive) setText(body);
      })
      .catch(() => {
        if (alive) setFailed(true);
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [pairing, sessionId, file, isText]);

  const remote = api.file(pairing, sessionId, file);

  return (
    <View style={[styles.root, { backgroundColor: t.bg }]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onClose} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <View style={styles.headInfo}>
          <Text style={[styles.title, { color: t.title }]} numberOfLines={1}>
            {file.name}
          </Text>
          <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }}>
            {isImage ? '图片' : isMarkdown ? 'Markdown' : isText ? '文本' : '其他'} · {sizeText}
          </Text>
        </View>
      </View>

      {isImage ? (
        // 捏合放大 / 拖动 / 双击（见 ./Zoomable），往下拖关闭（见上面的 ImagePane）。
        // 原来那个 ScrollView 的 maximumZoomScale 只有 iOS 认，安卓上是一张不会动的图。
        <ImagePane source={{ uri: remote.url, headers: remote.headers }} onClose={onClose} />
      ) : isMarkdown ? (
        // 一份 md 是给人读的，不是给人看源码的：交给正文同一个渲染器（表格、列表、
        // 代码块都有；围栏里的 mermaid 也是那张图），外层只负责滚动和内边距。
        <ScrollView contentContainerStyle={styles.textWrap}>
          {loading ? (
            <ActivityIndicator color={t.faint} style={{ marginTop: 30 }} />
          ) : failed ? (
            <Text style={{ color: t.red, fontSize: 13 }}>取不到这个文件了。</Text>
          ) : (
            <View style={[styles.docCard, { backgroundColor: t.panel, borderColor: t.border }]}>
              <Markdown text={text ?? ''} />
            </View>
          )}
        </ScrollView>
      ) : isText ? (
        <ScrollView contentContainerStyle={styles.textWrap}>
          {loading ? (
            <ActivityIndicator color={t.faint} style={{ marginTop: 30 }} />
          ) : failed ? (
            <Text style={{ color: t.red, fontSize: 13 }}>取不到这个文件了。</Text>
          ) : (
            <Text style={[styles.doc, { color: t.text, backgroundColor: t.panel, borderColor: t.border }]}>
              {text}
            </Text>
          )}
        </ScrollView>
      ) : (
        <View style={styles.notice}>
          <Feather name="external-link" size={22} color={t.faint} />
          <Text style={{ color: t.faint, fontSize: 13, marginTop: 10, textAlign: 'center' }}>
            这种文件手机打不开，在电脑上打开。
          </Text>
        </View>
      )}
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
  title: { fontSize: 14.5, fontWeight: '600' },
  imageHost: { flex: 1, padding: 16 },
  textWrap: { padding: 16 },
  doc: {
    borderWidth: 1,
    borderRadius: 10,
    padding: 14,
    fontSize: 13,
    lineHeight: 21,
    fontFamily: 'monospace',
  },
  /** 渲染出来的 markdown：同一张卡片，但里面是正文样式，所以不要 monospace、不要行高。 */
  docCard: { borderWidth: 1, borderRadius: 10, padding: 14, overflow: 'hidden' },
  notice: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 30 },
});
