import { useMemo } from 'react';
import { Dimensions, StyleSheet, View } from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import { Easing, runOnJS, useAnimatedStyle, useSharedValue, withTiming } from 'react-native-reanimated';

/**
 * 左缘那条「手势区」的宽度：只认从这里起手的划动。
 *
 * 16 不是随手取的数：正文的左右内边距就是 16，所以代码块、表格这些**从 16 开始**，
 * 手势区和它们不重叠 —— 在代码块上横划永远落不到这个手势里。
 */
const EDGE_BACK_ZONE = 16;
/** 页面跟着手指走的整屏宽度；会话列表那一页的整页手势也用这个上界 */
export const SCREEN = Dimensions.get('window').width;

/**
 * 从屏幕左缘往右划 = 返回上一屏。**手势与动画都在 UI 线程**（gesture-handler + reanimated），
 * 和列表里那一行左滑是同一条路。
 *
 * **手势层只盖住左缘那一条**（`EdgeBackZone`），不铺满整屏 —— 这一条是硬要求：
 * 铺满整屏时手势层是列表的**父级**，手指一落下就先被它接住，`failOffsetY` 那句「竖着划
 * 就让给列表」不成立（真机上实测：点击有效、整个列表滑不动）。让手势层根本不覆盖正文，
 * 滚动就不再取决于手势库怎么判优先级。
 *
 * 窄条里仍然要判方向：
 *
 * - **起点必须在左缘**（`onBegin` 里看 `absoluteX`）—— 现在窄条本身已经保证了。
 * - **右划 10px 才激活、竖着动 16px 就放弃**（`activeOffsetX` / `failOffsetY`）：窄条上的
 *   竖划不该把页面拖走。
 *
 * 跟手：整屏按位移右移（`style` 照旧挂在页面根节点上），松手时超过三成屏宽（或者甩得够快）
 * 就补完这段动画再返回，否则弹回。手势一旦激活，手指移出窄条也继续跟。
 *
 * **Android 上系统手势导航会先截走左缘这一划**，所以那里多数时候走的是 App 的 BackHandler
 * （见 App.tsx）；这条手势覆盖 iOS 与用三键导航的 Android。
 */
export function useEdgeBack(onBack?: () => void) {
  const tx = useSharedValue(0);
  // 这一划是不是从左缘起手的（不是的话，手势照旧会激活，但页面一动不动）
  const armed = useSharedValue(false);

  const gesture = useMemo(
    () =>
      Gesture.Pan()
        .enabled(!!onBack)
        // 往右 10px 才认；竖着动 16px 就放弃
        .activeOffsetX([10, SCREEN])
        .failOffsetY([-16, 16])
        .onBegin((e) => {
          armed.value = e.absoluteX <= EDGE_BACK_ZONE;
        })
        .onUpdate((e) => {
          if (!armed.value) return;
          tx.value = Math.max(0, Math.min(SCREEN, e.translationX));
        })
        .onEnd((e) => {
          if (!armed.value) {
            tx.value = 0;
            return;
          }
          const done = tx.value > SCREEN * 0.3 || e.velocityX > 900;
          if (done && onBack) {
            // 先补完「推出去」这一段，再真的换屏：中途换屏会闪
            tx.value = withTiming(SCREEN, { duration: 180, easing: Easing.out(Easing.quad) }, () => {
              runOnJS(onBack)();
            });
            return;
          }
          tx.value = withTiming(0, { duration: 160, easing: Easing.out(Easing.cubic) });
        })
        .onFinalize(() => {
          armed.value = false;
        }),
    [onBack, armed, tx],
  );

  const style = useAnimatedStyle(() => ({ transform: [{ translateX: tx.value }] }));
  return { gesture, style };
}

/**
 * 挂手势的那一条。**放在页面根节点里、内容之后**（绝对定位盖在上面），宽度就是
 * `EDGE_BACK_ZONE`。它只有这么窄，正文上的滑动与它无关 —— 这是列表能滚的前提。
 */
export function EdgeBackZone({ gesture }: { gesture: ReturnType<typeof Gesture.Pan> }) {
  return (
    <GestureDetector gesture={gesture}>
      <View style={styles.zone} />
    </GestureDetector>
  );
}

/** 整页右滑：往右推够这么多才认领（单个正数只设那一端 —— 见下面那段窗口语义） */
const PAGE_SWIPE = 32;

/**
 * 整页往右推 = 进下一个页面（现在只有会话列表页用它：推到设置页 —— 选电脑的那一屏）。
 *
 * **为什么不是左缘那一条。** 那看起来更对称（和返回同一个起手位置），但在 Android 上
 * 左缘那一划归系统的返回手势，App 根本收不到 —— 实测就是直接退回桌面。所以这一页只能
 * 铺满：整页的任何位置往右推都算。
 *
 * **铺满的代价是阈值必须给对。** gesture-handler 的两端是「窗口」语义：数组 `[a, b]` 表示
 * `dx < a` 或 `dx > b` 都算激活 —— 写成 `[12, SCREEN]` 的话 `dx < 12` 恒成立，真机上手指
 * 一动就激活，点击、长按、行左滑全被这一层抢走（这一屏就只剩右滑好用，这是踩过的）。
 * 单个正数只设右半边，32px 远超点击与长按的手指抖动，而真正的划动远不止这些。
 *
 * 行的手势因此不用改：它的 `activeOffsetX([-8, 8])` 只在往左那半边赢，往右落不到它手里。
 */
export function usePageSwipe(onSwipe?: () => void) {
  const tx = useSharedValue(0);

  const gesture = useMemo(
    () =>
      Gesture.Pan()
        .enabled(!!onSwipe)
        // 只认往右，够 32px 才认领；竖着动 18px 就交给列表去滚
        .activeOffsetX(PAGE_SWIPE)
        .failOffsetY([-18, 18])
        .onUpdate((e) => {
          tx.value = Math.max(0, Math.min(SCREEN, e.translationX));
        })
        .onEnd((e) => {
          const done = tx.value > SCREEN * 0.3 || e.velocityX > 900;
          if (done && onSwipe) {
            // 和返回那条一样：先把「推走」补完，再真的换屏 —— 中途换会闪
            tx.value = withTiming(SCREEN, { duration: 180, easing: Easing.out(Easing.quad) }, () => {
              runOnJS(onSwipe)();
            });
            return;
          }
          tx.value = withTiming(0, { duration: 160, easing: Easing.out(Easing.cubic) });
        }),
    [onSwipe, tx],
  );

  const style = useAnimatedStyle(() => ({ transform: [{ translateX: tx.value }] }));
  return { gesture, style };
}

const styles = StyleSheet.create({
  zone: {
    position: 'absolute',
    left: 0,
    top: 0,
    bottom: 0,
    width: EDGE_BACK_ZONE,
    zIndex: 10,
  },
});
