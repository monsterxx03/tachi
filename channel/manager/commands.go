package manager

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/transcript/render"
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
	return func(ctx context.Context, cmd channel.SlashCommand) (string, error) {
		result, err := m.executeSlashCommand(cmd)
		if err != nil {
			return "", err
		}
		return result.Reply.Content, result.Err
	}
}

// executeSlashCommand dispatches a SlashCommand to the appropriate handler.
// Returns a HandlerResult so commands that need file attachments (e.g. /transcript)
// can include them. Text-only commands return HandlerResult with just Content set.
func (m *Manager) executeSlashCommand(cmd channel.SlashCommand) (channel.HandlerResult, error) {
	switch cmd.Name {
	case "new":
		text, err := m.handleNewCommand(cmd.ThreadID)
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
		text, err := m.handleCronCommand()
		return textHandlerResult(text), err
	case "v":
		text, err := m.handleVerboseCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "stop":
		text, err := m.handleStopCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "model":
		text, err := m.handleModelCommand(cmd.Args)
		return textHandlerResult(text), err
	case "skill":
		text, err := m.handleSkillCommand(cmd.Args)
		return textHandlerResult(text), err
	case "transcript":
		return m.handleTranscriptCommand(cmd.ThreadID, cmd.Args), nil
	default:
		m.logger.Log("channel: unknown slash command: %s (thread=%s)", cmd.Name, cmd.ThreadID)
		// Build available commands list from shared registry.
		var help strings.Builder
		help.WriteString(fmt.Sprintf("Unknown command: /%s\n\nAvailable commands in channel mode:\n", cmd.Name))
		for _, def := range cmds.ForMode(cmds.ModeChannel) {
			switch def.Name {
			case "mcp":
				help.WriteString(fmt.Sprintf("  /%-12s — %s\n", def.Name, def.Description))
				help.WriteString("  /mcp auth <server> — Start OAuth authorization for an MCP server\n")
			default:
				help.WriteString(fmt.Sprintf("  /%-12s — %s\n", def.Name, def.Description))
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
func (m *Manager) handleSlashCommand(msg channel.IncomingMessage) channel.HandlerResult {
	parts := strings.Fields(msg.Content)
	if len(parts) == 0 {
		return channel.HandlerResult{}
	}
	name := strings.TrimPrefix(parts[0], "/")
	args := ""
	if len(parts) > 1 {
		args = strings.Join(parts[1:], " ")
	}
	result, err := m.executeSlashCommand(channel.SlashCommand{
		Name:     name,
		ThreadID: msg.ThreadID,
		Args:     args,
	})
	if err != nil {
		// Errors from text-only handlers: wrap in a reply with ThreadID/ReplyTo.
		result = channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  fmt.Sprintf("❌ %v", err),
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
	return result
}

// --- /model ---

// handleModelCommand lists available providers/models or switches to a named provider.
// /model          — list all configured providers with the current one marked
// /model <name>   — switch to the named provider
func (m *Manager) handleModelCommand(args string) (string, error) {
	if m.cfg == nil || len(m.cfg.Providers) == 0 {
		return "No providers configured.", nil
	}

	args = strings.TrimSpace(args)

	if args == "" {
		// List mode: show all providers.
		return m.handleModelList()
	}

	// Switch mode: resolve and activate the named provider.
	return m.handleModelSwitch(args)
}

// handleModelList returns a formatted list of all configured providers,
// marking the currently active one with a star.
func (m *Manager) handleModelList() (string, error) {
	m.providerMu.RLock()
	currentName := m.currentProviderName
	m.providerMu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Configured models (%d):\n", len(m.cfg.Providers)))

	for _, p := range m.cfg.Providers {
		marker := " "
		if p.Name == currentName {
			marker = "*"
		}
		fmt.Fprintf(&sb, "\n%s %s\n", marker, p.Name)
		fmt.Fprintf(&sb, "  Type: %s  Model: %s\n", p.Type, p.Model)
	}

	sb.WriteString("\nUse /model <name> to switch.")

	return sb.String(), nil
}

// handleModelSwitch resolves and activates the named provider.
func (m *Manager) handleModelSwitch(name string) (string, error) {
	pCfg := m.cfg.FindProvider(name)
	if pCfg == nil {
		return fmt.Sprintf("Provider %q not found. Use /model to see available models.", name), nil
	}

	resolved, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		return "", fmt.Errorf("resolve provider %q: %w", name, err)
	}

	provider, err := llm.NewProvider(
		resolved.Type,
		resolved.APIKey,
		resolved.BaseURL,
		resolved.Model,
	)
	if err != nil {
		return "", fmt.Errorf("create provider %q: %w", name, err)
	}

	m.providerMu.Lock()
	m.provider = provider
	m.resolvedConfig = &config.ResolvedConfig{
		Provider:      *resolved,
		MaxTokens:     m.resolvedConfig.MaxTokens,
		MaxIterations: m.resolvedConfig.MaxIterations,
	}
	m.currentProviderName = name
	m.providerMu.Unlock()

	// Drop every cached AIAgent so the next message rebuilds against the
	// new provider/model. (acquireAgent also detects this via providerName
	// comparison, but evicting up-front frees resources sooner.)
	m.evictAllAgents()

	m.logger.Log("channel: /model switched to %s (%s/%s)", name, resolved.Type, resolved.Model)

	return fmt.Sprintf("✅ Switched to **%s** (%s, %s).\nNew conversations will use this model.", name, resolved.Type, resolved.Model), nil
}

// --- /new ---

func (m *Manager) handleNewCommand(threadID string) (string, error) {
	// Cancel any running agent turn first, so the next message
	// starts a fresh conversation rather than being steered
	// into the old turn.
	m.cancelThreadTurn(threadID)

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
		m.logger.Log("channel: /new find session for %s: %v", threadID, err)
	}

	if sess != nil {
		// Clear the ThreadID on the old session so FindByThreadID won't
		// match it on the next message, then end the current session.
		if err := sm.SetThreadID(""); err != nil {
			m.logger.Log("channel: /new clear thread_id for %s: %v", threadID, err)
		}
		sm.EndCurrent()
		m.logger.Log("channel: /new ended session %s for thread %s", sess.ID, threadID)
	}

	// Reset verbose state for the new session.
	m.verboseMu.Lock()
	if m.verboseState != nil {
		delete(m.verboseState, threadID)
	}
	m.verboseMu.Unlock()

	return "✅ Started a new conversation. Previous session has been ended.", nil
}

// --- /stop ---

// handleStopCommand stops the currently-running LLM turn for the given thread.
// If no turn is active, returns a message indicating that.
func (m *Manager) handleStopCommand(threadID string) (string, error) {
	m.cancelThreadTurn(threadID)
	return "⏹️ 已停止当前对话。", nil
}

// --- /compact ---

// finalizeCompactResult creates a new session with the LLM-generated summary,
// links it bidirectionally to the old session, migrates the ThreadID, and
// returns a formatted result string for the channel response.
//
// Unlike the old handleCompactCommand, this does NOT run the LLM call itself —
// the summary has already been generated by runAgentTurn using the current
// session context (no history re-embedding).
func (m *Manager) finalizeCompactResult(threadID string, summary string) (string, error) {
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
	_, err = agent.FinalizeCompact(sm, m.systemPrompt, summary)
	if err != nil {
		return "", fmt.Errorf("创建压缩会话失败: %w", err)
	}

	// Migrate ThreadID to new session (sm.Current now points to the new session).
	if err := sm.SetThreadID(threadID); err != nil {
		m.logger.Log("channel: /compact set thread_id: %v", err)
	}

	return fmt.Sprintf(
		"🔍 对话已压缩\n\n原会话: %s (%s)\n消息数: %d\n\n摘要:\n%s",
		sess.Title, sess.ID[:8], len(sessionMsgs), summary,
	), nil
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
			m.logger.Log("channel: /mcp auth flow for %q failed: %v", serverName, err)
			m.sendToThread(context.Background(), threadID,
				fmt.Sprintf("❌ OAuth failed for **%s**: %v", serverName, err), "")
			return
		}

		m.logger.Log("channel: /mcp auth flow for %q succeeded", serverName)
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
		m.logger.Log("channel: /usage find session for %s: %v", threadID, err)
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
		model := resolved.Provider.Model
		contextWindow = resolved.Provider.ContextWindow
		pCfg := m.cfg.FindProvider(resolved.Provider.Name)
		if pCfg != nil {
			price = llm.ResolveModelPrice(model, pCfg.InputPrice, pCfg.OutputPrice, pCfg.CacheReadInputPrice, pCfg.CacheCreationInputPrice)
		}
		if price == nil {
			price = llm.ResolveModelPrice(model, nil, nil, nil, nil)
		}
	}

	report, err := agent.ComputeSessionUsage(sm, price, contextWindow)
	if err != nil {
		return fmt.Sprintf("Failed to compute usage: %v", err), nil
	}

	// Convert tool call stats to shared type
	toolCalls := make(map[string]*cmds.ToolCallStat, len(report.ToolCalls))
	for name, st := range report.ToolCalls {
		toolCalls[name] = &cmds.ToolCallStat{Count: st.Count, ErrCount: st.ErrCount}
	}

	info := &cmds.UsageReportInfo{
		SessionID:                report.Session.ID,
		Provider:                 report.Session.Provider,
		Model:                    report.Session.Model,
		Title:                    report.Session.Title,
		ContextWindow:            report.ContextWindow,
		InputTokens:              report.Usage.InputTokens,
		LastInputTokens:          report.Usage.LastInputTokens,
		CacheReadInputTokens:     report.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: report.Usage.CacheCreationInputTokens,
		OutputTokens:             report.Usage.OutputTokens,
		Cost:                     report.Cost,
		ToolCalls:                toolCalls,
		MainCount:                report.MainCount,
		SubCount:                 report.SubCount,
	}

	return cmds.FormatUsageReport(info), nil
}

// --- /cron ---

// handleCronCommand handles the /cron slash command, listing all cron jobs.
func (m *Manager) handleCronCommand() (string, error) {
	if m.scheduler == nil {
		return "Cron scheduler is not enabled. Set cron.enabled: true in config.yaml.", nil
	}

	jobs, err := m.scheduler.List()
	if err != nil {
		return "", fmt.Errorf("cron: list: %w", err)
	}

	if len(jobs) == 0 {
		return "No cron jobs configured.\n\nYou can ask me to create one! Example:\n\"帮我设置一个每天早上9点的日报提醒\"", nil
	}

	slices.SortFunc(jobs, func(a, b *cron.Job) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 Cron Jobs (%d)\n", len(jobs)))

	for _, job := range jobs {
		status := "🟢 Active"
		if job.Status == cron.JobStatusPaused {
			status = "⏸️ Paused"
		}
		if job.Type == cron.JobTypeOneshot {
			status += " · Oneshot"
		}
		sb.WriteString(fmt.Sprintf("\n%s **%s** [%s]\n", status, job.Name, job.ID))
		sb.WriteString(fmt.Sprintf("  Schedule: `%s`\n", job.Schedule))
		sb.WriteString(fmt.Sprintf("  Prompt: %s\n", truncateForDisplay(job.Prompt, 60)))
		if !job.LastRunAt.IsZero() {
			icon := "✅"
			if job.LastRunStatus == "error" {
				icon = "❌"
			}
			sb.WriteString(fmt.Sprintf("  Last run: %s %s\n", icon, job.LastRunAt.Format("01-02 15:04")))
		}
	}

	return sb.String(), nil
}

// --- /v ---

// handleVerboseCommand toggles verbose tool call output for the given thread.
// When on, subsequent replies include a summary of tool calls made by the agent.
func (m *Manager) handleVerboseCommand(threadID string) (string, error) {
	m.verboseMu.Lock()
	if m.verboseState == nil {
		m.verboseState = make(map[string]bool)
	}
	current := m.verboseState[threadID]
	m.verboseState[threadID] = !current
	m.verboseMu.Unlock()

	if !current {
		return "🔍 Verbose mode: ON\n后续回复将显示工具调用过程。", nil
	}
	return "🔍 Verbose mode: OFF\n后续回复仅显示最终结果。", nil
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

	return fmt.Sprintf("Skills 已重新加载 — 发现 %d 个 skill(s)", len(metas)), nil
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
			return errReply(fmt.Errorf("no session found for this thread. Send a message first to start a session."))
		}
		sess = found
	}

	msgs, err := sm.LoadMessages()
	if err != nil {
		return errReply(fmt.Errorf("load messages: %w", err))
	}
	if len(msgs) == 0 {
		return errReply(fmt.Errorf("session %q has no messages yet. Run a conversation first.", sess.ID))
	}

	data := render.BuildReportDataFromMessages(sess, msgs)
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

	m.logger.Log("channel: transcript generated for session %s (%d bytes)", sess.ID, len(html))

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
