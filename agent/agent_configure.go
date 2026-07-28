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

// configure wires up all agent sub-systems from the extracted system config.
// This is the internal implementation called by NewAIAgentWithConfig.
func (a *AIAgent) configure(ctx context.Context, sysCfg AgentSystemConfig) (*mcp.Manager, error) {
	// --- Memory backend (before skills — buildReminderCollector reads a.memory) ---
	if sysCfg.Memory.Type != "" {
		memCfg := sysCfg.Memory.ToMemoryConfig()
		backend, err := memory.New(sysCfg.Memory.Type, memCfg, a.logger)
		if err != nil {
			a.logger.Error(ctx, "Memory: failed to init backend", err, "type", sysCfg.Memory.Type)
		} else {
			a.memory = &MemoryState{Backend: backend}
			a.logger.Info(ctx, "Memory: using backend", "type", sysCfg.Memory.Type)

			// Wire keyword extractor for topic backend.
			// Requires an LLM provider — skip when nil (e.g. `tachi tools`).
			// Keyword provider resolution (from config provider name) is the
			// caller's responsibility — pass a pre-resolved provider in
			// AgentConfig.KeywordProvider, or let configure fall back to the
			// main provider.
			if tb, ok := backend.(*memory.TopicBackend); ok && a.provider != nil {
				tb.SetKeywordExtractor(NewLLMKeywordExtractor(a.provider, a.provider.Model(), sysCfg.Memory.Timeout, a.logger))
				a.logger.Info(ctx, "Memory: keyword extractor wired for topic backend")
			}
		}
	}

	// --- Skill system ---
	a.initSkills()

	// --- Bash permission policy (global config + project .tachi/permissions.yaml) ---
	// Caller should set permission policy via AgentConfig or SetPermissionPolicy before Configure.
	// The old Configure() path sets it from the full config; the new path expects it pre-set.
	if a.permissionPolicy == nil && a.cfg != nil {
		a.SetPermissionPolicy(NewPermissionPolicyFromConfig(a.cfg, config.FindProjectRoot(), a.logger))
	}

	// --- Reminder collector (after memory + skills, before MCP) ---
	a.buildReminderCollectorFrom(SystemReminderConfig{
		GitReminder:         sysCfg.SystemReminder.GitReminder,
		MemoryRecallLimit:   sysCfg.Memory.RecallLimit,
		MemoryRecallTimeout: sysCfg.Memory.Timeout,
	})

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
	} else if len(sysCfg.MCPServers) > 0 {
		var err error
		mgr, err = a.initMCPAsync(ctx, sysCfg)
		if err != nil {
			a.logger.Error(ctx, "MCP: failed to start async init", err)
		}
	}

	// --- SubAgent tool ---
	a.SetupSubagentProvider(a.cfg)
	executor := subagent.NewExecutor(a, sysCfg.Subagent)
	if sysCfg.Subagent.Worktree {
		executor.EnableWorktree(a.logger)
	}
	a.subagentRunner = executor
	a.RegisterTool(tools.NewSubagentTool(executor))

	// --- Hook system (after tools, so user command hooks can reference them) ---
	a.initHookSystemFrom(&sysCfg)

	// --- LSP servers ---
	if sysCfg.LSP.IsEnabled() && len(sysCfg.LSP.Servers) > 0 {
		lspCfg := convertLSPConfig(&sysCfg.LSP)
		a.lspManager = lsp.NewManager(lspCfg)
		a.RegisterTool(tools.NewLSPTool(a.lspManager))
		a.RegisterTool(tools.NewLSPDiagnosticsTool(a.lspManager))
		a.reminderCollector.AddReminder(&systemreminder.LSPDiagnosticsReminder{
			Provider: a.lspManager,
		})
		a.logger.Info(ctx, "LSP: initialized", "servers", len(lspCfg.Servers))
	}

	return mgr, nil
}

// initHookSystemFrom initialises the event hook dispatcher from an AgentSystemConfig.
func (a *AIAgent) initHookSystemFrom(sysCfg *AgentSystemConfig) {
	ctx := context.Background()

	if !sysCfg.Hooks.IsEnabled() {
		return
	}

	d := hooks.NewDispatcher(a.logger)

	// Load user-defined command hooks from config
	for event, cmds := range sysCfg.Hooks.Events {
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
	if sysCfg.Herdr.IsEnabled() && hooks.DetectHerdr() {
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

// initMCPAsync is the internal variant that takes AgentSystemConfig.
func (a *AIAgent) initMCPAsync(ctx context.Context, sysCfg AgentSystemConfig) (*mcp.Manager, error) {
	mgr := mcp.NewManager(ctx, sysCfg.ToolResult.MaxResultChars(), sysCfg.ToolResult.ResultFileDir(), a.logger)
	a.mcpManager = mgr

	// Register MCPSearchTools immediately so the LLM can discover tools
	// as they come in. The pool is empty initially, so search returns
	// nothing until MCP servers finish connecting.
	searchTool := tools.NewMCPSearchToolsTool(mgr.Pool(), mgr.DiscoveredSet())
	a.RegisterTool(searchTool)
	a.logger.Info(ctx, "MCP: registered MCPSearchTools tool (async init)", "servers", len(sysCfg.MCPServers))

	// Connect and discover tools in the background.
	// Pass the full config (a.cfg) for downstream methods that still need it.
	// When called from NewAIAgentWithConfig, a.cfg was set from AgentConfig.FullConfig.
	go a.connectMCPBackground(ctx, a.cfg)

	return mgr, nil
}

// connectMCPBackground populates the manager's deferred pool, registers
// auto-load tools into the agent's registry, and attaches DeferredToolReminder.
// Runs in a background goroutine started by InitMCPAsync / initMCPAsync.
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

// buildReminderCollectorFrom builds the reminder collector with core reminders,
// using an explicit config instead of reading from a.cfg.
func (a *AIAgent) buildReminderCollectorFrom(sysCfg SystemReminderConfig) {
	var reminders []systemreminder.Reminder

	// Always-on reminders.
	reminders = append(reminders,
		systemreminder.DateReminder{},
		systemreminder.ProjectContextReminder{},
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
	if sysCfg.GitReminder == nil || *sysCfg.GitReminder {
		reminders = append(reminders, systemreminder.GitReminder{})
	}

	// Memory recall reminder (only when memory backend is enabled).
	if a.memory != nil {
		limit := sysCfg.MemoryRecallLimit
		if limit <= 0 {
			limit = 5
		}
		reminders = append(reminders, systemreminder.MemoryRecallReminder{
			Backend: a.memory.Backend,
			Limit:   limit,
			Timeout: sysCfg.MemoryRecallTimeout,
		})
	}

	a.reminderCollector = systemreminder.NewCollector(reminders...)
}

// resolveKeywordProvider resolves the configured KeywordProvider from config
// and wires it into the TopicBackend's keyword extractor. This must be called
// AFTER configure() creates the memory backend.
//
// It is safe to call when memory is not configured or when no topic backend
// is in use — it checks a.memory before doing anything.
func (a *AIAgent) resolveKeywordProvider(cfg *config.Config) {
	if a.memory == nil || a.provider == nil {
		return
	}
	tb, ok := a.memory.Backend.(*memory.TopicBackend)
	if !ok {
		return
	}

	kpName := cfg.Memory.KeywordProvider
	if kpName == "" {
		return // no dedicated keyword provider configured; main provider is already set
	}

	kpCfg := cfg.FindProvider(kpName)
	if kpCfg == nil {
		a.logger.Info(context.Background(), "Memory: keyword_provider not found, falling back to main provider", "provider", kpName)
		return
	}

	resolved, err := config.ResolveProviderConfig(kpCfg)
	if err != nil {
		a.logger.Error(context.Background(), "Memory: failed to resolve keyword provider, falling back to main provider", err, "provider", kpName)
		return
	}

	sp, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		a.logger.Error(context.Background(), "Memory: failed to create keyword provider, falling back to main provider", err, "provider", kpName)
		return
	}

	tb.SetKeywordExtractor(NewLLMKeywordExtractor(sp, resolved.Model, cfg.Memory.Timeout, a.logger))
	a.logger.Info(context.Background(), "Memory: keyword extractor re-wired with dedicated provider", "provider", kpName, "type", resolved.Type, "model", resolved.Model)
}
