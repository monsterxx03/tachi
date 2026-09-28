package main

// 一条等待「已经有答案了」的通报。
//
// 前端自己回答时它不需要这条消息——那张卡片是它自己清掉的。但**从设备进来的回答它
// 不知道**：设备走的是 desktop/link 那条路，直接调后端的回答函数，窗口里那张卡片
// 于是会一直挂着，而且还能再点一次（点下去后端会说「没有这条等待」，但用户看到的
// 是一张活着的卡片）。
//
// 所以回答成功就广播一次，谁在听谁撤卡片。前端自己回答时这条也会到，重复清一次
// 没有副作用。
type AskAnsweredEvent struct {
	SessionID string `json:"sessionId"`
	ToolID    string `json:"toolId"`
	Kind      string `json:"kind"` // permission / ask_user
}

func (d *desktopApp) notifyAskAnswered(sessionID, toolID string, kind pendingAskKind) {
	if d.app == nil {
		return
	}
	d.app.Event.Emit("agent:ask_answered", AskAnsweredEvent{
		SessionID: sessionID,
		ToolID:    toolID,
		Kind:      string(kind),
	})
}
