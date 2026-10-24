import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { ActivityIndicator, Pressable, StyleSheet, Text, View } from 'react-native';
import Animated, {
  Easing,
  FadeIn,
  useAnimatedStyle,
  useSharedValue,
  withRepeat,
  withTiming,
} from 'react-native-reanimated';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';

/**
 * 打开一段对话时的占位屏。
 *
 * 它存在的理由不是好看。快照要走一个来回（服务端读记录、算用量、传 100 多条消息），而在这段
 * 时间里，原来的画面是**没变过的列表** —— 点下去没反应，然后整段对话突然出现，读起来就是
 * 「卡了一下」。所以这里画的是那段对话自己的形状（标题、几行气泡、输入框），配一点呼吸，
 * 让「正在打开」看得见。
 *
 * 尺寸照着 Chat 的几块来（header 58、气泡与输入框的宽度），所以真的内容接上来时不会跳。
 * 另外它有两条出路：慢了很久说一句实话，以及一个「重试」—— 骨架如果只能转圈，那就从一个
 * 观感问题变成一个真的卡死。
 */
export function Opening({
  title,
  onBack,
  onRetry,
}: {
  /** 列表里那一行的标题：还没读到时为空，画「对话」 */
  title?: string;
  onBack: () => void;
  onRetry: () => void;
}) {
  const { tokens: t } = useTheme();
  // 呼吸的是整块占位，不是逐条：同一屏里的东西应该一起呼吸，各自为政看起来像加载失败了。
  const pulse = useSharedValue(0.4);
  useEffect(() => {
    pulse.value = withRepeat(
      withTiming(1, { duration: 900, easing: Easing.inOut(Easing.quad) }),
      -1,
      true,
    );
  }, [pulse]);
  const breathing = useAnimatedStyle(() => ({ opacity: pulse.value }));

  // 6 秒还没打开，多半是那台电脑没在应答（或者这段会话刚在别处被删了）。说一句，并给出口。
  const [slow, setSlow] = useState(false);
  useEffect(() => {
    const id = setTimeout(() => setSlow(true), 6000);
    return () => clearTimeout(id);
  }, []);

  return (
    // 淡入而不是立刻拍上去：一次快速的打开（局域网、小会话）里它只存在一瞬，硬切就是闪一下；
    // 160ms 的渐显让那种情况下几乎看不见它，而慢的那几次仍然看得见形状。
    <Animated.View entering={FadeIn.duration(160)} style={[styles.root, { backgroundColor: t.bg }]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable
          onPress={onBack}
          style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}
        >
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <View style={styles.headInfo}>
          <Text style={[styles.headTitle, { color: t.title }]} numberOfLines={1}>
            {title || '对话'}
          </Text>
          <Text style={[styles.headSub, { color: t.faint }]} numberOfLines={1}>
            正在打开…
          </Text>
        </View>
        <ActivityIndicator size="small" color={t.faint} style={styles.spinner} />
      </View>

      {/* 真内容在标题下面还有一条用量条（金额 / 缓存命中 / 模型）。不占上它，内容接上来时
          整块会往下跳 40 多像素 —— 那就是另一处「卡了一下」。 */}
      <Animated.View
        style={[styles.meta, { borderBottomColor: t.border, backgroundColor: t.bgTop }, breathing]}
      >
        <View style={[styles.metaChip, { width: 54, backgroundColor: t.washStrong }]} />
        <View style={[styles.metaChip, { width: 78, backgroundColor: t.washStrong, marginLeft: 'auto' }]} />
      </Animated.View>

      <Animated.View style={[styles.body, breathing]}>
        {/* 三组，两左一右：真内容里也是这个节奏（助手说一段、你回一句、助手再说一段）。
            单行一列摆开的话，看起来像分隔线，不像对话。 */}
        <Hint lines={['72%', '88%', '54%']} t={t} />
        <Hint lines={['46%']} mine t={t} />
        <Hint lines={['80%', '62%']} t={t} />
      </Animated.View>

      {slow ? (
        <View style={styles.slow}>
          <Text style={{ color: t.faint, fontSize: 12, textAlign: 'center', lineHeight: 18 }}>
            这段对话还没打开：那台电脑可能没在应答。
          </Text>
          <Pressable
            onPress={onRetry}
            style={({ pressed }) => [
              styles.retry,
              { borderColor: t.accentLine, backgroundColor: pressed ? t.wash : 'transparent' },
            ]}
          >
            <Feather name="refresh-cw" size={13} color={t.accent2} />
            <Text style={{ color: t.accent2, fontSize: 12.5 }}>重新打开</Text>
          </Pressable>
        </View>
      ) : null}

      {/* 输入框的位置也占上：真内容接上来时，底下不会凭空长出一条。 */}
      <Animated.View style={[styles.composer, breathing]}>
        <View style={[styles.input, { backgroundColor: t.panel, borderColor: t.border }]} />
      </Animated.View>
    </Animated.View>
  );
}

/** 一组占位「消息」：几行圆角块，左对齐或靠右（mine = 读者自己那一句）。 */
function Hint({ lines, mine, t }: { lines: string[]; mine?: boolean; t: ThemeTokens }) {
  return (
    <View style={[styles.group, mine ? styles.mine : styles.theirs]}>
      {lines.map((w, i) => (
        <View
          key={i}
          style={[styles.line, { width: w, backgroundColor: mine ? t.accentSoft : t.washStrong }]}
        />
      ))}
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
  headTitle: { fontSize: 14.5, fontWeight: '600' },
  headSub: { fontSize: 11.5, marginTop: 2 },
  spinner: { marginRight: 6 },
  // padding 与 Chat 的消息区一致，气泡从同一个左边距开始
  body: { flex: 1, paddingHorizontal: 16, paddingTop: 22 },
  meta: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
    height: 44,
    paddingHorizontal: 16,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  metaChip: { height: 9, borderRadius: 5 },
  group: { width: '100%', gap: 7, marginBottom: 22 },
  theirs: { alignItems: 'flex-start' },
  mine: { alignItems: 'flex-end' },
  line: { height: 11, borderRadius: 6 },
  slow: { paddingHorizontal: 24, paddingBottom: 10, gap: 10 },
  retry: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 6,
    borderWidth: 1,
    borderRadius: 10,
    paddingVertical: 10,
  },
  composer: { paddingHorizontal: 12, paddingBottom: 12 },
  input: { height: 44, borderWidth: 1, borderRadius: 14 },
});
