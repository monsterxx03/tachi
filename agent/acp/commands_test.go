package acp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// expectedACPCmds lists all static commands we expect in buildACPAvailableCommands.
var expectedACPCmds = []struct {
	name     string
	hasInput bool
}{
	{name: "model", hasInput: true},
	{name: "commit"},
	{name: "review", hasInput: true}, // InputHint "[rounds]" — adversarial review takes an optional round count
	{name: "init"},
	{name: "compact"},
	{name: "usage"},
	{name: "mcp", hasInput: true},
	{name: "skill", hasInput: true},
	{name: "transcript"},
	{name: "research", hasInput: true},
}

func TestBuildACPAvailableCommands_StaticCommands(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, 0)
	cmds := buildACPAvailableCommands(aiAgent)

	// Collect all returned command names for duplicate checking
	names := make(map[string]int)
	for _, c := range cmds {
		names[c.Name]++
	}

	// Check each expected command
	for _, ec := range expectedACPCmds {
		t.Run(ec.name, func(t *testing.T) {
			count, found := names[ec.name]
			assert.True(t, found, "command %q should be present in available commands", ec.name)
			assert.Equal(t, 1, count, "command %q should appear exactly once", ec.name)

			// Find the command and check fields
			for _, c := range cmds {
				if c.Name == ec.name {
					assert.NotEmpty(t, c.Description, "command %q should have a non-empty description", ec.name)
					if ec.hasInput {
						assert.NotNil(t, c.Input, "command %q should have Input set", ec.name)
						if c.Input != nil {
							assert.NotNil(t, c.Input.Unstructured, "command %q Input should have Unstructured", ec.name)
							assert.NotEmpty(t, c.Input.Unstructured.Hint, "command %q Input should have a non-empty Hint", ec.name)
						}
					} else {
						assert.Nil(t, c.Input, "command %q should NOT have Input set", ec.name)
					}
				}
			}
		})
	}
}

func TestBuildACPAvailableCommands_Count(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, 0)
	cmds := buildACPAvailableCommands(aiAgent)

	// Should have exactly len(expectedACPCmds) commands (no skills configured)
	assert.Len(t, cmds, len(expectedACPCmds))
}

func TestBuildACPAvailableCommands_NoDuplicates(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, 0)
	cmds := buildACPAvailableCommands(aiAgent)

	names := make(map[string]int)
	for _, c := range cmds {
		names[c.Name]++
	}

	for name, count := range names {
		assert.Equal(t, 1, count, "command %q appears %d times — no duplicates expected", name, count)
	}
}

func TestBuildACPAvailableCommands_NilAgent(t *testing.T) {
	// Passing nil should not panic; returns empty static commands
	cmds := buildACPAvailableCommands(nil)
	assert.Len(t, cmds, len(expectedACPCmds),
		"nil agent should still return static commands (no skills)")
}

// ---------------------------------------------------------------------------
// Adversarial review — handleACPReview multi-round loop (driven by the shared
// cmds.ReviewOrchestrator). See docs/2026-07-30-adversarial-review-design.md.
// ---------------------------------------------------------------------------

// acpReviewMockProvider counts CreateChatStream calls (one per round) and
// records the last user message it was given, so tests can observe how many
// rounds ran and what the next round's prompt looked like.
type acpReviewMockProvider struct {
	streamCalls int
	lastPrompt  string
	name        string
	model       string
}

func (p *acpReviewMockProvider) Name() string  { return p.name }
func (p *acpReviewMockProvider) Model() string { return p.model }
func (p *acpReviewMockProvider) CreateChat(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *acpReviewMockProvider) CreateChatStream(_ context.Context, messages []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	p.streamCalls++
	// Record the last user message (the per-round prompt).
	for _, m := range messages {
		if m.Role == "user" {
			p.lastPrompt = m.Content
		}
	}
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "round output"}
	ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop"}
	close(ch)
	return ch, nil
}

// newACPReviewSession builds a minimal ACPSession + a real agent-side
// connection whose notifications are discarded (peerOutput blocks forever so
// the connection never terminates).
func newACPReviewSession(t *testing.T, cfg *config.Config, p llm.Provider) (*ACPSession, *acp.AgentSideConnection) {
	t.Helper()
	a := agent.NewAIAgent(p, 10)
	a.Config.Logger = logger.Default()
	t.Cleanup(a.Close)
	sess := &ACPSession{
		ID:    "test-sess",
		cwd:   t.TempDir(),
		cfg:   cfg,
		agent: a,
	}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	conn := acp.NewAgentSideConnection(&TachiAgent{}, io.Discard, pr)
	return sess, conn
}

// ---- /review multi-round tests ----

// TestHandleACPReview_MultiRound_ChainCompletes drives the full handler: 3
// rounds must run (one fork + one LLM call each), finish with EndTurn, and
// leave sess.history cleared. The report dir is anchored at sess.cwd (the
// ACP convention) — NOT the process CWD.
func TestHandleACPReview_MultiRound_ChainCompletes(t *testing.T) {
	p := &acpReviewMockProvider{name: "prov", model: "mock-model"}
	sess, conn := newACPReviewSession(t, config.DefaultConfig(), p)

	// Seed the history cache — must be cleared by the handler.
	sess.history = []llm.Message{{Role: "user", Content: "prior turn"}}

	stopReason, err := handleACPReview(context.Background(), sess, conn, "3")
	if err != nil {
		t.Fatalf("handleACPReview error: %v", err)
	}
	if stopReason != acp.StopReasonEndTurn {
		t.Errorf("stopReason = %v, want EndTurn", stopReason)
	}
	if p.streamCalls != 3 {
		t.Errorf("streamCalls = %d, want 3 (one LLM call per round)", p.streamCalls)
	}
	if sess.history != nil {
		t.Error("sess.history must be cleared after /review (one-off turn)")
	}
	// The parent agent must remain usable after 3 fork/Close cycles.
	if sess.agent.ToolSchemas() == nil {
		t.Error("parent agent registry broken after round forks")
	}
	// The report dir was created by the orchestrator under sess.cwd — the
	// same dir the round's WriteFile resolves relative paths against (the
	// orchestrator's os.Stat verification must see what the LLM wrote).
	reportDir := filepath.Join(sess.cwd, ".tachi/reviews")
	if _, err := os.Stat(reportDir); err != nil {
		t.Errorf("report dir not created under sess.cwd: %v", err)
	}
	// Sanity: the process CWD must NOT hold the report dir (sess.cwd is a
	// temp dir distinct from the test runner's CWD).
	if _, err := os.Stat(".tachi/reviews"); !os.IsNotExist(err) {
		t.Error("report dir must not be created under the process CWD")
	}
}

// TestHandleACPReview_MultiRound_ModelNamesAreUsed verifies the adversarial
// provider names flow into the report paths: with 2 configured models + judge
// over 4 rounds, round files carry distinct model names.
func TestHandleACPReview_MultiRound_ModelNamesAreUsed(t *testing.T) {
	p := &acpReviewMockProvider{name: "prov-a", model: "model-a"}
	p2 := &acpReviewMockProvider{name: "prov-b", model: "model-b"}
	pj := &acpReviewMockProvider{name: "prov-j", model: "model-judge"}
	sess, conn := newACPReviewSession(t, config.DefaultConfig(), p)

	// Pre-resolve the adversarial providers on the agent (Configure-time
	// resolution normally does this).
	sess.agent.Config.AdversarialModels = []llm.Provider{p, p2}
	sess.agent.Config.AdversarialJudge = pj
	sess.cfg.Review.Adversarial = &config.AdversarialReviewConfig{
		Models:     []string{"model-a", "model-b"},
		JudgeModel: "model-judge",
	}

	stopReason, err := handleACPReview(context.Background(), sess, conn, "4")
	if err != nil {
		t.Fatalf("handleACPReview error: %v", err)
	}
	if stopReason != acp.StopReasonEndTurn {
		t.Errorf("stopReason = %v, want EndTurn", stopReason)
	}

	// Rounds: 1=model-a (Reviewer), 2=model-b (Challenger),
	// 3=model-a (middle Judge), 4=model-judge (fixed final Judge).
	// Each provider recorded the prompt of ITS last round — verify the
	// per-round prompt names its own deterministic report path.
	cases := []struct {
		p    *acpReviewMockProvider
		want string // report path fragment in the prompt
	}{
		{p, "round-3-judge-model-a.md"},      // round 3 (model-a, middle judge)
		{p2, "round-2-challenge-model-b.md"}, // round 2 (model-b, challenger)
		{pj, "round-4-judge-model-judge.md"}, // round 4 (final judge, fixed model)
	}
	for _, c := range cases {
		if !strings.Contains(c.p.lastPrompt, c.want) {
			t.Errorf("prompt for %s missing its own report path %q", c.p.model, c.want)
		}
	}
}

// TestHandleACPReview_MultiRound_FailFastAbortsBeforeRound1 verifies the
// shared cmds.CheckAdversarialProviders gate: a configured-but-unresolvable
// model name aborts /review before any round runs (no LLM call, error
// returned).
func TestHandleACPReview_MultiRound_FailFastAbortsBeforeRound1(t *testing.T) {
	p := &acpReviewMockProvider{name: "prov-a", model: "model-a"}
	sess, conn := newACPReviewSession(t, config.DefaultConfig(), p)

	// Configure a model name whose Configure-time resolution failed.
	sess.cfg.Review.Adversarial = &config.AdversarialReviewConfig{
		Models: []string{"bad-model"},
	}
	sess.agent.Config.AdversarialModels = []llm.Provider{nil}

	stopReason, err := handleACPReview(context.Background(), sess, conn, "3")
	if err == nil {
		t.Fatal("fail fast must return an error")
	}
	if stopReason != acp.StopReasonEndTurn {
		t.Errorf("stopReason = %v, want EndTurn (error aborts the command)", stopReason)
	}
	if p.streamCalls != 0 {
		t.Errorf("streamCalls = %d, want 0 — no round may start before fail fast", p.streamCalls)
	}
}

// acpFailingProvider's CreateChatStream always fails — models an API error
// (rate limit, timeout) mid-round.
type acpFailingProvider struct {
	streamCalls int
	name        string
	model       string
}

func (p *acpFailingProvider) Name() string  { return p.name }
func (p *acpFailingProvider) Model() string { return p.model }
func (p *acpFailingProvider) CreateChat(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *acpFailingProvider) CreateChatStream(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	p.streamCalls++
	return nil, errors.New("rate limit exceeded")
}

// TestHandleACPReview_MultiRound_ApiErrorTerminatesChain guards the TUI/ACP
// parity bug the review report flagged: a mid-round API error must stop the
// chain, not continue into the next round (wasting calls on a broken model).
// streamToACP carries the failure back as an error while keeping the legacy
// EndTurn stop reason; runRound propagates it and the chain terminates.
func TestHandleACPReview_MultiRound_ApiErrorTerminatesChain(t *testing.T) {
	p := &acpFailingProvider{name: "prov-a", model: "model-a"}
	sess, conn := newACPReviewSession(t, config.DefaultConfig(), p)

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	stopReason, err := handleACPReview(context.Background(), sess, conn, "3")
	if err == nil {
		t.Fatal("round API error must propagate out of handleACPReview")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("err = %v, want the underlying API error", err)
	}
	// Legacy stop-reason behavior preserved for single-round callers, but the
	// chain must have stopped after the failed round.
	if stopReason != acp.StopReasonEndTurn {
		t.Errorf("stopReason = %v, want EndTurn (legacy mapping)", stopReason)
	}
	if p.streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1 — the chain must stop after the failed round", p.streamCalls)
	}
	if sess.history != nil {
		t.Error("sess.history must be cleared even on early termination")
	}
}

// ---- runAdversarialReviewRounds (shared orchestrator Run contract) ----

// The round-loop tests below drive the shared cmds.ReviewOrchestrator.Run —
// the ACP frontend's synchronous driver (the TUI drives the same orchestrator
// event-driven via Next/Complete). ACP-specific semantics: a non-EndTurn
// stopReason maps to cmds.ErrStopReview inside the runRound closure, and the
// handler (handleACPReview) maps it back to the original stop reason.

func TestRunAdversarialReviewRounds_ChainCompletes(t *testing.T) {
	p1 := &acpReviewMockProvider{name: "a", model: "model-a"}
	p2 := &acpReviewMockProvider{name: "b", model: "model-b"}
	dir := t.TempDir()

	orch, err := cmds.NewReviewOrchestrator(3, []llm.Provider{p1, p2, p1}, dir, cmds.ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}

	var rounds []int
	err = orch.Run(func(spec cmds.RoundSpec) error {
		rounds = append(rounds, spec.Round)
		if !strings.Contains(spec.Prompt, spec.OutPath) {
			t.Errorf("round %d prompt must contain its exact outPath %q", spec.Round, spec.OutPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rounds) != 3 || rounds[0] != 1 || rounds[1] != 2 || rounds[2] != 3 {
		t.Errorf("rounds executed = %v, want [1 2 3]", rounds)
	}
	if orch.CurrentRound() != 3 || len(orch.Reports()) != 3 {
		t.Errorf("orchestrator state after completion: current=%d reports=%d, want 3/3",
			orch.CurrentRound(), len(orch.Reports()))
	}
}

// TestRunAdversarialReviewRounds_NonNaturalCompletionTerminates pins the
// early-termination contract: a frontend-initiated stop (ErrStopReview)
// stops the chain at that round — no further rounds are started.
func TestRunAdversarialReviewRounds_NonNaturalCompletionTerminates(t *testing.T) {
	p1 := &acpReviewMockProvider{name: "a", model: "model-a"}
	dir := t.TempDir()

	orch, err := cmds.NewReviewOrchestrator(3, []llm.Provider{p1, p1, p1}, dir, cmds.ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}

	var rounds []int
	err = orch.Run(func(spec cmds.RoundSpec) error {
		rounds = append(rounds, spec.Round)
		if spec.Round == 2 {
			return cmds.ErrStopReview // client disconnect mid-round
		}
		return nil
	})
	if !errors.Is(err, cmds.ErrStopReview) {
		t.Fatalf("err = %v, want ErrStopReview", err)
	}
	if len(rounds) != 2 {
		t.Errorf("rounds executed = %v, want [1 2] (chain stops at the non-natural completion)", rounds)
	}
}

// TestRunAdversarialReviewRounds_RoundErrorTerminates covers the runRound
// error path (fork/transport failure) — same early termination contract.
func TestRunAdversarialReviewRounds_RoundErrorTerminates(t *testing.T) {
	p1 := &acpReviewMockProvider{name: "a", model: "model-a"}
	dir := t.TempDir()

	orch, err := cmds.NewReviewOrchestrator(3, []llm.Provider{p1, p1, p1}, dir, cmds.ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}

	var rounds []int
	wantErr := errors.New("transport error")
	err = orch.Run(func(spec cmds.RoundSpec) error {
		rounds = append(rounds, spec.Round)
		if spec.Round == 1 {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if len(rounds) != 1 {
		t.Errorf("rounds executed = %v, want [1] (error stops the chain)", rounds)
	}
}

// TestRunAdversarialReviewRounds_MissingReportFlaggedInNextPrompt verifies
// report bookkeeping: a round that failed to save its report is flagged in
// the next round's prompt (and its path is NOT listed); a saved report's
// path IS listed.
func TestRunAdversarialReviewRounds_MissingReportFlaggedInNextPrompt(t *testing.T) {
	p1 := &acpReviewMockProvider{name: "a", model: "model-a"}
	dir := t.TempDir()

	orch, err := cmds.NewReviewOrchestrator(2, []llm.Provider{p1, p1}, dir, cmds.ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}

	var round2Prompt string
	err = orch.Run(func(spec cmds.RoundSpec) error {
		if spec.Round == 2 {
			round2Prompt = spec.Prompt
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(round2Prompt, "该轮未能成功保存报告，跳过") {
		t.Error("round 2 prompt must flag round 1's missing report")
	}
	if strings.Contains(round2Prompt, "round-1-review-model-a.md") {
		t.Error("round 1's path must not be listed when the report is missing")
	}
}

func TestRunAdversarialReviewRounds_SavedReportPathListedInNextPrompt(t *testing.T) {
	p1 := &acpReviewMockProvider{name: "a", model: "model-a"}
	dir := t.TempDir()

	// Pre-create round 1's report so Complete records Saved=true.
	report1 := filepath.Join(dir, "round-1-review-model-a.md")
	if err := os.WriteFile(report1, []byte("# report"), 0o644); err != nil {
		t.Fatal(err)
	}

	orch, err := cmds.NewReviewOrchestrator(2, []llm.Provider{p1, p1}, dir, cmds.ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}

	var round2Prompt string
	err = orch.Run(func(spec cmds.RoundSpec) error {
		if spec.Round == 2 {
			round2Prompt = spec.Prompt
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(round2Prompt, report1) {
		t.Errorf("round 2 prompt must list round 1's saved report path, got:\n%s", round2Prompt)
	}
	if strings.Contains(round2Prompt, "该轮未能成功保存报告") {
		t.Error("saved report must not be flagged as missing")
	}
}
