package manager

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// --- Thread activation management ---

// activateThread returns the threadActivation for threadID, creating one
// if it doesn't exist. The caller MUST check ta.steerRespCh to determine
// whether the thread is already active (steer case) or new (start case).
func (m *Manager) activateThread(threadID string, parentCtx context.Context) *threadActivation {
	m.threadActMu.Lock()
	defer m.threadActMu.Unlock()

	if m.threadActivations == nil {
		m.threadActivations = make(map[string]*threadActivation)
	}

	ta, ok := m.threadActivations[threadID]
	if !ok {
		ctx, cancel := context.WithCancel(parentCtx)
		ta = &threadActivation{ctx: ctx, cancel: cancel}
		m.threadActivations[threadID] = ta
	}
	return ta
}

// deactivateThread removes the thread activation for threadID,
// but only if the stored activation still matches the one held by
// the caller (pointer equality). This prevents a stale handler
// goroutine from deleting a new activation created by /new or /stop.
func (m *Manager) deactivateThread(threadID string, ta *threadActivation) {
	m.threadActMu.Lock()
	defer m.threadActMu.Unlock()
	if cur, ok := m.threadActivations[threadID]; ok && cur == ta {
		delete(m.threadActivations, threadID)
	}
}

// cancelThreadTurn cancels the agent context for the given thread
// and removes the activation from the map. Safe to call when no
// turn is active.
// Called by /stop and /new to terminate a running LLM turn.
func (m *Manager) cancelThreadTurn(threadID string) {
	m.threadActMu.Lock()
	ta, ok := m.threadActivations[threadID]
	if ok && ta.cancel != nil {
		ta.cancelled = true
		ta.cancel()
	}
	delete(m.threadActivations, threadID)
	m.threadActMu.Unlock()
}

// --- Handler build ---

// buildHandler returns a MessageHandler. Each call processes one incoming
// message. The first message for a thread starts a blocking agent turn;
// subsequent messages while an agent turn is active are injected via steer
// and return immediately with Steered=true.
func (m *Manager) buildHandler() channel.MessageHandler {
	return func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		m.logger.Log("channel: recv thread=%s id=%s len=%d",
			msg.ThreadID, msg.MessageID, len(msg.Content))

		// /compact goes through the agent turn (with session context) rather
		// than the synchronous slash-command path, so the LLM can summarize
		// using its existing context window without re-sending all history.
		isCompactCmd := strings.HasPrefix(msg.Content, "/compact")

		// Skill activation also goes through the agent turn so the LLM
		// can read and apply the skill instructions as part of its context.
		isSkillActivation := false
		var skillActivationMsg string
		if !isCompactCmd && strings.HasPrefix(msg.Content, "/") {
			if skillName, extraArgs, ok := m.isSkillActivation(msg.Content); ok {
				activationMsg, errMsg, err := m.prepareSkillActivation(skillName, extraArgs)
				if err != nil {
					return channel.HandlerResult{
						Reply: channel.OutgoingMessage{
							ThreadID: msg.ThreadID,
							Content:  fmt.Sprintf("❌ %s", errMsg),
							ReplyTo:  msg.MessageID,
						},
						Err: err,
					}
				}
				isSkillActivation = true
				skillActivationMsg = activationMsg
			}
		}

		// Other slash commands are handled synchronously (no LLM invocation),
		// EXCEPT compact (agent turn) and skill activation (agent turn).
		if !isCompactCmd && !isSkillActivation && strings.HasPrefix(msg.Content, "/") {
			// /transcript returns an attachment (HTML file), not plain text,
			// so it's handled separately from the general slash command path.
			if strings.HasPrefix(msg.Content, "/transcript") {
				return m.handleTranscriptCommand(msg)
			}

			result, err := m.handleSlashCommand(msg)
			if err != nil {
				return channel.HandlerResult{
					Reply: channel.OutgoingMessage{
						ThreadID: msg.ThreadID,
						Content:  fmt.Sprintf("❌ %v", err),
						ReplyTo:  msg.MessageID,
					},
					Err: err,
				}
			}
			return channel.HandlerResult{
				Reply: channel.OutgoingMessage{
					ThreadID: msg.ThreadID,
					Content:  result,
					ReplyTo:  msg.MessageID,
				},
			}
		}

		prov, resolved := m.getProvider()
		if prov == nil || resolved == nil {
			return channel.HandlerResult{
				Reply: channel.OutgoingMessage{
					ThreadID: msg.ThreadID,
					Content:  "❌ channel manager not initialized; call Start() first",
					ReplyTo:  msg.MessageID,
				},
				Err: fmt.Errorf("channel manager not initialized"),
			}
		}

		// sendProgress pushes intermediate tool results in verbose mode.
		sendProgress := func(text string) {
			m.sendToThread(ctx, msg.ThreadID, text, msg.MessageID)
		}

		// Check if an agent is already running for this thread.
		ta := m.activateThread(msg.ThreadID, ctx)
		ta.isCompact = isCompactCmd

		ta.mu.Lock()
		if ta.steerRespCh != nil {
			// /compact and /skill activation require a fresh agent turn with
			// session context — they cannot be injected as steer text because
			// the LLM would see the raw command string instead of the
			// transformed instruction. Cancel the running turn and restart.
			if isCompactCmd || isSkillActivation {
				ta.mu.Unlock()
				m.cancelThreadTurn(msg.ThreadID)
				m.logger.Log("channel: cancelled running turn for %s, restarting with compact/skill", msg.ThreadID)
				// Activate a fresh thread for the new turn.
				ta = m.activateThread(msg.ThreadID, ctx)
				ta.isCompact = isCompactCmd
			} else {
				// Agent already running — queue this message as steer input.
				ta.pending = append(ta.pending, msg.Content)
				pendingLen := len(ta.pending)
				ta.mu.Unlock()
				m.logger.Log("channel: steer queued thread=%s pending=%d", msg.ThreadID, pendingLen)
				return channel.HandlerResult{Steered: true}
			}
		} else {
			ta.mu.Unlock()
		}

		// First message for this thread (or fresh activation after cancel) —
		// start the agent.
		ta.mu.Lock()
		ta.steerRespCh = make(chan string)
		ta.resultCh = make(chan handlerResult, 1)
		ta.mu.Unlock()

		// Transform /compact to compact instruction for the LLM.
		// The LLM will summarize based on its existing session context
		// without re-sending all history as text.
		if isCompactCmd {
			msg.Content = agent.BuildCompactInstruction()
		}

		// Transform skill activation to the skill's instruction message.
		// The LLM will read the skill body and apply its instructions.
		if isSkillActivation {
			msg.Content = skillActivationMsg
		}

		// Run agent in a goroutine; handler blocks on the result channel.
		go m.runAgentTurn(ta.ctx, msg, sendProgress, ta)

		select {
		case result := <-ta.resultCh:
			m.deactivateThread(msg.ThreadID, ta)
			if ta.cancelled {
				// Turn was cancelled by /stop or /new.
				return channel.HandlerResult{Steered: true}
			}
			if result.err != nil {
				return channel.HandlerResult{
					Reply: channel.OutgoingMessage{
						ThreadID:    msg.ThreadID,
						Content:     fmt.Sprintf("❌ %v", result.err),
						ReplyTo:     msg.MessageID,
						Attachments: result.attachments,
					},
					Err: result.err,
				}
			}

			// If this was a /compact turn, finalize the compact by creating
			// a new session with the summary and migrating the ThreadID.
			if isCompactCmd {
				reply, err := m.finalizeCompactResult(msg.ThreadID, result.text)
				if err != nil {
					m.logger.Log("channel: finalizeCompactResult thread=%s err=%v", msg.ThreadID, err)
					return channel.HandlerResult{
						Reply: channel.OutgoingMessage{
							ThreadID: msg.ThreadID,
							Content:  fmt.Sprintf("❌ 压缩失败: %v", err),
							ReplyTo:  msg.MessageID,
						},
						Err: err,
					}
				}
				return channel.HandlerResult{
					Reply: channel.OutgoingMessage{
						ThreadID: msg.ThreadID,
						Content:  reply,
						ReplyTo:  msg.MessageID,
					},
				}
			}

			return channel.HandlerResult{
				Reply: channel.OutgoingMessage{
					ThreadID:    msg.ThreadID,
					Content:     result.text,
					ReplyTo:     msg.MessageID,
					Attachments: result.attachments,
				},
			}
		case <-ta.ctx.Done():
			m.deactivateThread(msg.ThreadID, ta)
			if ta.cancelled {
				// Turn was cancelled by /stop or /new — suppress
				// the "request cancelled" error reply.
				return channel.HandlerResult{Steered: true}
			}
			return channel.HandlerResult{
				Reply: channel.OutgoingMessage{
					ThreadID: msg.ThreadID,
					Content:  "❌ request cancelled",
					ReplyTo:  msg.MessageID,
				},
				Err: ta.ctx.Err(),
			}
		}
	}
}

// --- Agent turn ---

// runAgentTurn loads the per-thread cached AIAgent, attaches a session and
// steer channel, and runs a single conversation turn. Per-turn ephemeral
// tools (CronTool, SendFileTool) are scoped via SaveToolRegistry/Restore so
// they don't leak into the next turn.
func (m *Manager) runAgentTurn(ctx context.Context, msg channel.IncomingMessage, sendProgress func(string), ta *threadActivation) {
	defer func() {
		// Unblock the handler on panic.
		if r := recover(); r != nil {
			m.logger.Log("channel: agent panic for thread=%s: %v", msg.ThreadID, r)
			select {
			case ta.resultCh <- handlerResult{err: fmt.Errorf("agent panic: %v", r)}:
			default:
			}
		}
	}()

	// /compact runs against an isolated, throwaway agent so that
	// ClearToolRegistry() doesn't pollute the cached agent's tool set.
	if ta.isCompact {
		m.runCompactTurn(ctx, msg, sendProgress, ta)
		return
	}

	_, resolved := m.getProvider()

	ca, err := m.acquireAgent(ctx, msg.ThreadID)
	if err != nil {
		ta.resultCh <- handlerResult{err: fmt.Errorf("acquire agent: %w", err)}
		return
	}
	defer m.releaseAgent(ca)

	aiAgent := ca.agent

	// Snapshot the registry so we can cleanly remove per-turn ephemeral
	// tools (CronTool, SendFileTool) at the end of this turn.
	snap := aiAgent.SaveToolRegistry()
	defer aiAgent.RestoreToolRegistry(snap)

	// Register CronTool if scheduler is available.
	if m.scheduler != nil {
		aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string {
			return msg.ThreadID
		}))
	}

	// Per-thread session.
	sm, priorHistory, err := m.loadThreadSession(msg.ThreadID)
	if err != nil {
		m.logger.Log("channel: session setup for thread %s: %v", msg.ThreadID, err)
		sm = m.newSessionManager()
		priorHistory = nil
	}

	// Ensure a session exists for recording.
	if sm != nil && !sm.HasCurrent() {
		wd, _ := os.Getwd()
		if _, err := sm.New(resolved.Provider.Type, resolved.Provider.Model, wd); err != nil {
			m.logger.Log("channel: create fallback session: %v", err)
		} else {
			sm.SetThreadID(msg.ThreadID)
		}
	}

	if sm != nil {
		aiAgent.SetSessionManager(sm)
	}

	// Wire up steer channel — this enables mid-turn user input injection.
	aiAgent.SetSteerChannel(ta.steerRespCh)

	// Build the user message text with attachment content prepended.
	userContent, userImages := buildUserMessageWithAttachments(msg)

	// Attach images (if any) for multi-modal LLM input (vision).
	if len(userImages) > 0 {
		aiAgent.SetPendingImages(userImages)
	}

	// --- SendFile tool for file delivery via channel ---
	// The tool is available in channel mode so the LLM can send files
	// to the user (e.g. generated reports, screenshots, documents).
	// MUST be re-registered each turn because the callback closure captures
	// the per-turn `pendingAttachments` slice.
	var attachmentMu sync.Mutex
	var pendingAttachments []channel.OutgoingAttachment

	sendFileTool := tools.NewSendFileTool()
	sendFileTool.SetCallback(func(name, mimeType, localPath string) {
		attachmentMu.Lock()
		pendingAttachments = append(pendingAttachments, channel.OutgoingAttachment{
			Type:      channel.AttachmentTypeFile,
			FileName:  name,
			MimeType:  mimeType,
			LocalPath: localPath,
		})
		attachmentMu.Unlock()
	})
	aiAgent.RegisterTool(sendFileTool)

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, userContent, m.systemPrompt, llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})

	// Use a closure so /v toggles mid-turn are visible immediately,
	// rather than using the captured bool which is a one-time snapshot.
	isVerbose := func() bool {
		m.verboseMu.RLock()
		defer m.verboseMu.RUnlock()
		return m.verboseState != nil && m.verboseState[msg.ThreadID]
	}

	text, err := m.drainEvents(eventCh, aiAgent, isVerbose, sendProgress, ta)

	// Collect any pending file attachments from the SendFile tool.
	attachmentMu.Lock()
	attachments := make([]channel.OutgoingAttachment, len(pendingAttachments))
	copy(attachments, pendingAttachments)
	attachmentMu.Unlock()

	ta.resultCh <- handlerResult{text: text, err: err, attachments: attachments}
}

// runCompactTurn runs a /compact summarization against a throwaway AIAgent
// so the registry-clearing semantics (ClearToolRegistry) don't pollute the
// cached agent's tool set. Cached agents survive across turns and intentionally
// retain their tools, skills, and discovered MCP set; /compact's "no tools"
// stance is incompatible with that lifecycle, so we run it in isolation.
//
// The compact agent still uses the shared MCP state (cheap, harmless — it
// won't touch the deferred pool because all tools are cleared anyway), the
// shared ProcessManager, and the same per-thread session.
func (m *Manager) runCompactTurn(ctx context.Context, msg channel.IncomingMessage, sendProgress func(string), ta *threadActivation) {
	_, resolved := m.getProvider()
	if resolved == nil {
		ta.resultCh <- handlerResult{err: fmt.Errorf("channel: provider not initialized")}
		return
	}

	aiAgent, err := m.buildAgent(ctx, msg.ThreadID)
	if err != nil {
		ta.resultCh <- handlerResult{err: fmt.Errorf("compact: build agent: %w", err)}
		return
	}
	// Throwaway — release resources when this turn ends.
	defer aiAgent.Close()

	// /compact: no tool calls — only summarize from session context.
	aiAgent.ClearToolRegistry()

	sm, priorHistory, err := m.loadThreadSession(msg.ThreadID)
	if err != nil {
		m.logger.Log("channel: compact session setup for thread %s: %v", msg.ThreadID, err)
		sm = m.newSessionManager()
		priorHistory = nil
	}
	if sm != nil && !sm.HasCurrent() {
		wd, _ := os.Getwd()
		if _, err := sm.New(resolved.Provider.Type, resolved.Provider.Model, wd); err != nil {
			m.logger.Log("channel: compact create session: %v", err)
		} else {
			sm.SetThreadID(msg.ThreadID)
		}
	}
	if sm != nil {
		aiAgent.SetSessionManager(sm)
	}

	aiAgent.SetSteerChannel(ta.steerRespCh)

	userContent, userImages := buildUserMessageWithAttachments(msg)
	if len(userImages) > 0 {
		aiAgent.SetPendingImages(userImages)
	}

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, userContent, m.systemPrompt, llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})

	isVerbose := func() bool {
		m.verboseMu.RLock()
		defer m.verboseMu.RUnlock()
		return m.verboseState != nil && m.verboseState[msg.ThreadID]
	}

	text, err := m.drainEvents(eventCh, aiAgent, isVerbose, sendProgress, ta)
	ta.resultCh <- handlerResult{text: text, err: err}
}
