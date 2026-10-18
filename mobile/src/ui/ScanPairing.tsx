import { CameraView, useCameraPermissions } from 'expo-camera';
import { Feather } from '@expo/vector-icons';
import { useRef, useState } from 'react';
import { Pressable, StyleSheet, Text, View } from 'react-native';
import type { Pairing } from '../api/client';
import { parsePairingQR } from '../api/qr';
import { useTheme } from '../theme/ThemeContext';

/**
 * 扫电脑上的那张码。
 *
 * 只认协议里那个形状（见 api/qr.ts）；扫到别的东西就继续扫，不拿半截数据去连错地方。
 */
export function ScanPairing({
  onScanned,
  onClose,
}: {
  onScanned: (p: Pairing) => void;
  onClose: () => void;
}) {
  const { tokens: t } = useTheme();
  const [permission, requestPermission] = useCameraPermissions();
  const [hint, setHint] = useState<string | null>(null);
  // 扫码回调会连着触发好几次，用一个 ref 挡掉重复的
  const doneRef = useRef(false);

  if (!permission) {
    return (
      <View style={[styles.root, styles.center, { backgroundColor: t.bg }]}>
        <Text style={{ color: t.faint, fontSize: 13 }}>正在问相机权限…</Text>
      </View>
    );
  }

  if (!permission.granted) {
    return (
      <View style={[styles.root, styles.center, { backgroundColor: t.bg }]}>
        <Feather name="camera-off" size={26} color={t.faint} />
        <Text style={[styles.permissionText, { color: t.dim }]}>
          扫二维码需要相机权限。不给的话，也可以回到上一页手输地址和令牌。
        </Text>
        <Pressable
          onPress={() => void requestPermission()}
          style={({ pressed }) => [
            styles.permissionBtn,
            { backgroundColor: pressed ? t.accentStrong : t.accent },
          ]}
        >
          <Text style={{ color: t.onAccent, fontSize: 14, fontWeight: '600' }}>允许使用相机</Text>
        </Pressable>
        <Pressable onPress={onClose} style={{ marginTop: 14 }}>
          <Text style={{ color: t.faint, fontSize: 13 }}>返回手输</Text>
        </Pressable>
      </View>
    );
  }

  return (
    <View style={styles.root}>
      <CameraView
        style={StyleSheet.absoluteFill}
        facing="back"
        barcodeScannerSettings={{ barcodeTypes: ['qr'] }}
        onBarcodeScanned={
          doneRef.current
            ? undefined
            : ({ data }) => {
                const parsed = parsePairingQR(data);
                if (parsed) {
                  doneRef.current = true;
                  onScanned(parsed);
                } else {
                  // 扫到了但不是我们那张码 —— 说一句，让人挪开再扫
                  setHint('这不是电脑上的那张码');
                }
              }
        }
      />

      {/* 取景框 */}
      <View style={styles.overlay} pointerEvents="none">
        <View style={styles.frame} />
      </View>

      <View style={styles.topBar}>
        <Pressable onPress={onClose} style={styles.closeBtn}>
          <Feather name="x" size={20} color="#fff" />
        </Pressable>
      </View>

      <View style={styles.bottomBar} pointerEvents="none">
        <Text style={styles.bottomText}>
          {hint ?? '把电脑上的二维码放进取景框'}
        </Text>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: '#000' },
  center: { alignItems: 'center', justifyContent: 'center', padding: 30, backgroundColor: '#000' },
  permissionText: { fontSize: 13, lineHeight: 20, textAlign: 'center', marginTop: 12, marginBottom: 18 },
  permissionBtn: { borderRadius: 11, paddingHorizontal: 20, paddingVertical: 12 },
  overlay: { position: 'absolute', top: 0, left: 0, right: 0, bottom: 0, alignItems: 'center', justifyContent: 'center' },
  frame: {
    width: 230,
    height: 230,
    borderRadius: 18,
    borderWidth: 2,
    borderColor: 'rgba(255,255,255,0.85)',
  },
  topBar: { position: 'absolute', top: 16, left: 16 },
  closeBtn: {
    width: 36,
    height: 36,
    borderRadius: 18,
    backgroundColor: 'rgba(0,0,0,0.45)',
    alignItems: 'center',
    justifyContent: 'center',
  },
  bottomBar: { position: 'absolute', left: 0, right: 0, bottom: 60, alignItems: 'center' },
  bottomText: {
    color: 'rgba(255,255,255,0.9)',
    fontSize: 13,
    backgroundColor: 'rgba(0,0,0,0.45)',
    paddingHorizontal: 14,
    paddingVertical: 8,
    borderRadius: 18,
    overflow: 'hidden',
  },
});
