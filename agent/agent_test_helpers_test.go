package agent

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/llm"
)

// newBareTestAgent builds a minimal bare agent for tests that don't need the
// full NewAIAgentWithConfig pipeline: SkipConfigure keeps built-in tools /
// skills / reminders out (tests register what they need), and a temp usage
// recorder keeps provider calls off <home>/usage.
func newBareTestAgent(t *testing.T, provider llm.Provider, maxIter int) *AIAgent {
	t.Helper()
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
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
