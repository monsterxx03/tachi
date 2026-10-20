package main

// 线上类型的 Go 一侧。**字段名以 `mobile/src/api/types.ts` 为准**（那份是协议的唯一出处，
// 见 docs/agents/mobile-protocol.md）：这里改一个 json tag，手机那边就少一个字段，
// 而两边都不会报错 —— 表现只是"某个东西永远空着"。
//
// 两条跟着类型走的规矩：
//   - 数组字段空的时候是 `[]`，不是 `null`。nil 切片序列化成 null，手机把它当数组用。
//     所以每个产出这些结构的地方都显式 make(...)，而不是留 var x []T。
//   - 「等待」那两句文本只有两处来源：asks.go 的 waitingPermission / waitingQuestion。
//     手机不自己拼，这里也不抄第二份。

// mobileSessionSummary 是会话列表的一行 —— 一段对话一行（压缩链已折到叶子）。
type mobileSessionSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ProjectName string `json:"projectName,omitempty"`
	UpdatedAt   string `json:"updatedAt"`
	Running     bool   `json:"running"`
	Waiting     string `json:"waiting"`
	ReadOnly    bool   `json:"readOnly"`
	HeldByTask  bool   `json:"heldByTask"`
	Pinned      bool   `json:"pinned,omitempty"`
}

// mobileMessage 是历史里的一条记录。Index 是它在会话记录里的绝对位置 —— 翻页游标就是它
// （时间戳会撞，两条同一时刻的消息落在页边界上会整段跳过）。
type mobileMessage struct {
	Index          int    `json:"index"`
	Role           string `json:"role"` // user/assistant/tool_call/tool_result/reminder
	Content        string `json:"content"`
	DisplayContent string `json:"displayContent,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"`
	Iteration      int    `json:"iteration,omitempty"`
	Seq            int    `json:"seq,omitempty"`
	Turn           int    `json:"turn,omitempty"`
	ToolName       string `json:"toolName,omitempty"`
	ToolCallID     string `json:"toolCallId,omitempty"`
	Args           string `json:"args,omitempty"`
	Title          string `json:"title,omitempty"`
	ToolResult     string `json:"toolResult,omitempty"`
	IsError        bool   `json:"isError,omitempty"`
	Thinking       string `json:"thinking,omitempty"`
}

// mobileAsk 是一条等待。权限与提问是两种外壳，靠 kind 分：
//   - permission: ToolID/ToolName/PermissionKind/Preview
//   - question:   ToolID/Questions
//
// Preview 是 agent 的原文（命令 + 命中的规则），手机原样显示、不重新拼。
type mobileAsk struct {
	Kind           string           `json:"kind"` // permission / question
	ToolID         string           `json:"toolId"`
	ToolName       string           `json:"toolName,omitempty"`
	PermissionKind string           `json:"permissionKind,omitempty"` // bash / files / mcp_search
	Preview        string           `json:"preview,omitempty"`
	Questions      []mobileQuestion `json:"questions,omitempty"`
}

// mobileQuestion mirrors tools.Question for the wire. It is a copy rather than a reuse so the
// json tags are this layer's own: tools.Question is the agent's type, and a rename there must
// not silently reshape the phone's protocol.
type mobileQuestion struct {
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

// mobileAttachment 是这段对话里 SendFile 交付过的一个文件。ID 就是那次工具调用的 id，
// 所以对话重开、进程重启，链接都不变。
type mobileAttachment struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         int64  `json:"size"`
	Downloadable bool   `json:"downloadable"`
}

type mobileWorkspace struct {
	Primary    string   `json:"primary"`
	Additional []string `json:"additional"`
}

type mobileUsage struct {
	Cost         float64 `json:"cost"`
	Credit       float64 `json:"credit,omitempty"`
	CacheHitRate float64 `json:"cacheHitRate,omitempty"`
}

type mobileContextPart struct {
	Label  string `json:"label"`
	Tokens int64  `json:"tokens"`
}

type mobileContext struct {
	Estimate int64               `json:"estimate"`
	Window   int64               `json:"window"`
	Parts    []mobileContextPart `json:"parts"`
}

// mobileState 是一个会话的状态。与 AgentState 同样的三个字段加那句等待 —— 手机只渲染。
type mobileState struct {
	Status  string `json:"status"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Waiting string `json:"waiting"`
}

// mobileSnapshot 是打开一段对话时先给的那一份完整快照，之后才是增量。
type mobileSnapshot struct {
	Session      mobileSessionSummary `json:"session"`
	Messages     []mobileMessage      `json:"messages"`
	HasMore      bool                 `json:"hasMore"`
	State        mobileState          `json:"state"`
	Asks         []mobileAsk          `json:"asks"`
	Model        string               `json:"model"`
	SkipApproval bool                 `json:"skipApproval"`
	Workspace    mobileWorkspace      `json:"workspace"`
	Usage        mobileUsage          `json:"usage"`
	Context      mobileContext        `json:"context"`
	Attachments  []mobileAttachment   `json:"attachments"`
}

// 服务端 → 手机的事件名。它们是协议的一部分，两边照抄。
const (
	mobileEvSnapshot     = "snapshot"
	mobileEvText         = "text"
	mobileEvThinking     = "thinking"
	mobileEvToolStart    = "tool_start"
	mobileEvToolArgs     = "tool_args"
	mobileEvToolResult   = "tool_result"
	mobileEvTurn         = "turn"
	mobileEvError        = "error"
	mobileEvIdle         = "idle"
	mobileEvCompactStart = "compact_start"
	mobileEvCompactDone  = "compact_done"
	mobileEvState        = "state"
	mobileEvTps          = "tps"
	mobileEvUsage        = "usage"
	mobileEvCost         = "cost"
	mobileEvAsk          = "ask"
	mobileEvAskAnswered  = "ask_answered"
	mobileEvUserMessage  = "user_message"
	mobileEvSessionPrefs = "session_prefs"
	// mobileEvSessionMoved 这段对话搬到了另一个会话 id（压缩成功把它写进子会话）。
	// 订阅是跟着 id 走的：不换轨的手机就停在旧那一半上，看着一个再也不会动的画面。
	mobileEvSessionMoved = "session_moved"
	// mobileEvSessionState 一条会话的「在跑 / 在等」变了。和 state 不同，它是**广播**：
	// 停在列表页的手机没订阅任何会话，却正是需要更新那一行的人。
	mobileEvSessionState = "session_state"
	// mobileEvSessionsChanged 会话列表变了（新建 / 删除），广播给每一条连接 —— 列表不是
	// 某个会话的流，凡是连着这台电脑的手机都该重读一次。
	mobileEvSessionsChanged = "sessions_changed"
	// mobileEvCronChanged 定时任务表变了（新建 / 改 / 暂停 / 删除 / 跑完一次）。和会话列表
	// 一样是**广播**：这张表不属于任何一段对话，谁改的都要让另一块屏重读。
	mobileEvCronChanged = "cron_changed"
	mobileEvLagged      = "lagged"
)

// 待答项的 kind：**线上的词**，不是内部那张表的 kind。两者只差一个（`ask_user` 在线上叫
// `question`），而差的那一个正好让手机的卡片永远不认 —— 内部的名字一旦漏到线上，客户端的
// 类型是对的、代码是对的，只是那条分支永远不成立。
const (
	mobileAskKindPermission = "permission"
	mobileAskKindQuestion   = "question"
)

// 手机 → 服务端：订阅一个会话，以及心跳。
const (
	mobileCmdOpen = "open"
	mobileCmdPing = "ping"
)

// mobileToolResultLimit bounds a forwarded tool result. A ReadFile can carry tens of KB and the
// phone folds it anyway; the full output stays a desktop concern (protocol 不变量 3).
const mobileToolResultLimit = 2 << 10

// messageOrigin says which screen produced a user message. The echo rule is symmetric: a screen
// ignores the echo of its own message (it already drew the bubble) and appends everyone else's.
// Without it the window would show only the assistant's reply to a message sent from the phone.
type messageOrigin string

const (
	originDesktop messageOrigin = "desktop"
	originMobile  messageOrigin = "mobile"
	originBadge   messageOrigin = "badge"
	originCron    messageOrigin = "cron"
)
