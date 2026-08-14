package tui

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

// newTestAIAgent builds a minimal bare agent (no system setup) wired to a
// temp usage recorder, so provider calls never touch <home>/usage.
func newTestAIAgent(t *testing.T, provider llm.Provider, maxIter int) *agent.AIAgent {
	t.Helper()
	a, _, err := agent.NewAIAgentWithConfig(context.Background(), agent.AgentConfig{
		Resolved:      &llm.ResolvedProvider{Provider: provider},
		MaxIterations: maxIter,
		SkipConfigure: true,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	return a
}
