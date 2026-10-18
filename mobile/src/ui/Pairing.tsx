import { useState } from 'react';
import { ActivityIndicator, Pressable, StyleSheet, Text, TextInput, View } from 'react-native';
import { ping, type Pairing as PairingInfo } from '../api/client';
import { messageOfError } from '../state/mobile';
import { useTheme } from '../theme/ThemeContext';

/** 开发期的默认值，省得每次手输；换机器就改这两个 */
const DEV_URL = 'http://192.168.71.173:8787';
const DEV_TOKEN = 'dev-token';

export function Pairing({ onPaired }: { onPaired: (p: PairingInfo) => void }) {
  const { tokens: t } = useTheme();
  const [baseUrl, setBaseUrl] = useState(DEV_URL);
  const [token, setToken] = useState(DEV_TOKEN);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function test() {
    setBusy(true);
    setErr(null);
    const url = baseUrl.trim().replace(/\/+$/, '');
    try {
      const r = await ping(url);
      if (r.ok) onPaired({ baseUrl: url, token: token.trim() });
      else setErr('这台机器拒绝了这个地址');
    } catch (e) {
      setErr(messageOfError(e));
    } finally {
      setBusy(false);
    }
  }

  const field = {
    backgroundColor: t.inputBg,
    borderColor: t.border,
    color: t.text,
  };

  return (
    <View style={styles.root}>
      <Text style={[styles.title, { color: t.title }]}>配对电脑</Text>
      <Text style={[styles.sub, { color: t.faint }]}>
        在电脑上打开 Tachi 桌面端的设置页，用它给的地址和令牌。
      </Text>

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
        onPress={test}
        disabled={busy}
        style={({ pressed }) => [
          styles.btn,
          { backgroundColor: pressed || busy ? t.accentStrong : t.accent, opacity: busy ? 0.8 : 1 },
        ]}
      >
        {busy ? (
          <ActivityIndicator color={t.onAccent} size="small" />
        ) : (
          <Text style={{ color: t.onAccent, fontSize: 14, fontWeight: '600' }}>测试连接</Text>
        )}
      </Pressable>

      <Text style={[styles.note, { color: t.faint }]}>
        这条链路是局域网的明文 HTTP：拿到令牌的人就能让那台电脑执行命令。只在家里用自己的网络。
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, paddingHorizontal: 22, paddingTop: 28 },
  title: { fontSize: 26, fontWeight: '600', letterSpacing: -0.4 },
  sub: { fontSize: 13, lineHeight: 20, marginTop: 8, marginBottom: 22 },
  label: { fontSize: 12, marginBottom: 6, marginTop: 14 },
  input: { borderWidth: 1, borderRadius: 11, paddingHorizontal: 13, paddingVertical: 12, fontSize: 14 },
  errBox: { borderWidth: 1, borderRadius: 10, padding: 11, marginTop: 16 },
  btn: { borderRadius: 11, paddingVertical: 14, alignItems: 'center', marginTop: 22 },
  note: { fontSize: 11.5, lineHeight: 18, marginTop: 26 },
});
