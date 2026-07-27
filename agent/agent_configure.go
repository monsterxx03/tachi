package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/subagent"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
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
		backend, err := memory.New(cfg.Memory.Type, memCfg, a.logger)
		if err != nil {
			a.logger.Error(ctx, "Memory: failed to init backend", err, "type", cfg.Memory.Type)
		} else {
			a.memory = &MemoryState{Backend: backend}
			a.logger.Info(ctx, "Memory: using backend", "type", cfg.Memory.Type)

			// Wire keyword extractor for topic backend.
			// Requires an LLM provider — skip when nil (e.g. `tachi tools`).
			if tb, ok := backend.(*memory.TopicBackend); ok && a.provider != nil {
				kwProvider, kwModel := a.provider, a.provider.Model()

				// Resolve dedicated keyword provider if configured.
				if kpName := cfg.Memory.KeywordProvider; kpName != "" {
					if kpCfg := cfg.FindProvider(kpName); kpCfg != nil {
						resolved, err := config.ResolveProviderConfig(kpCfg)
						if err == nil {
							sp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
							if err == nil {
								kwProvider, kwModel = sp, resolved.Model
								a.logger.Info(ctx, "Memory: using keyword provider", "provider", kpName, "type", resolved.Type, "model", resolved.Model)
							}
						}
					}
					if kwProvider == a.provider {
						a.logger.Info(ctx, "Memory: keyword_provider not resolved, falling back to main provider", "provider", kpName)
					}
				}

				timeout := cfg.Memory.Timeout
				tb.SetKeywordExtractor(NewLLMKeywordExtractor(kwProvider, kwModel, timeout, a.logger))
				a.logger.Info(ctx, "Memory: keyword extractor wired for topic backend")
			}
		}
	}

	// --- Skill system (needs a.cfg for memory config) ---
	a.cfg = cfg
	a.initSkills()

	// --- Bash permission policy (global config + project .tachi/permissions.yaml) ---
	a.SetPermissionPolicy(NewPermissionPolicyFromConfig(cfg, config.FindProjectRoot(), a.logger))

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
			a.logger.Error(ctx, "MCP: failed to start async init", err)
		}
	}

	// --- SubAgent tool ---
	a.SetupSubagentProvider(cfg)
	executor := subagent.NewExecutor(a, cfg.Subagent)
	if cfg.Subagent.Worktree {
		executor.EnableWorktree(a.logger)
	}
	a.subagentRunner = executor
	a.RegisterTool(tools.NewSubagentTool(executor))

	// --- Hook system (after tools, so user command hooks can reference them) ---
	a.initHookSystem(ctx, cfg)

	// --- LSP servers ---
	if cfg.LSP.IsEnabled() && len(cfg.LSP.Servers) > 0 {
		lspCfg := convertLSPConfig(&cfg.LSP)
		a.lspManager = lsp.NewManager(lspCfg)
		a.RegisterTool(tools.NewLSPTool(a.lspManager))
		a.RegisterTool(tools.NewLSPDiagnosticsTool(a.lspManager))
		// Inject LSP diagnostics after tool results so the LLM sees
		// errors/warnings from recent edits without asking.
		a.reminderCollector.AddReminder(&systemreminder.LSPDiagnosticsReminder{
			Provider: a.lspManager,
		})
		a.logger.Info(ctx, "LSP: initialized", "servers", len(lspCfg.Servers))
	}

	return mgr, nil
}

// initHookSystem initialises the event hook dispatcher and registers handlers:
//  1. User-defined command hooks from config.yaml
//  2. Herdr integration (auto-detected from HERDR_ENV)
func (a *AIAgent) initHookSystem(ctx context.Context, cfg *config.Config) {
	if !cfg.Hooks.IsEnabled() {
		return
	}

	d := hooks.NewDispatcher(a.logger)

	// Load user-defined command hooks from config
	for event, cmds := range cfg.Hooks.Events {
		for _, cmd := range cmds {
			if cmd.Command == "" {
				continue
			}
			timeout := 5 * time.Second
			if cmd.Timeout != "" {
				if d, err := time.ParseDuration(cmd.Timeout); err == nil {
					timeout = d
				} else {
					a.logger.Warn(ctx, "Hooks: invalid command timeout, using default 5s", "timeout", cmd.Timeout, "error", err)
				}
			}
			async := true
			if cmd.Async != nil {
				async = *cmd.Async
			}
			d.RegisterCommand(event, hooks.Handler{
				Name:    cmd.Command,
				Command: cmd.Command,
				Timeout: timeout,
				Async:   async,
				Env:     cmd.Env,
			})
		}
	}

	// Auto-detect Herdr integration
	if cfg.Herdr.IsEnabled() && hooks.DetectHerdr() {
		handler := hooks.NewHerdrHandler()
		for event := range hooks.EventActions {
			evt := event
			d.RegisterCallback(evt, "herdr", func(ctx context.Context, e string, p []byte) {
				handler.Handle(ctx, e, p)
			})
		}
		a.logger.Info(ctx, "Hooks: Herdr integration enabled (auto-detected from HERDR_ENV)")
	}

	if len(d.Events()) > 0 {
		a.hookDispatcher = d
		a.logger.Info(ctx, "Hooks: dispatcher initialized", "events", len(d.Events()))
	}
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
		a.logger.Info(ctx, "MCP: tool list refresh disabled")
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
		a.logger.Info(ctx, "MCP: no HTTP servers, skipping tool list refresher")
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
			a.logger.Info(context.Background(), "MCP: refresh unregistered from tool registry", "tool", fullName)
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
			a.logger.Info(context.Background(), "MCP: refresh re-registered with updated schema", "tool", fullName)
		}
	}

	// 3. Notify the LLM about newly available tools
	if len(delta.Added) > 0 {
		a.NotifyDeferredToolsAdded()
	}

	// 4. Log summary
	totalChanges := len(delta.Added) + len(delta.Removed) + len(delta.Modified)
	if totalChanges > 0 {
		a.logger.Info(context.Background(), "MCP: refresh applied changes",
			"total", totalChanges, "server", delta.ServerName,
			"added", len(delta.Added), "removed", len(delta.Removed), "modified", len(delta.Modified))
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
	mgr := mcp.NewManager(ctx, cfg.ToolResult.MaxResultChars(), cfg.ToolResult.ResultFileDir(), a.logger)
	a.mcpManager = mgr

	// Register MCPSearchTools immediately so the LLM can discover tools
	// as they come in. The pool is empty initially, so search returns
	// nothing until MCP servers finish connecting.
	searchTool := tools.NewMCPSearchToolsTool(mgr.Pool(), mgr.DiscoveredSet())
	a.RegisterTool(searchTool)
	a.logger.Info(ctx, "MCP: registered MCPSearchTools tool (async init)", "servers", len(cfg.MCPServers))

	// Connect and discover tools in the background
	go a.connectMCPBackground(ctx, cfg)

	return mgr, nil
}

// connectMCPBackground populates the manager's deferred pool, registers
// auto-load tools into the agent's registry, and attaches DeferredToolReminder.
// Runs in a background goroutine started by InitMCPAsync.
func (a *AIAgent) connectMCPBackground(ctx context.Context, cfg *config.Config) {
	defer a.mcpManager.MarkInitDone()
	defer func() { a.logger.Info(ctx, "MCP: async init completed") }()

	autoLoad, all, errs := a.mcpManager.PopulateFromConnect(ctx, cfg)
	for _, err := range errs {
		a.logger.Error(ctx, "MCP: load error", err)
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
	}
	if len(all) == 0 {
		a.logger.Info(ctx, "MCP: no tools discovered from any server")
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
		a.logger.Info(ctx, "MCP: tools auto-registered async", "count", len(autoLoad))
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
		a.logger.Info(ctx, "MCP: DeferredToolReminder added", "undiscovered", total-discovered, "total", total)
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
	sm, err := session.NewManager(a.logger)
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
			a.logger.Error(context.Background(), "Agent: failed to chdir", err, "dir", latest.WorkingDir)
		}
	}

	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load messages: %w", err)
	}

	// Restore the token estimate from the most recent assistant message with
	// usage so that TokenWarningReminder works correctly in the resumed session.
	// Prefer the local estimate (EstimatedInputTokens) to match what was shown
	// during the active conversation; fall back to API-returned InputTokens.
	for i := len(sessionMsgs) - 1; i >= 0; i-- {
		if sessionMsgs[i].Type == session.MessageTypeAssistant && sessionMsgs[i].Usage != nil {
			restored := sessionMsgs[i].Usage.EstimatedInputTokens
			if restored <= 0 {
				restored = sessionMsgs[i].Usage.InputTokens
			}
			a.turn.setTokens(restored)
			a.logger.Info(context.Background(), "Agent: restored token estimate from session message", "lastInputTokens", restored)
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
		a.logger = a.logger.With("session_id", cur.ID)
	}
	// Notify memory backend that the resumed session is active
	a.StartSessionMemory()
	return llmMsgs, sessionMsgs, latest, nil
}

// backgroundTaskProvider adapts *tools.ProcessManager to
// systemreminder.BackgroundTaskProvider for the BackgroundTaskReminder.
type backgroundTaskProvider struct {
	pm *tools.ProcessManager
}

func (p *backgroundTaskProvider) DrainCompleted() []systemreminder.BackgroundTaskInfo {
	if p.pm == nil {
		return nil
	}
	completed := p.pm.DrainCompleted()
	infos := make([]systemreminder.BackgroundTaskInfo, len(completed))
	for i, c := range completed {
		infos[i] = systemreminder.BackgroundTaskInfo{
			Name:         c.Name,
			Command:      c.Command,
			ExitCode:     c.ExitCode,
			Status:       string(c.Status),
			Error:        c.Error,
			RecentStdout: c.RecentStdout,
			RecentStderr: c.RecentStderr,
		}
	}
	return infos
}

// buildReminderCollector builds the reminder collector with core reminders,
// the live skill list reminder, BackgroundTaskReminder, and
// MemoryRecallReminder (if memory is configured).
// Called once during Configure after sub-systems are initialized.
func (a *AIAgent) buildReminderCollector() {
	var reminders []systemreminder.Reminder

	// Always-on reminders.
	reminders = append(reminders,
		systemreminder.DateReminder{},
		systemreminder.ProjectContextReminder{},
		systemreminder.IterationWarningReminder{Threshold: a.cfg.SystemReminder.IterationWarningThreshold},
		systemreminder.TokenWarningReminder{ThresholdPct: a.cfg.SystemReminder.TokenWarningThresholdPct},
		a.skillListReminder,
		&systemreminder.BackgroundTaskReminder{
			Provider: &backgroundTaskProvider{pm: a.processManager},
		},
	)

	// Plan tracking reminder — only meaningful where SavePlan is available
	// (ACP sessions with a plan card UI).
	if a.planToolEnabled {
		reminders = append(reminders, systemreminder.PlanTrackingReminder{})
	}

	// Git reminder (configurable).
	if a.cfg.SystemReminder.GitReminder == nil || *a.cfg.SystemReminder.GitReminder {
		reminders = append(reminders, systemreminder.GitReminder{})
	}

	// Memory recall reminder (only when memory backend is enabled).
	if a.memory != nil {
		limit := a.cfg.Memory.RecallLimit
		if limit <= 0 {
			limit = 5
		}
		reminders = append(reminders, systemreminder.MemoryRecallReminder{
			Backend: a.memory.Backend,
			Limit:   limit,
			Timeout: a.cfg.Memory.Timeout,
		})
	}

	a.reminderCollector = systemreminder.NewCollector(reminders...)
}
