package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// reviewMockProvider streams a canned immediate response (no tools), so each
// review round completes with a normal TurnComplete.
type reviewMockProvider struct {
	name  string
	model string
	// lastOpts records the ChatOptions of the most recent stream request,
	// letting tests assert what the review round was actually launched with.
	lastOpts llm.ChatOptions
}

func (p *reviewMockProvider) Name() string         { return p.name }
func (p *reviewMockProvider) ProviderName() string { return "" }
func (p *reviewMockProvider) Model() string        { return p.model }
func (p *reviewMockProvider) CreateChat(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *reviewMockProvider) CreateChatStream(_ context.Context, _ []llm.Message, _ []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	p.lastOpts = opts
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "round output"}
	ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
	close(ch)
	return ch, nil
}

// reviewTestModel builds a Model wired to a real (but tool-less) agent with a
// mock provider, plus a saved main-conversation history.
func reviewTestModel(t *testing.T, providers ...llm.Provider) *Model {
	t.Helper()
	m := testModel()
	a := newTestAIAgent(t, providers[0], 10)
	a.Config.Logger = logger.Default() // NewAIAgent leaves Logger nil; the run loop logs
	t.Cleanup(a.Close)
	m.agent = a
	m.systemPrompt = "test-system"
	m.cfg = config.DefaultConfig()
	m.history = []llm.Message{{Role: "user", Content: "main conversation"}}
	m.savedHistory = make([]llm.Message, len(m.history))
	copy(m.savedHistory, m.history)
	return m
}

// startReview seeds the model's reviewOrch for a multi-round run without
// going through sendReviewCommand (which needs a full configured agent).
// The opts come from ResolveReviewOptions(m.cfg) — exactly what production
// /review uses — so thinking/thinking_level config flows through end to end.
// The orchestrator preallocates its reports slice with cap == totalRounds so
// appends during the whole run share one backing array (tests snapshot it
// mid-run via Reports()).
func startReview(m *Model, dir string, totalRounds int, providers ...llm.Provider) {
	orch, err := cmds.NewReviewOrchestrator(totalRounds, providers, dir, cmds.ResolveReviewOptions(m.cfg))
	if err != nil {
		panic(err) // test fixtures are valid by construction
	}
	m.reviewOrch = orch
	m.isReviewing = true
}

// drainTurnComplete reads from the round's event channel until a TurnComplete
// arrives (skipping text deltas / usage / tool events emitted before it).
func drainTurnComplete(t *testing.T, ch <-chan agent.AgentEvent) agent.AgentEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event channel closed before TurnComplete")
			}
			if ev.Type == agent.AgentEventTurnComplete {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for TurnComplete")
		}
	}
}

// TestAdversarialReview_ChainsRoundsAndRestoresHistory drives a 2-round chain
// end to end: intermediate round must chain to round 2 WITHOUT touching
// history or savedHistory; the final round restores the saved history through
// the normal one-off branch.
func TestAdversarialReview_ChainsRoundsAndRestoresHistory(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	p2 := &reviewMockProvider{name: "prov-b", model: "model-b"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()
	startReview(m, dir, 2, p1, p2)

	// Round 1 (Reviewer).
	m.startReviewRound()
	if m.reviewOrch.CurrentRound() != 1 {
		t.Fatalf("currentRound = %d, want 1", m.reviewOrch.CurrentRound())
	}
	// Statusbar badge reflects the current role and round position.
	if m.statusbar.reviewBadge != "⚔️ 审查者 1/2" {
		t.Errorf("badge after round 1 = %q, want %q", m.statusbar.reviewBadge, "⚔️ 审查者 1/2")
	}
	ev1 := drainTurnComplete(t, m.eventCh)
	ev1.Usage = &llm.Usage{InputTokens: 100, OutputTokens: 50}
	_, cmd := m.Update(agentEventMsg{event: ev1, gen: m.streamGen})
	if cmd == nil {
		t.Fatal("intermediate round must chain the next round (return a command)")
	}

	// Intermediate round invariants: history untouched, savedHistory alive,
	// usage accumulated, report recorded as missing (no file written),
	// still reviewing.
	if m.reviewOrch.CurrentRound() != 2 {
		t.Errorf("currentRound = %d, want 2 after chaining", m.reviewOrch.CurrentRound())
	}
	if m.savedHistory == nil || len(m.savedHistory) != 1 || m.savedHistory[0].Content != "main conversation" {
		t.Errorf("savedHistory must stay intact through intermediate rounds, got %v", m.savedHistory)
	}
	if len(m.history) != 1 || m.history[0].Content != "main conversation" {
		t.Errorf("intermediate round must not touch m.history, got %v", m.history)
	}
	if m.totalUsage.InputTokens != 100 || m.totalUsage.OutputTokens != 50 {
		t.Errorf("usage not accumulated per round: %+v", m.totalUsage)
	}
	if !m.isReviewing {
		t.Error("isReviewing must stay true through intermediate rounds")
	}
	// Badge advances with the chain: round 2 is the final Judge.
	if m.statusbar.reviewBadge != "⚔️ 裁决者 2/2" {
		t.Errorf("badge after round 2 start = %q, want %q", m.statusbar.reviewBadge, "⚔️ 裁决者 2/2")
	}
	if len(m.reviewOrch.Reports()) != 1 {
		t.Fatalf("reports = %d entries, want 1", len(m.reviewOrch.Reports()))
	}
	if m.reviewOrch.Reports()[0].Saved {
		t.Error("report should be marked Saved=false (no file was written)")
	}
	// Snapshot the reports slice — the final round appends into the same
	// backing array (preallocated with cap == totalRounds), then clears
	// reviewOrch. The header's len stays at 1, but reslicing to the
	// capacity reveals the final round's record.
	reportsSnapshot := m.reviewOrch.Reports()

	// Round 2 (final Judge).
	ev2 := drainTurnComplete(t, m.eventCh)
	m.Update(agentEventMsg{event: ev2, gen: m.streamGen})

	if m.reviewOrch != nil {
		t.Error("reviewOrch must be cleared after the final round")
	}
	if m.isReviewing {
		t.Error("isReviewing must be false after the final round")
	}
	if m.statusbar.reviewBadge != "" {
		t.Errorf("badge must be cleared after the final round, got %q", m.statusbar.reviewBadge)
	}
	if m.savedHistory != nil {
		t.Error("savedHistory must be restored (nil) after the final round")
	}
	if len(m.history) != 1 || m.history[0].Content != "main conversation" {
		t.Errorf("history must be restored to the saved conversation, got %v", m.history)
	}
	all := reportsSnapshot[:cap(reportsSnapshot)]
	if len(all) != 2 {
		t.Errorf("expected 2 recorded reports, got %d", len(all))
	}
	if all[0].Round != 1 || all[1].Round != 2 {
		t.Errorf("reports recorded out of order: %+v", all)
	}
	assertState(t, m, stateIdle)
}

// TestAdversarialReview_ReportSavedMarked verifies the orchestrator's
// Complete marks a report Saved=true when the orchestrator-allocated path
// exists on disk (the TUI surfaces it via showReviewReportHint).
func TestAdversarialReview_ReportSavedMarked(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()
	startReview(m, dir, 2, p1, p1)

	m.startReviewRound()
	ev1 := drainTurnComplete(t, m.eventCh)

	// The LLM "saved" its report to the orchestrator-allocated path.
	path := cmds.ReportPathFor(dir, 1, cmds.RoleReviewer, "model-a")
	if err := os.WriteFile(path, []byte("# report"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.Update(agentEventMsg{event: ev1, gen: m.streamGen})
	if got := m.reviewOrch.Reports()[0]; !got.Saved {
		t.Errorf("report %q should be marked Saved=true", got.Path)
	}
}

// TestAdversarialReview_ErrorBranch_FullCleanup verifies an AgentEventError
// (API failure / cancellation) terminates the chain and fully cleans up:
// reviewOrch cleared, isReviewing false, history restored, state idle.
func TestAdversarialReview_ErrorBranch_FullCleanup(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()
	startReview(m, dir, 3, p1, p1, p1)
	m.startReviewRound()

	m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type:   agent.AgentEventError,
			Result: &agent.RunResult{ExitReason: agent.ExitReasonInterrupted},
		},
		gen: m.streamGen,
	})

	if m.reviewOrch != nil {
		t.Error("reviewOrch must be cleared on error")
	}
	if m.isReviewing {
		t.Error("isReviewing must be cleared on error (input would be blocked forever)")
	}
	if m.statusbar.reviewBadge != "" {
		t.Errorf("badge must be cleared on error, got %q", m.statusbar.reviewBadge)
	}
	if m.savedHistory != nil {
		t.Error("savedHistory must be restored on error")
	}
	if len(m.history) != 1 || m.history[0].Content != "main conversation" {
		t.Errorf("history must be restored on error, got %v", m.history)
	}
	assertState(t, m, stateIdle)
	if m.cancelFunc != nil || m.eventCh != nil {
		t.Error("cancelFunc/eventCh must be cleared on error")
	}
}

// TestAdversarialReview_ErrorBranch_NonInterrupt also clears state.
func TestAdversarialReview_ErrorBranch_NonInterrupt(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()
	startReview(m, dir, 3, p1, p1, p1)
	m.startReviewRound()

	m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type:   agent.AgentEventError,
			Result: &agent.RunResult{ExitReason: agent.ExitReasonError, Error: errors.New("rate limit exceeded")},
		},
		gen: m.streamGen,
	})

	if m.reviewOrch != nil || m.isReviewing {
		t.Error("API failure must clear review state too (not just Ctrl+C)")
	}
	assertState(t, m, stateIdle)
}

// TestAdversarialReview_CtrlC_DefersCleanupToErrorBranch pins the Ctrl+C
// contract: the review branch cancels + queues nextEvent but does NOT clear
// reviewOrch/savedHistory — the trailing AgentEventError does that.
func TestAdversarialReview_CtrlC_DefersCleanupToErrorBranch(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()
	startReview(m, dir, 3, p1, p1, p1)
	m.startReviewRound()

	// Simulate the already-cancelled stream: cancelFunc is a no-op and the
	// channel tails the AgentEventError the loop emits on cancellation.
	m.cancelFunc = func() {}
	ch := make(chan agent.AgentEvent, 1)
	ch <- agent.AgentEvent{
		Type:   agent.AgentEventError,
		Result: &agent.RunResult{ExitReason: agent.ExitReasonInterrupted},
	}
	close(ch)
	m.eventCh = ch

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("Ctrl+C during review must queue nextEvent to drain the trailing error")
	}

	// Deferred cleanup — nothing cleared yet.
	if m.reviewOrch == nil || !m.isReviewing {
		t.Error("Ctrl+C must NOT clear reviewOrch (the trailing error branch does)")
	}
	if m.savedHistory == nil {
		t.Error("Ctrl+C must NOT restore savedHistory early (pollutes main history)")
	}

	// Draining the trailing error completes the cleanup.
	msg := cmd()
	if _, ok := msg.(agentEventMsg); !ok {
		t.Fatalf("queued cmd returned %T, want agentEventMsg", msg)
	}
	m.Update(msg)
	assertState(t, m, stateIdle)
	if m.reviewOrch != nil || m.isReviewing {
		t.Error("review state must be fully cleared after the trailing error")
	}
	if m.savedHistory != nil {
		t.Error("savedHistory must be restored after the trailing error")
	}
	if len(m.history) != 1 || m.history[0].Content != "main conversation" {
		t.Errorf("history must be restored, got %v", m.history)
	}
}

// TestAdversarialReview_ThinkingFollowsSession verifies the follow-the-session
// contract end to end: with no review.thinking / review.thinking_level
// configured, the round's stream is launched with the agent's live thinking
// config (the /thinking per-session override the main conversation runs with).
func TestAdversarialReview_ThinkingFollowsSession(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()

	// Simulate a session thinking state (e.g. /thinking high + provider-level
	// switch): the round must inherit both the switch and the effort.
	m.agent.Config.Resolved.Thinking = new(true)
	m.agent.Config.Resolved.ThinkingEffort = "max"

	startReview(m, dir, 2, p1, p1)
	m.startReviewRound()
	drainTurnComplete(t, m.eventCh)
	if p1.lastOpts.Thinking == nil || !*p1.lastOpts.Thinking {
		t.Errorf("round thinking = %v, want session value true", p1.lastOpts.Thinking)
	}
	if p1.lastOpts.ThinkingEffort != "max" {
		t.Errorf("round effort = %q, want session effort max", p1.lastOpts.ThinkingEffort)
	}
	if m.forkedAgent != nil {
		m.forkedAgent.Close()
		m.forkedAgent = nil
	}
}

// TestAdversarialReview_ThinkingConfigPins verifies the explicit
// review.thinking override wins over the session (single-round path).
func TestAdversarialReview_ThinkingConfigPins(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()

	// Session says thinking on / max; config pins it off.
	m.agent.Config.Resolved.Thinking = new(true)
	m.agent.Config.Resolved.ThinkingEffort = "max"
	m.cfg.Review.Thinking = new(false)

	startReview(m, dir, 1, p1)
	m.startReviewRound()
	drainTurnComplete(t, m.eventCh)
	if p1.lastOpts.Thinking == nil || *p1.lastOpts.Thinking {
		t.Errorf("round thinking = %v, want false (config wins)", p1.lastOpts.Thinking)
	}
	if m.forkedAgent != nil {
		m.forkedAgent.Close()
		m.forkedAgent = nil
	}
}

// TestAdversarialReview_ThinkingLevelPinsEffort verifies review.thinking_level
// pins the effort while the switch follows the session.
func TestAdversarialReview_ThinkingLevelPinsEffort(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()

	m.agent.Config.Resolved.Thinking = nil
	m.agent.Config.Resolved.ThinkingEffort = "low"
	m.cfg.Review.ThinkingLevel = "high"

	startReview(m, dir, 1, p1)
	m.startReviewRound()
	drainTurnComplete(t, m.eventCh)
	if p1.lastOpts.ThinkingEffort != "high" {
		t.Errorf("round effort = %q, want configured high", p1.lastOpts.ThinkingEffort)
	}
	if m.forkedAgent != nil {
		m.forkedAgent.Close()
		m.forkedAgent = nil
	}
}

func TestAdversarialReview_InputBlocked(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)
	dir := t.TempDir()
	startReview(m, dir, 3, p1, p1, p1)

	_, cmd := m.Update(InputSubmitMsg("hello during review"))
	if cmd != nil {
		t.Error("input during review must not produce a command")
	}
	if len(m.pendingQueue) != 0 {
		t.Error("input during review must not enter pendingQueue")
	}
}

// TestAdversarialReview_BlocksSkillInput covers the skill-resolution path,
// which sits BEFORE the general input gate: a "/skill-name" input must also
// be blocked (not activated) during a review, even in the stateWaiting
// window between rounds where the stateStreaming guard is inactive.
func TestAdversarialReview_BlocksSkillInput(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)

	// Install a resolvable skill so "/code-review ..." would normally activate.
	skillsDir := t.TempDir()
	skillDir := filepath.Join(skillsDir, "code-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: code-review\ndescription: test skill\n---\n# body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := skill.NewStoreWithDirs([]string{skillsDir}, []string{"test"})
	m.agent.Config.SkillStore = store
	if _, ok := store.ResolveCommand("code-review"); !ok {
		t.Fatal("test skill not resolvable — fixture broken")
	}

	dir := t.TempDir()
	startReview(m, dir, 3, p1, p1, p1)
	m.setState(stateWaiting) // the round-transition window

	_, cmd := m.Update(InputSubmitMsg("/code-review main.go"))
	if cmd != nil {
		t.Error("skill input during review must not produce a command")
	}
	if len(m.pendingQueue) != 0 {
		t.Error("skill input must not enter pendingQueue")
	}
	if m.reviewOrch == nil || !m.isReviewing {
		t.Error("review state must survive a skill input attempt")
	}
}

// TestAdversarialReview_SendReviewCommand_FailFast verifies the fail-fast
// gate end to end: a configured-but-unresolvable model name aborts /review
// before round 1 — no stream starts, savedHistory is restored, state returns
// to idle. The check itself is shared via cmds.CheckAdversarialProviders.
func TestAdversarialReview_SendReviewCommand_FailFast(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)

	// Configure a model name whose resolution failed (nil entry), plus a
	// judge that also failed to resolve. The explicit "/review 3" enters the
	// multi-round path, where the fail-fast gate fires before round 1.
	m.cfg.Review.Adversarial = &config.AdversarialReviewConfig{
		Models:     []string{"bad-model"},
		JudgeModel: "bad-judge",
	}
	m.agent.Config.AdversarialModels = []llm.Provider{nil}
	m.agent.Config.AdversarialJudge = nil
	m.subcommandInput = "/review 3"

	cmd := m.sendReviewCommand()
	if cmd != nil {
		t.Error("fail fast must return nil command (no round started)")
	}
	assertState(t, m, stateIdle)
	if m.savedHistory != nil {
		t.Error("savedHistory must be restored on fail fast")
	}
	if m.isReviewing || m.reviewOrch != nil {
		t.Error("no review state may exist after fail fast")
	}
	if m.eventCh != nil {
		t.Error("no event channel may be started after fail fast")
	}
}

// TestAdversarialReview_SendReviewCommand_FailFastOnlyWhenConfigured pins the
// other side of the gate: models configured AND fully resolved pass through
// and start round 1.
func TestAdversarialReview_SendReviewCommand_FailFastOnlyWhenConfigured(t *testing.T) {
	p1 := &reviewMockProvider{name: "prov-a", model: "model-a"}
	m := reviewTestModel(t, p1)

	// sendReviewCommand creates the relative ".tachi/reviews/<ts>" dir —
	// chdir to a temp dir so the repo stays clean.
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	// Explicit "/review 2" enters the multi-round path with a fully resolved
	// configured model.
	m.cfg.Review.Adversarial = &config.AdversarialReviewConfig{
		Models: []string{"model-a"},
	}
	m.agent.Config.AdversarialModels = []llm.Provider{p1}
	m.subcommandInput = "/review 2"

	cmd := m.sendReviewCommand()
	if cmd == nil {
		t.Fatal("resolved providers must start round 1 (non-nil command)")
	}
	if m.reviewOrch == nil || !m.isReviewing {
		t.Fatal("review state must be established")
	}
	if m.reviewOrch.CurrentRound() != 1 {
		t.Errorf("currentRound = %d, want 1", m.reviewOrch.CurrentRound())
	}
	if m.reviewOrch.TotalRounds() != 2 {
		t.Errorf("totalRounds = %d, want 2", m.reviewOrch.TotalRounds())
	}
	// Clean up the started round's fork.
	if m.forkedAgent != nil {
		m.forkedAgent.Close()
		m.forkedAgent = nil
	}
}
