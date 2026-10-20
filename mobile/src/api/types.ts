/**
 * 手机与服务端之间那层数据的样子 —— 线上的唯一一份定义。
 *
 * 服务端在桌面进程里（Go），这里改一个字段名，那边的投影就得跟着改；
 * 两边对不上的表现是"某个字段永远空着"，不会报错，所以字段名以这份为准。
 */

/** 一条等待（授权或提问）的两种；与桌面侧栏、菜单栏用的是同一批文案。 */
export type WaitingText = '' | '等待你的授权' | '等待你的回答';

/** 会话列表里的一行（一段对话一行：压缩链已经折到叶子）。 */
export interface SessionSummary {
  id: string;
  title: string;
  /** 项目名，仅作行内标签；V1 不按项目分组 */
  projectName?: string;
  /** RFC3339 */
  updatedAt: string;
  running: boolean;
  waiting: WaitingText;
  /** 子系统持有的只读会话（Kind != ""）；只是显示，真正的拒绝在 agent 入口 */
  readOnly: boolean;
  /** 被某台机器的任务占着 */
  heldByTask: boolean;
  pinned?: boolean;
}

/** 新建会话时可挑的项目。primary 是它会带来的工作目录，runnable=false 表示目录已经不在了。 */
export interface ProjectRef {
  id: string;
  name: string;
  primary: string;
  runnable: boolean;
}

export type MessageRole = 'user' | 'assistant' | 'tool_call' | 'tool_result' | 'reminder';

/** 历史里的一条记录。index 是它在会话记录文件里的绝对位置，翻页游标就是它。 */
export interface SessionMessage {
  index: number;
  role: MessageRole;
  content: string;
  /** 用户自己的原文（内容被 @ 文件展开改过时才有），气泡显示这个 */
  displayContent?: string;
  timestamp?: string;
  /** 1-based，属于哪一次模型调用；steer 消息靠它和本轮自己的提问区分 */
  iteration?: number;
  seq?: number;
  turn?: number;
  toolName?: string;
  toolCallId?: string;
  /** 工具参数（JSON 字符串） */
  args?: string;
  /** 给人看的一行摘要 */
  title?: string;
  toolResult?: string;
  isError?: boolean;
  /** 折叠起来的思考增量 */
  thinking?: string;
}

/** 权限卡片的外壳：决定标题写「命令 / 文件 / 搜索费用」 */
export type PermissionKind = 'bash' | 'files' | 'mcp_search';

export interface PermissionAsk {
  kind: 'permission';
  toolId: string;
  toolName: string;
  permissionKind: PermissionKind;
  /** agent 自己的原文（命令、路径、费用），手机不重新拼 */
  preview: string;
}

export interface QuestionAsk {
  kind: 'question';
  toolId: string;
  questions: AgentQuestion[];
}

export interface AgentQuestion {
  question: string;
  options?: string[];
  multiSelect?: boolean;
}

export type PendingAsk = PermissionAsk | QuestionAsk;

export interface AttachmentRef {
  /** 就是那次 SendFile 调用的 id */
  id: string;
  name: string;
  mimeType: string;
  size: number;
  /** 超过上限的文件不给下载，只列信息 */
  downloadable: boolean;
}

export interface WorkspaceInfo {
  primary: string;
  additional: string[];
}

export interface UsageInfo {
  cost: number;
  credit?: number;
  cacheHitRate?: number;
}

export interface ContextInfo {
  estimate: number;
  window: number;
  parts: { label: string; tokens: number }[];
}

/** 三个只读面板：这段对话能用的 MCP、可以委派过去的 agent、定时任务。 */
export interface Panels {
  mcp: MCPPanelRow[];
  agents: AgentPanel;
  cron: CronPanelRow[];
}

export interface MCPPanelRow {
  name: string;
  type: string;
  connected: boolean;
  /** 这台电脑上这个 server 要先去授权 */
  needsAuth: boolean;
  /** 这段对话已经载入的工具数（工具搜索下可能一个都没载入） */
  loaded: number;
  /** 这个 server 一共提供多少工具 */
  tools: number;
}

export interface AgentPanel {
  peers: AgentPanelRow[];
  checkedAt: string;
  /** false = 还没问过任何节点，界面要说出来，而不是画一个空的「没有节点」 */
  anyChecked: boolean;
}

export interface AgentPanelRow {
  name: string;
  label: string;
  url: string;
  note: string;
  /** 最近一次探测的结论（同一批文案给桌面那份面板用） */
  reach: string;
  skills: string[];
  err: string;
}

export interface CronPanelRow {
  id: string;
  name: string;
  scheduleText: string;
  status: string;
  /** session / pending / missing */
  targetKind: string;
  targetTitle: string;
  nextRun: string;
  lastRunStatus: string;
  lastRunError: string;
  running: boolean;
}

/** 打开一段对话时先给的那一份完整快照；之后才接增量。 */
export interface Snapshot {
  session: SessionSummary;
  messages: SessionMessage[];
  /** 还有更早的可以翻 */
  hasMore: boolean;
  state: SessionState;
  asks: PendingAsk[];
  model: string;
  skipApproval: boolean;
  workspace: WorkspaceInfo;
  usage: UsageInfo;
  context: ContextInfo;
  attachments: AttachmentRef[];
}

export interface SessionState {
  status: string;
  label: string;
  detail: string;
  waiting: WaitingText;
}

/** 一轮怎么结束的 */
export type TurnEnd = 'complete' | 'interrupted' | 'error';

/**
 * 服务端推来的增量。**只走投影**：没有整段历史，工具结果已经截断。
 * 记录是权威的 —— 这几条事件到达时都要重读一次快照（见 ReReadTrigger）。
 */
export type ServerEvent =
  | { type: 'snapshot'; snapshot: Snapshot }
  | { type: 'text'; text: string }
  | { type: 'thinking'; text: string }
  | { type: 'tool_start'; toolId: string; toolName: string; title: string }
  | { type: 'tool_args'; toolId: string; args: string }
  | { type: 'tool_result'; toolId: string; isError: boolean; result: string; truncated: boolean }
  | { type: 'turn'; durationMs: number; iterations: number; end: TurnEnd }
  | { type: 'error'; message: string; interrupted: boolean }
  | { type: 'idle'; end: TurnEnd }
  | { type: 'compact_start' }
  | { type: 'compact_done'; error?: string }
  | { type: 'state'; state: SessionState }
  | { type: 'tps'; tps: number; lastTps: number }
  | { type: 'usage'; usage: UsageInfo }
  | { type: 'cost'; cost: number; credit?: number; cacheHitRate?: number }
  | { type: 'ask'; ask: PendingAsk }
  | { type: 'ask_answered'; toolId: string }
  | { type: 'user_message'; text: string; displayText?: string; steer: boolean; origin: MessageOrigin }
  /** 这一段对话的模型 / 审批开关被某一块屏改了；两种开关一起发 */
  | { type: 'session_prefs'; provider: string; skipApproval: boolean }
  /** 会话列表变了（桌面或手机新建 / 删除）；广播给每一条连接 */
  | { type: 'sessions_changed' }
  /** 定时任务表变了（新建 / 改 / 暂停 / 删除 / 跑完），`id` 是变的那一条；广播给每一条连接 */
  | { type: 'cron_changed'; id: string }
  /** 某段会话的「在跑 / 在等」变了；广播给每一条连接（列表那一行要跟着亮） */
  | { type: 'session_state'; sessionId: string; running: boolean; waiting: WaitingText }
  | { type: 'lagged' };

/** 这条消息是从哪块屏来的；用来决定自己要不要再画一次 */
export type MessageOrigin = 'desktop' | 'mobile' | 'badge' | 'cron';

/** 手机发出去的命令 */
export type SendMode = 'send' | 'steer' | 'interrupt';

export type PermissionDecision = 'allow_once' | 'deny';
