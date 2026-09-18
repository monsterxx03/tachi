package agent

import (
	"context"
	"errors"

	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/subagent"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
)

// Runtime reloads: how a change to the live *config.Config reaches objects that were built
// from it at construction time.
//
// The desktop's settings table (desktop/settings.go) decides WHICH keys need this (`applyPush`)
// and calls these methods for every live session; the knowledge of WHAT has to be rebuilt
// belongs here, next to the code that built it. Each method follows the same contract:
//
//   - it only does the PUSH half. The value itself is already in the shared config object, put
//     there by the caller — a reload that also wrote it would be a second writer of one leaf.
//   - it is idempotent and safe to call when nothing changed: an extra rebuild costs one
//     construction, which is what makes "just reload everything that might care" affordable.
//   - it does NOT touch anything a FRONTEND injected (AgentConfig fields): those belong to the
//     caller, and replacing one would be us overriding its decision (see ReloadHooks).
//
// Four invariants the whole family shares — they are what to carry away from this file:
//
//  1. A REMINDER cannot be replaced (Collector.AddReminder only appends), so the agent holds
//     LSPDiagnosticsReminder / MemoryRecallReminder itself and re-points them
//     (SetProvider / SetBackend); nil means off. Those setters are synchronized — they are
//     called from the settings goroutine while the turn goroutine generates.
//  2. A frontend-injected object is not ours to replace (`hooksInjected` / `subagentInjected`).
//  3. A rebuild that owns a child process or an external resource tears the old one down FIRST
//     (ReloadLSP → Shutdown, or its servers are orphaned).
//  4. Shared-layer facts (the MCP manager and its pool/refresher) are driven ONCE from the app,
//     never per session.
//
// ⚠️ Reloads run on the settings-save goroutine while a turn may be in flight for the same
// agent, and the fields they swap (Config.HookDispatcher, Memory, LSPManager, SubagentRunner,
// Config.Resolved) are read by that turn without a lock. Pointer and int fields do not tear in
// practice; INTERFACE fields (SubagentRunner) and reminder internals are the sharp edges, and
// the reminder setters above are guarded for exactly that reason. Closing the window entirely
// means deferring the push for a session whose turn is in flight (AIAgent.TurnInFlight) — until
// then this is an accepted, documented risk, not an oversight.

// ReloadHooks rebuilds the hook dispatcher from cfg.
//
// A dispatcher the frontend injected via AgentConfig.HookDispatcher is left alone
// (`hooksInjected` records which of the two this agent has): replacing it would silently drop
// a caller's own hooks the first time somebody saved an unrelated setting. Everything else is
// rebuilt, including the "there was none" case — a user adding their FIRST hook in the settings
// page is exactly the case this has to cover.
//
// Disabling hooks must CLEAR the dispatcher: initHookSystemFrom only assigns when there is
// something to dispatch, so without the explicit nil the previous dispatcher would keep firing
// commands the user just turned off.
func (a *AIAgent) ReloadHooks(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("ReloadHooks: no config")
	}
	if a.hooksInjected {
		return nil
	}
	a.Config.HookDispatcher = nil
	sysCfg := SystemConfigFromConfig(cfg)
	a.initHookSystemFrom(&sysCfg) // assigns one again when the config has any
	return nil
}

// ReloadTools rebuilds the built-in tools from the live config and re-registers them.
//
// RegisterTools is the whole rebuild (it reads cfg.ToolResult / WebSearch / WebFetch / FileEdit
// / SavePlan gating and constructs fresh tool objects), and Registry.Register replaces by name,
// so calling it again is the update. Two consequences worth knowing:
//
//   - a tool instance captured earlier keeps working on its old config (the registry is what a
//     new call resolves); a turn already in flight therefore finishes under the old settings,
//     which is the same deal as every other reload here.
//   - RegisterTools only ADDS what the config justifies — nothing removes what it no longer
//     justifies. WebSearch is the one built-in that can lose its justification (it is only
//     registered when a provider with a key is configured), so it is dropped explicitly below;
//     the model must not keep calling a tool that cannot work.
//   - the rebuild re-registers AskUserQuestion, which the CONSTRUCTOR drops for a frontend that
//     cannot answer it (channel / `-p` / subagents). That gating is re-applied here, or a saved
//     setting would hand those agents a tool whose answer nobody can give.
func (a *AIAgent) ReloadTools() error {
	cfg := a.Config.FullConfig
	a.RegisterTools()
	if cfg != nil && !tools.NewWebSearchTool(webSearchToolConfig(cfg)).Configured() {
		a.UnregisterTool(tools.ToolNameWebSearch)
	}
	a.applyAskUserGating()
	return nil
}

// webSearchToolConfig is the one place a search tool's config is assembled, so RegisterTools
// and ReloadTools cannot drift.
func webSearchToolConfig(cfg *config.Config) tools.WebSearchToolConfig {
	return tools.WebSearchToolConfig{
		Providers:  cfg.WebSearch.Providers,
		Timeout:    cfg.WebSearch.Timeout,
		MaxResults: cfg.WebSearch.MaxResults,
		Proxy:      cfg.WebSearch.Proxy,
	}
}

// --- the per-subsystem rebuilds ---
//
// These three share a shape the collector forces on us: a reminder is registered for the agent's
// whole life and CANNOT be replaced (AddReminder only appends), so the object a reminder POINTS
// AT is what gets swapped. That is why the agent holds the two reminder instances itself
// (lspReminder / memoryReminder) and why their setters exist.

// lspReminderFor returns the agent's diagnostics reminder, creating it on first use.
//
// register is false only while configure() is assembling the collector from a slice (the
// collector does not exist yet); everywhere else it is true, which is how a reminder that
// appears LATER — the user just turned LSP on — reaches the live collector.
func (a *AIAgent) lspReminderFor(register bool) *systemreminder.LSPDiagnosticsReminder {
	if a.lspReminder != nil {
		return a.lspReminder
	}
	a.lspReminder = &systemreminder.LSPDiagnosticsReminder{}
	if register && a.Config.ReminderCollector != nil {
		a.Config.ReminderCollector.AddReminder(a.lspReminder)
	}
	return a.lspReminder
}

// memoryReminderFor is lspReminderFor for the memory-recall reminder.
func (a *AIAgent) memoryReminderFor(register bool) *systemreminder.MemoryRecallReminder {
	if a.memoryReminder != nil {
		return a.memoryReminder
	}
	a.memoryReminder = &systemreminder.MemoryRecallReminder{}
	if register && a.Config.ReminderCollector != nil {
		a.Config.ReminderCollector.AddReminder(a.memoryReminder)
	}
	return a.memoryReminder
}

// initLSP builds the language-server manager for cfg and points the two tools and the diagnostics
// reminder at it. Shared by configure() and ReloadLSP: the reload's extra work is the TEARDOWN
// (see ReloadLSP), not the build.
func (a *AIAgent) initLSP(ctx context.Context, cfg config.LSPConfig) {
	if !cfg.IsEnabled() || len(cfg.Servers) == 0 {
		return
	}
	lspCfg := convertLSPConfig(&cfg)
	a.Config.LSPManager = lsp.NewManager(lspCfg, a.Config.Logger)
	a.RegisterTool(tools.NewLSPTool(a.Config.LSPManager))
	a.RegisterTool(tools.NewLSPDiagnosticsTool(a.Config.LSPManager))
	a.lspReminderFor(true).SetProvider(a.Config.LSPManager)
	a.Config.Logger.Info(ctx, "LSP: initialized", "servers", len(lspCfg.Servers))
}

// ReloadLSP rebuilds the language servers from cfg.
//
// The teardown cannot be skipped: a manager left alive keeps its child processes running while
// nothing talks to them, so Shutdown goes first. When the new config has no servers, the two
// tools are unregistered and the reminder is pointed at nil (its Generate returns early) — the
// reminder itself stays in the collector, which cannot remove it.
func (a *AIAgent) ReloadLSP(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("ReloadLSP: no config")
	}
	if old := a.Config.LSPManager; old != nil {
		old.Shutdown(ctx)
		a.Config.LSPManager = nil
	}
	a.UnregisterTool(tools.ToolNameLSP)
	a.UnregisterTool(tools.ToolNameLSPDiagnostics)
	if !cfg.LSP.IsEnabled() || len(cfg.LSP.Servers) == 0 {
		if a.lspReminder != nil {
			a.lspReminder.SetProvider(nil)
		}
		return nil
	}
	a.initLSP(ctx, cfg.LSP)
	return nil
}

// initMemoryBackend builds the memory backend from cfg and wires the keyword extractor into it.
// Shared by configure() and ReloadMemory.
func (a *AIAgent) initMemoryBackend(ctx context.Context, memCfg config.MemoryConfig) error {
	// BaseDir is runtime-only (yaml:"-") — inject the state base dir here.
	memCfg.BaseDir = config.BaseDir()
	backend, err := memory.New(memCfg.Type, memCfg, a.Config.Logger)
	if err != nil {
		return err
	}
	a.Config.Memory = &MemoryState{Backend: backend}
	a.Config.Logger.Info(ctx, "Memory: using backend", "type", memCfg.Type)

	// Wire keyword extractor for topic backend. Requires an LLM provider — skip when nil
	// (e.g. `tachi tools`).
	if tb, ok := backend.(*memory.TopicBackend); ok && a.Provider() != nil {
		ext := NewLLMKeywordExtractor(a.Provider(), a.Model(), memCfg.Timeout, a.Config.Logger)
		ext.SetSessionIDResolver(a.sessionID)
		tb.SetKeywordExtractor(ext)
		a.Config.Logger.Info(ctx, "Memory: keyword extractor wired for topic backend")
	}
	return nil
}

// registerMemoryTools registers the two memory tools, or removes them when there is no backend —
// one place, so "memory is off" cannot leave a tool that answers from a backend nobody built.
func (a *AIAgent) registerMemoryTools() {
	if a.Config.Memory != nil && a.Config.Memory.Backend != nil {
		a.RegisterTool(tools.NewRecordMemoryTool(a))
		a.RegisterTool(tools.NewMemoryRecallTool(a.Config.Memory.Backend))
		return
	}
	a.UnregisterTool(tools.ToolNameRecordMemory)
	a.UnregisterTool(tools.ToolNameMemoryRecall)
}

// ReloadMemory swaps the memory backend for the one cfg describes.
//
// The order matters: the old backend is unhooked FIRST (its tools unregistered, the reminder
// pointed at nil), because a backend that is being replaced must not answer one more recall —
// and because initMemoryBackend can fail, in which case the agent is left with memory off
// rather than with a half-installed pair.
//
// A rebuilt backend is not the same object as a constructed one: two things are attached to
// it by callers that run AFTER configure (the dedicated keyword provider, and the topic
// backend's session fallback) and one flag lives only in memory, so all three are re-applied
// below. Dropping them is a silent degradation — the settings page promises `memory.*` is hot.
func (a *AIAgent) ReloadMemory(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("ReloadMemory: no config")
	}
	// SkipRecall is the one part of MemoryState that is NOT derived from config: `tachi run`
	// sets it, and the loop reads it per turn (agent_loop.go). A reload must not quietly turn
	// recall back on.
	skipRecall := a.Config.Memory != nil && a.Config.Memory.SkipRecall
	a.Config.Memory = nil
	a.registerMemoryTools()
	if a.memoryReminder != nil {
		a.memoryReminder.SetBackend(nil)
	}
	if cfg.Memory.Type == "" {
		return nil
	}
	if err := a.initMemoryBackend(ctx, cfg.Memory); err != nil {
		return err
	}
	// The three re-attachments, in one place so neither can be forgotten alone: the runtime
	// flag, the dedicated keyword provider, and the session fallback (see the method comment).
	a.Config.Memory.SkipRecall = skipRecall
	a.resolveKeywordProvider(cfg)
	a.wireMemorySessionProvider(a.Config.SessionManager)
	a.registerMemoryTools()
	limit := cfg.Memory.RecallLimit
	if limit <= 0 {
		limit = 5
	}
	r := a.memoryReminderFor(true)
	r.SetPolicy(limit, cfg.Memory.Timeout)
	r.SetBackend(a.Config.Memory.Backend)
	return nil
}

// newSubagentExecutor builds and wires an executor — one place, so a reload cannot drift from
// construction (the worktree being the line that is easy to forget).
func (a *AIAgent) newSubagentExecutor(sysCfg config.SubagentConfig) *subagent.Executor {
	executor := subagent.NewExecutor(a, sysCfg)
	if sysCfg.Worktree {
		executor.EnableWorktree(a.Config.Logger)
	}
	return executor
}

// ReloadSubagent rebuilds the subagent executor (and its dedicated provider) from cfg.
//
// A runner the frontend injected via AgentConfig is left alone (`subagentInjected`): the tool
// registered in its place points at that runner, and replacing it would silently take the
// caller's implementation out of the loop. The executor holds its own concurrency semaphore, so
// an in-flight subagent finishes under the old one — the same deal as every reload here.
func (a *AIAgent) ReloadSubagent(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("ReloadSubagent: no config")
	}
	if a.subagentInjected {
		return nil
	}
	a.SetupSubagentProvider(cfg)
	executor := a.newSubagentExecutor(cfg.Subagent)
	a.Config.SubagentRunner = executor
	a.RegisterTool(tools.NewSubagentTool(executor))
	return nil
}

// ReloadMCPToolRefresh stops and restarts the background tool-list poller from cfg.
//
// The manager is SHARED (one per process) while an AIAgent is one per session, so this must be
// driven once, from the app — not once per session: StartRefresher re-points the shared
// poller's callback at whichever agent calls it, and the refresh then updates that agent's
// registry. Driving it from every session would leave the poller bound to whichever ran last.
func (a *AIAgent) ReloadMCPToolRefresh(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("ReloadMCPToolRefresh: no config")
	}
	mgr := a.Config.MCPManager
	if mgr == nil {
		return nil // MCP is off; nothing polls
	}
	mgr.StopRefresher()
	a.startMCPToolRefresher(ctx, cfg)
	return nil
}
