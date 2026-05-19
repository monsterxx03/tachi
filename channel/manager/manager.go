package manager

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// Config holds the configuration for creating a Manager.
type Config struct {
	// Cfg is the loaded tachi configuration (providers, web search, MCP, etc.).
	Cfg *config.Config

	// SystemPrompt is the full system prompt used by all agent instances.
	SystemPrompt string

	// ProviderName overrides the default provider from config.
	// If empty, uses the config's default provider.
	ProviderName string

	// ModelName overrides the model. If empty, uses the provider's configured model.
	ModelName string

	// SessionStore overrides the default file-based session store.
	// If nil, sessions are stored under ~/.tachi/session (default).
	// Tests should inject a FileStore backed by a temporary directory.
	SessionStore session.Store
}

// initProviderResult holds the lazily-computed provider state.
type initProviderResult struct {
	provider llm.Provider
	resolved *config.ResolvedConfig
	name     string // Provider config name from config (e.g., "gpt-5.2", "claude")
}

// Manager orchestrates Channel implementations and bridges them to agent instances.
//
// # Responsibilities
//
//   - Channel lifecycle: starts/stops multiple Channel goroutines via Start().
//   - Message processing: on each incoming message, creates an agent, loads
//     or creates a per-thread session, runs one agent turn with auto-confirm
//     semantics, and returns the response.
//
// # Session Model
//
// Each ThreadID maps to a persistent session backed by session.Manager and
// stored on disk under ~/.tachi/session/. The mapping uses the Session.ThreadID
// field for reliable lookup.
//
// # Confirmation Strategy
//
// IM channels are non-interactive:
//   - skip_edit_confirm=true → all EditFile edits auto-approved (no user prompt).
//   - AskUserQuestion tool is unregistered → LLM never uses it in channel mode.
//   - If a confirmation or AskUser event somehow fires, drainEvents handles
//     it gracefully (auto-confirm / auto-reject).
//
// # Concurrency & Steer
//
// Each call to the handler creates a fresh agent instance — no mutable shared
// state between concurrent message processing. The session.Manager provides
// safe per-thread persistence. Multiple threads and multiple channels safely
// interleave.
//
// When a message arrives for a thread that already has an active agent turn,
// it is injected via the steer mechanism: the message is queued and delivered
// to the agent at the next tool-call boundary, allowing the user to refine
// instructions mid-turn without waiting for the current turn to finish.
type Manager struct {
	cfg          *config.Config
	systemPrompt string
	providerName         string
	modelName            string
	currentProviderName  string // Tracks which provider is currently active

	// Lazy-initialized via sync.OnceValues.
	initProviderFn func() (initProviderResult, error)
	provider       llm.Provider
	resolvedConfig *config.ResolvedConfig

	// providerMu protects provider and resolvedConfig during model switching.
	// Both are set once in initProvider() and can be updated by /model command.
	providerMu sync.RWMutex

	// Session store override (nil = use default ~/.tachi/session).
	sessionStore session.Store

	mu       sync.Mutex
	channels []channel.Channel

	// Cron scheduler (only active in channel mode when enabled).
	scheduler *cron.Scheduler

	// verboseState tracks per-thread verbose mode toggled by /v command.
	verboseState map[string]bool
	verboseMu    sync.RWMutex

	// skillStore provides skill listing and activation for /skill command.
	skillStore *skill.Store

	// Per-thread agent activations for steer support.
	threadActMu     sync.Mutex
	threadActivations map[string]*threadActivation

	logger *debuglog.Logger
}

// threadActivation holds the state for an active agent turn on a thread.
// When a new message arrives for a thread that already has a running agent,
// the message is queued in pending and injected via steer.
type threadActivation struct {
	mu          sync.Mutex
	steerRespCh chan string        // agent reads steer input from this
	resultCh    chan handlerResult // agent sends final result here
	pending     []string           // queued steer messages (BC merged)
	ctx         context.Context    // agent context for cancellation
	isCompact   bool               // true when this turn is a /compact operation
}

// handlerResult is the internal result type sent from the agent goroutine
// back to the blocking handler.
type handlerResult struct {
	text        string
	err         error
	attachments []channel.OutgoingAttachment
}

// New creates a Manager.
// Channels are interactive — the iteration budget is always unlimited (0).
func New(mcfg Config) *Manager {
	wd, _ := os.Getwd()
	return &Manager{
		cfg:          mcfg.Cfg,
		systemPrompt: mcfg.SystemPrompt,
		providerName: mcfg.ProviderName,
		modelName:    mcfg.ModelName,
		sessionStore: mcfg.SessionStore,
		skillStore:   skill.NewStore(wd),
		logger:       debuglog.DefaultLogger.WithSource("channel:manager"),
	}
}

// Add registers a Channel. Must be called before Start().
func (m *Manager) Add(ch channel.Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels = append(m.channels, ch)
}

// Start resolves the provider and launches all registered channels in their
// own goroutines. Returns immediately; errors from channel goroutines are
// only logged.
//
// ctx governs the lifetime of all channels — cancelling it triggers graceful
// shutdown.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.initProvider(); err != nil {
		return fmt.Errorf("channel: %w", err)
	}

	// Initialize cron scheduler if enabled.
	if m.cfg != nil && m.cfg.Cron.IsEnabled() {
		if err := m.initCron(ctx); err != nil {
			m.logger.Log("channel: cron init failed: %v", err)
			// Non-fatal: channels can still work without cron.
		}
	}

	m.mu.Lock()
	chans := make([]channel.Channel, len(m.channels))
	copy(chans, m.channels)
	m.mu.Unlock()

	handler := m.buildHandler()
	cmdHandler := m.buildCommandHandler()

	for _, ch := range chans {
		go func(ch channel.Channel) {
			m.logger.Log("channel: %s starting", ch.Name())

			// Inject CommandHandler if this channel supports it.
			if cc, ok := ch.(channel.CommandChannel); ok {
				cc.SetCommandHandler(cmdHandler)
				m.logger.Log("channel: %s received CommandHandler", ch.Name())
			}

			// Lifecycle: OnStart → Run.
			// OnStart gives the channel a chance to initialise before
			// entering its message loop. If it fails, the channel is
			// skipped entirely.
			if err := ch.OnStart(ctx); err != nil {
				m.logger.Log("channel: %s OnStart error: %v", ch.Name(), err)
				return
			}

			if err := ch.Run(ctx, handler); err != nil {
				m.logger.Log("channel: %s exited: %v", ch.Name(), err)
			} else {
				m.logger.Log("channel: %s exited cleanly", ch.Name())
			}
		}(ch)
	}

	// Start cron scheduler after channels are initialized.
	if m.scheduler != nil {
		if err := m.scheduler.Start(ctx); err != nil {
			m.logger.Log("channel: cron scheduler start failed: %v", err)
		}
	}

	return nil
}

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
			// Agent already running — queue this message as steer input.
			ta.pending = append(ta.pending, msg.Content)
			pendingLen := len(ta.pending)
			ta.mu.Unlock()
			m.logger.Log("channel: steer queued thread=%s pending=%d", msg.ThreadID, pendingLen)
			return channel.HandlerResult{Steered: true}
		}

		// First message for this thread — start the agent.
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
			m.deactivateThread(msg.ThreadID)
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
			m.deactivateThread(msg.ThreadID)
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

// activateThread returns the threadActivation for threadID, creating one
// if it doesn't exist. The caller MUST check ta.steerRespCh to determine
// whether the thread is already active (steer case) or new (start case).
func (m *Manager) activateThread(threadID string, ctx context.Context) *threadActivation {
	m.threadActMu.Lock()
	defer m.threadActMu.Unlock()

	if m.threadActivations == nil {
		m.threadActivations = make(map[string]*threadActivation)
	}

	ta, ok := m.threadActivations[threadID]
	if !ok {
		ta = &threadActivation{ctx: ctx}
		m.threadActivations[threadID] = ta
	}
	return ta
}

// deactivateThread removes the thread activation for threadID.
func (m *Manager) deactivateThread(threadID string) {
	m.threadActMu.Lock()
	defer m.threadActMu.Unlock()
	delete(m.threadActivations, threadID)
}

// runAgentTurn creates an agent instance, loads the per-thread session,
// runs the conversation stream with steer support, and delivers the result.
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

	prov, resolved := m.getProvider()

	aiAgent := agent.NewAIAgent(prov, resolved.Provider.Model, 0)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(m.cfg)
	aiAgent.SetupCommitProvider(m.cfg)

	mcpMgr, err := aiAgent.Configure(ctx, m.cfg)
	if err != nil {
		ta.resultCh <- handlerResult{err: fmt.Errorf("configure: %w", err)}
		return
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}

	// Unregister AskUserQuestion — IM channels are non-interactive.
	aiAgent.UnregisterTool(tools.ToolNameAskUser)

	// Register CronTool if scheduler is available.
	if m.scheduler != nil {
		aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string {
			return msg.ThreadID
		}))
	}

	// For /compact turns, clear all tools — the LLM should summarize
	// based on conversation context only, not make new tool calls.
	if ta.isCompact {
		aiAgent.ClearToolRegistry()
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
	userContent := buildUserMessageWithAttachments(msg)

	// --- SendFile tool for file delivery via channel ---
	// The tool is available in channel mode so the LLM can send files
	// to the user (e.g. generated reports, screenshots, documents).
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

	m.verboseMu.RLock()
	verbose := m.verboseState != nil && m.verboseState[msg.ThreadID]
	m.verboseMu.RUnlock()

	text, err := m.drainEventsWithSteer(eventCh, aiAgent, verbose, sendProgress, ta)

	// Collect any pending file attachments from the SendFile tool.
	attachmentMu.Lock()
	attachments := make([]channel.OutgoingAttachment, len(pendingAttachments))
	copy(attachments, pendingAttachments)
	attachmentMu.Unlock()

	ta.resultCh <- handlerResult{text: text, err: err, attachments: attachments}
}

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
	case "model":
		return m.handleModelCommand(cmd.Args)
	case "skill":
		return m.handleSkillCommand(cmd.Args)
	default:
		m.logger.Log("channel: unknown command via CommandHandler: %s", cmd.Name)
		return fmt.Sprintf("Unknown command: %s. Available: new, mcp, usage, cron, v, model, skill, compact", cmd.Name), nil
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
		return fmt.Sprintf("Unknown command: %s\n\nAvailable commands in channel mode:\n  /new — Start a new conversation\n  /mcp — List configured MCP servers\n  /model — List or switch provider/model\n  /skill — List or activate skills\n  /usage — Show session usage stats\n  /compact — Compress conversation history\n  /cron — List cron jobs\n  /v — Toggle verbose tool call output", cmd), nil
	}
}

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
func (m *Manager) handleNewCommand(threadID string) (string, error) {
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

// drainEvents consumes all AgentEvents, returning the final assistant text or
// an error. Because we control the agent instance, we can respond to any
// confirmation/AskUser events inline — though with skip_edit_confirm=true
// and AskUser unregistered, neither should appear.
//
// When verbose is true, tool call results are sent immediately via
// sendProgress as they arrive, instead of being collected for a single
// summary prefix.
func (m *Manager) drainEvents(ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent, verbose bool, sendProgress func(string)) (string, error) {
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
			if verbose {
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

		case agent.AgentEventToolResult:
			if event.ToolIsError {
				m.logger.Log("channel: tool %s error: %s", event.ToolName, event.ToolResult)
				if verbose {
					line := "  ❌ Error: " + truncateToolResult(event.ToolResult)
					if event.ToolDuration > 0 {
						line += " " + formatToolDuration(event.ToolDuration)
					}
					callLine, ok := pendingToolCalls[event.ToolID]
					if ok {
						sendProgress(callLine + "\n" + line)
						delete(pendingToolCalls, event.ToolID)
					} else {
						sendProgress("🔧 " + event.ToolName + "\n" + line)
					}
				}
			} else {
				m.logger.Log("channel: tool %s ok (%d bytes)", event.ToolName, len(event.ToolResult))
				if verbose {
					line := "  ✅ " + summarizeToolResult(event.ToolName, event.ToolResult)
					if event.ToolDuration > 0 {
						line += " " + formatToolDuration(event.ToolDuration)
					}
					callLine, ok := pendingToolCalls[event.ToolID]
					if ok {
						sendProgress(callLine + "\n" + line)
						delete(pendingToolCalls, event.ToolID)
					} else {
						sendProgress("🔧 " + event.ToolName + "\n" + line)
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

// drainEventsWithSteer is like drainEvents but also handles AgentEventSteerCheck
// for mid-turn user input injection (steer mechanism). When the agent reaches a
// tool-call boundary and requests steer input, we drain any queued pending
// messages and deliver them to the agent.
func (m *Manager) drainEventsWithSteer(ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent, verbose bool, sendProgress func(string), ta *threadActivation) (string, error) {
	var text strings.Builder
	var lastErr error

	var pendingToolCalls map[string]string

	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			text.WriteString(event.TextDelta)

		case agent.AgentEventThinkingDelta:
			// Thinking is internal to the agent; we don't expose it to IM.

		case agent.AgentEventToolCallStart:
			m.logger.Log("channel: tool call start: %s", event.ToolName)

		case agent.AgentEventToolCallArgs:
			m.logger.Log("channel: tool call args for %s: %s", event.ToolName, event.ToolArgs)
			if verbose {
				if pendingToolCalls == nil {
					pendingToolCalls = make(map[string]string)
				}
				pendingToolCalls[event.ToolID] = "🔧 " + summarizeToolCall(event.ToolName, event.ToolArgs)
			}

		case agent.AgentEventToolConfirmation:
			m.logger.Log("channel: auto-approving unexpected confirmation: %s", event.ToolName)
			aiAgent.ConfirmTool(true)

		case agent.AgentEventAskUser:
			m.logger.Log("channel: auto-rejecting unexpected AskUser")
			aiAgent.RespondToAskUser(nil, nil)

		case agent.AgentEventSteerCheck:
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
				if verbose {
					line := "  ❌ Error: " + truncateToolResult(event.ToolResult)
					if event.ToolDuration > 0 {
						line += " " + formatToolDuration(event.ToolDuration)
					}
					callLine, ok := pendingToolCalls[event.ToolID]
					if ok {
						sendProgress(callLine + "\n" + line)
						delete(pendingToolCalls, event.ToolID)
					} else {
						sendProgress("🔧 " + event.ToolName + "\n" + line)
					}
				}
			} else {
				m.logger.Log("channel: tool %s ok (%d bytes)", event.ToolName, len(event.ToolResult))
				if verbose {
					line := "  ✅ " + summarizeToolResult(event.ToolName, event.ToolResult)
					if event.ToolDuration > 0 {
						line += " " + formatToolDuration(event.ToolDuration)
					}
					callLine, ok := pendingToolCalls[event.ToolID]
					if ok {
						sendProgress(callLine + "\n" + line)
						delete(pendingToolCalls, event.ToolID)
					} else {
						sendProgress("🔧 " + event.ToolName + "\n" + line)
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
	if result == "" && lastErr == nil {
		return "", nil
	}
	return result, nil
}

// --- Provider resolution ---

// getProvider returns the current provider and resolved config under read lock.
// Use this in agent turn paths to safely read the provider state that may be
// updated by the /model command.
func (m *Manager) getProvider() (llm.Provider, *config.ResolvedConfig) {
	m.providerMu.RLock()
	defer m.providerMu.RUnlock()
	return m.provider, m.resolvedConfig
}

func (m *Manager) initProvider() error {
	if m.initProviderFn == nil {
		m.initProviderFn = sync.OnceValues(func() (initProviderResult, error) {
			flags := config.CLIFlags{}
			if m.providerName != "" {
				flags.Provider = m.providerName
				flags.ProviderSet = true
			}
			if m.modelName != "" {
				flags.Model = m.modelName
				flags.ModelSet = true
			}

			resolved, err := config.Resolve(m.cfg, flags)
			if err != nil {
				return initProviderResult{}, fmt.Errorf("resolve config: %w", err)
			}

			provider, err := llm.NewProvider(
				resolved.Provider.Type,
				resolved.Provider.APIKey,
				resolved.Provider.BaseURL,
				resolved.Provider.Model,
			)
			if err != nil {
				return initProviderResult{}, fmt.Errorf("create provider: %w", err)
			}

			// Capture the resolved provider name for /model display.
			name := resolved.Provider.Name

			return initProviderResult{provider: provider, resolved: resolved, name: name}, nil
		})
	}
	result, err := m.initProviderFn()
	if err != nil {
		return err
	}
	m.providerMu.Lock()
	m.provider = result.provider
	m.resolvedConfig = result.resolved
	m.currentProviderName = result.name
	m.providerMu.Unlock()
	return nil
}

// newSessionManager creates a session manager backed by m.sessionStore
// (if set) or the default ~/.tachi/session directory.
func (m *Manager) newSessionManager() *session.Manager {
	var sm *session.Manager
	if m.sessionStore != nil {
		sm = session.NewManagerWithStore(m.sessionStore)
	} else {
		var err error
		sm, err = session.NewManager()
		if err != nil {
			m.logger.Log("channel: session manager fallback failed: %v", err)
			return sm
		}
	}
	if m.cfg != nil {
		sm.SetMaxKeep(m.cfg.SessionCleanupMaxCount)
	}
	return sm
}

// --- Session helpers ---

// loadThreadSession looks up a session by ThreadID (via session.ThreadID field).
// If found, returns the session manager loaded with that session and the
// converted LLM message history. If not found, creates a new session manager
// with a fresh session and returns nil history.
func (m *Manager) loadThreadSession(threadID string) (*session.Manager, []llm.Message, error) {
	var sm *session.Manager
	if m.sessionStore != nil {
		sm = session.NewManagerWithStore(m.sessionStore)
	} else {
		var err error
		sm, err = session.NewManager()
		if err != nil {
			return nil, nil, fmt.Errorf("session manager: %w", err)
		}
	}

	_, resolved := m.getProvider()

	// Try to find an existing session for this ThreadID.
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		// Non-fatal — we'll start a fresh session.
		m.logger.Log("channel: find session for %s: %v", threadID, err)
		return sm, nil, nil
	}

	if sess == nil {
		// No existing session → create a new one now. The agent will
		// record the first message.
		wd, _ := os.Getwd()
		if _, err := sm.New(resolved.Provider.Type, resolved.Provider.Model, wd); err != nil {
			return sm, nil, fmt.Errorf("create session: %w", err)
		}
		if err := sm.SetThreadID(threadID); err != nil {
			m.logger.Log("channel: set thread_id for %s: %v", threadID, err)
		}
		return sm, nil, nil
	}

	// Existing session — convert its messages to LLM format for history.
	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		return sm, nil, fmt.Errorf("load messages: %w", err)
	}

	if len(sessionMsgs) == 0 {
		return sm, nil, nil
	}

	llmMsgs, err := agent.ConvertSessionToLLMMessages(sessionMsgs, resolved.Provider.Type)
	if err != nil {
		return sm, nil, fmt.Errorf("convert messages: %w", err)
	}

	m.logger.Log("channel: session %s thread=%s: %d session msgs → %d llm msgs",
		sess.ID, threadID, len(sessionMsgs), len(llmMsgs))

	return sm, llmMsgs, nil
}

// --- Cron Infrastructure ---

// initCron creates the cron store and scheduler with the manager as the
// trigger handler. Must be called before Start() fires channels.
func (m *Manager) initCron(_ context.Context) error {
	storePath := m.cfg.Cron.StorePath
	if storePath == "" {
		storePath = cron.DefaultStorePath()
	}

	store := cron.NewStore(storePath)
	scheduler := cron.NewScheduler(cron.SchedulerConfig{
		Store:            store,
		Handler:          m.OnCronTrigger,
		Logger:           m.logger,
		MaxConcurrent:    m.cfg.Cron.MaxConcurrent,
		ExecutionTimeout: m.cfg.Cron.ExecutionTimeout,
	})

	m.scheduler = scheduler
	m.logger.Log("channel: cron initialized (path=%s, max_concurrent=%d, timeout=%v)",
		storePath, m.cfg.Cron.MaxConcurrent, m.cfg.Cron.ExecutionTimeout)
	return nil
}

// OnCronTrigger is the TriggerHandler callback invoked by the cron scheduler
// when a job fires. It simulates an incoming message from the cron system:
// builds an agent with the job's prompt as the user message, runs the agent
// turn, and delivers the response to the target thread's channel.
func (m *Manager) OnCronTrigger(ctx context.Context, job *cron.Job) error {
	m.logger.Log("channel: cron trigger job=%s (%s) thread=%s", job.ID, job.Name, job.TargetThreadID)

	prov, resolved := m.getProvider()
	if prov == nil || resolved == nil {
		return fmt.Errorf("channel: provider not initialized for cron trigger")
	}

	aiAgent := agent.NewAIAgent(prov, resolved.Provider.Model, 0)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(m.cfg)
	aiAgent.SetupCommitProvider(m.cfg)

	mcpMgr, err := aiAgent.Configure(ctx, m.cfg)
	if err != nil {
		return fmt.Errorf("cron: configure agent: %w", err)
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}
	aiAgent.UnregisterTool(tools.ToolNameAskUser)

	// Register CronTool so cron jobs can manage themselves.
	aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string {
		return job.TargetThreadID
	}))

	// Load/create session for the target thread.
	sm, priorHistory, err := m.loadThreadSession(job.TargetThreadID)
	if err != nil {
		m.logger.Log("channel: cron session for %s: %v", job.TargetThreadID, err)
		sm = m.newSessionManager()
		priorHistory = nil
	}

	if sm != nil && !sm.HasCurrent() {
		wd, _ := os.Getwd()
		if _, err := sm.New(resolved.Provider.Type, resolved.Provider.Model, wd); err != nil {
			m.logger.Log("channel: cron create session: %v", err)
		} else {
			sm.SetThreadID(job.TargetThreadID)
		}
	}

	if sm != nil {
		aiAgent.SetSessionManager(sm)
	}

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, job.Prompt, m.systemPrompt, llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})

	m.verboseMu.RLock()
	verbose := m.verboseState != nil && m.verboseState[job.TargetThreadID]
	m.verboseMu.RUnlock()

	// sendProgress for cron: deliver intermediate tool results inline.
	sendProgress := func(text string) {
		m.sendToThread(ctx, job.TargetThreadID, text, fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()))
	}

	result, err := m.drainEvents(eventCh, aiAgent, verbose, sendProgress)
	if err != nil {
		m.logger.Log("channel: cron job %s drain error: %v", job.ID, err)
		return err
	}

	// Deliver the response to the target thread's channel.
	if result != "" {
		m.deliverCronResponse(ctx, channel.OutgoingMessage{
			ThreadID: job.TargetThreadID,
			Content:  result,
			ReplyTo:  fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()),
		})
	}

	return nil
}

// deliverCronResponse sends a cron-triggered response to the channel
// responsible for the given ThreadID. It iterates all registered channels
// and tries each one that implements MessageSender.
func (m *Manager) deliverCronResponse(ctx context.Context, msg channel.OutgoingMessage) {
	m.mu.Lock()
	chans := make([]channel.Channel, len(m.channels))
	copy(chans, m.channels)
	m.mu.Unlock()

	for _, ch := range chans {
		sender, ok := ch.(channel.MessageSender)
		if !ok {
			continue
		}
		if err := sender.Send(ctx, msg); err != nil {
			m.logger.Log("channel: cron send to %s failed: %v", ch.Name(), err)
		} else {
			m.logger.Log("channel: cron response delivered to %s (thread=%s)", ch.Name(), msg.ThreadID)
			return
		}
	}

	m.logger.Log("channel: cron response not delivered — no channel accepted thread %s", msg.ThreadID)
}

// buildUserMessageWithAttachments constructs the user message text sent to
// the LLM, prepending any file/attachment content before the user's own text.
//
// For text files the content is included inline (good for quick context).
// For all files (including text ones), the local SavedPath is also provided
// so the LLM can use the Bash tool to read/parse the file directly — useful
// for PDFs (pdftotext), Excel (openpyxl), archives, or any format that needs
// programmatic extraction.
func buildUserMessageWithAttachments(msg channel.IncomingMessage) string {
	if len(msg.Attachments) == 0 {
		return msg.Content
	}

	var parts []string

	for _, att := range msg.Attachments {
		if att.Error != "" {
			parts = append(parts, fmt.Sprintf("[文件: %s (下载失败: %s)]", att.FileName, att.Error))
			continue
		}

		switch att.Type {
		case channel.AttachmentTypeText, channel.AttachmentTypeFile:
			if att.TextContent != "" {
				// Text content included inline.
				fileHeader := fmt.Sprintf("[文件: %s]", att.FileName)
				if att.SavedPath != "" {
					fileHeader = fmt.Sprintf("[文件: %s (已保存到 %s)]", att.FileName, att.SavedPath)
				}
				parts = append(parts, fmt.Sprintf("%s\n```\n%s\n```", fileHeader, att.TextContent))
			} else if att.SavedPath != "" {
				// Binary file saved to disk — tell the LLM the path and
				// let it use Bash tools (pdftotext, python, etc.) to parse it.
				parts = append(parts, fmt.Sprintf(
					"[文件: %s (%s, %s)]\n文件已保存到本地: %s\n你可以使用 Bash 工具来解析这个文件（例如 pdftotext 解析 PDF、python 解析 Excel 等）。",
					att.FileName, att.MimeType, humanSize(int(att.Size)), att.SavedPath))
			} else {
				parts = append(parts, fmt.Sprintf("[文件: %s (%s, %s)]",
					att.FileName, att.MimeType, humanSize(int(att.Size))))
			}

		case channel.AttachmentTypeImage:
			imgMsg := fmt.Sprintf("[图片: %s (%s)]", att.FileName, humanSize(int(att.Size)))
			if att.SavedPath != "" {
				imgMsg = fmt.Sprintf("[图片: %s (已保存到 %s, %s)]", att.FileName, att.SavedPath, humanSize(int(att.Size)))
			}
			parts = append(parts, imgMsg)
		}
	}

	if msg.Content != "" {
		parts = append(parts, msg.Content)
	}

	return strings.Join(parts, "\n\n")
}
// ThreadID. Used for intermediate progress messages in verbose mode.
// This is best-effort — failures are logged but not propagated.
func (m *Manager) sendToThread(ctx context.Context, threadID, text, replyTo string) {
	m.mu.Lock()
	chans := make([]channel.Channel, len(m.channels))
	copy(chans, m.channels)
	m.mu.Unlock()

	for _, ch := range chans {
		sender, ok := ch.(channel.MessageSender)
		if !ok {
			continue
		}
		if err := sender.Send(ctx, channel.OutgoingMessage{
			ThreadID: threadID,
			Content:  text,
			ReplyTo:  replyTo,
		}); err != nil {
			m.logger.Log("channel: sendToThread to %s failed: %v", ch.Name(), err)
			return
		}
		m.logger.Log("channel: progress sent to %s (thread=%s)", ch.Name(), threadID)
		return
	}
	m.logger.Log("channel: sendToThread — no channel accepted thread %s", threadID)
}

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

// handleSkillCommand dispatches /skill sub-commands:
//   /skill or /skill list  → list skills
//   /skill reload          → re-scan skill directories
//   /skill <name>          → handled via agent turn (not via this method)
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

// zipFile creates an in-memory ZIP archive containing a single file.
func zipFile(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(name)
	if err != nil {
		return nil, fmt.Errorf("create zip entry: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return nil, fmt.Errorf("write zip entry: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}
	return buf.Bytes(), nil
}

// sanitizeFilename replaces characters that are problematic in filenames.
func sanitizeFilename(s string) string {
	if s == "" {
		return ""
	}
	// Replace problematic chars with underscore.
	replacer := strings.NewReplacer(
		" ", "_",
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	result := replacer.Replace(s)
	// Trim to reasonable length.
	if len(result) > 60 {
		result = result[:60]
	}
	return result
}

// --- Tool call summary helpers (used by drainEvents in verbose mode) ---

// summarizeToolCall produces a one-line summary of a tool invocation.
func summarizeToolCall(name, args string) string {
	summary := summarizeToolArgs(name, args)
	if summary == "" {
		return name
	}
	return name + "(" + summary + ")"
}

// summarizeToolArgs extracts the most informative fields from tool call JSON.
func summarizeToolArgs(name, args string) string {
	switch name {
	case tools.ToolNameRead:
		var p struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		_ = json.Unmarshal([]byte(args), &p)
		if p.Path == "" {
			return ""
		}
		if p.Offset > 0 && p.Limit > 0 {
			return fmt.Sprintf("%s L%d+%d", p.Path, p.Offset, p.Limit)
		}
		if p.Offset > 0 {
			return fmt.Sprintf("%s L%d", p.Path, p.Offset)
		}
		if p.Limit > 0 {
			return fmt.Sprintf("%s +%d", p.Path, p.Limit)
		}
		return p.Path

	case tools.ToolNameBash:
		var p struct{ Command string `json:"command"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.Command, 60)

	case tools.ToolNameWrite, tools.ToolNameEdit:
		var p struct{ Path string `json:"path"` }
		_ = json.Unmarshal([]byte(args), &p)
		return p.Path

	case tools.ToolNameGrep:
		var p struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
		}
		_ = json.Unmarshal([]byte(args), &p)
		if p.Path != "" && p.Pattern != "" {
			return p.Path + " " + truncateForDisplay(p.Pattern, 30)
		}
		if p.Pattern != "" {
			return truncateForDisplay(p.Pattern, 40)
		}
		return p.Path

	case tools.ToolNameWebSearch:
		var p struct{ Query string `json:"query"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.Query, 40)

	case tools.ToolNameWebFetch:
		var p struct{ URL string `json:"url"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.URL, 50)

	case tools.ToolNameGlob:
		var p struct{ Pattern string `json:"pattern"` }
		_ = json.Unmarshal([]byte(args), &p)
		return p.Pattern

	case tools.ToolNameSubAgent:
		var p struct{ Prompt string `json:"prompt"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.Prompt, 60)

	default:
		return truncateForDisplay(args, 60)
	}
}

// summarizeToolResult produces a one-line summary of a tool execution result.
func summarizeToolResult(name, result string) string {
	lineCount := strings.Count(result, "\n") + 1
	byteLen := len(result)

	switch name {
	case tools.ToolNameRead:
		return fmt.Sprintf("读取 %d 行", lineCount)
	case tools.ToolNameWrite:
		return "写入完成"
	case tools.ToolNameEdit:
		return "编辑完成"
	case tools.ToolNameBash:
		if byteLen <= 200 {
			return result
		}
		return fmt.Sprintf("输出 %d 行 (%s)", lineCount, humanSize(byteLen))
	case tools.ToolNameGrep:
		return fmt.Sprintf("匹配 %d 行", lineCount)
	case tools.ToolNameGlob:
		return fmt.Sprintf("匹配 %d 个文件", lineCount)
	case tools.ToolNameWebSearch:
		return "搜索完成"
	case tools.ToolNameWebFetch:
		return fmt.Sprintf("抓取完成 (%s)", humanSize(byteLen))
	default:
		if byteLen <= 200 {
			return result
		}
		return fmt.Sprintf("%d 行 (%s)", lineCount, humanSize(byteLen))
	}
}

// truncateToolResult limits an error string for display.
func truncateToolResult(s string) string {
	if len(s) <= 150 {
		return s
	}
	return s[:150] + "..."
}

// humanSize formats a byte count as a human-readable string.
func humanSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}

// truncateForDisplay limits a string for display in channel messages.
func truncateForDisplay(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// formatToolDuration formats a time.Duration as a concise human-readable string
// for channel display of tool execution results.
func formatToolDuration(d time.Duration) string {
	if d < time.Microsecond {
		return "(<1µs)"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("(%dµs)", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("(%.0fms)", float64(d.Microseconds())/1000)
	}
	if d < time.Minute {
		return fmt.Sprintf("(%.1fs)", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := d.Seconds() - float64(minutes*60)
	return fmt.Sprintf("(%dm%.0fs)", minutes, seconds)
}
