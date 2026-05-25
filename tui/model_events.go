package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
)

func (m *Model) nextEvent() tea.Cmd {
	ch := m.eventCh
	gen := m.streamGen
	return func() tea.Msg {
		event, ok := <-ch
		if !ok {
			return streamDoneMsg{gen: gen}
		}
		return agentEventMsg{event: event, gen: gen}
	}
}

func (m *Model) handleAgentEvent(event agent.AgentEvent) tea.Cmd {
	switch event.Type {
	case agent.AgentEventTextDelta:
		m.setState(stateStreaming)
		m.chatview.AppendTextDelta(event.TextDelta)
		return m.nextEvent()

	case agent.AgentEventThinkingDelta:
		m.setState(stateStreaming)
		m.chatview.AppendThinkingDelta(event.ThinkingDelta)
		m.thinkingView.Append(event.ThinkingDelta)
		return m.nextEvent()

	case agent.AgentEventToolCallStart:
		m.setState(stateStreaming)
		m.chatview.AddToolCall(event.ToolName, event.ToolID)
		return m.nextEvent()

	case agent.AgentEventToolCallArgs:
		m.chatview.UpdateToolArgs(event.ToolID, event.ToolArgs)
		return m.nextEvent()

	case agent.AgentEventUsage:
		// Incremental usage update after each tool-call API round.
		m.accumulateUsage(event.Usage)
		return m.nextEvent()

	case agent.AgentEventToolConfirmation:
		m.logger.Log("TUI: Received AgentEventToolConfirmation, diff length: %d", len(event.ToolDiff))
		m.pendingConfirm = &pendingConfirm{
			toolName: event.ToolName,
			toolID:   event.ToolID,
			toolArgs: event.ToolArgs,
			diff:     event.ToolDiff,
		}
		m.setState(stateAwaitingConfirmation)
		// Show diff as a message in chatview
		m.chatview.AddMessage(chatMessage{
			Role:    "tool_confirmation",
			Content: "Edit File Confirmation\n" + event.ToolDiff,
		})
		if len(event.ToolDiff) > 100 {
			runes := []rune(event.ToolDiff)
			preview := string(runes[:100])
			m.logger.Log("TUI: diff preview: %s...", preview)
		} else {
			m.logger.Log("TUI: diff: %s", event.ToolDiff)
		}
		return nil

	case agent.AgentEventAskUser:
		m.logger.Log("TUI: Received AgentEventAskUser, %d questions", len(event.Questions))
		m.askUserView = NewAskUserView(event.Questions, m.width)
		m.setState(stateAskUserQuestion)
		m.layout()
		return nil

	case agent.AgentEventToolResult:
		m.chatview.UpdateToolResult(event.ToolID, event.ToolResult, event.ToolIsError, event.ToolDuration)
		return m.nextEvent()

	case agent.AgentEventSubagentStart:
		// Sub-agent started — mark the tool call as having a subagent.
		m.chatview.MarkSubagent(event.ToolID)
		return m.nextEvent()

	case agent.AgentEventSubagentDone:
		// Sub-agent completed — refresh cost to include subagent usage.
		m.refreshSessionCost()
		return m.nextEvent()

	case agent.AgentEventSteerCheck:
		if len(m.pendingQueue) > 0 {
			combined := strings.Join(m.pendingQueue, "\n\n")
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			// Expand @-file references before sending to the LLM.
			expandResult := ExpandAtReferences(combined)
			// Add as a normal user message in chatview for visual continuity.
			m.chatview.AddMessage(chatMessage{Role: "user", Content: combined})
			// Attach images from steer expansion (if any).
			if len(expandResult.Images) > 0 {
				m.agent.SetPendingImages(expandResult.Images)
			}
			// Send expanded steer text to agent (non-blocking with select).
			select {
			case m.steerRespCh <- expandResult.Text:
			default:
			}
		} else {
			select {
			case m.steerRespCh <- "":
			default:
			}
		}
		return m.nextEvent()

	case agent.AgentEventTurnComplete:
		m.steerRespCh = nil
		if event.Messages != nil {
			m.history = event.Messages
		}

		// Compact handling — before one-off restore
		if m.isCompacting {
			m.isCompacting = false
			if event.Result != nil && event.Result.Error != nil {
				m.rollbackCompact("压缩失败: " + event.Result.Error.Error())
				return nil
			}

			summary := event.Result.Response
			sm := m.agent.SessionManager()

			// Save old ThreadID before FinalizeCompact (sm.New changes current)
			oldThreadID := ""
			if oldSess := sm.Current(); oldSess != nil {
				oldThreadID = oldSess.ThreadID
			}

			oldMsgCount := len(m.savedHistory)
			newHistory, err := agent.FinalizeCompact(sm, m.systemPrompt, summary)
			if err != nil {
				m.rollbackCompact("压缩失败: " + err.Error())
				return nil
			}

			// Migrate ThreadID to new session
			if oldThreadID != "" {
				sm.SetThreadID(oldThreadID)
			}

			m.history = newHistory
			m.savedHistory = nil

			// Restore tools (cleared before compact)
			if m.savedTools != nil {
				m.agent.RestoreToolRegistry(m.savedTools)
				m.savedTools = nil
			}

			// Update usage (compact LLM call's tokens count toward the session)
			m.accumulateUsage(event.Usage)
			if event.Usage != nil {
				m.totalUsage.LastInputTokens = event.Usage.InputTokens
			}

			// Rebuild chatview for the new session
			m.chatview.Clear()
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: formatCompactSummary(summary, oldMsgCount),
			})
			m.chatview.FinishStreaming()
			m.syncSessionInfo()
			m.setState(stateIdle)
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			m.cancelFunc = nil
			m.eventCh = nil
			return nil
		}

		isOneOff := m.savedHistory != nil
		if isOneOff {
			m.history = m.savedHistory
			m.savedHistory = nil
		}
		if event.Usage != nil {
			m.accumulateUsage(event.Usage)
			// For one-off commands (e.g. /commit), only accumulate tokens and cost;
			// don't overwrite the context-usage numerator (LastInputTokens) so the
			// statusbar continues showing the main conversation's context fraction.
			if !isOneOff {
				m.totalUsage.LastInputTokens = event.Usage.InputTokens
			}
		}
		if m.savedTools != nil {
			m.agent.RestoreToolRegistry(m.savedTools)
			m.savedTools = nil
		}
		m.chatview.FinishStreaming()
		m.syncSessionInfo()

		// Send terminal notification when a turn completes (not for one-offs like /commit).
		if m.notifyOnComplete && !isOneOff {
			notifyTerminal("tachi", "Reply ready")
		}

		// Drain pending queue if not in a one-off context (e.g. /commit, /init).
		if len(m.pendingQueue) > 0 && !isOneOff {
			combined := strings.Join(m.pendingQueue, "\n\n")
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			m.cancelFunc = nil
			m.eventCh = nil
			return m.sendMessage(combined)
		}

		// Discard pending queue for one-off contexts (savedHistory was set).
		if isOneOff {
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
		}

		m.setState(stateIdle)
		m.cancelFunc = nil
		m.eventCh = nil
		return nil

	case agent.AgentEventSessionTitle:
		// Title generated early: refresh statusbar immediately without
		// waiting for TurnComplete.
		m.syncSessionInfo()
		return m.nextEvent()

	case agent.AgentEventError:
		m.steerRespCh = nil
		if event.Messages != nil {
			m.history = event.Messages
		}
		if m.savedHistory != nil {
			m.history = m.savedHistory
			m.savedHistory = nil
		}
		if m.savedTools != nil {
			m.agent.RestoreToolRegistry(m.savedTools)
			m.savedTools = nil
		}
		// Clear pending queue on error (Ctrl+C clears it earlier in handleCtrlC,
		// this handles non-interrupt errors like API failures).
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		if event.Result != nil && event.Result.ExitReason == "interrupted" {
			m.chatview.FinishStreaming()
		} else {
			errMsg := "Unknown error"
			if event.Result != nil && event.Result.Error != nil {
				errMsg = event.Result.Error.Error()
			}
			m.chatview.AddMessage(chatMessage{Role: "error", Content: errMsg})
			// Notify on error (but not for user-initiated interruptions).
			if m.notifyOnComplete {
				notifyTerminal("tachi", "Error — "+errMsg)
			}
		}
		m.setState(stateIdle)
		m.cancelFunc = nil
		m.eventCh = nil
		return nil
	}

	return m.nextEvent()
}