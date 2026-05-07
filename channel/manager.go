package channel

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// ManagerConfig holds the configuration for creating a Manager.
type ManagerConfig struct {
	// Config is the loaded tachi configuration (providers, web search, MCP, etc.).
	Config *config.Config

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
	channels []Channel

	logger *debuglog.Logger
}

// NewManager creates a Manager.
// Channels are interactive — the iteration budget is always unlimited (0).
func NewManager(mcfg ManagerConfig) *Manager {
	return &Manager{
		cfg:          mcfg.Config,
		systemPrompt: mcfg.SystemPrompt,
		providerName: mcfg.ProviderName,
		modelName:    mcfg.ModelName,
		sessionStore: mcfg.SessionStore,
		logger:       debuglog.DefaultLogger.WithSource("channel:manager"),
	}
}

// Add registers a Channel. Must be called before Start().
func (m *Manager) Add(ch Channel) {
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

	m.mu.Lock()
	chans := make([]Channel, len(m.channels))
	copy(chans, m.channels)
	m.mu.Unlock()

	handler := m.buildHandler()

	for _, ch := range chans {
		go func(ch Channel) {
			m.logger.Log("channel: %s starting", ch.Name())
			if err := ch.Run(ctx, handler); err != nil {
				m.logger.Log("channel: %s exited: %v", ch.Name(), err)
			} else {
				m.logger.Log("channel: %s exited cleanly", ch.Name())
			}
		}(ch)
	}

	return nil
}

// buildHandler returns a MessageHandler. Each call processes one incoming
// message through a fresh agent instance.
func (m *Manager) buildHandler() MessageHandler {
	return func(ctx context.Context, msg IncomingMessage) (OutgoingMessage, error) {
		m.logger.Log("channel: recv thread=%s id=%s len=%d",
			msg.ThreadID, msg.MessageID, len(msg.Content))

		result, err := m.process(ctx, msg)
		if err != nil {
			return OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  fmt.Sprintf("❌ %v", err),
				ReplyTo:  msg.MessageID,
			}, err
		}
		return OutgoingMessage{
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
func (m *Manager) process(ctx context.Context, msg IncomingMessage) (string, error) {
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
	aiAgent.UnregisterTool("AskUserQuestion")

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
		if _, err := sm.New(m.resolvedConfig.Provider.Type, m.resolvedConfig.Provider.Model); err != nil {
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

	return m.drainEvents(eventCh, aiAgent)
}

// handleSlashCommand dispatches message starting with "/" to the appropriate
// handler. Returns the response text for the channel to send back.
func (m *Manager) handleSlashCommand(msg IncomingMessage) (string, error) {
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
	default:
		m.logger.Log("channel: unknown slash command from thread %s: %s", msg.ThreadID, cmd)
		return fmt.Sprintf("Unknown command: %s\n\nAvailable commands in channel mode:\n  /new — Start a new conversation\n  /mcp — List configured MCP servers", cmd), nil
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

// drainEvents consumes all AgentEvents, returning the final assistant text or
// an error. Because we control the agent instance, we can respond to any
// confirmation/AskUser events inline — though with skip_edit_confirm=true
// and AskUser unregistered, neither should appear.
func (m *Manager) drainEvents(ch <-chan agent.AgentEvent, aiAgent *agent.AIAgent) (string, error) {
	var text strings.Builder
	var lastErr error

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
			} else {
				m.logger.Log("channel: tool %s ok (%d bytes)", event.ToolName, len(event.ToolResult))
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
		if _, err := sm.New(m.resolvedConfig.Provider.Type, m.resolvedConfig.Provider.Model); err != nil {
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
