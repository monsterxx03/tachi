package acp

import (
	"context"
	"encoding/json"
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
	toolArgs := make(map[string]string)      // toolID → accumulated args JSON
	pendingStarts := make(map[string]string) // toolID → toolName (buffered start, sent when args arrive)

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
				// Buffer the start — we'll send it with rawInput when args arrive.
				pendingStarts[event.ToolID] = event.ToolName

			case agent.AgentEventToolCallArgs:
				// Accumulate tool args for extraction.
				toolArgs[event.ToolID] += event.ToolArgs

				if toolName, pending := pendingStarts[event.ToolID]; pending {
					// First args arrival: send StartToolCall with rawInput + locations included.
					delete(pendingStarts, event.ToolID)
					title := buildToolTitle(toolName, toolArgs[event.ToolID])
					opts := []acp.ToolCallStartOpt{
						acp.WithStartKind(mapToolKind(toolName)),
						acp.WithStartStatus(acp.ToolCallStatusInProgress),
					}
					if parsed := parseRawInput(toolArgs[event.ToolID]); parsed != nil {
						opts = append(opts, acp.WithStartRawInput(parsed))
					}
					if path, line := extractFileLocation(toolArgs[event.ToolID]); path != "" {
						opts = append(opts, acp.WithStartLocations([]acp.ToolCallLocation{{
							Path: path,
							Line: line,
						}}))
					}
					_ = conn.SessionUpdate(ctx, acp.SessionNotification{
						SessionId: sessionID,
						Update:    acp.StartToolCall(acp.ToolCallId(event.ToolID), title, opts...),
					})
				} else {
					// Subsequent args: update rawInput + locations.
					var updateOpts []acp.ToolCallUpdateOpt
					if parsed := parseRawInput(toolArgs[event.ToolID]); parsed != nil {
						updateOpts = append(updateOpts, acp.WithUpdateRawInput(parsed))
					}
					if path, line := extractFileLocation(toolArgs[event.ToolID]); path != "" {
						updateOpts = append(updateOpts, acp.WithUpdateLocations([]acp.ToolCallLocation{{
							Path: path,
							Line: line,
						}}))
					}
					if len(updateOpts) > 0 {
						_ = conn.SessionUpdate(ctx, acp.SessionNotification{
							SessionId: sessionID,
							Update:    acp.UpdateToolCall(acp.ToolCallId(event.ToolID), updateOpts...),
						})
					}
				}

			case agent.AgentEventToolResult:
				status := acp.ToolCallStatusCompleted
				if event.ToolIsError {
					status = acp.ToolCallStatusFailed
				}
				updateOpts := []acp.ToolCallUpdateOpt{
					acp.WithUpdateStatus(status),
				}
				// For EditFile/WriteFile, attach diff content so Zed can
				// render a proper diff view (green additions, red deletions).
				if event.ToolName == tools.ToolNameEdit || event.ToolName == tools.ToolNameWrite {
					if diffContent := buildDiffFromArgs(event.ToolName, toolArgs[event.ToolID]); diffContent != nil {
						updateOpts = append(updateOpts, acp.WithUpdateContent([]acp.ToolCallContent{*diffContent}))
					}
				}
				update := acp.UpdateToolCall(
					acp.ToolCallId(event.ToolID),
					updateOpts...,
				)
				_ = conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: sessionID,
					Update:    update,
				})
				// For SavePlan, also emit an ACP plan session update so the
				// editor can display a structured plan panel (steps with
				// pending/in_progress/completed status).
				if event.ToolName == tools.ToolNameSavePlan && !event.ToolIsError {
					if planUpdate := buildPlanUpdateFromArgs(toolArgs[event.ToolID]); planUpdate != nil {
						_ = conn.SessionUpdate(ctx, acp.SessionNotification{
							SessionId: sessionID,
							Update:    *planUpdate,
						})
					}
				}
				// After each tool result, send a UsageUpdate so Zed can show
				// real-time context window usage in its status bar.
				sendUsageUpdate(conn, sessionID, sess)

			case agent.AgentEventTurnComplete:
				// Clear buffers for the next turn.
				clear(toolArgs)
				clear(pendingStarts)
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
					sendUsageUpdate(conn, sessionID, sess)
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
				// Save partial messages so the next Prompt (e.g. Zed Steer)
				// can resume from where we left off instead of starting over.
				if event.Messages != nil {
					sess.history = stripPendingToolCalls(event.Messages)
				}
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

			case agent.AgentEventUsage:
				// After each API round, send a UsageUpdate so Zed can display
				// real-time context window consumption in its status bar.
				sendUsageUpdate(conn, sessionID, sess)

				// Events we intentionally ignore in ACP mode:
				// AgentEventSteerCheck — ACP doesn't use steer
				// AgentEventSubagentStart/Done — internal detail
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

// parseRawInput attempts to parse accumulated tool args JSON into a rawInput value.
// Returns nil if the JSON is invalid (incremental args may not be complete yet).
func parseRawInput(argsJSON string) any {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil
	}
	if len(args) == 0 {
		return nil
	}
	return args
}

// buildToolTitle constructs a descriptive title from the tool name and args,
// using shared arg extraction from tools.ToolArgsTitle.
func buildToolTitle(toolName, argsJSON string) string {
	summary := tools.ToolArgsTitle(toolName, argsJSON)
	if summary == "" || summary == toolName {
		return toolName
	}
	switch toolName {
	case tools.ToolNameRead:
		return "Read " + summary
	case tools.ToolNameWrite:
		return "Write " + summary
	case tools.ToolNameEdit:
		return "Edit " + summary
	case tools.ToolNameBash:
		return "Run `" + summary + "`"
	case tools.ToolNameGlob:
		return "Find `" + summary + "`"
	case tools.ToolNameGrep:
		return "Search `" + summary + "`"
	case tools.ToolNameWebSearch:
		return "Search " + summary
	case tools.ToolNameWebFetch:
		return "Fetch " + summary
	case tools.ToolNameLSP:
		return "LSP " + summary
	case tools.ToolNameSubAgent:
		return "SubAgent: " + summary
	}
	return toolName
}

// replayToolTitle builds a title for replay, handling both string and map args.
func replayToolTitle(toolName string, args any) string {
	switch a := args.(type) {
	case string:
		return buildToolTitle(toolName, a)
	case map[string]any:
		if len(a) == 0 {
			return toolName
		}
		b, err := json.Marshal(a)
		if err != nil {
			return toolName
		}
		return buildToolTitle(toolName, string(b))
	default:
		return toolName
	}
}

// extractFileLocation attempts to extract a file path and optional line number
// from the tool call arguments. Tools that have a `line` parameter (e.g. LSP)
// or an `offset` parameter (e.g. ReadFile, 1-indexed) will provide line numbers
// for precise navigation in the editor UI.
func extractFileLocation(argsJSON string) (path string, line *int) {
	var args struct {
		Path   string `json:"path"`
		Line   *int   `json:"line,omitempty"`
		Offset int    `json:"offset,omitempty"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", nil
	}
	if args.Line != nil {
		return args.Path, args.Line
	}
	if args.Offset > 0 {
		// ReadFile's offset is 1-indexed, same as ACP's line.
		return args.Path, &args.Offset
	}
	return args.Path, nil
}

// buildDiffFromArgs attempts to build a diff content block from tool call arguments.
// For EditFile, it extracts path, old_string, new_string.
// For WriteFile (new file), it extracts path and content (new) with no old text.
// Returns nil if args are missing or incomplete.
func buildDiffFromArgs(toolName string, argsJSON string) *acp.ToolCallContent {
	if argsJSON == "" {
		return nil
	}

	switch toolName {
	case tools.ToolNameEdit:
		var args struct {
			Path      string `json:"path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return nil
		}
		if args.Path == "" || (args.OldString == "" && args.NewString == "") {
			return nil
		}
		c := acp.ToolDiffContent(args.Path, args.NewString, args.OldString)
		return &c

	case tools.ToolNameWrite:
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return nil
		}
		if args.Path == "" || args.Content == "" {
			return nil
		}
		// WriteFile creates/replaces a file — show new content without old text.
		c := acp.ToolDiffContent(args.Path, args.Content)
		return &c
	}

	return nil
}

// buildPlanUpdateFromArgs parses SavePlan tool args and builds an ACP plan
// session update with structured entries. Returns nil if args are invalid.
func buildPlanUpdateFromArgs(argsJSON string) *acp.SessionUpdate {
	if argsJSON == "" {
		return nil
	}
	var args struct {
		Title string `json:"title"`
		Steps []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil
	}
	if len(args.Steps) == 0 {
		return nil
	}

	entries := make([]acp.PlanEntry, 0, len(args.Steps))

	for _, s := range args.Steps {
		var status acp.PlanEntryStatus
		switch s.Status {
		case "in_progress":
			status = acp.PlanEntryStatusInProgress
		case "completed":
			status = acp.PlanEntryStatusCompleted
		default:
			status = acp.PlanEntryStatusPending
		}
		entries = append(entries, acp.PlanEntry{
			Content:  s.Content,
			Priority: acp.PlanEntryPriorityMedium,
			Status:   status,
		})
	}

	update := acp.UpdatePlan(entries...)
	return &update
}

// sendUsageUpdate sends a UsageUpdate notification to the ACP client with the
// current context window usage estimate, matching the values shown in the TUI
// statusbar (LastInputEstimate / ContextWindow). Skips sending if either value
// is zero (agent not fully initialized).
func sendUsageUpdate(conn *acp.AgentSideConnection, sessionID acp.SessionId, sess *ACPSession) {
	if conn == nil || sess == nil || sess.agent == nil {
		return
	}
	cw := sess.agent.ContextWindow()
	used := sess.agent.LastInputEstimate()
	if used <= 0 || cw <= 0 {
		return
	}
	_ = conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.SessionUpdate{
			UsageUpdate: &acp.SessionUsageUpdate{
				Size: int(cw),
				Used: int(used),
			},
		},
	})
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
			title := replayToolTitle(msg.Name, msg.Args)
			opts := []acp.ToolCallStartOpt{
				acp.WithStartKind(mapToolKind(msg.Name)),
				acp.WithStartStatus(acp.ToolCallStatusInProgress),
			}
			if msg.Args != nil {
				opts = append(opts, acp.WithStartRawInput(msg.Args))
			}
			update := acp.StartToolCall(
				acp.ToolCallId(msg.ToolCallID),
				title,
				opts...,
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

// stripPendingToolCalls removes dangling ToolCalls from the last assistant
// message when there are no corresponding tool results. This prevents API
// errors when a cancelled turn's partial message history is reused as the
// context for the next Prompt (e.g. Zed Steer).
//
// The agent loop accumulates messages incrementally:
//  1. assistantMessage (with tool_calls) is appended BEFORE tool execution
//  2. tool results are appended AFTER execution
//
// If cancellation happens during step 2, we'd have tool_calls without results
// which violates the LLM API's alternating role requirement.
func stripPendingToolCalls(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return msgs
	}
	cleaned := make([]llm.Message, len(msgs))
	copy(cleaned, msgs)

	// Walk backwards to find the last assistant message with tool_calls.
	for i := len(cleaned) - 1; i >= 0; i-- {
		if cleaned[i].Role != "assistant" || len(cleaned[i].ToolCalls) == 0 {
			continue
		}
		// Check if any tool result exists AFTER this message.
		hasResults := false
		for j := i + 1; j < len(cleaned); j++ {
			if cleaned[j].Role == "tool" {
				hasResults = true
				break
			}
		}
		if !hasResults {
			cleaned[i].ToolCalls = nil
		}
		break // only the last assistant message matters
	}
	return cleaned
}
