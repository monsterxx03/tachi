package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
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
		// Reset thinking view: thinking for this turn is complete when
		// tool calls start. The next turn (after tool results) will
		// accumulate fresh thinking.
		m.thinkingView.Reset()
		return m.nextEvent()

	case agent.AgentEventToolCallArgs:
		m.chatview.UpdateToolArgs(event.ToolID, event.ToolArgs)
		return m.nextEvent()

	case agent.AgentEventUsage:
		// Incremental usage update after each tool-call API round.
		m.accumulateUsage(event.Usage)
		// Update LastInputTokens from the local estimate (per-call context size)
		// instead of accumulating, so the statusbar shows the true per-call
		// context fraction rather than the monotonically growing total.
		if est := m.agent.LastInputEstimate(); est > 0 {
			m.totalUsage.LastInputTokens = est
		}
		return m.nextEvent()

	case agent.AgentEventToolConfirmation:
		m.logger.Info(context.Background(), "TUI: Received AgentEventToolConfirmation", "diffLen", len(event.ToolDiff))
		m.pendingConfirm = &pendingConfirm{
			toolName: event.ToolName,
			toolID:   event.ToolID,
			toolArgs: event.ToolArgs,
			diff:     event.ToolDiff,
		}
		m.setState(stateAwaitingConfirmation)
		// Show diff as a message in chatview
		confirmTitle := "Edit File Confirmation"
		if event.ToolName == tools.ToolNameBash {
			confirmTitle = "Bash Command Confirmation"
		}
		m.chatview.AddMessage(chatMessage{
			Role:    "tool_confirmation",
			Content: confirmTitle + "\n" + event.ToolDiff,
		})
		if len(event.ToolDiff) > 100 {
			runes := []rune(event.ToolDiff)
			preview := string(runes[:100])
			m.logger.Info(context.Background(), "TUI: diff preview", "text", preview+"...")
		} else {
			m.logger.Info(context.Background(), "TUI: diff", "text", event.ToolDiff)
		}
		return nil

	case agent.AgentEventAskUser:
		m.logger.Info(context.Background(), "TUI: Received AgentEventAskUser", "questions", len(event.Questions))
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
		// Sub-agent completed — update tool call display with stats and refresh cost.
		if event.IterCount > 0 {
			for i := range m.chatview.currentTools {
				if m.chatview.currentTools[i].ID == event.ToolID {
					m.chatview.currentTools[i].IterCount = event.IterCount
					break
				}
			}
		}
		m.refreshSessionCost()
		return m.nextEvent()

	case agent.AgentEventSubagentToolCall:
		// Real-time subagent internal tool call — update tool call counters.
		m.chatview.UpdateSubagentToolCall(event.ToolID, event.SubagentToolName)
		return m.nextEvent()

	case agent.AgentEventSteerCheck:
		if len(m.pendingQueue) > 0 {
			combined := strings.Join(m.pendingQueue, "\n\n")
			m.pendingQueue = nil
			m.chatview.RemovePendingItems()
			m.statusbar.SetPendingCount(0)
			// Expand @-file references before sending to the LLM.
			expandResult := m.ExpandAtReferences(combined)
			// Add as a normal user message in chatview for visual continuity.
			m.chatview.AddMessage(chatMessage{Role: "user", Content: combined})
			// Send expanded steer text to agent (non-blocking with select).
			select {
			case m.steerCh <- agent.SteerInput{Text: expandResult.Text, Images: expandResult.Images}:
			default:
			}
		} else {
			select {
			case m.steerCh <- agent.SteerInput{Text: ""}:
			default:
			}
		}
		return m.nextEvent()

	case agent.AgentEventTurnComplete:
		// ---- review dispatch (BEFORE anything else: intermediate rounds
		//      must never touch m.history, and savedHistory must stay non-nil
		//      until the final round's TurnComplete restores it in the normal
		//      one-off branch) ----
		if m.reviewOrch != nil {
			done, report := m.reviewOrch.Complete()

			if !done {
				// ===== intermediate round: seal bubble, accumulate usage,
				// close fork, chain the next round =====
				m.chatview.FinishStreaming()
				if event.Usage != nil {
					m.accumulateUsage(event.Usage) // every round's cost shows in /usage
				}
				m.showReviewReportHint(report)
				if m.forkedAgent != nil {
					m.forkedAgent.Close()
					m.forkedAgent = nil
				}
				return m.startReviewRound() // no history restore, no normal branch
			}

			// ===== final round: clear state, fall through to normal one-off handling =====
			// Keep m.forkedAgent alive — the normal branch reads the one-off
			// transcript path from it before closing (closing here would make
			// the normal branch fall back to m.agent's stale path and the
			// trailing "📄 旁路记录" would point at the wrong file).
			// usage accumulation, FinishStreaming and savedHistory restore are
			// all done by the normal branch.
			m.showReviewReportHint(report)
			total := m.reviewOrch.TotalRounds()
			m.isReviewing = false
			m.reviewOrch = nil
			m.statusbar.ClearReviewBadge()
			if total > 1 {
				m.chatview.AppendTextDelta(fmt.Sprintf(
					"\n✅ 对抗式审查完成 (%d/%d rounds)\n", total, total))
				// Multi-round runs take a long time — notify proactively (the
				// normal branch does not notify for one-offs).
				if m.notifyOnComplete && !herdrNotifications(m.cfg) {
					notifyTerminal("tachi", "对抗式审查完成")
				}
			}
			// fall through
		}

		if event.Messages != nil {
			m.history = event.Messages
		}

		// Compact handling — before one-off restore
		if m.isCompacting {
			m.isCompacting = false

			if event.Result != nil && event.Result.Error != nil {
				if m.compactForSwitch {
					m.abortCompactForSwitch("压缩失败，未能切换到目标模型: " + event.Result.Error.Error())
					return nil
				}
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
			newHistory, err := m.agent.CompleteCompact(sm, m.systemPrompt, summary)
			if err != nil {
				if m.compactForSwitch {
					m.abortCompactForSwitch("压缩失败，未能切换到目标模型: " + err.Error())
					return nil
				}
				m.rollbackCompact("压缩失败: " + err.Error())
				return nil
			}

			// Migrate ThreadID to new session
			if oldThreadID != "" {
				sm.SetThreadID(oldThreadID)
			}

			m.history = newHistory
			m.savedHistory = nil

			// Update usage (compact LLM call's tokens count toward the session)
			m.accumulateUsage(event.Usage)
			if est := m.agent.LastInputEstimate(); est > 0 {
				m.totalUsage.LastInputTokens = est
			}

			if m.compactForSwitch {
				// Compact-on-switch: show summary, apply pending switch, done.
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: formatCompactSummary(summary, oldMsgCount),
				})
				m.applyPendingSwitch()
				m.pendingQueue = nil
				m.chatview.RemovePendingItems()
				m.statusbar.SetPendingCount(0)
				return nil
			}

			// Normal /compact: rebuild chatview for the new session.
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
		// Capture the one-off transcript path (/commit on m.agent, /review on
		// the fork) BEFORE the fork is closed below.
		oneoffPath := ""
		if isOneOff {
			if m.forkedAgent != nil {
				oneoffPath = m.forkedAgent.Agent().LastOneoffTranscriptPath()
			} else {
				oneoffPath = m.agent.LastOneoffTranscriptPath()
			}
		}
		if event.Usage != nil {
			m.accumulateUsage(event.Usage)
			// For one-off commands (e.g. /commit), only accumulate tokens and cost;
			// don't overwrite the context-usage numerator (LastInputTokens) so the
			// statusbar continues showing the main conversation's context fraction.
			if !isOneOff {
				if est := m.agent.LastInputEstimate(); est > 0 {
					m.totalUsage.LastInputTokens = est
				}
			}
		}
		// Clean up forked agent (e.g. /review) — must happen after the one-off
		// event stream has been fully consumed and the agent is idle.
		if m.forkedAgent != nil {
			m.forkedAgent.Close()
			m.forkedAgent = nil
		}

		// Append turn summary (iterations + duration) to the assistant's response
		// before finalizing the stream display. Skipped for one-off commands
		// (e.g. /commit, /init) and error-only results with no iterations.
		if event.Result != nil && event.Result.IterationsUsed > 0 && !isOneOff {
			if summary := agent.FormatTurnSummary(event.Result.IterationsUsed, event.Result.Duration, event.Result.TraceID); summary != "" {
				m.chatview.AppendTextDelta(summary)
			}
		}

		m.chatview.FinishStreaming()
		m.syncSessionInfo()

		// Point the user at the full side-channel execution record.
		if oneoffPath != "" {
			m.chatview.AddMessage(chatMessage{
				Role:    "oneoff_note",
				Content: "📄 旁路记录: " + oneoffPath,
			})
		}

		// Send terminal notification when a turn completes (not for one-offs like /commit).
		// Skip when Herdr integration is active — Herdr provides its own visual indicators.
		if m.notifyOnComplete && !isOneOff && !herdrNotifications(m.cfg) {
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

	case agent.AgentEventAutoCompactStart:
		m.isCompacting = true
		m.statusbar.SetCompacting(true)
		return m.nextEvent()

	case agent.AgentEventAutoCompactDone:
		m.isCompacting = false
		m.statusbar.SetCompacting(false)
		if event.Result != nil && event.Result.Error != nil {
			// Compact failed — notify the user but continue.
			m.chatview.AddMessage(chatMessage{
				Role:    "compact_done",
				Content: fmt.Sprintf("⚠️ 对话压缩失败: %v，将保留原始上下文继续对话。", event.Result.Error),
			})
		} else {
			// Compact succeeded — show the summary inline and continue.
			m.chatview.SetStreaming(false)
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: formatCompactSummary(event.CompactSummary, event.OldMsgCount),
			})
			m.chatview.FinishStreaming()
		}
		// Sync session info — compact may have created a new session
		// (different ID, title inherited from old).
		m.syncSessionInfo()
		return m.nextEvent()

	case agent.AgentEventError:

		// Review interruption cleanup — must run before the existing logic
		// below (it knows nothing about reviewOrch; without this, isReviewing
		// would stay true forever, blocking user input, and reviewOrch would
		// leak into the next /review).
		wasReviewing := m.reviewOrch != nil
		if wasReviewing {
			m.reviewOrch = nil
			m.isReviewing = false
			m.statusbar.ClearReviewBadge()
		}

		// Clean up pending model switch BEFORE restoring savedHistory.
		// During a compact-for-switch, savedHistory holds the pre-compact
		// history — restoring it would incorrectly roll back the conversation.
		if m.compactForSwitch {
			m.compactForSwitch = false
			m.pendingSwitchProvider = nil
			m.savedHistory = nil
		}

		if event.Messages != nil {
			m.history = event.Messages
		}
		if m.savedHistory != nil {
			m.history = m.savedHistory
			m.savedHistory = nil
		}
		// Clean up forked agent on error (e.g. /review cancelled or failed).
		if m.forkedAgent != nil {
			m.forkedAgent.Close()
			m.forkedAgent = nil
		}
		// Clear pending queue on error (Ctrl+C clears it earlier in handleCtrlC,
		// this handles non-interrupt errors like API failures).
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		if event.Result != nil && event.Result.ExitReason == agent.ExitReasonInterrupted {
			m.chatview.FinishStreaming()
			if wasReviewing {
				m.chatview.AddMessage(chatMessage{Role: "assistant", Content: "⏹️ 对抗式审查已取消"})
			}
		} else {
			errMsg := "Unknown error"
			if event.Result != nil && event.Result.Error != nil {
				errMsg = event.Result.Error.Error()
			}
			m.chatview.AddMessage(chatMessage{Role: "error", Content: errMsg})
			// Notify on error (but not for user-initiated interruptions).
			// Skip when Herdr integration is active — Herdr provides its own visual indicators.
			if m.notifyOnComplete && !herdrNotifications(m.cfg) {
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
