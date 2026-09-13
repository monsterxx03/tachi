package agent

import (
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/strutil"
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
			if strutil.IsCJK(r) {
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

// EstimateTokenCount estimates the number of tokens in a string using the
// same character-class-aware heuristic as the internal token estimation.
// Exported for frontends that need per-delta estimates (e.g. the TUI's
// live tokens-per-minute status).
func EstimateTokenCount(s string) int64 {
	return approxTokenCount(s)
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
// This is a character-class estimate, and its bias depends on the content: it OVER-counts plain
// English prose (the 4-chars-per-token rule is deliberately pessimistic) and UNDER-counts mixed
// CJK + JSON, where whitespace is charged nothing and a Hanzi is assumed to cost one token —
// measured 0.815x against the provider's own accounting on a long Chinese conversation with tool
// results (a 716,714 estimate against 879,259 billed, ≈18% low). So the estimate is only half the
// story: what the frontends DISPLAY is anchored on the real prompt size of the last call
// (convState.contextEstimate), and this estimate supplies the movement since.
//
// The auto-compact THRESHOLD reads the anchored value too (see shouldAutoCompact), so the trigger
// and the meter agree — which is most of the point of anchoring: the reader can predict when it
// fires. The cooldown is the one comparison left on this raw estimate, and legitimately so: it asks
// whether the conversation has grown 20% since the last compaction, an estimate measured against
// itself, where the bias cancels.
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
		case "user", llm.RoleSteer:
			tb.UserMessages += msgTokens
		case "assistant":
			tb.AssistantMessages += msgTokens
		case "tool":
			tb.ToolResults += msgTokens
		case "system":
			// System prompt is already counted in tb.SystemPrompt above.
			// Skip to avoid double-counting in Other.
		default:
			// Any other unrecognized roles
			tb.Other += msgTokens
		}
	}

	tb.Total += tb.SystemPrompt + tb.InternalTools + tb.MCPTools
	return tb
}

// EstimateAndUpdateTokens estimates the total input tokens for the current
// messages and records them in the conversation state (a.conv), so that
// buildReminderContext sees the current (not previous-turn) context size.
// The categorised breakdown is stored alongside for the TUI statusbar and
// /usage report.
//
// rs is the owning run's state, or nil when called outside a run (e.g. ACP
// session/load priming the estimate before the first turn). One-off runs
// (rs.SkipSessionWrites) are skipped — they must not pollute the main
// conversation's estimate or side-effect auto-compact decisions.
func (a *AIAgent) EstimateAndUpdateTokens(rs *RunState, messages []llm.Message) {
	if rs != nil && rs.SkipSessionWrites {
		return
	}

	// Resolve the session for per-session MCP tool filtering. Called from
	// within a run (session exists) or from tests outside one (empty ID →
	// auto-load tools only, which is fine for an estimate).
	schemas := a.filterActiveSchemas(a.currentSessionID(), a.Config.ToolRegistry.GetSchemas())
	systemPrompt := ""
	if len(messages) > 0 && messages[0].Role == "system" {
		systemPrompt = messages[0].Content
	}
	tb := estimateInputTokens(messages, systemPrompt, schemas)
	a.conv.setEstimate(tb.Total, tb)
}

// LastTokenBreakdown returns the most recent token estimate breakdown
// computed by EstimateAndUpdateTokens. Returns a zero-value Breakdown
// if no estimate has been computed yet.
//
// To report the breakdown alongside its total, prefer LastInputEstimateWithBreakdown:
// calling LastInputEstimate and LastTokenBreakdown separately can straddle a
// concurrent turn's update and mix values from two different estimates.
func (a *AIAgent) LastTokenBreakdown() tokenbreakdown.Breakdown {
	return a.conv.snapshotBreakdown()
}

// LastInputEstimateWithBreakdown returns the context size to report and its breakdown, read
// atomically so the two always describe the same number.
//
// The total is ANCHORED on the last call's real prompt size when there is one (see
// convState.contextEstimate): the character estimate alone runs ~18% low on mixed CJK/JSON
// content, and a context ring that under-reports is worse than no ring. The breakdown is scaled to
// the reported total so its parts still add up to it.
func (a *AIAgent) LastInputEstimateWithBreakdown() (int64, tokenbreakdown.Breakdown) {
	est, tb := a.conv.estimateSnapshot()
	real := a.conv.contextEstimate()
	if real <= 0 || real == est {
		return est, tb
	}
	return real, tb.ScaleTo(real)
}

// shouldAutoCompact checks whether automatic compaction should be triggered.
// Returns true when all of the following hold:
//   - auto-compact is enabled in config
//   - context window is known
//   - the reported context size >= contextWindow * threshold
//   - not in cooldown (token estimate hasn't grown 20% since last compact)
//
// The size it compares is contextEstimate() — the SAME number the ring and the statusbar show, not
// the raw character estimate. The two disagree on mixed content (the estimate runs ~18% low there),
// and a trigger that disagreed with the meter would compact while the ring still looked roomy, or
// hold off while it warned; the reader has to be able to predict when it fires. Before the anchor
// existed the raw estimate was the only thing available.
//
// The cooldown is deliberately still on raw estimates (see convState.compactCooldown): it asks
// whether the conversation has grown 20% since the last compaction, a comparison of the estimate
// against ITSELF, where the bias — and so the units — cancel.
func (a *AIAgent) shouldAutoCompact() bool {
	if a.Config.FullConfig == nil || (a.Config.FullConfig.Compact.Auto != nil && !*a.Config.FullConfig.Compact.Auto) {
		return false
	}
	if a.ContextWindow() <= 0 {
		return false
	}
	if a.isCompactCooldown() {
		return false
	}
	pct := float64(a.conv.contextEstimate()) / float64(a.ContextWindow())
	return pct >= a.Config.FullConfig.Compact.Threshold
}
