package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// TestConfigure_DisableMCPAndSkills verifies that the DisableMCP and
// DisableSkills switches (used by non-interactive modes like `tachi -p`)
// skip MCP server connection and skill discovery entirely: no MCP manager,
// no skill store, and neither the Skill nor MCPSearchTools tools registered.
func TestConfigure_DisableMCPAndSkills(t *testing.T) {
	full := config.DefaultConfig()
	sysCfg := SystemConfigFromConfig(full)
	// Configure a server that would otherwise trigger async MCP init —
	// DisableMCP must skip it without attempting any connection.
	sysCfg.MCPServers = []config.MCPServerConfig{
		{Name: "test-server", Type: config.MCPTransportStdio, Command: "/bin/echo"},
	}

	a, mgr, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:               &config.ResolvedProvider{Provider: &mockStreamProvider{name: "openai"}},
		Logger:                 logger.Default(),
		FullConfig:             full,
		SystemConfig:           sysCfg,
		DisableSystemReminders: true,
		DisableMCP:             true,
		DisableSkills:          true,
	})
	require.NoError(t, err)
	defer a.Close()

	assert.Nil(t, mgr, "MCP manager must not be created when DisableMCP is set")
	assert.Nil(t, a.Config.MCPManager)
	assert.Nil(t, a.Config.SkillStore)
	assert.Nil(t, a.Config.ToolRegistry.GetTool(tools.ToolNameSkill))
	assert.Nil(t, a.Config.ToolRegistry.GetTool(tools.ToolNameMCPSearchTools))

	// Reminder collector must still be non-nil (no-op in this mode) so
	// later AddReminder calls stay safe.
	assert.NotNil(t, a.Config.ReminderCollector)
}

// TestConfigure_DisableSkills_WithFullReminders exercises the combination
// of DisableSkills with a normal (non-disabled) reminder collector — e.g.
// `tachi commit`, which keeps system reminders but skips skill discovery.
// The nil SkillListReminder must not panic on collect.
func TestConfigure_DisableSkills_WithFullReminders(t *testing.T) {
	full := config.DefaultConfig()
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &config.ResolvedProvider{Provider: &mockStreamProvider{name: "openai"}},
		Logger:        logger.Default(),
		FullConfig:    full,
		SystemConfig:  SystemConfigFromConfig(full),
		DisableMCP:    true,
		DisableSkills: true,
	})
	require.NoError(t, err)
	defer a.Close()

	assert.Nil(t, a.Config.SkillStore)
	assert.Nil(t, a.Config.ToolRegistry.GetTool(tools.ToolNameSkill))

	// First-message context fires DateReminder / GitReminder /
	// ProjectContextReminder — must not panic with skillListReminder nil.
	block := a.collectReminders(context.Background(), a.buildReminderContext(true, false))
	assert.NotEmpty(t, block)
}
