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
	d.systemPrompt = agent.BuildSystemPrompt(cfg.Language, "", "", cfg.ExtraSystemPrompt)
	d.sm = d.newSessionManager()
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
