package acp

import (
	"context"
	"fmt"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// streamToACP consumes the AgentEvent channel and converts events into ACP
// session/update notifications. Blocks until the channel is closed or ctx is cancelled.
// Returns the final StopReason and cumulative usage from the last turn.
func streamToACP(
	ctx context.Context,
	sess *ACPSession,
	conn *acp.AgentSideConnection,
	events <-chan agent.AgentEvent,
) (acp.StopReason, *llm.Usage) {
	sessionID := acp.SessionId(sess.ID)
	stopReason := acp.StopReasonEndTurn
	var lastUsage *llm.Usage

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return stopReason, lastUsage
			}

			switch event.Type {
			case agent.AgentEventTextDelta:
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: sessionID,
					Update:    acp.UpdateAgentMessageText(event.TextDelta),
				})

			case agent.AgentEventThinkingDelta:
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: sessionID,
					Update:    acp.UpdateAgentThoughtText(event.ThinkingDelta),
				})

			case agent.AgentEventToolCallStart:
				update := acp.StartToolCall(
					acp.ToolCallId(event.ToolID),
					event.ToolName,
					acp.WithStartKind(mapToolKind(event.ToolName)),
					acp.WithStartStatus(acp.ToolCallStatusInProgress),
				)
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: sessionID,
					Update:    update,
				})

			case agent.AgentEventToolResult:
				status := acp.ToolCallStatusCompleted
				if event.ToolIsError {
					status = acp.ToolCallStatusFailed
				}
				update := acp.UpdateToolCall(
					acp.ToolCallId(event.ToolID),
					acp.WithUpdateStatus(status),
				)
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: sessionID,
					Update:    update,
				})

			case agent.AgentEventTurnComplete:
				if event.Result != nil {
					stopReason = mapStopReason(event.Result.ExitReason)
					lastUsage = event.Result.Usage
					// Send turn summary (iterations + duration) as a final text update.
					if event.Result.IterationsUsed > 0 {
						if summary := agent.FormatTurnSummary(event.Result.IterationsUsed, event.Result.Duration); summary != "" {
							_ = conn.SessionUpdate(ctx, acp.SessionNotification{
								SessionId: sessionID,
								Update:    acp.UpdateAgentMessageText(summary),
							})
						}
					}
					// Send token usage update so Zed can show context window usage.
					// Uses the same values as the TUI statusbar:
					//   Used = LastInputEstimate() (local chars/4 heuristic)
					//   Size = ContextWindow() (model's context window size)
					{
						cw := sess.agent.ContextWindow()
						used := sess.agent.LastInputEstimate()
						if used > 0 && cw > 0 {
							_ = conn.SessionUpdate(ctx, acp.SessionNotification{
								SessionId: sessionID,
								Update: acp.SessionUpdate{
									UsageUpdate: &acp.SessionUsageUpdate{
										Size: int(cw),
										Used: int(used),
									},
								},
							})
						}
					}
				}
				// Cache the full message history so subsequent Prompt calls
				// can reuse it instead of re-reading messages.jsonl from disk.
				if event.Messages != nil {
					sess.history = event.Messages
				}

			case agent.AgentEventSessionTitle:
				// Send title update to the editor so it can display the session name
				title := event.Title
				if title != "" {
					_ = conn.SessionUpdate(ctx, acp.SessionNotification{
						SessionId: sessionID,
						Update: acp.SessionUpdate{
							SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
								Title: new(title),
							},
						},
					})
				}

			case agent.AgentEventAutoCompactStart:
				// Compact in progress; send a brief notification.
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: sessionID,
					Update:    acp.UpdateAgentMessageText("🔄 正在压缩对话历史……"),
				})

			case agent.AgentEventAutoCompactDone:
				if event.Result != nil && event.Result.Error != nil {
					// Compact failed.
					_ = conn.SessionUpdate(ctx, acp.SessionNotification{
						SessionId: sessionID,
						Update:    acp.UpdateAgentMessageText(fmt.Sprintf("⚠️ 对话压缩失败: %v", event.Result.Error)),
					})
				} else if event.CompactSummary != "" {
					// Compact succeeded — send the summary.
					summary := fmt.Sprintf("🔍 **对话已压缩**（旧消息数: %d 条）\n\n%s", event.OldMsgCount, event.CompactSummary)
					_ = conn.SessionUpdate(ctx, acp.SessionNotification{
						SessionId: sessionID,
						Update:    acp.UpdateAgentMessageText(summary),
					})
				}

			case agent.AgentEventError:
				stopReason = acp.StopReasonEndTurn
				if event.Result != nil && event.Result.Error != nil {
					_ = conn.SessionUpdate(ctx, acp.SessionNotification{
						SessionId: sessionID,
						Update:    acp.UpdateAgentMessageText("Error: " + event.Result.Error.Error()),
					})
				}

			case agent.AgentEventToolConfirmation:
				// In ACP mode with PermissionModeExternal, this should not fire.
				// If it does (defensive), auto-approve via the agent's confirm channel.
				sess.agent.ConfirmTool(true)

				// Events we intentionally ignore in ACP mode:
				// AgentEventToolCallArgs — incremental args, ACP doesn't need
				// AgentEventSteerCheck — ACP doesn't use steer
				// AgentEventSubagentStart/Done — internal detail
				// AgentEventUsage — internal stats
				// AgentEventAskUser — AskUser tool is unregistered
			}

		case <-ctx.Done():
			return acp.StopReasonCancelled, lastUsage
		}
	}
}

// mapToolKind maps a Tachi tool name to an ACP ToolKind.
func mapToolKind(toolName string) acp.ToolKind {
	switch toolName {
	case tools.ToolNameRead:
		return acp.ToolKindRead
	case tools.ToolNameWrite, tools.ToolNameEdit:
		return acp.ToolKindEdit
	case tools.ToolNameBash:
		return acp.ToolKindExecute
	case tools.ToolNameGlob, tools.ToolNameGrep:
		return acp.ToolKindSearch
	case tools.ToolNameWebSearch, tools.ToolNameWebFetch:
		return acp.ToolKindFetch
	default:
		return ""
	}
}

// mapStopReason maps Tachi's ExitReason to ACP StopReason.
func mapStopReason(exitReason string) acp.StopReason {
	switch exitReason {
	case "stop":
		return acp.StopReasonEndTurn
	case "cancelled", "interrupted":
		return acp.StopReasonCancelled
	default:
		return acp.StopReasonEndTurn
	}
}

// replaySessionHistory replays all stored messages from a loaded session as ACP
// session/update notifications. This satisfies the session/load protocol requirement
// that the agent MUST replay the entire conversation history to the client.
func replaySessionHistory(ctx context.Context, conn *acp.AgentSideConnection, sess *ACPSession) {
	if conn == nil || sess.sessMgr == nil {
		return
	}

	msgs, err := sess.sessMgr.LoadMessages()
	if err != nil || len(msgs) == 0 {
		return
	}

	sessionID := acp.SessionId(sess.ID)

	for _, msg := range msgs {
		switch msg.Type {
		case session.MessageTypeUser:
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sessionID,
				Update:    acp.UpdateUserMessageText(msg.Content),
			})

		case session.MessageTypeAssistant:
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sessionID,
				Update:    acp.UpdateAgentMessageText(msg.Content),
			})

		case session.MessageTypeThinking:
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sessionID,
				Update:    acp.UpdateAgentThoughtText(msg.Content),
			})

		case session.MessageTypeToolCall:
			update := acp.StartToolCall(
				acp.ToolCallId(msg.ToolCallID),
				msg.Name,
				acp.WithStartKind(mapToolKind(msg.Name)),
				acp.WithStartStatus(acp.ToolCallStatusInProgress),
			)
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sessionID,
				Update:    update,
			})

		case session.MessageTypeToolResult:
			status := acp.ToolCallStatusCompleted
			if msg.IsError {
				status = acp.ToolCallStatusFailed
			}
			update := acp.UpdateToolCall(
				acp.ToolCallId(msg.ToolCallID),
				acp.WithUpdateStatus(status),
			)
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sessionID,
				Update:    update,
			})

			// MessageTypeConfirm is internal — skip in replay
		}
	}

	// Cache the converted LLM history so the first Prompt call
	// reuses it instead of re-reading messages.jsonl from disk.
	if llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.ProviderType()); convErr == nil {
		sess.history = llmMsgs
	} else {
		debuglog.DefaultLogger.Log("ACP: replaySessionHistory ConvertSessionToLLMMessages failed: %v", convErr)
	}
}
