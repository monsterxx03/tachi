package main

import (
	"context"

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
	return nil
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
// using the shared desktop config/system prompt. Provider/thinking overrides are
// applied by the caller after construction. It returns the agent and its
// (disabled) MCP manager so the caller can track both for teardown.
func (d *desktopApp) buildAgentForSession(ctx context.Context, sm *session.Manager) (*agent.AIAgent, *mcp.Manager, error) {
	maxIters := config.DefaultMaxIterations
	if d.cfg != nil {
		if m := d.cfg.GetMaxIterations(); m > 0 {
			maxIters = m
		}
	}
	a, mcpMgr, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		MaxIterations:          maxIters,
		Logger:                 logger.New("desktop"),
		PermissionMode:         agent.PermissionModeSkip,
		DisableMCP:             true, // MCP wired in a later iteration
		DisableSkills:          true,
		DisableSystemReminders: true,
		FullConfig:             d.cfg,
		SystemConfig:           agent.SystemConfigFromConfig(d.cfg),
	})
	if err != nil {
		return nil, nil, err
	}
	if sm != nil {
		a.SetSessionManager(sm)
	}
	return a, mcpMgr, nil
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

// teardownAgent closes every per-session agent and its MCP manager, ensuring
// lifecycle events (e.g. session_end) are dispatched before the process exits.
func (d *desktopApp) teardownAgent() {
	d.mu.Lock()
	runs := make([]*sessionRun, 0, len(d.runs))
	for _, r := range d.runs {
		runs = append(runs, r)
	}
	d.mu.Unlock()
	for _, r := range runs {
		if r.agent != nil {
			r.agent.Close()
		}
		if r.mcp != nil {
			r.mcp.Close()
		}
	}
}
