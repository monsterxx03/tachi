package main

import (
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// newThinkingTestAgent builds a real agent whose FullConfig carries the given
// provider spec, so applyThinking's re-resolve has a config to read (see
// applyThinking). No traffic: llm.NewProvider only constructs a client.
func newThinkingTestAgent(t *testing.T, spec config.ModelSpec) *agent.AIAgent {
	t.Helper()
	cfg := &config.Config{
		Provider: "astra",
		Providers: []config.ProviderConfig{{
			Name:   "astra",
			Type:   "openai-res",
			Model:  "gpt-6-astra",
			APIKey: "k",
			Spec:   spec,
		}},
	}
	prov, err := llm.NewProvider("openai-res", "k", "https://api.test", "gpt-6-astra")
	if err != nil {
		t.Fatalf("llm.NewProvider: %v", err)
	}
	a, _, err := agent.NewAIAgentWithConfig(t.Context(), agent.AgentConfig{
		Resolved:      &llm.ResolvedProvider{Provider: prov, Name: "astra"},
		FullConfig:    cfg,
		Logger:        logger.Default(),
		SkipConfigure: true,
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	return a
}

// TestApplyThinkingKeepsProviderDefault pins the bug that hid the provider's
// thinking_level: a session with NO override used to have the agent's resolved
// effort CLEARED, so a provider configured with thinking_level: high went out
// with no effort at all — invisible until the Responses API, whose reasoning
// summary is only requested when an effort is set (a configured gpt-6-astra
// then produced no thinking chain).
func TestApplyThinkingKeepsProviderDefault(t *testing.T) {
	a := newThinkingTestAgent(t, config.ModelSpec{ThinkingLevel: "high"})

	// Session without an override: the provider's own level survives.
	applyThinking(a, "")
	if got := a.Config.Resolved.ThinkingEffort; got != "high" {
		t.Errorf("effort after no-override apply = %q, want the provider's %q", got, "high")
	}

	// "default" restores the provider's level after an override.
	applyThinking(a, "low")
	if got := a.Config.Resolved.ThinkingEffort; got != "low" {
		t.Fatalf("effort after applying low = %q, want low", got)
	}
	applyThinking(a, "default")
	if got := a.Config.Resolved.ThinkingEffort; got != "high" {
		t.Errorf("effort after \"default\" = %q, want the provider's %q (not cleared)", got, "high")
	}

	// A concrete level wins as before.
	applyThinking(a, "xhigh")
	if got := a.Config.Resolved.ThinkingEffort; got != "xhigh" {
		t.Errorf("effort after applying xhigh = %q, want xhigh", got)
	}

	// "none" is the switch, not an effort.
	applyThinking(a, "none")
	if a.Config.Resolved.Thinking == nil || *a.Config.Resolved.Thinking {
		t.Error("thinking switch after \"none\" is not off")
	}
	if got := a.Config.Resolved.ThinkingEffort; got != "" {
		t.Errorf("effort after \"none\" = %q, want empty", got)
	}
}

// TestApplyThinkingWithoutProviderDefault pins the other half: a provider that
// declares nothing keeps the model default (empty effort), which is what the
// agent was built with.
func TestApplyThinkingWithoutProviderDefault(t *testing.T) {
	a := newThinkingTestAgent(t, config.ModelSpec{})
	applyThinking(a, "")
	if got := a.Config.Resolved.ThinkingEffort; got != "" {
		t.Errorf("effort = %q, want empty (no provider default declared)", got)
	}
}
