package main

import (
	"context"
	"log"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// initAgent bootstraps the real tachi config. It does NOT construct the agent
// itself — agents are built lazily per session (see prepareSession) so multiple
// conversations run independently, mirroring channel's per-thread cachedAgent.
//
// d.sm is always created (even when bootstrap fails) so the sidebar can list and
// browse on-disk sessions; on failure d.cfg stays nil and turns fall back to the
// simulated path.
func (d *desktopApp) initAgent(ctx context.Context) error {
	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		d.cfg = nil
		d.sm = d.newSessionManager()
		return err
	}
	cfg := boot.Config
	d.cfg = cfg
	d.sm = d.newSessionManager()
	// The system prompt is deliberately NOT built here: it is session-derived
	// (working directory, ID), so systemPromptFor resolves it per turn — and that
	// builder is where the Mermaid capability of this frontend is declared.

	// Build the shared MCP manager (when any servers are configured) and connect
	// in the background. It is shared across all per-session agents; the
	// manager's per-session discovered sets keep each session's tool loading
	// isolated (mirrors channel's initSharedMCP).
	if cfg.MCPEnabled() {
		d.mcp = mcp.NewManager(ctx, cfg, logger.New("desktop"))
		go d.populateSharedMCP(ctx, d.mcp)
	}
	return nil
}

// systemPromptCacheMax pins the built-prompt memo (see systemPromptFor). Keys are
// the exact build inputs, so the pin only matters for a long-lived process that
// walks through many sessions and directories; when it is reached the map is
// dropped wholesale and rebuilt on demand.
const systemPromptCacheMax = 64

// promptKey identifies a built system prompt. Both parts are build inputs, so a
// miss and a change are the same event.
type promptKey struct {
	cwd string
	id  string
}

// systemPromptFor returns the system prompt to send for session id, built from
// the session's CURRENT working directory and ID.
//
// The prompt cannot be a startup singleton the way a single-session TUI's is
// (there the process cwd IS the session directory, thanks to os.Chdir). One
// desktop process hosts several sessions, each pointing at its own tree, and a
// GUI's process cwd is not a working directory at all — macOS hands a
// Finder-launched app "/" — so a prompt built once would advertise "/" forever
// while the tools (wdctx) and @-file references already resolved against the
// session's directory. Resolving on demand keeps the advertised directory, the
// tool root and the @-file root the same thing.
//
// The cache is keyed on the resolved (working directory, session ID) pair, which
// is what makes a directory change take effect on the next turn with no
// invalidation hook: the new directory is a new key, so it rebuilds. The build
// is not free — it probes git (shutil in agent.buildSystemPrompt) — hence the
// memo for the unchanged case.
//
// Callers must NOT hold d.mu: the working directory is read through it, and the
// build is far too slow to run under it.
//
// The Mermaid capability is declared here because this frontend renders diagrams
// (and a zoomable overlay for them).
func (d *desktopApp) systemPromptFor(id string) string {
	if d.cfg == nil {
		return ""
	}
	cwd := d.sessionWorkDir(id)
	if cwd == "" {
		// A session that never picked a folder runs its tools in the process cwd
		// (wdctx's fallback, and the @-file root's). Say so, instead of letting
		// BuildSystemPrompt walk up to a git root the tools would never use.
		cwd = processCWD()
	}
	key := promptKey{cwd: cwd, id: id}

	d.promptMu.Lock()
	cached, ok := d.promptCache[key]
	d.promptMu.Unlock()
	if ok {
		return cached
	}

	prompt := agent.BuildSystemPrompt(d.cfg.Language, cwd, id, d.cfg.ExtraSystemPrompt,
		agent.WithFrontendCapabilities(agent.MermaidCapabilityPrompt))

	d.promptMu.Lock()
	defer d.promptMu.Unlock()
	if d.promptCache == nil || len(d.promptCache) >= systemPromptCacheMax {
		d.promptCache = make(map[promptKey]string)
	}
	d.promptCache[key] = prompt
	return prompt
}

// populateSharedMCP connects all configured MCP servers in the background and
// fills the manager's deferred pool / per-session discovered sets. Errors are
// logged; partial discovery is acceptable (mirrors channel's populateSharedMCP).
func (d *desktopApp) populateSharedMCP(ctx context.Context, mgr *mcp.Manager) {
	defer mgr.MarkInitDone()
	_, _, errs := mgr.PopulateFromConnect(ctx, d.cfg)
	for _, err := range errs {
		log.Printf("desktop MCP: load error: %v", err)
	}
}

// newSessionManager creates a fresh session manager honoring the configured
// cleanup cap. It is the per-run analog of channel's newSessionManager.
func (d *desktopApp) newSessionManager() *session.Manager {
	sm, err := session.NewManager(nil)
	if err != nil {
		return nil
	}
	if d.cfg != nil {
		sm.SetMaxKeep(d.cfg.SessionCleanupMaxCount)
	}
	return sm
}

// buildAgentForSession constructs an AIAgent wired to the given session manager,
// using the shared desktop config/system prompt and the SHARED MCP manager (so
// multiple sessions reuse one MCP connection layer; per-session tool loading is
// isolated by the manager's discovered sets). It returns the agent; the caller
// applies provider/thinking overrides.
func (d *desktopApp) buildAgentForSession(ctx context.Context, sm *session.Manager) (*agent.AIAgent, error) {
	maxIters := config.DefaultMaxIterations
	if d.cfg != nil {
		if m := d.cfg.GetMaxIterations(); m > 0 {
			maxIters = m
		}
	}
	a, _, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		MaxIterations:          maxIters,
		Logger:                 logger.New("desktop"),
		PermissionMode:         agent.PermissionModeSkip,
		AskUserEnabled:         true,         // the desktop renders question forms itself
		DisableMCP:             d.mcp == nil, // MCP enabled only when a shared manager exists
		DisableSkills:          true,
		DisableSystemReminders: true,
		MCPManager:             d.mcp, // shared manager (nil when no MCP configured)
		FullConfig:             d.cfg,
		SystemConfig:           agent.SystemConfigFromConfig(d.cfg),
	})
	if err != nil {
		return nil, err
	}
	if sm != nil {
		a.SetSessionManager(sm)
	}
	return a, nil
}

// applyThinking configures the given agent's thinking level from a session's
// stored value. "" / "default" means DON'T set it — follow the provider's own
// default; "none" disables thinking. Mirrors SetThinkingLevel's semantics.
func applyThinking(a *agent.AIAgent, level string) {
	switch level {
	case "none":
		f := false
		a.SetThinking(&f, "")
	case "", "default":
		a.SetThinking(nil, "")
	default:
		t := true
		a.SetThinking(&t, level)
	}
}

// teardownAgent closes every per-session agent and the shared MCP manager,
// ensuring lifecycle events (e.g. session_end) are dispatched before exit.
func (d *desktopApp) teardownAgent() {
	d.mu.Lock()
	runs := make([]*sessionRun, 0, len(d.runs))
	for _, r := range d.runs {
		runs = append(runs, r)
	}
	mcp := d.mcp
	d.mu.Unlock()
	for _, r := range runs {
		if r.agent != nil {
			r.agent.Close()
		}
	}
	if mcp != nil {
		mcp.Close()
	}
}
