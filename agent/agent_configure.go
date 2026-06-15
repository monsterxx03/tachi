package agent

import (
	"context"
	"fmt"
	"os"

	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/subagent"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	"time"
)

// deferredToolProviderAdapter adapts mcp.DeferredPool to the
// systemreminder.DeferredToolProvider interface.
type deferredToolProviderAdapter struct {
	pool *mcp.DeferredPool
}

func (a *deferredToolProviderAdapter) All() []systemreminder.DeferredToolRecord {
	tools := a.pool.All()
	records := make([]systemreminder.DeferredToolRecord, len(tools))
	for i, t := range tools {
		records[i] = systemreminder.DeferredToolRecord{
			Name:        t.Name,
			Description: t.Description,
		}
	}
	return records
}

// Configure wires up all agent sub-systems from config: reminders, built-in
// tools, web search, and MCP server connections. Returns the MCP manager for
// later cleanup (may be nil).
func (a *AIAgent) Configure(ctx context.Context, cfg *config.Config) (*mcp.Manager, error) {
	// --- Memory backend (before skills — buildReminderCollector reads a.memory) ---
	if cfg.Memory.Type != "" {
		memCfg := cfg.Memory.ToMemoryConfig()
		backend, err := memory.New(cfg.Memory.Type, memCfg)
		if err != nil {
			a.logger.Log("Memory: failed to init %s backend: %v", cfg.Memory.Type, err)
		} else {
			a.memory = &MemoryState{Backend: backend}
			a.logger.Log("Memory: using %s backend", cfg.Memory.Type)

			// Wire keyword extractor for topic backend if provider is already set.
			if a.provider != nil {
				if tb, ok := backend.(*memory.TopicBackend); ok {
					tb.SetKeywordExtractor(NewLLMKeywordExtractor(a.provider, a.model))
					a.logger.Log("Memory: keyword extractor wired for topic backend")
				}
			}
		}
	}

	// --- Skill system (needs a.cfg for memory config) ---
	a.cfg = cfg
	a.initSkills()

	// --- Reminder collector (after memory + skills, before MCP) ---
	a.buildReminderCollector()

	// --- built-in tools + web search ---
	a.RegisterTools()

	// --- MCP servers (async) ---
	var mgr *mcp.Manager
	if a.sharedMCP {
		// Shared MCP was injected via SetSharedMCP — reuse it. The owner
		// (e.g. channel.Manager) is responsible for ConnectAll/Close, so we
		// return nil here to keep the caller's `defer mgr.Close()` a no-op.
		// We still register the search tool and DeferredToolReminder so this
		// agent can use the shared deferred pool for tool discovery.
		a.attachSharedMCPReminder()
		searchTool := tools.NewMCPSearchToolsTool(a.DeferredPool(), a.discoveredSet())
		a.RegisterTool(searchTool)
	} else if cfg.MCPEnabled() {
		var err error
		mgr, err = a.InitMCPAsync(ctx, cfg)
		if err != nil {
			a.logger.Log("MCP: failed to start async init: %v", err)
		}
	}

	// --- SubAgent tool ---
	a.SetupSubagentProvider(cfg)
	executor := subagent.NewExecutor(a, cfg.Subagent)
	if cfg.Subagent.Worktree {
		executor.EnableWorktree(a.logger)
	}
	a.RegisterTool(tools.NewSubagentTool(executor))

	// --- LSP servers ---
	if cfg.LSP.Enabled && len(cfg.LSP.Servers) > 0 {
		lspCfg := convertLSPConfig(&cfg.LSP)
		a.lspManager = lsp.NewManager(lspCfg)
		a.RegisterTool(tools.NewLSPTool(a.lspManager))
		a.RegisterTool(tools.NewLSPDiagnosticsTool(a.lspManager))
		// Inject LSP diagnostics after tool results so the LLM sees
		// errors/warnings from recent edits without asking.
		a.reminderCollector.AddReminder(&systemreminder.LSPDiagnosticsReminder{
			Provider: a.lspManager,
		})
		a.logger.Log("LSP: initialized with %d server(s)", len(lspCfg.Servers))
	}

	return mgr, nil
}

// convertLSPConfig converts from config.LSPConfig to lsp.Config.
func convertLSPConfig(cfg *config.LSPConfig) *lsp.Config {
	servers := make([]lsp.ServerConfig, len(cfg.Servers))
	for i, s := range cfg.Servers {
		servers[i] = lsp.ServerConfig{
			Name:               s.Name,
			Command:            s.Command,
			Args:               s.Args,
			Extensions:         s.Extensions,
			Languages:          s.Languages,
			InitializationOpts: s.InitializationOpts,
			Settings:           s.Settings,
			Env:                s.Env,
			WorkspaceFolder:    s.WorkspaceFolder,
			StartupTimeout:     time.Duration(s.StartupTimeout),
			ConcurrencyLimit:   s.ConcurrencyLimit,
		}
	}
	return &lsp.Config{
		Enabled:          cfg.Enabled,
		MaxRestarts:      cfg.MaxRestarts,
		MaxFileSize:      cfg.MaxFileSize,
		MaxResults:       cfg.MaxResults,
		ConcurrencyLimit: cfg.ConcurrencyLimit,
		RequestTimeout:   time.Duration(cfg.RequestTimeout),
		StartupTimeout:   time.Duration(cfg.StartupTimeout),
		Servers:          servers,
	}
}

// attachSharedMCPReminder configures DeferredToolReminder for an agent
// that uses a shared deferred pool (injected via SetSharedMCP). Mirrors the
// final reminder-attach logic from connectMCPBackground, but skips the
// connection / discovery phase.
func (a *AIAgent) attachSharedMCPReminder() {
	pool := a.DeferredPool()
	set := a.discoveredSet()
	if pool == nil || set == nil {
		return
	}
	a.deferredToolReminder = &systemreminder.DeferredToolReminder{
		Provider: &deferredToolProviderAdapter{pool: pool},
		Tracker:  set,
	}
	total := pool.Len()
	discovered := len(set.List())
	if discovered < total {
		a.reminderCollector.AddReminder(a.deferredToolReminder)
	}
}

// startMCPToolRefresher starts background tool list polling for HTTP MCP
// servers if enabled in config. The callback handles registry updates and
// system-reminder notification when tool changes are detected.
func (a *AIAgent) startMCPToolRefresher(ctx context.Context, cfg *config.Config) {
	if a.mcpManager == nil {
		return
	}

	interval := cfg.MCPToolRefresh.RefreshInterval()
	if interval <= 0 {
		a.logger.Log("MCP: tool list refresh disabled")
		return
	}

	// Only start if there are HTTP servers connected
	hasHTTPServer := false
	for _, srv := range cfg.MCPServers {
		if srv.Type == config.MCPTransportHTTP && srv.IsEnabled() {
			hasHTTPServer = true
			break
		}
	}
	if !hasHTTPServer {
		a.logger.Log("MCP: no HTTP servers, skipping tool list refresher")
		return
	}

	a.mcpManager.StartRefresher(ctx, interval, func(delta *mcp.ToolListDelta) {
		a.onMCPToolsRefreshed(delta)
	})
}

// onMCPToolsRefreshed handles tool list changes detected by the background
// refresher. It updates the tool registry (for eagerly-registered tools) and
// marks the DeferredToolReminder dirty so the LLM is notified.
func (a *AIAgent) onMCPToolsRefreshed(delta *mcp.ToolListDelta) {
	prefix := "mcp__" + delta.ServerName + "__"

	// 1. Remove tools from the active registry if they were eagerly registered
	for _, name := range delta.Removed {
		fullName := prefix + name
		if a.toolRegistry.GetTool(fullName) != nil {
			a.toolRegistry.Unregister(fullName)
			a.logger.Log("MCP: refresh unregistered %s from tool registry", fullName)
		}
	}

	// 2. For modified tools that were eagerly registered, re-register with new schema.
	//    The pool has already been updated by Manager.applyToolDelta — we just need
	//    to update the registry if the tool was auto-loaded.
	for _, t := range delta.Modified {
		fullName := t.Name()
		if a.toolRegistry.GetTool(fullName) != nil {
			// Re-register with the updated tool instance
			a.toolRegistry.Unregister(fullName)
			a.RegisterTool(t)
			a.logger.Log("MCP: refresh re-registered %s with updated schema", fullName)
		}
	}

	// 3. Notify the LLM about newly available tools
	if len(delta.Added) > 0 {
		a.NotifyDeferredToolsAdded()
	}

	// 4. Log summary
	totalChanges := len(delta.Added) + len(delta.Removed) + len(delta.Modified)
	if totalChanges > 0 {
		a.logger.Log("MCP: refresh applied %d changes on %q (+%d -%d ~%d)",
			totalChanges, delta.ServerName,
			len(delta.Added), len(delta.Removed), len(delta.Modified))
	}
}

// InitMCPAsync starts MCP server connections in a background goroutine
// and returns immediately. The manager (which owns the deferred pool,
// discovered set, and init-done channel) is set up synchronously; actual
// tool discovery happens asynchronously.
//
// Use MCPReady() to get a channel that closes when init completes,
// or WaitForMCP(ctx) to block with a context deadline.
//
// Thread-safe: tools are registered via the (now thread-safe) Registry,
// and the deferred pool has its own mutex.
func (a *AIAgent) InitMCPAsync(ctx context.Context, cfg *config.Config) (*mcp.Manager, error) {
	mgr := mcp.NewManager(cfg.ToolResult.MaxResultChars(), cfg.ToolResult.ResultFileDir())
	mgr.SetLogger(a.logger)
	a.mcpManager = mgr

	// Register MCPSearchTools immediately so the LLM can discover tools
	// as they come in. The pool is empty initially, so search returns
	// nothing until MCP servers finish connecting.
	searchTool := tools.NewMCPSearchToolsTool(mgr.Pool(), mgr.DiscoveredSet())
	a.RegisterTool(searchTool)
	a.logger.Log("MCP: registered MCPSearchTools tool (async init, %d servers)",
		len(cfg.MCPServers))

	// Connect and discover tools in the background
	go a.connectMCPBackground(ctx, cfg)

	return mgr, nil
}

// connectMCPBackground populates the manager's deferred pool, registers
// auto-load tools into the agent's registry, and attaches DeferredToolReminder.
// Runs in a background goroutine started by InitMCPAsync.
func (a *AIAgent) connectMCPBackground(ctx context.Context, cfg *config.Config) {
	defer a.mcpManager.MarkInitDone()
	defer a.logger.Log("MCP: async init completed")

	autoLoad, all, errs := a.mcpManager.PopulateFromConnect(ctx, cfg)
	for _, err := range errs {
		a.logger.Log("MCP: load error: %v", err)
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}
	if len(all) == 0 {
		a.logger.Log("MCP: no tools discovered from any server")
		return
	}

	// Single-agent path: eagerly register auto-load tools so they are
	// visible to the agent's tool registry without going through the
	// lazy-register fallback. Channel mode skips this and relies on
	// AIAgent.lazyRegisterMCPTool at first invocation.
	for _, t := range autoLoad {
		a.RegisterTool(t)
	}
	if len(autoLoad) > 0 {
		a.logger.Log("MCP: %d tools auto-registered async", len(autoLoad))
	}

	pool := a.mcpManager.Pool()
	set := a.mcpManager.DiscoveredSet()
	total := pool.Len()
	discovered := len(set.List())

	// Create DeferredToolReminder (always, for potential use via toggle)
	a.deferredToolReminder = &systemreminder.DeferredToolReminder{
		Provider: &deferredToolProviderAdapter{pool: pool},
		Tracker:  set,
	}

	// Register DeferredToolReminder only if there are undiscovered tools
	if discovered < total {
		a.reminderCollector.AddReminder(a.deferredToolReminder)
		a.logger.Log("MCP: DeferredToolReminder added (%d undiscovered of %d)",
			total-discovered, total)
	}

	// Start background tool list refresher for HTTP MCP servers
	a.startMCPToolRefresher(ctx, cfg)
}

// WaitForMCP blocks until the background MCP initialization completes,
// or the context is cancelled / times out. Returns nil on success.
func (a *AIAgent) WaitForMCP(ctx context.Context) error {
	if a.mcpManager == nil {
		return nil // MCP not configured
	}
	return a.mcpManager.WaitInit(ctx)
}

// MCPReady returns a channel that's closed when MCP background init completes.
// If MCP is not configured, returns a pre-closed channel.
func (a *AIAgent) MCPReady() <-chan struct{} {
	if a.mcpManager == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return a.mcpManager.InitDone()
}

// ResumeSession loads the most recent session from disk, converts it to LLM
// message format, prepends the given system prompt (if non-empty), and attaches
// the session manager to the agent for ongoing session recording.
// Returns the loaded session metadata alongside the messages so callers can
// rebuild the provider to match the session's original provider/model.
func (a *AIAgent) ResumeSession(providerType, systemPrompt string) ([]llm.Message, []session.Message, *session.Session, error) {
	sm, err := session.NewManager()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("session manager: %w", err)
	}

	sessions, err := sm.List()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, nil, nil, fmt.Errorf("no sessions to resume")
	}

	latest := sessions[0]
	if _, err := sm.Load(latest.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("load session %s: %w", latest.ID, err)
	}

	// Restore working directory if recorded
	if latest.WorkingDir != "" {
		if err := os.Chdir(latest.WorkingDir); err != nil {
			a.logger.Log("Agent: failed to chdir to %s: %v", latest.WorkingDir, err)
		}
	}

	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load messages: %w", err)
	}

	// Restore lastInputTokens from the most recent assistant message with usage
	// so that TokenWarningReminder works correctly in the resumed session.
	// Prefer the local estimate (EstimatedInputTokens) to match what was shown
	// during the active conversation; fall back to API-returned InputTokens.
	for i := len(sessionMsgs) - 1; i >= 0; i-- {
		if sessionMsgs[i].Type == session.MessageTypeAssistant && sessionMsgs[i].Usage != nil {
			if sessionMsgs[i].Usage.EstimatedInputTokens > 0 {
				a.lastInputTokens = sessionMsgs[i].Usage.EstimatedInputTokens
			} else {
				a.lastInputTokens = sessionMsgs[i].Usage.InputTokens
			}
			a.logger.Log("Agent: restored lastInputTokens=%d from session message", a.lastInputTokens)
			break
		}
	}

	llmMsgs, err := ConvertSessionToLLMMessages(sessionMsgs, providerType)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("convert session messages: %w", err)
	}

	if systemPrompt != "" {
		llmMsgs = append([]llm.Message{{Role: "system", Content: systemPrompt}}, llmMsgs...)
	}

	a.sessionManager = sm
	// Update logger with session ID for debug log tracking
	if cur := a.sessionManager.Current(); cur != nil {
		a.logger = a.logger.WithSessionID(cur.ID)
	}
	// Notify memory backend that the resumed session is active
	a.StartSessionMemory()
	return llmMsgs, sessionMsgs, latest, nil
}

// buildReminderCollector builds the reminder collector with core reminders,
// the live skill list reminder, and MemoryRecallReminder (if memory is
// configured). Called once during Configure after sub-systems are initialized.
func (a *AIAgent) buildReminderCollector() {
	core := []systemreminder.Reminder{
		systemreminder.DateReminder{},
		systemreminder.ProjectContextReminder{},
		systemreminder.IterationWarningReminder{Threshold: a.cfg.SystemReminder.IterationWarningThreshold},
		systemreminder.TokenWarningReminder{ThresholdPct: a.cfg.SystemReminder.TokenWarningThresholdPct},
	}
	if a.cfg.SystemReminder.GitReminder == nil || *a.cfg.SystemReminder.GitReminder {
		core = append(core, systemreminder.GitReminder{})
	}

	all := make([]systemreminder.Reminder, 0, len(core)+2)
	all = append(all, core...)
	all = append(all, a.skillListReminder)
	if a.memory != nil {
		all = append(all, systemreminder.MemoryRecallReminder{
			Backend: a.memory.Backend,
			Limit:   5,
			Timeout: a.cfg.Memory.Timeout,
		})
	}
	a.reminderCollector = systemreminder.NewCollector(all...)
	a.reminderCollector.SetLogger(a.logger)
}
