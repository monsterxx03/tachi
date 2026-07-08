package manager

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// drainEvents consumes all AgentEvents, returning the final assistant text or
// an error.
//
// For non-interactive channels AskUser is unregistered so AgentEventAskUser
// should never fire. For interactive channels (InteractiveChannel interface),
// drainEvents sends questions to the user via sendToThread, blocks on
// ta.askUserRespCh waiting for the handler to route the reply, then calls
// RespondToAskUser to resume the agent turn. When ta is nil (cron, tests) or
// the thread has no askUserThreadID set, AskUser auto-rejects.
//
// When ta is non-nil, drainEvents also handles AgentEventSteerCheck for
// mid-turn user input injection (steer mechanism). When the agent reaches a
// tool-call boundary and requests steer input, pending messages are drained
// and delivered to the agent. When ta is nil (cron, tests), steer is skipped.
//
// onTextDelta is an optional callback for streaming text output. It is called
// for each AgentEventTextDelta so channel implementations can push text in
// real time (e.g. Wave streaming cards). It may be nil.
func (m *Manager) drainEvents(ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent, sendProgress func(string), ta *threadActivation, onTextDelta StreamingCallback) (string, error) {
	var text strings.Builder
	var lastErr error
	pushedTools := make(map[string]bool) // tool IDs already streamed to card

	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			text.WriteString(event.TextDelta)
			if onTextDelta != nil {
				onTextDelta(event.TextDelta)
			}

		case agent.AgentEventThinkingDelta:
			// Thinking is internal to the agent; we don't expose it to IM.
			// The content is still recorded in the session for context
			// preservation on resume.

		case agent.AgentEventToolCallStart:
			m.logger.Log("channel: tool call start: %s", event.ToolName)

		case agent.AgentEventToolCallArgs:
			m.logger.Log("channel: tool call args for %s: %s", event.ToolName, event.ToolArgs)
			if onTextDelta != nil && !pushedTools[event.ToolID] {
				pushedTools[event.ToolID] = true
				onTextDelta("\n\n> <font color=\"comment\">🔧 " + event.ToolName + formatToolArgs(event.ToolName, event.ToolArgs) + "</font>\n\n")
			}

		case agent.AgentEventToolConfirmation:
			// Should not happen with skip_edit_confirm=true, but handle safely.
			m.logger.Log("channel: auto-approving unexpected confirmation: %s", event.ToolName)
			aiAgent.ConfirmTool(true)

		case agent.AgentEventAskUser:
			if ta != nil && ta.askUserThreadID != "" {
				ta.mu.Lock()
				ta.askUserRespCh = make(chan tools.AskUserResult, 1)
				threadID := ta.askUserThreadID
				replyID := ta.askUserReplyID
				ta.mu.Unlock()

				m.logger.Log("channel: AskUser — %d question(s) for thread=%s",
					len(event.Questions), threadID)

				// Deliver structured questions to the channel.
				m.presentQuestionsToChannel(threadID, replyID, convertQuestions(event.Questions))

				// Block until the handler routes a user reply into askUserRespCh,
				// or the agent context is cancelled.
				select {
				case resp := <-ta.askUserRespCh:
					m.logger.Log("channel: AskUser — received answer (%d entries)", len(resp.Answers))
					aiAgent.RespondToAskUser(resp.Answers, resp.Annotations)
				case <-ta.ctx.Done():
					m.logger.Log("channel: AskUser — cancelled")
					aiAgent.RespondToAskUser(nil, nil)
				}

				ta.mu.Lock()
				ta.askUserRespCh = nil
				ta.mu.Unlock()
			} else {
				m.logger.Log("channel: auto-rejecting AskUser (non-interactive)")
				aiAgent.RespondToAskUser(nil, nil)
			}

		case agent.AgentEventSteerCheck:
			// Only process steer when ta is non-nil (agent turn path).
			if ta == nil {
				continue
			}
			// Agent reached a tool boundary — inject any pending steer messages
			// and any buffered ambient (group chat) messages.
			ta.mu.Lock()
			var parts []string
			if len(ta.pending) > 0 {
				parts = append(parts, ta.pending...)
				ta.pending = nil
			}
			// Drain ambient messages as formatted steer context.
			if len(ta.ambientPending) > 0 {
				parts = append(parts, formatAmbientForSteer(ta.ambientPending))
				ta.ambientPending = nil
			}
			joined := ""
			if len(parts) > 0 {
				joined = strings.Join(parts, "\n\n")
				m.logger.Log("channel: steer inject thread=%s content=%d chars", "", len(joined))
			}
			ta.mu.Unlock()

			// Write to steerRespCh; agent is blocking on this read.
			// Use select with ctx fallback to avoid deadlock on cancellation.
			select {
			case ta.steerRespCh <- joined:
			case <-ta.ctx.Done():
				return text.String(), ta.ctx.Err()
			}

		case agent.AgentEventToolResult:
			if event.ToolIsError {
				m.logger.Log("channel: tool %s error: %s", event.ToolName, event.ToolResult)
			} else {
				m.logger.Log("channel: tool %s ok (%d bytes)", event.ToolName, len(event.ToolResult))
			}

		case agent.AgentEventAutoCompactStart:
			// Compact is in progress; nothing to do yet.

		case agent.AgentEventAutoCompactDone:
			if event.Result != nil && event.Result.Error != nil {
				// Compact failed — notify via progress if available.
				if sendProgress != nil {
					sendProgress(fmt.Sprintf("⚠️ 对话压缩失败: %v，将保留原始上下文继续对话。", event.Result.Error))
				}
			} else if event.CompactSummary != "" {
				// Compact succeeded — send summary as progress notification.
				if sendProgress != nil {
					sendProgress(fmt.Sprintf("🔍 对话已压缩（旧消息数: %d 条）\n%s", event.OldMsgCount, event.CompactSummary))
				}
			}

		case agent.AgentEventTurnComplete:
			if event.Result != nil {
				if event.Result.Response != "" {
					text.Reset()
					text.WriteString(event.Result.Response)
					// Append turn summary for non-trivial turns.
					if event.Result.IterationsUsed > 0 {
						if summary := agent.FormatTurnSummary(event.Result.IterationsUsed, event.Result.Duration); summary != "" {
							text.WriteString(summary)
						}
					}
				}
				if event.Result.Error != nil {
					lastErr = event.Result.Error
				}
			}

		case agent.AgentEventError:
			if event.Result != nil {
				// Preserve partial response if available (e.g., interrupted).
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
	// If we got an error but some text was produced, return the text.
	// The agent may have been interrupted mid-response or hit a budget limit
	// after outputting something useful.
	if result == "" && lastErr == nil {
		return "", nil
	}
	return result, nil
}

// convertQuestions converts agent-level tools.Question values to the
// channel-level channel.Question type so they can be passed through
// OutgoingMessage.AskUserQuestions without creating a dependency from
// pkg/channel on agent/tools.
func convertQuestions(qs []tools.Question) []channel.Question {
	cqs := make([]channel.Question, len(qs))
	for i, q := range qs {
		opts := make([]channel.QuestionOption, len(q.Options))
		for j, o := range q.Options {
			opts[j] = channel.QuestionOption{
				Label:       o.Label,
				Description: o.Description,
			}
		}
		cqs[i] = channel.Question{
			Question:    q.Question,
			Header:      q.Header,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		}
	}
	return cqs
}

// formatToolArgs extracts key parameters from a tool's JSON arguments string
// for display in a streaming card status line. It knows the argument schemas
// of common tools and formats them concisely.
// formatToolArgs extracts key parameters from a tool's JSON arguments string
// for display in a streaming card status line. Delegates to the shared
// tools.ToolArgsSummary and adds truncation + " — " prefix.
func formatToolArgs(toolName, argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	summary := tools.ToolArgsSummary(toolName, argsJSON)
	if summary == "" || summary == argsJSON {
		return ""
	}
	// Truncate long summaries for channel display.
	maxLen := 60
	switch toolName {
	case tools.ToolNameRead, tools.ToolNameEdit, tools.ToolNameWrite,
		tools.ToolNameGlob, tools.ToolNameWebFetch, tools.ToolNameMCPSearchTools:
		maxLen = 50
	}
	if len(summary) > maxLen {
		summary = summary[:maxLen] + "..."
	}
	return " — " + summary
}
