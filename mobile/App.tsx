import { StatusBar } from 'expo-status-bar';
import { useEffect, useState } from 'react';
import { BackHandler, StyleSheet, Text, useWindowDimensions, View } from 'react-native';
import { SafeAreaProvider, SafeAreaView } from 'react-native-safe-area-context';
import type { AttachmentRef } from './src/api/types';
import { api } from './src/api/client';
import type { MobileApi } from './src/state/mobile';
import { useMobile } from './src/state/mobile';
import { ThemeProvider, useTheme } from './src/theme/ThemeContext';
import { Chat } from './src/ui/Chat';
import { FileViewer } from './src/ui/FileViewer';
import { Pairing } from './src/ui/Pairing';
import { SessionInfo } from './src/ui/SessionInfo';
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

/** 对话那一栏该显示什么：层压在它上面，然后是聊天，最后是空态 */
function ChatPane({
  m,
  showInfo,
  setShowInfo,
  openFile,
  setOpenFile,
}: {
  m: MobileApi;
  showInfo: boolean;
  setShowInfo: (v: boolean) => void;
  openFile: AttachmentRef | null;
  setOpenFile: (f: AttachmentRef | null) => void;
}) {
  const { tokens: t } = useTheme();
  const inChat = Boolean(m.openId && m.chat.snapshot);

  if (openFile && m.pairing && m.openId)
    return <FileViewer pairing={m.pairing} sessionId={m.openId} file={openFile} onClose={() => setOpenFile(null)} />;

  if (showInfo && inChat && m.chat.snapshot)
    return (
      <SessionInfo
        snapshot={m.chat.snapshot}
        providers={m.providers}
        onClose={() => setShowInfo(false)}
        onPickModel={(name) => void m.setProvider(name)}
        onToggleSkip={(skip) => void m.setSkipApproval(skip)}
      />
    );

  if (inChat && m.chat.snapshot)
    return (
      <Chat
        snapshot={m.chat.snapshot}
        live={m.chat.live}
        fileSource={(a) => (m.pairing && m.openId ? api.file(m.pairing, m.openId, a) : null)}
        onBack={m.close}
        onDecide={(toolId, allow) => void m.decide(toolId, allow ? 'allow_once' : 'deny')}
        onAnswer={(_toolId, text) => {
          const q = m.chat.snapshot?.asks[0];
          if (q && q.kind === 'question') void m.answer(text ? { [q.questions[0].question]: text } : {});
        }}
        onOpenInfo={() => setShowInfo(true)}
        onOpenModels={() => setShowInfo(true)}
        onOpenFile={(id) => {
          const f = m.chat.snapshot?.attachments.find((a) => a.id === id);
          if (f) setOpenFile(f);
        }}
        onSend={m.send}
        onStop={() => m.send('', 'interrupt')}
      />
    );

  return (
    <View style={styles.emptyPane}>
      <Text style={{ color: t.faint, fontSize: 13 }}>从左边挑一段对话</Text>
    </View>
  );
}

function Shell() {
  const { tokens: t, isDark } = useTheme();
  const m = useMobile();
  const { width } = useWindowDimensions();
  const wide = width >= 768; // 平板：列表和对话并排
  const [showSettings, setShowSettings] = useState(false);
  const [showInfo, setShowInfo] = useState(false);
  const [openFile, setOpenFile] = useState<AttachmentRef | null>(null);

  // Android 的返回键：先关掉打开的层，再退出一段对话，最后才让系统去退出 App
  useEffect(() => {
    const sub = BackHandler.addEventListener('hardwareBackPress', () => {
      if (openFile) {
        setOpenFile(null);
        return true;
      }
      if (showInfo) {
        setShowInfo(false);
        return true;
      }
      if (showSettings) {
        setShowSettings(false);
        return true;
      }
      if (m.openId) {
        m.close();
        return true;
      }
      return false;
    });
    return () => sub.remove();
  }, [openFile, showInfo, showSettings, m]);

  const list = (
    <SessionList
      sessions={m.sessions}
      currentId={m.openId}
      conn={m.conn}
      onOpen={(id) => void m.open(id)}
      onSettings={() => setShowSettings(true)}
      onRefresh={() => void m.refresh()}
    />
  );

  if (!m.ready) return <SafeAreaView style={[styles.root, { backgroundColor: t.bg }]} edges={['top', 'bottom']} />;

  return (
    <SafeAreaView style={[styles.root, { backgroundColor: t.bg }]} edges={['top', 'bottom']}>
      <StatusBar style={isDark ? 'light' : 'dark'} />
      {m.pairing && m.error ? <NoticeBar text={m.error} /> : null}

      {!m.pairing ? (
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
      ) : wide ? (
        <View style={styles.wide}>
          <View style={[styles.wideSide, { borderRightColor: t.border, backgroundColor: t.sid }]}>{list}</View>
          <View style={styles.wideMain}>
            <ChatPane
              m={m}
              showInfo={showInfo}
              setShowInfo={setShowInfo}
              openFile={openFile}
              setOpenFile={setOpenFile}
            />
          </View>
        </View>
      ) : m.openId && m.chat.snapshot ? (
        <ChatPane
          m={m}
          showInfo={showInfo}
          setShowInfo={setShowInfo}
          openFile={openFile}
          setOpenFile={setOpenFile}
        />
      ) : (
        list
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
  wide: { flex: 1, flexDirection: 'row' },
  wideSide: { width: 320, borderRightWidth: StyleSheet.hairlineWidth },
  wideMain: { flex: 1 },
  emptyPane: { flex: 1, alignItems: 'center', justifyContent: 'center' },
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
