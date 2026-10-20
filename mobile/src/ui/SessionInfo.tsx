import { Feather } from '@expo/vector-icons';
import { useCallback, useEffect, useRef, useState } from 'react';
import { ActivityIndicator, Alert, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';
import type { AgentPanel, CronPanelRow, MCPPanelRow, Panels, Snapshot } from '../api/types';
import type { Live } from '../state/chat';
import { GestureDetector } from 'react-native-gesture-handler';
import Animated from 'react-native-reanimated';
import { useTheme } from '../theme/ThemeContext';
import { useEdgeBack } from './edgeBack';

type Tab = 'overview' | 'mcp' | 'agents' | 'cron';

const TABS: { key: Tab; label: string }[] = [
  { key: 'overview', label: '概览' },
  { key: 'mcp', label: 'MCP' },
  { key: 'agents', label: 'Agent' },
  { key: 'cron', label: '定时' },
];

/** RFC3339 → 本地「MM-DD HH:mm」；空串照旧空着（没有下次运行就是没有） */
function shortTime(ts: string): string {
  if (!ts) return '';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** 分项条的四档：同一个色系由深到浅，别用语义色（绿/琥珀说的是「好/警告」，这里说的是「占比」） */
function partColors(t: ReturnType<typeof useTheme>['tokens']): string[] {
  return [t.accent, t.accent2, t.accentLine, t.accentSoft];
}

function CtxBar({
  parts,
  total,
  t,
}: {
  parts: { label: string; tokens: number }[];
  total: number;
  t: ReturnType<typeof useTheme>['tokens'];
}) {
  const colors = partColors(t);
  // 没有分项（这段对话在本进程里还没跑过一轮）时只画总量：编几个 0 的桶只会让人以为
  // 「工具结果占 0」是测出来的。
  const rows = parts.length > 0 ? parts : [{ label: '上下文', tokens: total }];
  return (
    <>
      <View style={[styles.ctxBar, { backgroundColor: t.wash }]}>
        <View style={styles.ctxFill}>
          {rows.map((p, i) => (
            <View
              key={p.label}
              style={{ height: 8, width: `${total > 0 ? (p.tokens / total) * 100 : 0}%`, backgroundColor: colors[i % colors.length] }}
            />
          ))}
        </View>
      </View>
      {parts.length > 0 ? (
        <View style={styles.ctxRows}>
          {parts.map((p, i) => (
            <View key={p.label} style={styles.ctxRow}>
              <View style={{ width: 7, height: 7, borderRadius: 2, backgroundColor: colors[i % colors.length] }} />
              <Text style={[styles.ctxLabel, { color: t.faint }]}>{p.label}</Text>
              <Text style={[styles.ctxNum, { color: t.dim }]}>{p.tokens.toLocaleString()}</Text>
            </View>
          ))}
        </View>
      ) : (
        <Text style={[styles.hint, { color: t.faint }]}>
          只有总量：分项要在电脑上跑过一轮才有。
        </Text>
      )}
    </>
  );
}

function Switch({ on, disabled, onPress, t }: { on: boolean; disabled?: boolean; onPress: () => void; t: ReturnType<typeof useTheme>['tokens'] }) {
  return (
    <Pressable
      disabled={disabled}
      onPress={onPress}
      style={[
        styles.switch,
        {
          backgroundColor: on ? t.amberWash : t.washStrong,
          borderColor: on ? t.amberLine : t.borderStrong,
          opacity: disabled ? 0.4 : 1,
        },
      ]}
    >
      <View style={[styles.knob, { left: on ? 20 : 2, backgroundColor: on ? t.amber : t.switchOff }]} />
    </Pressable>
  );
}

export function SessionInfo({
  snapshot,
  live,
  providers,
  onClose,
  onPickModel,
  onToggleSkip,
  loadPanels,
  cronVersion,
  onCronAction,
}: {
  snapshot: Snapshot;
  live: Live;
  providers: string[];
  onClose: () => void;
  onPickModel: (name: string) => void;
  onToggleSkip: (skip: boolean) => void;
  /** 三个只读面板的数据源（打开面板才拉） */
  loadPanels: () => Promise<{ panels: Panels | null; error: string | null }>;
  /** 定时任务表的版本号：另一块屏改了任务就 +1 */
  cronVersion: number;
  /** 定时任务的动作（跑一次 / 暂停 / 恢复 / 删除）；成功返回 null */
  onCronAction: (id: string, action: 'run' | 'pause' | 'resume' | 'delete') => Promise<string | null>;
}) {
  const { tokens: t } = useTheme();
  // 左缘右划 = 返回对话（和这个页面上那个 chevron 同一个动作）
  const back = useEdgeBack(onClose);
  const s = snapshot.session;
  const ratio = snapshot.context.window > 0 ? snapshot.context.estimate / snapshot.context.window : 0;
  const readOnly = s.readOnly;
  const tps = live.tps || live.lastTps;
  const usage = snapshot.usage;
  const [tab, setTab] = useState<Tab>('overview');
  const [panels, setPanels] = useState<Panels | null>(null);
  const [panelError, setPanelError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  // 这一屏问过一次就不再自动问：失败也停下。之前是「没拿到就再试」，读不到的时候
  // 变成一圈没有出口的重试 —— 屏幕上就是疯狂闪烁，电脑那边被轮番请求。
  const [asked, setAsked] = useState(false);
  // 定时任务的动作：进行中的那一条 + 上一次的结果（成功也留一句，否则按了没反应）
  const [busyJob, setBusyJob] = useState<string | null>(null);
  const [cronNotice, setCronNotice] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const r = await loadPanels();
      // 失败时不清掉已经拿到的：看到旧数据 + 一句原因，比一瞬间变空好
      if (r.panels) setPanels(r.panels);
      setPanelError(r.error);
    } finally {
      setLoading(false);
    }
  }, [loadPanels]);

  useEffect(() => {
    if (tab === 'overview' || asked || loading) return;
    setAsked(true);
    void refresh();
  }, [tab, asked, loading, refresh]);

  // 另一块屏改了定时任务（窗口、另一台手机、或者任务刚跑完）：面板已经打开就重读一次。
  // 版本号只在真的收到事件时变，所以这里不会变成轮询。
  const seenCron = useRef(cronVersion);
  useEffect(() => {
    if (cronVersion === seenCron.current) return;
    seenCron.current = cronVersion;
    if (tab === 'cron' || panels) void refresh();
  }, [cronVersion, tab, panels, refresh]);

  const cronAct = async (id: string, action: 'run' | 'pause' | 'resume' | 'delete') => {
    setBusyJob(id);
    setCronNotice(null);
    const err = await onCronAction(id, action);
    setBusyJob(null);
    setCronNotice(err ?? (action === 'run' ? '已开始运行' : action === 'delete' ? '已删除' : action === 'pause' ? '已暂停' : '已恢复'));
    if (!err) await refresh();
  };

  // 换了一段对话（同一个组件被复用）时把上一段的面板清掉
  const sessionId = snapshot.session.id;
  useEffect(() => {
    setPanels(null);
    setPanelError(null);
    setAsked(false);
  }, [sessionId]);

  return (
    <GestureDetector gesture={back.gesture}>
      <Animated.View style={[styles.root, { backgroundColor: t.bg }, back.style]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onClose} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <Text style={[styles.title, { color: t.title }]}>会话信息</Text>
      </View>

      <View style={[styles.tabs, { borderBottomColor: t.border }]}>
        {TABS.map((x) => {
          const on = tab === x.key;
          return (
            <Pressable
              key={x.key}
              onPress={() => setTab(x.key)}
              style={[
                styles.tab,
                { borderColor: on ? t.accentLine : 'transparent', backgroundColor: on ? t.accentSoft : 'transparent' },
              ]}
            >
              <Text style={{ color: on ? t.accent2 : t.faint, fontSize: 12.5 }}>{x.label}</Text>
            </Pressable>
          );
        })}
      </View>

      {tab !== 'overview' ? (
        <ScrollView contentContainerStyle={styles.body}>
          {loading && !panels ? (
            <View style={styles.panelLoading}>
              <ActivityIndicator size="small" color={t.faint} />
              <Text style={{ color: t.faint, fontSize: 12 }}>正在读面板…</Text>
            </View>
          ) : null}
          {panelError ? (
            <View style={[styles.errBox, { borderColor: t.redLine, backgroundColor: t.redWash }]}>
              <Text style={{ color: t.red, fontSize: 12.5, lineHeight: 18 }}>{panelError}</Text>
            </View>
          ) : null}
          {panels ? (
            <>
              {tab === 'mcp' ? <MCPList rows={panels.mcp} t={t} /> : null}
              {tab === 'agents' ? <AgentList panel={panels.agents} t={t} /> : null}
              {tab === 'cron' ? (
                <CronList
                  rows={panels.cron}
                  t={t}
                  busyId={busyJob}
                  notice={cronNotice}
                  onRun={(id) => void cronAct(id, 'run')}
                  onPause={(id, paused) => void cronAct(id, paused ? 'pause' : 'resume')}
                  onDelete={(row) =>
                    Alert.alert('删除这个定时任务？', row.name, [
                      { text: '取消', style: 'cancel' },
                      { text: '删除', style: 'destructive', onPress: () => void cronAct(row.id, 'delete') },
                    ])
                  }
                />
              ) : null}
            </>
          ) : null}
          {asked ? (
            <Pressable
              onPress={() => void refresh()}
              disabled={loading}
              style={({ pressed }) => [
                styles.refreshBtn,
                { borderColor: t.border, backgroundColor: pressed ? t.wash : 'transparent', opacity: loading ? 0.6 : 1 },
              ]}
            >
              <Feather name="refresh-cw" size={13} color={t.faint} />
              <Text style={{ color: t.faint, fontSize: 12 }}>{loading ? '读取中…' : '刷新'}</Text>
            </Pressable>
          ) : null}
          <Text style={[styles.hint, { color: t.faint, textAlign: 'center' }]}>
            {tab === 'cron'
              ? '这里可以跑一次、暂停、删除；新建任务和改计划在电脑上做。'
              : '面板只读：连 server 和授权都在电脑上做。'}
          </Text>
        </ScrollView>
      ) : (
      <ScrollView contentContainerStyle={styles.body}>
        {/* 上下文 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={styles.cardHead}>
            <Text style={[styles.cardTitle, { color: t.text }]}>上下文</Text>
            <Text style={[styles.cardNum, { color: t.dim }]}>
              {snapshot.context.estimate.toLocaleString()} / {snapshot.context.window.toLocaleString()}
              <Text style={{ color: t.faint }}> · {Math.round(ratio * 100)}%</Text>
            </Text>
          </View>
          <CtxBar parts={snapshot.context.parts} total={snapshot.context.estimate} t={t} />
        </View>

        {/* 工作区 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>工作区</Text>
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>主目录</Text>
            <Text style={[styles.v, styles.mono, { color: t.dim }]}>{snapshot.workspace.primary || '—'}</Text>
          </View>
          {snapshot.workspace.additional.map((d) => (
            <View key={d} style={styles.kv}>
              <Text style={[styles.k, { color: t.faint }]}>附加</Text>
              <Text style={[styles.v, styles.mono, { color: t.dim }]}>{d}</Text>
            </View>
          ))}
          <Text style={[styles.hint, { color: t.faint }]}>改目录是电脑上的事，手机只读。</Text>
        </View>

        {/* 模型 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>模型</Text>
          {providers.map((name) => {
            const on = name === snapshot.model;
            return (
              <Pressable
                key={name}
                disabled={readOnly}
                onPress={() => onPickModel(name)}
                style={[
                  styles.modelRow,
                  {
                    borderColor: on ? t.accentLine : t.border,
                    backgroundColor: on ? t.accentSoft : 'transparent',
                    opacity: readOnly ? 0.55 : 1,
                  },
                ]}
              >
                <Text style={{ color: t.text, fontSize: 13.5, flex: 1 }}>{name}</Text>
                {on ? <Feather name="check" size={14} color={t.accent2} /> : null}
              </Pressable>
            );
          })}
          <Text style={[styles.hint, { color: t.faint }]}>只影响这段对话，不改电脑上的默认。</Text>
        </View>

        {/* 审批 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <View style={styles.switchRow}>
            <View style={{ flex: 1 }}>
              <Text style={[styles.cardTitle, { color: t.text }]}>跳过审批</Text>
              <Text style={[styles.hint, { color: t.faint }]}>
                命中 ask 规则的命令会直接执行，不再停下来问。只影响这段对话。
              </Text>
            </View>
            <Switch on={snapshot.skipApproval} disabled={readOnly} onPress={() => onToggleSkip(!snapshot.skipApproval)} t={t} />
          </View>
          {readOnly ? (
            <Text style={[styles.hint, { color: t.faint, marginTop: 8 }]}>这段对话由别的任务占用，改不了。</Text>
          ) : null}
        </View>

        {/* 用量 */}
        <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
          <Text style={[styles.cardTitle, { color: t.text }]}>用量</Text>
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>这段对话</Text>
            <Text style={[styles.v, { color: t.dim }]}>¥{usage.cost.toFixed(3)}</Text>
          </View>
          {usage.credit ? (
            <View style={styles.kv}>
              <Text style={[styles.k, { color: t.faint }]}>积分</Text>
              <Text style={[styles.v, { color: t.dim }]}>{usage.credit.toFixed(2)}</Text>
            </View>
          ) : null}
          {usage.cacheHitRate != null ? (
            <View style={styles.kv}>
              <Text style={[styles.k, { color: t.faint }]}>缓存命中</Text>
              <Text style={[styles.v, { color: t.dim }]}>{Math.round(usage.cacheHitRate * 100)}%</Text>
            </View>
          ) : null}
          <View style={styles.kv}>
            <Text style={[styles.k, { color: t.faint }]}>速度</Text>
            <Text style={[styles.v, { color: t.dim }]}>{tps ? `${Math.round(tps)} tok/s` : '—'}</Text>
          </View>
          <Text style={[styles.hint, { color: t.faint }]}>
            金额与积分来自电脑上的用量账本（人民币）；速度是这一轮实测的输出速度。
          </Text>
        </View>
      </ScrollView>
      )}
      </Animated.View>
    </GestureDetector>
  );
}

/** MCP：连没连上、这段对话能用几个工具 —— 工具的参数 schema 手机上给不了，也不给。 */
function MCPList({ rows, t }: { rows: MCPPanelRow[]; t: ReturnType<typeof useTheme>['tokens'] }) {
  if (rows.length === 0)
    return <Text style={[styles.empty, { color: t.faint }]}>这台电脑上没配 MCP server。</Text>;
  return (
    <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
      <Text style={[styles.cardTitle, { color: t.text }]}>MCP</Text>
      {rows.map((r) => {
        const state = r.connected ? '已连接' : r.needsAuth ? '等待授权（在电脑上）' : '未连接';
        const tone = r.connected ? t.green : r.needsAuth ? t.amber : t.faint;
        return (
          <View key={r.name} style={styles.panelRow}>
            <View style={{ flex: 1, minWidth: 0 }}>
              <Text style={{ color: t.text, fontSize: 13.5 }} numberOfLines={1}>
                {r.name}
              </Text>
              <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }}>
                {r.type ? `${r.type} · ` : ''}
                {r.connected ? `这段对话载入 ${r.loaded}/${r.tools} 个工具` : '没有工具可用'}
              </Text>
            </View>
            <Text style={{ color: tone, fontSize: 11.5 }}>{state}</Text>
          </View>
        );
      })}
    </View>
  );
}

function AgentList({ panel, t }: { panel?: AgentPanel; t: ReturnType<typeof useTheme>['tokens'] }) {
  if (!panel) return <Text style={[styles.empty, { color: t.faint }]}>还没有读到节点列表。</Text>;
  if (panel.peers.length === 0)
    return (
      <Text style={[styles.empty, { color: t.faint }]}>
        {panel.anyChecked ? '这台电脑不会连到别的 agent。' : '还没问过任何节点 —— 在电脑上打开一次 Agent 面板。'}
      </Text>
    );
  return (
    <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
      <Text style={[styles.cardTitle, { color: t.text }]}>Agent</Text>
      {panel.peers.map((p, i) => (
        <View key={`${p.name}-${i}`} style={styles.panelRow}>
          <View style={{ flex: 1, minWidth: 0 }}>
            <Text style={{ color: t.text, fontSize: 13.5 }} numberOfLines={1}>
              {p.name || p.url}
            </Text>
            <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }} numberOfLines={1}>
              {[p.reach, p.note, p.url].filter(Boolean).join(' · ')}
            </Text>
            {p.skills.length > 0 ? (
              <Text style={{ color: t.dim, fontSize: 11.5, marginTop: 4 }} numberOfLines={2}>
                {p.skills.join('、')}
              </Text>
            ) : null}
            {p.err ? (
              <Text style={{ color: t.red, fontSize: 11.5, marginTop: 3 }} numberOfLines={1}>
                {p.err}
              </Text>
            ) : null}
          </View>
        </View>
      ))}
      <Text style={[styles.hint, { color: t.faint }]}>
        {panel.checkedAt ? `最近一次探测：${panel.checkedAt}` : '还没有探测结果'}
      </Text>
    </View>
  );
}

function CronList({
  rows,
  t,
  busyId,
  notice,
  onRun,
  onPause,
  onDelete,
}: {
  rows: CronPanelRow[];
  t: ReturnType<typeof useTheme>['tokens'];
  busyId: string | null;
  notice: string | null;
  onRun: (id: string) => void;
  onPause: (id: string, paused: boolean) => void;
  onDelete: (row: CronPanelRow) => void;
}) {
  if (rows.length === 0)
    return (
      <>
        <Text style={[styles.empty, { color: t.faint }]}>没有定时任务。</Text>
        {notice ? <Text style={[styles.empty, { color: t.faint }]}>{notice}</Text> : null}
      </>
    );
  return (
    <View style={[styles.card, { borderColor: t.border, backgroundColor: t.panel }]}>
      <Text style={[styles.cardTitle, { color: t.text }]}>定时任务</Text>
      {notice ? (
        <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 8 }}>{notice}</Text>
      ) : null}
      {rows.map((j) => (
        <View key={j.id} style={styles.cronItem}>
          <View style={styles.panelRow}>
            <View style={{ flex: 1, minWidth: 0 }}>
              <Text style={{ color: t.text, fontSize: 13.5 }} numberOfLines={1}>
                {j.name}
              </Text>
              <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }} numberOfLines={1}>
                {[j.scheduleText, j.targetTitle || '（会话不在了）'].filter(Boolean).join(' · ')}
              </Text>
              <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }} numberOfLines={1}>
                {j.running ? '正在跑' : j.nextRun ? `下次 ${shortTime(j.nextRun)}` : '不会自动跑'}
                {j.lastRunStatus ? ` · 上次 ${j.lastRunStatus}` : ''}
              </Text>
              {j.lastRunError ? (
                <Text style={{ color: t.red, fontSize: 11.5, marginTop: 3 }} numberOfLines={2}>
                  {j.lastRunError}
                </Text>
              ) : null}
            </View>
            <Text style={{ color: j.status === 'paused' ? t.faint : t.accent2, fontSize: 11.5 }}>
              {j.status === 'paused' ? '已暂停' : '启用'}
            </Text>
          </View>
          <View style={styles.cronActions}>
            <CronBtn label={j.running ? '运行中…' : '运行一次'} disabled={j.running || busyId === j.id} onPress={() => onRun(j.id)} t={t} />
            <CronBtn
              label={j.status === 'paused' ? '恢复' : '暂停'}
              disabled={busyId === j.id}
              onPress={() => onPause(j.id, j.status !== 'paused')}
              t={t}
            />
            <CronBtn label="删除" tone="red" disabled={busyId === j.id} onPress={() => onDelete(j)} t={t} />
          </View>
        </View>
      ))}
    </View>
  );
}

function CronBtn({
  label,
  onPress,
  disabled,
  tone,
  t,
}: {
  label: string;
  onPress: () => void;
  disabled?: boolean;
  tone?: 'red';
  t: ReturnType<typeof useTheme>['tokens'];
}) {
  const color = tone === 'red' ? t.red : t.dim;
  return (
    <Pressable
      disabled={disabled}
      onPress={onPress}
      style={({ pressed }) => [
        styles.cronBtn,
        { borderColor: tone === 'red' ? t.redLine : t.border, backgroundColor: pressed ? t.wash : 'transparent', opacity: disabled ? 0.5 : 1 },
      ]}
    >
      <Text style={{ color, fontSize: 11.5 }}>{label}</Text>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 6,
    height: 58,
    paddingHorizontal: 8,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  back: { width: 34, height: 34, borderRadius: 9, alignItems: 'center', justifyContent: 'center' },
  title: { fontSize: 15, fontWeight: '600' },
  body: { padding: 16, gap: 12 },
  card: { borderWidth: 1, borderRadius: 12, padding: 13 },
  cardHead: { flexDirection: 'row', alignItems: 'baseline', justifyContent: 'space-between' },
  cardTitle: { fontSize: 13.5, fontWeight: '600' },
  cardNum: { fontSize: 12, fontVariant: ['tabular-nums'] },
  ctxBar: { height: 8, borderRadius: 5, overflow: 'hidden', marginTop: 11, marginBottom: 13 },
  ctxFill: { flexDirection: 'row', height: '100%' },
  ctxRows: { gap: 7 },
  ctxRow: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  ctxLabel: { fontSize: 11.5, flex: 1 },
  ctxNum: { fontSize: 11.5, fontVariant: ['tabular-nums'] },
  kv: { flexDirection: 'row', alignItems: 'baseline', gap: 12, paddingTop: 10 },
  k: { fontSize: 12, width: 52 },
  v: { fontSize: 12.5, flex: 1, lineHeight: 18 },
  mono: { fontFamily: 'monospace', fontSize: 11.5 },
  hint: { fontSize: 11.5, lineHeight: 17, marginTop: 8 },
  tabs: { flexDirection: 'row', gap: 6, paddingHorizontal: 12, paddingVertical: 8, borderBottomWidth: StyleSheet.hairlineWidth },
  tab: { borderWidth: 1, borderRadius: 9, paddingHorizontal: 11, paddingVertical: 6 },
  panelRow: { flexDirection: 'row', alignItems: 'flex-start', gap: 10, paddingTop: 11 },
  panelLoading: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 12 },
  empty: { fontSize: 12.5, lineHeight: 19, paddingVertical: 10 },
  errBox: { borderWidth: 1, borderRadius: 10, padding: 11 },
  cronItem: { paddingTop: 11 },
  cronActions: { flexDirection: 'row', gap: 8, paddingTop: 9 },
  cronBtn: { borderWidth: 1, borderRadius: 8, paddingHorizontal: 10, paddingVertical: 6 },
  refreshBtn: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 7,
    borderWidth: 1,
    borderRadius: 10,
    paddingVertical: 10,
  },
  modelRow: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
    borderWidth: 1,
    borderRadius: 10,
    paddingHorizontal: 12,
    paddingVertical: 11,
    marginTop: 8,
  },
  switchRow: { flexDirection: 'row', alignItems: 'flex-start', gap: 12 },
  switch: { width: 44, height: 26, borderRadius: 20, borderWidth: 1, justifyContent: 'center' },
  knob: { position: 'absolute', width: 20, height: 20, borderRadius: 10 },
});
