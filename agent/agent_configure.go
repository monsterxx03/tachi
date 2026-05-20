package agent

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

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
	// --- reminders ---
	a.baseReminders = []systemreminder.Reminder{
		systemreminder.DateReminder{},
		systemreminder.ProjectContextReminder{},
		systemreminder.IterationWarningReminder{Threshold: cfg.SystemReminder.IterationWarningThreshold},
		systemreminder.TokenWarningReminder{ThresholdPct: cfg.SystemReminder.TokenWarningThresholdPct},
	}
	if cfg.SystemReminder.GitReminder == nil || *cfg.SystemReminder.GitReminder {
		a.baseReminders = append(a.baseReminders, systemreminder.GitReminder{})
	}

	// --- Memory backend (before skills — rebuildSkillCollector reads a.memoryBackend) ---
	if cfg.Memory.Type != "" {
		// Parse timeouts from config strings, with sensible defaults
		timeout := 10 * time.Second
		if cfg.Memory.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Memory.Timeout); err == nil && d > 0 {
				timeout = d
			}
		}
		reqTimeout := 15 * time.Second
		if cfg.Memory.Mem9.RequestTimeout != "" {
			if d, err := time.ParseDuration(cfg.Memory.Mem9.RequestTimeout); err == nil && d > 0 {
				reqTimeout = d
			}
		}

		memCfg := memory.Config{
			Type:         cfg.Memory.Type,
			BaseDir:      config.BaseDir(),
			Timeout:      timeout,
			ExcludeRepos: cfg.Memory.ExcludeRepos,
			Mem9: memory.Mem9Config{
				APIURL:         cfg.Memory.Mem9.APIURL,
				APIKey:         cfg.Memory.Mem9.APIKey,
				AgentID:        cfg.Memory.Mem9.AgentID,
				Mode:           cfg.Memory.Mem9.Mode,
				Proxy:          cfg.Memory.Mem9.Proxy,
				RequestTimeout: reqTimeout,
			},
		}
		backend, err := memory.New(cfg.Memory.Type, memCfg)
		if err != nil {
			a.logger.Log("Memory: failed to init %s backend: %v", cfg.Memory.Type, err)
		} else {
			a.memoryBackend = backend
			a.memoryTimeout = timeout
			a.excludeRepos = normalizeRepoPaths(cfg.Memory.ExcludeRepos)
			a.logger.Log("Memory: using %s backend", cfg.Memory.Type)
		}
	}

	// --- Skill system ---
	a.initSkills()

	// --- built-in tools + web search ---
	a.RegisterTools()
	ws := tools.WebSearchTool{
		ProviderType: cfg.WebSearch.Type,
		APIKey:       cfg.WebSearch.Key,
		Timeout:      cfg.WebSearch.Timeout,
		MaxResults:   cfg.WebSearch.MaxResults,
		Proxy:        cfg.WebSearch.Proxy,
	}
	if _, key := ws.ResolveProvider(); key != "" {
		a.RegisterTool(&ws)
	}

	// WebFetch — always registered, no API key needed.
	wf := tools.WebFetchTool{
		Timeout: cfg.WebFetch.Timeout,
		Proxy:   cfg.WebFetch.Proxy,
	}
	a.RegisterTool(&wf)

	// --- RecordMemory tool (only when memory backend is configured) ---
	if a.memoryBackend != nil {
		a.RegisterTool(tools.NewRecordMemoryTool(a))
		a.logger.Log("Memory: RecordMemory tool registered")
	}

	// --- MCP servers ---
	var mgr *mcp.Manager
	if cfg.MCPEnabled() {
		mgr = mcp.NewManager()
		mgr.SetLogger(a.logger)
		mcpTools, errs := mgr.ConnectAll(ctx, cfg.MCPServers)
		for _, err := range errs {
			a.logger.Log("MCP: load error: %v", err)
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}

		// Store all MCP tools for search and execution
		a.deferredPool = mcp.NewDeferredPool()
		a.discoveredSet = mcp.NewDiscoveredSet()

		// Build server config lookup (name -> config)
		serverCfgs := make(map[string]config.MCPServerConfig, len(cfg.MCPServers))
		for _, srv := range cfg.MCPServers {
			serverCfgs[srv.Name] = srv
		}

		// Decide whether ToolSearch is active
		useToolSearch := cfg.MCPToolSearch.IsEnabled() && len(mcpTools) > cfg.MCPToolSearch.MinToolsForSearch

		for _, t := range mcpTools {
			// Check for per-server overrides
			srvCfg, hasCfg := serverCfgs[t.ServerName()]

			// Apply search hint override from config
			var searchHint string
			if hasCfg && srvCfg.SearchHints != nil {
				searchHint = srvCfg.SearchHints[t.ToolName()]
			}

			// Store in deferred pool (always — needed for search and Invoke fallback)
			dt := mcp.NewDeferredToolFromMCPTool(t, searchHint)
			a.deferredPool.Add(dt)

			// Auto-load when ToolSearch is disabled or tool is in always_load list
			autoLoad := !useToolSearch
			if !autoLoad && hasCfg {
				if slices.ContainsFunc(srvCfg.AlwaysLoadTools, func(name string) bool {
					return strings.EqualFold(name, t.ToolName())
				}) {
					autoLoad = true
				}
			}

			// Lazy registration: only register in Registry when auto-loading.
			// Non-auto-loaded tools stay only in deferredPool and are registered
			// on first use (via executeToolCallsSequential fallback) or when
			// discovered through MCPSearchTools.
			if autoLoad {
				a.RegisterTool(t)
				a.discoveredSet.Add(t.Name())
			}

			a.logger.Log("MCP: %s tool %s (auto-load=%v)", map[bool]string{true: "registered", false: "deferred"}[autoLoad], t.Name(), autoLoad)
		}

		// Log summary
		total := a.deferredPool.Len()
		discovered := len(a.discoveredSet.List())
		if useToolSearch {
			a.logger.Log("MCP: ToolSearch active — %d/%d tools loaded (threshold=%d)",
				discovered, total, cfg.MCPToolSearch.MinToolsForSearch)
		} else {
			a.logger.Log("MCP: all %d tools loaded (ToolSearch disabled or below threshold)", total)
		}

		// Register the MCPSearchTools tool
		searchTool := tools.NewMCPSearchToolsTool(a.deferredPool, a.discoveredSet)
		a.RegisterTool(searchTool)
		a.logger.Log("MCP: registered MCPSearchTools tool")

		// Register DeferredToolReminder only if there are undiscovered tools
		if discovered < total {
			a.baseReminders = append(a.baseReminders, systemreminder.DeferredToolReminder{
				Provider: &deferredToolProviderAdapter{pool: a.deferredPool},
				Tracker:  a.discoveredSet,
			})
			a.rebuildSkillCollector()
		}
	}

	// --- SubAgent tool ---
	a.SetupSubagentProvider(cfg)
	executor := subagent.NewExecutor(a, cfg.Subagent)
	if cfg.Subagent.Worktree {
		executor.EnableWorktree(a.logger)
	}
	a.RegisterTool(tools.NewSubagentTool(executor))

	return mgr, nil
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
	for i := len(sessionMsgs) - 1; i >= 0; i-- {
		if sessionMsgs[i].Type == session.MessageTypeAssistant && sessionMsgs[i].Usage != nil {
			a.lastInputTokens = sessionMsgs[i].Usage.InputTokens
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
	return llmMsgs, sessionMsgs, latest, nil
}
