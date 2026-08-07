package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// TestDisabledReminderCollector_NoOp verifies the no-op collector used by
// non-interactive modes never produces output, even after AddReminder
// registers reminders that would otherwise fire.
func TestDisabledReminderCollector_NoOp(t *testing.T) {
	c := disabledReminderCollector{}

	// DateReminder fires on every first user message — must stay silent.
	if got := c.Collect(context.Background(), systemreminder.Context{IsFirstMessage: true}); got != "" {
		t.Fatalf("Collect = %q, want empty", got)
	}

	// AddReminder must be inert: registering a real reminder changes nothing.
	c.AddReminder(systemreminder.DateReminder{})
	if got := c.Collect(context.Background(), systemreminder.Context{IsFirstMessage: true}); got != "" {
		t.Fatalf("Collect after AddReminder = %q, want empty", got)
	}
}

// TestConfigure_DisableSystemReminders ensures the agent configured with the
// flag collects zero reminders on a first-message context (where DateReminder
// / GitReminder / ProjectContextReminder would otherwise fire), while the
// collector stays non-nil so later AddReminder calls (LSP diagnostics,
// deferred MCP tools) remain safe.
func TestConfigure_DisableSystemReminders(t *testing.T) {
	full := config.DefaultConfig()
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:               &config.ResolvedProvider{Provider: &mockStreamProvider{name: "openai"}},
		Logger:                 logger.Default(),
		FullConfig:             full,
		SystemConfig:           SystemConfigFromConfig(full),
		DisableSystemReminders: true,
	})
	require.NoError(t, err)
	defer a.Close()

	block := a.collectReminders(context.Background(), a.buildReminderContext(true, false))
	assert.Empty(t, block)
	assert.NotNil(t, a.Config.ReminderCollector)

	// Tool-result boundary must also stay silent.
	block = a.collectReminders(context.Background(), a.buildReminderContext(false, true))
	assert.Empty(t, block)
}
