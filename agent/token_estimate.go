package agent

import (
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
func estimateInputTokens(messages []llm.Message, systemPrompt string, schemas []tools.Schema) int64 {
	var total int64

	// System prompt
	total += approxTokenCount(systemPrompt)

	// Tool schemas — each tool's name, description, and parameter schemas
	for _, s := range schemas {
		total += approxTokenCount(s.Name)
		total += approxTokenCount(s.Description)
		for name, prop := range s.Parameters.Properties {
			total += approxTokenCount(name)
			total += approxTokenCount(prop.Description)
		}
		// Overhead for JSON schema structure (~8 tokens per property)
		total += int64(len(s.Parameters.Properties)) * 8
	}
	// Overhead for tool array (~4 tokens per tool)
	total += int64(len(schemas)) * 4

	// Messages
	for _, msg := range messages {
		total += approxTokenCount(string(msg.Role))
		total += approxTokenCount(msg.Content)
		for _, part := range msg.ContentParts {
			total += approxTokenCount(string(part.Type))
			total += approxTokenCount(part.Text)
		}
		for _, tc := range msg.ToolCalls {
			total += approxTokenCount(tc.ID)
			total += approxTokenCount(tc.Function.Name)
			total += approxTokenCount(tc.Function.Arguments)
		}
		total += approxTokenCount(msg.ToolCallID)
	}

	return total
}

// estimateAndUpdateTokens estimates the total input tokens for the current
// messages and updates a.lastInputTokens so that buildReminderContext and
// TokenWarningReminder see the current (not previous-turn) context size.
func (a *AIAgent) estimateAndUpdateTokens(messages []llm.Message) {
	schemas := a.filterActiveSchemas(a.toolRegistry.GetSchemas())
	systemPrompt := ""
	if len(messages) > 0 && messages[0].Role == "system" {
		systemPrompt = messages[0].Content
	}
	a.lastInputTokens = estimateInputTokens(messages, systemPrompt, schemas)
}
