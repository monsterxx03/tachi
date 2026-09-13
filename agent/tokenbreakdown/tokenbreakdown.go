// Package tokenbreakdown holds the categorized token estimate breakdown
// produced by the agent's local chars/4 heuristic. It lives in its own
// package so both agent/ (producer) and agent/commands/ (consumer via
// UsageReportInfo) can reference it without circular imports.
package tokenbreakdown

// Breakdown categorizes the local token estimate into named buckets.
// Total matches the value stored in a.lastInputTokens — all existing
// consumers (compact threshold, token-warning reminders, TUI statusbar
// context %) continue to work unchanged.
// ScaleTo returns the breakdown scaled so its parts sum to total, keeping their proportions.
// Used when the reported total is anchored on a REAL prompt size (see convState.contextEstimate):
// the parts describe the same prompt, so a popover whose parts did not add up to the number above
// them would read as a bug.
func (b Breakdown) ScaleTo(total int64) Breakdown {
	if b.Total <= 0 || total <= 0 || total == b.Total {
		return b
	}
	scale := func(v int64) int64 { return v * total / b.Total }
	b.SystemPrompt = scale(b.SystemPrompt)
	b.InternalTools = scale(b.InternalTools)
	b.MCPTools = scale(b.MCPTools)
	b.UserMessages = scale(b.UserMessages)
	b.AssistantMessages = scale(b.AssistantMessages)
	b.ToolResults = scale(b.ToolResults)
	b.Other = scale(b.Other)
	b.Total = b.SystemPrompt + b.InternalTools + b.MCPTools + b.UserMessages +
		b.AssistantMessages + b.ToolResults + b.Other
	return b
}

type Breakdown struct {
	SystemPrompt      int64 // system prompt text only
	InternalTools     int64 // built-in tool schemas (name + description + parameters + overhead)
	MCPTools          int64 // MCP tool schemas (name + description + parameters + overhead)
	UserMessages      int64 // messages with Role "user" (content + content parts + tool_call_id)
	AssistantMessages int64 // messages with Role "assistant" (content + content parts + tool calls + tool_call_id)
	ToolResults       int64 // messages with Role "tool" (tool execution outputs)
	Other             int64 // messages with unrecognized roles (not "user"/"steer"/"assistant"/"tool"/"system")
	Total             int64 // sum of all categories
}
