package main

// 远端的三个只读面板 + 定时任务的那三个动作。
//
// 数据全部来自手机接口的 `/v1/sessions/{id}/panels`：MCP 的连接状态、可委派的节点、定时任务。
// 它是**按会话**答的，因为 MCP 那一半回答的是「这段对话能用哪些工具」；定时任务那一半还可能
// 带上这段对话所属 IM 渠道自己的任务（只读，跑它需要那个渠道的运行时）。
//
// 为什么面板不直接复用桌面的那几个组件：远端给的是**计数**（MCP 每台几把工具、载入几把），
// 不是工具清单，也不是节点的完整卡片 —— 硬塞进本地面板会得到一排空的可展开区和一个刷不新
// 的「刷新」按钮。所以 MCP / Agents 在这里各有一份只读的小渲染，定时任务复用同一个 `CronPanel`
// （它的数据是完整的，而且跑一次 / 暂停 / 删除这三个动作协议里本来就有）。

import (
	"context"
	"net/http"
	"net/url"
)

type RemoteMCPRowVO struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Connected bool   `json:"connected"`
	NeedsAuth bool   `json:"needsAuth"`
	// Loaded is how many of this server's Tools this conversation has loaded; Tools is how many
	// it offers. The pair is the number the native panel draws as `n/m 工具`.
	Loaded int `json:"loaded"`
	Tools  int `json:"tools"`
}

type RemoteAgentRowVO struct {
	Name   string   `json:"name"`
	Label  string   `json:"label"`
	ID     string   `json:"id"`
	URL    string   `json:"url"`
	Note   string   `json:"note"`
	Reach  string   `json:"reach"`
	Err    string   `json:"err"`
	Skills []string `json:"skills"`
}

type RemoteAgentsVO struct {
	Peers      []RemoteAgentRowVO `json:"peers"`
	CheckedAt  string             `json:"checkedAt"`
	AnyChecked bool               `json:"anyChecked"`
}

type RemoteCronRowVO struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ScheduleText  string `json:"scheduleText"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	TargetKind    string `json:"targetKind"`
	TargetTitle   string `json:"targetTitle"`
	NextRun       string `json:"nextRun"`
	LastRunStatus string `json:"lastRunStatus"`
	LastRunError  string `json:"lastRunError"`
	Running       bool   `json:"running"`
}

type RemotePanelsVO struct {
	MCP    []RemoteMCPRowVO  `json:"mcp"`
	Agents RemoteAgentsVO    `json:"agents"`
	Cron   []RemoteCronRowVO `json:"cron"`
}

// RemotePanels reads the three panels for one conversation on the other machine. A read: it
// builds nothing there and forces no probe — the agents half is whatever that machine's last
// look found, which is exactly why it carries `checkedAt`.
func (s *AgentService) RemotePanels(hostID, sessionID string) (RemotePanelsVO, string) {
	var empty RemotePanelsVO
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return empty, why
	}
	// 协议把这三种面板包在一个字段里：{"panels":{"mcp":…,"agents":…,"cron":…}}。少了这层信封
	// 解码不会报错，只会得到三个空数组 —— 「明明有任务，面板却是空的」就是它。
	var env struct {
		Panels *RemotePanelsVO `json:"panels"`
	}
	if err := e.call(context.Background(), http.MethodGet, sessionPath(sessionID, "/panels"), nil, &env); err != nil {
		return empty, remoteReason(err)
	}
	if env.Panels == nil {
		return empty, "面板数据读不出来（对面回的格式不对）"
	}
	out := *env.Panels
	if out.MCP == nil {
		out.MCP = []RemoteMCPRowVO{}
	}
	if out.Agents.Peers == nil {
		out.Agents.Peers = []RemoteAgentRowVO{}
	}
	if out.Cron == nil {
		out.Cron = []RemoteCronRowVO{}
	}
	return out, ""
}

// RemoteRunCronJob runs one scheduled job now. The reply is only "accepted": the run happens
// over there, and what it does arrives as a normal turn in the conversation it belongs to.
func (s *AgentService) RemoteRunCronJob(hostID, jobID string) string {
	return s.remoteCronCall(hostID, http.MethodPost, "/v1/cron/"+url.PathEscape(jobID)+"/run", nil)
}

func (s *AgentService) RemotePauseCronJob(hostID, jobID string, paused bool) string {
	return s.remoteCronCall(hostID, http.MethodPost, "/v1/cron/"+url.PathEscape(jobID)+"/pause", map[string]any{"paused": paused})
}

func (s *AgentService) RemoteDeleteCronJob(hostID, jobID string) string {
	return s.remoteCronCall(hostID, http.MethodDelete, "/v1/cron/"+url.PathEscape(jobID), nil)
}

func (s *AgentService) remoteCronCall(hostID, method, path string, body any) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	if err := e.call(context.Background(), method, path, body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}
