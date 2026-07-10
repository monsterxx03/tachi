package agent

import (
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// charsPerToken is the approximate number of ASCII alphanumeric characters
// per token in BPE tokenizers (cl100k_base, etc.). Anthropic's recommended
// heuristic is ~3.5, using 4 gives a slight overestimate which is desirable
// for conservative context window warnings.
const charsPerToken = 4

// approxTokenCount estimates the number of tokens in a string using
// character-class-aware heuristics that approximate BPE tokenizer behaviour
// (cl100k_base / o200k_base). It is more accurate than a naive chars/4
// approach for mixed CJK/English text and punctuation-heavy content (JSON,
// code, tool arguments) while remaining conservative for plain English prose.
//
// Rules:
//   - ASCII letters/digits: chars/4 (words tend to merge into 1 token)
//   - ASCII punctuation/symbols: 1 token each (punctuation almost always
//     gets its own token in BPE tokenizers)
//   - CJK characters (Hanzi, Hiragana, Katakana, Hangul): 1 token each
//     (CJK characters tokenize roughly 1:1)
//   - Whitespace: 0 tokens (leading whitespace is merged into the next
//     token in most tokenizers)
//   - Other Unicode: byte-length/4 (fallback for emoji, symbols, etc.)
func approxTokenCount(s string) int64 {
	var total int64
	var asciiWordLen int // consecutive ASCII alphanumeric chars

	flushWord := func() {
		if asciiWordLen > 0 {
			total += int64((asciiWordLen + charsPerToken - 1) / charsPerToken)
			asciiWordLen = 0
		}
	}

	for _, r := range s {
		if r <= 0x7F {
			if isASCIIAlphaNum(r) {
				asciiWordLen++
			} else {
				flushWord()
				if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
					// Whitespace: merged into next token, negligible cost
				} else {
					// Punctuation/symbol: typically gets its own token
					total++
				}
			}
		} else {
			flushWord()
			if isCJK(r) {
				total++ // ~1 token per CJK character
			} else {
				// Other Unicode: approximate by byte length
				total += int64((utf8.RuneLen(r) + charsPerToken - 1) / charsPerToken)
			}
		}
	}
	flushWord()

	return total
}

// isASCIIAlphaNum reports whether r is an ASCII letter or digit.
func isASCIIAlphaNum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isCJK reports whether r is a CJK character (Chinese/Japanese/Korean).
// Covers the most common Unicode blocks: CJK Unified Ideographs, Hiragana,
// Katakana, and Hangul.
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Unified Ideographs Extension A
		(r >= 0x20000 && r <= 0x2A6DF) || // CJK Unified Ideographs Extension B
		(r >= 0x3040 && r <= 0x309F) || // Hiragana
		(r >= 0x30A0 && r <= 0x30FF) || // Katakana
		(r >= 0xAC00 && r <= 0xD7AF) // Hangul Syllables
}

// EstimateContentTokens converts session messages to llm.Message (via
// ConvertSessionToLLMMessages) and delegates to estimateInputTokens, ensuring
// token estimation is consistent with estimateAndUpdateTokens (used in the
// agent loop). Returns the total estimated token count.
//
// providerName and cfg are passed through to ConvertSessionToLLMMessages for
// correct provider-specific message regrouping. When providerName is empty the
// conversion falls back to a best-effort mapping, which is sufficient for
// threshold checks.
func EstimateContentTokens(msgs []session.Message, providerName string) int64 {
	llmMsgs, err := ConvertSessionToLLMMessages(msgs, providerName)
	if err != nil {
		// If conversion fails, fall back to a rough chars/4 estimate.
		// This is acceptable for pre-compaction threshold checks.
		var total int64
		for _, msg := range msgs {
			total += approxTokenCount(msg.Content)
			total += approxTokenCount(msg.Name)
			total += approxTokenCount(msg.Result)
			total += approxTokenCount(msg.ToolCallID)
			if s, ok := msg.Args.(string); ok {
				total += approxTokenCount(s)
			}
		}
		return total
	}
	// System prompt and tool schemas are excluded since they contribute a
	// small constant fraction that doesn't affect threshold decisions.
	breakdown := estimateInputTokens(llmMsgs, "", nil)
	return breakdown.Total
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
