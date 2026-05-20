package manager

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent"
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
		return m.executeSlashCommand(cmd)
	}
}

// executeSlashCommand dispatches a SlashCommand to the appropriate handler.
func (m *Manager) executeSlashCommand(cmd channel.SlashCommand) (string, error) {
	switch cmd.Name {
	case "new":
		return m.handleNewCommand(cmd.ThreadID)
	case "mcp":
		return m.handleMCPList()
	case "usage":
		return m.handleUsageCommand(cmd.ThreadID)
	case "cron":
		return m.handleCronCommand()
	case "v":
		return m.handleVerboseCommand(cmd.ThreadID)
	case "stop":
		return m.handleStopCommand(cmd.ThreadID)
	case "model":
		return m.handleModelCommand(cmd.Args)
	case "skill":
		return m.handleSkillCommand(cmd.Args)
	default:
		m.logger.Log("channel: unknown command via CommandHandler: %s", cmd.Name)
		return fmt.Sprintf("Unknown command: %s. Available: new, mcp, usage, cron, v, stop, model, skill, compact", cmd.Name), nil
	}
}

// handleSlashCommand dispatches message starting with "/" to the appropriate
// handler. Returns the response text for the channel to send back.
func (m *Manager) handleSlashCommand(msg channel.IncomingMessage) (string, error) {
	parts := strings.Fields(msg.Content)
	if len(parts) == 0 {
		return "", nil
	}
	cmd := parts[0]

	switch cmd {
	case "/new":
		return m.handleNewCommand(msg.ThreadID)
	case "/mcp":
		return m.handleMCPList()
	case "/usage":
		return m.handleUsageCommand(msg.ThreadID)
	case "/cron":
		return m.handleCronCommand()
	case "/v":
		return m.handleVerboseCommand(msg.ThreadID)
	case "/stop":
		return m.handleStopCommand(msg.ThreadID)
	case "/model":
		args := ""
		if len(parts) > 1 {
			args = strings.Join(parts[1:], " ")
		}
		return m.handleModelCommand(args)
	case "/skill":
		args := ""
		if len(parts) > 1 {
			args = strings.Join(parts[1:], " ")
		}
		return m.handleSkillCommand(args)
	default:
		m.logger.Log("channel: unknown slash command from thread %s: %s", msg.ThreadID, cmd)
		return fmt.Sprintf("Unknown command: %s\n\nAvailable commands in channel mode:\n  /new — Start a new conversation\n  /mcp — List configured MCP servers\n  /model — List or switch provider/model\n  /skill — List or activate skills\n  /usage — Show session usage stats\n  /compact — Compress conversation history\n  /cron — List cron jobs\n  /v — Toggle verbose tool call output\n  /stop — Stop the current LLM turn", cmd), nil
	}
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

	m.logger.Log("channel: /model switched to %s (%s/%s)", name, resolved.Type, resolved.Model)

	return fmt.Sprintf("✅ Switched to **%s** (%s, %s).\nNew conversations will use this model.", name, resolved.Type, resolved.Model), nil
}

// --- /new ---

func (m *Manager) handleNewCommand(threadID string) (string, error) {
	// Cancel any running agent turn first, so the next message
	// starts a fresh conversation rather than being steered
	// into the old turn.
	m.cancelThreadTurn(threadID)

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

// handleMCPList returns a formatted list of configured MCP servers.
func (m *Manager) handleMCPList() (string, error) {
	servers := m.cfg.MCPServers
	if len(servers) == 0 {
		return "No MCP servers configured.", nil
	}

	var sb strings.Builder
	sb.WriteString("MCP Servers:\n")

	for _, srv := range servers {
		enabled := srv.IsEnabled()
		status := "Disabled"
		if enabled {
			status = "Enabled"
		}

		transport := "?"
		switch srv.Type {
		case config.MCPTransportStdio:
			transport = fmt.Sprintf("stdio (%s)", srv.Command)
		case config.MCPTransportHTTP:
			transport = fmt.Sprintf("http (%s)", srv.URL)
		}

		fmt.Fprintf(&sb, "\n- %s [%s]\n  Transport: %s\n", srv.Name, status, transport)
		if srv.HasOAuth() {
			sb.WriteString("  OAuth: configured\n")
		}
	}

	return sb.String(), nil
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

	// Resolve price
	var price *llm.ModelPrice
	_, resolved := m.getProvider()
	if resolved != nil {
		model := resolved.Provider.Model
		pCfg := m.cfg.FindProvider(resolved.Provider.Name)
		if pCfg != nil {
			price = llm.ResolveModelPrice(model, pCfg.InputPrice, pCfg.OutputPrice, pCfg.CacheReadInputPrice, pCfg.CacheCreationInputPrice)
		}
		if price == nil {
			price = llm.ResolveModelPrice(model, nil, nil, nil, nil)
		}
	}

	report, err := agent.ComputeSessionUsage(sm, price, 0)
	if err != nil {
		return fmt.Sprintf("Failed to compute usage: %v", err), nil
	}

	var sb strings.Builder
	sb.WriteString("📊 Session Usage\n\n")
	sb.WriteString(fmt.Sprintf("Session: %s\n", report.Session.ID))
	title := report.Session.Title
	if title == "" {
		title = "(untitled)"
	}
	sb.WriteString(fmt.Sprintf("Title: %s\n", title))
	sb.WriteString(fmt.Sprintf("Provider: %s | Model: %s\n\n", report.Session.Provider, report.Session.Model))

	u := report.Usage
	sb.WriteString("Token Usage:\n")
	sb.WriteString(fmt.Sprintf("  Input:  %d\n", u.InputTokens))
	if u.CacheReadInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  Cache read: %d\n", u.CacheReadInputTokens))
	}
	if u.CacheCreationInputTokens > 0 {
		sb.WriteString(fmt.Sprintf("  Cache created: %d\n", u.CacheCreationInputTokens))
	}
	sb.WriteString(fmt.Sprintf("  Output: %d\n", u.OutputTokens))
	sb.WriteString(fmt.Sprintf("  Total:  %d\n\n", u.InputTokens+u.OutputTokens))

	if report.Cost > 0 {
		sb.WriteString(fmt.Sprintf("Cost: ¥%.4f\n\n", report.Cost))
	}

	sb.WriteString("Tool Calls:\n")
	names := slices.Sorted(maps.Keys(report.ToolCalls))
	for _, name := range names {
		st := report.ToolCalls[name]
		line := fmt.Sprintf("  %s: %d", name, st.Count)
		if st.ErrCount > 0 {
			line += fmt.Sprintf(" (%d failed)", st.ErrCount)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString(fmt.Sprintf("\nTotal: %d main + %d subagent = %d call(s)",
		report.MainCount, report.SubCount, report.MainCount+report.SubCount))

	return sb.String(), nil
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
	if len(metas) == 0 {
		return "没有可用的 Skill。\n\n在 `.tachi/skills/<name>/` 或 `~/.tachi/skills/<name>/` 下创建 `SKILL.md` 即可添加。", nil
	}

	var sb strings.Builder
	sb.WriteString("**可用 Skills:**\n\n")

	for _, meta := range metas {
		sourceTag := ""
		if meta.Source == "project" {
			sourceTag = " 🏠"
		}
		sb.WriteString(fmt.Sprintf("- **%s**%s\n", meta.Name, sourceTag))
		sb.WriteString(fmt.Sprintf("  %s\n", meta.Description))
		if len(meta.Tags) > 0 {
			sb.WriteString(fmt.Sprintf("  标签: %s\n", strings.Join(meta.Tags, ", ")))
		}
		sb.WriteString(fmt.Sprintf("  使用 `/ %s` 或 `/skill %s` 激活\n\n", meta.Name, meta.Name))
	}
	sb.WriteString(fmt.Sprintf("%d 个 skill(s)", len(metas)))

	return sb.String(), nil
}

// handleSkillReload re-scans skill directories and returns the updated count.
func (m *Manager) handleSkillReload() (string, error) {
	if m.skillStore == nil {
		return "Skill system not available.", nil
	}

	wd, _ := os.Getwd()
	m.skillStore = skill.NewStore(wd)
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
// Usage in channel:
//
//	/transcript          — transcript for current thread's session
//	/transcript --latest — transcript for the most recent session
func (m *Manager) handleTranscriptCommand(msg channel.IncomingMessage) channel.HandlerResult {
	errReply := func(err error) channel.HandlerResult {
		return channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  fmt.Sprintf("❌ %v", err),
				ReplyTo:  msg.MessageID,
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

	parts := strings.Fields(msg.Content)
	useLatest := len(parts) > 1 && parts[1] == "--latest"

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
		found, findErr := sm.FindByThreadID(msg.ThreadID)
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
	zipFileName := fmt.Sprintf("%s-%s-transcript.zip", fileName, sess.ID[:8])

	m.logger.Log("channel: transcript generated for session %s (%d bytes)", sess.ID, len(html))

	// Compress the HTML into a zip archive so WeChat (and other IM
	// platforms that block .html files for security reasons) can
	// deliver it as a regular file attachment. The user can extract
	// and open the HTML in any browser.
	zipData, err := zipFile(htmlFileName, []byte(html))
	if err != nil {
		return errReply(fmt.Errorf("compress transcript: %w", err))
	}

	contentText := fmt.Sprintf("📊 Transcript: %s\n\nSession: %s\nTurns: %d · Tools: %d · HTML: %s · Zip: %s",
		sess.Title, sess.ID[:8],
		data.Stats.TurnCount, data.Stats.ToolCallCount, humanSize(len(html)), humanSize(len(zipData)))

	return channel.HandlerResult{
		Reply: channel.OutgoingMessage{
			ThreadID: msg.ThreadID,
			Content:  contentText,
			Attachments: []channel.OutgoingAttachment{
				{
					Type:     channel.AttachmentTypeFile,
					FileName: zipFileName,
					MimeType: "application/zip",
					Data:     zipData,
				},
			},
			ReplyTo: msg.MessageID,
		},
	}
}
