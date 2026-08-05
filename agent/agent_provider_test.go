package agent

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// TestResolveAdversarialRoundModels covers the combined "validate + assign
// per-round providers" entry point used by both the TUI and ACP /review
// paths. The fail-fast check is cmds.CheckAdversarialProviders and the
// assignment is cmds.ResolveRoundModels (both tested in agent/commands);
// here we pin the composition contract.
func TestResolveAdversarialRoundModels(t *testing.T) {
	a := NewAIAgent(nil, 10)
	cfg := config.DefaultConfig()
	fallback := &mockStreamProvider{name: "fb", sequences: nil}

	t.Run("unconfigured uses fallback for all rounds", func(t *testing.T) {
		rounds, err := a.ResolveAdversarialRoundModels(cfg, fallback, 3)
		if err != nil {
			t.Fatalf("unconfigured must pass, got %v", err)
		}
		if len(rounds) != 3 {
			t.Fatalf("rounds = %d, want 3", len(rounds))
		}
		for i, p := range rounds {
			if p != fallback {
				t.Errorf("round %d = %v, want fallback", i+1, p)
			}
		}
	})

	t.Run("configured but failed resolution errors", func(t *testing.T) {
		cfg.Review.Adversarial = &config.AdversarialReviewConfig{Models: []string{"bad-model"}}
		a.Config.AdversarialModels = []llm.Provider{nil} // Configure-time resolution failed
		if _, err := a.ResolveAdversarialRoundModels(cfg, fallback, 3); err == nil {
			t.Fatal("nil resolved model must fail fast")
		}
	})

	t.Run("resolved assigns rounds with fixed judge", func(t *testing.T) {
		p := &mockStreamProvider{name: "prov", sequences: nil}
		judge := &mockStreamProvider{name: "judge", sequences: nil}
		cfg.Review.Adversarial = &config.AdversarialReviewConfig{
			Models:     []string{"ok"},
			JudgeModel: "judge",
		}
		a.Config.AdversarialModels = []llm.Provider{p}
		a.Config.AdversarialJudge = judge

		rounds, err := a.ResolveAdversarialRoundModels(cfg, fallback, 2)
		if err != nil {
			t.Fatalf("resolved providers must pass, got %v", err)
		}
		if rounds[0] != p {
			t.Errorf("round 1 = %v, want configured model", rounds[0])
		}
		if rounds[1] != judge {
			t.Errorf("round 2 = %v, want judge (final round fixed)", rounds[1])
		}
	})
}

// ---- SetupAdversarialProviders ----

// TestSetupAdversarialProviders verifies the Configure-time resolution of
// review.adversarial model/judge names into providers, and the reset
// semantics: repeated calls never accumulate entries (appending would
// silently grow the slice and trip cmds.CheckAdversarialProviders' length
// comparison).
func TestSetupAdversarialProviders(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Providers = []config.ProviderConfig{
		{Name: "model-a", Type: "openai", Model: "gpt-4o", BaseURL: "https://api.openai.com/v1", APIKey: "sk-1"},
		{Name: "model-b", Type: "openai", Model: "gpt-4.1", BaseURL: "https://api.openai.com/v1", APIKey: "sk-1"},
		{Name: "judge-m", Type: "anthropic", Model: "claude-opus-4", BaseURL: "https://api.anthropic.com", APIKey: "sk-2"},
	}
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{
		Models:     []string{"model-a", "model-b"},
		JudgeModel: "judge-m",
	}

	a := NewAIAgent(nil, 10)
	a.Config.Logger = logger.Default()
	a.SetupAdversarialProviders(cfg)

	if len(a.Config.AdversarialModels) != 2 {
		t.Fatalf("models = %d, want 2", len(a.Config.AdversarialModels))
	}
	if got := a.Config.AdversarialModels[0].Model(); got != "gpt-4o" {
		t.Errorf("models[0] = %q, want gpt-4o", got)
	}
	if got := a.Config.AdversarialModels[1].Model(); got != "gpt-4.1" {
		t.Errorf("models[1] = %q, want gpt-4.1", got)
	}
	if a.Config.AdversarialJudge == nil || a.Config.AdversarialJudge.Model() != "claude-opus-4" {
		t.Errorf("judge = %v, want claude-opus-4", a.Config.AdversarialJudge)
	}

	// Idempotency: a second call (Configure + explicit re-setup) must reset
	// before repopulating — not append.
	a.SetupAdversarialProviders(cfg)
	if len(a.Config.AdversarialModels) != 2 {
		t.Errorf("after re-setup models = %d, want 2 (no accumulation)", len(a.Config.AdversarialModels))
	}
}

// TestSetupAdversarialProviders_UnresolvableRecordsNil verifies that a model
// name with no matching named provider is recorded as nil (the fail-fast
// happens later, at /review start) and that the reset clears a previous
// healthy setup.
func TestSetupAdversarialProviders_UnresolvableRecordsNil(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Providers = []config.ProviderConfig{
		{Name: "model-a", Type: "openai", Model: "gpt-4o", BaseURL: "https://api.openai.com/v1", APIKey: "sk-1"},
	}
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{Models: []string{"model-a"}}

	a := NewAIAgent(nil, 10)
	a.Config.Logger = logger.Default()
	a.SetupAdversarialProviders(cfg)
	if len(a.Config.AdversarialModels) != 1 || a.Config.AdversarialModels[0] == nil {
		t.Fatalf("initial setup: models = %v, want 1 resolved entry", a.Config.AdversarialModels)
	}

	// Re-setup with an unresolvable name: reset must drop the old entries and
	// record exactly one nil.
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{Models: []string{"ghost"}}
	a.SetupAdversarialProviders(cfg)
	if len(a.Config.AdversarialModels) != 1 || a.Config.AdversarialModels[0] != nil {
		t.Errorf("after re-setup with bad name: models = %v, want single nil entry", a.Config.AdversarialModels)
	}
}

// TestConfigure_AdversarialPreresolvedSkipsSetup pins the caller-precedence
// contract at the Configure call site: when AgentConfig pre-resolves the
// adversarial providers, config-based Setup is skipped entirely (matching the
// sibling providers' if-caller-else-config pattern) — a FullConfig
// adversarial section must not overwrite or append to the caller's providers.
func TestConfigure_AdversarialPreresolvedSkipsSetup(t *testing.T) {
	pre := &mockStreamProvider{name: "pre"}
	full := config.DefaultConfig()
	full.Providers = []config.ProviderConfig{
		{Name: "cfg-model", Type: "openai", Model: "gpt-4o", BaseURL: "https://api.openai.com/v1", APIKey: "sk-1"},
	}
	full.Review.Adversarial = &config.AdversarialReviewConfig{Models: []string{"cfg-model"}}

	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Provider:          &mockStreamProvider{name: "main"},
		Logger:            logger.Default(),
		AdversarialModels: []llm.Provider{pre},
		AdversarialJudge:  pre,
		FullConfig:        full,
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	// The caller's providers are wrapped by RecordingProvider (usage billing),
	// which is a transparent decorator — the underlying pre-resolved provider
	// must still be the one in use (Name()/Model() pass through).
	if len(a.Config.AdversarialModels) != 1 || a.Config.AdversarialModels[0] == nil {
		t.Fatalf("caller-preresolved models dropped: %v", a.Config.AdversarialModels)
	}
	if got := a.Config.AdversarialModels[0].Name(); got != pre.Name() {
		t.Errorf("caller-preresolved models overwritten: Name()=%s want %s", got, pre.Name())
	}
	if a.Config.AdversarialJudge == nil || a.Config.AdversarialJudge.Name() != pre.Name() {
		t.Errorf("caller-preresolved judge overwritten: %v", a.Config.AdversarialJudge)
	}
}
