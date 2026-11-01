import { Asset } from 'expo-asset';
import { File } from 'expo-file-system';
import { useEffect, useState } from 'react';
import { Modal, Pressable, StyleSheet, Text, View } from 'react-native';
import { WebView } from 'react-native-webview';
import type { Pairing } from '../api/client';
import { pairingQRPayload } from '../api/qr';
import { useTheme } from '../theme/ThemeContext';

/**
 * 一台已经配对过的电脑的配对码 —— 让第二台手机直接扫这一张，不必再走到电脑跟前。
 *
 * 二维码是**在手机本地画出来的**，不向那台电脑要图：换新手机的那一刻，恰恰可能是这台手机
 * 还连着、而那台电脑不方便碰的时候；而要这一张码的场景本身也不依赖网络。
 *
 * 编码器随包发（`assets/qrcode.min.txt`，由 esbuild 从仓库已有的 `qrcode` 依赖打成一个
 * 浏览器 IIFE，24KB），和 mermaid 同一条路：不加原生依赖，离线可用。所以它是**文本资源**，
 * 由 WebView 内联执行 —— 走 Metro 的模块系统会被当成普通 JS 模块要求导出。
 *
 * 页面只装我们自己生成的东西：那段 SVG 来自本地的编码器，`data` 是这台手机记住的地址与
 * token，没有一段是别人给的 HTML。
 */
let cachedJS: string | null = null;

async function loadQRJS(): Promise<string> {
  if (cachedJS) return cachedJS;
  const asset = Asset.fromModule(require('../../assets/qrcode.min.txt'));
  if (!asset.localUri) await asset.downloadAsync();
  cachedJS = await new File(asset.localUri ?? asset.uri).text();
  return cachedJS;
}

/** 把编码器和一份数据装进一页 HTML：页面上只留那张 SVG。 */
function pageFor(js: string, payload: string): string {
  const encoded = JSON.stringify(payload);
  return `<!doctype html><html><head><meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<style>
  html,body{margin:0;height:100%;background:transparent}
  body{display:flex;align-items:center;justify-content:center}
  svg{width:min(78vw,300px);height:auto;display:block}
</style></head>
<body><div id="q"></div>
<script>${js}</script>
<script>
  // toString 不带回调时返回一个 Promise（它自己也支持回调式写法）—— 直接把它塞进
  // innerHTML，屏幕上就是「[object Promise]」。
  Promise.resolve(QRCodeLib.toString(${encoded}, { type: 'svg', margin: 1, errorCorrectionLevel: 'M' }))
    .then(function (svg) { document.getElementById('q').innerHTML = svg; })
    .catch(function (e) { document.getElementById('q').textContent = '这个码画不出来：' + e; });
</script></body></html>`;
}

export function PairingQR({ pairing, onClose }: { pairing: (Pairing & { label?: string }) | null; onClose: () => void }) {
  const { tokens: t } = useTheme();
  const [html, setHtml] = useState<string | null>(null);

  useEffect(() => {
    if (!pairing) {
      setHtml(null);
      return;
    }
    let alive = true;
    void loadQRJS()
      .then((js) => alive && setHtml(pageFor(js, pairingQRPayload(pairing))))
      .catch(() => alive && setHtml(pageFor('', pairingQRPayload(pairing))));
    return () => {
      alive = false;
    };
  }, [pairing]);

  if (!pairing) return null;
  return (
    <Modal visible transparent animationType="fade" onRequestClose={onClose}>
      <Pressable style={styles.backdrop} onPress={onClose} accessibilityRole="button" accessibilityLabel="关闭">
        <View style={[styles.card, { backgroundColor: t.panel, borderColor: t.border }]}>
          <Text style={{ color: t.text, fontSize: 14.5, fontWeight: '600' }}>扫这一张配对</Text>
          <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 4, textAlign: 'center' }}>
            {pairing.label || pairing.baseUrl}
          </Text>
          <View style={[styles.qr, { backgroundColor: '#fff' }]}>
            {html ? (
              <WebView
                originWhitelist={['*']}
                source={{ html }}
                style={{ width: 260, height: 260, backgroundColor: 'transparent' }}
                javaScriptEnabled
                scrollEnabled={false}
              />
            ) : null}
          </View>
          <Text style={{ color: t.faint, fontSize: 11, marginTop: 10, textAlign: 'center' }}>
            新手机扫这张码，等于扫电脑上那一张：地址和令牌都在这张里。
          </Text>
        </View>
      </Pressable>
    </Modal>
  );
}

const styles = StyleSheet.create({
  backdrop: { flex: 1, alignItems: 'center', justifyContent: 'center', backgroundColor: 'rgba(0,0,0,0.45)' },
  // 尺寸都写死：WebView 没有内在高度，交给 flex 去算它会一路撑到整屏，把标题挤到状态栏后面。
  card: { width: 320, borderWidth: 1, borderRadius: 16, padding: 18, alignItems: 'center' },
  qr: { marginTop: 12, borderRadius: 10, padding: 6, width: 272, height: 272 },
});
