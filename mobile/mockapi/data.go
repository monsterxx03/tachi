// 手机端开发用的假服务端：说真协议，喂假数据。
//
// 它存在的理由和 itest/mockllm 一样 —— 让手机端和协议这一半可以独立跑完整个流程
// （配对 → 列表 → 打开会话 → 发消息 → 收流式输出 → 回答卡片），不必先把桌面进程
// 改好。协议按 docs/2026-10-18-mobile-client-design.md 的 §4/§5 实现，字段名与
// mobile/src/api/types.ts 一一对应：那两边对不上就是 bug。
//
// 它**不是**产品的一部分，接上真的移动接口之后就该删掉。
package main

type SessionSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ProjectName string `json:"projectName,omitempty"`
	UpdatedAt   string `json:"updatedAt"`
	Running     bool   `json:"running"`
	// 只有两句（与桌面同一批常量）或空
	Waiting    string `json:"waiting"`
	ReadOnly   bool   `json:"readOnly"`
	HeldByTask bool   `json:"heldByTask"`
}

type Message struct {
	Index          int    `json:"index"`
	Role           string `json:"role"` // user / assistant / tool_call / tool_result / reminder
	Content        string `json:"content"`
	DisplayContent string `json:"displayContent,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"`
	Iteration      int    `json:"iteration,omitempty"`
	ToolName       string `json:"toolName,omitempty"`
	ToolCallID     string `json:"toolCallId,omitempty"`
	Title          string `json:"title,omitempty"`
	ToolResult     string `json:"toolResult,omitempty"`
	IsError        bool   `json:"isError,omitempty"`
	Thinking       string `json:"thinking,omitempty"`
}

type PermissionAsk struct {
	Kind           string `json:"kind"` // permission
	ToolID         string `json:"toolId"`
	ToolName       string `json:"toolName"`
	PermissionKind string `json:"permissionKind"` // bash / files / mcp_search
	Preview        string `json:"preview"`
}

type Question struct {
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

type QuestionAsk struct {
	Kind      string     `json:"kind"` // question
	ToolID    string     `json:"toolId"`
	Questions []Question `json:"questions"`
}

type AttachmentRef struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         int64  `json:"size"`
	Downloadable bool   `json:"downloadable"`
}

type SessionState struct {
	Status  string `json:"status"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Waiting string `json:"waiting"`
}

type WorkspaceInfo struct {
	Primary    string   `json:"primary"`
	Additional []string `json:"additional"`
}

type UsageInfo struct {
	Cost         float64 `json:"cost"`
	Credit       float64 `json:"credit,omitempty"`
	CacheHitRate float64 `json:"cacheHitRate,omitempty"`
}

type ContextPart struct {
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
}

type ContextInfo struct {
	Estimate int           `json:"estimate"`
	Window   int           `json:"window"`
	Parts    []ContextPart `json:"parts"`
}

// Ask 是两种待答之一，用 Kind 区分（线上就是这个形状）
type Ask struct {
	Permission *PermissionAsk `json:"permission,omitempty"`
	Question   *QuestionAsk   `json:"question,omitempty"`
}

type Snapshot struct {
	Session     SessionSummary  `json:"session"`
	Messages    []Message       `json:"messages"`
	HasMore     bool            `json:"hasMore"`
	State       SessionState    `json:"state"`
	Asks        []any           `json:"asks"`
	Model       string          `json:"model"`
	SkipApprove bool            `json:"skipApproval"`
	Workspace   WorkspaceInfo   `json:"workspace"`
	Usage       UsageInfo       `json:"usage"`
	Context     ContextInfo     `json:"context"`
	Attachments []AttachmentRef `json:"attachments"`
}

// session 是一个假会话的全部状态
type session struct {
	Snapshot
	// 记录里的下一个 index（翻页与新增都用它）
	nextIndex int
}

func (s *session) append(m Message) Message {
	m.Index = s.nextIndex
	s.nextIndex++
	s.Snapshot.Messages = append(s.Snapshot.Messages, m)
	return m
}

func newSession(m *session) *session { return m }

// seed 造出几个能覆盖各种状态的会话：在跑、等你授权、等你回答、只读、空闲。
func seed() (order []string, byID map[string]*session) {
	byID = map[string]*session{}
	order = []string{}

	add := func(s *session) {
		byID[s.Session.ID] = s
		order = append(order, s.Session.ID)
	}

	s1 := &session{}
	s1.Session = SessionSummary{
		ID: "s1", Title: "清理构建目录", ProjectName: "tachi", UpdatedAt: "刚刚",
		Running: true, Waiting: "等待你的授权",
	}
	s1.Model = "Alpha 4.5"
	s1.Workspace = WorkspaceInfo{Primary: "~/repos/tachi"}
	s1.Usage = UsageInfo{Cost: 0.12}
	s1.Context = ContextInfo{Estimate: 52400, Window: 200000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 8200}, {Label: "工具定义", Tokens: 12100},
		{Label: "对话", Tokens: 21400}, {Label: "工具结果", Tokens: 10700},
	}}
	s1.State = SessionState{Status: "working", Label: "在跑", Detail: "正在执行命令", Waiting: "等待你的授权"}
	s1.append(Message{Role: "user", Content: "把 build 目录里的旧产物清掉，只保留最近两次构建。"})
	s1.append(Message{Role: "assistant", Content: "我先看一下目录大小和最近一次构建时间。"})
	s1.append(Message{Role: "tool_call", ToolName: "Bash", ToolCallID: "tc-0", Title: "du -sh build && ls -lt build | head -24"})
	s1.append(Message{Role: "tool_result", ToolName: "Bash", ToolCallID: "tc-0", ToolResult: "328M\tbuild\n… 6 个 7 天前的产物\n… 2 个今天的产物"})
	s1.append(Message{Role: "assistant", Content: "`build/` 一共 328M，其中 6 个是 7 天前的产物。清理命令命中了 `permissions.bash.ask` 规则，需要你确认。"})
	s1.Asks = []any{PermissionAsk{
		Kind: "permission", ToolID: "tc-1", ToolName: "Bash", PermissionKind: "bash",
		Preview: "$ rm -rf build/electron-20261009-* build/web-20261010-*\n匹配规则 permissions.bash.ask ·「rm -rf*」",
	}}
	add(s1)

	s2 := &session{}
	s2.Session = SessionSummary{
		ID: "s2", Title: "登录流程重构", ProjectName: "tachi", UpdatedAt: "3 分钟前",
		Running: true, Waiting: "等待你的回答",
	}
	s2.Model = "Beta 5.2"
	s2.Workspace = WorkspaceInfo{Primary: "~/repos/tachi"}
	s2.Usage = UsageInfo{Cost: 0.31}
	s2.Context = ContextInfo{Estimate: 118000, Window: 400000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 9000}, {Label: "工具定义", Tokens: 14000},
		{Label: "对话", Tokens: 76000}, {Label: "工具结果", Tokens: 19000},
	}}
	s2.State = SessionState{Status: "waiting", Label: "等你回答", Detail: "有一个问题待回答", Waiting: "等待你的回答"}
	s2.append(Message{Role: "user", Content: "把登录失败的处理统一一下，别每个页面各写一套。"})
	s2.append(Message{Role: "assistant", Content: "好，我先看现有的三个登录入口。"})
	s2.append(Message{
		Role: "assistant", Content: "三个入口里有两套处理，收敛到一个地方最省事，但先确认一下你想要的形态。",
		Thinking: "LoginPage 用全局 toast，SignupPage 在表单里写红字，OAuthCallback 什么都不显示。",
	})
	s2.Asks = []any{QuestionAsk{
		Kind: "question", ToolID: "q-1",
		Questions: []Question{{Question: "失败提示放在哪一层？", Options: []string{"页面各自展示", "统一的 toast 组件", "全局错误边界"}}},
	}}
	add(s2)

	s3 := &session{}
	s3.Session = SessionSummary{ID: "s3", Title: "周报整理", ProjectName: "个人", UpdatedAt: "2 小时前"}
	s3.Model = "Alpha 4.5"
	s3.SkipApprove = true
	s3.Workspace = WorkspaceInfo{Primary: "~/notes"}
	s3.Usage = UsageInfo{Cost: 0.04}
	s3.Context = ContextInfo{Estimate: 38000, Window: 200000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 8000}, {Label: "工具定义", Tokens: 12000},
		{Label: "对话", Tokens: 15000}, {Label: "工具结果", Tokens: 3000},
	}}
	s3.State = SessionState{Status: "idle", Label: "空闲"}
	s3.append(Message{Role: "user", Content: "把本周的提交整理成周报，按模块分组。"})
	s3.append(Message{Role: "assistant", Content: "本周有 23 个提交，集中在移动端设计、记忆调度和 MCP 面板三块。周报写在 `~/notes/weekly/2026-10-18.md` 了。"})
	s3.append(Message{Role: "tool_call", ToolName: "SendFile", ToolCallID: "a-1", Title: "weekly/2026-10-18.md"})
	s3.append(Message{Role: "tool_result", ToolName: "SendFile", ToolCallID: "a-1", ToolResult: "已交付 2026-10-18.md"})
	s3.Attachments = []AttachmentRef{{ID: "a-1", Name: "2026-10-18.md", MimeType: "text/markdown", Size: 1843, Downloadable: true}}
	add(s3)

	s4 := &session{}
	s4.Session = SessionSummary{
		ID: "s4", Title: "竞品页面抓取", ProjectName: "节点任务", UpdatedAt: "昨天",
		ReadOnly: true, HeldByTask: true,
	}
	s4.Model = "Delta V3"
	s4.Workspace = WorkspaceInfo{Primary: "~/workspace/a2a/ab12f0"}
	s4.Usage = UsageInfo{Cost: 0.18}
	s4.Context = ContextInfo{Estimate: 96000, Window: 128000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 7000}, {Label: "工具定义", Tokens: 9000},
		{Label: "对话", Tokens: 60000}, {Label: "工具结果", Tokens: 20000},
	}}
	s4.State = SessionState{Status: "idle", Label: "空闲"}
	s4.append(Message{Role: "user", Content: "抓取这四个产品的定价页，整理成一张表。"})
	s4.append(Message{Role: "assistant", Content: "4 个页面都抓到了。价格表在附件的 `pricing.md` 里，两家的企业版价格需要登录后才看得到，我在表里标了「未公开」。"})
	s4.Attachments = []AttachmentRef{{ID: "a-2", Name: "pricing.md", MimeType: "text/markdown", Size: 4300, Downloadable: true}}
	add(s4)

	s5 := &session{}
	s5.Session = SessionSummary{ID: "s5", Title: "移动端接入方案", ProjectName: "tachi", UpdatedAt: "昨天"}
	s5.Model = "Alpha 4.5"
	s5.Workspace = WorkspaceInfo{Primary: "~/repos/tachi"}
	s5.Usage = UsageInfo{Cost: 0.07}
	s5.Context = ContextInfo{Estimate: 61000, Window: 200000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 8000}, {Label: "工具定义", Tokens: 12000},
		{Label: "对话", Tokens: 32000}, {Label: "工具结果", Tokens: 9000},
	}}
	s5.State = SessionState{Status: "idle", Label: "空闲"}
	s5.append(Message{Role: "user", Content: "移动端怎么接进来？画一下消息从手机到会话的路径。"})
	s5.append(Message{Role: "assistant", Content: mermaidReply})
	add(s5)

	return order, byID
}

const mermaidReply = `不像 channel 那样各聊各的：手机连的是开着的那台电脑，会话还是桌面上的那段。

` + "```mermaid" + `
flowchart TD
  P["手机 App"] -->|"局域网"| D["桌面进程"]
  D --> S["会话"]
  D --> Q["待答表"]
  Q --> A["审批 / 提问"]
` + "```" + `

所以手机不会多出一份记忆：它只是同一段对话的另一块屏。`

// 附件正文（下载端点用）
var fileBodies = map[string]string{
	"a-1": `# 周报 · 2026-10-18

本周 23 个提交 · 3 个模块

## 移动端

- 定下形态：手机是桌面的另一块屏，不跑 agent、不拥有会话。
- 原型：手机单栏 / 平板两栏；白天与黑夜两套配色。

## 下周

搭移动端的服务端骨架：开关、令牌、会话列表，先用 curl 跑通。
`,
	"a-2": `# 竞品定价

抓取时间 2026-10-17 22:10

| 产品 | 个人版 | 团队版 |
| --- | --- | --- |
| Alpha | $20 / 月 | $35 / 人 / 月 |
| Beta | $18 / 月 | $30 / 人 / 月 |
| Gamma | 免费 | $12 / 人 / 月 |
`,
}
