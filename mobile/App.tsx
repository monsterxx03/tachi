import { StatusBar } from 'expo-status-bar';
import { SafeAreaView, StyleSheet, Text, View, useColorScheme } from 'react-native';
import { themes } from './src/theme/tokens';

/**
 * 骨架：现在只证明两件事 —— 主题 token 在真机上出得来、深浅色跟着系统走。
 * 真正的界面（会话列表 / 聊天 / 设置）在这一层之下展开。
 */
export default function App() {
  const scheme = useColorScheme();
  const isDark = scheme !== 'light';
  const t = themes[isDark ? 'dark' : 'light'];

  return (
    <SafeAreaView style={[styles.root, { backgroundColor: t.bg }]}>
      <StatusBar style={isDark ? 'light' : 'dark'} />
      <View style={[styles.header, { backgroundColor: t.bgTop, borderBottomColor: t.border }]}>
        <Text style={[styles.title, { color: t.title }]}>会话</Text>
        <View style={[styles.accentDot, { backgroundColor: t.accent }]} />
      </View>

      <View style={styles.body}>
        <Text style={[styles.hint, { color: t.faint }]}>还没有配对电脑</Text>
        <Text style={[styles.sub, { color: t.dim }]}>
          {isDark ? '深色' : '浅色'} · 配色取自桌面那份 token
        </Text>
      </View>
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    paddingHorizontal: 20,
    paddingVertical: 14,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  title: { fontSize: 22, fontWeight: '600' },
  accentDot: { width: 8, height: 8, borderRadius: 4 },
  body: { flex: 1, alignItems: 'center', justifyContent: 'center', gap: 6 },
  hint: { fontSize: 16 },
  sub: { fontSize: 13 },
});
