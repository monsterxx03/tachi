package agent

import (
	"testing"

	"github.com/monsterxx03/tachi/llm"
)

// TestSetThinkingDropsUnsupportedEffort pins the guard that keeps a thinking
// level from outliving the provider it was chosen for: a session moved onto an
// endpoint that rejects it (/model, or a resumed session after the config
// changed) must not send an effort that fails the whole request.
func TestSetThinkingDropsUnsupportedEffort(t *testing.T) {
	declared := &llm.ResolvedProvider{
		Name:            "gpt-6-astra",
		Model:           "gpt-6-astra",
		ThinkingEfforts: []string{"low", "medium", "high", "xhigh"},
	}

	t.Run("declared provider drops a rejected effort", func(t *testing.T) {
		a := newBareTestAgent(t, nil, 10)
		a.Config.Resolved = declared

		a.SetThinking(nil, "max")
		if got := a.Config.Resolved.ThinkingEffort; got != "" {
			t.Errorf("ThinkingEffort = %q, want \"\" (provider default), not max", got)
		}
	})

	t.Run("declared provider keeps an accepted effort", func(t *testing.T) {
		a := newBareTestAgent(t, nil, 10)
		a.Config.Resolved = declared

		a.SetThinking(nil, "xhigh")
		if got := a.Config.Resolved.ThinkingEffort; got != "xhigh" {
			t.Errorf("ThinkingEffort = %q, want xhigh", got)
		}
	})

	t.Run("undeclared provider keeps every effort", func(t *testing.T) {
		a := newBareTestAgent(t, nil, 10)

		a.SetThinking(nil, "max")
		if got := a.Config.Resolved.ThinkingEffort; got != "max" {
			t.Errorf("ThinkingEffort = %q, want max (no declared set)", got)
		}
	})
}
