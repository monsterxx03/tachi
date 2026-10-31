import { StatusBar } from 'expo-status-bar';
import { useEffect, useState } from 'react';
import {
  AppState,
  BackHandler,
  Keyboard,
  StyleSheet,
  Text,
  useWindowDimensions,
  View,
} from 'react-native';
import { GestureHandlerRootView } from 'react-native-gesture-handler';
import { SafeAreaProvider, SafeAreaView, useSafeAreaInsets } from 'react-native-safe-area-context';
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
import { NewSession } from './src/ui/NewSession';
import { Settings } from './src/ui/Settings';
import { Opening } from './src/ui/Opening';

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
  onToggleList,
}: {
  m: MobileApi;
  showInfo: boolean;
  setShowInfo: (v: boolean) => void;
  openFile: AttachmentRef | null;
  setOpenFile: (f: AttachmentRef | null) => void;
  onToggleList?: () => void;
}) {
  const { tokens: t } = useTheme();
  const inChat = Boolean(m.openId && m.chat.snapshot);

  if (openFile && m.pairing && m.openId)
    return <FileViewer pairing={m.pairing} sessionId={m.openId} file={openFile} onClose={() => setOpenFile(null)} />;

  if (showInfo && inChat && m.chat.snapshot)
    return (
      <SessionInfo
        snapshot={m.chat.snapshot}
        live={m.chat.live}
        providers={m.providers}
        onClose={() => setShowInfo(false)}
        onPickModel={(name) => void m.setProvider(name)}
        onToggleSkip={(skip) => void m.setSkipApproval(skip)}
        loadPanels={m.loadPanels}
        cronVersion={m.cronVersion}
        onCronAction={m.cronAction}
      />
    );

  if (inChat && m.chat.snapshot)
    return (
      <Chat
        snapshot={m.chat.snapshot}
        live={m.chat.live}
        fileSource={(a) => (m.pairing && m.openId ? api.file(m.pairing, m.openId, a) : null)}
        onBack={m.close}
        onToggleList={onToggleList}
        onDecide={(toolId, allow) => void m.decide(toolId, allow ? 'allow_once' : 'deny')}
        onAnswer={(answers) => void m.answer(answers)}
        onDecline={() => void m.decline()}
        onOpenInfo={() => setShowInfo(true)}
        onOpenModels={() => setShowInfo(true)}
        onOpenFile={(id) => {
          const f = m.chat.snapshot?.attachments.find((a) => a.id === id);
          if (f) setOpenFile(f);
        }}
        onSend={m.send}
        onUpload={m.uploadFile}
        onStop={() => void m.stop()}
        pending={m.pending}
        onQueue={m.queueMessage}
        onDropPending={m.dropPending}
        onSendPending={m.sendPending}
        onClearPending={m.clearPending}
        onSendPendingNow={m.sendPendingNow}
        onLoadOlder={() => void m.loadOlder()}
        olderLoading={m.olderLoading}
        // 和列表里长按一行写的是同一个地方
        onRename={m.renameSession}
        // 窄屏就是「返回列表」；宽屏侧栏收起时把它拉回来
        onEdgeBack={onToggleList ?? m.close}
      />
    );

  // 已经点名了一段对话，但快照还没到：画它自己的骨架，而不是停在列表上。
  // 这一屏取代的正是「点了没反应」——快照要一个来回（读记录 + 算用量 + 传一百多条消息），
  // 这段时间里没有任何反馈，读起来就是卡住。
  if (m.openId) {
    const row = m.sessions.find((s) => s.id === m.openId);
    return (
      <Opening
        title={row?.title}
        onBack={onToggleList ?? m.close}
        onRetry={() => void m.open(m.openId!)}
      />
    );
  }

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
  const insets = useSafeAreaInsets();
  const wide = width >= 768; // 平板：列表和对话并排
  const [showSettings, setShowSettings] = useState(false);
  const [showNew, setShowNew] = useState(false);
  const [addingDevice, setAddingDevice] = useState(false);
  const [creating, setCreating] = useState(false);
  const [showInfo, setShowInfo] = useState(false);
  const [openFile, setOpenFile] = useState<AttachmentRef | null>(null);
  const [sideCollapsed, setSideCollapsed] = useState(false);
  // 键盘高度自己算：SDK 58 是 edge-to-edge，adjustResize 不再把窗口缩下来，
  // 不补这一段输入框就整条躺在键盘底下。
  const [keyboard, setKeyboard] = useState(0);

  useEffect(() => {
    const show = Keyboard.addListener('keyboardDidShow', (e) => setKeyboard(e.endCoordinates.height));
    const hide = Keyboard.addListener('keyboardDidHide', () => setKeyboard(0));
    return () => {
      show.remove();
      hide.remove();
    };
  }, []);

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
      if (addingDevice) {
        setAddingDevice(false);
        return true;
      }
      if (showNew) {
        setShowNew(false);
        return true;
      }
      if (showSettings) {
        setShowSettings(false);
        return true;
      }
      if (m.openId) {
        // 宽屏侧栏收起时，「返回」= 把列表拉回来；否则才是退回列表页。
        // （Android 上从左边划那一下就是系统的返回手势，所以这条路必须和手势走同一个判断。）
        if (wide && sideCollapsed) {
          setSideCollapsed(false);
          return true;
        }
        m.close();
        return true;
      }
      return false;
    });
    return () => sub.remove();
  }, [openFile, showInfo, showNew, showSettings, addingDevice, m, wide, sideCollapsed]);

  // 从后台回来先重读一遍：挂后台期间连接可能早断了，界面还停在旧状态
  useEffect(() => {
    const sub = AppState.addEventListener('change', (state) => {
      if (state === 'active') void m.reloadCurrent();
    });
    return () => sub.remove();
  }, [m]);

  // 列表页。`onForward` 只有窄屏传 —— 宽屏时它是常驻侧栏，推走没有意义
  const list = (onForward?: () => void) => (
    <SessionList
      sessions={m.sessions}
      currentId={m.openId}
      conn={m.conn}
      deviceName={m.devices[m.activeDevice]?.label}
      // 「连上了但列表没读回来」单独说 —— 这条横幅平时不出现
      error={m.conn === 'open' && m.sessions.length === 0 ? m.error : null}
      // 这些必须是稳定引用：行组件 memo 过，回调每帧新建的话 memo 等于没有
      onOpen={m.open}
      onSettings={() => setShowSettings(true)}
      onNew={() => setShowNew(true)}
      onRefresh={m.refresh}
      onDelete={m.deleteSession}
      onPin={m.setPinned}
      onRename={m.renameSession}
      onForward={onForward}
    />
  );

  // 新建会话：建好就进去。失败原因由顶部那条提示说（m.error）。
  const create = async (opts: { projectId?: string; workingDir?: string }) => {
    setCreating(true);
    try {
      const s = await m.createSession(opts);
      if (s) {
        setShowNew(false);
        void m.open(s.id);
      }
    } finally {
      setCreating(false);
    }
  };

  if (!m.ready) return <SafeAreaView style={[styles.root, { backgroundColor: t.bg }]} edges={['top', 'bottom']} />;

  return (
    <SafeAreaView style={[styles.root, { backgroundColor: t.bg }]} edges={['top']}>
      <View style={[styles.root, { paddingBottom: keyboard > 0 ? keyboard : insets.bottom }]}>
        <StatusBar style={isDark ? 'light' : 'dark'} />
        {/* 配对那一屏自己有错误位；把上一台的连接错误压在它上面只会吓人一跳 */}
        {m.pairing && m.error && !addingDevice && !showNew ? <NoticeBar text={m.error} /> : null}

        {!m.pairing || addingDevice ? (
          <Pairing
            onPaired={(p, meta) => {
              // 加完一台就回设置页：留在配对上会让人以为没成功
              setAddingDevice(false);
              void m.pair(p, meta);
            }}
            onClose={m.pairing ? () => setAddingDevice(false) : undefined}
          />
        ) : showNew ? (
          <NewSession
            projects={m.projects}
            busy={creating}
            onClose={() => setShowNew(false)}
            onCreate={(opts) => void create(opts)}
          />
        ) : showSettings ? (
          <Settings
            pairing={m.pairing}
            devices={m.devices}
            activeDevice={m.activeDevice}
            conn={m.conn}
            onSwitchDevice={(i) => {
              setShowSettings(false);
              void m.switchDevice(i);
            }}
            onRemoveDevice={(i) => void m.removeDevice(i)}
            onRenameDevice={(i, label) => void m.renameDevice(i, label)}
            onSelectAddress={(i, url) => void m.selectAddress(i, url)}
            onAddDevice={() => setAddingDevice(true)}
            onBack={() => setShowSettings(false)}
            awake={m.awake}
            onLoadAwake={m.loadAwake}
            onSetAwake={m.setAwake}
            onUnpair={() => {
              setShowSettings(false);
              void m.unpair();
            }}
          />
        ) : wide ? (
          <View style={styles.wide}>
            {sideCollapsed ? null : (
              <View style={[styles.wideSide, { borderRightColor: t.border, backgroundColor: t.sid }]}>{list()}</View>
            )}
            <View style={styles.wideMain}>
              <ChatPane
                m={m}
                showInfo={showInfo}
                setShowInfo={setShowInfo}
                openFile={openFile}
                setOpenFile={setOpenFile}
                onToggleList={() => setSideCollapsed((v) => !v)}
              />
            </View>
          </View>
        ) : m.openId ? (
          <ChatPane
            m={m}
            showInfo={showInfo}
            setShowInfo={setShowInfo}
            openFile={openFile}
            setOpenFile={setOpenFile}
          />
        ) : (
          // 整页往右推 = 进设置页（左滑已经归了行上的置顶 / 删除）
          list(() => setShowSettings(true))
        )}
      </View>
    </SafeAreaView>
  );
}

export default function App() {
  // 手势在 UI 线程解析：左滑那类「跟手」的动作不能交给 JS 线程（它随时在被事件流占着）。
  return (
    <GestureHandlerRootView style={{ flex: 1 }}>
      <SafeAreaProvider>
        <ThemeProvider>
          <Shell />
        </ThemeProvider>
      </SafeAreaProvider>
    </GestureHandlerRootView>
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
