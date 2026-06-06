package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
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
			return m.handleSlashCommand(msg)
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
			msg.Content = cmds.BuildCompactInstruction(m.cfg.Language)
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
				// Evict the cached agent so the next turn reloads history from
				// the newly-created compacted session (via disk fallback).
				m.evictAgent(msg.ThreadID)
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

// runAgentTurn executes one user-driven turn for a thread. It supports two
// modes, dispatched on ta.isCompact:
//
//   - Normal turn (ta.isCompact == false): grabs the per-thread cached
//     AIAgent, scopes ephemeral tools (CronTool, SendFileTool) via
//     SaveToolRegistry/Restore, and collects file attachments.
//   - Compact turn (ta.isCompact == true): builds a one-off throwaway agent
//     with ClearToolRegistry so /compact summarizes from session context
//     only, without polluting the cached agent's tool set.
//
// The two modes share session loading, steer wiring, image attachment, and
// drainEvents — only the agent-acquisition and tool-registration steps
// differ, so they're isolated in acquireForTurn.
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

	_, resolved := m.getProvider()
	if resolved == nil {
		ta.resultCh <- handlerResult{err: fmt.Errorf("channel: provider not initialized")}
		return
	}

	// Mode-specific prologue: cached agent vs throwaway compact agent.
	aiAgent, ca, sink, cleanup, err := m.acquireForTurn(ctx, msg.ThreadID, ta.isCompact)
	if err != nil {
		ta.resultCh <- handlerResult{err: err}
		return
	}
	defer cleanup()

	// Per-thread session — always needed for session recording (JSONL).
	// For compact turns, we also need the history from disk since there's
	// no in-memory cache (throwaway agent).
	sm, diskHistory := m.prepareThreadSession(msg.ThreadID, resolved)
	if sm != nil {
		aiAgent.SetSessionManager(sm)
		// Notify memory backend when a new session was created
		aiAgent.StartSessionMemory()
	}

	// Use the in-memory cached history when available (normal turns).
	// This keeps the message prefix stable across turns, maximising prompt
	// cache hit rates. Fall back to disk-loaded history on first turn or
	// after agent eviction (ca.history == nil).
	priorHistory := diskHistory
	if ca != nil && ca.history != nil {
		priorHistory = ca.history
		m.logger.Log("channel: thread=%s using cached history (%d msgs)", msg.ThreadID, len(ca.history))
	}

	// Steer channel + user content (text + images).
	aiAgent.SetSteerChannel(ta.steerRespCh)
	userContent, userImages := buildUserMessageWithAttachments(msg)
	if len(userImages) > 0 {
		aiAgent.SetPendingImages(userImages)
	}

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, userContent, agent.BuildSystemPrompt(m.cfg.Language, ""), llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})
	text, err := m.drainEvents(eventCh, aiAgent, m.isVerboseFor(msg.ThreadID), sendProgress, ta)

	// Update the in-memory history cache with the full message slice from
	// this turn (history + wrapped user msg + assistant + tool results).
	// We always update even on error/cancel so the cached state stays
	// consistent with what was sent to the LLM and recorded in the session.
	if ca != nil {
		if msgs := aiAgent.GetLastMessages(); len(msgs) > 0 {
			ca.history = msgs
			m.logger.Log("channel: thread=%s updated cached history (%d msgs)", msg.ThreadID, len(msgs))
		}
	}

	var attachments []channel.OutgoingAttachment
	if sink != nil {
		attachments = sink.snapshot()
	}
	ta.resultCh <- handlerResult{text: text, err: err, attachments: attachments}
}

// acquireForTurn handles the mode-specific prologue: choose between cached
// agent (normal turn) and throwaway agent (/compact), apply the registry
// scope, and register per-turn tools.
//
// Returns (agent, ca, sink, cleanup, err).
// ca is the cachedAgent for normal turns (nil for /compact throwaway agents).
// Callers use ca.history as the prior-history input to RunConversationStream
// and update ca.history via agent.GetLastMessages() after the turn.
// sink is non-nil only on the normal path and accumulates SendFile attachments.
// cleanup MUST be called via defer; it restores the registry / closes the
// throwaway agent / releases the cached-agent lock in the right order.
func (m *Manager) acquireForTurn(ctx context.Context, threadID string, isCompact bool) (*agent.AIAgent, *cachedAgent, *attachmentSink, func(), error) {
	if isCompact {
		aiAgent, err := m.buildAgent(ctx, threadID)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("compact: build agent: %w", err)
		}
		// /compact: no tool calls — only summarize from session context.
		aiAgent.ClearToolRegistry()
		cleanup := func() { aiAgent.Close() }
		return aiAgent, nil, nil, cleanup, nil
	}

	ca, err := m.acquireAgent(ctx, threadID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("acquire agent: %w", err)
	}
	aiAgent := ca.agent

	// Snapshot the registry so per-turn ephemeral tools (CronTool,
	// SendFileTool) don't leak into the next turn on this cached agent.
	snap := aiAgent.SaveToolRegistry()

	// CronTool — only present when scheduler is wired.
	if m.scheduler != nil {
		aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string { return threadID }))
	}

	// SendFileTool — its callback closure captures a fresh sink, so it
	// MUST be re-registered each turn.
	sendFileTool, sink := newSendFileTool()
	aiAgent.RegisterTool(sendFileTool)

	cleanup := func() {
		aiAgent.RestoreToolRegistry(snap)
		m.releaseAgent(ca)
	}
	return aiAgent, ca, sink, cleanup, nil
}
