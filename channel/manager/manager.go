package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
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
// # Concurrency
//
// Each call to the handler creates a fresh agent instance — no mutable shared
// state between concurrent message processing. The session.Manager provides
// safe per-thread persistence. Multiple threads and multiple channels safely
// interleave.
type Manager struct {
	cfg          *config.Config
	systemPrompt string
	providerName string
	modelName    string

	// Lazy-initialized.
	initOnce       sync.Once
	initErr        error
	provider       llm.Provider
	resolvedConfig *config.ResolvedConfig

	// Session store override (nil = use default ~/.tachi/session).
	sessionStore session.Store

	mu       sync.Mutex
	channels []channel.Channel

	// Cron scheduler (only active in channel mode when enabled).
	scheduler *cron.Scheduler

	// verboseState tracks per-thread verbose mode toggled by /v command.
	verboseState map[string]bool
	verboseMu    sync.RWMutex

	logger *debuglog.Logger
}

// New creates a Manager.
// Channels are interactive — the iteration budget is always unlimited (0).
func New(mcfg Config) *Manager {
	return &Manager{
		cfg:          mcfg.Cfg,
		systemPrompt: mcfg.SystemPrompt,
		providerName: mcfg.ProviderName,
		modelName:    mcfg.ModelName,
		sessionStore: mcfg.SessionStore,
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

	for _, ch := range chans {
		go func(ch channel.Channel) {
			m.logger.Log("channel: %s starting", ch.Name())

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
// message through a fresh agent instance.
func (m *Manager) buildHandler() channel.MessageHandler {
	return func(ctx context.Context, msg channel.IncomingMessage) (channel.OutgoingMessage, error) {
		m.logger.Log("channel: recv thread=%s id=%s len=%d",
			msg.ThreadID, msg.MessageID, len(msg.Content))

		// sendProgress pushes an intermediate progress message to the
		// same thread. Only effective in verbose mode.
		sendProgress := func(text string) {
			m.sendToThread(ctx, msg.ThreadID, text, msg.MessageID)
		}

		result, err := m.process(ctx, msg, sendProgress)
		if err != nil {
			return channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  fmt.Sprintf("❌ %v", err),
				ReplyTo:  msg.MessageID,
			}, err
		}
		return channel.OutgoingMessage{
			ThreadID: msg.ThreadID,
			Content:  result,
			ReplyTo:  msg.MessageID,
		}, nil
	}
}

// process builds an agent, sets up a per-thread session, runs the conversation
// with auto-confirm, and returns the response text.
//
// Slash commands (messages starting with "/") are intercepted and handled
// directly without invoking the LLM. Currently supported: /new, /mcp.
//
// sendProgress is called to push intermediate progress messages (tool call
// results) to the channel during verbose mode. It is a no-op when the channel
// does not support proactive send or when verbose is off.
func (m *Manager) process(ctx context.Context, msg channel.IncomingMessage, sendProgress func(string)) (string, error) {
	// --- slash command interception ---
	if strings.HasPrefix(msg.Content, "/") {
		return m.handleSlashCommand(msg)
	}

	if m.resolvedConfig == nil || m.provider == nil {
		return "", fmt.Errorf("channel manager not initialized; call Start() first")
	}

	aiAgent := agent.NewAIAgent(m.provider, m.resolvedConfig.Provider.Model, 0)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.SetContextWindow(m.resolvedConfig.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(m.cfg)
	aiAgent.SetupCommitProvider(m.cfg)

	mcpMgr, err := aiAgent.Configure(ctx, m.cfg)
	if err != nil {
		return "", fmt.Errorf("configure: %w", err)
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

	// Per-thread session.
	sm, priorHistory, err := m.loadThreadSession(msg.ThreadID)
	if err != nil {
		m.logger.Log("channel: session setup for thread %s: %v", msg.ThreadID, err)
		// Continue anyway with a fresh session manager and no history.
		sm = m.newSessionManager()
		priorHistory = nil
	}

	// Ensure a session exists for recording. If loadThreadSession created
	// one, the agent's RunConversationStream will use it. If it failed,
	// create a session here so the agent can still record.
	if sm != nil && !sm.HasCurrent() {
		wd, _ := os.Getwd()
		if _, err := sm.New(m.resolvedConfig.Provider.Type, m.resolvedConfig.Provider.Model, wd); err != nil {
			m.logger.Log("channel: create fallback session: %v", err)
		} else {
			sm.SetThreadID(msg.ThreadID)
		}
	}

	if sm != nil {
		aiAgent.SetSessionManager(sm)
	}

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, msg.Content, m.systemPrompt, llm.ChatOptions{
		MaxTokens: m.resolvedConfig.MaxTokens,
	})

	m.verboseMu.RLock()
	verbose := m.verboseState != nil && m.verboseState[msg.ThreadID]
	m.verboseMu.RUnlock()

	return m.drainEvents(eventCh, aiAgent, verbose, sendProgress)
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
	default:
		m.logger.Log("channel: unknown slash command from thread %s: %s", msg.ThreadID, cmd)
		return fmt.Sprintf("Unknown command: %s\n\nAvailable commands in channel mode:\n  /new — Start a new conversation\n  /mcp — List configured MCP servers\n  /usage — Show session usage stats\n  /cron — List cron jobs\n  /v — Toggle verbose tool call output", cmd), nil
	}
}

// handleNewCommand ends the current session for the given ThreadID so the
// next message starts a fresh conversation.
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
	if m.resolvedConfig != nil {
		model := m.resolvedConfig.Provider.Model
		if m.cfg != nil && m.cfg.Provider != "" {
			pCfg := m.cfg.FindProvider(m.cfg.Provider)
			if pCfg != nil {
				price = llm.ResolveModelPrice(model, pCfg.InputPrice, pCfg.OutputPrice, pCfg.CacheReadInputPrice, pCfg.CacheCreationInputPrice)
			}
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
	names := make([]string, 0, len(report.ToolCalls))
	for name := range report.ToolCalls {
		names = append(names, name)
	}
	sort.Strings(names)
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

// --- Provider resolution ---

func (m *Manager) initProvider() error {
	m.initOnce.Do(func() {
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
			m.initErr = fmt.Errorf("resolve config: %w", err)
			return
		}

		provider, err := llm.NewProvider(
			resolved.Provider.Type,
			resolved.Provider.APIKey,
			resolved.Provider.BaseURL,
			resolved.Provider.Model,
		)
		if err != nil {
			m.initErr = fmt.Errorf("create provider: %w", err)
			return
		}

		m.provider = provider
		m.resolvedConfig = resolved
	})
	return m.initErr
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
		if _, err := sm.New(m.resolvedConfig.Provider.Type, m.resolvedConfig.Provider.Model, wd); err != nil {
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

	llmMsgs, err := agent.ConvertSessionToLLMMessages(sessionMsgs, m.resolvedConfig.Provider.Type)
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

	if m.resolvedConfig == nil || m.provider == nil {
		return fmt.Errorf("channel: provider not initialized for cron trigger")
	}

	aiAgent := agent.NewAIAgent(m.provider, m.resolvedConfig.Provider.Model, 0)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.SetContextWindow(m.resolvedConfig.Provider.ContextWindow)
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
		if _, err := sm.New(m.resolvedConfig.Provider.Type, m.resolvedConfig.Provider.Model, wd); err != nil {
			m.logger.Log("channel: cron create session: %v", err)
		} else {
			sm.SetThreadID(job.TargetThreadID)
		}
	}

	if sm != nil {
		aiAgent.SetSessionManager(sm)
	}

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, job.Prompt, m.systemPrompt, llm.ChatOptions{
		MaxTokens: m.resolvedConfig.MaxTokens,
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

// sendToThread delivers a message to the channel responsible for the given
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

	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
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
