import { useMemo } from 'react';
import { StyleSheet, View } from 'react-native';
import { WebView } from 'react-native-webview';
import { useTheme } from '../theme/ThemeContext';

/**
 * 回复里的 ```mermaid 围栏。
 *
 * 送进 WebView 的只有围栏里的**原文**，页面上那棵 SVG 是渲染器自己画出来的 ——
 * 和桌面的 markdown / HTML 沙箱同一条规矩：模型写的 HTML/SVG 永远不当页面内容插进去。
 * 用 strict 模式，图里内联的脚本与交互都关掉。
 *
 * 待办：正式实现要把 mermaid 随 App 离线打包（现在是从 npm 镜像取，第一次要联网）。
 */
const MERMAID_URL = 'https://registry.npmmirror.com/mermaid/12.2.0/files/dist/mermaid.min.js';

function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export function Mermaid({ source, height = 260 }: { source: string; height?: number }) {
  const { tokens: t, isDark } = useTheme();

  const html = useMemo(() => {
    return `<!doctype html>
<html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,maximum-scale=1">
<script src="${MERMAID_URL}"></script>
<style>
  html,body{margin:0;padding:0;background:transparent}
  .wrap{display:flex;align-items:center;justify-content:center;min-height:${height - 16}px;padding:8px}
  svg{max-width:100%;height:auto}
</style></head>
<body>
<div class="wrap"><pre class="mermaid">${escapeHtml(source)}</pre></div>
<script>
  mermaid.initialize({
    startOnLoad: true,
    securityLevel: 'strict',
    theme: 'base',
    themeVariables: {
      background: '${t.panel}',
      primaryColor: '${t.panel2}',
      primaryTextColor: '${t.text}',
      primaryBorderColor: '${t.accent}',
      lineColor: '${t.accentLine}',
      secondaryColor: '${t.raise}',
      tertiaryColor: '${t.sid}',
      fontFamily: '-apple-system, system-ui, sans-serif',
      fontSize: '13px'
    },
    flowchart: { curve: 'basis', useMaxWidth: true }
  });
</script>
</body></html>`;
  }, [source, height, t]);

  return (
    <View style={[styles.box, { borderColor: t.border, backgroundColor: t.panel }]}>
      <WebView
        originWhitelist={['*']}
        source={{ html }}
        style={{ height, backgroundColor: 'transparent' }}
        scrollEnabled={false}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  box: { borderWidth: 1, borderRadius: 12, marginVertical: 10, overflow: 'hidden' },
});
