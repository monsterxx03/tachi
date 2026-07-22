// Package tokenbreakdown holds the categorized token estimate breakdown
// produced by the agent's local chars/4 heuristic. It lives in its own
// package so both agent/ (producer) and agent/commands/ (consumer via
// UsageReportInfo) can reference it without circular imports.
package tokenbreakdown

// Breakdown categorizes the local token estimate into named buckets.
// Total matches the value stored in a.lastInputTokens — all existing
// consumers (compact threshold, token-warning reminders, TUI statusbar
// context %) continue to work unchanged.
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
