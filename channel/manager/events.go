package manager

import (
	"strings"

	"github.com/monsterxx03/tachi/agent"
)

// drainEvents consumes all AgentEvents, returning the final assistant text or
// an error. Because we control the agent instance, we can respond to any
// confirmation/AskUser events inline — though with skip_edit_confirm=true
// and AskUser unregistered, neither should appear.
//
// verboseFn is called on each tool event to check whether verbose mode is
// currently active. Using a function (rather than a captured bool) allows
// /v toggles mid-turn to take effect immediately.
//
// When ta is non-nil, drainEvents also handles AgentEventSteerCheck for
// mid-turn user input injection (steer mechanism). When the agent reaches a
// tool-call boundary and requests steer input, pending messages are drained
// and delivered to the agent. When ta is nil (cron, tests), steer is skipped.
func (m *Manager) drainEvents(ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent, verboseFn func() bool, sendProgress func(string), ta *threadActivation) (string, error) {
	var text strings.Builder
	var lastErr error

	// verbose mode: pending tool call lines keyed by ToolID, flushed on result
	var pendingToolCalls map[string]string // ToolID → "🔧 ToolName(args)"

	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			text.WriteString(event.TextDelta)

		case agent.AgentEventThinkingDelta:
			// Thinking is internal to the agent; we don't expose it to IM.
			// The content is still recorded in the session for context
			// preservation on resume.

		case agent.AgentEventToolCallStart:
			m.logger.Log("channel: tool call start: %s", event.ToolName)

		case agent.AgentEventToolCallArgs:
			m.logger.Log("channel: tool call args for %s: %s", event.ToolName, event.ToolArgs)
			if verboseFn() {
				if pendingToolCalls == nil {
					pendingToolCalls = make(map[string]string)
				}
				pendingToolCalls[event.ToolID] = "🔧 " + summarizeToolCall(event.ToolName, event.ToolArgs)
			}

		case agent.AgentEventToolConfirmation:
			// Should not happen with skip_edit_confirm=true, but handle safely.
			m.logger.Log("channel: auto-approving unexpected confirmation: %s", event.ToolName)
			aiAgent.ConfirmTool(true)

		case agent.AgentEventAskUser:
			// Should not happen with AskUser unregistered, but handle safely.
			m.logger.Log("channel: auto-rejecting unexpected AskUser")
			aiAgent.RespondToAskUser(nil, nil)

		case agent.AgentEventSteerCheck:
			// Only process steer when ta is non-nil (agent turn path).
			if ta == nil {
				continue
			}
			// Agent reached a tool boundary — inject any pending steer messages.
			ta.mu.Lock()
			joined := ""
			if len(ta.pending) > 0 {
				joined = strings.Join(ta.pending, "\n\n")
				ta.pending = nil
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
				if verboseFn() {
					line := "  ❌ Error: " + truncateToolResult(event.ToolResult)
					if event.ToolDuration > 0 {
						line += " " + formatToolDuration(event.ToolDuration)
					}
					callLine, ok := pendingToolCalls[event.ToolID]
					if ok {
						if sendProgress != nil {
							sendProgress(callLine + "\n" + line)
						}
						delete(pendingToolCalls, event.ToolID)
					} else {
						if sendProgress != nil {
							sendProgress("🔧 " + event.ToolName + "\n" + line)
						}
					}
				}
			} else {
				m.logger.Log("channel: tool %s ok (%d bytes)", event.ToolName, len(event.ToolResult))
				if verboseFn() {
					line := "  ✅ " + summarizeToolResult(event.ToolName, event.ToolResult)
					if event.ToolDuration > 0 {
						line += " " + formatToolDuration(event.ToolDuration)
					}
					callLine, ok := pendingToolCalls[event.ToolID]
					if ok {
						if sendProgress != nil {
							sendProgress(callLine + "\n" + line)
						}
						delete(pendingToolCalls, event.ToolID)
					} else {
						if sendProgress != nil {
							sendProgress("🔧 " + event.ToolName + "\n" + line)
						}
					}
				}
			}

		case agent.AgentEventTurnComplete:
			if event.Result != nil {
				if event.Result.Response != "" {
					text.Reset()
					text.WriteString(event.Result.Response)
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
