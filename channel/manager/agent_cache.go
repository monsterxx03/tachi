package manager

import (
	"context"
	"os"
	"sync"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// cachedAgent wraps a per-thread AIAgent with serialization (so a single
// thread never runs two turns concurrently against the same agent state)
// and bookkeeping fields used to detect when the cached agent must be
// rebuilt (e.g. after /model switches the active provider).
type cachedAgent struct {
	mu           sync.Mutex
	agent        *agent.AIAgent
	providerName string // currentProviderName when the agent was built

	// workDir is the working directory for this thread. All tools (Bash,
	// Read, Write, Edit, Glob, etc.) resolve relative paths against it.
	// Initialized to os.Getwd() on first agent creation; updated by /cd.
	workDir string

	// history caches the full LLM message slice (system prompt + all prior
	// turns) so each new turn can pass it directly to RunConversationStream
	// without reloading from disk. Updated after every completed turn via
	// agent.GetLastMessages(). Nil until the first turn completes (or after
	// an eviction), at which point prepareThreadSession loads from disk once.
	history []llm.Message
}

// initSharedMCP lazily creates the shared MCP manager and kicks off async
// ConnectAll. Subsequent calls are no-ops (sync.Once). The returned manager
// owns its own DeferredPool / DiscoveredSet / init-done channel; agents
// access them through the manager rather than via separate fields.
//
// Returns nil if MCP is not configured in cfg.
func (m *Manager) initSharedMCP() *mcp.Manager {
	m.sharedMCPOnce.Do(func() {
		if m.cfg == nil || !m.cfg.MCPEnabled() {
			m.logger.Info(context.Background(), "channel: shared MCP skipped (not enabled)")
			return
		}

		mgr := mcp.NewManager(context.Background(), m.cfg, m.logger)

		m.sharedMCPMu.Lock()
		m.sharedMCPMgr = mgr
		m.sharedMCPMu.Unlock()

		// Connect and populate pool in the background. context.Background()
		// is intentional: shared MCP outlives any single triggering message
		// and is torn down explicitly by Manager.Close().
		go m.populateSharedMCP(context.Background(), mgr)

		m.logger.Info(context.Background(), "channel: shared MCP initialized", "servers", len(m.cfg.MCPServers))
	})

	m.sharedMCPMu.RLock()
	defer m.sharedMCPMu.RUnlock()
	return m.sharedMCPMgr
}

// populateSharedMCP delegates to mcp.Manager.PopulateFromConnect to discover
// tools, fill the deferred pool, and add auto-load tools to the discovered
// set. Channel mode does NOT eagerly register tools into any agent's registry —
// AIAgent.lazyRegisterMCPTool handles registration on first invocation.
//
// Errors are logged; partial discovery is acceptable.
func (m *Manager) populateSharedMCP(ctx context.Context, mgr *mcp.Manager) {
	defer mgr.MarkInitDone()

	_, _, errs := mgr.PopulateFromConnect(ctx, m.cfg)
	for _, err := range errs {
		m.logger.Error(ctx, "channel: shared MCP load error", err)
	}

	// Start background refresher for HTTP MCP servers in channel mode.
	// Tools are lazily registered (per-agent, on first invocation), so the
	// callback only needs to log changes — pool/discovered-set updates are
	// handled by Manager.applyToolDelta internally.
	interval := m.cfg.MCPToolRefresh.RefreshInterval()
	if interval > 0 {
		mgr.StartRefresher(ctx, interval, func(delta *mcp.ToolListDelta) {
			m.logger.Info(ctx, "channel: MCP refresh detected changes", "server", delta.ServerName, "added", len(delta.Added), "removed", len(delta.Removed), "modified", len(delta.Modified))
		})
	}
}

// acquireAgent returns the cached AIAgent for the given threadID, creating
// one if absent. The returned agent is locked — callers MUST call
// releaseAgent(ca) (typically via defer) when they finish their turn.
//
// The agent is rebuilt if the active provider changes (detected via
// currentProviderName / model fields under the providerMu).
//
// Concurrency model:
//   - agentCacheMu is a SHORT lock used only for map read/write.
//   - cachedAgent.mu is a LONG lock held for the duration of a turn.
//   - We acquire cacheMu, find/create the slot, release cacheMu, THEN
//     take ca.mu. After taking ca.mu we re-check that ca is still the
//     current entry for threadID — otherwise it was evicted concurrently
//     and we must retry.
func (m *Manager) acquireAgent(ctx context.Context, threadID string) (*cachedAgent, error) {
	resolved := m.getProviderForThread(threadID)
	curName := resolved.Name

	for {
		m.agentCacheMu.Lock()
		if m.agentCache == nil {
			m.agentCache = make(map[string]*cachedAgent)
		}
		ca, ok := m.agentCache[threadID]
		if ok && ca.providerName != curName {
			// Provider switched — request a rebuild. Note: we cannot
			// Close() the agent here because it might still be in use
			// by a turn that hasn't released ca.mu yet. evictAgent
			// handles the safe-shutdown path (it takes ca.mu first).
			delete(m.agentCache, threadID)
			ok = false
		}
		if !ok {
			ca = &cachedAgent{
				providerName: curName,
				workDir:      initialWorkDir(),
			}
			m.agentCache[threadID] = ca
		}
		m.agentCacheMu.Unlock()

		// Take the long lock OUTSIDE cacheMu so other threads can
		// acquire/evict their own agents concurrently while a long turn
		// runs on this one.
		ca.mu.Lock()

		// Re-check that ca is still the current entry. If it was evicted
		// (e.g. by /new) while we were waiting on ca.mu, retry from scratch.
		m.agentCacheMu.Lock()
		cur, stillThere := m.agentCache[threadID]
		m.agentCacheMu.Unlock()
		if !stillThere || cur != ca {
			ca.mu.Unlock()
			continue
		}

		if ca.agent == nil {
			built, err := m.buildAgent(ctx, threadID, resolved)
			if err != nil {
				// Roll back: drop the empty slot so the next attempt
				// starts fresh, and release the long lock.
				m.agentCacheMu.Lock()
				if m.agentCache[threadID] == ca {
					delete(m.agentCache, threadID)
				}
				m.agentCacheMu.Unlock()
				ca.mu.Unlock()
				return nil, err
			}
			ca.agent = built
		}
		return ca, nil
	}
}

// releaseAgent unlocks the cached agent slot.
func (m *Manager) releaseAgent(ca *cachedAgent) {
	if ca == nil {
		return
	}
	ca.mu.Unlock()
}

// buildAgent constructs a new AIAgent wired to the manager's shared MCP
// state, process manager, skill store, and so on. The agent is otherwise
// empty — the caller (runAgentTurn / OnCronTrigger) is responsible for
// session loading, steer channel, and per-turn ephemeral tools.
//
// prov and resolved are the provider and config to use for this agent.
// Callers should pass the result of getProviderForThread(threadID) so that
// per-thread /model overrides are respected.
func (m *Manager) buildAgent(ctx context.Context, threadID string, resolved *llm.ResolvedProvider) (*agent.AIAgent, error) {
	titleGen := false // channel mode has no title UI; skip the extra LLM call

	// Inject shared MCP — Configure() will skip MCP init.
	var sharedMCP *mcp.Manager
	if m.cfg != nil && m.cfg.MCPEnabled() {
		sharedMCP = m.initSharedMCP()
	}

	a, _, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		Resolved:        resolved,
		Logger:          m.logger,
		PermissionMode:  agent.PermissionModeSkip,
		TitleGenEnabled: &titleGen,
		ProcessManager:  m.processManager,
		MCPManager:      sharedMCP,
		FullConfig:      m.cfg,
		SystemConfig:    agent.SystemConfigFromConfig(m.cfg),
	})
	if err != nil {
		return nil, err
	}

	// Non-interactive channels unregister AskUserQuestion so the LLM
	// never attempts to use it. Interactive channels (those implementing
	// InteractiveChannel with Interactive()==true) keep it registered.
	//
	// Note: NewAIAgentWithConfig unconditionally unregisters AskUser for
	// PermissionModeSkip (channel mode), so the interactive branch must
	// re-register it here — "thread is interactive" logging alone would be
	// misleading (the tool exists only after this re-registration).
	if !m.isThreadChannelInteractive(threadID) {
		a.UnregisterTool(tools.ToolNameAskUser)
	} else {
		a.RegisterTool(tools.AskUserTool{})
		m.logger.Info(ctx, "channel: thread is interactive", "thread", threadID)
	}

	// Register channel-specific tools (optional ToolProvider interface).
	// e.g. the wave channel injects its wave tools, which need the wave
	// client's access_token and only make sense on wave threads.
	var channelTools []tools.Tool
	if tp, ok := m.channelForThread(threadID).(channel.ToolProvider); ok {
		channelTools = tp.ChannelTools()
	}
	for _, t := range channelTools {
		a.RegisterTool(t)
		m.logger.Info(ctx, "channel: registered channel tool", "thread", threadID, "tool", t.Name())
	}

	// Register the channel's per-thread reminder (optional
	// ThreadReminderChannel interface) into the agent's reminder collector,
	// so it rides in the unified <system-reminder> block alongside
	// date/memory/etc. The agent cache is per-thread, so the adapter can
	// close over the threadID. When reminders are disabled the collector is
	// a no-op and AddReminder is safely inert.
	if tc, ok := m.channelForThread(threadID).(channel.ThreadReminderChannel); ok {
		a.Config.ReminderCollector.AddReminder(&threadReminder{ch: tc, threadID: threadID})
		m.logger.Info(ctx, "channel: registered thread reminder", "thread", threadID)
	}

	m.logger.Info(ctx, "channel: built cached agent", "thread", threadID, "provider", resolved.Type, "model", resolved.Model)
	return a, nil
}

// threadReminder adapts a channel.ThreadReminderChannel into the agent's
// systemreminder.Reminder interface for a single thread. The channel decides
// per call whether anything should be injected (it owns the inject-once
// semantics); this adapter only guards the call boundary.
type threadReminder struct {
	ch       channel.ThreadReminderChannel
	threadID string
}

func (r *threadReminder) Generate(ctx context.Context, rctx systemreminder.Context) []string {
	// A channel reminder is a per-user-turn concern. Tool-result boundary
	// collections happen many times per turn — repeating the reminder there
	// would both spam the transcript and double-consume the channel's
	// inject-once state.
	if rctx.IsToolResult {
		return nil
	}
	text, ok := r.ch.ThreadReminder(ctx, r.threadID)
	if !ok || text == "" {
		return nil
	}
	return []string{text}
}

// evictAgent removes the cached agent for a thread. Called by /new (the
// thread's session is being ended, so any per-thread state should reset)
// and by callers that want to force a rebuild on the next turn.
func (m *Manager) evictAgent(threadID string) {
	m.agentCacheMu.Lock()
	defer m.agentCacheMu.Unlock()
	if ca, ok := m.agentCache[threadID]; ok {
		// Take the agent lock briefly to ensure we don't yank an agent
		// from under a concurrent acquireAgent caller. If a turn is in
		// flight, this blocks until it returns.
		ca.mu.Lock()
		if ca.agent != nil {
			ca.agent.Close()
		}
		ca.mu.Unlock()
		delete(m.agentCache, threadID)
		m.logger.Info(context.Background(), "channel: evicted cached agent", "thread", threadID)
	}
}

// evictAllAgents drops every cached agent. Called when the active provider
// changes via /model so the next message rebuilds against the new config.
func (m *Manager) evictAllAgents() {
	m.agentCacheMu.Lock()
	defer m.agentCacheMu.Unlock()
	for threadID, ca := range m.agentCache {
		ca.mu.Lock()
		if ca.agent != nil {
			ca.agent.Close()
		}
		ca.mu.Unlock()
		delete(m.agentCache, threadID)
	}
	m.logger.Info(context.Background(), "channel: evicted all cached agents (provider switch)")
}

// getAgentEstimateWithBreakdown returns the token estimate and its breakdown
// from the cached agent for the given thread, read atomically so the two
// always describe the same estimate. Returns zero values when no agent exists.
//
// The cachedAgent lock is deliberately not taken: /usage may run while a turn
// is in flight on this thread. The agent's turn state carries its own lock, so
// this observes a consistent snapshot without blocking the turn.
func (m *Manager) getAgentEstimateWithBreakdown(threadID string) (int64, tokenbreakdown.Breakdown) {
	m.agentCacheMu.Lock()
	defer m.agentCacheMu.Unlock()
	ca, ok := m.agentCache[threadID]
	if !ok || ca.agent == nil {
		return 0, tokenbreakdown.Breakdown{}
	}
	return ca.agent.LastInputEstimateWithBreakdown()
}

// initialWorkDir returns the default working directory for a new cachedAgent.
// Matches the initial value used by prepareThreadSession when creating a session.
func initialWorkDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// getThreadWorkDir returns the working directory for a thread. The cached
// agent's workDir wins; on a cache miss (e.g. right after /new evicted the
// agent, or after a restart before the first turn) it falls back to the
// thread's persisted session WorkingDir — set by /new with announcement
// defaults or by a previous /cd. This keeps state-less commands (/sh, /model,
// ...) and announcement updates observing the configured directory instead of
// the process CWD. Returns "" when neither source has a directory.
func (m *Manager) getThreadWorkDir(threadID string) string {
	m.agentCacheMu.Lock()
	ca, ok := m.agentCache[threadID]
	m.agentCacheMu.Unlock()
	if ok {
		return ca.workDir
	}

	// Cache miss — consult the persisted session metadata. Only reached
	// when the agent cache is empty for this thread, so the disk lookup is
	// rare (per /new, not per turn).
	sm := m.newSessionManager()
	if sm == nil {
		return ""
	}
	sess, err := sm.FindByThreadID(threadID)
	if err != nil || sess == nil {
		return ""
	}
	return sess.WorkingDir
}
