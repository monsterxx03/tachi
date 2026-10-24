package main

import (
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
)

// 事件投影：**绝不原样转发 agent.AgentEvent**。
//
// 那个事件在每轮结束时带着 Messages（整段历史），在工具结果里带着完整输出（一条 ReadFile
// 可以几十 KB）。线上只走投影 —— 手机屏幕本来就会把这些折叠起来，完整输出是桌面的事。
//
// 挂点都是现有的「唯一一条路」，不新开旁路：handleEventIn / registerAsk / notifyAskAnswered /
// setSessionState / emitIdle / tpsAdd / tpsReset / emitUsage。

// mobileProjectEvent forwards one agent event to the phones subscribed to this session.
//
// The lane gate is the same one the transcript uses: a side-channel run (/review, /commit) has
// no conversation turn on screen, so its text is not part of this conversation and must not be
// appended to the phone's either.
func (d *desktopApp) mobileProjectEvent(id string, ev agent.AgentEvent, lane runLane) {
	if !lane.intoTranscript() {
		return
	}
	switch ev.Type {
	case agent.AgentEventTextDelta:
		if ev.TextDelta != "" {
			d.mobileNotify(id, map[string]any{"type": mobileEvText, "text": ev.TextDelta})
		}
	case agent.AgentEventThinkingDelta:
		if ev.ThinkingDelta != "" {
			d.mobileNotify(id, map[string]any{"type": mobileEvThinking, "text": ev.ThinkingDelta})
		}
	case agent.AgentEventToolCallStart:
		d.mobileNotify(id, map[string]any{
			"type":     mobileEvToolStart,
			"toolId":   ev.ToolID,
			"toolName": ev.ToolName,
			"title":    tools.ToolArgsSummary(ev.ToolName, ev.ToolArgs),
		})
	case agent.AgentEventToolCallArgs:
		d.mobileNotify(id, map[string]any{
			"type":   mobileEvToolArgs,
			"toolId": ev.ToolID,
			"args":   ev.ToolArgs,
		})
	case agent.AgentEventToolResult:
		result, truncated := truncateMobileText(ev.ToolResult, mobileToolResultLimit)
		d.mobileNotify(id, map[string]any{
			"type":      mobileEvToolResult,
			"toolId":    ev.ToolID,
			"isError":   ev.ToolIsError,
			"result":    result,
			"truncated": truncated,
		})
	case agent.AgentEventTurnComplete:
		evt := map[string]any{"type": mobileEvTurn, "end": mobileTurnEnd(ev.Result)}
		if ev.Result != nil {
			evt["durationMs"] = ev.Result.Duration.Milliseconds()
			evt["iterations"] = ev.Result.IterationsUsed
		}
		d.mobileNotify(id, evt)
	case agent.AgentEventError:
		message := "见日志"
		interrupted := false
		if ev.Result != nil {
			if ev.Result.Error != nil {
				message = ev.Result.Error.Error()
			}
			interrupted = ev.Result.ExitReason == agent.ExitReasonInterrupted ||
				ev.Result.ExitReason == agent.ExitReasonCancelled
		}
		d.mobileNotify(id, map[string]any{
			"type": mobileEvError, "message": message, "interrupted": interrupted,
		})
	case agent.AgentEventAutoCompactStart:
		d.mobileNotify(id, map[string]any{"type": mobileEvCompactStart})
	case agent.AgentEventAutoCompactDone:
		evt := map[string]any{"type": mobileEvCompactDone}
		if ev.Result != nil && ev.Result.Error != nil {
			evt["error"] = ev.Result.Error.Error()
		}
		// 成功时这里只说「压完了」。搬到子会话的事走 session_moved —— 那一步发生在
		// 这一轮结束之后（followCompaction），和桌面拿到的 agent:session_switched 是同一刻。
		d.mobileNotify(id, evt)
	}
}

// mobileTurnEnd maps a run's exit reason onto the three words the phone knows. A user's stop is
// "interrupted", not an error: the same distinction the desktop makes between an ⚠ and a normal
// conclusion.
func mobileTurnEnd(res *agent.RunResult) string {
	if res == nil {
		return "complete"
	}
	switch res.ExitReason {
	case agent.ExitReasonInterrupted, agent.ExitReasonCancelled:
		return "interrupted"
	case agent.ExitReasonError, agent.ExitReasonBudgetExhausted:
		return "error"
	}
	return "complete"
}

// truncateMobileText cuts a tool result to the protocol's limit, on a rune boundary so the JSON
// encoder does not have to replace a half character.
func truncateMobileText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// mobileAskOf renders one pending wait in the wire's shape. The card's content is the agent's own
// (preview / questions); the phone does not re-describe it.
func mobileAskOf(a *pendingAsk) mobileAsk {
	ma := mobileAsk{ToolID: a.toolID}
	switch a.kind {
	case askKindPermission:
		ma.Kind = mobileAskKindPermission
		ma.ToolName = a.toolName
		ma.PermissionKind = permEventKind(a.toolName)
		ma.Preview = a.preview
	case askKindAskUser:
		ma.Kind = mobileAskKindQuestion
		ma.Questions = make([]mobileQuestion, 0, len(a.questions))
		for _, q := range a.questions {
			labels := make([]string, 0, len(q.Options))
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			ma.Questions = append(ma.Questions, mobileQuestion{
				Question: q.Question, Options: labels, MultiSelect: q.MultiSelect,
			})
		}
	}
	return ma
}

// notifyMobileAsk tells the subscribed phone that a turn just parked on the user.
func (d *desktopApp) notifyMobileAsk(a *pendingAsk) {
	d.mobileNotify(a.sessionID, map[string]any{"type": mobileEvAsk, "ask": mobileAskOf(a)})
}

// notifyMobileUserMessage echoes a user message to the other screens (design §4.5).
//
// The desktop window draws its own bubble at submit time — that bubble never comes from an
// event — so a message sent from the phone would otherwise appear as a reply to nothing. The
// `origin` is what lets each screen skip its own echo.
func (d *desktopApp) notifyMobileUserMessage(sessionID, text string, steer bool, origin messageOrigin) {
	d.mobileNotify(sessionID, map[string]any{
		"type":        mobileEvUserMessage,
		"text":        text,
		"displayText": text,
		"steer":       steer,
		"origin":      string(origin),
	})
}

// notifySessionPrefs tells the OTHER screens that a session's two switches moved (the model it
// runs on, and whether ask rules still raise a card).
//
// Both travel together because they are the two per-session facts a phone can change, and a
// screen that re-read one without the other could show a pair that never existed. It is emitted
// by the WRITERS (switchProviderFor / setSkipApprovalFor), not by the endpoints, so the window's
// own controls produce it too — there is one fact, and everyone who changed it says so.
func (d *desktopApp) notifySessionPrefs(sessionID, provider string, skipApproval bool) {
	if d.hasUI() {
		d.emit("agent:session_prefs", map[string]any{
			"sessionId":    sessionID,
			"provider":     provider,
			"skipApproval": skipApproval,
		})
	}
	d.mobileNotify(sessionID, map[string]any{
		"type":         mobileEvSessionPrefs,
		"provider":     provider,
		"skipApproval": skipApproval,
	})
}

// sessionPrefs reads the record's two switches — the durable half every screen shows.
func (d *desktopApp) sessionPrefs(id string) (provider string, skipApproval bool) {
	if ss := d.sessionRecord(id); ss != nil {
		return ss.ProviderName, ss.SkipApproval
	}
	return "", false
}

// echoUserMessage is the same echo on the desktop event bus. It is emitted for every origin: the
// window ignores its own (origin == desktop) because it already drew that bubble, and appends
// everyone else's.
func (d *desktopApp) echoUserMessage(sessionID, text string, steer bool, origin messageOrigin) {
	if d.hasUI() {
		d.emit("agent:user_message", map[string]any{
			"sessionId":   sessionID,
			"text":        text,
			"displayText": text,
			"steer":       steer,
			"origin":      string(origin),
		})
	}
	d.notifyMobileUserMessage(sessionID, text, steer, origin)
}
