import { useState } from 'react';
import { ActivityIndicator, Pressable, StyleSheet, Text, TextInput, View } from 'react-native';
import { Feather } from '@expo/vector-icons';
import type { Pairing as PairingInfo } from '../api/client';
import { normalizeAddresses, probePairing } from '../api/addresses';
import { deviceLabel } from '../api/label';
import { EdgeBackZone, useEdgeBack } from './edgeBack';
import { messageOfError } from '../state/mobile';
import Animated from 'react-native-reanimated';
import { useTheme } from '../theme/ThemeContext';
import { ScanPairing } from './ScanPairing';

/** 开发期的默认值，省得每次手输；换机器就改这两个 */
const DEV_URL = 'http://192.168.71.173:8787';
const DEV_TOKEN = 'dev-token';

/** 配对页交给上层的东西：名字与地址表都是探测顺手带回来的，探测失败时它们就不在 */
interface PairedMeta {
  label?: string;
  hostname?: string;
  urls?: string[];
  /** 那台电脑的稳定身份（ping 报的）；推送点开时要靠它认机器 */
  deviceId?: string;
  /** 一条地址都没应答：照样存下来，但界面上要说一句「现在连不上」 */
  unreachable?: boolean;
}

export function Pairing({
  onPaired,
  onClose,
}: {
  onPaired: (p: PairingInfo, meta?: PairedMeta) => void;
  /** 从设置页「添加电脑」进来时给一个返回；第一台配对时没有 */
  onClose?: () => void;
}) {
  const { tokens: t } = useTheme();
  const [baseUrl, setBaseUrl] = useState(DEV_URL);
  const [token, setToken] = useState(DEV_TOKEN);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [scanning, setScanning] = useState(false);
  // 从设置页「添加电脑」进来时才有返回；首次配对没有上一屏，手势自然无效
  const back = useEdgeBack(onClose);

  async function test(given?: PairingInfo) {
    const target: PairingInfo = given ?? {
      baseUrl: baseUrl.trim().replace(/\/+$/, ''),
      token: token.trim(),
    };
    // 手输的地址可能是空的、或者带了个别的 scheme。存下一条永远连不上的设备没有意义，
    // 所以这里先拦一句 —— 扫码那条路进的地址由解析器保证形状。
    if (normalizeAddresses(target).length === 0) {
      setErr('地址要写成 http://192.168.1.10:8787 这样');
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      // 那台电脑可能不止一条地址（局域网 + Tailscale）：一起试、谁先应答用谁。只试排第一的
      // 那条的话，人在外面时它必然不通，而这里的请求没有自己的上限 —— 页面就一直转圈，
      // 要等手机系统判 TCP 超时才给一句「连不上」。
      const outcome = await probePairing(target);
      // 通不通都记下来：探测只用来挑地址、取名字，不是配对的门禁（见 api/addresses.ts 的
      // probePairing）。之后手机会自己继续找路，那台电脑一开就会连上。
      onPaired(
        outcome.pairing,
        outcome.ping
          ? {
              label: deviceLabel(outcome.ping, outcome.pairing.baseUrl),
              hostname: outcome.ping.hostname,
              urls: outcome.pairing.urls,
              deviceId: outcome.ping.deviceId,
            }
          : { unreachable: true },
      );
    } catch (e) {
      setErr(messageOfError(e));
    } finally {
      setBusy(false);
    }
  }

  if (scanning)
    return (
      <ScanPairing
        onClose={() => setScanning(false)}
        onScanned={(p) => {
          setScanning(false);
          setBaseUrl(p.baseUrl);
          setToken(p.token);
          void test(p);
        }}
      />
    );

  const field = {
    backgroundColor: t.inputBg,
    borderColor: t.border,
    color: t.text,
  };

  return (
      <Animated.View style={[styles.root, back.style]}>
      {onClose ? (
        <Pressable
          onPress={onClose}
          style={({ pressed }) => [styles.back, { backgroundColor: pressed ? t.wash : 'transparent' }]}
        >
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
      ) : null}
      <Text style={[styles.title, { color: t.title }]}>配对电脑</Text>
      <Text style={[styles.sub, { color: t.faint }]}>
        在电脑上打开 Tachi 桌面端的设置页，用它给的地址和令牌。
      </Text>

      <Pressable
        onPress={() => setScanning(true)}
        style={({ pressed }) => [
          styles.scanBtn,
          { borderColor: t.borderStrong, backgroundColor: pressed ? t.wash : 'transparent' },
        ]}
      >
        <Feather name="camera" size={17} color={t.accent2} />
        <Text style={{ color: t.accent2, fontSize: 13.5, fontWeight: '600' }}>扫电脑上的二维码</Text>
      </Pressable>

      <View style={styles.orRow}>
        <View style={[styles.rule, { backgroundColor: t.border }]} />
        <Text style={{ color: t.faint, fontSize: 11.5 }}>或者手输</Text>
        <View style={[styles.rule, { backgroundColor: t.border }]} />
      </View>

      <Text style={[styles.label, { color: t.faint }]}>地址</Text>
      <TextInput
        value={baseUrl}
        onChangeText={setBaseUrl}
        autoCapitalize="none"
        autoCorrect={false}
        keyboardType="url"
        placeholder="http://192.168.1.10:8787"
        placeholderTextColor={t.faint}
        style={[styles.input, field]}
      />
      <Text style={[styles.note, { color: t.faint, marginTop: 6 }]}>
        模拟器可以填 127.0.0.1（配 adb reverse）；真机填电脑的任意一个地址——局域网的和 Tailscale 的都行，
        扫码会把它们一起记住。
      </Text>

      <Text style={[styles.label, { color: t.faint }]}>令牌</Text>
      <TextInput
        value={token}
        onChangeText={setToken}
        autoCapitalize="none"
        autoCorrect={false}
        secureTextEntry
        placeholder="粘贴令牌"
        placeholderTextColor={t.faint}
        style={[styles.input, field]}
      />

      {err ? (
        <View style={[styles.errBox, { backgroundColor: t.redWash, borderColor: t.redLine }]}>
          <Text style={{ color: t.red, fontSize: 12.5 }}>{err}</Text>
        </View>
      ) : null}

      <Pressable
        onPress={() => void test()}
        disabled={busy}
        style={({ pressed }) => [
          styles.btn,
          { backgroundColor: pressed || busy ? t.accentStrong : t.accent, opacity: busy ? 0.8 : 1 },
        ]}
      >
        {busy ? (
          <View style={styles.btnBusy}>
            <ActivityIndicator color={t.onAccent} size="small" />
            <Text style={{ color: t.onAccent, fontSize: 14, fontWeight: '600' }}>正在找这台电脑</Text>
          </View>
        ) : (
          <Text style={{ color: t.onAccent, fontSize: 14, fontWeight: '600' }}>测试连接</Text>
        )}
      </Pressable>

      <Text style={[styles.note, { color: t.faint }]}>
        链路是明文 HTTP：走局域网时同一个 Wi-Fi 下的人能看见，走 Tailscale 时那段包在 Tailscale 的加密隧道里。
        两种情况下，拿到令牌的人都能让那台电脑执行命令。
      </Text>
        <EdgeBackZone gesture={back.gesture} />
      </Animated.View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, paddingHorizontal: 22, paddingTop: 28 },
  back: { width: 34, height: 34, borderRadius: 10, alignItems: 'center', justifyContent: 'center', marginBottom: 6 },
  title: { fontSize: 26, fontWeight: '600', letterSpacing: -0.4 },
  sub: { fontSize: 13, lineHeight: 20, marginTop: 8, marginBottom: 18 },
  scanBtn: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 8,
    borderWidth: 1,
    borderRadius: 10,
    paddingVertical: 13,
  },
  orRow: { flexDirection: 'row', alignItems: 'center', gap: 10, marginTop: 18, marginBottom: 2 },
  rule: { flex: 1, height: StyleSheet.hairlineWidth },
  label: { fontSize: 12, marginBottom: 6, marginTop: 14 },
  input: { borderWidth: 1, borderRadius: 10, paddingHorizontal: 13, paddingVertical: 12, fontSize: 14 },
  errBox: { borderWidth: 1, borderRadius: 10, padding: 11, marginTop: 16 },
  btn: { borderRadius: 10, paddingVertical: 14, alignItems: 'center', marginTop: 22 },
  btnBusy: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  note: { fontSize: 11.5, lineHeight: 18, marginTop: 26 },
});

