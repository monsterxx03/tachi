package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// --- Thread activation management ---

// activateThread returns the threadActivation for threadID, creating one
// if it doesn't exist. The caller MUST check ta.steerRespCh to determine
// whether the thread is already active (steer case) or new (start case).
func (m *Manager) activateThread(threadID string, parentCtx context.Context) *threadActivation {
	ta, _ := m.threadActivations.LoadOrCompute(threadID, func() *threadActivation {
		ctx, cancel := context.WithCancel(parentCtx)
		return &threadActivation{ctx: ctx, cancel: cancel}
	})
	return ta
}

// deactivateThread removes the thread activation for threadID,
// but only if the stored activation still matches the one held by
// the caller (pointer equality). This prevents a stale handler
// goroutine from deleting a new activation created by /new or /stop.
func (m *Manager) deactivateThread(threadID string, ta *threadActivation) {
	m.threadActivations.CompareAndDelete(threadID, ta)
}

// cancelThreadTurn cancels the agent context for the given thread
// and removes the activation from the map. Safe to call when no
// turn is active.
// Called by /stop and /new to terminate a running LLM turn.
func (m *Manager) cancelThreadTurn(threadID string) {
	ta, ok := m.threadActivations.LoadAndDelete(threadID)
	if ok && ta.cancel != nil {
		ta.cancelled = true
		ta.cancel()
	}
}

// --- Handler build ---

// buildHandler returns a MessageHandler. Each call processes one incoming
// message. The first message for a thread starts a blocking agent turn;
// subsequent messages while an agent turn is active are injected via steer
// and return immediately with Steered=true.
func (m *Manager) buildHandler() channel.MessageHandler {
	return func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		m.logger.Info(ctx, "channel: recv", "thread", msg.ThreadID, "id", msg.MessageID, "len", len(msg.Content))

		// ---- Channel Whisper guard ----
		// Only non-directed messages in group chat mode with whisper enabled
		// are routed through the ambient pipeline. Single-chat messages
		// (even with Directed=false) never enter the ambient path.
		if !msg.Directed && msg.GroupChat && m.cfg.Channel.Whisper.WhisperEnabled() {
			return m.handleAmbientMessage(ctx, msg)
		}

		// ---- Whisper disabled: silently drop non-directed group messages ----
		// When whisper is off, non-directed messages in group chats are ignored
		// entirely — they don't trigger any agent turn or reply.
		if !msg.Directed && msg.GroupChat && !m.cfg.Channel.Whisper.WhisperEnabled() {
			m.logger.Info(ctx, "channel: drop (whisper disabled)", "thread", msg.ThreadID)
			return channel.HandlerResult{Dropped: true}
		}

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
		// A directed message resets the ambient silence backoff (design doc §7.3).
		ta.silenceCount.Store(0)

		// Record group chat mode on first activation (immutable for session lifetime).
		// Also cancel any pending ambient timer — directed messages take priority.
		ta.mu.Lock()
		if !ta.groupChat && msg.GroupChat {
			ta.groupChat = true
		}
		if ta.ambientTimer != nil {
			ta.ambientTimer.Stop()
			ta.ambientTimer = nil
		}
		// Transfer buffered ambient messages to steer context for the directed turn.
		var ambientSteer []ambientMsg
		if len(ta.ambientPending) > 0 {
			ambientSteer = ta.ambientPending
			ta.ambientPending = nil
			// Also record in ambient history so future ambient turns see these messages.
			m.appendToAmbientHistory(ta, ambientSteer...)
		}
		ta.mu.Unlock()

		// If there are ambient messages buffered, prepend them as steer context.
		if len(ambientSteer) > 0 {
			ta.mu.Lock()
			ta.pending = append([]string{formatAmbientForSteer(ambientSteer)}, ta.pending...)
			ta.mu.Unlock()
		}

		ta.mu.Lock()
		// A directed message preempts a running ambient turn: directed
		// messages must always get a reply, while an ambient turn is
		// best-effort and disposable. Cancelling unwinds the fork via ctx;
		// its cleanup (endAmbientTurn) sees steerRespCh replaced/nil and
		// leaves the new directed turn's state untouched.
		if ta.ambientCancel != nil {
			ta.ambientCancel()
			ta.ambientCancel = nil
			ta.steerRespCh = nil
			m.logger.Info(ctx, "channel: preempted ambient turn for directed message", "thread", msg.ThreadID)
		}
		if ta.steerRespCh != nil {
			// Agent already running — check if drainEvents is waiting for
			// an AskUser answer. When askUserRespCh is non-nil and the
			// message carries structured answers (set by the channel after
			// receiving a UI callback), route them directly to the agent.
			// Raw text messages during an AskUser wait are treated as a
			// fallback answer (user typed directly instead of using UI).
			if ta.askUserRespCh != nil && !isCompactCmd && !isSkillActivation {
				answers := msg.AskUserAnswers
				if msg.CancelAskUser {
					// Explicit cancellation — route nil answers to signal the agent.
					answers = nil
				} else if answers == nil {
					// Fallback: treat raw text as answer to the first question.
					answers = map[string]string{"q0": msg.Content}
				}
				ta.mu.Unlock()
				select {
				case ta.askUserRespCh <- tools.AskUserResult{Answers: answers}:
					m.logger.Info(ctx, "channel: AskUser answer delivered", "thread", msg.ThreadID, "entries", len(answers))
				default:
					m.logger.Warn(ctx, "channel: AskUser answer dropped (channel full)", "thread", msg.ThreadID)
				}
				return channel.HandlerResult{Steered: true}
			}

			// /compact and /skill activation require a fresh agent turn with
			// session context — they cannot be injected as steer text because
			// the LLM would see the raw command string instead of the
			// transformed instruction. Cancel the running turn and restart.
			if isCompactCmd || isSkillActivation {
				ta.mu.Unlock()
				m.cancelThreadTurn(msg.ThreadID)
				m.logger.Info(ctx, "channel: cancelled running turn, restarting with compact/skill", "thread", msg.ThreadID)
				// Activate a fresh thread for the new turn.
				ta = m.activateThread(msg.ThreadID, ctx)
				ta.isCompact = isCompactCmd
			} else {
				// Agent already running — queue this message as steer input.
				ta.pending = append(ta.pending, msg.Content)
				pendingLen := len(ta.pending)
				ta.mu.Unlock()
				m.logger.Info(ctx, "channel: steer queued", "thread", msg.ThreadID, "pending", pendingLen)
				return channel.HandlerResult{Steered: true}
			}
		} else {
			ta.mu.Unlock()
		}

		// First message for this thread (or fresh activation after cancel) —
		// start the agent.
		ta.mu.Lock()
		ta.steerRespCh = make(chan agent.SteerInput)
		ta.resultCh = make(chan handlerResult, 1)
		ta.mu.Unlock()

		// Transform /compact to compact instruction for the LLM.
		// The LLM will summarize based on its existing session context
		// without re-sending all history as text.
		if isCompactCmd {
			msg.Content = cmds.BuildCompactInstruction()
		}

		// Transform skill activation to the skill's instruction message.
		// The LLM will read the skill body and apply its instructions.
		if isSkillActivation {
			msg.Content = skillActivationMsg
		}

		// Run agent in a goroutine; handler blocks on the result channel.
		// Extract streaming callback from context before activating the thread.
		// Channel implementations (e.g., Discord) set this for real-time tool call
		// progress display. Passed directly to runAgentTurn rather than going
		// through context indirection.
		onTextDelta := streamingCallbackFromCtx(ctx)
		go m.runAgentTurn(ta.ctx, msg, sendProgress, ta, onTextDelta)

		select {
		case result := <-ta.resultCh:
			m.deactivateThread(msg.ThreadID, ta)
			if ta.cancelled {
				// Turn was cancelled by /stop or /new.
				return channel.HandlerResult{Steered: true}
			}
			// Capture the thread's working directory and model for the channel
			// to use (e.g., updating Discord channel topic). Read before
			// eviction in the compact path below.
			workDir := m.getThreadWorkDir(msg.ThreadID)
			model := ""
			if _, resolved, _ := m.getProviderForThread(msg.ThreadID); resolved != nil {
				model = resolved.Provider.Model
			}
			if result.err != nil {
				return channel.HandlerResult{
					Reply: channel.OutgoingMessage{
						ThreadID:    msg.ThreadID,
						Content:     fmt.Sprintf("❌ %v", result.err),
						ReplyTo:     msg.MessageID,
						Attachments: result.attachments,
					},
					Err:     result.err,
					WorkDir: workDir,
					Model:   model,
				}
			}

			// If this was a /compact turn, finalize the compact by creating
			// a new session with the summary and migrating the ThreadID.
			if isCompactCmd {
				// The compact turn ran on a throwaway agent that is already
				// closed, so borrow the thread's cached agent to supply the
				// memory backend for the pre-compaction write.
				//
				// The lock is released explicitly rather than by defer: the
				// evictAgent call below takes ca.mu, so holding it until this
				// function returns would deadlock.
				reply, err := func() (string, error) {
					var memAgent *agent.AIAgent
					if ca, acqErr := m.acquireAgent(ctx, msg.ThreadID); acqErr == nil {
						memAgent = ca.agent
						defer m.releaseAgent(ca)
					} else {
						m.logger.Error(ctx, "channel: /compact acquire agent for memory write", acqErr, "thread", msg.ThreadID)
					}
					return m.finalizeCompactResult(msg.ThreadID, result.text, memAgent)
				}()
				if err != nil {
					m.logger.Error(ctx, "channel: finalizeCompactResult", err, "thread", msg.ThreadID)
					return channel.HandlerResult{
						Reply: channel.OutgoingMessage{
							ThreadID: msg.ThreadID,
							Content:  fmt.Sprintf("❌ 压缩失败: %v", err),
							ReplyTo:  msg.MessageID,
						},
						Err:     err,
						WorkDir: workDir,
						Model:   model,
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
					WorkDir: workDir,
					Model:   model,
				}
			}

			return channel.HandlerResult{
				Reply: channel.OutgoingMessage{
					ThreadID:    msg.ThreadID,
					Content:     result.text,
					ReplyTo:     msg.MessageID,
					Attachments: result.attachments,
				},
				WorkDir: workDir,
				Model:   model,
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
//     AIAgent, attaches ephemeral tools (CronTool, SendFileTool) to the run
//     via a per-run tool view, and collects file attachments.
//   - Compact turn (ta.isCompact == true): builds a one-off throwaway agent
//     and runs it under a no-tools view, so /compact summarizes from session
//     context only, without polluting the cached agent's tool set.
//
// The two modes share session loading, steer wiring, image attachment, and
// drainEvents — only agent acquisition and the run's tool view differ, so
// they're isolated in acquireForTurn.
func (m *Manager) runAgentTurn(ctx context.Context, msg channel.IncomingMessage, sendProgress func(string), ta *threadActivation, onTextDelta StreamingCallback) {
	defer func() {
		// Unblock the handler on panic.
		if r := recover(); r != nil {
			m.logger.Error(ctx, "channel: agent panic", fmt.Errorf("%v", r), "thread", msg.ThreadID)
			select {
			case ta.resultCh <- handlerResult{err: fmt.Errorf("agent panic: %v", r)}:
			default:
			}
		}
	}()

	_, resolved, _ := m.getProviderForThread(msg.ThreadID)
	if resolved == nil {
		ta.resultCh <- handlerResult{err: fmt.Errorf("channel: provider not initialized")}
		return
	}

	// Mode-specific prologue: cached agent vs throwaway compact agent.
	scope, err := m.acquireForTurn(ctx, msg.ThreadID, ta.isCompact)
	if err != nil {
		ta.resultCh <- handlerResult{err: err}
		return
	}
	defer scope.cleanup()
	aiAgent, ca, sink := scope.agent, scope.ca, scope.sink

	// Bind the thread's working directory to the context so all tools
	// (Bash, Read, Write, Edit, Glob, etc.) resolve relative paths
	// against it. Falls back to the process CWD on first turn.
	workDir := "."
	if ca != nil && ca.workDir != "" {
		workDir = ca.workDir
		ctx = wdctx.WithDir(ctx, ca.workDir)
	}

	// Per-thread session — always needed for session recording (JSONL).
	// For compact turns, we also need the history from disk since there's
	// no in-memory cache (throwaway agent).
	sm, diskHistory := m.prepareThreadSession(msg.ThreadID, resolved)
	if sm != nil {
		aiAgent.SetSessionManager(sm)

		// Restore persisted working directory from session metadata.
		// When a thread's session has a WorkingDir (set by a previous /cd
		// that was persisted via persistThreadWorkDir), use it to override
		// the default initialWorkDir() so changes survive restarts.
		// Otherwise, if the cached agent has a workDir set by a recent /cd
		// (before the first message), persist it to the session.
		if ca != nil {
			sess := sm.Current()
			if sess != nil {
				if sess.WorkingDir != "" && sess.WorkingDir != ca.workDir {
					// Session has a persisted workDir from a prior /cd
					// (e.g. after restart when the cache was lost).
					ca.workDir = sess.WorkingDir
					workDir = sess.WorkingDir
					ctx = wdctx.WithDir(ctx, sess.WorkingDir)
				} else if sess.WorkingDir == "" && ca.workDir != "" && ca.workDir != "." {
					// First message after /cd on a fresh thread: persist
					// the cached agent's workDir to the session so it
					// survives restarts.
					sess.WorkingDir = ca.workDir
					if err := sm.UpdateMeta(sess); err != nil {
						m.logger.Error(ctx, "channel: persist workDir", err, "thread", msg.ThreadID)
					}
				}
			}
		}
	}

	// Use the in-memory cached history when available (normal turns).
	// This keeps the message prefix stable across turns, maximising prompt
	// cache hit rates. Fall back to disk-loaded history on first turn or
	// after agent eviction (ca.history == nil).
	priorHistory := diskHistory
	if ca != nil && ca.history != nil {
		priorHistory = ca.history
		m.logger.Info(ctx, "channel: using cached history", "thread", msg.ThreadID, "msgs", len(ca.history))
	}

	// Steer channel + user content (text + images).
	userContent, userImages := buildUserMessageWithAttachments(msg)
	scope.ropts = append(scope.ropts, agent.WithSteerChannel(ta.steerRespCh))
	if len(userImages) > 0 {
		scope.ropts = append(scope.ropts, agent.WithPendingImages(userImages))
	}

	// Build system prompt — append whisper instructions for group chat threads.
	sessionID := ""
	if sm != nil && sm.Current() != nil {
		sessionID = sm.Current().ID
	}
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, workDir, sessionID, m.cfg.Debug.PPROF)
	if ta.groupChat && m.cfg.Channel.Whisper.WhisperEnabled() {
		systemPrompt += "\n" + whisperPromptSuffix
	}

	// Append channel-specific system prompt suffix (e.g., interactive
	// channels telling the LLM to use AskUserQuestion proactively).
	m.threadChannelMu.RLock()
	threadCh := m.threadChannels[msg.ThreadID]
	m.threadChannelMu.RUnlock()
	if s, ok := threadCh.(channel.SystemPromptSuffixer); ok {
		if suffix := s.SystemPromptSuffix(); suffix != "" {
			systemPrompt += "\n" + suffix
		}
	}

	// Store thread context for AskUser support — drainEvents uses these
	// to send questions to the user and wait for a reply.
	ta.mu.Lock()
	ta.askUserThreadID = msg.ThreadID
	ta.askUserReplyID = msg.MessageID
	ta.mu.Unlock()

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, userContent, systemPrompt, llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	}, scope.ropts...)
	text, err := m.drainEvents(ctx, eventCh, aiAgent, sendProgress, ta, onTextDelta)

	// Update the in-memory history cache with the full message slice from
	// this turn (history + wrapped user msg + assistant + tool results).
	// On cancel (/stop, /new) we skip the update so the agent doesn't
	// resume partial work from a cancelled turn on the next message.
	if ca != nil && !ta.cancelled {
		if msgs := aiAgent.GetLastMessages(); len(msgs) > 0 {
			ca.history = msgs
			m.logger.Info(ctx, "channel: updated cached history", "thread", msg.ThreadID, "msgs", len(msgs))
		}
	}

	var attachments []channel.OutgoingAttachment
	if sink != nil {
		attachments = sink.snapshot()
	}
	ta.resultCh <- handlerResult{text: text, err: err, attachments: attachments}
}

// turnScope bundles everything acquireForTurn hands back for one turn. It
// replaces a 5-value positional return that was getting hard to read, and
// keeps the per-turn sink off the long-lived cachedAgent — parking per-turn
// state on a cached object is the lifetime mismatch this refactor removes.
type turnScope struct {
	agent *agent.AIAgent
	// ca is the cachedAgent for normal turns, nil for /compact throwaways.
	// Callers read ca.history for prior history and write it back after.
	ca *cachedAgent
	// sink accumulates SendFile attachments; nil on the /compact path.
	sink *attachmentSink
	// ropts must be forwarded to RunConversationStream — it carries the run's
	// tool view (ephemeral tools normally, no tools at all for /compact).
	ropts []agent.RunOption
	// cleanup MUST be called via defer: it closes the throwaway agent or
	// releases the cached-agent lock.
	cleanup func()
}

// acquireForTurn handles the mode-specific prologue: choose between cached
// agent (normal turn) and throwaway agent (/compact), and build the per-run
// tool view.
func (m *Manager) acquireForTurn(ctx context.Context, threadID string, isCompact bool) (*turnScope, error) {
	if isCompact {
		prov, resolved, _ := m.getProviderForThread(threadID)
		aiAgent, err := m.buildAgent(ctx, threadID, prov, resolved)
		if err != nil {
			return nil, fmt.Errorf("compact: build agent: %w", err)
		}
		// /compact: no tool calls — only summarize from session context.
		return &turnScope{
			agent:   aiAgent,
			ropts:   []agent.RunOption{agent.WithNoTools()},
			cleanup: func() { aiAgent.Close() },
		}, nil
	}

	ca, err := m.acquireAgent(ctx, threadID)
	if err != nil {
		return nil, fmt.Errorf("acquire agent: %w", err)
	}

	// Ephemeral per-turn tools are attached to the run rather than registered
	// on the cached agent. Both are genuinely per-turn — the SendFileTool's
	// callback closes over a fresh attachment sink, and the CronTool's over
	// this thread's ID — so run scope is what they actually mean. The cached
	// agent's registry is never touched, so a concurrent turn on another
	// thread can't observe them and there is no unregister step to forget.
	var extra []tools.Tool
	if m.scheduler != nil {
		extra = append(extra, tools.NewCronTool(m.scheduler, func() string { return threadID }))
	}
	sendFileTool, sink := newSendFileTool()
	extra = append(extra, sendFileTool)

	return &turnScope{
		agent:   ca.agent,
		ca:      ca,
		sink:    sink,
		ropts:   []agent.RunOption{agent.WithExtraTools(extra...)},
		cleanup: func() { m.releaseAgent(ca) },
	}, nil
}
