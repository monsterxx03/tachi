import { ImageZoom } from '@likashefqet/react-native-image-zoom';
import { StyleSheet, View } from 'react-native';
import type { SharedValue } from 'react-native-reanimated';

/**
 * 一张能放大看的图：双指捏合、放大后拖动、双击在 1 倍和 2.5 倍之间来回。
 *
 * 手势交给 `@likashefqet/react-native-image-zoom`（它内部就是 gesture-handler 的
 * Pinch / Pan / Tap 三件套，边界与惯性都算好了）。之前这里是自己写的一版：双击能放大，但
 * 捏合和拖动在 iOS 与安卓上都没有反应 —— 这种「手势组合」的细节自己拼，两边平台都很容易漏。
 *
 * 图片地址要带令牌，所以走 `source`（库默认只认 `uri`，会把 headers 丢掉，那样图是空的）。
 *
 * `scale` 可以把库的缩放倍数交出来（它自己那个是内部的）：阅读器靠它判断「现在是不是 1 倍」，
 * 从而决定竖直拖动归谁 —— 见 FileViewer 里的 `ImagePane`。
 */
export function ZoomableImage({
  source,
  scale,
}: {
  source: { uri: string; headers?: Record<string, string> };
  scale?: SharedValue<number>;
}) {
  return (
    <View style={styles.host}>
      <ImageZoom
        source={source}
        scale={scale}
        minScale={1}
        maxScale={5}
        doubleTapScale={2.5}
        resizeMode="contain"
        style={styles.image}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  host: { flex: 1 },
  // 库内部会把这张图当 flex:1 的容器用；明确给它一个能算尺寸的框
  image: { flex: 1, width: '100%' },
});
