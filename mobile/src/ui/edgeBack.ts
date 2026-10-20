import { useMemo } from 'react';
import { Dimensions } from 'react-native';
import { Gesture } from 'react-native-gesture-handler';
import { Easing, runOnJS, useAnimatedStyle, useSharedValue, withTiming } from 'react-native-reanimated';

/**
 * 左缘那条「手势区」的宽度：只认从这里起手的划动。
 *
 * 16 不是随手取的数：正文的左右内边距就是 16，所以代码块、表格这些**从 16 开始**，
 * 手势区和它们不重叠 —— 在代码块上横划永远落不到这个手势里。
 */
const EDGE_BACK_ZONE = 16;
/** 页面跟着手指走的整屏宽度 */
const SCREEN = Dimensions.get('window').width;

/**
 * 从屏幕左缘往右划 = 返回上一屏。**手势与动画都在 UI 线程**（gesture-handler + reanimated），
 * 和列表里那一行左滑是同一条路。
 *
 * 三条判据，缺一条都会出问题：
 *
 * - **起点必须在左缘**（`onBegin` 里看 `absoluteX`）。正文里的代码块、表格、mermaid 各有
 *   自己的横向操作，从正文中间起手的横划不该被这个手势吃掉。
 * - **右划 10px 才激活、竖着动 16px 就放弃**（`activeOffsetX` / `failOffsetY`）：竖划归列表
 *   滚动。`PanResponder` 那版是「先抢到再说」，所以正文里的横向滚动会被它夺走 —— 表现就是
 *   在代码块上划一下就退回列表。
 * - **内层的横向滚动优先**：GH 里原生滚动手势（ScrollView）的优先级高于 JS 手势，所以手指
 *   落在代码块上时，这一划归代码块。这也是为什么这个 hook 不再用 `PanResponder`。
 *
 * 跟手：整屏按位移右移，松手时超过三成屏宽（或者甩得够快）就补完这段动画再返回，否则弹回。
 *
 * **Android 上这条手势归系统**（手势导航的左缘返回），App 收不到 —— 那边由 App 的
 * BackHandler 做（见 App.tsx）。这里覆盖 iOS，以及用三键导航的 Android。
 */
export function useEdgeBack(onBack?: () => void) {
  const tx = useSharedValue(0);
  // 这一划是不是从左缘起手的（不是的话，手势照旧会激活，但页面一动不动）
  const armed = useSharedValue(false);

  const gesture = useMemo(
    () =>
      Gesture.Pan()
        .enabled(!!onBack)
        // 往右 10px 才认；竖着动 16px 就让给列表
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
