package agent

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/agent/subagent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The runtime reloads (agent_reload.go) and the two switches that are hot by construction. Each
// test pins the property that made the desktop's settings row go from 需重启 to 真生效 — and the
// counter-property that keeps it honest (an injected object must NOT be replaced, an unrelated
// change must NOT rebuild).

// TestCheckpointManagerKeyCarriesItsOptions: the manager bakes its Options in at construction
// (the two guards and the retention), so the per-session key that caches it has to contain them.
// Without that, changing checkpoints.max_files and restarting a turn kept serving the manager
// built with the old limit — the "I changed it and nothing happened" bug.
func TestCheckpointManagerKeyCarriesItsOptions(t *testing.T) {
	a, _, ctx := checkpointTestAgent(t)
	first := a.checkpointManager(ctx)
	require.NotNil(t, first)

	a.Config.FullConfig.Checkpoints.MaxFiles = 7
	if again := a.checkpointManager(ctx); again == first {
		t.Fatal("the cached manager was served after max_files changed: the new guard would never apply")
	}

	// The other half of the contract: an unrelated change must not throw the manager away (the
	// point of the cache is that a long session does not rebuild it every turn).
	second := a.checkpointManager(ctx)
	a.Config.FullConfig.Language = "zh"
	if again := a.checkpointManager(ctx); again != second {
		t.Error("an unrelated config change rebuilt the checkpoint manager")
	}
}

// TestReloadHooksRebuildsOnlyWhatConfigureBuilt covers the three states the reload has to tell
// apart: a dispatcher the FRONTEND injected (leave it), one built from config (replace it), and
// none at all (which is exactly the case where the user just added their first hook).
func TestReloadHooksRebuildsOnlyWhatConfigureBuilt(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})

	enabled := true
	withHook := func(cfg *config.Config) {
		cfg.Hooks.Enabled = &enabled
		cfg.Hooks.Events = map[string][]config.HookCommand{
			"tool_call": {{Command: "echo hi"}},
		}
	}

	// 1. "There was none" → the first hook must arrive.
	cfg := &config.Config{}
	withHook(cfg)
	a.Config.FullConfig = cfg
	require.NoError(t, a.ReloadHooks(cfg))
	require.NotNil(t, a.Config.HookDispatcher, "the first hook in the config must build a dispatcher")

	// 2. Disabled → cleared, not merely left behind (initHookSystemFrom assigns only when there
	// is something to dispatch).
	off := false
	cfg.Hooks.Enabled = &off
	require.NoError(t, a.ReloadHooks(cfg))
	assert.Nil(t, a.Config.HookDispatcher, "turning hooks off must clear the dispatcher")

	// 3. Injected → untouched. A dispatcher the caller handed us is its own; replacing it would
	// silently drop hooks we know nothing about.
	injected := hooks.NewDispatcher(a.Config.Logger)
	a.Config.HookDispatcher = injected
	a.hooksInjected = true
	withHook(cfg)
	require.NoError(t, a.ReloadHooks(cfg))
	assert.Same(t, injected, a.Config.HookDispatcher, "an injected dispatcher must survive a reload")
}

// TestReloadToolsRebuildsAndDropsSearch: RegisterTools only ADDS what the config justifies, so a
// reload has to remove the one built-in that can lose its justification — otherwise the model
// keeps calling a search tool that can no longer work.
func TestReloadToolsRebuildsAndDropsSearch(t *testing.T) {
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("BRAVE_API_KEY", "")
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})

	cfg := &config.Config{}
	cfg.WebSearch.Providers = []config.WebSearchProviderConfig{{Type: "exa", Key: "k"}}
	a.Config.FullConfig = cfg
	require.NoError(t, a.ReloadTools())
	if a.Config.ToolRegistry.GetTool(tools.ToolNameWebSearch) == nil {
		t.Fatal("a configured web_search was not registered")
	}

	// The key is what makes it work; drop it and the tool has to go.
	cfg.WebSearch.Providers = nil
	require.NoError(t, a.ReloadTools())
	if a.Config.ToolRegistry.GetTool(tools.ToolNameWebSearch) != nil {
		t.Error("web_search survived a reload that unconfigured it: the model would keep calling a dead tool")
	}
	// A tool that is NOT web_search is rebuilt rather than dropped (WebFetch needs no key).
	if a.Config.ToolRegistry.GetTool(tools.ToolNameWebFetch) == nil {
		t.Error("web_fetch disappeared on reload; only the search tool loses its justification")
	}
}

// TestGitReminderSwitchIsReadLive: the reminder is registered unconditionally and asks the agent
// per generation, which is what makes the switch hot WITHOUT rebuilding the collector (a rebuild
// would cost every other reminder its runtime state).
func TestGitReminderSwitchIsReadLive(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})

	a.Config.FullConfig = nil
	assert.True(t, a.gitReminderEnabled(), "no config = the default, which is on")

	cfg := &config.Config{}
	a.Config.FullConfig = cfg
	assert.True(t, a.gitReminderEnabled(), "unset = on, like every other *bool in the config")

	off := false
	cfg.SystemReminder.GitReminder = &off
	assert.False(t, a.gitReminderEnabled(), "the switch has to be read from the live config")

	on := true
	cfg.SystemReminder.GitReminder = &on
	assert.True(t, a.gitReminderEnabled())
}

// TestReloadFamilyIsIdempotent: every reload may be called when nothing changed (the settings
// page calls them for whatever it built, and a second save is a second call). Cheap to assert,
// and it is the property that makes "reload everything that might care" affordable.
func TestReloadFamilyIsIdempotent(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})
	cfg := &config.Config{}
	a.Config.FullConfig = cfg
	for range 2 {
		require.NoError(t, a.ReloadHooks(cfg))
		require.NoError(t, a.ReloadTools())
	}
	assert.Nil(t, a.Config.HookDispatcher, "a config with no hooks leaves no dispatcher behind")
	assert.NotNil(t, a.Config.ToolRegistry.GetTool(tools.ToolNameBash), "the built-ins are still there")
}

// TestReloadLSPRebuildsAndCleansUp: the manager is per agent and owns CHILD PROCESSES, so the
// two half-measures are the interesting ones — leaving the old manager running, and leaving the
// tools registered after the servers are gone.
func TestReloadLSPRebuildsAndCleansUp(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})
	ctx := context.Background()

	on := &config.Config{}
	on.LSP.Enabled = boolPtr(true)
	on.LSP.Servers = []config.LSPServerConfig{{Name: "go", Command: "gopls", Extensions: []string{".go"}}}
	require.NoError(t, a.ReloadLSP(ctx, on))
	require.NotNil(t, a.Config.LSPManager, "an enabled LSP block must build a manager")
	for _, name := range []string{tools.ToolNameLSP, tools.ToolNameLSPDiagnostics} {
		if a.Config.ToolRegistry.GetTool(name) == nil {
			t.Errorf("%s was not registered", name)
		}
	}
	first := a.Config.LSPManager

	// Rebuilding replaces the manager (the old one is shut down first), and the reminder follows
	// the NEW one — it cannot be re-registered, only re-pointed.
	on.LSP.MaxResults = 7
	require.NoError(t, a.ReloadLSP(ctx, on))
	if a.Config.LSPManager == first {
		t.Error("the manager was not rebuilt")
	}
	require.NotNil(t, a.lspReminder, "the reminder is created on first use")
	if a.lspReminder.Provider == nil {
		t.Error("the diagnostics reminder still points at nothing")
	}

	// Off: no manager, no tools, and a reminder that stays quiet.
	off := &config.Config{}
	off.LSP.Enabled = boolPtr(false)
	require.NoError(t, a.ReloadLSP(ctx, off))
	assert.Nil(t, a.Config.LSPManager)
	for _, name := range []string{tools.ToolNameLSP, tools.ToolNameLSPDiagnostics} {
		if a.Config.ToolRegistry.GetTool(name) != nil {
			t.Errorf("%s survived the LSP block being turned off", name)
		}
	}
	assert.Nil(t, a.lspReminder.Provider, "the reminder must be pointed at nil, not at a shut-down manager")
}

// TestReloadMemorySwapsBackendAndReminder: the memory tools and the recall reminder both hold the
// backend, so "memory off" has to unhook all three — a tool answering from a backend nobody
// replaced is the failure mode this pins.
func TestReloadMemorySwapsBackendAndReminder(t *testing.T) {
	oldBase := config.BaseDir()
	t.Cleanup(func() { config.SetBaseDir(oldBase) })
	config.SetBaseDir(t.TempDir())

	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})
	ctx := context.Background()
	// Give the agent a session manager, so the topic backend's temporal fallback has something
	// to be wired to (SetSessionManager does that at construction; ReloadMemory must re-do it
	// for the backend it builds).
	a.SetSessionManager(newTestSessionManager(t, "anthropic"))

	cfg := &config.Config{}
	cfg.Memory.Type = "topic"
	cfg.Memory.DecayHalfLifeDays = 7
	require.NoError(t, a.ReloadMemory(ctx, cfg))
	require.NotNil(t, a.Config.Memory)
	require.NotNil(t, a.Config.Memory.Backend)
	for _, name := range []string{tools.ToolNameRecordMemory, tools.ToolNameMemoryRecall} {
		if a.Config.ToolRegistry.GetTool(name) == nil {
			t.Errorf("%s was not registered", name)
		}
	}
	require.NotNil(t, a.memoryReminder, "the reminder is created on first use")
	if a.memoryReminder.Backend() == nil {
		t.Error("the reminder was not pointed at the new backend")
	}
	if tb, ok := a.Config.Memory.Backend.(*memory.TopicBackend); !ok {
		t.Fatalf("backend is %T, want *memory.TopicBackend", a.Config.Memory.Backend)
	} else if !tb.SessionProviderWired() {
		t.Error("the rebuilt backend lost its session provider: temporal queries would degrade silently")
	}
	first := a.Config.Memory.Backend

	// A different backend type replaces the first one, and the reminder follows it.
	cfg.Memory.DecayHalfLifeDays = 30
	require.NoError(t, a.ReloadMemory(ctx, cfg))
	if a.Config.Memory.Backend == first {
		t.Error("the backend was not rebuilt")
	}

	// SkipRecall is not derived from config — it belongs to the process (`tachi run` sets it)
	// and the loop reads it per turn, so a reload must carry it over rather than reset it.
	a.Config.Memory.SkipRecall = true
	cfg.Memory.DecayHalfLifeDays = 1
	require.NoError(t, a.ReloadMemory(ctx, cfg))
	assert.True(t, a.Config.Memory.SkipRecall, "a reload turned recall back on for a run that had suppressed it")

	// Off: everything goes, including the reminder's reference.
	cfg.Memory.Type = ""
	require.NoError(t, a.ReloadMemory(ctx, cfg))
	assert.Nil(t, a.Config.Memory)
	for _, name := range []string{tools.ToolNameRecordMemory, tools.ToolNameMemoryRecall} {
		if a.Config.ToolRegistry.GetTool(name) != nil {
			t.Errorf("%s survived memory being turned off", name)
		}
	}
	assert.Nil(t, a.memoryReminder.Backend(), "the reminder still points at the old backend")
}

// TestReloadSubagentKeepsAnInjectedRunner: the executor is rebuilt, but a runner the frontend
// handed us is not — the tool registered in its place points at that runner.
func TestReloadSubagentKeepsAnInjectedRunner(t *testing.T) {
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})
	cfg := &config.Config{}

	require.NoError(t, a.ReloadSubagent(cfg))
	require.NotNil(t, a.Config.SubagentRunner)
	if a.Config.ToolRegistry.GetTool(tools.ToolNameSubAgent) == nil {
		t.Error("the subagent tool was not registered")
	}

	injected := &subagent.Executor{}
	a.Config.SubagentRunner = injected
	a.subagentInjected = true
	require.NoError(t, a.ReloadSubagent(cfg))
	assert.Same(t, injected, a.Config.SubagentRunner, "an injected runner must survive a reload")
}

// TestReloadToolsKeepsAskUserGatedForANonInteractiveFrontend: RegisterTools registers
// AskUserQuestion unconditionally, while the CONSTRUCTOR drops it for a frontend that cannot
// answer it (PermissionModeSkip without AskUserEnabled: channel, `tachi -p`, subagents). A
// settings-page rebuild must not hand those agents a tool whose answer nobody can give — the
// gating has to be part of the reload, not only of construction.
func TestReloadToolsKeepsAskUserGatedForANonInteractiveFrontend(t *testing.T) {
	// newTestAgent is exactly the non-interactive shape (PermissionModeSkip, AskUserEnabled false).
	a := newTestAgent(t, &mockStreamProvider{name: "anthropic"})
	require.NoError(t, a.ReloadTools())
	if a.Config.ToolRegistry.GetTool(tools.ToolNameAskUser) != nil {
		t.Error("ReloadTools resurrected AskUserQuestion on a frontend that cannot answer it")
	}

	// The interactive frontends keep it (desktop / TUI / ACP).
	b := newTestAgent(t, &mockStreamProvider{name: "anthropic"})
	b.Config.PermissionMode = PermissionModeExternal
	b.Config.AskUserEnabled = true
	require.NoError(t, b.ReloadTools())
	if b.Config.ToolRegistry.GetTool(tools.ToolNameAskUser) == nil {
		t.Error("an interactive frontend must keep AskUserQuestion across a reload")
	}
}
