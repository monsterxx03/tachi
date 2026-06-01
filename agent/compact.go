package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// FinalizeCompact creates a new session containing the compacted summary,
// links it bidirectionally to the old session, and returns the new conversation
// history (with system prompt prepended) ready for RunConversationStream.
//
// The old session's meta is updated with compacted_child_id.
// ThreadID migration is handled by the caller if needed.
//
// Parameters:
//   - sm: session manager with the OLD session loaded as current
//   - systemPrompt: prepended as the first message in the returned history
//   - summary: the LLM-generated summary text
//
// Returns:
//   - newHistory: []llm.Message ready for RunConversationStream
//   - err: any error during session creation or persistence
func FinalizeCompact(sm *session.Manager, systemPrompt string, summary string) ([]llm.Message, error) {
	oldSess := sm.Current()
	if oldSess == nil {
		return nil, fmt.Errorf("no active session to compact")
	}

	// 1. Create new session (becomes current)
	newSess, err := sm.New(oldSess.Provider, oldSess.Model, oldSess.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("create compact session: %w", err)
	}

	// 2. Write compact messages to the new session
	now := time.Now()
	if err := sm.AppendMessage(&session.Message{
		Type:      session.MessageTypeAssistant,
		Timestamp: now,
		Content:   fmt.Sprintf("历史摘要：\n\n%s\n\n---\n以上是之前对话的压缩摘要。", summary),
	}); err != nil {
		return nil, fmt.Errorf("append summary message: %w", err)
	}
	if err := sm.AppendMessage(&session.Message{
		Type:      session.MessageTypeUser,
		Timestamp: now,
		Content:   "请基于以上摘要继续对话。",
	}); err != nil {
		return nil, fmt.Errorf("append continue message: %w", err)
	}

	// 3. Update new session meta
	newSess.Title = oldSess.Title
	newSess.CompactedParentID = oldSess.ID
	newSess.CompactedParentTitle = oldSess.Title
	if err := sm.UpdateMeta(newSess); err != nil {
		return nil, fmt.Errorf("update new session meta: %w", err)
	}

	// 4. Update old session meta
	oldSess.CompactedChildID = newSess.ID
	if err := sm.UpdateMeta(oldSess); err != nil {
		// Non-fatal: log but continue. The new session is already created.
		// The old session's compacted_child_id is missing, but the new
		// session's compacted_parent_id is correct — one-way link still works.
		_ = err // best-effort
	}

	// 5. Build and return history
	return buildCompactHistory(systemPrompt, summary), nil
}

// buildCompactHistory constructs the llm.Message history for the compacted session.
// It includes the system prompt (always present), the summary as an assistant
// message, and a "continue" user message.
//
// This history is non-empty, so RunConversationStream will NOT inject the system
// prompt automatically — the system prompt must be explicitly included here.
func buildCompactHistory(systemPrompt, summary string) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "assistant", Content: fmt.Sprintf("历史摘要：\n\n%s\n\n---\n以上是之前对话的压缩摘要。", summary)},
		{Role: "user", Content: "请基于以上摘要继续对话。"},
	}
}

// DrainCompactEvents collects the final assistant response from a RunOneOffStream
// event channel and returns the full response text. Non-text events (tool calls,
// thinking, etc.) are consumed but ignored. Returns an error if the channel
// ends without a complete turn or with an error.
func DrainCompactEvents(ch <-chan AgentEvent) (string, error) {
	var text strings.Builder
	var lastErr error

	for event := range ch {
		switch event.Type {
		case AgentEventTextDelta:
			text.WriteString(event.TextDelta)
		case AgentEventThinkingDelta:
			// Consume but ignore
		case AgentEventToolConfirmation:
			// Should not happen (compact agent doesn't have EditFile),
			// but consume gracefully — just ignore (relevant channel handle)
		case AgentEventAskUser:
			// Should not happen (AskUser unregistered), but consume gracefully
		case AgentEventToolCallStart, AgentEventToolCallArgs, AgentEventToolResult:
			// Consume tool events silently
		case AgentEventTurnComplete:
			if event.Result != nil {
				if event.Result.Response != "" {
					text.Reset()
					text.WriteString(event.Result.Response)
				}
				if event.Result.Error != nil {
					lastErr = event.Result.Error
				}
			}
		case AgentEventError:
			if event.Result != nil {
				if event.Result.Response != "" {
					text.Reset()
					text.WriteString(event.Result.Response)
				}
				if event.Result.Error != nil {
					lastErr = event.Result.Error
				}
			}
		}
	}

	result := strings.TrimSpace(text.String())
	if result == "" && lastErr != nil {
		return "", lastErr
	}
	return result, nil
}
