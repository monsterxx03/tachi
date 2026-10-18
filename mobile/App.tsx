import { StatusBar } from 'expo-status-bar';
import { useState } from 'react';
import { StyleSheet, Text, View } from 'react-native';
import { SafeAreaProvider, SafeAreaView } from 'react-native-safe-area-context';
import { useMobile } from './src/state/mobile';
import { ThemeProvider, useTheme } from './src/theme/ThemeContext';
import { Chat } from './src/ui/Chat';
import { Pairing } from './src/ui/Pairing';
import { SessionList } from './src/ui/SessionList';
import { Settings } from './src/ui/Settings';

/** 一条常驻的提示（连不上、令牌失效、服务端的拒绝原因） */
function NoticeBar({ text }: { text: string }) {
  const { tokens: t } = useTheme();
  return (
    <View style={[styles.notice, { backgroundColor: t.redWash, borderBottomColor: t.border }]}>
      <View style={[styles.noticeDot, { backgroundColor: t.red }]} />
      <Text style={{ color: t.red, fontSize: 12, flex: 1 }}>{text}</Text>
    </View>
  );
}

function Shell() {
  const { tokens: t, isDark } = useTheme();
  const m = useMobile();
  const [showSettings, setShowSettings] = useState(false);

  return (
    <SafeAreaView style={[styles.root, { backgroundColor: t.bg }]} edges={['top', 'bottom']}>
      <StatusBar style={isDark ? 'light' : 'dark'} />

      {!m.ready ? null : !m.pairing ? (
        <Pairing onPaired={m.pair} />
      ) : showSettings ? (
        <Settings
          pairing={m.pairing}
          conn={m.conn}
          onBack={() => setShowSettings(false)}
          onUnpair={() => {
            setShowSettings(false);
            void m.unpair();
          }}
        />
      ) : m.openId && m.chat.snapshot ? (
        <>
          {m.error ? <NoticeBar text={m.error} /> : null}
          <Chat
            snapshot={m.chat.snapshot}
            live={m.chat.live}
            onBack={m.close}
            onDecide={(toolId, allow) => void m.decide(toolId, allow ? 'allow_once' : 'deny')}
            onAnswer={(_toolId, text) => {
              // 答案的形状沿用桌面：键是问题原文
              const q = m.chat.snapshot?.asks[0];
              if (q && q.kind === 'question') {
                void m.answer(text ? { [q.questions[0].question]: text } : {});
              }
            }}
            onOpenInfo={() => {}}
            onOpenModels={() => {}}
            onOpenFile={() => {}}
            onSend={m.send}
            onStop={() => m.send('', 'interrupt')}
          />
        </>
      ) : (
        <>
          {m.error ? <NoticeBar text={m.error} /> : null}
          <SessionList
            sessions={m.sessions}
            currentId={m.openId}
            conn={m.conn}
            onOpen={(id) => void m.open(id)}
            onSettings={() => setShowSettings(true)}
            onRefresh={() => void m.refresh()}
          />
        </>
      )}
    </SafeAreaView>
  );
}

export default function App() {
  return (
    <SafeAreaProvider>
      <ThemeProvider>
        <Shell />
      </ThemeProvider>
    </SafeAreaProvider>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  notice: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 8,
    paddingVertical: 7,
    paddingHorizontal: 16,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  noticeDot: { width: 7, height: 7, borderRadius: 3.5 },
});
