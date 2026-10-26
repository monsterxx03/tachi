package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// ImageResolver yields the bytes of an image blob an image part refers to (session.Part.Ref).
// *session.Manager implements it, scoped to its current session exactly as LoadMessages is —
// a record is only meaningful next to the directory its references live in.
//
// It is a parameter rather than something this package looks up because where the bytes live is
// the CALLER's knowledge, and because a conversion that forgot to resolve would silently send a
// history whose images are gone, which is the failure this whole path exists to end. nil is a
// deliberate choice to convert text only (token estimates; records that carry their own bytes).
type ImageResolver interface {
	LoadImage(ref string) ([]byte, error)
}

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
//
// Image parts are restored too, through images: a record keeps the parts a message was SENT with
// (session.Message.Parts) and their bytes as a blob reference, so the rebuilt request is the
// request that was sent — an image missing here would break the provider's prefix cache from
// that image to the end of the conversation, and would leave the model unable to see an image it
// was in fact given. A part whose blob is gone is dropped (the record's text still says an image
// was there) rather than failing the conversion.
func ConvertSessionToLLMMessages(sessionMsgs []session.Message, providerType string, images ImageResolver) ([]llm.Message, error) {
	var result []llm.Message

	// Buffered state for grouping related messages into a single assistant message.
	var thinkingBlocks []llm.ThinkingBlock
	var assistantText string
	var toolCalls []llm.ToolCall

	// One loader for the whole conversion, so an image sent twice is read (and base64-encoded)
	// once.
	parts := newPartImages(images)

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

	for i, msg := range sessionMsgs {
		switch msg.Type {
		case session.MessageTypeUser:
			flushAssistant()
			flushToolResults()
			content := msg.Content
			if len(pendingReminders) > 0 {
				content = reminderPrefix(pendingReminders) + content
				pendingReminders = nil
			}
			result = append(result, llm.Message{
				Role:         "user",
				Content:      content,
				ContentParts: rebuildParts(msg.Parts, parts),
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
				Role:         "tool",
				Content:      msg.Result,
				ToolCallID:   msg.ToolCallID,
				Name:         msg.Name,
				IsError:      msg.IsError,
				ContentParts: rebuildParts(msg.Parts, parts),
			})

		case session.MessageTypeConfirm:
			// Confirm messages are UI-only — skip them.
			continue

		case session.MessageTypeReminder:
			// Two shapes wear the same type, and only their POSITION tells them apart.
			//
			// A block whose next non-reminder record is the user message is that
			// message's wrapper: the live turn prepended it (systemreminder.WrapUserMessage),
			// and an artifact block may be followed by that turn's date block before the
			// user text arrives. Buffering it is what this variable is for.
			//
			// Anything else was injected MID-turn: the loop appends one as a user-role
			// message of its own, right after that iteration's tool results
			// (agent_loop.go's injectLoopReminders). Buffering it would instead attach a
			// stale block — a finished background task, an old plan id, old diagnostics —
			// to a much later turn, and shift every request's prefix with it, so the
			// reloaded history stops matching the provider's prompt cache. Emit it in
			// place, which is where it was sent.
			if reminderBelongsToNextUserMessage(sessionMsgs, i) {
				pendingReminders = append(pendingReminders, msg.Content)
				continue
			}
			flushAssistant()
			flushToolResults()
			result = append(result, llm.Message{Role: "user", Content: msg.Content})
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

	// Strip the tool calls no tool result answers.
	//
	// LLM APIs (DeepSeek, Anthropic) require every tool_use block to have a corresponding
	// tool_result — the request is rejected outright otherwise. A history rebuilt here can hold an
	// unanswered call for two reasons: the process died mid-turn (the loop records each CALL before
	// it runs and each RESULT as it finishes — tool_executor.go — so a batch killed halfway leaves
	// an assistant message whose calls are only PARTLY answered), or the user continued from
	// another frontend without ever running that call.
	//
	// The pairing is per CALL, by id: the old check asked only whether ANY tool result followed the
	// message and kept the whole batch when one did, so an orphan rode along for the rest of the
	// conversation, with the session broken until someone edited the record by hand.
	//
	// A message left with nothing at all — no text, no thinking, no calls — is dropped with its
	// calls: it says nothing, and an empty assistant message is not a valid one either.
	answered := make(map[string]bool)
	for _, m := range result {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	kept := result[:0]
	for i := range result {
		m := result[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			kept = append(kept, m)
			continue
		}
		calls := m.ToolCalls[:0]
		for _, tc := range m.ToolCalls {
			if answered[tc.ID] {
				calls = append(calls, tc)
			}
		}
		m.ToolCalls = calls
		if m.IsEmptyAssistant() {
			continue
		}
		kept = append(kept, m)
	}

	return kept, nil
}

// reminderPrefix renders buffered reminder blocks the way the live turn wraps a user
// message: systemreminder.WrapUserMessage prepends the rendered block, which already
// ends with a newline, so the user's own text follows it directly. Adding one more
// newline here is not cosmetic — it changes the request's prefix, and the provider's
// prompt cache then misses on every turn that was appended since the last reload (the
// reloaded history has to be the history that was sent, byte for byte).
func reminderPrefix(blocks []string) string {
	prefix := strings.Join(blocks, "\n")
	if !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	return prefix
}

// reminderBelongsToNextUserMessage reports whether the reminder at index i is the
// wrapper of the user message that follows it, as opposed to a block the loop injected
// mid-turn (see the MessageTypeReminder case). Consecutive reminders all belong to the
// same following message — an artifact block is routinely followed by that turn's date
// block — and an artifact reminder at the very end of the session has no user message to
// wrap at all, so it is left buffered for the trailing flush, which emits it on its own.
//
// Position alone is not quite enough: a block injected mid-turn can be the LAST record of
// its turn (the loop injected it and nothing followed — a stop or a hard failure), and
// then the next user message belongs to another turn. The iteration the records already
// carry separates the two remaining shapes: a wrapper is recorded with the same iteration
// as the message it wraps (0 for a turn's reminder + its user message; the continuation
// prompt's reminder + prompt share theirs), while a stranded block's iteration belongs to
// the turn that already ended.
func reminderBelongsToNextUserMessage(msgs []session.Message, i int) bool {
	for j := i + 1; j < len(msgs); j++ {
		switch msgs[j].Type {
		case session.MessageTypeReminder, session.MessageTypeConfirm:
			continue // another block of the same wrapper; confirm records are UI-only
		case session.MessageTypeUser:
			return msgs[i].Iteration == msgs[j].Iteration
		default:
			return false
		}
	}
	return true
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
