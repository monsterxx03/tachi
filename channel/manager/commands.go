package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/session"
)

// --- CommandHandler bridge: typed slash command dispatch ---

// buildCommandHandler returns a channel.CommandHandler that dispatches
// typed SlashCommand values to the Manager's slash command methods.
// This allows channels to invoke manager operations programmatically
// without routing through the text-based message handler path.
func (m *Manager) buildCommandHandler() channel.CommandHandler {
	return func(ctx context.Context, cmd channel.SlashCommand) (channel.OutgoingMessage, string, string, error) {
		result, err := m.executeSlashCommand(ctx, cmd)
		if err != nil {
			// Carry partial output (e.g. a /review that failed mid-chain) so
			// the channel can show completed work alongside the error (B7).
			reply := result.Reply
			if reply.ThreadID == "" {
				reply.ThreadID = cmd.ThreadID
			}
			return reply, "", "", err
		}
		// Read the current workDir from cache for channel topic updates.
		workDir := result.WorkDir
		if workDir == "" {
			workDir = m.getThreadWorkDir(cmd.ThreadID)
		}
		// Resolve the current model name for channel topic display.
		model := ""
		if _, resolved, _ := m.getProviderForThread(cmd.ThreadID); resolved != nil {
			model = resolved.Provider.Model
		}
		// Return the full OutgoingMessage so channels can send attachments
		// (e.g., /transcript HTML file) alongside the text reply.
		reply := result.Reply
		if reply.ThreadID == "" {
			reply.ThreadID = cmd.ThreadID
		}
		return reply, workDir, model, result.Err
	}
}

// executeSlashCommand dispatches a SlashCommand to the appropriate handler.
// Returns a HandlerResult so commands that need file attachments (e.g. /transcript)
// can include them. Text-only commands return HandlerResult with just Content set.
//
// ctx carries the channel's streaming callback (Discord status embeds) for
// long-running LLM commands like /commit and /review, plus cancellation.
func (m *Manager) executeSlashCommand(ctx context.Context, cmd channel.SlashCommand) (channel.HandlerResult, error) {
	switch cmd.Name {
	case "new":
		text, err := m.handleNewCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "commit":
		text, err := m.handleCommitCommand(ctx, cmd.ThreadID)
		return textHandlerResult(text), err
	case "review":
		text, err := m.handleReviewCommand(ctx, cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "mcp":
		if cmd.Args != "" {
			argParts := strings.Fields(cmd.Args)
			if len(argParts) > 0 && argParts[0] == "auth" {
				serverName := ""
				if len(argParts) > 1 {
					serverName = argParts[1]
				}
				text, err := m.handleMCPAuth(cmd.ThreadID, serverName)
				return textHandlerResult(text), err
			}
		}
		text, err := m.handleMCPList()
		return textHandlerResult(text), err
	case "usage":
		text, err := m.handleUsageCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "cron":
		text, err := m.handleCronCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "cd":
		text, err := m.handleCDCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "stop":
		text, err := m.handleStopCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "model":
		text, err := m.handleModelCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "thinking":
		text, err := m.handleThinkingCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "skill":
		text, err := m.handleSkillCommand(cmd.Args)
		return textHandlerResult(text), err
	case "transcript":
		return m.handleTranscriptCommand(cmd.ThreadID, cmd.Args), nil
	case "research":
		text, err := m.handleResearchCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "restart":
		text, err := m.handleRestartCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	default:
		m.logger.Info(context.Background(), "channel: unknown slash command", "action", cmd.Name, "thread", cmd.ThreadID)
		// Build available commands list from shared registry.
		var help strings.Builder
		fmt.Fprintf(&help, "Unknown command: /%s\n\nAvailable commands in channel mode:\n", cmd.Name)
		for _, def := range cmds.ForMode(cmds.ModeChannel) {
			switch def.Name {
			case "mcp":
				fmt.Fprintf(&help, "  /%-12s — %s\n", def.Name, def.Description)
				help.WriteString("  /mcp auth <server> — Start OAuth authorization for an MCP server\n")
			default:
				fmt.Fprintf(&help, "  /%-12s — %s\n", def.Name, def.Description)
			}
		}
		return textHandlerResult(help.String()), nil
	}
}

// textHandlerResult wraps a text string into a HandlerResult with just Content set.
// The caller (handleSlashCommand) fills in ThreadID/ReplyTo for the channel reply.
func textHandlerResult(text string) channel.HandlerResult {
	return channel.HandlerResult{
		Reply: channel.OutgoingMessage{
			Content: text,
		},
	}
}

// handleSlashCommand parses a text-based slash command from an IncomingMessage
// into a typed SlashCommand, then delegates to executeSlashCommand.
// Returns a fully populated HandlerResult with ThreadID and ReplyTo set.
func (m *Manager) handleSlashCommand(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
	parts := strings.Fields(msg.Content)
	if len(parts) == 0 {
		return channel.HandlerResult{}
	}
	name := strings.TrimPrefix(parts[0], "/")
	args := ""
	if len(parts) > 1 {
		args = strings.Join(parts[1:], " ")
	}
	result, err := m.executeSlashCommand(ctx, channel.SlashCommand{
		Name:     name,
		ThreadID: msg.ThreadID,
		Args:     args,
	})
	if err != nil {
		// Errors from handlers: wrap in a reply with ThreadID/ReplyTo.
		// If the handler already produced partial output (e.g. a multi-round
		// /review that failed on round 3), keep it so completed work is never
		// silently discarded (B7) — just append the error.
		content := fmt.Sprintf("❌ %v", err)
		if errors.Is(err, context.Canceled) {
			// User-initiated /stop — the stop reply already acknowledged it.
			content = "⏹️ 已取消。"
		} else if result.Reply.Content != "" {
			content = result.Reply.Content + "\n\n❌ " + err.Error()
		}
		result = channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  content,
				ReplyTo:  msg.MessageID,
			},
			Err: err,
		}
	}
	// Fill in ThreadID/ReplyTo for text-only commands that don't set them.
	// Commands like /transcript already set these themselves.
	if result.Reply.ThreadID == "" {
		result.Reply.ThreadID = msg.ThreadID
	}
	if result.Reply.ReplyTo == "" {
		result.Reply.ReplyTo = msg.MessageID
	}

	// Propagate the thread's current working directory so channel implementations
	// can update platform-specific UI (e.g., Discord channel topic). Commands
	// like /cd have just updated this in the cache.
	if result.WorkDir == "" {
		result.WorkDir = m.getThreadWorkDir(msg.ThreadID)
	}

	return result
}

// --- /model ---

// handleModelCommand lists available providers/models or switches to a named provider
// for the given thread. /model only affects the current thread; other threads
// continue using the global default provider.
// /model          — list all configured providers with current thread's choice marked
// /model <name>   — switch the current thread to the named provider
func (m *Manager) handleModelCommand(threadID, args string) (string, error) {
	if m.cfg == nil || len(m.cfg.Providers) == 0 {
		return "No providers configured.", nil
	}

	args = strings.TrimSpace(args)

	if args == "" {
		// List mode: show all providers.
		return m.handleModelList(threadID)
	}

	// Switch mode: resolve and activate the named provider for this thread.
	return m.handleModelSwitch(threadID, args)
}

// handleModelList returns a formatted list of all configured providers,
// marking the current thread's active provider with a star. Falls back to the
// global default when the thread has no per-thread override.
func (m *Manager) handleModelList(threadID string) (string, error) {
	// Determine which provider is active for this thread.
	currentName := m.providerNameForThread(threadID)

	var sb strings.Builder
	fmt.Fprintf(&sb, "Configured models (%d):\n", len(m.cfg.Providers))

	for _, p := range m.cfg.Providers {
		marker := " "
		if p.Name == currentName {
			marker = "*"
		}
		fmt.Fprintf(&sb, "\n%s %s\n", marker, p.Name)
		fmt.Fprintf(&sb, "  Type: %s  Model: %s\n", p.Type, p.Model)
	}

	sb.WriteString("\nUse /model <name> to switch (per-thread).")

	return sb.String(), nil
}

// handleModelSwitch resolves and activates the named provider for the
// current thread by persisting the choice to the session's meta.json.
// Other threads (sessions) are unaffected. On next message, the provider
// is resolved from the session's ProviderName field.
//
// If the current session's context exceeds the target model's context
// window, a compaction is triggered first using the current (wide-context)
// provider — otherwise the next API call would fail with context overflow.
func (m *Manager) handleModelSwitch(threadID, name string) (string, error) {
	pCfg := m.cfg.FindProvider(name)
	if pCfg == nil {
		return fmt.Sprintf("Provider %q not found. Use /model to see available models.", name), nil
	}

	resolved, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		return "", fmt.Errorf("resolve provider %q: %w", name, err)
	}

	sm := m.newSessionManager()
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /model find session failed", err, "thread", threadID)
	}

	compactNote := ""

	// ── Pre-switch compact check ──────────────────────────────────────
	// If the current session's context exceeds the new model's window,
	// compact first using the current (wide-context) provider so the next
	// message doesn't fail with context overflow.
	if sess != nil && sm.HasCurrent() {
		sessionMsgs, loadErr := sm.LoadMessages()
		if loadErr == nil && len(sessionMsgs) > 0 {
			currentEstimate := agent.EstimateContentTokens(sessionMsgs, sess.ProviderName)
			threshold := m.cfg.Compact.Threshold

			if currentEstimate > 0 && resolved.ContextWindow > 0 &&
				float64(currentEstimate) >= float64(resolved.ContextWindow)*threshold {

				m.logger.Info(context.Background(), "channel: /model pre-switch compact triggered", "thread", threadID, "estimate", currentEstimate, "targetCW", resolved.ContextWindow)

				summary, compactErr := m.runCompactForSwitch(threadID, sm, sessionMsgs)
				if compactErr != nil {
					m.logger.Error(context.Background(), "channel: /model pre-switch compact failed", compactErr)
					compactNote = "\n\n⚠ 自动压缩失败，切换后如果遇到上下文溢出错误，请运行 /compact。"
				} else {
					sid := ""
					if cur := sm.Current(); cur != nil {
						sid = cur.ID
					}
					systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, "", sid, m.cfg.Debug.PPROF)
					_, finalizeErr := agent.FinalizeCompact(sm, systemPrompt, summary)
					if finalizeErr != nil {
						m.logger.Error(context.Background(), "channel: /model FinalizeCompact failed", finalizeErr)
						compactNote = "\n\n⚠ 压缩后创建新 session 失败，请运行 /compact。"
					} else {
						// Migrate ThreadID to the new (current) session.
						sm.SetThreadID(threadID)
						compactNote = "\n\n🔍 当前上下文超过目标模型窗口，已自动压缩完成后切换。"
						m.logger.Info(context.Background(), "channel: /model pre-switch compact completed", "thread", threadID)
					}
				}
			}
		}
	}

	// ── Persist the new provider name ─────────────────────────────────
	// After a potential compact, sm.Current() is either the new session
	// (compact succeeded) or the original session (no compact needed).
	curr := sm.Current()
	if curr == nil {
		// No session at all — create one now to persist the model choice.
		wd, _ := os.Getwd()
		newSess, err := sm.New(m.cfg.ResolveAlias(name), wd)
		if err != nil {
			return "", fmt.Errorf("create session: %w", err)
		}
		sm.SetThreadID(threadID)
		curr = newSess
	}
	curr.ProviderName = m.cfg.ResolveAlias(name)
	curr.UpdatedAt = time.Now()
	if err := sm.UpdateMeta(curr); err != nil {
		m.logger.Error(context.Background(), "channel: /model update session meta failed", err, "thread", threadID)
	}

	// Evict only the current thread's cached agent so the next message
	// rebuilds with the new provider. Other threads are unaffected.
	m.evictAgent(threadID)

	m.logger.Info(context.Background(), "channel: /model switched provider", "thread", threadID, "name", name, "type", resolved.Type, "model", resolved.Model)

	return fmt.Sprintf("✅ Switched to **%s** (%s, %s).\nThis thread will now use this model. Other threads are unchanged.%s",
		name, resolved.Type, resolved.Model, compactNote), nil
}

// runCompactForSwitch runs a compaction LLM call using the thread's CURRENT
// (wide-context) provider. It's called before switching to a smaller-context
// model so the summarization call itself doesn't overflow.
//
// The caller (handleModelSwitch) is responsible for calling FinalizeCompact
// to create the new session and migrate the ThreadID.
func (m *Manager) runCompactForSwitch(threadID string, sm *session.Manager, sessionMsgs []session.Message) (string, error) {
	// Get the current (old) provider — before switching.
	oldProvider, _, _ := m.getProviderForThread(threadID)
	if oldProvider == nil {
		return "", fmt.Errorf("no current provider available for compact")
	}

	// Convert session messages to LLM messages for the provider API.
	llmMsgs, err := agent.ConvertSessionToLLMMessages(sessionMsgs, oldProvider.Name())
	if err != nil {
		return "", fmt.Errorf("convert messages: %w", err)
	}

	// Build messages: system prompt + history + compact instruction.
	sid := ""
	if cur := sm.Current(); cur != nil {
		sid = cur.ID
	}
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, "", sid, m.cfg.Debug.PPROF)
	compactMsgs := make([]llm.Message, 0, len(llmMsgs)+2)
	if systemPrompt != "" {
		compactMsgs = append(compactMsgs, llm.Message{Role: "system", Content: systemPrompt})
	}
	compactMsgs = append(compactMsgs, llmMsgs...)
	compactMsgs = append(compactMsgs, llm.Message{Role: "user", Content: cmds.BuildCompactInstruction()})

	// Resolve timeout and max tokens from config.
	compactTimeout := 5 * time.Minute
	if m.cfg != nil && m.cfg.Compact.Timeout > 0 {
		compactTimeout = m.cfg.Compact.Timeout
	}
	maxTokens := 4096
	if m.cfg != nil && m.cfg.Compact.MaxTokens > 0 {
		maxTokens = m.cfg.Compact.MaxTokens
	}

	ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
	defer cancel()

	resp, err := oldProvider.CreateChat(ctx, compactMsgs, nil, llm.ChatOptions{
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("compact LLM call: %w", err)
	}

	return resp.Content, nil
}

// --- /thinking ---

// handleThinkingCommand shows or sets the per-session thinking level for the
// given thread. The setting is persisted to the session's meta.json and only
// affects this session — other threads/sessions are unchanged.
//
//	/thinking           → show current level + valid options
//	/thinking <level>   → set level: none | low | medium | high | xhigh | max | default
func (m *Manager) handleThinkingCommand(threadID, args string) (string, error) {
	args = strings.TrimSpace(args)

	sm := m.newSessionManager()
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /thinking find session failed", err, "thread", threadID)
		return "", fmt.Errorf("find session: %w", err)
	}

	// No argument — show the current level and valid options.
	if args == "" {
		current := "default"
		if sess != nil && sess.ThinkingLevel != "" {
			current = sess.ThinkingLevel
		}
		return cmds.FormatThinkingStatus(current), nil
	}

	if !cmds.IsValidThinkingLevel(args) {
		return fmt.Sprintf("无效的 thinking level: **%s**\n\n可选级别:\n%s",
			args, cmds.FormatThinkingOptions()), nil
	}

	// Ensure a provider is available before persisting. The per-session
	// override is applied to the cached agent below via a single
	// getProviderForThread (session meta already updated); a thread-level
	// override that fails to resolve falls back to the global provider, so
	// checking the global state here is sufficient.
	if _, resolved := m.getProvider(); resolved == nil {
		return "无法解析当前 provider 配置，无法设置 thinking level。", nil
	}

	if sess == nil {
		// No session yet — create one bound to this thread so the override
		// persists across turns (same pattern as /model).
		wd, _ := os.Getwd()
		newSess, err := sm.New(m.currentProviderName, wd)
		if err != nil {
			return "", fmt.Errorf("create session: %w", err)
		}
		sm.SetThreadID(threadID)
		sess = newSess
	}

	// Persist the per-session override ("" = default, no override).
	if args == "default" {
		sess.ThinkingLevel = ""
	} else {
		sess.ThinkingLevel = args
	}
	sess.UpdatedAt = time.Now()
	if err := sm.UpdateMeta(sess); err != nil {
		m.logger.Error(context.Background(), "channel: /thinking update session meta failed", err, "thread", threadID)
		return "", fmt.Errorf("update session meta: %w", err)
	}

	// Apply to the cached agent immediately so the next turn uses it.
	// Future agent rebuilds pick the override up via getProviderForThread.
	m.applyThinkingToCachedAgent(threadID, sess)

	shown := sess.ThinkingLevel
	if shown == "" {
		shown = "default"
	}
	return fmt.Sprintf("✅ 当前会话 thinking level 已设为 **%s**（%s）。\n其他会话不受影响。",
		shown, cmds.ThinkingLevelDescriptions[shown]), nil
}

// applyThinkingToCachedAgent updates the cached agent for a thread with the
// session's effective thinking config. If no agent is cached yet, the next
// turn's build picks the override up via getProviderForThread — no-op here.
func (m *Manager) applyThinkingToCachedAgent(threadID string, sess *session.Session) {
	_, resolved, _ := m.getProviderForThread(threadID)
	if resolved == nil {
		return
	}
	thinking, effort := cmds.EffectiveThinking(sess.ThinkingLevel, resolved.Provider)

	m.agentCacheMu.Lock()
	ca, ok := m.agentCache[threadID]
	m.agentCacheMu.Unlock()
	if !ok {
		return
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()
	if ca.agent != nil {
		ca.agent.SetThinking(thinking, effort)
	}
}

// --- /new ---

func (m *Manager) handleNewCommand(threadID string) (string, error) {
	// Cancel any running agent turn first, so the next message
	// starts a fresh conversation rather than being steered
	// into the old turn. Also cancel any running one-off command
	// (/commit, /review) — /new is the user's "reset everything" escape
	// hatch after a mis-issued /review 10.
	m.cancelThreadTurn(threadID)
	m.cancelOneoff(threadID)

	// Drop the cached AIAgent for this thread so any state that
	// accumulated during the previous session (skill activation,
	// reminder cadence, MCP discovered set, etc.) is reset.
	m.evictAgent(threadID)

	sm := m.newSessionManager()
	if sm == nil {
		return "", fmt.Errorf("session manager unavailable")
	}

	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /new find session failed", err, "thread", threadID)
	}

	if sess != nil {
		// Clear the ThreadID on the old session so FindByThreadID won't
		// match it on the next message, then end the current session.
		sm.SetThreadID("")
		sm.EndCurrent()
		m.logger.Info(context.Background(), "channel: /new ended session", "id", sess.ID, "thread", threadID)
	}

	return "✅ Started a new conversation. Previous session has been ended.", nil
}

// --- /cd ---

// handleCDCommand changes the working directory for the current thread.
// The new directory takes effect on the next agent turn; all tools (Bash,
// Read, Write, Edit, Glob, etc.) resolve relative paths against it.
func (m *Manager) handleCDCommand(threadID, dir string) (string, error) {
	if dir == "" {
		return "Usage: /cd <directory>", nil
	}

	// Expand ~ to home directory.
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		if dir == "~" {
			dir = home
		} else {
			dir = filepath.Join(home, dir[1:])
		}
	}

	// Read or create the cached agent — on a new thread before the first
	// message, no cache entry exists yet. We create a lightweight placeholder
	// (agent == nil) to track the workDir; the AIAgent is lazily built by
	// acquireAgent on the first message.
	m.agentCacheMu.Lock()
	if m.agentCache == nil {
		m.agentCache = make(map[string]*cachedAgent)
	}
	ca, ok := m.agentCache[threadID]
	if !ok {
		// Fetch provider name outside the lock to avoid lock ordering
		// issues with providerMu (getProviderForThread acquires RLock).
		m.agentCacheMu.Unlock()
		_, _, curName := m.getProviderForThread(threadID)
		m.agentCacheMu.Lock()
		if ca, ok = m.agentCache[threadID]; !ok {
			ca = &cachedAgent{
				providerName: curName,
				workDir:      initialWorkDir(),
			}
			m.agentCache[threadID] = ca
		}
	}
	curDir := ca.workDir
	m.agentCacheMu.Unlock()

	// Resolve path relative to current workDir.
	target := dir
	if !filepath.IsAbs(dir) {
		target = filepath.Join(curDir, dir)
	}

	// Clean and verify.
	target = filepath.Clean(target)
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Sprintf("❌ Directory %q does not exist", target), nil
	}
	if !info.IsDir() {
		return fmt.Sprintf("❌ %q is not a directory", target), nil
	}

	// Update the cached agent's workDir under lock.
	m.agentCacheMu.Lock()
	if ca, ok := m.agentCache[threadID]; ok {
		ca.workDir = target
	}
	m.agentCacheMu.Unlock()

	// Persist the new working directory to the thread's session metadata
	// so it survives restarts. This is best-effort; the in-memory cache has
	// already been updated.
	m.persistThreadWorkDir(threadID, target)

	return fmt.Sprintf("✅ Working directory changed to `%s`", target), nil
}

// --- /stop ---

// handleStopCommand stops the currently-running LLM turn for the given
// thread — both agent turns (threadActivation) and one-off commands
// (/commit, /review registered via registerOneoff). If nothing is running,
// returns a message indicating that (instead of a misleading "stopped").
func (m *Manager) handleStopCommand(threadID string) (string, error) {
	if m.cancelThreadTurn(threadID) || m.cancelOneoff(threadID) {
		return "⏹️ 已停止当前任务。", nil
	}
	return "当前没有运行中的任务。", nil
}

// --- /compact ---

// finalizeCompactResult creates a new session with the LLM-generated summary,
// links it bidirectionally to the old session, migrates the ThreadID, and
// returns a formatted result string for the channel response.
//
// Unlike the old handleCompactCommand, this does NOT run the LLM call itself —
// the summary has already been generated by runAgentTurn using the current
// session context (no history re-embedding).
//
// aiAgent supplies the memory backend for the pre-compaction write; it may be
// nil, in which case the session is finalized without a memory write.
func (m *Manager) finalizeCompactResult(threadID string, summary string, aiAgent *agent.AIAgent) (string, error) {
	sm := m.newSessionManager()
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		return "", fmt.Errorf("加载 session 失败: %w", err)
	}
	if sess == nil || !sm.HasCurrent() {
		return "没有活跃的会话可以压缩。请先发送消息开始对话。", nil
	}

	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		return "", fmt.Errorf("加载消息失败: %w", err)
	}

	// Finalize: create new session, write summary, link old ↔ new.
	sid := ""
	if cur := sm.Current(); cur != nil {
		sid = cur.ID
	}
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, "", sid, m.cfg.Debug.PPROF)
	if aiAgent != nil {
		_, err = aiAgent.CompleteCompact(sm, systemPrompt, summary)
	} else {
		_, err = agent.FinalizeCompact(sm, systemPrompt, summary)
	}
	if err != nil {
		return "", fmt.Errorf("创建压缩会话失败: %w", err)
	}

	// Migrate ThreadID to new session (sm.Current now points to the new session).
	sm.SetThreadID(threadID)

	return fmt.Sprintf(
		"🔍 对话已压缩\n\n原会话: %s (%s)\n消息数: %d\n\n摘要:\n%s",
		sess.Title, sess.ID[:8], len(sessionMsgs), summary,
	), nil
}

// --- /commit ---

// handleCommitCommand runs a one-off LLM turn that drafts a commit message
// and commits the current repo changes via the Bash tool (no direct exec
// here — the model drives git itself). It runs in a clean context without
// conversation history, using the dedicated commit provider when configured
// (commit_provider), otherwise the thread's main provider.
//
// Thinking is disabled for /commit: the commit task is simple and avoiding
// thinking saves tokens/latency (same as TUI/ACP).
//
// The one-off run holds the thread's cached-agent lock for its duration
// (same as /research), so a concurrent message on this thread waits for the
// commit to finish before starting its own turn.
func (m *Manager) handleCommitCommand(ctx context.Context, threadID string) (string, error) {
	if m.cfg == nil {
		return "", fmt.Errorf("manager config unavailable")
	}

	// Global one-off concurrency cap: reject with a hint instead of silently
	// queueing behind the cached-agent lock.
	select {
	case m.oneoffSem <- struct{}{}:
		defer func() { <-m.oneoffSem }()
	default:
		return "", fmt.Errorf("已有 %d 个长任务（/commit、/review）在运行，请稍后再试", len(m.oneoffSem))
	}

	// Register so /stop and /new can cancel this run mid-flight.
	ctx, done := m.registerOneoff(threadID, ctx)
	defer done()

	ca, err := m.acquireAgent(ctx, threadID)
	if err != nil {
		return "", fmt.Errorf("acquire agent: %w", err)
	}
	defer m.releaseAgent(ca)

	workDir := m.effectiveThreadWorkDir(ca, threadID)
	// Bind the thread's working directory so the run's Bash tool resolves
	// relative paths against it (same as runAgentTurn).
	ctx = wdctx.WithDir(ctx, workDir)

	aiAgent := ca.agent
	commitProvider := aiAgent.CommitProvider()
	model := aiAgent.Model()
	if commitProvider != nil {
		// The commit prompt's co-author trailer must reflect the model that
		// actually runs, not the main provider's.
		model = commitProvider.Model()
	}

	thinkingDisabled := false
	opts := llm.ChatOptions{
		MaxTokens: config.DefaultMaxTokens,
		Thinking:  &thinkingDisabled,
	}

	sessionID := m.threadSessionID(threadID)
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, workDir, sessionID, m.cfg.Debug.PPROF)

	// /commit only needs Bash — the tool view hides everything else for the
	// duration of this run without touching the registry.
	eventCh := aiAgent.RunOneOffStream(ctx, commitProvider, systemPrompt,
		cmds.CommitUserPrompt(model), opts,
		agent.OneOffMeta{Kind: "commit", SessionID: sessionID},
		agent.WithToolSet(tools.ToolNameBash))

	text, err, incomplete := m.drainOneOffEvents(ctx, eventCh, aiAgent)
	if err != nil {
		return text, err
	}
	if incomplete {
		text += "\n\n⚠️ 提交过程未完整完成（部分输出可能缺失）。请检查 git 状态确认提交是否成功。"
	}
	return text, nil
}

// --- /review ---

// handleReviewCommand runs a code review of the current repo changes in an
// isolated fork with limited tools (Bash, ReadFile, WriteFile, Glob, Grep).
// The forked agent does NOT inherit conversation history — it gets a clean
// prompt to review git diff output.
//
// "/review N" (N ≥ 2) runs N sequential adversarial rounds in isolated forks:
// Reviewer → Challenger → Judge (role cycle, final round fixed as Judge).
// Without a round count, /review stays the single-round code review.
// See docs/2026-07-30-adversarial-review-design.md.
//
// The shared cmds.ReviewOrchestrator owns all orchestration state (round
// resolution, provider assignment with fail-fast, report directory, round
// bookkeeping) — this handler only drives the synchronous round loop, the
// same pattern as ACP (agent/acp/commands.go handleACPReview).
func (m *Manager) handleReviewCommand(ctx context.Context, threadID, args string) (string, error) {
	if m.cfg == nil {
		return "", fmt.Errorf("manager config unavailable")
	}

	// Global one-off concurrency cap: reject with a hint instead of silently
	// queueing behind the cached-agent lock.
	select {
	case m.oneoffSem <- struct{}{}:
		defer func() { <-m.oneoffSem }()
	default:
		return "", fmt.Errorf("已有 %d 个长任务（/commit、/review）在运行，请稍后再试", len(m.oneoffSem))
	}

	// Register so /stop and /new can cancel this run mid-flight.
	ctx, done := m.registerOneoff(threadID, ctx)
	defer done()

	ca, err := m.acquireAgent(ctx, threadID)
	if err != nil {
		return "", fmt.Errorf("acquire agent: %w", err)
	}
	defer m.releaseAgent(ca)

	workDir := m.effectiveThreadWorkDir(ca, threadID)
	// Bind the thread's working directory so each round's Bash/WriteFile
	// tools resolve relative paths against it — this MUST match the baseDir
	// passed to NewReviewOrchestratorFromCommand below, otherwise the
	// orchestrator's on-disk verification and the LLM's WriteFile would
	// disagree about where reports land.
	ctx = wdctx.WithDir(ctx, workDir)

	aiAgent := ca.agent

	// Resolve review provider and model from config (or fall back to main).
	reviewProvider := aiAgent.Provider()
	if rp := aiAgent.ReviewProvider(); rp != nil {
		reviewProvider = rp
	}

	// Parameter defaults/overrides come from the shared resolver (same as
	// the TUI/ACP side); only the provider lookup is agent-specific.
	ropts := cmds.ResolveReviewOptions(m.cfg)
	thinking, effort := cmds.ResolveReviewThinking(ropts,
		aiAgent.Config.Thinking, aiAgent.Config.ThinkingEffort)

	sessionID := m.threadSessionID(threadID)
	systemPrompt := agent.BuildSystemPrompt(m.cfg.Language, workDir, sessionID, m.cfg.Debug.PPROF)
	opts := llm.ChatOptions{
		MaxTokens:      config.DefaultMaxTokens,
		Thinking:       thinking,
		ThinkingEffort: effort,
	}

	// The shared orchestrator resolves rounds, assigns per-round providers
	// (fail-fast on unresolvable adversarial models) and creates the report
	// directory. Single-round reviews flow through the same path — this
	// frontend never branches on round count. The report dir is anchored at
	// the thread's working directory (the base the round's Bash/WriteFile
	// tools resolve against) — NOT the process CWD.
	orch, err := cmds.NewReviewOrchestratorFromCommand("/review "+args, ropts,
		func(rounds int) ([]llm.Provider, error) {
			if rounds == 1 {
				return []llm.Provider{reviewProvider}, nil
			}
			return aiAgent.ResolveAdversarialRoundModels(m.cfg, reviewProvider, rounds)
		}, workDir)
	if err != nil {
		return "", err
	}

	// Proactive progress so the user knows the review is running (a
	// multi-round review can take minutes).
	if orch.IsMultiRound() {
		m.sendToThread(ctx, threadID,
			fmt.Sprintf("🔍 开始代码审查 — **%d 轮对抗式审查**（Reviewer → Challenger → Judge）...", orch.TotalRounds()), "")
	} else {
		m.sendToThread(ctx, threadID, "🔍 开始代码审查...", "")
	}

	// Streaming callback for channel implementations that show real-time
	// tool-call progress (e.g. Discord status embeds). The callback rides on
	// ctx from the message/interaction handler — drainOneOffEvents forwards it.

	var out strings.Builder
	incompleteRounds := 0
	runErr := orch.Run(func(spec cmds.RoundSpec) error {
		// Per-round banner + report path hint (multi-round only).
		banner := fmt.Sprintf("── 第 %d 轮（%d/%d）— %s ──", spec.Round, spec.Round, orch.TotalRounds(), cmds.RoleName(spec.Role))
		if orch.IsMultiRound() && spec.OutPath != "" {
			m.sendToThread(ctx, threadID, banner+"\n报告输出: "+spec.OutPath, "")
		}

		forked := aiAgent.Fork(agent.ForkConfig{
			Provider:      spec.Provider,
			MaxIterations: ropts.MaxIterations,
			AllowedTools:  ropts.AllowedTools,
			Logger:        aiAgent.Logger(),
		})
		defer forked.Close()

		eventCh := forked.Agent().RunOneOffStream(ctx, spec.Provider, systemPrompt, spec.Prompt, opts,
			agent.OneOffMeta{Kind: spec.Kind, SessionID: sessionID})

		text, err, incomplete := m.drainOneOffEvents(ctx, eventCh, forked.Agent())
		if err != nil {
			return err
		}
		if incomplete {
			incompleteRounds++
		}
		// Single-round: the LLM text IS the review output. Multi-round:
		// collect banners + round text so the reply carries the full chain
		// (reports are also on disk under the report dir).
		if orch.IsMultiRound() {
			out.WriteString(banner + "\n")
		}
		if text != "" {
			out.WriteString(text + "\n\n")
			// Push the round's full text as it completes, so a later failure
			// never loses rounds the user has already seen (B7).
			if orch.IsMultiRound() {
				m.sendToThread(ctx, threadID, banner+"\n"+text, "")
			}
		}
		return nil
	})

	result := strings.TrimSpace(out.String())

	// Success line. Multi-round: status reflects any incomplete rounds and
	// points at the report directory. Single-round: add a short completion
	// marker so an empty/plain reply still signals the review happened.
	if runErr == nil {
		if orch.IsMultiRound() {
			dir, _ := filepath.Rel(workDir, orch.ReportDir())
			if dir == "" || strings.HasPrefix(dir, "..") {
				dir = orch.ReportDir()
			}
			status := fmt.Sprintf("✅ 审查完成（%d 轮）", orch.TotalRounds())
			if incompleteRounds > 0 {
				status = fmt.Sprintf("⚠️ 审查完成（%d 轮，其中 %d 轮未完整完成）", orch.TotalRounds(), incompleteRounds)
			}
			result = result + "\n\n" + status + "。报告目录: `" + dir + "`"
		} else {
			result = result + "\n\n✅ 审查完成（1 轮）"
		}
	}
	return result, runErr
}

// --- helpers for one-off LLM commands (/commit, /review) ---

// effectiveThreadWorkDir returns the working directory a one-off command
// should run in: the session's persisted WorkingDir wins (it survives
// restarts, e.g. after /cd + restart), falling back to the cached agent's
// workDir and finally ".".
func (m *Manager) effectiveThreadWorkDir(ca *cachedAgent, threadID string) string {
	if threadID != "" {
		sm := m.newSessionManager()
		if sm != nil {
			if sess, err := sm.FindByThreadID(threadID); err == nil && sess != nil && sess.WorkingDir != "" {
				return sess.WorkingDir
			}
		}
	}
	if ca != nil && ca.workDir != "" {
		return ca.workDir
	}
	return "."
}

// threadSessionID is shared with the ambient pipeline (ambient.go) — one-off
// transcripts (/commit, /review, ambient) all anchor under the session
// directory via this helper.

// drainOneOffEvents consumes an agent event stream for a one-off LLM run
// (/commit, /review round). Unlike runAgentTurn's drainEvents call, one-off
// runs have no thread activation: steer, AskUser waiting and attachment
// collection are all skipped (ta == nil). The channel's streaming callback
// (if the handler ctx carried one) is forwarded so Discord can show live
// tool-call progress embeds.
//
// The third return value (incomplete) reports whether the run ended
// abnormally — an error event occurred but drainEvents still returned nil
// because partial text was produced (its result normalization is tuned for
// regular conversation, where partial output beats a hard failure). One-off
// callers use it to mark the round/commit as incomplete instead of claiming
// success ("✅ 审查完成" when a round died midway is a lie).
func (m *Manager) drainOneOffEvents(ctx context.Context, ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent) (string, error, bool) {
	onTextDelta := streamingCallbackFromCtx(ctx)

	// Tee the event stream to spot error events that drainEvents swallows.
	// The goroutine only exits when ch closes (one-off runs have no
	// AskUser/Steer early-return paths), so the channel close establishes a
	// happens-before edge: by the time drainEvents returns, incomplete is final.
	tee := make(chan agent.AgentEvent, 8)
	var incomplete bool
	go func() {
		defer close(tee)
		for e := range ch {
			switch e.Type {
			case agent.AgentEventError:
				incomplete = true
			case agent.AgentEventTurnComplete:
				if e.Result != nil && e.Result.Error != nil {
					incomplete = true
				}
			}
			tee <- e
		}
	}()

	text, err := m.drainEvents(ctx, tee, aiAgent, nil, nil, onTextDelta)
	return text, err, incomplete
}

// --- /mcp ---

// handleMCPAuth starts the OAuth2 flow for an HTTP MCP server.
//
// Usage: /mcp auth <server>
//
// Runs in a background goroutine and skips the browser-open step entirely
// (channel mode is headless). startManualFlow binds a local callback listener
// on the configured callback_host (e.g. 10.x.x.x) and sends the authorization
// URL to the user via sendToThread. The OAuth provider redirects the user's
// browser to that address; the listener catches the code and completes the
// exchange automatically — no paste-back required.
//
// On success the token is persisted to disk; the next agent turn will
// automatically connect to the server using the stored credentials.
func (m *Manager) handleMCPAuth(threadID, serverName string) (string, error) {
	if serverName == "" {
		return "Usage: `/mcp auth <server>` — authorize an HTTP MCP server", nil
	}

	if m.cfg == nil {
		return "", fmt.Errorf("manager config unavailable")
	}

	// Find the server config.
	var srv *config.MCPServerConfig
	for i := range m.cfg.MCPServers {
		if m.cfg.MCPServers[i].Name == serverName {
			srv = &m.cfg.MCPServers[i]
			break
		}
	}
	if srv == nil {
		return fmt.Sprintf("MCP server **%s** not found. Use `/mcp` to list configured servers.", serverName), nil
	}
	if srv.Type != config.MCPTransportHTTP {
		return fmt.Sprintf("OAuth is only supported for HTTP MCP servers. **%s** uses stdio transport.", serverName), nil
	}

	// Run the OAuth flow in a background goroutine so we can return an
	// immediate acknowledgement. RunManualOAuthFlow skips the browser-open
	// attempt; startManualFlow binds on the configured callback_host and
	// sends the authorization URL to the user via statusFn.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		statusFn := func(msg string) {
			m.sendToThread(context.Background(), threadID, msg, "")
		}

		if err := mcp.RunManualOAuthFlow(ctx, srv, statusFn); err != nil {
			m.logger.Error(context.Background(), "channel: /mcp auth flow failed", err, "name", serverName)
			m.sendToThread(context.Background(), threadID,
				fmt.Sprintf("❌ OAuth failed for **%s**: %v", serverName, err), "")
			return
		}

		m.logger.Info(context.Background(), "channel: /mcp auth flow succeeded", "name", serverName)
		m.sendToThread(context.Background(), threadID,
			fmt.Sprintf("✅ OAuth authorization successful for **%s**!\n\nSend a message to start using the server's tools.", serverName), "")
	}()

	return fmt.Sprintf("🔐 Starting OAuth authorization for **%s**...\n\nThe authorization URL will be sent to you in a moment.", serverName), nil
}

// handleMCPList returns a markdown-formatted list of configured MCP servers
// with their discovered (loaded) tools when the shared MCP manager is available.
func (m *Manager) handleMCPList() (string, error) {
	servers := m.cfg.MCPServers
	if len(servers) == 0 {
		return "No MCP servers configured.", nil
	}

	mgr := m.initSharedMCP()
	infos := cmds.BuildMCPServerInfos(servers, mgr)
	return cmds.FormatMCPList(infos), nil
}

// --- /usage ---

// handleUsageCommand returns usage stats for the session associated with the ThreadID.
func (m *Manager) handleUsageCommand(threadID string) (string, error) {
	if threadID == "" {
		return "No active session (no thread ID).", nil
	}

	sm := m.newSessionManager()
	if sm == nil {
		return "Session manager unavailable.", nil
	}

	_, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /usage find session failed", err, "thread", threadID)
		return "Failed to find session.", nil
	}
	if !sm.HasCurrent() {
		return "No session found for this thread. Send a message first to start a session.", nil
	}

	// Resolve price and context window
	var price *llm.ModelPrice
	var contextWindow int64
	_, resolved := m.getProvider()
	if resolved != nil {
		contextWindow = resolved.Provider.ContextWindow
		price = cmds.ResolveModelPrice(m.cfg, resolved.Provider.Name, resolved.Provider.Model)
	}

	report, err := agent.ComputeSessionUsage(sm, price, contextWindow)
	if err != nil {
		return fmt.Sprintf("Failed to compute usage: %v", err), nil
	}

	// Read the estimate and its breakdown together: a turn may be running on
	// this thread, and two separate reads could mix values from two estimates.
	estTokens, estBreakdown := m.getAgentEstimateWithBreakdown(threadID)

	// Convert tool call stats to shared type
	toolCalls := make(map[string]*cmds.ToolCallStat, len(report.ToolCalls))
	for name, st := range report.ToolCalls {
		toolCalls[name] = &cmds.ToolCallStat{Count: st.Count, ErrCount: st.ErrCount}
	}

	info := &cmds.UsageReportInfo{
		SessionID:                report.Session.ID,
		Provider:                 report.Session.ProviderName,
		Title:                    report.Session.Title,
		ContextWindow:            report.ContextWindow,
		InputTokens:              report.Usage.InputTokens,
		LastInputTokens:          report.Usage.LastInputTokens,
		CacheReadInputTokens:     report.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: report.Usage.CacheCreationInputTokens,
		OutputTokens:             report.Usage.OutputTokens,
		EstimatedInputTokens:     estTokens,
		Cost:                     report.Cost,
		ToolCalls:                toolCalls,
		MainCount:                report.MainCount,
		SubCount:                 report.SubCount,
		PprofAddr:                m.cfg.Debug.PPROF.Addr(),
	}
	info.EstBreakdown = estBreakdown

	return cmds.FormatUsageReport(info), nil
}

// --- /cron ---

// handleCronCommand handles the /cron slash command, listing cron jobs
// scoped to the current thread. Pass an empty threadID to list all jobs.
func (m *Manager) handleCronCommand(threadID string) (string, error) {
	if m.scheduler == nil {
		return "Cron scheduler is not enabled. Set cron.enabled: true in config.yaml.", nil
	}

	allJobs, err := m.scheduler.List()
	if err != nil {
		return "", fmt.Errorf("cron: list: %w", err)
	}

	// Filter by current thread, matching CronTool.handleList() behavior.
	var jobs []*cron.Job
	for _, job := range allJobs {
		if threadID == "" || job.TargetThreadID == threadID {
			jobs = append(jobs, job)
		}
	}

	if len(jobs) == 0 {
		if threadID == "" {
			return "No cron jobs configured.\n\nYou can ask me to create one! Example:\n\"帮我设置一个每天早上9点的日报提醒\"", nil
		}
		return "No cron jobs configured for this thread.", nil
	}

	slices.SortFunc(jobs, func(a, b *cron.Job) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Cron Jobs (%d)\n", len(jobs))

	for _, job := range jobs {
		status := "🟢 Active"
		if job.Status == cron.JobStatusPaused {
			status = "⏸️ Paused"
		}
		if job.Type == cron.JobTypeOneshot {
			status += " · Oneshot"
		}
		fmt.Fprintf(&sb, "\n%s **%s** [%s]\n", status, job.Name, job.ID)
		fmt.Fprintf(&sb, "  Schedule: `%s`\n", job.Schedule)
		fmt.Fprintf(&sb, "  Prompt: %s\n", truncateForDisplay(job.Prompt, 60))
		if !job.LastRunAt.IsZero() {
			icon := "✅"
			if job.LastRunStatus == "error" {
				icon = "❌"
			}
			fmt.Fprintf(&sb, "  Last run: %s %s\n", icon, job.LastRunAt.Format("01-02 15:04"))
		}
	}

	return sb.String(), nil
}

// --- /skill ---

// handleSkillCommand dispatches /skill sub-commands:
//
//	/skill or /skill list  → list skills
//	/skill reload          → re-scan skill directories
//	/skill <name>          → handled via agent turn (not via this method)
func (m *Manager) handleSkillCommand(args string) (string, error) {
	args = strings.TrimSpace(args)
	switch args {
	case "", "list":
		return m.handleSkillList()
	case "reload":
		return m.handleSkillReload()
	default:
		// /skill <name> — activation goes through agent turn.
		// If we reach here via synchronous dispatch, the name is unknown.
		// This shouldn't normally happen because buildHandler intercepts
		// skill activations before handleSlashCommand, but we handle it
		// gracefully via the CommandHandler path.
		if m.skillStore != nil {
			if _, found := m.skillStore.ResolveCommand(args); found {
				return "", fmt.Errorf("skill activation requires an agent turn; send via message, not typed command")
			}
		}
		return "Unknown /skill sub-command. Available: list, reload, <skill-name>", nil
	}
}

// handleSkillList returns a formatted list of all available skills.
func (m *Manager) handleSkillList() (string, error) {
	if m.skillStore == nil {
		return "Skill system not available.", nil
	}

	metas := m.skillStore.List()
	return cmds.FormatSkillList(metas), nil
}

// handleSkillReload re-scans skill directories and returns the updated count.
func (m *Manager) handleSkillReload() (string, error) {
	if m.skillStore == nil {
		return "Skill system not available.", nil
	}

	// Re-scan using the same directory scope the store was constructed
	// with. (Tests that injected a hermetic store via Config.SkillStore
	// keep their scope; production callers that used the default
	// ~/.tachi/skills layout get the same dirs back.)
	m.skillStore = skill.NewStoreWithDirs(m.skillStore.Dirs(), m.skillStore.Sources())
	metas := m.skillStore.List()

	// Propagate the reload to all cached agents so their SkillListReminder
	// re-fires and the skill tool uses the updated store.
	m.reloadAgentSkills()

	return fmt.Sprintf("Skills 已重新加载 — 发现 %d 个 skill(s)", len(metas)), nil
}

// reloadAgentSkills calls ReloadSkills on every cached AIAgent so the new
// skill store is picked up, the SkillListReminder is marked dirty, and the
// skill tool is re-registered with the updated store.
//
// Lock ordering: agentCacheMu → ca.mu (consistent with acquireAgent and
// evictAllAgents). Each ca.mu is acquired and released individually so that
// agents in use on other threads are naturally serialized.
func (m *Manager) reloadAgentSkills() {
	m.agentCacheMu.Lock()
	defer m.agentCacheMu.Unlock()
	for _, ca := range m.agentCache {
		ca.mu.Lock()
		if ca.agent != nil {
			ca.agent.ReloadSkills()
		}
		ca.mu.Unlock()
	}
	m.logger.Info(context.Background(), "channel: skill reload propagated", "count", len(m.agentCache))
}

// prepareSkillActivation builds the user message content for skill activation.
// Returns the activation message string, or an error message as string + error.
func (m *Manager) prepareSkillActivation(skillName string, extraArgs string) (string, string, error) {
	if m.skillStore == nil {
		return "", "Skill system not available.", fmt.Errorf("skill system not available")
	}

	sk, err := m.skillStore.Load(skillName)
	if err != nil {
		return "", fmt.Sprintf("Skill **%s** 未找到。使用 `/skill` 查看可用 skills。", skillName), err
	}

	msg := skill.BuildActivationMessage(sk, extraArgs)
	return msg, "", nil
}

// isSkillActivation checks if the message is a skill activation pattern:
//   - /skill <name> [args]
//   - /<skillname> [args]
//
// Returns (skillName, extraArgs, isActivation).
func (m *Manager) isSkillActivation(content string) (string, string, bool) {
	parts := strings.Fields(strings.TrimPrefix(content, "/"))
	if len(parts) == 0 {
		return "", "", false
	}

	// /skill <name> [args]
	if strings.HasPrefix(content, "/skill ") && len(parts) >= 2 {
		sub := strings.TrimPrefix(content, "/skill ")
		subParts := strings.Fields(sub)
		if len(subParts) == 0 {
			return "", "", false
		}
		skillName := subParts[0]
		if skillName == "list" || skillName == "reload" {
			return "", "", false // handled synchronously
		}
		extraArgs := ""
		if len(subParts) > 1 {
			extraArgs = strings.Join(subParts[1:], " ")
		}
		return skillName, extraArgs, true
	}

	// /<skillname> [args]
	skillName := parts[0]
	if name, found := m.skillStore.ResolveCommand(skillName); found {
		extraArgs := ""
		if len(parts) > 1 {
			extraArgs = strings.Join(parts[1:], " ")
		}
		return name, extraArgs, true
	}

	return "", "", false
}

// --- /transcript ---

// handleTranscriptCommand generates an HTML transcript for the session
// associated with the given thread and returns it as a file attachment.
//
// Called from executeSlashCommand for the "transcript" case.
//
// Args:
//   - threadID: the thread whose session to render (or "" if using --latest)
//   - args: command arguments (e.g. "--latest")
func (m *Manager) handleTranscriptCommand(threadID, args string) channel.HandlerResult {
	errReply := func(err error) channel.HandlerResult {
		return channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: threadID,
				Content:  fmt.Sprintf("❌ %v", err),
			},
			Err: err,
		}
	}

	sm := m.newSessionManager()
	if sm == nil {
		return errReply(fmt.Errorf("session manager unavailable"))
	}

	var sess *session.Session
	var err error

	parts := strings.Fields(args)
	useLatest := len(parts) > 0 && parts[0] == "--latest"

	if useLatest {
		sessions, listErr := sm.List()
		if listErr != nil {
			return errReply(fmt.Errorf("list sessions: %w", listErr))
		}
		if len(sessions) == 0 {
			return errReply(fmt.Errorf("no sessions found"))
		}
		sess, err = sm.Load(sessions[0].ID)
		if err != nil {
			return errReply(fmt.Errorf("load session: %w", err))
		}
	} else {
		found, findErr := sm.FindByThreadID(threadID)
		if findErr != nil {
			return errReply(fmt.Errorf("find session: %w", findErr))
		}
		if found == nil {
			return errReply(fmt.Errorf("no session found for this thread; send a message first to start a session"))
		}
		sess = found
	}

	msgs, err := sm.LoadMessages()
	if err != nil {
		return errReply(fmt.Errorf("load messages: %w", err))
	}
	if len(msgs) == 0 {
		return errReply(fmt.Errorf("session %q has no messages yet; run a conversation first", sess.ID))
	}

	// Sub-agent sidecar messages are optional — a load failure is non-fatal.
	subagents, _ := sm.LoadSubagentMessages(sess.ID)

	data := render.BuildReportDataFromMessages(sess, msgs, subagents)
	html, err := render.GenerateHTML(data)
	if err != nil {
		return errReply(fmt.Errorf("generate HTML: %w", err))
	}

	// Use session title as filename, sanitized.
	fileName := sanitizeFilename(sess.Title)
	if fileName == "" {
		fileName = "transcript"
	}
	htmlFileName := fmt.Sprintf("%s-%s.html", fileName, sess.ID[:8])

	m.logger.Info(context.Background(), "channel: transcript generated", "id", sess.ID, "bytes", len(html))

	contentText := fmt.Sprintf("📊 Transcript: %s\n\nSession: %s\nTurns: %d · Tools: %d · Size: %s",
		sess.Title, sess.ID[:8],
		data.Stats.TurnCount, data.Stats.ToolCallCount, humanSize(len(html)))

	return channel.HandlerResult{
		Reply: channel.OutgoingMessage{
			Content:  contentText,
			ThreadID: threadID,
			Attachments: []channel.OutgoingAttachment{
				{
					Type:     channel.AttachmentTypeFile,
					FileName: htmlFileName,
					MimeType: "text/html",
					Data:     []byte(html),
				},
			},
		},
	}
}

// --- /research ---

// handleResearchCommand runs deep research on the given topic.
// In channel mode, this runs synchronously (the channel goroutine blocks
// while research is in progress). Progress messages are sent asynchronously
// via sendToThread.
//
// Uses the cached agent for the thread to get the SubagentRunner.
// The agent lock is held for the duration of research, preventing other
// messages on the same thread from interfering.
func (m *Manager) handleResearchCommand(threadID, args string) (string, error) {
	parsed := cmds.ParseResearchArgs(args)
	if parsed.Topic == "" {
		return "Usage: `/research <topic> [--depth N] [--breadth N]`", nil
	}

	if m.cfg == nil {
		return "", fmt.Errorf("manager config unavailable")
	}

	if parsed.Depth <= 0 {
		parsed.Depth = m.cfg.DeepResearch.DefaultDepth
	}
	if parsed.Breadth <= 0 {
		parsed.Breadth = m.cfg.DeepResearch.DefaultBreadth
	}

	// Acquire the cached agent for this thread. This gives us a fully
	// configured agent with SubagentRunner available.
	// Use background context since research may outlive the message context.
	ca, err := m.acquireAgent(context.Background(), threadID)
	if err != nil {
		return "", fmt.Errorf("acquire agent: %w", err)
	}
	defer m.releaseAgent(ca)

	engine, err := ca.agent.NewDeepResearch(m.cfg)
	if err != nil {
		return "", fmt.Errorf("create research engine: %w", err)
	}
	if engine == nil {
		return "Deep Research is not available (engine creation returned nil).", nil
	}

	// Send initial progress message
	m.sendToThread(context.Background(), threadID,
		fmt.Sprintf("🔬 **深度研究已启动**\n\n**主题**: %s\n**深度**: %d | **广度**: %d\n\n正在搜索中...",
			parsed.Topic, parsed.Depth, parsed.Breadth), "")

	// Run research synchronously (blocks this goroutine but the agent lock
	// prevents concurrent access on the same thread). Progress callbacks
	// stream intermediate updates to the thread.
	researchCtx, cancel := context.WithTimeout(context.Background(), m.cfg.DeepResearch.Timeout)
	defer cancel()

	report, runErr := engine.Run(researchCtx, parsed.Topic, parsed.Depth, parsed.Breadth, func(format string, args ...any) {
		m.sendToThread(researchCtx, threadID, fmt.Sprintf(format, args...), "")
	})
	if runErr != nil {
		return "", fmt.Errorf("research failed: %w", runErr)
	}

	return report, nil
}

// --- /restart ---

// handleRestartCommand checks if Tachi is running under systemd and restarts
// the service via systemctl. Returns an error if not running under systemd.
// It sends a proactive "restarting" message before executing the restart.
func (m *Manager) handleRestartCommand(threadID string) (string, error) {
	// Check if running under systemd by verifying PPID == 1 and
	// /proc/1/comm is "systemd". This works on all systemd versions,
	// unlike INVOCATION_ID which requires systemd >= 232 (CentOS 7 has v219).
	if !isRunningUnderSystemd() {
		return "", errors.New("tachi 不是由 systemd 启动的，无法通过 systemctl 重启")
	}

	// Determine if this is a user service or system service.
	// User services have XDG_RUNTIME_DIR set.
	_, isUser := os.LookupEnv("XDG_RUNTIME_DIR")

	// Build the systemctl command.
	args := []string{}
	if isUser {
		args = append(args, "--user")
	}
	args = append(args, "restart", "tachi")

	// Send a proactive reply BEFORE executing the restart, so the user
	// actually sees the message before systemd kills this process.
	m.sendToThread(context.Background(), threadID,
		"🔄 正在重启 Tachi...\n\n```\nsystemctl "+strings.Join(args, " ")+"\n```", "")

	// Execute asynchronously — we don't wait for the process to exit
	// because the systemd stop signal will kill us first.
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动 systemctl 失败: %w", err)
	}

	// Return an empty message — the real reply was already sent via
	// sendToThread above. Returning empty tells the caller not to send
	// a duplicate.
	return "", nil
}

// isRunningUnderSystemd checks if the current process is managed by systemd.
// It verifies that the parent process (PPID 1) is systemd, which works on all
// systemd versions including older ones like systemd 219 on CentOS 7.
func isRunningUnderSystemd() bool {
	data, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		return false
	}
	return string(data) == "systemd\n"
}
