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
  /** 这段对话属于某个 IM channel（discord / weixin / 插件自己的名字）——答复发回那边 */
  channelSession?: boolean;
  /** 是哪个 channel；宿主说不出来时为空（重启之后、那段对话还没再说过话） */
  channelName?: string;
  /** 有几个定时任务写进这段对话（缺省 = 没有）。列表靠它画那个小闹钟 */
  cron?: number;
  /**
   * 这些任务下一次叫醒这段对话是什么时候（RFC3339，缺省 = 没有排上：一个任务都没有，或者全暂停了）。
   * 取的是最近的那一次 —— 列表回答的是「它什么时候会自己动」，不是「最后一次是什么时候」。
   */
  cronNext?: string;
}

/**
 * 这台电脑的「保持唤醒」开关。它不属于任何一段会话 —— 一整台机器只有一个值。
 *
 * on 是开关本身；durationMin 是偏好（下次开启用多少分钟，0 = 不自动关）；until 是这次开着时
 * 到点自动关的时刻（RFC3339），空串表示没开或不自动关。
 */
export interface AwakeState {
  on: boolean;
  durationMin: number;
  until: string;
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
  /**
   * 这次调用是**用户自己点名**的（桌面的 /sh）——输出就是要看的东西，卡片直接摊开。
   * 只有实时那一轮带它：记录里没有这个字段（它是一次显示提示，不是对话内容）。
   */
  autoExpand?: boolean;
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
  /**
   * 节点的身份（pin 住的那一个）。`#a:` 引用只认它 —— 而它是空的时候这一行**不能**被当成
   * 可引用的：一个空的 `#a:` 看着像引用，插进去却解析不了。
   */
  id: string;
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
  /** recurring = 反复跑；oneshot = 只跑一次，跑完自己删掉。空值按 oneshot 读（老数据） */
  type: string;
  status: string;
  /** session / pending / missing / channel —— channel 是本段对话所属渠道的定时任务 */
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
  /** 这一段对话关掉手机推送了吗（会话自己的开关；全局开关在那台电脑的设置里） */
  notifyOff: boolean;
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
  | { type: 'tool_start'; toolId: string; toolName: string; title: string; autoExpand?: boolean }
  | { type: 'tool_args'; toolId: string; args: string }
  | { type: 'tool_result'; toolId: string; isError: boolean; result: string; truncated: boolean }
  | { type: 'turn'; durationMs: number; iterations: number; end: TurnEnd }
  | { type: 'error'; message: string; interrupted: boolean }
  | { type: 'idle'; end: TurnEnd }
  | { type: 'compact_start' }
  | { type: 'compact_done'; error?: string }
  /**
   * 对话搬到了另一个会话 id（压缩成功写进子会话）。订阅必须跟着走。
   * end 是这一轮为什么结束：换轨之后 idle 发往新 id，手机多半收不到，队列要靠它决定。
   */
  | { type: 'session_moved'; from: string; to: string; end: TurnEnd }
  | { type: 'state'; state: SessionState }
  | { type: 'tps'; tps: number; lastTps: number }
  | { type: 'usage'; usage: UsageInfo }
  | { type: 'cost'; cost: number; credit?: number; cacheHitRate?: number }
  | { type: 'ask'; ask: PendingAsk }
  | { type: 'ask_answered'; toolId: string }
  | { type: 'user_message'; text: string; displayText?: string; steer: boolean; origin: MessageOrigin }
  /** 这一段对话的模型 / 审批 / 推送开关被某一块屏改了；三个一起发 */
  | { type: 'session_prefs'; provider: string; skipApproval: boolean; notifyOff: boolean }
  /** 会话列表变了（桌面或手机新建 / 删除）；广播给每一条连接 */
  | { type: 'sessions_changed' }
  /** 定时任务表变了（新建 / 改 / 暂停 / 删除 / 跑完），`id` 是变的那一条；广播给每一条连接 */
  | { type: 'cron_changed'; id: string }
  /**
   * 这台电脑的「保持唤醒」变了：托盘勾的、另一块屏改的、或者到点自己关的。
   * 带着完整的新状态，广播给每一条连接 —— 收到就照它画，不用再读什么。
   */
  | { type: 'awake_changed'; awake: AwakeState }
  /** 某段会话的「在跑 / 在等」变了；广播给每一条连接（列表那一行要跟着亮） */
  | { type: 'session_state'; sessionId: string; running: boolean; waiting: WaitingText }
  | { type: 'lagged' };

/** 这条消息是从哪块屏来的；用来决定自己要不要再画一次 */
export type MessageOrigin = 'desktop' | 'mobile' | 'badge' | 'cron';

/** 手机发出去的命令 */
export type SendMode = 'send' | 'steer' | 'interrupt';

/**
 * 手机传上去、宿主存进这段会话目录里的一个文件（`POST /v1/sessions/{id}/files` 的答案）。
 *
 * `name` 是宿主**存下来的**名字（重名会加 -1、-2），发送时手机递的就是它 —— 协议里从不出现
 * 路径：令牌不能等于「读那台机器上的任意文件」。`kind` 是 @ 展开会把它当成什么
 * （image / text / binary），手机只拿它挑个图标。
 */
export interface UploadedFile {
  name: string;
  size: number;
  kind: string;
}

/** 待发送队列里的一条：一段话 + 可选的文件（排队时它们是一起发生的事）。 */
export interface PendingMessage {
  text: string;
  files: UploadedFile[];
}

export type PermissionDecision = 'allow_once' | 'deny';
