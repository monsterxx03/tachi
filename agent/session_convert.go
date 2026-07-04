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
func ConvertSessionToLLMMessages(sessionMsgs []session.Message, providerName string, cfg *config.Config) ([]llm.Message, error) {
	// Resolve provider type from the provider name so we know how to handle
	// provider-specific message formats (e.g., thinking blocks for OpenAI).
	providerType := providerName // fallback: treat as type string directly
	if cfg != nil {
		if pCfg := cfg.FindProvider(providerName); pCfg != nil {
			providerType = pCfg.Type
		}
	}
	var result []llm.Message

	// Buffered state for grouping related messages into a single assistant message.
	var thinkingBlocks []llm.ThinkingBlock
	var assistantText string
	var toolCalls []llm.ToolCall

	// Tool results are buffered so they appear after the assistant message
	// that contains their tool_calls — which is what the LLM API expects.
	var pendingToolResults []llm.Message

	flushAssistant := func() {
		if len(thinkingBlocks) == 0 && assistantText == "" && len(toolCalls) == 0 {
			return
		}

		content := assistantText

		// OpenAI doesn't support thinking blocks natively — prepend them
		// to the Content field so the conversation context is preserved.
		if providerType == llm.ProviderTypeOpenAI && len(thinkingBlocks) > 0 {
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
			result = append(result, llm.Message{
				Role:    "user",
				Content: msg.Content,
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
		}
	}

	// Flush any remaining buffered content.
	flushAssistant()
	flushToolResults()

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
