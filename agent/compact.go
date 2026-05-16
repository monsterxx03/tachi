package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// BuildCompactInstruction builds the instruction portion of the compact prompt.
// Unlike BuildCompactPrompt, it does NOT embed conversation history as text —
// the history is expected to be passed as structured conversation context
// (e.g. via RunConversationStream with the session's prior messages).
// System messages are skipped in the instruction since they are already
// transmitted as structured context.
func BuildCompactInstruction() string {
	return "请将以上对话历史压缩成一份简洁但全面的综合摘要。\n\n" +
		"摘要必须包含：\n" +
		"1. 用户的主要目标和问题\n" +
		"2. 已完成的关键操作和修改（具体文件名、命令、配置变更）\n" +
		"3. 重要发现和结论\n" +
		"4. 当前状态和待解决问题\n" +
		"5. 工作环境（目录、分支等）\n\n" +
		"约束：\n" +
		"- 保持信息密度，删除重复和无关内容\n" +
		"- 保留具体的文件名、命令、配置值等关键细节\n" +
		"- 不要添加新的分析或建议——只总结已经发生的事情\n" +
		"- 不要调用任何工具，直接输出摘要文本\n\n" +
		"请输出压缩摘要："
}

// BuildCompactPrompt builds the prompt asking the LLM to produce a conversation summary.
// history is the in-memory conversation. System messages are skipped.
// Tool result content exceeding 500 chars is truncated in the prompt.
//
// Deprecated: prefer BuildCompactInstruction() and pass history as structured
// context via RunOneOffStream's history parameter. This function embeds history
// as text in the prompt, which wastes tokens compared to structured context.
func BuildCompactPrompt(history []llm.Message) string {
	var sb strings.Builder
	sb.WriteString("你是一个对话压缩助手。请将以下对话历史压缩成一份简洁但全面的综合摘要。\n\n")
	sb.WriteString("摘要必须包含：\n")
	sb.WriteString("1. 用户的主要目标和问题\n")
	sb.WriteString("2. 已完成的关键操作和修改（具体文件名、命令、配置变更）\n")
	sb.WriteString("3. 重要发现和结论\n")
	sb.WriteString("4. 当前状态和待解决问题\n")
	sb.WriteString("5. 工作环境（目录、分支等）\n\n")
	sb.WriteString("约束：\n")
	sb.WriteString("- 保持信息密度，删除重复和无关内容\n")
	sb.WriteString("- 保留具体的文件名、命令、配置值等关键细节\n")
	sb.WriteString("- 不要添加新的分析或建议——只总结已经发生的事情\n\n")
	sb.WriteString("---对话历史---\n\n")

	const maxToolResultLen = 500

	for _, msg := range history {
		if msg.Role == "system" {
			continue
		}

		switch msg.Role {
		case "user":
			sb.WriteString(fmt.Sprintf("用户: %s\n", truncateContent(msg.Content, 1000)))
		case "assistant":
			content := msg.Content
			if content != "" {
				sb.WriteString(fmt.Sprintf("助手: %s\n", truncateContent(content, 2000)))
			}
			if len(msg.ThinkingBlocks) > 0 {
				for _, tb := range msg.ThinkingBlocks {
					sb.WriteString(fmt.Sprintf("[思考: %s]\n", truncateContent(tb.Thinking, 500)))
				}
			}
			for _, tc := range msg.ToolCalls {
				sb.WriteString(fmt.Sprintf("[工具调用: %s(%s)]\n", tc.Function.Name, truncateContent(tc.Function.Arguments, 200)))
			}
		case "tool":
			content := msg.Content
			if len(content) > maxToolResultLen {
				content = content[:maxToolResultLen] + "..."
			}
			sb.WriteString(fmt.Sprintf("[工具结果: %s]\n", content))
		default:
			content := msg.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, content))
		}
	}

	sb.WriteString("\n---\n请输出压缩摘要：")
	return sb.String()
}

// truncateContent truncates s to at most maxLen characters.
// A "..." suffix is appended when truncation occurs.
func truncateContent(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

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
