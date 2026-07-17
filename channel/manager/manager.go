package manager

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/lockedmap"
	"github.com/monsterxx03/tachi/pkg/logger"
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
// The cached agent is rebuilt or evicted in these cases:
//   - /new on a thread → that thread's cached agent is dropped and its
//     per-thread provider override (from /model) is cleared, so the next
//     message starts cleanly with the global default provider.
//   - /model on a thread → stores a per-thread provider override and evicts
//     only that thread's cached agent. Other threads are unaffected.
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
// Channels are non-interactive by default:
//   - skip_edit_confirm=true → all EditFile edits auto-approved (no user prompt).
//   - AskUserQuestion tool is unregistered → LLM never uses it in channel mode.
//   - If a confirmation or AskUser event somehow fires, drainEvents handles
//     it gracefully (auto-confirm / auto-reject).
//
// Channels that implement the InteractiveChannel interface and return true
// from Interactive() keep AskUserQuestion registered, enabling interactive
// forms when the platform supports them.
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
	cfg                 *config.Config
	providerName        string
	modelName           string
	currentProviderName string // Tracks which provider is currently active

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

	// System-level scheduler for background tasks (AutoDream, etc.).
	// Completely isolated from the user-facing cron scheduler.
	systemScheduler *cron.SystemScheduler

	// skillStore provides skill listing and activation for /skill command.
	skillStore *skill.Store

	// Per-thread agent activations for steer support.
	threadActivations lockedmap.Map[string, *threadActivation]

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

	// --- Thread → Channel mapping ---
	//
	// Tracks which channel a thread belongs to. Used by buildAgent to
	// decide whether to unregister AskUserQuestion (non-interactive
	// channels unregister it; InteractiveChannel implementations keep it).
	// Populated on first message by the per-channel handler wrapper.
	threadChannels  map[string]channel.Channel
	threadChannelMu sync.RWMutex

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

	logger *logger.Logger

	// wg tracks running channel goroutines. Done() returns a channel that
	// closes when all channel goroutines have exited.
	wg   sync.WaitGroup
	done chan struct{}
	once sync.Once
}

// Done returns a channel that is closed when all registered channel goroutines
// have exited. Useful for runChannels to know when to stop waiting.
func (m *Manager) Done() <-chan struct{} {
	return m.done
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

	// --- AskUser state ---
	//
	// When the agent invokes AskUserQuestion, drainEvents creates
	// askUserRespCh and blocks on it. The handler routes the user's
	// next reply into this channel instead of the steer queue.
	askUserRespCh   chan tools.AskUserResult // non-nil when waiting for AskUser answer
	askUserThreadID string                   // ThreadID for sending questions
	askUserReplyID  string                   // MessageID for sendToThread replyTo

	// --- Whisper ambient state (only active when groupChat=true) ---

	groupChat      bool         // whether this thread is in group chat mode (set once)
	ambientPending []ambientMsg // buffered non-directed messages
	ambientTimer   *time.Timer  // batch window timer (nil when inactive)
	lastAmbient    time.Time    // when the last ambient turn ended
	silenceCount   atomic.Int32 // consecutive [SILENT] replies (for backoff)
}

// ambientMsg represents a single non-directed message buffered for whisper processing.
type ambientMsg struct {
	content   string
	sender    string
	timestamp time.Time
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
		agentCache:     make(map[string]*cachedAgent),
		processManager: tools.NewProcessManager(),
		logger:         logger.New("channel"),
		done:           make(chan struct{}),
		threadChannels: make(map[string]channel.Channel),
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
			m.logger.Error(ctx, "channel: cron init failed", err)
			// Non-fatal: channels can still work without cron.
		}
	}

	m.mu.Lock()
	chans := make([]channel.Channel, len(m.channels))
	copy(chans, m.channels)
	m.mu.Unlock()

	cmdHandler := m.buildCommandHandler()
	baseHandler := m.buildHandler()

	for _, ch := range chans {
		// Wrap the shared handler with per-channel thread tracking so
		// buildAgent can later check whether the channel is interactive.
		handler := m.buildHandlerForChannel(ch, baseHandler)

		m.wg.Add(1)
		go func(ch channel.Channel) {
			defer m.wg.Done()
			m.logger.Info(ctx, "channel: starting", "name", ch.Name())

			// Inject CommandHandler if this channel supports it.
			if cc, ok := ch.(channel.CommandChannel); ok {
				cc.SetCommandHandler(cmdHandler)
				m.logger.Info(ctx, "channel: received CommandHandler", "name", ch.Name())
			}

			// Inject provider names for slash command autocomplete.
			if ac, ok := ch.(channel.Autocompleter); ok {
				var names []string
				if m.cfg != nil {
					for _, p := range m.cfg.Providers {
						if p.Name != "" {
							names = append(names, p.Name)
						}
					}
				}
				ac.SetProviderNames(names)
				if len(names) > 0 {
					m.logger.Info(ctx, "channel: received provider names", "name", ch.Name(), "count", len(names))
				}
			}

			// Lifecycle: OnStart → Run.
			// OnStart gives the channel a chance to initialise before
			// entering its message loop. If it fails, the channel is
			// skipped entirely.
			if err := ch.OnStart(ctx); err != nil {
				m.logger.Error(ctx, "channel: OnStart error", err, "name", ch.Name())
				return
			}

			if err := ch.Run(ctx, handler); err != nil {
				m.logger.Error(ctx, "channel: exited with error", err, "name", ch.Name())
			} else {
				m.logger.Info(ctx, "channel: exited cleanly", "name", ch.Name())
			}
		}(ch)
	}

	// Wait for all channel goroutines to exit and signal Done().
	go func() {
		m.wg.Wait()
		m.once.Do(func() { close(m.done) })
	}()

	// Start cron scheduler after channels are initialized.
	if m.scheduler != nil {
		if err := m.scheduler.Start(ctx); err != nil {
			m.logger.Error(ctx, "channel: cron scheduler start failed", err)
		}
	}

	// Start system-level scheduler (AutoDream, etc.) — fully isolated from user cron.
	if m.cfg != nil && m.cfg.Dream.Enabled {
		m.systemScheduler = cron.NewSystemScheduler(cron.SystemSchedulerConfig{
			Logger: m.logger,
		})
		if err := m.systemScheduler.Register(
			"auto-dream",
			m.cfg.Dream.Schedule,
			m.cfg.Dream.SubagentTimeout,
			m.executeDream,
		); err != nil {
			m.logger.Error(ctx, "channel: auto-dream registration failed", err)
		} else {
			m.systemScheduler.Start(ctx)
		}
	}

	return nil
}

// --- Provider resolution ---

// getProvider returns the current global (default) provider and resolved config
// under read lock. Use getProviderForThread for agent turn paths that should
// respect per-thread /model overrides.
func (m *Manager) getProvider() (llm.Provider, *config.ResolvedConfig) {
	m.providerMu.RLock()
	defer m.providerMu.RUnlock()
	return m.provider, m.resolvedConfig
}

// getProviderForThread returns the provider for the given thread by reading
// the session's ProviderName override (set by /model) from session meta.
// Falls back to the global provider when the session has no override or
// the override cannot be resolved.
func (m *Manager) getProviderForThread(threadID string) (llm.Provider, *config.ResolvedConfig, string) {
	if threadID != "" && m.cfg != nil {
		// Peek session meta to check for a /model override.
		sm := m.newSessionManager()
		sess, err := sm.FindByThreadID(threadID)
		if err == nil && sess != nil && sess.ProviderName != "" {
			pCfg := m.cfg.FindProvider(sess.ProviderName)
			if pCfg != nil {
				resolved, err := config.ResolveProviderConfig(pCfg)
				if err == nil {
					provider, err := llm.NewProvider(
						resolved.Type,
						resolved.APIKey,
						resolved.BaseURL,
						resolved.Model,
					)
					if err == nil {
						m.logger.Info(context.Background(), "channel: thread using session override provider", "thread", threadID, "provider", sess.ProviderName, "model", resolved.Model)
						return provider, &config.ResolvedConfig{Provider: *resolved}, sess.ProviderName
					}
				}
			}
			m.logger.Warn(context.Background(), "channel: thread has ProviderName but could not resolve; falling back to global",
				"thread", threadID, "provider_name", sess.ProviderName)
		}
	}

	m.providerMu.RLock()
	defer m.providerMu.RUnlock()
	return m.provider, m.resolvedConfig, m.currentProviderName
}

// providerNameForThread returns the provider config name active for the
// given thread, preferring a session-level /model override over the global
// default.
func (m *Manager) providerNameForThread(threadID string) string {
	if threadID != "" && m.cfg != nil {
		sm := m.newSessionManager()
		sess, err := sm.FindByThreadID(threadID)
		if err == nil && sess != nil && sess.ProviderName != "" {
			return sess.ProviderName
		}
	}
	m.providerMu.RLock()
	defer m.providerMu.RUnlock()
	return m.currentProviderName
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
		sm = session.NewManagerWithStore(m.sessionStore, m.logger)
	} else {
		var err error
		sm, err = session.NewManager(m.logger)
		if err != nil {
			m.logger.Error(context.Background(), "channel: session manager fallback failed", err)
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
func (m *Manager) loadThreadSession(threadID string, resolved *config.ResolvedConfig) (*session.Manager, []llm.Message, error) {
	var sm *session.Manager
	if m.sessionStore != nil {
		sm = session.NewManagerWithStore(m.sessionStore, m.logger)
	} else {
		var err error
		sm, err = session.NewManager(m.logger)
		if err != nil {
			return nil, nil, fmt.Errorf("session manager: %w", err)
		}
	}

	// Try to find an existing session for this ThreadID.
	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		// Non-fatal — we'll start a fresh session.
		m.logger.Warn(context.Background(), "channel: find session", "thread", threadID, "error", err)
		return sm, nil, nil
	}

	if sess == nil {
		// No existing session → create a new one now. The agent will
		// record the first message.
		if _, err := sm.New(resolved.Provider.Name, ""); err != nil {
			return sm, nil, fmt.Errorf("create session: %w", err)
		}
		sm.SetThreadID(threadID)
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

	m.logger.Info(context.Background(), "channel: session loaded", "session", sess.ID, "thread", threadID, "session_msgs", len(sessionMsgs), "llm_msgs", len(llmMsgs))

	return sm, llmMsgs, nil
}

// sendToThread delivers an intermediate progress message to the channel for
// the given ThreadID. Used for intermediate progress messages like auto-compact
// notifications. This is best-effort — failures are logged but not propagated.
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
			m.logger.Error(ctx, "channel: sendToThread failed", err, "name", ch.Name())
			return
		}
		m.logger.Info(ctx, "channel: progress sent", "name", ch.Name(), "thread", threadID)
		return
	}
	m.logger.Warn(ctx, "channel: sendToThread — no channel accepted thread", "thread", threadID)
}

// persistThreadWorkDir persists the thread's working directory to its session
// metadata so it survives restarts. Errors are logged but not returned since
// the in-memory cache has already been updated — persistence is best-effort.
func (m *Manager) persistThreadWorkDir(threadID, workDir string) {
	sm := m.newSessionManager()
	if sm == nil {
		return
	}
	sess, err := sm.FindByThreadID(threadID)
	if err != nil || sess == nil {
		return
	}
	sm.SetCurrent(sess)
	sess.WorkingDir = workDir
	sess.UpdatedAt = time.Now()
	if err := sm.UpdateMeta(sess); err != nil {
		m.logger.Error(context.Background(), "channel: persist workDir", err, "thread", threadID)
	}
}

// presentQuestionsToChannel delivers structured AskUser questions to the
// channel that owns the given thread. Interactive channels receive the
// questions via PresentQuestions; non-interactive channels should never
// reach this path (AskUser is unregistered for them).
func (m *Manager) presentQuestionsToChannel(threadID, replyID string, questions []channel.Question) {
	m.threadChannelMu.RLock()
	ch, ok := m.threadChannels[threadID]
	m.threadChannelMu.RUnlock()

	if !ok {
		m.logger.Warn(context.Background(), "channel: presentQuestionsToChannel — no channel for thread", "thread", threadID)
		return
	}

	ic, ok := ch.(channel.InteractiveChannel)
	if !ok {
		m.logger.Warn(context.Background(), "channel: presentQuestionsToChannel — channel is not interactive, questions dropped", "name", ch.Name())
		return
	}

	if err := ic.PresentQuestions(context.Background(), threadID, replyID, questions); err != nil {
		m.logger.Error(context.Background(), "channel: PresentQuestions failed", err, "name", ch.Name())
	}
}

// Close releases all resources held by the Manager, including killing all
// tracked background processes, evicting cached agents, and tearing down
// the shared MCP manager. Safe to call multiple times.
func (m *Manager) Close() {
	// Stop system scheduler first (may have in-flight jobs).
	if m.systemScheduler != nil {
		m.systemScheduler.Stop()
	}

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

// --- Channel-tracking helpers ---

// buildHandlerForChannel wraps the shared base handler with per-channel
// thread tracking. When an incoming message arrives, the handler records
// the channel→threadID mapping so buildAgent can later determine whether
// the channel supports interactive tools.
func (m *Manager) buildHandlerForChannel(ch channel.Channel, base channel.MessageHandler) channel.MessageHandler {
	if base == nil {
		base = m.buildHandler()
	}
	return func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		m.setThreadChannel(msg.ThreadID, ch)
		return base(ctx, msg)
	}
}

// setThreadChannel records the channel that owns a given thread.
func (m *Manager) setThreadChannel(threadID string, ch channel.Channel) {
	m.threadChannelMu.Lock()
	m.threadChannels[threadID] = ch
	m.threadChannelMu.Unlock()
}

// isThreadChannelInteractive checks whether the channel for the given
// thread implements InteractiveChannel and reports itself as interactive.
func (m *Manager) isThreadChannelInteractive(threadID string) bool {
	m.threadChannelMu.RLock()
	ch, ok := m.threadChannels[threadID]
	m.threadChannelMu.RUnlock()
	if !ok {
		return false
	}
	ic, ok := ch.(channel.InteractiveChannel)
	return ok && ic.Interactive()
}
