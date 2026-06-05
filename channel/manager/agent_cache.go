package manager

import (
	"context"
	"errors"
	"sync"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
)

// cachedAgent wraps a per-thread AIAgent with serialization (so a single
// thread never runs two turns concurrently against the same agent state)
// and bookkeeping fields used to detect when the cached agent must be
// rebuilt (e.g. after /model switches the active provider).
type cachedAgent struct {
	mu           sync.Mutex
	agent        *agent.AIAgent
	providerName string // currentProviderName when the agent was built
	model        string // resolved model when the agent was built

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
			m.logger.Log("channel: shared MCP skipped (not enabled)")
			return
		}

		mgr := mcp.NewManager(m.cfg.ToolResult.MaxResultChars(), m.cfg.ToolResult.ResultFileDir())
		mgr.SetLogger(m.logger)

		m.sharedMCPMu.Lock()
		m.sharedMCPMgr = mgr
		m.sharedMCPMu.Unlock()

		// Connect and populate pool in the background. context.Background()
		// is intentional: shared MCP outlives any single triggering message
		// and is torn down explicitly by Manager.Close().
		go m.populateSharedMCP(context.Background(), mgr)

		m.logger.Log("channel: shared MCP initialized (%d servers)",
			len(m.cfg.MCPServers))
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
		m.logger.Log("channel: shared MCP load error: %v", err)
	}

	// Start background refresher for HTTP MCP servers in channel mode.
	// Tools are lazily registered (per-agent, on first invocation), so the
	// callback only needs to log changes — pool/discovered-set updates are
	// handled by Manager.applyToolDelta internally.
	interval := m.cfg.MCPToolRefresh.RefreshInterval()
	if interval > 0 {
		mgr.StartRefresher(ctx, interval, func(delta *mcp.ToolListDelta) {
			m.logger.Log("channel: MCP refresh detected changes on %q: +%d -%d ~%d",
				delta.ServerName, len(delta.Added), len(delta.Removed), len(delta.Modified))
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
	prov, resolved := m.getProvider()
	if prov == nil || resolved == nil {
		return nil, errProviderNotInitialized
	}

	m.providerMu.RLock()
	curName := m.currentProviderName
	m.providerMu.RUnlock()
	curModel := resolved.Provider.Model

	for {
		m.agentCacheMu.Lock()
		if m.agentCache == nil {
			m.agentCache = make(map[string]*cachedAgent)
		}
		ca, ok := m.agentCache[threadID]
		if ok && (ca.providerName != curName || ca.model != curModel) {
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
				model:        curModel,
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
			built, err := m.buildAgent(ctx, threadID)
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
func (m *Manager) buildAgent(ctx context.Context, threadID string) (*agent.AIAgent, error) {
	prov, resolved := m.getProvider()
	if prov == nil || resolved == nil {
		return nil, errProviderNotInitialized
	}

	a := agent.NewAIAgent(prov, resolved.Provider.Model, 0)
	a.SetProcessManager(m.processManager) // shared PM survives turns
	a.SetSkipEditConfirm(true)
	a.SetContextWindow(resolved.Provider.ContextWindow)
	a.SetupTitleProvider(m.cfg)
	a.SetupCommitProvider(m.cfg)

	// Inject shared MCP — Configure() will skip InitMCPAsync.
	if m.cfg != nil && m.cfg.MCPEnabled() {
		if mgr := m.initSharedMCP(); mgr != nil {
			a.SetSharedMCP(mgr)
		}
	}

	if _, err := a.Configure(ctx, m.cfg); err != nil {
		return nil, err
	}

	// Channel mode is non-interactive — AskUser is unavailable.
	a.UnregisterTool(tools.ToolNameAskUser)

	m.logger.Log("channel: built cached agent for thread %s (provider=%s model=%s)",
		threadID, resolved.Provider.Type, resolved.Provider.Model)
	return a, nil
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
		m.logger.Log("channel: evicted cached agent for thread %s", threadID)
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
	m.logger.Log("channel: evicted all cached agents (provider switch)")
}

// errProviderNotInitialized is returned when acquireAgent runs before
// initProvider has populated the manager's provider state.
var errProviderNotInitialized = errors.New("channel: provider not initialized; call Start() first")

// getAgentEstimate returns the LastInputEstimate from the cached agent for
// the given thread, or 0 if no agent exists. Uses a short lock on
// agentCacheMu — does NOT acquire ca.mu — so this is safe for quick reads
// (e.g. /usage). A stale or zero value is acceptable for display purposes.
func (m *Manager) getAgentEstimate(threadID string) int64 {
	m.agentCacheMu.Lock()
	defer m.agentCacheMu.Unlock()
	ca, ok := m.agentCache[threadID]
	if !ok || ca.agent == nil {
		return 0
	}
	return ca.agent.LastInputEstimate()
}
