package manager

import (
	"context"
	"fmt"
	"sync"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// Config holds the configuration for creating a Manager.
type Config struct {
	// Cfg is the loaded tachi configuration (providers, web search, MCP, etc.).
	Cfg *config.Config

	// ProviderName overrides the default provider from config.
	// If empty, uses the config's default provider.
	ProviderName string

	// ModelName overrides the model. If empty, uses the provider's configured model.
	ModelName string

	// SessionStore overrides the default file-based session store.
	// If nil, sessions are stored under ~/.tachi/session (default).
	// Tests should inject a FileStore backed by a temporary directory.
	SessionStore session.Store

	// SkillStore overrides the default skill store. If nil, the manager
	// auto-builds one scanning the project's `.tachi/skills/` and the
	// user-global skill directory. Tests should inject a hermetic store
	// (e.g. via skill.NewStoreWithDirs) so they don't pick up real skills
	// from the host filesystem.
	SkillStore *skill.Store
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
//   - Message processing: on each incoming message, looks up (or builds) a
//     per-thread cached AIAgent, loads or creates a per-thread session, runs
//     one agent turn with auto-confirm semantics, and returns the response.
//
// # Agent Lifecycle
//
// Each ThreadID maps to a long-lived *AIAgent stored in agentCache. The cached
// agent is reused for every subsequent message on the same thread, so state
// that is meaningful to accumulate across turns — MCP discoveredSet, skill
// activations, lastInputTokens for token-warning reminders, etc. — survives
// without being reset on every message. Per-turn ephemeral tools (CronTool,
// SendFileTool) are scoped via SaveToolRegistry / RestoreToolRegistry so they
// don't leak into the next turn.
//
// The cached agent is rebuilt or evicted in three cases:
//   - /new on a thread → that thread's cached agent is dropped so the next
//     message starts cleanly.
//   - /model switches the active provider → all cached agents are evicted
//     because they were built against the old provider/model.
//   - /compact runs a one-off summarization against a throwaway agent (with
//     ClearToolRegistry) so the cached agent's tool set isn't disturbed.
//
// All cached agents share a single *mcp.Manager + DeferredPool + DiscoveredSet
// (see initSharedMCP). MCP servers connect once at first use and are torn down
// in Manager.Close(); they are NOT reconnected per message.
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
// Cross-thread isolation comes from per-ThreadID cached agents (separate map
// entries) and per-thread sessions on disk. Same-thread serialization is
// enforced at two layers: the threadActivation gate prevents two handler
// goroutines from running concurrently on one thread (the second message is
// queued via steer instead), and cachedAgent.mu adds a defense-in-depth lock
// so paths that bypass threadActivation (e.g. cron triggers landing on the
// same thread as a regular message) cannot race on the same *AIAgent.
//
// When a message arrives for a thread that already has an active agent turn,
// it is injected via the steer mechanism: the message is queued and delivered
// to the agent at the next tool-call boundary, allowing the user to refine
// instructions mid-turn without waiting for the current turn to finish.
type Manager struct {
	cfg          *config.Config
	providerName string
	modelName    string
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

	// Shared ProcessManager for background processes across all agent turns.
	// Per-turn AIAgent instances are ephemeral, but background processes must
	// survive across turns. The Manager owns and cleans up this shared PM.
	processManager *tools.ProcessManager

	// --- Per-thread AIAgent cache (B-full) ---
	//
	// Agent state that is meaningful to accumulate across turns of the same
	// thread (skill activation, MCP discoveredSet, reminder cadence, etc.) is
	// preserved by reusing the same *AIAgent for every message arriving on a
	// given ThreadID. Cross-thread state isolation is preserved because each
	// thread has its own cached agent.
	agentCacheMu sync.Mutex
	agentCache   map[string]*cachedAgent

	// --- Shared MCP backend ---
	//
	// All cached agents share a single *mcp.Manager. The manager owns the
	// DeferredPool, DiscoveredSet, and init-done channel — see agent/mcp/
	// for details. This avoids per-turn reconnection of MCP servers and
	// preserves "tools the LLM has discovered via MCPSearchTools" across
	// turns and across threads. Lazily initialized on first agent acquisition.
	sharedMCPOnce sync.Once
	sharedMCPMu   sync.RWMutex
	sharedMCPMgr  *mcp.Manager

	logger *debuglog.Logger
}

// threadActivation holds the state for an active agent turn on a thread.
// When a new message arrives for a thread that already has a running agent,
// the message is queued in pending and injected via steer.
type threadActivation struct {
	mu          sync.Mutex
	steerRespCh chan string        // agent reads steer input from this
	resultCh    chan handlerResult // agent sends final result here
	pending     []string           // queued steer messages
	ctx         context.Context    // agent context for cancellation
	cancel      context.CancelFunc // cancels the agent turn
	cancelled   bool               // true when this turn was cancelled externally
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
	skillStore := mcfg.SkillStore
	if skillStore == nil {
		skillStore = skill.NewStore(config.FindProjectRoot())
	}
	return &Manager{
		cfg:            mcfg.Cfg,
		providerName:   mcfg.ProviderName,
		modelName:      mcfg.ModelName,
		sessionStore:   mcfg.SessionStore,
		skillStore:     skillStore,
		processManager: tools.NewProcessManager(),
		logger:         debuglog.DefaultLogger.WithSource("channel:manager"),
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
			// If a channel-specific provider name is configured, override.
			cfg := m.cfg
			if m.providerName != "" {
				cfgCopy := *m.cfg
				cfgCopy.Provider = m.providerName
				cfg = &cfgCopy
			}

			resolved, err := config.Resolve(cfg)
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
		if _, err := sm.New(resolved.Provider.Type, resolved.Provider.Model, config.FindProjectRoot()); err != nil {
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

// sendToThread delivers an intermediate progress message to the channel for
// the given ThreadID. Used for intermediate progress messages in verbose mode.
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

// Close releases all resources held by the Manager, including killing all
// tracked background processes, evicting cached agents, and tearing down
// the shared MCP manager. Safe to call multiple times.
func (m *Manager) Close() {
	m.evictAllAgents()

	m.sharedMCPMu.Lock()
	mgr := m.sharedMCPMgr
	m.sharedMCPMgr = nil
	m.sharedMCPMu.Unlock()
	if mgr != nil {
		mgr.Close()
	}

	if m.processManager != nil {
		m.processManager.KillAll()
	}
}
