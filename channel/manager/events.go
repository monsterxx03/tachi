package manager

import (
	"context"
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
func (m *Manager) drainEvents(ctx context.Context, ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent, sendProgress func(string), ta *threadActivation, onTextDelta StreamingCallback) (string, error) {
	var text strings.Builder
	var lastErr error
	pushedTools := make(map[string]bool) // tool IDs already streamed to card

	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			text.WriteString(event.TextDelta)
			if onTextDelta != nil {
				if err := onTextDelta(StreamEvent{Type: StreamEventTextDelta, Text: event.TextDelta}); err != nil {
					m.logger.Warn(ctx, "channel: streaming text callback error", "error", err)
				}
			}

		case agent.AgentEventThinkingDelta:
			// Thinking is internal to the agent; we don't expose it to IM.
			// The content is still recorded in the session for context
			// preservation on resume.

		case agent.AgentEventToolCallStart:
			// Round boundary: the next LLM iteration appends fresh text after
			// this tool executes. Break the segment so a round whose text
			// doesn't end in a newline can't fuse onto the next round's
			// opening ("让我先搜索一下搜索结果是…").
			ensureSegmentBreak(&text)
			m.logger.Info(ctx, "channel: tool call start", "tool", event.ToolName)

		case agent.AgentEventToolCallArgs:
			m.logger.Info(ctx, "channel: tool call args", "tool", event.ToolName, "args", event.ToolArgs)
			if onTextDelta != nil && !pushedTools[event.ToolID] {
				pushedTools[event.ToolID] = true
				if err := onTextDelta(StreamEvent{
					Type:     StreamEventToolCall,
					ToolName: event.ToolName,
					ToolArgs: event.ToolArgs,
				}); err != nil {
					m.logger.Warn(ctx, "channel: streaming tool-call callback error", "error", err)
				}
			}

		case agent.AgentEventToolConfirmation:
			// Should not happen in PermissionModeSkip, but handle safely.
			m.logger.Warn(ctx, "channel: auto-approving unexpected confirmation", "tool", event.ToolName)
			aiAgent.ConfirmTool(agent.ConfirmAllowOnce)

		case agent.AgentEventAskUser:
			// Question round boundary: the LLM's follow-up text starts a new
			// segment after the user answers.
			ensureSegmentBreak(&text)
			if ta != nil && ta.askUserThreadID != "" {
				ta.mu.Lock()
				ta.askUserRespCh = make(chan tools.AskUserResult, 1)
				threadID := ta.askUserThreadID
				replyID := ta.askUserReplyID
				ta.mu.Unlock()

				m.logger.Info(ctx, "channel: AskUser — questions", "thread", threadID, "count", len(event.Questions))

				// Deliver structured questions to the channel.
				m.presentQuestionsToChannel(threadID, replyID, convertQuestions(event.Questions))

				// Block until the handler routes a user reply into askUserRespCh,
				// or the agent context is cancelled.
				select {
				case resp := <-ta.askUserRespCh:
					m.logger.Info(ctx, "channel: AskUser — received answer", "entries", len(resp.Answers))
					aiAgent.RespondToAskUser(resp.Answers, resp.Annotations)
				case <-ta.ctx.Done():
					m.logger.Info(ctx, "channel: AskUser — cancelled")
					aiAgent.RespondToAskUser(nil, nil)
				}

				ta.mu.Lock()
				ta.askUserRespCh = nil
				ta.mu.Unlock()
			} else {
				m.logger.Warn(ctx, "channel: auto-rejecting AskUser (non-interactive)")
				aiAgent.RespondToAskUser(nil, nil)
			}

		case agent.AgentEventSteerCheck:
			// Steer fires at a tool boundary; the next iteration's text opens
			// a fresh segment.
			ensureSegmentBreak(&text)
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
			// Drain ambient messages as formatted steer context, and record
			// them into ambient history — "seen means recorded" — so future
			// ambient turns see what was steered into this turn.
			if len(ta.ambientPending) > 0 {
				parts = append(parts, formatAmbientForSteer(ta.ambientPending))
				m.appendToAmbientHistory(ta, ta.ambientPending...)
				ta.ambientPending = nil
			}
			// Capture the steer channel under lock: a directed message may
			// preempt an ambient turn and swap ta.steerRespCh concurrently.
			steerCh := ta.steerRespCh
			joined := ""
			if len(parts) > 0 {
				joined = strings.Join(parts, "\n\n")
				m.logger.Info(ctx, "channel: steer inject", "content_len", len(joined))
			}
			ta.mu.Unlock()

			// Write to steerCh; agent is blocking on this read.
			// Select on both the turn ctx (ambient preemption) and the thread
			// ctx (/stop) to avoid leaking this goroutine on cancellation.
			select {
			case steerCh <- agent.SteerInput{Text: joined}:
			case <-ctx.Done():
				return text.String(), ctx.Err()
			case <-ta.ctx.Done():
				return text.String(), ta.ctx.Err()
			}

		case agent.AgentEventToolResult:
			if event.ToolIsError {
				m.logger.Error(ctx, "channel: tool error", fmt.Errorf("%s", event.ToolResult), "tool", event.ToolName)
			} else {
				m.logger.Info(ctx, "channel: tool ok", "tool", event.ToolName, "bytes", len(event.ToolResult))
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
				// text already accumulated all AgentEventTextDelta across
				// iterations — keep it. event.Result.Response only carries
				// the last iteration's text and would discard earlier output.
				if event.Result.IterationsUsed > 0 {
					if summary := agent.FormatTurnSummary(event.Result); summary != "" {
						text.WriteString(summary)
						// Streaming channels accumulate text via onTextDelta,
						// so the turn summary must also be streamed — otherwise
						// it would only exist in the final text and be dropped
						// by channels that build their reply from streamed
						// deltas (e.g. wave's streaming card uses sw.total).
						if onTextDelta != nil {
							if err := onTextDelta(StreamEvent{Type: StreamEventTextDelta, Text: summary}); err != nil {
								m.logger.Warn(ctx, "channel: streaming turn summary callback error", "error", err)
							}
						}
					}
				}
				if event.Result.Error != nil {
					lastErr = event.Result.Error
				}
			}

		case agent.AgentEventError:
			if event.Result != nil {
				// Preserve accumulated text across all iterations
				// instead of resetting to the last partial response.
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

// ensureSegmentBreak separates consecutive LLM iterations in the accumulated
// reply text. The agent streams one text segment per iteration — "text → tool
// call → text → …" — and without a break a round whose text doesn't end in a
// newline fuses onto the next round's opening. No-op when the buffer is empty
// (tool called before any text) or already ends in a newline.
func ensureSegmentBreak(text *strings.Builder) {
	if text.Len() == 0 {
		return
	}
	s := text.String()
	if s[len(s)-1] == '\n' {
		return
	}
	text.WriteByte('\n')
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
