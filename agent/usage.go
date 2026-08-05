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
	ContextWindow int64
	ToolCalls     map[string]*ToolCallStat // keyed by tool name
	MainCount     int                      // tool calls in main session
	SubCount      int                      // tool calls in sub-agents

	// Usage ledger aggregation (docs/2026-08-05-usage-billing.md).
	// Cost is computed EXCLUSIVELY from the ledger — never from
	// messages/subagent transcripts (which would double-count subagent
	// rows, since subagent calls are also in the ledger).
	Cost            float64                             // total cost (all kinds)
	LedgerAvailable bool                                // false = pre-upgrade session (no ledger rows)
	KindCosts       map[llm.UsageKind]cmds.KindCostStat // per-kind breakdown
	ModelCosts      map[string]float64                  // "provider:model" → cost
	UnpricedCalls   int                                 // rows without an effective price at call time
}

// ComputeSessionUsage computes the SessionUsageReport for the given session
// by scanning its messages (and sub-agent JSONL files) for token/tool stats,
// plus the usage ledger for cost.
//
// sm must have the target session already loaded as current (e.g. via
// sm.Load(sessID) or sm.FindByThreadID).
//
// rec may be nil — the ledger is then skipped and LedgerAvailable stays false
// (cost section shows "no billing data").
// contextWindow is optional (0 = not shown).
func ComputeSessionUsage(sm SessionManager, rec *llm.UsageRecorder, contextWindow int64) (*SessionUsageReport, error) {
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

	// ---- cost: usage ledger only ----
	report := &SessionUsageReport{
		Session:       curr,
		Usage:         usage,
		ContextWindow: contextWindow,
	}
	if rec != nil {
		// Scan lower bound: session IDs embed the creation date
		// (YYYY-MM-DD-…), so day files older than the session can be skipped.
		rows, rErr := rec.Rows(curr.ID, curr.CreatedAt)
		if rErr != nil {
			return nil, fmt.Errorf("load usage ledger: %w", rErr)
		}
		report.LedgerAvailable = len(rows) > 0
		report.KindCosts = make(map[llm.UsageKind]cmds.KindCostStat)
		report.ModelCosts = make(map[string]float64)
		for _, row := range rows {
			c := row.Cost()
			report.Cost += c
			ks := report.KindCosts[row.Kind]
			ks.Cost += c
			ks.Calls++
			report.KindCosts[row.Kind] = ks
			report.ModelCosts[row.Provider+":"+row.Model] += c
			if row.Unpriced() {
				report.UnpricedCalls++
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
	report.ToolCalls = tc
	report.MainCount = mainCount
	report.SubCount = subCount

	return report, nil
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
		LedgerAvailable:          report.LedgerAvailable,
		KindCosts:                report.KindCosts,
		ModelCosts:               report.ModelCosts,
		UnpricedCalls:            report.UnpricedCalls,
		ToolCalls:                toolCalls,
		MainCount:                report.MainCount,
		SubCount:                 report.SubCount,
		PprofAddr:                pprofAddr,
	}
	info.EstBreakdown = estBreakdown
	return info
}
