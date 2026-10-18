import { Asset } from 'expo-asset';
import { File } from 'expo-file-system';
import { useEffect, useMemo, useState } from 'react';
import { StyleSheet, Text, View } from 'react-native';
import { WebView } from 'react-native-webview';
import { useTheme } from '../theme/ThemeContext';

/**
 * 回复里的 ```mermaid 围栏。
 *
 * 送进 WebView 的只有围栏里的**原文**，页面上那棵 SVG 是渲染器自己画出来的 ——
 * 和桌面的 markdown / HTML 沙箱同一条规矩：模型写的 HTML/SVG 永远不当页面内容插进去。
 * 用 strict 模式，图里内联的脚本与交互都关掉。
 *
 * mermaid 本身随 App 打包（`assets/mermaid.min.txt`），不在线取：手机上是离线可用的，
 * 第一次画图不该卡在网络上。
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

export function Mermaid({ source, height = 260 }: { source: string; height?: number }) {
  const { tokens: t } = useTheme();
  const [js, setJs] = useState<string | null>(cachedJS);

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

  const html = useMemo(() => {
    if (!js) return null;
    return `<!doctype html>
<html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,maximum-scale=1">
<style>
  html,body{margin:0;padding:0;background:transparent}
  .wrap{display:flex;align-items:center;justify-content:center;min-height:${height - 16}px;padding:8px}
  svg{max-width:100%;height:auto}
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
  }, [js, source, height, t]);

  return (
    <View style={[styles.box, { borderColor: t.border, backgroundColor: t.panel }]}>
      {html ? (
        <WebView
          originWhitelist={['*']}
          source={{ html }}
          style={{ height, backgroundColor: 'transparent' }}
          scrollEnabled={false}
        />
      ) : (
        <View style={[styles.placeholder, { height }]}>
          <Text style={{ color: t.faint, fontSize: 12 }}>正在准备图的渲染器…</Text>
        </View>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  box: { borderWidth: 1, borderRadius: 12, marginVertical: 10, overflow: 'hidden' },
  placeholder: { alignItems: 'center', justifyContent: 'center' },
});
