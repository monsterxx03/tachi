package agent

import (
	"fmt"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// ToolCallStat records per-tool call counts and error counts.
type ToolCallStat struct {
	Count    int
	ErrCount int
}

// SessionUsageReport aggregates token usage, cost, and tool call statistics
// for a session. Computed once by ComputeSessionUsage, then formatted
// differently by TUI (Markdown) and channel (plain text) callers.
type SessionUsageReport struct {
	Session       *session.Session
	Usage         llm.Usage
	Cost          float64
	ContextWindow int64
	ToolCalls     map[string]*ToolCallStat // keyed by tool name
	MainCount     int                      // tool calls in main session
	SubCount      int                      // tool calls in sub-agents
}

// ComputeSessionUsage computes the SessionUsageReport for the given session
// by scanning its messages (and sub-agent JSONL files). It reconstructs
// cumulative usage, calculates cost, and counts tool calls per name.
//
// sm must have the target session already loaded as current (e.g. via
// sm.Load(sessID) or sm.FindByThreadID).
//
// price may be nil — in that case Cost is left at 0.
// contextWindow is optional (0 = not shown).
func ComputeSessionUsage(sm SessionManager, price *llm.ModelPrice, contextWindow int64) (*SessionUsageReport, error) {
	if sm == nil || !sm.HasCurrent() {
		return nil, fmt.Errorf("no active session")
	}

	curr := sm.Current()
	msgs, err := sm.LoadMessages()
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}

	// ---- usage ----
	var totalInput, totalOutput, totalCacheRead, totalCacheCreation int64
	var lastInput int64
	for _, msg := range msgs {
		if msg.Usage != nil {
			if msg.Usage.InputTokens > 0 {
				lastInput = msg.Usage.InputTokens
			}
			totalInput += msg.Usage.InputTokens
			totalOutput += msg.Usage.OutputTokens
			totalCacheCreation += msg.Usage.CacheCreationInputTokens
			totalCacheRead += msg.Usage.CacheReadInputTokens
		}
	}
	usage := llm.Usage{
		InputTokens:              totalInput,
		LastInputTokens:          lastInput,
		OutputTokens:             totalOutput,
		CacheCreationInputTokens: totalCacheCreation,
		CacheReadInputTokens:     totalCacheRead,
	}

	// ---- cost ----
	var cost float64
	if price != nil {
		cost = llm.CalculateCost(&usage, price)
		subMsgs, _ := sm.LoadSubagentMessages(curr.ID)
		for _, sMsgs := range subMsgs {
			for _, msg := range sMsgs {
				if msg.Usage != nil {
					cost += llm.CalculateCost(&llm.Usage{
						InputTokens:              msg.Usage.InputTokens,
						OutputTokens:             msg.Usage.OutputTokens,
						CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
						CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
					}, price)
				}
			}
		}
	}

	// ---- tool calls ----
	tc := make(map[string]*ToolCallStat)
	mainCount := 0
	countMessages(msgs, tc, &mainCount)

	subCount := 0
	subMsgsMap, _ := sm.LoadSubagentMessages(curr.ID)
	for _, sMsgs := range subMsgsMap {
		countMessages(sMsgs, tc, &subCount)
	}

	return &SessionUsageReport{
		Session:       curr,
		Usage:         usage,
		Cost:          cost,
		ContextWindow: contextWindow,
		ToolCalls:     tc,
		MainCount:     mainCount,
		SubCount:      subCount,
	}, nil
}

// countMessages scans a message slice, counting tool calls and errors into
// the provided map. total is incremented for each MessageTypeToolCall found.
func countMessages(msgs []session.Message, tc map[string]*ToolCallStat, total *int) {
	for _, msg := range msgs {
		if msg.Type == session.MessageTypeToolCall {
			*total++
			name := msg.Name
			if name == "" {
				name = "(unknown)"
			}
			st, ok := tc[name]
			if !ok {
				st = &ToolCallStat{}
				tc[name] = st
			}
			st.Count++
		}
		if msg.Type == session.MessageTypeToolResult && msg.IsError {
			if st, ok := tc[msg.Name]; ok {
				st.ErrCount++
			}
		}
	}
}

// BuildUsageReportInfo converts a SessionUsageReport into the shared
// presentation struct used by FormatUsageReport across TUI / channel / ACP.
// The three frontends previously each hand-built the 15-field struct (plus
// the ToolCallStat map conversion) — this keeps that in one place.
//
// estTokens and estBreakdown come from the frontend's live estimate source
// (they differ per mode: TUI uses totalUsage.LastInputTokens, channel uses
// getAgentEstimateWithBreakdown, ACP uses LastInputEstimate). pprofAddr is
// the debug server address shown in the report footer.
func BuildUsageReportInfo(report *SessionUsageReport, estTokens int64, estBreakdown tokenbreakdown.Breakdown, pprofAddr string) *cmds.UsageReportInfo {
	toolCalls := make(map[string]*cmds.ToolCallStat, len(report.ToolCalls))
	for name, st := range report.ToolCalls {
		toolCalls[name] = &cmds.ToolCallStat{Count: st.Count, ErrCount: st.ErrCount}
	}
	info := &cmds.UsageReportInfo{
		SessionID:                report.Session.ID,
		Provider:                 report.Session.ProviderName,
		Title:                    report.Session.Title,
		ContextWindow:            report.ContextWindow,
		InputTokens:              report.Usage.InputTokens,
		LastInputTokens:          report.Usage.LastInputTokens,
		CacheReadInputTokens:     report.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: report.Usage.CacheCreationInputTokens,
		OutputTokens:             report.Usage.OutputTokens,
		EstimatedInputTokens:     estTokens,
		Cost:                     report.Cost,
		ToolCalls:                toolCalls,
		MainCount:                report.MainCount,
		SubCount:                 report.SubCount,
		PprofAddr:                pprofAddr,
	}
	info.EstBreakdown = estBreakdown
	return info
}
