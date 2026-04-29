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

	// MaxIterations caps the agent-loop iterations per message.
	// If zero, uses config.MaxIterations.
	MaxIterations int
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
	maxIters     int

	// Lazy-initialized.
	initOnce       sync.Once
	initErr        error
	provider       llm.Provider
	resolvedConfig *config.ResolvedConfig

	mu       sync.Mutex
	channels []Channel
}

// NewManager creates a Manager.
func NewManager(mcfg ManagerConfig) *Manager {
	maxIters := mcfg.MaxIterations
	if maxIters <= 0 {
		maxIters = mcfg.Config.MaxIterations
	}
	return &Manager{
		cfg:          mcfg.Config,
		systemPrompt: mcfg.SystemPrompt,
		providerName: mcfg.ProviderName,
		modelName:    mcfg.ModelName,
		maxIters:     maxIters,
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
			debuglog.Log("channel: %s starting", ch.Name())
			if err := ch.Run(ctx, handler); err != nil {
				debuglog.Log("channel: %s exited: %v", ch.Name(), err)
			} else {
				debuglog.Log("channel: %s exited cleanly", ch.Name())
			}
		}(ch)
	}

	return nil
}

// buildHandler returns a MessageHandler. Each call processes one incoming
// message through a fresh agent instance.
func (m *Manager) buildHandler() MessageHandler {
	return func(ctx context.Context, msg IncomingMessage) (OutgoingMessage, error) {
		debuglog.Log("channel: recv thread=%s id=%s len=%d",
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
func (m *Manager) process(ctx context.Context, msg IncomingMessage) (string, error) {
	if m.resolvedConfig == nil || m.provider == nil {
		return "", fmt.Errorf("channel manager not initialized; call Start() first")
	}

	aiAgent := agent.NewAIAgent(m.provider, m.resolvedConfig.Provider.Model, m.maxIters)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.SetContextWindow(m.resolvedConfig.Provider.ContextWindow)

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
		debuglog.Log("channel: session setup for thread %s: %v", msg.ThreadID, err)
		// Continue anyway with a fresh session manager and no history.
		sm, _ = session.NewManager()
		priorHistory = nil
	}

	// Ensure a session exists for recording. If loadThreadSession created
	// one, the agent's RunConversationStream will use it. If it failed,
	// create a session here so the agent can still record.
	if sm != nil && !sm.HasCurrent() {
		if _, err := sm.New(m.resolvedConfig.Provider.Type, m.resolvedConfig.Provider.Model); err != nil {
			debuglog.Log("channel: create fallback session: %v", err)
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
			debuglog.Log("channel: tool call start: %s", event.ToolName)

		case agent.AgentEventToolCallArgs:
			debuglog.Log("channel: tool call args for %s: %s", event.ToolName, event.ToolArgs)

		case agent.AgentEventToolConfirmation:
			// Should not happen with skip_edit_confirm=true, but handle safely.
			debuglog.Log("channel: auto-approving unexpected confirmation: %s", event.ToolName)
			aiAgent.ConfirmTool(true)

		case agent.AgentEventAskUser:
			// Should not happen with AskUser unregistered, but handle safely.
			debuglog.Log("channel: auto-rejecting unexpected AskUser")
			aiAgent.RespondToAskUser(nil, nil)

		case agent.AgentEventToolResult:
			if event.ToolIsError {
				debuglog.Log("channel: tool %s error: %s", event.ToolName, event.ToolResult)
			} else {
				debuglog.Log("channel: tool %s ok (%d bytes)", event.ToolName, len(event.ToolResult))
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

// --- Session helpers ---

// loadThreadSession looks up a session by ThreadID (via session.ThreadID field).
// If found, returns the session manager loaded with that session and the
// converted LLM message history. If not found, creates a new session manager
// with a fresh session and returns nil history.
func (m *Manager) loadThreadSession(threadID string) (*session.Manager, []llm.Message, error) {
	sm, err := session.NewManager()
	if err != nil {
		return nil, nil, fmt.Errorf("session manager: %w", err)
	}

	// Try to find an existing session for this ThreadID.
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		// Non-fatal — we'll start a fresh session.
		debuglog.Log("channel: find session for %s: %v", threadID, err)
		return sm, nil, nil
	}

	if sess == nil {
		// No existing session → create a new one now. The agent will
		// record the first message.
		if _, err := sm.New(m.resolvedConfig.Provider.Type, m.resolvedConfig.Provider.Model); err != nil {
			return sm, nil, fmt.Errorf("create session: %w", err)
		}
		if err := sm.SetThreadID(threadID); err != nil {
			debuglog.Log("channel: set thread_id for %s: %v", threadID, err)
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

	debuglog.Log("channel: session %s thread=%s: %d session msgs → %d llm msgs",
		sess.ID, threadID, len(sessionMsgs), len(llmMsgs))

	return sm, llmMsgs, nil
}
