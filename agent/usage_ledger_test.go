package agent

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// closeEnough compares floats within the accumulated float64 rounding noise
// of /1e6-scaled token costs.
func closeEnough(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

// TestComputeSessionUsage_LedgerCost verifies that /usage cost comes
// EXCLUSIVELY from the usage ledger: conversation + commit + subagent rows of
// the current session are aggregated (per kind, per model), rows of other
// sessions are excluded, and the old messages-based cost path is gone.
func TestComputeSessionUsage_LedgerCost(t *testing.T) {
	sm := &fakeSessionManager{}
	if _, err := sm.New("deepseek-v4-flash", "/tmp"); err != nil {
		t.Fatal(err)
	}
	sid := sm.Current().ID

	rec := llm.NewUsageRecorder(t.TempDir())
	now := time.Now()
	rows := []llm.UsageRow{
		{TS: now, SessionID: sid, Kind: llm.UsageKindConversation, Provider: "deepseek-v4-flash", Model: "deepseek-chat", InputTokens: 1000, OutputTokens: 100, InputPrice: 1.0, OutputPrice: 2.0},
		{TS: now, SessionID: sid, Kind: llm.UsageKindCommit, Provider: "deepseek-v4-flash", Model: "deepseek-chat", InputTokens: 500, OutputTokens: 50, InputPrice: 1.0, OutputPrice: 2.0},
		{TS: now, SessionID: sid, Kind: llm.UsageKindSubagent, Provider: "deepseek-v4-flash", Model: "deepseek-chat", InputTokens: 200, OutputTokens: 20, InputPrice: 1.0, OutputPrice: 2.0},
		// Unpriced row — counted, cost 0.
		{TS: now, SessionID: sid, Kind: llm.UsageKindAmbient, Provider: "anthropic", Model: "claude-x", InputTokens: 100, OutputTokens: 10, InputPrice: 0, OutputPrice: 0},
		// Other session — must be excluded.
		{TS: now, SessionID: "other-session", Kind: llm.UsageKindConversation, Provider: "x", Model: "y", InputTokens: 99999, OutputTokens: 1, InputPrice: 1.0, OutputPrice: 1.0},
	}
	for _, r := range rows {
		if err := rec.Record(r); err != nil {
			t.Fatal(err)
		}
	}

	report, err := ComputeSessionUsage(sm, rec, 128000)
	if err != nil {
		t.Fatal(err)
	}
	if !report.LedgerAvailable {
		t.Error("LedgerAvailable = false, want true")
	}
	wantCost := 1000.0/1e6*1 + 100.0/1e6*2 +
		500.0/1e6*1 + 50.0/1e6*2 +
		200.0/1e6*1 + 20.0/1e6*2 +
		0 // unpriced ambient
	if report.Cost != wantCost {
		t.Errorf("Cost = %v, want %v", report.Cost, wantCost)
	}
	if got := report.KindCosts[llm.UsageKindCommit]; got.Calls != 1 || !closeEnough(got.Cost, 600.0/1e6) {
		t.Errorf("commit kind = %+v", got)
	}
	if got := report.KindCosts[llm.UsageKindSubagent]; got.Calls != 1 {
		t.Errorf("subagent kind = %+v", got)
	}
	if report.UnpricedCalls != 1 {
		t.Errorf("UnpricedCalls = %d, want 1", report.UnpricedCalls)
	}
	if len(report.ModelCosts) != 2 { // deepseek-v4-flash:deepseek-chat + anthropic:claude-x
		t.Errorf("ModelCosts = %v, want 2 entries", report.ModelCosts)
	}
}

// TestComputeSessionUsage_NilRecorder: a nil recorder (pre-upgrade or startup
// failure) must not fail — the report just has no billing data.
func TestComputeSessionUsage_NilRecorder(t *testing.T) {
	sm := &fakeSessionManager{}
	if _, err := sm.New("deepseek-v4-flash", "/tmp"); err != nil {
		t.Fatal(err)
	}
	report, err := ComputeSessionUsage(sm, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.LedgerAvailable {
		t.Error("LedgerAvailable = true with nil recorder")
	}
	if report.Cost != 0 {
		t.Errorf("Cost = %v, want 0", report.Cost)
	}
}

// TestRunOneOffStream_RecordsUsageRows is the end-to-end wiring test: a
// one-off run (kind=commit) with a wrapped provider records exactly one
// ledger row anchored to the current session, tagged commit.
func TestRunOneOffStream_RecordsUsageRows(t *testing.T) {
	overrideOneoffDirs(t)

	provider := &mockStreamProvider{
		name: "openai",
		sequences: [][]llm.StreamEvent{
			{
				{Type: llm.StreamEventDone, FinishReason: "stop", Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10}},
			},
		},
	}
	rec := llm.NewUsageRecorder(t.TempDir())
	sm := &fakeSessionManager{}
	if _, err := sm.New("deepseek-v4-flash", "/tmp"); err != nil {
		t.Fatal(err)
	}

	a := newBareTestAgent(t, provider, 10)
	a.Config.Logger = logger.Default()
	a.Config.SessionManager = sm
	a.Config.UsageRecorder = rec

	// Wrap the provider manually — NewAIAgent does not (NewAIAgentWithConfig
	// does); the kind/session anchoring being tested lives in RunOneOffStream.
	wrapped := llm.WrapRecordingProvider(provider, rec, func(provider llm.Provider, model string) llm.ResolvedPrice {
		return llm.ResolvedPrice{Price: llm.ModelPrice{InputPrice: 1.0, OutputPrice: 2.0}}
	})

	ch := a.RunOneOffStream(context.Background(), wrapped, "sys", "hello",
		llm.ChatOptions{MaxTokens: 100}, WithOneOffMeta(&OneOffMeta{Kind: "commit"}))
	for ev := range ch {
		if ev.Type == AgentEventError {
			t.Fatalf("one-off run failed: %v", ev.Result.Error)
		}
	}

	rows, err := rec.Rows(sm.Current().ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Kind != llm.UsageKindCommit {
		t.Errorf("Kind = %q, want commit", row.Kind)
	}
	if row.SessionID != sm.Current().ID {
		t.Errorf("SessionID = %q, want %q (anchored to current session)", row.SessionID, sm.Current().ID)
	}
	if row.InputTokens != 100 || row.OutputTokens != 10 {
		t.Errorf("tokens = %d/%d", row.InputTokens, row.OutputTokens)
	}
}

// TestWrapForUsage_ResolvesAlias is the regression guard for ledger rows
// recording a provider_aliases key instead of the real provider name:
// config `provider: main_provider` with
// `provider_aliases: {main_provider: deepseek-v4-flash}` must never write
// "main_provider" into usage rows. Alias expansion happens ONCE at config
// load (Config.ExpandProviderAliases — called by LoadFrom); this test
// exercises that expansion + the wrapping chain end-to-end, so the row
// carries the resolved config provider name ("deepseek-v4-flash").
func TestWrapForUsage_ResolvesAlias(t *testing.T) {
	overrideOneoffDirs(t)

	provider := &mockStreamProvider{
		name:         "openai",
		providerName: "deepseek-v4-flash", // config-resolved providers carry their name
		sequences: [][]llm.StreamEvent{
			{
				{Type: llm.StreamEventDone, FinishReason: "stop", Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10}},
			},
		},
	}
	rec := llm.NewUsageRecorder(t.TempDir())
	full := config.DefaultConfig()
	full.ProviderAliases = map[string]string{"main_provider": "deepseek-v4-flash"}
	full.Providers = append(full.Providers, config.ProviderConfig{
		Name: "deepseek-v4-flash", Type: "openai", Model: "deepseek-chat",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-test",
	})
	full.Provider = "main_provider" // alias in the top-level provider field
	full.ExpandProviderAliases()    // what LoadFrom does after parsing YAML

	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &config.ResolvedProvider{Provider: provider},
		Logger:        logger.Default(),
		FullConfig:    full,
		UsageRecorder: rec,
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	// wrapUsageProviders must have wrapped the main provider under the RESOLVED
	// name — exercise it end-to-end through a one-off run.
	wrapped := a.Config.Resolved.Provider
	if _, ok := wrapped.(*llm.RecordingProvider); !ok {
		t.Fatalf("main provider = %T, want *llm.RecordingProvider", wrapped)
	}

	sm := &fakeSessionManager{}
	if _, err := sm.New("deepseek-v4-flash", "/tmp"); err != nil {
		t.Fatal(err)
	}
	a.Config.SessionManager = sm

	ch := a.RunOneOffStream(context.Background(), wrapped, "sys", "hello",
		llm.ChatOptions{MaxTokens: 100}, WithOneOffMeta(&OneOffMeta{Kind: "commit"}))
	for ev := range ch {
		if ev.Type == AgentEventError {
			t.Fatalf("one-off run failed: %v", ev.Result.Error)
		}
	}

	rows, err := rec.Rows(sm.Current().ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	if rows[0].Provider != "deepseek-v4-flash" {
		t.Errorf("Provider = %q, want %q (alias must resolve to the real provider name)", rows[0].Provider, "deepseek-v4-flash")
	}
}

// TestRunOneOffStream_NoSessionFallsToGlobal: session-less one-off runs
// (tachi -c) must record rows with an empty session_id — visible in the
// global bucket, not silently dropped.
func TestRunOneOffStream_NoSessionFallsToGlobal(t *testing.T) {
	overrideOneoffDirs(t)

	provider := &mockStreamProvider{
		name: "openai",
		sequences: [][]llm.StreamEvent{
			{
				{Type: llm.StreamEventDone, FinishReason: "stop", Usage: &llm.Usage{InputTokens: 50, OutputTokens: 5}},
			},
		},
	}
	rec := llm.NewUsageRecorder(t.TempDir())
	a := newBareTestAgent(t, provider, 10)
	a.Config.Logger = logger.Default()
	a.Config.UsageRecorder = rec // no SessionManager at all

	wrapped := llm.WrapRecordingProvider(provider, rec, nil)
	ch := a.RunOneOffStream(context.Background(), wrapped, "sys", "msg",
		llm.ChatOptions{MaxTokens: 100}, WithOneOffMeta(&OneOffMeta{Kind: "commit"}))
	for ev := range ch {
		if ev.Type == AgentEventError {
			t.Fatalf("one-off run failed: %v", ev.Result.Error)
		}
	}

	global, err := rec.Rows("", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 1 || global[0].SessionID != "" {
		t.Fatalf("session-less rows must land in global bucket: %+v", global)
	}
}

// TestNewAIAgentWithConfig_DedicatedProvidersBilled is the F1 regression
// guard: config-resolved dedicated providers (title/commit/review/run/
// subagent) are created by Setup*Provider via BuildProvider — brand-new
// instances that never pass through wrapUsageProviders. They must be wrapped
// at the setupDedicatedProvider choke point so their calls hit the ledger.
func TestNewAIAgentWithConfig_DedicatedProvidersBilled(t *testing.T) {
	overrideOneoffDirs(t)

	rec := llm.NewUsageRecorder(t.TempDir())
	full := config.DefaultConfig()
	// Point every dedicated provider at a provider that resolves to a stub
	// provider type "openai" so Setup*Provider actually creates instances.
	commitName := "commit-pro"
	full.Providers = append(full.Providers, config.ProviderConfig{
		Name: commitName, Type: "openai", Model: "deepseek-chat",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-test",
	})
	full.CommitProvider = commitName

	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved:      &config.ResolvedProvider{Provider: &mockStreamProvider{name: "openai", sequences: [][]llm.StreamEvent{}}},
		Logger:        logger.Default(),
		FullConfig:    full,
		UsageRecorder: rec,
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}
	if a.CommitProvider() == nil {
		t.Fatal("CommitProvider not resolved")
	}
	// The resolved provider must be wrapped by RecordingProvider (so its calls
	// hit the ledger), and re-wrapping with the same recorder is a no-op
	// (idempotence — prevents double-counting if another path wraps again).
	commitProv := a.CommitProvider()
	if _, ok := commitProv.(*llm.RecordingProvider); !ok {
		t.Fatalf("CommitProvider = %T, want *llm.RecordingProvider (F1: Setup-resolved providers must be wrapped)", commitProv)
	}
	if wrapped := llm.WrapRecordingProvider(commitProv, rec, nil); wrapped != commitProv {
		t.Error("WrapRecordingProvider not idempotent: re-wrapping changed the instance")
	}
}

// TestSetResolvedProvider_SwitchesWholesale pins the wholesale provider switch used
// by runAgent (tachi -p with a run provider): SetResolvedProvider resolves the
// named provider via the agent's FullConfig and swaps EVERY dimension together
// (provider + context window + thinking defaults — no per-field overrides
// needed after the switch) while wrapping the provider for usage billing, so
// post-switch calls stay on the ledger grouped by the config name.
func TestSetResolvedProvider_SwitchesWholesale(t *testing.T) {
	overrideOneoffDirs(t)

	full := config.DefaultConfig()
	cw := int64(200_000)
	full.Providers = append(full.Providers, config.ProviderConfig{
		Name: "run-pro", Type: "openai", Model: "deepseek-chat",
		BaseURL: "https://api.openai.com/v1", APIKey: "sk-test",
		Spec: config.ModelSpec{ContextWindow: &cw, ThinkingLevel: "high"},
	})
	full.RunProvider = "run-pro"

	off := false
	a, _, err := NewAIAgentWithConfig(context.Background(), AgentConfig{
		Resolved: &config.ResolvedProvider{
			Provider:       &mockStreamProvider{name: "old"},
			ContextWindow:  64_000,
			Thinking:       &off,
			ThinkingEffort: "",
		},
		Logger:        logger.Default(),
		FullConfig:    full,
		UsageRecorder: llm.NewUsageRecorder(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewAIAgentWithConfig: %v", err)
	}

	// Wholesale switch by provider name: the target's full resolved config
	// (context window / thinking) takes effect in one call.
	applied, err := a.SetResolvedProvider(full.RunProvider)
	if err != nil {
		t.Fatalf("SetResolvedProvider: %v", err)
	}

	// Every dimension switched together — no SetContextWindow / SetThinking
	// overrides needed after the switch.
	if a.ContextWindow() != 200_000 {
		t.Errorf("ContextWindow = %d, want 200000 (must follow the new provider)", a.ContextWindow())
	}
	if a.Config.Resolved.ThinkingEffort != "high" {
		t.Errorf("ThinkingEffort = %q, want %q (must follow the new provider)", a.Config.Resolved.ThinkingEffort, "high")
	}
	if _, ok := a.Config.Resolved.Provider.(*llm.RecordingProvider); !ok {
		t.Fatalf("provider after SetResolvedProvider = %T, want *llm.RecordingProvider", a.Config.Resolved.Provider)
	}
	// The wrapped provider carries the config name, so ledger rows group by it
	// (the contract the old SetProvider test pinned end-to-end via the ledger).
	if got := a.Config.Resolved.Provider.ProviderName(); got != "run-pro" {
		t.Errorf("ProviderName = %q, want %q (ledger grouping by config name)", got, "run-pro")
	}
	if applied.Type != "openai" || applied.Model != "deepseek-chat" {
		t.Errorf("applied = %+v, want the run provider's display metadata", applied)
	}

	// Detached: the agent owns a private copy — caller-side mutation of the
	// returned object must not leak into the agent.
	applied.ContextWindow = 1
	if a.ContextWindow() != 200_000 {
		t.Errorf("ContextWindow = %d after caller-side mutation, want detached copy", a.ContextWindow())
	}
}
