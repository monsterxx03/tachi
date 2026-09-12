package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// ---- mock provider ----

type reviewMockProvider struct {
	name  string
	model string
}

func (p *reviewMockProvider) Name() string         { return p.name }
func (p *reviewMockProvider) ProviderName() string { return "" }
func (p *reviewMockProvider) Model() string        { return p.model }
func (p *reviewMockProvider) CreateChat(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *reviewMockProvider) CreateChatStream(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("not implemented")
}

func testProviders(models ...string) []llm.Provider {
	out := make([]llm.Provider, 0, len(models))
	for _, m := range models {
		out = append(out, &reviewMockProvider{name: "prov-" + m, model: m})
	}
	return out
}

// ---- ResolveReviewRounds ----

func TestResolveReviewRounds_NoArgumentSingle(t *testing.T) {
	if got := ResolveReviewRounds("/review"); got != 1 {
		t.Errorf("/review = %d, want 1 (no argument → normal review)", got)
	}
	if got := ResolveReviewRounds(""); got != 1 {
		t.Errorf("empty input = %d, want 1", got)
	}
}

func TestResolveReviewRounds_ExplicitRounds(t *testing.T) {
	for input, want := range map[string]int{
		"/review 2":  2,
		"/review 6":  6,
		"/review 10": 10,
	} {
		if got := ResolveReviewRounds(input); got != want {
			t.Errorf("%s = %d, want %d", input, got, want)
		}
	}
}

func TestResolveReviewRounds_ZeroOneNegative(t *testing.T) {
	for _, input := range []string{"/review 0", "/review 1", "/review -3"} {
		if got := ResolveReviewRounds(input); got != 1 {
			t.Errorf("%s = %d, want 1", input, got)
		}
	}
}

func TestResolveReviewRounds_NonNumericArgument(t *testing.T) {
	// A typo must not upgrade into N× cost — single round, always.
	for _, input := range []string{"/review foo", "/review --depth 2"} {
		if got := ResolveReviewRounds(input); got != 1 {
			t.Errorf("%s = %d, want 1", input, got)
		}
	}
}

func TestResolveReviewRounds_ClampUpper(t *testing.T) {
	if got := ResolveReviewRounds("/review 20"); got != maxReviewRounds {
		t.Errorf("/review 20 = %d, want %d", got, maxReviewRounds)
	}
}

// ---- ResolveRoundModels ----

func TestResolveRoundModels_EmptyModelsAllFallback(t *testing.T) {
	fallback := &reviewMockProvider{name: "fallback", model: "main-model"}
	got := ResolveRoundModels(nil, nil, fallback, 6)
	if len(got) != 6 {
		t.Fatalf("len = %d, want 6", len(got))
	}
	for i, p := range got {
		if p != fallback {
			t.Errorf("round %d = %v, want fallback", i+1, p)
		}
	}
}

func TestResolveRoundModels_SingleModelAllRounds(t *testing.T) {
	m := testProviders("sonnet")
	fallback := &reviewMockProvider{name: "fb", model: "fb"}
	got := ResolveRoundModels(m, nil, fallback, 4)
	for i, p := range got {
		if p != m[0] {
			t.Errorf("round %d = %v, want sonnet", i+1, p)
		}
	}
}

func TestResolveRoundModels_ModuloAssignment(t *testing.T) {
	models := testProviders("sonnet", "gpt-4o", "opus")
	fallback := &reviewMockProvider{name: "fb", model: "fb"}

	// Example 2 from the design: 3 models, 6 rounds → R1=s, R2=g, R3=o, R4=s, R5=g, R6=o (final Judge = opus).
	got := ResolveRoundModels(models, nil, fallback, 6)
	want := []string{"sonnet", "gpt-4o", "opus", "sonnet", "gpt-4o", "opus"}
	for i, w := range want {
		if got[i].Model() != w {
			t.Errorf("round %d = %s, want %s", i+1, got[i].Model(), w)
		}
	}

	// Example 3: 2 models, 5 rounds → final round lands on the modulo result.
	got = ResolveRoundModels(models[:2], nil, fallback, 5)
	want = []string{"sonnet", "gpt-4o", "sonnet", "gpt-4o", "sonnet"}
	for i, w := range want {
		if got[i].Model() != w {
			t.Errorf("round %d = %s, want %s", i+1, got[i].Model(), w)
		}
	}
}

func TestResolveRoundModels_JudgeFixesFinalRound(t *testing.T) {
	models := testProviders("sonnet", "gpt-4o")
	judge := &reviewMockProvider{name: "judge", model: "claude-opus"}
	fallback := &reviewMockProvider{name: "fb", model: "fb"}

	// Example 4 from the design: judge_model fixes round 5.
	got := ResolveRoundModels(models, judge, fallback, 5)
	want := []string{"sonnet", "gpt-4o", "sonnet", "gpt-4o", "claude-opus"}
	for i, w := range want {
		if got[i].Model() != w {
			t.Errorf("round %d = %s, want %s", i+1, got[i].Model(), w)
		}
	}

	// Judge with no per-round models: all fallback except the final.
	got = ResolveRoundModels(nil, judge, fallback, 3)
	if got[0] != fallback || got[1] != fallback || got[2] != judge {
		t.Errorf("judge-only assignment wrong: %v %v %v", got[0], got[1], got[2])
	}
}

// ---- ResolveRole (role cycle + final round fixed as Judge) ----

func TestResolveRole_Cycle(t *testing.T) {
	cases := []struct {
		round, total int
		want         ReviewRole
	}{
		{1, 3, RoleReviewer},
		{2, 3, RoleChallenger},
		{3, 3, RoleJudge}, // final → Judge
		{4, 5, RoleReviewer},
		{5, 5, RoleJudge}, // final → Judge
		{1, 2, RoleReviewer},
		{2, 2, RoleJudge}, // final → Judge
		{3, 5, RoleJudge}, // middle Judge (not final)
		{4, 4, RoleJudge}, // rounds%3==1 → two consecutive Judges (accepted for v1)
		{7, 10, RoleReviewer},
		{10, 10, RoleJudge},
	}
	for _, c := range cases {
		if got := ResolveRole(c.round, c.total); got != c.want {
			t.Errorf("ResolveRole(%d, %d) = %v, want %v", c.round, c.total, got, c.want)
		}
	}
}

// ---- ReportPathFor / sanitizeFileName ----

func TestReportPathFor(t *testing.T) {
	got := ReportPathFor("/tmp/reviews", 2, RoleChallenger, "gpt-4o")
	want := "/tmp/reviews/round-2-challenge-gpt-4o.md"
	if got != want {
		t.Errorf("ReportPathFor = %q, want %q", got, want)
	}

	// Model IDs with path-illegal characters are sanitized.
	got = ReportPathFor("/tmp/reviews", 1, RoleReviewer, "qwen3:32b")
	want = "/tmp/reviews/round-1-review-qwen3-32b.md"
	if got != want {
		t.Errorf("ReportPathFor sanitized = %q, want %q", got, want)
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		"qwen3:32b":  "qwen3-32b",
		"a/b/c":      "a-b-c",
		"plain-name": "plain-name",
		"x y":        "x-y",
	}
	for in, want := range cases {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewReviewReportDir_CollisionSafe verifies two report dirs created in
// the same second don't share a directory (S4): the second call must fall
// back to a numeric-suffix name instead of silently reusing the first
// (MkdirAll was idempotent, so reports would have overwritten each other).
func TestNewReviewReportDir_CollisionSafe(t *testing.T) {
	base := t.TempDir()

	d1, err := NewReviewReportDir(base)
	if err != nil {
		t.Fatalf("NewReviewReportDir #1: %v", err)
	}
	d2, err := NewReviewReportDir(base)
	if err != nil {
		t.Fatalf("NewReviewReportDir #2: %v", err)
	}

	if d1 == d2 {
		t.Errorf("same-second report dirs must not collide, got %q twice", d1)
	}
	if info, err := os.Stat(d1); err != nil || !info.IsDir() {
		t.Errorf("report dir %q missing or not a dir: %v", d1, err)
	}
	if info, err := os.Stat(d2); err != nil || !info.IsDir() {
		t.Errorf("report dir %q missing or not a dir: %v", d2, err)
	}
	if filepath.Dir(d1) != filepath.Dir(d2) {
		t.Errorf("report dirs should share the reviews root: %q vs %q", filepath.Dir(d1), filepath.Dir(d2))
	}
}

// TestRoleFileSuffix_UnknownRoleDoesNotPanic guards the defensive fallback —
// a future ReviewRole value must not index out of range.
func TestRoleFileSuffix_UnknownRoleDoesNotPanic(t *testing.T) {
	for _, r := range []ReviewRole{RoleReviewer, RoleChallenger, RoleJudge, ReviewRole(99)} {
		suffix := roleFileSuffix(r)
		if suffix == "" {
			t.Errorf("roleFileSuffix(%d) returned empty", r)
		}
	}
	if got := roleFileSuffix(RoleJudge); got != "judge" {
		t.Errorf("roleFileSuffix(Judge) = %q, want judge", got)
	}
}

// TestRoleEnName_UnknownRoleDoesNotPanic mirrors the roleFileSuffix guard for
// the English mapping — BuildReviewPrompt previously indexed a raw slice
// ([]string{"Reviewer","Challenger","Judge"}[role]); a future ReviewRole value
// must not panic.
func TestRoleEnName_UnknownRoleDoesNotPanic(t *testing.T) {
	for _, r := range []ReviewRole{RoleReviewer, RoleChallenger, RoleJudge, ReviewRole(99)} {
		name := RoleEnName(r)
		if name == "" {
			t.Errorf("RoleEnName(%d) returned empty", r)
		}
	}
	for r, want := range map[ReviewRole]string{
		RoleReviewer:   "Reviewer",
		RoleChallenger: "Challenger",
		RoleJudge:      "Judge",
	} {
		if got := RoleEnName(r); got != want {
			t.Errorf("RoleEnName(%v) = %q, want %q", r, got, want)
		}
	}
}

// ---- CheckAdversarialProviders (fail-fast gate shared by TUI + ACP) ----

func TestCheckAdversarialProviders_PassesWhenUnconfigured(t *testing.T) {
	cfg := config.DefaultConfig() // adversarial pointer stays nil
	if err := CheckAdversarialProviders(cfg, nil, nil); err != nil {
		t.Errorf("unconfigured must pass, got %v", err)
	}
	if err := CheckAdversarialProviders(nil, nil, nil); err != nil {
		t.Errorf("nil cfg must pass, got %v", err)
	}
	// Configured pointer with no models/judge_model also passes (all fallback).
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{}
	if err := CheckAdversarialProviders(cfg, nil, nil); err != nil {
		t.Errorf("configured without models must pass, got %v", err)
	}
}

func TestCheckAdversarialProviders_NilModelEntryFails(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{Models: []string{"bad-name"}}
	err := CheckAdversarialProviders(cfg, []llm.Provider{nil}, nil)
	if err == nil {
		t.Fatal("nil resolved model must fail fast")
	}
	if !strings.Contains(err.Error(), "bad-name") {
		t.Errorf("error should name the failing model, got %v", err)
	}
}

func TestCheckAdversarialProviders_CountMismatchFails(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{Models: []string{"a", "b"}}
	if err := CheckAdversarialProviders(cfg, []llm.Provider{testProviders("a")[0]}, nil); err == nil {
		t.Fatal("resolution count mismatch must fail fast")
	}
}

func TestCheckAdversarialProviders_NilJudgeFails(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{JudgeModel: "bad-judge"}
	err := CheckAdversarialProviders(cfg, nil, nil)
	if err == nil {
		t.Fatal("nil judge with configured judge_model must fail fast")
	}
	if !strings.Contains(err.Error(), "bad-judge") {
		t.Errorf("error should name the failing judge model, got %v", err)
	}
}

func TestCheckAdversarialProviders_ResolvedPasses(t *testing.T) {
	cfg := config.DefaultConfig()
	m := testProviders("ok")
	cfg.Review.Adversarial = &config.AdversarialReviewConfig{Models: []string{"ok"}, JudgeModel: "judge"}
	judge := testProviders("judge")[0]
	if err := CheckAdversarialProviders(cfg, m, judge); err != nil {
		t.Errorf("fully resolved providers must pass, got %v", err)
	}
}

// ---- ResolveReviewOptions (shared /review parameter resolution) ----

func TestResolveReviewOptions_Defaults(t *testing.T) {
	opts := ResolveReviewOptions(nil)
	if opts.MaxIterations != DefaultReviewMaxIterations {
		t.Errorf("MaxIterations = %d, want %d", opts.MaxIterations, DefaultReviewMaxIterations)
	}
	if len(opts.AllowedTools) != 5 || opts.AllowedTools[0] != "Bash" {
		t.Errorf("AllowedTools = %v, want default 5-tool list", opts.AllowedTools)
	}
	if opts.Thinking != nil {
		t.Error("Thinking must default to nil (follow the current session)")
	}
	if opts.ThinkingLevel != "" {
		t.Errorf("ThinkingLevel = %q, want empty (follow the current session)", opts.ThinkingLevel)
	}
	// A nil config (a bootstrap that failed) must not invent a language: the prompt builders
	// turn "" into DefaultReplyLanguage themselves (see languageOf).
	if opts.Language != "" {
		t.Errorf("nil config: Language = %q, want empty", opts.Language)
	}

	cfg := config.DefaultConfig()
	opts = ResolveReviewOptions(cfg)
	if opts.MaxIterations != DefaultReviewMaxIterations {
		t.Errorf("default config MaxIterations = %d, want %d", opts.MaxIterations, DefaultReviewMaxIterations)
	}
	if opts.Thinking != nil {
		t.Error("default config must not pin Thinking (nil = follow the current session)")
	}
	// …and the language follows the config the same way the system prompt's "Reply in …" does:
	// DefaultConfig applies the yaml default (`default:"English"`), so it must land here too.
	if opts.Language != "English" {
		t.Errorf("Language = %q, want the config default English", opts.Language)
	}
}

func TestResolveReviewOptions_ConfigOverrides(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Review.MaxIterations = 42
	cfg.Review.AllowedTools = []string{"Bash", "ReadFile"}
	thinking := true
	cfg.Review.Thinking = &thinking
	cfg.Review.ThinkingLevel = "high"
	cfg.Language = "zh"

	opts := ResolveReviewOptions(cfg)
	if opts.MaxIterations != 42 {
		t.Errorf("MaxIterations = %d, want 42", opts.MaxIterations)
	}
	if len(opts.AllowedTools) != 2 || opts.AllowedTools[0] != "Bash" {
		t.Errorf("AllowedTools = %v, want configured list", opts.AllowedTools)
	}
	if opts.Thinking == nil || !*opts.Thinking {
		t.Error("Thinking must reflect the configured value")
	}
	if opts.ThinkingLevel != "high" {
		t.Errorf("ThinkingLevel = %q, want high", opts.ThinkingLevel)
	}
	// The review report's language comes from the same `language` the session replies in.
	if opts.Language != "zh" {
		t.Errorf("Language = %q, want zh (config.Language must reach the review prompt)", opts.Language)
	}
}

// ---- ResolveReviewThinking (review thinking: config wins, session follows) ----

func TestResolveReviewThinking_FollowsSession(t *testing.T) {
	// Nothing configured → both dimensions follow the session.
	sessT, sessE := new(true), "max"
	gotT, gotE := ResolveReviewThinking(ReviewOptions{}, sessT, sessE)
	if gotT == nil || !*gotT {
		t.Errorf("thinking = %v, want session value true", gotT)
	}
	if gotE != "max" {
		t.Errorf("effort = %q, want session effort max", gotE)
	}

	// Session with no override (nil/"") stays nil/"" — the fork's
	// runAgentLoop fallback resolves the provider/model default.
	gotT, gotE = ResolveReviewThinking(ReviewOptions{}, nil, "")
	if gotT != nil || gotE != "" {
		t.Errorf("unset session must pass through as nil/empty, got %v/%q", gotT, gotE)
	}
}

func TestResolveReviewThinking_ConfigPins(t *testing.T) {
	// thinking pins the switch only; effort follows the session.
	gotT, gotE := ResolveReviewThinking(ReviewOptions{Thinking: new(true)}, new(false), "low")
	if gotT == nil || !*gotT {
		t.Errorf("thinking = %v, want true (config wins)", gotT)
	}
	if gotE != "low" {
		t.Errorf("effort = %q, want session effort low (unpinned)", gotE)
	}

	// thinking=false overrides the session's true.
	gotT, gotE = ResolveReviewThinking(ReviewOptions{Thinking: new(false)}, new(true), "max")
	if gotT == nil || *gotT {
		t.Errorf("thinking = %v, want false (config wins)", gotT)
	}
	if gotE != "max" {
		t.Errorf("effort = %q, want session effort max (unpinned)", gotE)
	}

	// thinking_level pins the effort; switch follows the session.
	gotT, gotE = ResolveReviewThinking(ReviewOptions{ThinkingLevel: "high"}, new(false), "")
	if gotT == nil || *gotT {
		t.Errorf("thinking = %v, want session true (unpinned)", gotT)
	}
	if gotE != "high" {
		t.Errorf("effort = %q, want configured high", gotE)
	}

	// thinking + thinking_level both set: switch from thinking, effort from level.
	gotT, gotE = ResolveReviewThinking(ReviewOptions{Thinking: new(true), ThinkingLevel: "max"}, nil, "")
	if gotT == nil || !*gotT {
		t.Errorf("thinking = %v, want true", gotT)
	}
	if gotE != "max" {
		t.Errorf("effort = %q, want configured max", gotE)
	}
}

func TestResolveReviewThinking_LevelSpecialCases(t *testing.T) {
	// "none" forces the switch off and clears the effort.
	gotT, gotE := ResolveReviewThinking(ReviewOptions{ThinkingLevel: "none"}, new(true), "max")
	if gotT == nil || *gotT {
		t.Errorf("thinking = %v, want false (none forces off)", gotT)
	}
	if gotE != "" {
		t.Errorf("effort = %q, want empty for none", gotE)
	}

	// "default" clears the effort to the provider/model default.
	gotT, gotE = ResolveReviewThinking(ReviewOptions{ThinkingLevel: "default"}, new(true), "max")
	if gotT == nil || !*gotT {
		t.Errorf("thinking = %v, want session true (unpinned)", gotT)
	}
	if gotE != "" {
		t.Errorf("effort = %q, want empty (provider default) for default level", gotE)
	}
}

// ---- NewReviewReportDir ----

func TestNewReviewReportDir(t *testing.T) {
	// chdir to a temp dir so the relative ".tachi/reviews/..." lands cleanly.
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	// baseDir "" → relative to the process CWD (the TUI convention).
	dir, err := NewReviewReportDir("")
	if err != nil {
		t.Fatalf("NewReviewReportDir: %v", err)
	}
	// Format: .tachi/reviews/<YYYYMMDD-HHmmss> (seconds precision).
	base := filepath.Base(dir)
	if len(base) != 15 { // 20060102-150405
		t.Errorf("report dir timestamp %q must be YYYYMMDD-HHmmss (seconds precision)", base)
	}
	if _, err := time.Parse("20060102-150405", base); err != nil {
		t.Errorf("report dir timestamp %q not parseable: %v", base, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("report dir not created as directory: %v", err)
	}

	// Non-empty baseDir anchors the directory there (the ACP convention:
	// sess.cwd may differ from the process CWD). The two must be the same
	// dir the round's WriteFile resolves relative paths against.
	baseDir := t.TempDir()
	absDir, err := NewReviewReportDir(baseDir)
	if err != nil {
		t.Fatalf("NewReviewReportDir(baseDir): %v", err)
	}
	if !filepath.IsAbs(absDir) {
		t.Errorf("report dir with baseDir must be absolute, got %q", absDir)
	}
	if rel, err := filepath.Rel(baseDir, absDir); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("report dir %q not under baseDir %q (rel=%q)", absDir, baseDir, rel)
	}
	if fi, err := os.Stat(absDir); err != nil || !fi.IsDir() {
		t.Errorf("anchored report dir not created as directory: %v", err)
	}
}

// ---- ReviewOrchestrator (shared single/multi-round orchestration) ----

func TestNewReviewOrchestrator_Validation(t *testing.T) {
	if _, err := NewReviewOrchestrator(0, nil, "", ReviewOptions{}); err == nil {
		t.Error("rounds=0 must be rejected")
	}
	if _, err := NewReviewOrchestrator(3, []llm.Provider{testProviders("a")[0]}, "", ReviewOptions{}); err == nil {
		t.Error("provider count != rounds must be rejected")
	}
}

func TestReviewOrchestrator_SingleRoundSpec(t *testing.T) {
	p := testProviders("main")[0]
	orch, err := NewReviewOrchestrator(1, []llm.Provider{p}, "", ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}
	if orch.IsMultiRound() {
		t.Error("single round must not be multi-round")
	}

	spec, ok := orch.Next()
	if !ok {
		t.Fatal("Next must return the single round")
	}
	if spec.Round != 1 || spec.Provider != p || spec.Kind != "review" {
		t.Errorf("spec = %+v, want round 1 / main provider / kind review", spec)
	}
	// Since the artifact follow-up change, single-round reviews also get an
	// orchestrator-owned output path and the prompt instructs the LLM to
	// write the report there.
	if spec.OutPath == "" {
		t.Error("single-round OutPath must be non-empty (orchestrator-owned path)")
	}
	if !strings.Contains(spec.Prompt, spec.OutPath) {
		t.Errorf("single-round prompt must reference the outPath %q", spec.OutPath)
	}

	done, report := orch.Complete()
	if !done {
		t.Error("single round must complete after one round")
	}
	if report.Path == "" {
		t.Error("single-round report must carry the orchestrator-owned path")
	}
	if _, ok := orch.Next(); ok {
		t.Error("Next after completion must return ok=false")
	}
}

func TestReviewOrchestrator_MultiRoundCycle(t *testing.T) {
	models := testProviders("sonnet", "gpt-4o", "opus")
	dir := t.TempDir()
	orch, err := NewReviewOrchestrator(3, models, dir, ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}
	if !orch.IsMultiRound() {
		t.Fatal("3 rounds must be multi-round")
	}

	// Round 1: Reviewer, sonnet.
	spec, ok := orch.Next()
	if !ok || spec.Round != 1 || spec.Role != RoleReviewer || spec.Provider != models[0] || spec.Kind != "review-round-1" {
		t.Fatalf("round 1 spec = %+v ok=%v", spec, ok)
	}
	if spec.OutPath != ReportPathFor(dir, 1, RoleReviewer, "sonnet") {
		t.Errorf("round 1 outPath = %q", spec.OutPath)
	}
	if !strings.Contains(spec.Prompt, spec.OutPath) {
		t.Error("round prompt must contain its exact outPath")
	}

	// Complete round 1 (report missing) → not done, report recorded.
	done, report := orch.Complete()
	if done || report.Saved {
		t.Fatalf("round 1 complete: done=%v saved=%v, want done=false saved=false", done, report.Saved)
	}
	if len(orch.Reports()) != 1 || orch.Reports()[0].Round != 1 {
		t.Fatalf("reports = %+v, want [round 1 missing]", orch.Reports())
	}

	// Round 2: Challenger, gpt-4o.
	spec, ok = orch.Next()
	if !ok || spec.Role != RoleChallenger || spec.Provider != models[1] {
		t.Fatalf("round 2 spec = %+v ok=%v", spec, ok)
	}

	// Round 3: final → fixed Judge, opus.
	orch.Complete()
	spec, ok = orch.Next()
	if !ok || spec.Role != RoleJudge || spec.Provider != models[2] {
		t.Fatalf("round 3 spec = %+v ok=%v", spec, ok)
	}
	done, _ = orch.Complete()
	if !done {
		t.Error("final round must complete the chain")
	}
	if len(orch.Reports()) != 3 {
		t.Errorf("reports = %d, want 3", len(orch.Reports()))
	}
}

func TestReviewOrchestrator_FromCommand(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	p := testProviders("main")[0]
	resolve := func(rounds int) ([]llm.Provider, error) {
		if rounds == 1 {
			return []llm.Provider{p}, nil
		}
		// Real frontends (ResolveAdversarialRoundModels) always return exactly
		// `rounds` providers (modulo-cycled models + fixed judge).
		models := testProviders("a", "b")
		out := make([]llm.Provider, rounds)
		for i := range out {
			out[i] = models[i%len(models)]
		}
		return out, nil
	}

	// No argument → single round. Since the artifact follow-up change the
	// single round ALSO gets an orchestrator-owned report dir + path, so the
	// report can be registered as a followable artifact.
	baseDir := t.TempDir()
	orch, err := NewReviewOrchestratorFromCommand("/review", ReviewOptions{}, resolve, baseDir)
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	if orch.IsMultiRound() || orch.TotalRounds() != 1 {
		t.Errorf("single: total=%d multi=%v, want 1/false", orch.TotalRounds(), orch.IsMultiRound())
	}
	if fi, err := os.Stat(filepath.Join(baseDir, ".tachi/reviews")); err != nil || !fi.IsDir() {
		t.Error("single round must create the report directory (artifact follow-up)")
	}

	// "/review 5" → 5 rounds, report dir created by the orchestrator.
	orch, err = NewReviewOrchestratorFromCommand("/review 5", ReviewOptions{}, resolve, baseDir)
	if err != nil {
		t.Fatalf("multi: %v", err)
	}
	if orch.TotalRounds() != 5 || !orch.IsMultiRound() {
		t.Errorf("multi: total=%d multi=%v, want 5/true", orch.TotalRounds(), orch.IsMultiRound())
	}
	if fi, err := os.Stat(filepath.Join(baseDir, ".tachi/reviews")); err != nil || !fi.IsDir() {
		t.Errorf("multi: report dir not created: %v", err)
	}

	// A non-empty baseDir anchors the report dir there (the ACP convention:
	// sess.cwd may differ from the process CWD — the dir the LLM's WriteFile
	// resolves against MUST match the one the orchestrator verifies).
	anchoredOrch, err := NewReviewOrchestratorFromCommand("/review 3", ReviewOptions{}, resolve, baseDir)
	if err != nil {
		t.Fatalf("multi+baseDir: %v", err)
	}
	spec, ok := anchoredOrch.Next()
	if !ok || spec.OutPath == "" {
		t.Fatalf("anchored round 1 spec = %+v ok=%v", spec, ok)
	}
	if !strings.HasPrefix(spec.OutPath, filepath.Join(baseDir, ".tachi/reviews")) {
		t.Errorf("anchored outPath = %q, want under %q", spec.OutPath, filepath.Join(baseDir, ".tachi/reviews"))
	}
	if _, err := os.Stat(filepath.Join(baseDir, ".tachi/reviews")); err != nil {
		t.Errorf("anchored report dir not created: %v", err)
	}
}

func TestReviewOrchestrator_FromCommandPropagatesResolveError(t *testing.T) {
	wantErr := errors.New("fail fast: bad model")
	_, err := NewReviewOrchestratorFromCommand("/review 3", ReviewOptions{},
		func(int) ([]llm.Provider, error) { return nil, wantErr }, "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want resolve error propagated", err)
	}
}

func TestReviewOrchestrator_Run_SingleRound(t *testing.T) {
	p := testProviders("main")[0]
	orch, err := NewReviewOrchestrator(1, []llm.Provider{p}, "", ReviewOptions{})
	if err != nil {
		t.Fatalf("NewReviewOrchestrator: %v", err)
	}

	calls := 0
	if err := orch.Run(func(spec RoundSpec) error { calls++; return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("runRound calls = %d, want 1 (single round)", calls)
	}
}

// ---- BuildRoundPrompt (role + outPath + prompt in one call) ----

func TestBuildRoundPrompt_MatchesManualComputation(t *testing.T) {
	dir := "/tmp/reviews"
	provider := testProviders("model-x")[0]
	prev := []RoundReport{{Round: 1, Path: "/tmp/reviews/round-1-review-model-x.md", Saved: true}}

	// BuildRoundPrompt must compute exactly what the manual three-liner does
	// (TUI and ACP previously each spelled this out by hand).
	role, outPath, prompt := BuildRoundPrompt(dir, 2, 3, provider, prev, "")
	wantRole := ResolveRole(2, 3)
	wantPath := ReportPathFor(dir, 2, wantRole, provider.Model())
	wantPrompt := BuildReviewPrompt(wantRole, 2, 3, wantPath, prev, "")

	if role != wantRole {
		t.Errorf("role = %v, want %v", role, wantRole)
	}
	if outPath != wantPath {
		t.Errorf("outPath = %q, want %q", outPath, wantPath)
	}
	if prompt != wantPrompt {
		t.Error("prompt differs from manual BuildReviewPrompt computation")
	}

	// Final round is always Judge.
	role, outPath, _ = BuildRoundPrompt(dir, 3, 3, provider, prev, "")
	if role != RoleJudge || outPath != ReportPathFor(dir, 3, RoleJudge, "model-x") {
		t.Errorf("final round: role=%v outPath=%q, want Judge round-3-judge", role, outPath)
	}
}

// ---- RoundReportFrom (os.Stat-backed verification) ----

func TestRoundReportFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "round-1-review-m.md")

	// Missing file → Saved=false.
	r := RoundReportFrom(1, path)
	if r.Round != 1 || r.Path != path || r.Saved {
		t.Errorf("missing report: %+v, want Saved=false", r)
	}

	// Existing file → Saved=true.
	if err := os.WriteFile(path, []byte("# report"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = RoundReportFrom(1, path)
	if !r.Saved {
		t.Errorf("existing report: %+v, want Saved=true", r)
	}
}

// ---- BuildReviewPrompt ----

func TestBuildReviewPrompt_ReviewerRound(t *testing.T) {
	p := BuildReviewPrompt(RoleReviewer, 1, 3, "/reviews/round-1-review-m.md", nil, "")
	for _, want := range []string{
		"第 1 轮审查者", "Round 1/3 — Reviewer",
		"全部变更", "Correctness", "Security", "Maintainability",
		"中间裁决", // round 1 of 3 is not final
		"/reviews/round-1-review-m.md",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("reviewer prompt missing %q", want)
		}
	}
	if strings.Contains(p, "最终轮") {
		t.Error("round 1/3 should not be marked as final round")
	}
}

func TestBuildReviewPrompt_ChallengerListsPriorReports(t *testing.T) {
	prev := []RoundReport{
		{Round: 1, Path: "/reviews/round-1-review-m1.md", Saved: true},
		{Round: 2, Path: "/reviews/round-2-challenge-m2.md", Saved: false}, // missing
	}
	p := BuildReviewPrompt(RoleJudge, 3, 3, "/reviews/round-3-judge-m3.md", prev, "")
	for _, want := range []string{
		"第 3 轮裁决者", "Round 3/3 — Judge",
		"/reviews/round-1-review-m1.md", // saved → path listed
		"该轮未能成功保存报告，跳过",                 // missing → flagged, path NOT listed
		"最终轮", // final round
	} {
		if !strings.Contains(p, want) {
			t.Errorf("judge prompt missing %q", want)
		}
	}
	// The missing round's path must not appear (nothing to read).
	if strings.Contains(p, "/reviews/round-2-challenge-m2.md") {
		t.Error("missing round's path should not be listed — the report does not exist")
	}
	if !strings.Contains(p, "Confirmed") {
		t.Error("final judge prompt should ask for Confirmed/Disputed/Rejected verdicts")
	}
}

func TestBuildReviewPrompt_OutPathVerbatimNoPlaceholders(t *testing.T) {
	p := BuildReviewPrompt(RoleChallenger, 2, 3, "/reviews/r2-challenge.md", []RoundReport{{Round: 1, Path: "/reviews/r1-review.md", Saved: true}}, "")
	if !strings.Contains(p, "/reviews/r2-challenge.md") {
		t.Error("prompt must contain the exact outPath")
	}
	for _, ph := range []string{"<outPath>", "<dir>", "round-<", "placeholder"} {
		if strings.Contains(p, ph) {
			t.Errorf("prompt contains placeholder %q — all paths must be concrete", ph)
		}
	}
}

// TestBuildReviewPrompt_JudgeRound1NeverSaysPriorZero guards the latent
// "前 0 轮" phrasing — callers never hit it (rounds==1 takes a separate path),
// but the prompt must degrade gracefully if it is reused.
func TestBuildReviewPrompt_JudgeRound1NeverSaysPriorZero(t *testing.T) {
	p := BuildReviewPrompt(RoleJudge, 1, 1, "/reviews/r1-judge.md", nil, "")
	if strings.Contains(p, "前 0 轮") {
		t.Error("judge prompt with round==1 must not read '前 0 轮'")
	}
	if !strings.Contains(p, "Confirmed") {
		t.Error("judge prompt must still ask for verdicts")
	}
}

// TestBuildReviewPrompt_UnknownRoleDoesNotPanic guards the whole prompt
// builder against a future ReviewRole value — the header naming must fall
// back instead of panicking on a slice index.
func TestBuildReviewPrompt_UnknownRoleDoesNotPanic(t *testing.T) {
	p := BuildReviewPrompt(ReviewRole(99), 1, 3, "/reviews/round-1-review-m.md", nil, "")
	if p == "" {
		t.Error("prompt must not be empty")
	}
	// The unknown role degrades to the Reviewer fallback in both zh and en.
	for _, want := range []string{"第 1 轮审查者", "Round 1/3 — Reviewer"} {
		if !strings.Contains(p, want) {
			t.Errorf("unknown role prompt missing fallback naming %q", want)
		}
	}
}

// TestReviewAllowlistExcludesReportFinding: the reviewer's whitelist lists what it may
// READ. ReportFinding is added to a review fork by ForkConfig.ForReview instead, so it
// must not appear here — this list is also the parent-registry filter, and the tool is
// deliberately not in the parent registry.
func TestReviewAllowlistExcludesReportFinding(t *testing.T) {
	for _, name := range DefaultReviewAllowedTools() {
		if name == tools.ToolNameReportFinding {
			t.Fatal("ReportFinding must not be in DefaultReviewAllowedTools (it is added by ForReview)")
		}
	}
}

// TestAppendReviewScope covers the text the turn-level entry adds.
func TestAppendReviewScope(t *testing.T) {
	const base = "Review the current repo changes.\n"

	if got := AppendReviewScope(base, nil); got != base {
		t.Errorf("an empty scope must leave the prompt untouched, got %q", got)
	}

	got := AppendReviewScope(base, []string{"src/main.go", "pkg/x.go"})
	for _, want := range []string{
		"## Scope (only these files)",
		"- src/main.go",
		"- pkg/x.go",
		"git diff HEAD -- src/main.go pkg/x.go",
		"git ls-files --others --exclude-standard -- src/main.go pkg/x.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scoped prompt is missing %q\n%s", want, got)
		}
	}
	// The scope is a suffix, not a replacement: the review instructions must survive.
	if !strings.HasPrefix(got, "Review the current repo changes.") {
		t.Error("the scoped prompt dropped the original instructions")
	}
}

// TestReviewScopeReachesThePrompt is the plumbing check that matters: a scoped run
// (desktop's "review these changes") sets ReviewOptions.Scope, and the only thing the
// forked reviewer ever sees is the prompt the orchestrator hands it. If the scope stops
// at ReviewOptions, the review silently widens back to the whole tree.
func TestReviewScopeReachesThePrompt(t *testing.T) {
	opts := ReviewOptions{MaxIterations: 5, AllowedTools: DefaultReviewAllowedTools(), Scope: []string{"src/main.go"}}
	orch, err := NewReviewOrchestrator(1, []llm.Provider{testProviders("mock")[0]}, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}

	spec, ok := orch.Next()
	if !ok {
		t.Fatal("orchestrator produced no round")
	}
	if !strings.Contains(spec.Prompt, "## Scope (only these files)") || !strings.Contains(spec.Prompt, "- src/main.go") {
		t.Errorf("the single-round prompt lost the scope:\n%s", spec.Prompt)
	}

	// Multi-round: every round keeps it (the adversarial rounds discuss the same files).
	opts.Scope = []string{"src/main.go"}
	multi, err := NewReviewOrchestrator(2, testProviders("a", "b"), t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	round, ok := multi.Next()
	if !ok {
		t.Fatal("multi-round orchestrator produced no round")
	}
	if !strings.Contains(round.Prompt, "## Scope (only these files)") {
		t.Errorf("the multi-round prompt lost the scope:\n%s", round.Prompt)
	}
}

// languageDirective is the sentence ReviewLanguageSection renders for a language, and the marker
// the tests below match on. It is deliberately NOT the section heading: the templates used to
// carry an `### Output language` of their own, and any `###` heading contains the `##` prefix —
// so a heading-shaped marker would still be found even if the appended section were dropped
// (which is exactly the regression these tests exist to catch).
func languageDirective(language string) string {
	return "Write the report AND every finding (its text and its suggestion) in " + language
}

// TestReviewLanguageReachesThePrompt: the report is read by a human, so `language` has to travel
// from the config all the way into the prompt the forked reviewer sees — the review templates are
// mixed-language, and a model reading them otherwise answers in whichever language they were
// written in (the complaint: 「review 报告提示 llm 跟随目前 config.language 设定的语言」).
//
// The config → ReviewOptions hop is asserted in TestResolveReviewOptions_*; this one starts at
// ReviewOptions and follows the value into every prompt a round can hand the model.
func TestReviewLanguageReachesThePrompt(t *testing.T) {
	opts := ReviewOptions{MaxIterations: 5, AllowedTools: DefaultReviewAllowedTools(), Language: "zh"}
	// Single round…
	orch, err := NewReviewOrchestrator(1, []llm.Provider{testProviders("mock")[0]}, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := orch.Next()
	if !ok {
		t.Fatal("orchestrator produced no round")
	}
	if !strings.Contains(spec.Prompt, languageDirective("zh")) {
		t.Errorf("single-round prompt lost the language:\n%s", spec.Prompt)
	}
	// …and every round of a multi-round chain (the judge writes the final report).
	multi, err := NewReviewOrchestrator(2, testProviders("a", "b"), t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	rounds := 0
	for {
		round, ok := multi.Next()
		if !ok {
			break
		}
		rounds++
		if !strings.Contains(round.Prompt, languageDirective("zh")) {
			t.Errorf("round %d lost the language:\n%s", round.Round, round.Prompt)
		}
		multi.Complete()
	}
	if rounds != 2 {
		t.Errorf("rounds walked = %d, want 2 (the loop body is what asserts the language)", rounds)
	}
	// The orchestrator is not the only way in: BuildRoundPrompt is also used directly.
	if _, _, p := BuildRoundPrompt(t.TempDir(), 1, 3, testProviders("m")[0], nil, "English"); !strings.Contains(p, languageDirective("English")) {
		t.Errorf("BuildRoundPrompt lost the language:\n%s", p)
	}

	// ONE directive, not two: the templates used to carry their own `### Output language` pointing
	// at the SYSTEM PROMPT while ReviewLanguageSection points at the config — two sources of truth
	// for one choice, which a model may resolve either way. Both builders must now say it once.
	for name, p := range map[string]string{
		"single round": ReviewUserPrompt("/r.md", "zh"),
		"multi round":  BuildReviewPrompt(RoleJudge, 2, 2, "/r.md", nil, "zh"),
	} {
		if strings.Contains(p, "reply language instruction") {
			t.Errorf("%s: the stale 'language specified by your system prompt' directive is back:\n%s", name, p)
		}
		if n := strings.Count(p, languageDirective("zh")); n != 1 {
			t.Errorf("%s: the language directive appears %d times, want exactly 1", name, n)
		}
	}
}

// TestReviewLanguageEmptyFallsBack: an unset `language` must not leave a hollow "write in ''" —
// the same sentinel the system prompt uses applies here (DefaultReplyLanguage). Asserted through
// both prompt builders rather than on the section alone: the fallback matters where it lands.
func TestReviewLanguageEmptyFallsBack(t *testing.T) {
	want := languageDirective(DefaultReplyLanguage)
	for name, got := range map[string]string{
		"reviewer round": BuildReviewPrompt(RoleReviewer, 1, 1, "/r.md", nil, ""),
		"single round":   ReviewUserPrompt("/r.md", ""),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s: empty language must fall back to %q:\n%s", name, DefaultReplyLanguage, got)
		}
	}
}
