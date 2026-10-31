import { Asset } from 'expo-asset';
import { File } from 'expo-file-system';
import { useEffect, useMemo, useState } from 'react';
import { Modal, Pressable, StyleSheet, Text, View } from 'react-native';
import { WebView } from 'react-native-webview';
import { useSafeAreaInsets } from 'react-native-safe-area-context';
import { useTheme } from '../theme/ThemeContext';
import type { ThemeTokens } from '../theme/tokens';

/**
 * 回复里的 ```mermaid 围栏。
 *
 * 送进 WebView 的只有围栏里的**原文**，页面上那棵 SVG 是渲染器自己画出来的 ——
 * 和桌面的 markdown / HTML 沙箱同一条规矩：模型写的 HTML/SVG 永远不当页面内容插进去。
 * 用 strict 模式，图里内联的脚本与交互都关掉。
 *
 * mermaid 本身随 App 打包（`assets/mermaid.min.txt`），不在线取：手机上是离线可用的，
 * 第一次画图不该卡在网络上。
 *
 * **点一下这张图会铺满整屏并允许缩放**：对话里的那一份是按宽度等比缩过的，节点里的字在手机上
 * 常常小到看不清。放大那一份是同一个渲染器、同一份原文，只是 viewport 放开了缩放 —— 图是 SVG，
 * 放大是矢量级的清晰，不是把一张位图拉大。
 */
let cachedJS: string | null = null;

async function loadMermaidJS(): Promise<string> {
  if (cachedJS) return cachedJS;
  const asset = Asset.fromModule(require('../../assets/mermaid.min.txt'));
  if (!asset.localUri) await asset.downloadAsync();
  cachedJS = await new File(asset.localUri ?? asset.uri).text();
  return cachedJS;
}

function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/** 行内那份的 viewport：不允许缩放（整张图就嵌在消息里，缩放归放大那一份） */
const INLINE_VIEWPORT = 'width=device-width,initial-scale=1,maximum-scale=1';
/** 放大那份：允许捏合。上限 6 倍够看节点里的字，再大就只剩手指在屏幕上打滑 */
const ZOOM_VIEWPORT = 'width=device-width,initial-scale=1,user-scalable=yes,maximum-scale=6';
/** 放大那份图四周留的空（页面内边距与竖直居中时的下限），px */
const ZOOM_PADDING = 16;

/**
 * 同一段 HTML 的两个尺寸。
 *
 * zoom=false：按宽度铺满、背景透明（衬在消息那张卡上），画完把真实高度报回 RN。
 * zoom=true：铺满整屏、背景是页面底色、允许缩放；不报高度 —— 它自己就是整屏。
 */
function buildMermaidHtml(
  js: string | null,
  source: string,
  theme: ThemeTokens,
  zoom: boolean,
): string | null {
  if (!js) return null;
  const background = zoom ? theme.bg : 'transparent';
  const wrapPadding = zoom ? `${ZOOM_PADDING}px` : '8px';
  const svgSize = zoom ? 'width:100%;height:auto' : 'max-width:100%;height:auto';
  const reportHeight = !zoom;
  return `<!doctype html>
<html><head><meta charset="utf-8">
<meta name="viewport" content="${zoom ? ZOOM_VIEWPORT : INLINE_VIEWPORT}">
<style>
  html,body{margin:0;padding:0;background:${background}}
  .wrap{display:flex;align-items:center;justify-content:center;padding:${wrapPadding}}
  svg{${svgSize}}
</style></head>
<body>
<div class="wrap"><pre class="mermaid">${escapeHtml(source)}</pre></div>
<script>${js}</script>
<script>
  mermaid.initialize({
    startOnLoad: true,
    securityLevel: 'strict',
    theme: 'base',
    themeVariables: {
      background: '${theme.panel}',
      primaryColor: '${theme.panel2}',
      primaryTextColor: '${theme.text}',
      primaryBorderColor: '${theme.accent}',
      lineColor: '${theme.accentLine}',
      secondaryColor: '${theme.raise}',
      tertiaryColor: '${theme.sid}',
      fontFamily: '-apple-system, system-ui, sans-serif',
      fontSize: '13px'
    },
    flowchart: { curve: 'basis', useMaxWidth: true }
  });

  // 画完把真实高度报回 RN（mermaid 是异步渲染的，所以轮询等它出现）
  if (${reportHeight}) (function () {
    var tries = 0;
    function report() {
      var svg = document.querySelector('.wrap svg');
      if (svg) {
        var h = Math.ceil(svg.getBoundingClientRect().height) + 24;
        if (window.ReactNativeWebView) window.ReactNativeWebView.postMessage(String(h));
        return;
      }
      if (tries++ < 60) setTimeout(report, 100);
    }
    report();
  })();

  // 放大那一份：图比屏幕短就竖直居中（贴着顶上，刘海机和状态栏会把图头吃掉一块）；
  // 比屏幕长就维持贴着上边 —— 居中的话上半截会跑到滚不到的地方去。
  if (${zoom}) (function () {
    var tries = 0;
    function center() {
      var svg = document.querySelector('.wrap svg');
      if (!svg) {
        if (tries++ < 60) setTimeout(center, 100);
        return;
      }
      var h = svg.getBoundingClientRect().height;
      var top = Math.max(${ZOOM_PADDING}, Math.round((window.innerHeight - h) / 2));
      document.querySelector('.wrap').style.paddingTop = top + 'px';
    }
    center();
  })();
</script>
</body></html>`;
}

export function Mermaid({ source, height = 220 }: { source: string; height?: number }) {
  const { tokens: t } = useTheme();
  const insets = useSafeAreaInsets();
  const [js, setJs] = useState<string | null>(cachedJS);
  // 图有多高只有画完才知道。写死一个高度就会把下半张裁掉，而 WebView 里又滚不动
  // （scrollEnabled=false），所以让页面量完把高度报回来。
  const [boxHeight, setBoxHeight] = useState(height);
  const [zoomed, setZoomed] = useState(false);

  useEffect(() => {
    if (js) return;
    let alive = true;
    void loadMermaidJS()
      .then((s) => {
        if (alive) setJs(s);
      })
      .catch(() => {
        // 读不出来就停在占位上，别把整条消息拖垮
      });
    return () => {
      alive = false;
    };
  }, [js]);

  const html = useMemo(() => buildMermaidHtml(js, source, t, false), [js, source, t]);
  const zoomHtml = useMemo(() => buildMermaidHtml(js, source, t, true), [js, source, t]);

  return (
    <>
      <View style={[styles.box, { borderColor: t.border, backgroundColor: t.panel }]}>
        {html ? (
          <>
            <WebView
              originWhitelist={['*']}
              source={{ html }}
              style={{ height: boxHeight, backgroundColor: 'transparent' }}
              scrollEnabled={false}
              onMessage={(e) => {
                const h = Number(e.nativeEvent.data);
                // 只接受合理的高度：太小是量到了没画完的盒子，太大是异常
                if (Number.isFinite(h) && h > 60) setBoxHeight(Math.min(Math.ceil(h), 1600));
              }}
            />
            {/* WebView 自己会吃掉点击，所以放大这一下来自盖在它上面的一层透明 Pressable */}
            <Pressable
              style={StyleSheet.absoluteFill}
              onPress={() => setZoomed(true)}
              accessibilityRole="button"
              accessibilityLabel="放大这张图"
            />
          </>
        ) : (
          <View style={[styles.placeholder, { height: boxHeight }]}>
            <Text style={{ color: t.faint, fontSize: 12 }}>正在准备图的渲染器…</Text>
          </View>
        )}
      </View>

      <Modal
        visible={zoomed}
        animationType="fade"
        // 安卓的返回键 = 关掉，和点空白处同一条路
        onRequestClose={() => setZoomed(false)}
      >
        <View
          style={[
            styles.zoomRoot,
            {
              backgroundColor: t.bg,
              // 整屏的 Modal 从 y=0 开始，刘海和状态栏会把图头吃掉：让出安全区。
              paddingTop: insets.top,
              paddingBottom: insets.bottom,
              paddingLeft: insets.left,
              paddingRight: insets.right,
            },
          ]}
        >
          {zoomHtml ? (
            <WebView
              originWhitelist={['*']}
              source={{ html: zoomHtml }}
              style={{ flex: 1, backgroundColor: t.bg }}
              // 安卓要显式打开内置缩放，iOS 靠页面里那个 viewport 就够了
              setBuiltInZoomControls
              setDisplayZoomControls={false}
            />
          ) : null}
          <Pressable
            onPress={() => setZoomed(false)}
            style={[
              styles.closeBtn,
              {
                top: insets.top + 12,
                right: insets.right + 16,
                backgroundColor: t.panel,
                borderColor: t.border,
              },
            ]}
            accessibilityRole="button"
            accessibilityLabel="关闭"
          >
            <Text style={{ color: t.text, fontSize: 16 }}>✕</Text>
          </Pressable>
        </View>
      </Modal>
    </>
  );
}

const styles = StyleSheet.create({
  box: { borderWidth: 1, borderRadius: 10, marginVertical: 10, overflow: 'hidden' },
  placeholder: { alignItems: 'center', justifyContent: 'center' },
  zoomRoot: { flex: 1 },
  closeBtn: {
    // top / right 由安全区决定（见上面那个 Pressable）
    position: 'absolute',
    width: 34,
    height: 34,
    borderRadius: 17,
    borderWidth: 1,
    alignItems: 'center',
    justifyContent: 'center',
  },
});
