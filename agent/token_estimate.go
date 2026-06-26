package agent

import (
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
)

// approxTokenCount estimates the number of tokens in a string using the
// chars/4 heuristic. This is the same approach used by Pi Agent, Crush
// (OpenCode Go), and anomalyco/opencode (TypeScript). It deliberately
// overestimates to provide conservative context window warnings.
//
//	chars/4 = ~3.5 English chars per token (Anthropic's recommended heuristic)
//	(len(s) + 3) / 4 = integer ceil(len/4) in Go
func approxTokenCount(s string) int64 {
	return int64((len(s) + 3) / 4)
}

// estimateInputTokens estimates the total input tokens that will be consumed
// by the given messages, system prompt, and tool schemas. Used to provide
// proactive context window warnings before the API call.
//
// Accepts []tools.Schema directly instead of []llm.Tool to avoid the
// allocation overhead of buildLLMTools conversion in the hot path
// (estimateAndUpdateTokens is called multiple times per agent loop).
//
// The estimate is deliberately conservative (overestimates) to trigger timely
// warnings and compaction. Once the API responds, the actual InputTokens from
// the response replace this estimate.
func estimateInputTokens(messages []llm.Message, systemPrompt string, schemas []tools.Schema) tokenbreakdown.Breakdown {
	var tb tokenbreakdown.Breakdown

	// System prompt
	tb.SystemPrompt = approxTokenCount(systemPrompt)

	// Tool schemas — split between internal and MCP
	var internalCount, mcpCount int64
	for _, s := range schemas {
		var toolTokens int64
		toolTokens += approxTokenCount(s.Name)
		toolTokens += approxTokenCount(s.Description)
		for name, prop := range s.Parameters.Properties {
			toolTokens += approxTokenCount(name)
			toolTokens += approxTokenCount(prop.Description)
		}
		// Overhead for JSON schema structure (~8 tokens per property)
		toolTokens += int64(len(s.Parameters.Properties)) * 8

		if tools.IsMCPSchema(s.Name) {
			tb.MCPTools += toolTokens
			mcpCount++
		} else {
			tb.InternalTools += toolTokens
			internalCount++
		}
	}
	// Tool array overhead (~4 tokens per tool), split proportionally
	tb.InternalTools += internalCount * 4
	tb.MCPTools += mcpCount * 4

	// Messages — categorize by role
	for _, msg := range messages {
		var msgTokens int64
		msgTokens += approxTokenCount(string(msg.Role))
		msgTokens += approxTokenCount(msg.Content)
		for _, part := range msg.ContentParts {
			msgTokens += approxTokenCount(string(part.Type))
			msgTokens += approxTokenCount(part.Text)
		}
		for _, tc := range msg.ToolCalls {
			msgTokens += approxTokenCount(tc.ID)
			msgTokens += approxTokenCount(tc.Function.Name)
			msgTokens += approxTokenCount(tc.Function.Arguments)
		}
		msgTokens += approxTokenCount(msg.ToolCallID)

		// All messages contribute to Total
		tb.Total += msgTokens

		switch msg.Role {
		case "user":
			tb.UserMessages += msgTokens
		case "assistant":
			tb.AssistantMessages += msgTokens
		// system/tool/steer messages contribute to Total but not to
		// the named categories — they're not user or assistant msgs
		}
	}

	tb.Total += tb.SystemPrompt + tb.InternalTools + tb.MCPTools
	return tb
}

// estimateAndUpdateTokens estimates the total input tokens for the current
// messages and updates a.lastInputTokens so that buildReminderContext and
// TokenWarningReminder see the current (not previous-turn) context size.
// Also updates a.lastInputEstimate for the TUI statusbar context fraction.
func (a *AIAgent) estimateAndUpdateTokens(messages []llm.Message) {
	schemas := a.filterActiveSchemas(a.toolRegistry.GetSchemas())
	systemPrompt := ""
	if len(messages) > 0 && messages[0].Role == "system" {
		systemPrompt = messages[0].Content
	}
	tb := estimateInputTokens(messages, systemPrompt, schemas)
	a.lastInputTokens = tb.Total
	a.lastTokenBreakdown = tb
}

// LastTokenBreakdown returns the most recent token estimate breakdown
// computed by estimateAndUpdateTokens. Returns a zero-value Breakdown
// if no estimate has been computed yet.
func (a *AIAgent) LastTokenBreakdown() tokenbreakdown.Breakdown {
	return a.lastTokenBreakdown
}

// shouldAutoCompact checks whether automatic compaction should be triggered.
// Returns true when all of the following hold:
//   - auto-compact is enabled in config
//   - context window is known
//   - estimated input tokens >= contextWindow * threshold
//   - not in cooldown (token estimate hasn't grown 20% since last compact)
func (a *AIAgent) shouldAutoCompact() bool {
	if a.cfg == nil || !a.cfg.Compact.Auto {
		return false
	}
	if a.contextWindow <= 0 {
		return false
	}
	if a.isCompactCooldown() {
		return false
	}
	pct := float64(a.lastInputTokens) / float64(a.contextWindow)
	return pct >= a.cfg.Compact.Threshold
}
