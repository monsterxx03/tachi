package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// ConvertSessionToLLMMessages converts a flat list of session messages back into
// the provider-specific llm.Message format suitable for reloading a conversation.
//
// The key challenge is that session messages are recorded chronologically but
// must be regrouped for the LLM API:
//   - Consecutive thinking + assistant + tool_call → single assistant message
//   - Tool results remain individual messages (Anthropic provider groups them later)
//
// For OpenAI (which doesn't support native thinking blocks), thinking content is
// prepended to the assistant text content so context is preserved.
//
// Reminder blocks are NOT stripped: they were stored as separate MessageTypeReminder
// entries ahead of their MessageTypeUser, and are prepended to the user message
// content during conversion. This preserves the <system-reminder> prefix in the
// loaded history, which agent_loop.go's historyHasReminder() relies on to avoid
// re-injecting first-message-only reminders (project context, git status) on
// subsequent turns after the history was reloaded from disk (e.g. after /commit).
func ConvertSessionToLLMMessages(sessionMsgs []session.Message, providerType string) ([]llm.Message, error) {
	var result []llm.Message

	// Buffered state for grouping related messages into a single assistant message.
	var thinkingBlocks []llm.ThinkingBlock
	var assistantText string
	var toolCalls []llm.ToolCall

	// Tool results are buffered so they appear after the assistant message
	// that contains their tool_calls — which is what the LLM API expects.
	var pendingToolResults []llm.Message

	// pendingReminders buffers consecutive MessageTypeReminder contents to
	// be prepended to the subsequent MessageTypeUser, preserving the
	// <system-reminder> prefix that historyHasReminder() checks for.
	// ACCUMULATING: an artifact reminder (from /research or /review) is
	// routinely followed by that turn's date reminder — a single-value
	// buffer would drop the artifact on the next disk reload, breaking
	// follow-up after a restart.
	var pendingReminders []string

	flushAssistant := func() {
		if len(thinkingBlocks) == 0 && assistantText == "" && len(toolCalls) == 0 {
			return
		}

		content := assistantText

		// OpenAI doesn't support thinking blocks natively — prepend them
		// to the Content field so the conversation context is preserved.
		if providerType == config.ProviderTypeOpenAI && len(thinkingBlocks) > 0 {
			var parts []string
			for _, tb := range thinkingBlocks {
				parts = append(parts, tb.Thinking)
			}
			thinkingContent := strings.Join(parts, "\n")
			if content != "" {
				content = thinkingContent + "\n\n" + content
			} else {
				content = thinkingContent
			}
			thinkingBlocks = nil // Don't attach to message; already embedded in Content
		}

		result = append(result, llm.Message{
			Role:           "assistant",
			Content:        content,
			ThinkingBlocks: thinkingBlocks,
			ToolCalls:      toolCalls,
		})

		thinkingBlocks = nil
		assistantText = ""
		toolCalls = nil
	}

	flushToolResults := func() {
		result = append(result, pendingToolResults...)
		pendingToolResults = nil
	}

	for _, msg := range sessionMsgs {
		switch msg.Type {
		case session.MessageTypeUser:
			flushAssistant()
			flushToolResults()
			content := msg.Content
			if len(pendingReminders) > 0 {
				content = strings.Join(pendingReminders, "\n") + "\n" + content
				pendingReminders = nil
			}
			result = append(result, llm.Message{
				Role:    "user",
				Content: content,
			})

		case session.MessageTypeThinking:
			// A new thinking block after tool calls means the LLM made
			// another API call — flush the previous assistant + results.
			if len(toolCalls) > 0 {
				flushAssistant()
				flushToolResults()
			}
			thinkingBlocks = append(thinkingBlocks, llm.ThinkingBlock{
				Type:      "thinking",
				Thinking:  msg.Content,
				Signature: msg.Signature,
			})

		case session.MessageTypeAssistant:
			// Assistant text after tool calls means a new API call —
			// flush the previous assistant + results.
			if len(toolCalls) > 0 {
				flushAssistant()
				flushToolResults()
			}
			assistantText = msg.Content

		case session.MessageTypeToolCall:
			argsStr, err := convertArgsToString(msg.Args)
			if err != nil {
				return nil, fmt.Errorf("tool_call %s: %w", msg.Name, err)
			}
			toolCalls = append(toolCalls, llm.ToolCall{
				ID:   msg.ToolCallID,
				Type: "function",
				Function: llm.ToolCallFunction{
					Name:      msg.Name,
					Arguments: argsStr,
				},
			})

		case session.MessageTypeToolResult:
			// Buffer tool results — they'll be flushed after the assistant
			// message that contains their tool_calls.
			pendingToolResults = append(pendingToolResults, llm.Message{
				Role:       "tool",
				Content:    msg.Result,
				ToolCallID: msg.ToolCallID,
				Name:       msg.Name,
				IsError:    msg.IsError,
			})

		case session.MessageTypeConfirm:
			// Confirm messages are UI-only — skip them.
			continue

		case session.MessageTypeReminder:
			// Accumulate; ALL buffered reminders are prepended to the next
			// user message. Keeps artifact reminders alive even when
			// followed by per-turn reminders like the date block.
			pendingReminders = append(pendingReminders, msg.Content)
		}
	}

	// Flush any remaining buffered content.
	flushAssistant()
	flushToolResults()

	// Trailing un-consumed reminders (no user message after them, e.g. an
	// artifact reminder appended by a completed /research or /review) must
	// still reach the LLM — otherwise reloading from disk would drop them,
	// breaking follow-up after agent eviction or a restart.
	if len(pendingReminders) > 0 {
		result = append(result, llm.Message{
			Role:    "user",
			Content: strings.Join(pendingReminders, "\n"),
		})
	}

	// Strip orphaned tool calls from all assistant messages.
	// LLM APIs (DeepSeek, Anthropic) require every tool_use block to have
	// a corresponding tool_result in the next message. When a session is
	// restored mid-turn (e.g., after a crash or interruption), an assistant
	// message may contain tool calls whose results were never saved — e.g.
	// a tool_call followed by user messages (user continued in a different
	// channel) without a matching tool_result. Stripping them prevents API
	// protocol errors.
	for i := len(result) - 1; i >= 0; i-- {
		if result[i].Role != "assistant" || len(result[i].ToolCalls) == 0 {
			continue
		}
		// Check if any tool result exists AFTER this message.
		hasResults := false
		for j := i + 1; j < len(result); j++ {
			if result[j].Role == "tool" {
				hasResults = true
				break
			}
		}
		if !hasResults {
			result[i].ToolCalls = nil
		}
	}

	return result, nil
}

// convertArgsToString normalizes the Args field (which is any in the session
// struct) to a JSON string suitable for llm.ToolCallFunction.Arguments.
func convertArgsToString(args any) (string, error) {
	if args == nil {
		return "{}", nil
	}
	switch v := args.(type) {
	case string:
		return v, nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("failed to marshal args: %w", err)
		}
		return string(data), nil
	}
}
