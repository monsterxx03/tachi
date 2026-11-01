// 手机端开发用的假服务端：说真协议，喂假数据。
//
// 它存在的理由和 itest/mockllm 一样 —— 让手机端和协议这一半可以独立跑完整个流程
// （配对 → 列表 → 打开会话 → 发消息 → 收流式输出 → 回答卡片），不必先把桌面进程
// 改好。协议按 docs/2026-10-18-mobile-client-design.md 的 §4/§5 实现，字段名与
// mobile/src/api/types.ts 一一对应：那两边对不上就是 bug。
//
// 它**不是**产品的一部分，接上真的移动接口之后就该删掉。
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"time"
)

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
	Pinned     bool   `json:"pinned,omitempty"`
	// 这段对话属于哪个 IM channel（宿主说不出来时 channelName 为空）
	ChannelSession bool   `json:"channelSession,omitempty"`
	ChannelName    string `json:"channelName,omitempty"`
	// 有几个定时任务写进这段对话（0 / 缺省 = 没有）：列表上那颗小闹钟
	Cron int `json:"cron,omitempty"`
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
	NotifyOff   bool            `json:"notifyOff"`
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
	// 手机传上来的文件名：假服务端只留名字（不存字节），够验证「上传 → 发送」那条链
	Uploads []string
}

// append 把一条假消息放进会话，并给它盖上时刻：手机现在会把它显示在气泡上，没有时刻的
// 种子会让那一行永远空着 —— 空着的功能与没有这个功能在截图里长得一模一样。
//
// 时刻是「现在」，所以 mock 的这一屏永远像刚聊过；要演示「过去的日子显示日期」的那种，
// 种子自己给 Timestamp（见 seed 里那条旧会话）。
func (s *session) append(m Message) Message {
	if m.Timestamp == "" {
		m.Timestamp = time.Now().Format(time.RFC3339)
	}
	m.Index = s.nextIndex
	s.nextIndex++
	s.Messages = append(s.Messages, m)
	return m
}

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
	s3.Session = SessionSummary{ID: "s3", Title: "周报整理", ProjectName: "个人", UpdatedAt: "2 小时前",
		ChannelSession: true, ChannelName: "discord", Cron: 2}
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
	s5.Session = SessionSummary{ID: "s5", Title: "移动端接入方案", ProjectName: "tachi", UpdatedAt: "昨天", Pinned: true}
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

	// 带图片附件的一段：手机端那条「缩略图铺在气泡里 → 点开进阅读器」的路要有真像素可走
	s6 := &session{}
	s6.Session = SessionSummary{ID: "s6", Title: "索引估值提醒", ProjectName: "投资", UpdatedAt: "昨天", Cron: 1}
	s6.Model = "Delta V3"
	s6.Workspace = WorkspaceInfo{Primary: "~/repos/lixinger"}
	s6.Usage = UsageInfo{Cost: 0.02}
	s6.Context = ContextInfo{Estimate: 44000, Window: 128000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 6500}, {Label: "工具定义", Tokens: 8500},
		{Label: "对话", Tokens: 24000}, {Label: "工具结果", Tokens: 5000},
	}}
	s6.State = SessionState{Status: "idle", Label: "空闲"}
	s6.append(Message{Role: "user", Content: "今天收盘后看一下沪深300的估值分位。"})
	s6.append(Message{Role: "assistant", Content: "沪深300 当前 PE 分位 38%，处在中低区间；银行和红利板块的分位更低一些（21% / 26%）。要不要我把这条挂成每日收盘后的提醒？"})
	s6.append(Message{Role: "tool_call", ToolName: "SendFile", ToolCallID: "a-3", Title: "hs300-valuation.png"})
	s6.append(Message{Role: "tool_result", ToolName: "SendFile", ToolCallID: "a-3", ToolResult: "已交付 hs300-valuation.png"})
	// 围栏代码块在手机上曾经有两道底色：短行那块右边浅、长行那块右边也浅，缝跟着最长那行跑。
	// 这里放一段短行 yaml 和一段长行 sh，截图与回归都靠它看见那道缝。
	s6.append(Message{Role: "assistant", Content: "写成配置就是这样：\n\n" + "```yaml" + `
push:
  enabled: true
  keys: ["<这里填通道的 key>"]
` + "```" + `

地址不用写，默认就指向官方那个。配完先手动发一条确认：

` + "```sh" + `
curl "https://api.example.com/<key>/测试/来自-tachi"
` + "```"})
	s6.Attachments = []AttachmentRef{{ID: "a-3", Name: "hs300-valuation.png", MimeType: "image/png", Size: 186000, Downloadable: true}}
	add(s6)

	// 一段很长的历史：快照只带最近 100 条（和真服务端同一个上限），更早的靠 page 翻。
	// 手机上那条「往上滑加载更早」的路要有真东西可翻。
	s7 := &session{}
	s7.Session = SessionSummary{ID: "s7", Title: "长对话：接口梳理", ProjectName: "tachi", UpdatedAt: "前天"}
	s7.Model = "Beta 5.2"
	s7.Workspace = WorkspaceInfo{Primary: "~/repos/tachi"}
	s7.Usage = UsageInfo{Cost: 1.42, Credit: 1200, CacheHitRate: 0.72}
	s7.Context = ContextInfo{Estimate: 168000, Window: 400000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 9000}, {Label: "工具定义", Tokens: 14000},
		{Label: "对话", Tokens: 120000}, {Label: "工具结果", Tokens: 25000},
	}}
	s7.State = SessionState{Status: "idle", Label: "空闲"}
	for i := 0; i < 130; i++ {
		if i%4 == 0 {
			s7.append(Message{Role: "user", Content: fmt.Sprintf("第 %d 轮：把这个模块的接口列一下。", i/4+1)})
		} else {
			s7.append(Message{Role: "assistant", Content: fmt.Sprintf("已记录第 %d 条：接口与调用方都在这儿。", i)})
		}
	}
	add(s7)

	// 一轮里连着调用好几个工具：手机上那行「N 步」要有东西可折；其中一个失败的留在外面。
	s8 := &session{}
	s8.Session = SessionSummary{ID: "s8", Title: "接口巡检", ProjectName: "tachi", UpdatedAt: "3 天前"}
	s8.Model = "Gamma K2"
	s8.Workspace = WorkspaceInfo{Primary: "~/repos/tachi"}
	s8.Usage = UsageInfo{Cost: 0.09}
	s8.Context = ContextInfo{Estimate: 41000, Window: 200000, Parts: []ContextPart{
		{Label: "系统提示词", Tokens: 8000}, {Label: "工具定义", Tokens: 12000},
		{Label: "对话", Tokens: 18000}, {Label: "工具结果", Tokens: 3000},
	}}
	s8.State = SessionState{Status: "idle", Label: "空闲"}
	s8.append(Message{Role: "user", Content: "看一下 mobile 那边的接口有没有漏掉鉴权。"})
	s8.append(Message{Role: "assistant", Content: "我先翻一遍服务端的路由表和手机端的调用。"})
	s8.append(Message{Role: "tool_call", ToolName: "ReadFile", ToolCallID: "tc-8a", Title: "desktop/mobile.go"})
	s8.append(Message{Role: "tool_result", ToolName: "ReadFile", ToolCallID: "tc-8a", ToolResult: "… 13 条 mux.HandleFunc …"})
	// 真实记录里，两个 tool call 之间一定夹着 thinking 和**内容为空的 assistant**（模型每调一次
	// 就写一条）。种子照这个形状写，手机上「折叠」那条路才验得到真的东西。
	s8.append(Message{Role: "assistant", Content: "", Thinking: "先看看 Bash 的版本，再跑 vet。"})
	s8.append(Message{Role: "tool_call", ToolName: "Bash", ToolCallID: "tc-8b", Title: "go vet ./desktop"})
	s8.append(Message{Role: "tool_result", ToolName: "Bash", ToolCallID: "tc-8b", ToolResult: "exit status 1: build constraints exclude all Go files", IsError: true})
	s8.append(Message{Role: "assistant", Content: "", Thinking: "vet 挂了，换成搜 guard 的调用。"})
	s8.append(Message{Role: "tool_call", ToolName: "Grep", ToolCallID: "tc-8c", Title: "guard(...)"})
	s8.append(Message{Role: "tool_result", ToolName: "Grep", ToolCallID: "tc-8c", ToolResult: "13 处都在 m.guard(...) 里"})
	s8.append(Message{Role: "tool_call", ToolName: "ReadFile", ToolCallID: "tc-8d", Title: "mobile/src/api/client.ts"})
	s8.append(Message{Role: "tool_result", ToolName: "ReadFile", ToolCallID: "tc-8d", ToolResult: "… Authorization: Bearer …"})
	s8.append(Message{Role: "assistant", Content: "13 个端点都过了 guard，除了 ping（它本来就不该要令牌）。有个 vet 失败是目录不对，不是代码问题。"})
	add(s8)

	return order, byID
}

// snapshotLimit 与真服务端的 mobileSnapshotLimit 对齐：快照只够画一屏，更早的要翻页。
const snapshotLimit = 100

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

// chartPNG 现画一张图当图片附件 —— 手机端要有一条真的图片路径可走
// （缩略图铺在气泡里、点开进阅读器、令牌走请求头）。
func chartPNG() []byte {
	const w, h = 640, 400
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{0xfd, 0xfb, 0xf7, 0xff}
	grid := color.RGBA{0x3e, 0x36, 0x2c, 0x22}
	accent := color.RGBA{0x4a, 0x6c, 0xf5, 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	for y := 60; y < h-40; y += 60 {
		for x := 40; x < w-40; x++ {
			img.Set(x, y, grid)
		}
	}

	// 一条往上走的折线
	series := []int{320, 300, 312, 268, 250, 256, 218, 200, 212, 178, 150, 162, 128}
	step := (w - 80) / (len(series) - 1)
	for i := 0; i < len(series)-1; i++ {
		line(img, 40+i*step, series[i], 40+(i+1)*step, series[i+1], accent)
	}
	for i, y := range series {
		x := 40 + i*step
		for dy := -3; dy <= 3; dy++ {
			for dx := -3; dx <= 3; dx++ {
				img.Set(x+dx, y+dy, accent)
			}
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func line(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	dx, dy := x1-x0, y1-y0
	steps := max(abs(dx), abs(dy))
	if steps == 0 {
		steps = 1
	}
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		for t := -1; t <= 1; t++ {
			img.Set(x, y+t, c)
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
