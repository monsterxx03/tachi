import { StatusBar } from 'expo-status-bar';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  AppState,
  BackHandler,
  Keyboard,
  Linking,
  Pressable,
  StyleSheet,
  Text,
  useWindowDimensions,
  View,
} from 'react-native';
import { GestureHandlerRootView } from 'react-native-gesture-handler';
import { SafeAreaProvider, SafeAreaView, useSafeAreaInsets } from 'react-native-safe-area-context';
import type { AttachmentRef } from './src/api/types';
import { api } from './src/api/client';
import { deviceKey } from './src/api/storage';
import type { MobileApi } from './src/state/mobile';
import { useMobile } from './src/state/mobile';
import { ThemeProvider, useTheme } from './src/theme/ThemeContext';
import { Chat } from './src/ui/Chat';
import { FileViewer } from './src/ui/FileViewer';
import { Pairing } from './src/ui/Pairing';
import { SessionInfo } from './src/ui/SessionInfo';
import { SessionList } from './src/ui/SessionList';
import { NewSession } from './src/ui/NewSession';
import { Devices } from './src/ui/Devices';
import { machineColor } from './src/ui/machineColor';
import { DeviceDetail } from './src/ui/DeviceDetail';
import { Opening } from './src/ui/Opening';

/**
 * 顶上那条红字（连不上、令牌失效、服务端的拒绝原因）。
 *
 * 它以前没有出路：挂上去之后只有「下一次同类操作成功」才会让位，于是一次失败的发送能在
 * 顶上留很久，久到读者会把它当成现在的状态读。现在两条路——点它（或那颗 ×）立即收起；
 * 没人点的时候 8 秒后自己退场。报错值得读两遍，不值得一直占着一行。
 */
const NOTICE_MS = 8000;

function NoticeBar({ text, onDismiss }: { text: string; onDismiss: () => void }) {
  const { tokens: t } = useTheme();
  // 回调每帧都是新的（store 每次渲染都返回一个新箭头），把它放进 ref 里，计时只跟着 text
  // 重来 —— 否则那条 8 秒的计时会被任何一次无关的重渲染推后，永远不到点。
  const dismissRef = useRef(onDismiss);
  dismissRef.current = onDismiss;
  useEffect(() => {
    const timer = setTimeout(() => dismissRef.current(), NOTICE_MS);
    return () => clearTimeout(timer);
  }, [text]);
  return (
    <Pressable
      onPress={onDismiss}
      accessibilityRole="button"
      accessibilityLabel="收起提示"
      style={[styles.notice, { backgroundColor: t.redWash, borderBottomColor: t.border }]}
    >
      <View style={[styles.noticeDot, { backgroundColor: t.red }]} />
      <Text style={{ color: t.red, fontSize: 12, flex: 1 }}>{text}</Text>
      <Text style={{ color: t.red, fontSize: 15, paddingHorizontal: 4 }}>×</Text>
    </Pressable>
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
  const { tokens: t, isDark } = useTheme();
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
        onToggleNotify={(off) => void m.setNotifyOff(off)}
        loadPanels={m.loadPanels}
        cronVersion={m.cronVersion}
        onCronAction={m.cronAction}
      />
    );

  if (inChat && m.chat.snapshot)
    return (
      <Chat
        sessions={m.sessions}
        machine={m.devices[m.activeDevice]?.label}
        machineColor={
          m.devices[m.activeDevice]
            ? machineColor(deviceKey(m.devices[m.activeDevice]), isDark)
            : undefined
        }
        loadAgents={async () => {
          const { panels } = await m.loadPanels();
          return panels?.agents.peers ?? [];
        }}
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
    // 先找聚合那份：打开的是别的机器上的会话时，当前这台的镜像里还没有它
    const row =
      m.overview.find((r) => r.session.id === m.openId)?.session ??
      m.sessions.find((s) => s.id === m.openId);
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
  // 正在看哪一台的详情（null = 电脑清单）。和 showSettings 一起表示「在电脑这块」。
  const [deviceDetail, setDeviceDetail] = useState<number | null>(null);
  // 首页「只看某一台」的筛选（键是 deviceKey）。不放列表内部：进出对话一趟回来还保持着，
  // 比每次回来都被重置更符合预期；重开 App 才回到「全部」。
  const [filterKey, setFilterKey] = useState<string | null>(null);
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
        if (deviceDetail != null) setDeviceDetail(null);
        else setShowSettings(false);
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
  }, [openFile, showInfo, showNew, showSettings, deviceDetail, addingDevice, m, wide, sideCollapsed]);

  // 从后台回来先重读一遍：挂后台期间连接可能早断了，界面还停在旧状态。
  // 进后台则退订当前会话 —— 那是宿主判断「人还在不在看」的依据，也是推送该不该响的依据：
  // 揣进口袋的手机若还订阅着，回合结束时就不会响，而那正是最该响的时刻。
  useEffect(() => {
    const sub = AppState.addEventListener('change', (state) => {
      if (state === 'active') void m.reloadCurrent();
      else if (state === 'background') m.background();
    });
    return () => sub.remove();
  }, [m]);

  // 点开一条推送 = 系统用 tachi://… 打开这个 App。冷启动从 getInitialURL 拿，热启动从
  // 事件拿；两条路都交给 handleLink（它会先切到那台电脑，再打开那段对话）。
  //
  // 冷启动那一条要等 ready：设备簿是从钥匙串异步读出来的，早一帧拿到链接会看到一本空簿子，
  // 于是明明配对过的电脑被报成「还没在这部手机上配对」。
  useEffect(() => {
    if (!m.ready) return;
    let cancelled = false;
    void Linking.getInitialURL().then((url) => {
      if (url && !cancelled) m.handleLink(url);
    });
    const sub = Linking.addEventListener('url', (e) => m.handleLink(e.url));
    return () => {
      cancelled = true;
      sub.remove();
    };
  }, [m.ready, m.handleLink]);

  // 详情页那一台的「保持唤醒」：读的是那一台，不是正在连的那一台。引用要稳定 ——
  // 一页里那个 effect 靠它决定「什么时候再读一次」，每帧新建一个新箭头就会自己喂自己。
  const loadAwakeFor = useCallback(() => {
    void m.loadAwake(deviceDetail ?? undefined);
  }, [m.loadAwake, deviceDetail]);

  // 列表页。`onForward` 只有窄屏传 —— 宽屏时它是常驻侧栏，推走没有意义
  const list = (onForward?: () => void) => (
    <SessionList
      rows={m.overview}
      devices={m.devices}
      reachable={m.reachable}
      currentId={m.openId}
      conn={m.conn}
      filterKey={filterKey}
      onFilter={setFilterKey}
      // 「连上了但列表没读回来」单独说 —— 这条横幅平时不出现
      error={m.error}
      // 这些必须是稳定引用：行组件 memo 过，回调每帧新建的话 memo 等于没有
      onOpen={m.openOn}
      onSettings={() => setShowSettings(true)}
      onNew={() => setShowNew(true)}
      onRefresh={m.refresh}
      onDelete={m.deleteSessionOn}
      onPin={m.setPinnedOn}
      onRename={m.renameSessionOn}
      onForward={onForward}
    />
  );

  // 新建会话：先选机器、再建，建好就进去。失败原因由顶部那条提示说（m.error）。
  const create = async (deviceIndex: number, opts: { projectId?: string; workingDir?: string }) => {
    setCreating(true);
    try {
      const s = await m.createSessionOn(deviceIndex, opts);
      if (s) setShowNew(false);
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
        {m.pairing && m.error && !addingDevice && !showNew ? (
          <NoticeBar text={m.error} onDismiss={m.dismissError} />
        ) : null}

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
            devices={m.devices}
            reachable={m.reachable}
            loadProjects={m.projectsFor}
            busy={creating}
            onClose={() => setShowNew(false)}
            onCreate={(i, opts) => void create(i, opts)}
          />
        ) : showSettings ? (
          deviceDetail != null && m.devices[deviceDetail] ? (
            <DeviceDetail
              device={m.devices[deviceDetail]}
              online={m.reachable[deviceKey(m.devices[deviceDetail])] !== false}
              conn={deviceDetail === m.activeDevice ? m.conn : 'idle'}
              syncedAt={m.syncedAt(deviceKey(m.devices[deviceDetail]))}
              onBack={() => setDeviceDetail(null)}
              onRemove={() => {
                const at = deviceDetail;
                setDeviceDetail(null);
                void m.removeDevice(at);
              }}
              onRename={(label) => void m.renameDevice(deviceDetail, label)}
              onSelectAddress={(url) => void m.selectAddress(deviceDetail, url)}
              awake={m.awakeOf(deviceKey(m.devices[deviceDetail]))}
              onLoadAwake={loadAwakeFor}
              onSetAwake={(on, min) => m.setAwake(deviceDetail, on, min)}
            />
          ) : (
            <Devices
              devices={m.devices}
              reachable={m.reachable}
              syncedAt={m.syncedAt}
              onAddDevice={() => setAddingDevice(true)}
              onOpen={(i) => setDeviceDetail(i)}
              onBack={() => setShowSettings(false)}
            />
          )
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
