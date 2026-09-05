package main

import (
	"context"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// initAgent bootstraps the real tachi config and constructs an AIAgent bound to
// a session manager. On failure it logs and leaves d.aiAgent nil, so the UI
// falls back to simulated turns rather than crashing.
func (d *desktopApp) initAgent(ctx context.Context) error {
	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

	maxIters := cfg.GetMaxIterations()
	if maxIters <= 0 {
		maxIters = config.DefaultMaxIterations
	}

	aiAgent, mcpMgr, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		MaxIterations:          maxIters,
		Logger:                 logger.New("desktop"),
		PermissionMode:         agent.PermissionModeSkip,
		DisableMCP:             true, // S2: skip MCP connection; wired in S3
		DisableSkills:          true,
		DisableSystemReminders: true,
		FullConfig:             cfg,
		SystemConfig:           agent.SystemConfigFromConfig(cfg),
	})
	if err != nil {
		return err
	}

	sm, err := session.NewManager(nil)
	if err != nil {
		aiAgent.Close()
		return err
	}
	sm.SetMaxKeep(cfg.SessionCleanupMaxCount)
	aiAgent.SetSessionManager(sm)

	d.aiAgent = aiAgent
	d.mcp = mcpMgr
	d.sm = sm
	d.cfg = cfg
	d.systemPrompt = agent.BuildSystemPrompt(cfg.Language, "", "", cfg.ExtraSystemPrompt)
	return nil
}

// teardownAgent closes the MCP manager and the agent, ensuring lifecycle events
// (e.g. session_end) are dispatched before the process exits.
func (d *desktopApp) teardownAgent() {
	if d.mcp != nil {
		d.mcp.Close()
	}
	if d.aiAgent != nil {
		d.aiAgent.Close()
	}
}
