package main

// Context-ring popover: the token breakdown behind the composer's context ring.
//
// The ring itself only has room for one number — the fraction of the context
// window in use. Clicking it opens these numbers split into the same buckets
// /usage reports (see agent/tokenbreakdown), which is what answers "the window
// is 80% full, of WHAT?": system prompt, tool schemas, the conversation
// itself, or a pile of tool output. Nothing here is persisted — the breakdown
// is derived from the live agent's most recent estimate (see
// AIAgent.LastInputEstimateWithBreakdown).

import "github.com/monsterxx03/tachi/agent/tokenbreakdown"

// ContextPartVO is one bucket of the local input-token estimate. Key is stable
// and doubles as the frontend's colour hook (--ctx-<key> in base.css); Label is
// the wording the popover shows.
type ContextPartVO struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Tokens int64  `json:"tokens"`
}

// ContextInfoVO is the ring's popover payload. Estimate is the total of Parts
// (0 = nothing measured yet); ContextWindow is 0 when the model's window is
// unknown, in which case the frontend shows tokens without a percentage.
type ContextInfoVO struct {
	Estimate      int64           `json:"estimate"`
	ContextWindow int64           `json:"contextWindow"`
	Parts         []ContextPartVO `json:"parts"`
}

// contextParts maps a breakdown onto display buckets, dropping the empty ones
// (a popover listing six zeros tells the user nothing) and keeping the order
// /usage uses: prompt → tool schemas → conversation → tool output.
func contextParts(tb tokenbreakdown.Breakdown) []ContextPartVO {
	all := []ContextPartVO{
		{Key: "system", Label: "系统提示词", Tokens: tb.SystemPrompt},
		{Key: "internalTools", Label: "内置工具", Tokens: tb.InternalTools},
		{Key: "mcpTools", Label: "MCP 工具", Tokens: tb.MCPTools},
		{Key: "userMessages", Label: "用户消息", Tokens: tb.UserMessages},
		{Key: "assistantMessages", Label: "助手消息", Tokens: tb.AssistantMessages},
		{Key: "toolResults", Label: "工具结果", Tokens: tb.ToolResults},
		{Key: "other", Label: "其他", Tokens: tb.Other},
	}
	parts := make([]ContextPartVO, 0, len(all))
	for _, p := range all {
		if p.Tokens > 0 {
			parts = append(parts, p)
		}
	}
	return parts
}

// GetContextInfo returns the context-usage breakdown for the session the ring
// belongs to. An unknown session (or one whose agent is not built yet) yields
// the zero value, which the frontend renders as "not measured yet".
func (s *AgentService) GetContextInfo(id string) ContextInfoVO {
	d := s.desk
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	if r == nil || r.agent == nil {
		return ContextInfoVO{}
	}

	info := ContextInfoVO{ContextWindow: r.agent.ContextWindow()}
	// Total and breakdown must come from ONE snapshot: reading them separately
	// can straddle a concurrent turn's update and mix two estimates.
	est, tb := r.agent.LastInputEstimateWithBreakdown()
	if est <= 0 {
		// Resumed session, no turn in this app run yet: only the total survives
		// on disk (a message's estimated_input_tokens — the breakdown is not
		// persisted), which is exactly what /usage falls back to as well.
		est = d.estimateFromMessages(r)
	}
	info.Estimate = est
	info.Parts = contextParts(tb)
	return info
}
