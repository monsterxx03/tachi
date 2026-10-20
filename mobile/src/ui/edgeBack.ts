import { useRef } from 'react';
import { PanResponder } from 'react-native';

/** 左缘那条「手势区」的宽度：够窄，不至于把正文里的横向操作吃掉 */
const EDGE_BACK_ZONE = 24;
/** 划够这么远才算数 */
const EDGE_BACK_DISTANCE = 64;

/**
 * 从屏幕左缘往右划 = 返回上一屏。
 *
 * 只认「起点在左缘」的手势：消息里的表格、代码块、mermaid 都有各自的横向操作，
 * 不能让它们被这一个手势吃掉。
 *
 * **Android 上这条手势归系统**（手势导航的左缘返回），App 收不到 —— 那边对应的事情
 * 由 App 的 BackHandler 做（见 App.tsx）。这里覆盖的是 iOS，以及用三键导航的 Android。
 */
export function useEdgeBack(onBack?: () => void) {
  const cb = useRef(onBack);
  cb.current = onBack;
  const pan = useRef(
    PanResponder.create({
      onMoveShouldSetPanResponder: (_e, g) =>
        g.x0 <= EDGE_BACK_ZONE && g.dx > 12 && Math.abs(g.dx) > Math.abs(g.dy) * 1.5,
      onPanResponderRelease: (_e, g) => {
        if (g.dx >= EDGE_BACK_DISTANCE) cb.current?.();
      },
      onPanResponderTerminationRequest: () => true,
    }),
  ).current;
  return pan.panHandlers;
}
