package agent

import (
	"context"
	"errors"
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
	// --- Memory backend (before skills — buildReminderCollector reads a.Config.Memory) ---
	if sysCfg.Memory.Type != "" {
		// BaseDir is runtime-only (yaml:"-") — inject the state base dir here.
		memCfg := sysCfg.Memory
		memCfg.BaseDir = config.BaseDir()
		backend, err := memory.New(sysCfg.Memory.Type, memCfg, a.Config.Logger)
		if err != nil {
			a.Config.Logger.Error(ctx, "Memory: failed to init backend", err, "type", sysCfg.Memory.Type)
		} else {
			a.Config.Memory = &MemoryState{Backend: backend}
			a.Config.Logger.Info(ctx, "Memory: using backend", "type", sysCfg.Memory.Type)

			// Wire keyword extractor for topic backend.
			// Requires an LLM provider — skip when nil (e.g. `tachi tools`).
			// Keyword provider resolution (from config provider name) is the
			// caller's responsibility — pass a pre-resolved provider in
			// AgentConfig.KeywordProvider, or let configure fall back to the
			// main provider.
			if tb, ok := backend.(*memory.TopicBackend); ok && a.Provider() != nil {
				ext := NewLLMKeywordExtractor(a.Provider(), a.Model(), sysCfg.Memory.Timeout, a.Config.Logger)
				ext.SetSessionIDResolver(a.sessionID)
				tb.SetKeywordExtractor(ext)
				a.Config.Logger.Info(ctx, "Memory: keyword extractor wired for topic backend")
			}
		}
	}

	// --- Skill system ---
	if a.Config.DisableSkills {
		// Non-interactive modes (e.g. `tachi -p`) skip skill discovery:
		// no skill store scan, no Skill tool registration, no skill
		// list reminder. The SkillListReminder stays nil — code paths
		// referencing it must nil-check (see buildReminderCollectorFrom).
		a.Config.Logger.Info(ctx, "Skills: disabled (non-interactive mode)")
	} else {
		a.initSkills()
	}

	// --- Bash permission policy (global config + project .tachi/permissions.yaml) ---
	// Caller should set permission policy via AgentConfig or SetPermissionPolicy before Configure.
	if a.Config.PermissionPolicy == nil && a.Config.FullConfig != nil {
		a.SetPermissionPolicy(NewPermissionPolicyFromConfig(a.Config.FullConfig, config.FindProjectRoot(), a.Config.Logger))
	}

	// --- Reminder collector (after memory + skills, before MCP) ---
	if a.Config.DisableSystemReminders {
		// Non-interactive modes (e.g. `tachi -p`) want zero system reminders:
		// no date/git/project context, no skills catalog, no memory recall.
		// The no-op collector keeps later AddReminder calls (LSP diagnostics,
		// deferred MCP tools) inert and safe.
		a.Config.Logger.Info(ctx, "System reminders: disabled (non-interactive mode)")
		a.Config.ReminderCollector = disabledReminderCollector{}
	} else {
		a.buildReminderCollectorFrom(SystemReminderConfig{
			GitReminder:         sysCfg.SystemReminder.GitReminder,
			MemoryRecallLimit:   sysCfg.Memory.RecallLimit,
			MemoryRecallTimeout: sysCfg.Memory.Timeout,
			Pprof:               &sysCfg.Debug.PPROF,
		})
	}

	// --- built-in tools + web search ---
	a.RegisterTools()

	// --- MCP servers (async) ---
	var mgr *mcp.Manager
	if a.Config.DisableMCP {
		// Non-interactive modes (e.g. `tachi -p`) skip MCP entirely:
		// no server connection, no MCPSearchTools registration, no
		// DeferredToolReminder. MCPManager stays nil — WaitForMCP /
		// MCPReady / DeferredPool / discoveredSet all nil-check.
		a.Config.Logger.Info(ctx, "MCP: disabled (non-interactive mode)")
	} else if !a.mcpOwned {
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
			a.Config.Logger.Error(ctx, "MCP: failed to start async init", err)
		}
	}

	// --- SubAgent tool ---
	if a.Config.SubagentRunner == nil {
		if a.Config.FullConfig != nil {
			a.SetupSubagentProvider(a.Config.FullConfig)
		}
		executor := subagent.NewExecutor(a, sysCfg.Subagent)
		if sysCfg.Subagent.Worktree {
			executor.EnableWorktree(a.Config.Logger)
		}
		a.Config.SubagentRunner = executor
		a.RegisterTool(tools.NewSubagentTool(executor))
	} else {
		// Custom SubagentRunner provided via AgentConfig — register the tool anyway
		a.RegisterTool(tools.NewSubagentTool(a.Config.SubagentRunner))
	}

	// Hook system (after tools, so user command hooks can reference them).
	// An externally injected dispatcher (AgentConfig.HookDispatcher) wins —
	// skip parsing config hooks entirely in that case.
	if a.Config.HookDispatcher == nil {
		a.initHookSystemFrom(&sysCfg)
	}

	// --- LSP servers ---
	if sysCfg.LSP.IsEnabled() && len(sysCfg.LSP.Servers) > 0 {
		lspCfg := convertLSPConfig(&sysCfg.LSP)
		a.Config.LSPManager = lsp.NewManager(lspCfg, a.Config.Logger)
		a.RegisterTool(tools.NewLSPTool(a.Config.LSPManager))
		a.RegisterTool(tools.NewLSPDiagnosticsTool(a.Config.LSPManager))
		a.Config.ReminderCollector.AddReminder(&systemreminder.LSPDiagnosticsReminder{
			Provider: a.Config.LSPManager,
		})
		a.Config.Logger.Info(ctx, "LSP: initialized", "servers", len(lspCfg.Servers))
	}

	return mgr, nil
}

// initHookSystemFrom initialises the event hook dispatcher from an AgentSystemConfig.
func (a *AIAgent) initHookSystemFrom(sysCfg *AgentSystemConfig) {
	ctx := context.Background()

	if !sysCfg.Hooks.IsEnabled() {
		return
	}

	d := hooks.NewDispatcher(a.Config.Logger)

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
					a.Config.Logger.Warn(ctx, "Hooks: invalid command timeout, using default 5s", "timeout", cmd.Timeout, "error", err)
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
		handler.SetLogger(a.Config.Logger)
		for event := range hooks.EventActions {
			evt := event
			d.RegisterCallback(evt, "herdr", func(ctx context.Context, e string, p []byte) {
				handler.Handle(ctx, e, p)
			})
		}
		a.Config.Logger.Info(ctx, "Hooks: Herdr integration enabled (auto-detected from HERDR_ENV)")
	}

	if len(d.Events()) > 0 {
		a.Config.HookDispatcher = d
		a.Config.Logger.Info(ctx, "Hooks: dispatcher initialized", "events", len(d.Events()))
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
		a.Config.ReminderCollector.AddReminder(a.deferredToolReminder)
	}
}

// startMCPToolRefresher starts background tool list polling for HTTP MCP
// servers if enabled in config. The callback handles registry updates and
// system-reminder notification when tool changes are detected.
func (a *AIAgent) startMCPToolRefresher(ctx context.Context, cfg *config.Config) {
	if a.Config.MCPManager == nil {
		return
	}

	interval := cfg.MCPToolRefresh.RefreshInterval()
	if interval <= 0 {
		a.Config.Logger.Info(ctx, "MCP: tool list refresh disabled")
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
		a.Config.Logger.Info(ctx, "MCP: no HTTP servers, skipping tool list refresher")
		return
	}

	a.Config.MCPManager.StartRefresher(ctx, interval, func(delta *mcp.ToolListDelta) {
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
		if a.Config.ToolRegistry.GetTool(fullName) != nil {
			a.Config.ToolRegistry.Unregister(fullName)
			a.Config.Logger.Info(context.Background(), "MCP: refresh unregistered from tool registry", "tool", fullName)
		}
	}

	// 2. For modified tools that were eagerly registered, re-register with new schema.
	for _, t := range delta.Modified {
		fullName := t.Name()
		if a.Config.ToolRegistry.GetTool(fullName) != nil {
			a.Config.ToolRegistry.Unregister(fullName)
			a.RegisterTool(t)
			a.Config.Logger.Info(context.Background(), "MCP: refresh re-registered with updated schema", "tool", fullName)
		}
	}

	// 3. Notify the LLM about newly available tools
	if len(delta.Added) > 0 {
		a.NotifyDeferredToolsAdded()
	}

	// 4. Log summary
	totalChanges := len(delta.Added) + len(delta.Removed) + len(delta.Modified)
	if totalChanges > 0 {
		a.Config.Logger.Info(context.Background(), "MCP: refresh applied changes",
			"total", totalChanges, "server", delta.ServerName,
			"added", len(delta.Added), "removed", len(delta.Removed), "modified", len(delta.Modified))
	}
}

// initMCPAsync is the internal variant that takes AgentSystemConfig.
func (a *AIAgent) initMCPAsync(ctx context.Context, sysCfg AgentSystemConfig) (*mcp.Manager, error) {
	mgr := mcp.NewManager(ctx, sysCfg.ToolResult.MaxResultChars(), sysCfg.ToolResult.ResultFileDir(), a.Config.Logger)
	a.Config.MCPManager = mgr

	// Register MCPSearchTools immediately so the LLM can discover tools
	// as they come in. The pool is empty initially, so search returns
	// nothing until MCP servers finish connecting.
	searchTool := tools.NewMCPSearchToolsTool(mgr.Pool(), mgr.DiscoveredSet())
	a.RegisterTool(searchTool)
	a.Config.Logger.Info(ctx, "MCP: registered MCPSearchTools tool (async init)", "servers", len(sysCfg.MCPServers))

	// Connect and discover tools in the background.
	go a.connectMCPBackground(ctx, a.Config.FullConfig)

	return mgr, nil
}

// connectMCPBackground populates the manager's deferred pool, registers
// auto-load tools into the agent's registry, and attaches DeferredToolReminder.
// Runs in a background goroutine started by InitMCPAsync / initMCPAsync.
func (a *AIAgent) connectMCPBackground(ctx context.Context, cfg *config.Config) {
	defer a.Config.MCPManager.MarkInitDone()
	defer func() { a.Config.Logger.Info(ctx, "MCP: async init completed") }()

	autoLoad, all, errs := a.Config.MCPManager.PopulateFromConnect(ctx, cfg)
	for _, err := range errs {
		a.Config.Logger.Error(ctx, "MCP: load error", err)
	}
	a.SetMCPInitErrors(errs)
	if len(all) == 0 {
		a.Config.Logger.Info(ctx, "MCP: no tools discovered from any server")
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
		a.Config.Logger.Info(ctx, "MCP: tools auto-registered async", "count", len(autoLoad))
	}

	pool := a.Config.MCPManager.Pool()
	set := a.Config.MCPManager.DiscoveredSet()
	total := pool.Len()
	discovered := len(set.List())

	// Create DeferredToolReminder (always, for potential use via toggle)
	a.deferredToolReminder = &systemreminder.DeferredToolReminder{
		Provider: &deferredToolProviderAdapter{pool: pool},
		Tracker:  set,
	}

	// Register DeferredToolReminder only if there are undiscovered tools
	if discovered < total {
		a.Config.ReminderCollector.AddReminder(a.deferredToolReminder)
		a.Config.Logger.Info(ctx, "MCP: DeferredToolReminder added", "undiscovered", total-discovered, "total", total)
	}

	// Start background tool list refresher for HTTP MCP servers
	a.startMCPToolRefresher(ctx, cfg)
}

// WaitForMCP blocks until the background MCP initialization completes,
// or the context is cancelled / times out. Returns nil on success.
func (a *AIAgent) WaitForMCP(ctx context.Context) error {
	if a.Config.MCPManager == nil {
		return nil // MCP not configured
	}
	return a.Config.MCPManager.WaitInit(ctx)
}

// MCPReady returns a channel that's closed when MCP background init completes.
// If MCP is not configured, returns a pre-closed channel.
func (a *AIAgent) MCPReady() <-chan struct{} {
	if a.Config.MCPManager == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return a.Config.MCPManager.InitDone()
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
// using an explicit config instead of reading from a.Config.FullConfig.
func (a *AIAgent) buildReminderCollectorFrom(sysCfg SystemReminderConfig) {
	var reminders []systemreminder.Reminder

	// Always-on reminders.
	reminders = append(reminders,
		systemreminder.DateReminder{},
		systemreminder.ProjectContextReminder{},
		&systemreminder.BackgroundTaskReminder{
			Provider: &backgroundTaskProvider{pm: a.Config.ProcessManager},
		},
	)

	// Skill list reminder — only when skills are initialized (DisableSkills
	// leaves skillListReminder nil, and appending a nil Reminder would panic
	// on first Collect).
	if a.skillListReminder != nil {
		reminders = append(reminders, a.skillListReminder)
	}

	// Plan tracking reminder — only meaningful where SavePlan is available
	// (ACP sessions with a plan card UI).
	if a.Frontend.PlanToolEnabled {
		reminders = append(reminders, systemreminder.PlanTrackingReminder{})
	}

	// Git reminder (configurable).
	if sysCfg.GitReminder == nil || *sysCfg.GitReminder {
		reminders = append(reminders, systemreminder.GitReminder{})
	}

	// Pprof debug reminder — one shot per agent instance, carrying the actual
	// bound port (bootstrap may have auto-incremented it) and the process PID.
	if sysCfg.Pprof != nil && sysCfg.Pprof.Enabled {
		reminders = append(reminders, &systemreminder.PprofReminder{
			Enabled: sysCfg.Pprof.Enabled,
			Port:    sysCfg.Pprof.Port,
			PID:     os.Getpid(),
		})
	}

	// Memory recall reminder (only when memory backend is enabled).
	if a.Config.Memory != nil {
		limit := sysCfg.MemoryRecallLimit
		if limit <= 0 {
			limit = 5
		}
		reminders = append(reminders, systemreminder.MemoryRecallReminder{
			Backend: a.Config.Memory.Backend,
			Limit:   limit,
			Timeout: sysCfg.MemoryRecallTimeout,
		})
	}

	a.Config.ReminderCollector = systemreminder.NewCollector(reminders...)
}

// resolveKeywordProvider resolves the configured KeywordProvider from config
// and wires it into the TopicBackend's keyword extractor. This must be called
// AFTER configure() creates the memory backend.
//
// It is safe to call when memory is not configured or when no topic backend
// is in use — it checks a.Config.Memory before doing anything.
func (a *AIAgent) resolveKeywordProvider(cfg *config.Config) {
	if a.Config.Memory == nil || a.Provider() == nil {
		return
	}
	tb, ok := a.Config.Memory.Backend.(*memory.TopicBackend)
	if !ok {
		return
	}

	kpName := cfg.Memory.KeywordProvider
	if kpName == "" {
		return // no dedicated keyword provider configured; main provider is already set
	}

	resolved, err := llm.BuildProvider(cfg, kpName)
	if errors.Is(err, llm.ErrProviderNotFound) {
		a.Config.Logger.Info(context.Background(), "Memory: keyword_provider not found, falling back to main provider", "provider", kpName)
		return
	}
	if err != nil {
		a.Config.Logger.Error(context.Background(), "Memory: failed to resolve keyword provider, falling back to main provider", err, "provider", kpName)
		return
	}

	// Wrap the dedicated provider for usage billing (main provider is
	// already wrapped at construction), and anchor extraction to the
	// current session. The row's provider name comes from the provider
	// itself (BuildProvider-resolved).
	sp := wrapForUsage(resolved.Provider, a.usageRecorder(), cfg)
	ext := NewLLMKeywordExtractor(sp, resolved.Model, cfg.Memory.Timeout, a.Config.Logger)
	ext.SetSessionIDResolver(a.sessionID)
	tb.SetKeywordExtractor(ext)
	a.Config.Logger.Info(context.Background(), "Memory: keyword extractor re-wired with dedicated provider", "provider", kpName, "type", resolved.Type, "model", resolved.Model)
}
