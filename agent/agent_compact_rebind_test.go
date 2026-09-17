package agent

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Provider switches must carry the compact path with them.
//
// The compact strategy holds a provider of its own, built at construction,
// while SetResolvedProvider replaces a.Config.Resolved wholesale. Nothing else
// reads the strategy's provider, so a switch that skipped the rebind would
// leave /compact and auto-compact summarising through the model the user just
// left — silently, since the summary still arrives.
//
// The other half is the reason the rebind is type-gated rather than
// unconditional: tests inject fakeCompactStrategy to keep the LLM out of the
// loop, and replacing it would turn every one of those into a real call.

func TestSetResolvedProvider_RebindsCompactStrategy(t *testing.T) {
	overrideOneoffDirs(t)

	full := config.DefaultConfig()
	full.Providers = append(full.Providers,
		config.ProviderConfig{Name: "first", Type: "openai", Model: "m1", BaseURL: "https://api.test/v1", APIKey: "k1"},
		config.ProviderConfig{Name: "second", Type: "openai", Model: "m2", BaseURL: "https://api.test/v1", APIKey: "k2"},
	)

	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &llm.ResolvedProvider{Provider: &mockStreamProvider{name: "m1", providerName: "first"}},
		Logger:        logger.Default(),
		FullConfig:    full,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}

	// The production strategy is what a constructed agent carries; the agent
	// under test must start on it for the rebind to be observable at all.
	before, ok := a.Config.CompactStrategy.(*llmCompactStrategy)
	if !ok {
		t.Fatalf("CompactStrategy = %T, want *llmCompactStrategy", a.Config.CompactStrategy)
	}
	if got := before.provider.ProviderName(); got != "first" {
		t.Fatalf("strategy provider before switch = %q, want %q", got, "first")
	}

	if _, err := a.SetResolvedProvider("second"); err != nil {
		t.Fatalf("SetResolvedProvider: %v", err)
	}

	after, ok := a.Config.CompactStrategy.(*llmCompactStrategy)
	if !ok {
		t.Fatalf("CompactStrategy after switch = %T, want *llmCompactStrategy", a.Config.CompactStrategy)
	}
	if after == before {
		t.Error("the strategy instance was left in place — it still holds the old provider")
	}
	if got := after.provider.ProviderName(); got != "second" {
		t.Errorf("strategy provider after switch = %q, want %q (compact must follow /model)", got, "second")
	}
	// Same object the main path now uses, so a summary is billed to the model
	// that produced it.
	if after.provider != a.Config.Resolved.Provider {
		t.Error("strategy provider is not the agent's current main provider")
	}
}

// A caller-injected strategy is the caller's: a provider switch must not swap
// it out for the production implementation.
func TestSetResolvedProvider_KeepsInjectedCompactStrategy(t *testing.T) {
	overrideOneoffDirs(t)

	full := config.DefaultConfig()
	full.Providers = append(full.Providers,
		config.ProviderConfig{Name: "first", Type: "openai", Model: "m1", BaseURL: "https://api.test/v1", APIKey: "k1"},
		config.ProviderConfig{Name: "second", Type: "openai", Model: "m2", BaseURL: "https://api.test/v1", APIKey: "k2"},
	)

	fake := &fakeCompactStrategy{summary: "固定摘要"}
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:        &llm.ResolvedProvider{Provider: &mockStreamProvider{name: "m1", providerName: "first"}},
		Logger:          logger.Default(),
		FullConfig:      full,
		CompactStrategy: fake,
		UsageRecorder:   llm.NewUsageRecorder(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}

	if _, err := a.SetResolvedProvider("second"); err != nil {
		t.Fatalf("SetResolvedProvider: %v", err)
	}

	if a.Config.CompactStrategy != CompactStrategy(fake) {
		t.Errorf("CompactStrategy = %T, want the injected fake (a provider switch must not "+
			"replace a caller-owned strategy — that would turn the test into a real LLM call)",
			a.Config.CompactStrategy)
	}
}
