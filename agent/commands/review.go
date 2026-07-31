package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// This file holds everything the /review code review command needs beyond the
// command registry (commands.go) and the prompt texts (prompts.go): shared
// orchestration helpers used by the TUI and ACP paths, plus the configuration
// defaults for /review parameters (see
// docs/2026-07-30-adversarial-review-design.md). Everything here is a pure
// function of (config, round bookkeeping) — the two frontends differ only in
// how they drive the round streams (tea.Cmd loop vs streamToACP), which is
// why that part stays per-frontend.

// DefaultReviewMaxIterations is the default iteration budget for /review.
const DefaultReviewMaxIterations = 200

// DefaultReviewAllowedTools returns a fresh copy of the default allowed tool
// list for /review. Callers must not modify the returned slice.
func DefaultReviewAllowedTools() []string {
	return []string{"Bash", "ReadFile", "WriteFile", "Glob", "Grep"}
}

// maxReviewRounds caps the adversarial /review round count.
const maxReviewRounds = 10

// ResolveReviewRounds decides how many rounds this /review run gets.
// input is the full command text (e.g. "/review 6"). Rules:
//
//  1. "/review N" (N >= 2) → N rounds (clamped to maxReviewRounds)
//  2. "/review 0", "/review 1", negative or non-numeric argument → single round
//  3. "/review" (no argument) → single round (normal review; multi-round
//     requires an explicit round count — no config source anymore)
//
// When 1 is returned, callers keep the existing single-round path unchanged.
func ResolveReviewRounds(input string) int {
	parts := strings.Fields(input)
	if len(parts) < 2 {
		return 1 // no argument → normal single-round review
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		// Non-numeric argument (e.g. "/review foo") → single round; do not
		// guess a round count — a typo must not upgrade into N× cost.
		return 1
	}
	if n < 2 {
		return 1 // "/review 0", "/review 1", negative → single round
	}
	return min(n, maxReviewRounds)
}

// ResolveRoundModels assigns resolved providers to each adversarial round.
// models is the per-round provider list (from review.adversarial.models);
// empty → every round uses fallback. Non-empty → round i gets models[i % len].
// judge (from review.adversarial.judge_model) fixes the final round when set.
//
// Pure allocation, never errors: "configured but failed to resolve" must be
// checked by the caller BEFORE calling this (nil entries in models).
func ResolveRoundModels(models []llm.Provider, judge, fallback llm.Provider, rounds int) []llm.Provider {
	providers := make([]llm.Provider, rounds)
	for i := range providers {
		if len(models) == 0 {
			providers[i] = fallback
		} else {
			providers[i] = models[i%len(models)]
		}
	}
	if judge != nil {
		providers[rounds-1] = judge // final round fixed
	}
	return providers
}

// CheckAdversarialProviders is the fail-fast gate for adversarial review
// model resolution, shared by TUI and ACP (both previously copied this check;
// see docs/2026-07-30 §7). It verifies that every model name configured in
// review.adversarial has been successfully resolved to a provider at
// Configure time. A nil entry in models (or a nil judge while judge_model is
// configured) means resolution failed — returning an error aborts the review
// before round 1, instead of silently falling back to the main model and
// turning "multi-model adversarial review" into a lie.
//
// cfg may be nil; when `adversarial:` is unconfigured the pointer is nil and
// the check passes (nothing was configured, nothing can have failed).
func CheckAdversarialProviders(cfg *config.Config, models []llm.Provider, judge llm.Provider) error {
	var adv *config.AdversarialReviewConfig
	if cfg != nil && cfg.Review.Adversarial != nil {
		adv = cfg.Review.Adversarial
	}

	if adv != nil && len(adv.Models) > 0 {
		if len(models) != len(adv.Models) {
			return fmt.Errorf("对抗式审查模型解析失败：配置了 %d 个模型但未能全部解析", len(adv.Models))
		}
		for i, p := range models {
			if p == nil {
				return fmt.Errorf("对抗式审查模型解析失败：模型 %q 无法解析为 provider，请检查 review.adversarial.models 配置", adv.Models[i])
			}
		}
	}
	if adv != nil && adv.JudgeModel != "" && judge == nil {
		return fmt.Errorf("对抗式审查模型解析失败：judge_model %q 无法解析为 provider", adv.JudgeModel)
	}
	return nil
}

// ReviewOptions is the /review runtime configuration resolved from config,
// minus the provider (each frontend resolves its provider from its own agent,
// since commands must not depend on the agent package). Shared by TUI and ACP
// so the default/override rules live in one place.
type ReviewOptions struct {
	MaxIterations int
	AllowedTools  []string
	Thinking      *bool
}

// ResolveReviewOptions applies the config defaults for /review parameters:
// max_iterations (default 200), allowed_tools (default
// [Bash, ReadFile, WriteFile, Glob, Grep]), thinking (default false).
func ResolveReviewOptions(cfg *config.Config) ReviewOptions {
	// MaxIterations and Thinking are populated by defaults.Set() from struct tags.
	maxIter := DefaultReviewMaxIterations
	thinking := new(bool)
	if cfg != nil {
		maxIter = cfg.Review.MaxIterations
		thinking = cfg.Review.Thinking
	}

	// AllowedTools is a slice and can't use the `default` tag — handle in code.
	allowedTools := DefaultReviewAllowedTools()
	if cfg != nil && len(cfg.Review.AllowedTools) > 0 {
		allowedTools = cfg.Review.AllowedTools
	}

	return ReviewOptions{
		MaxIterations: maxIter,
		AllowedTools:  allowedTools,
		Thinking:      thinking,
	}
}

// NewReviewReportDir creates the orchestrator-owned report directory
// "<baseDir>/.tachi/reviews/<YYYYMMDD-HHmmss>" and returns its path. Seconds
// precision (20060102-150405) — minute precision would share a directory
// between two /review runs in the same minute and round reports would
// overwrite each other (MkdirAll is idempotent, no error).
//
// baseDir anchors the report directory: "" → the process CWD (the TUI
// frontend's convention — the LLM writes reports relative to the process
// CWD, which IS the project root there). ACP passes sess.cwd instead, so the
// orchestrator's os.Stat verification and the LLM's WriteFile resolve to the
// SAME absolute path even when sess.cwd != process CWD. Callers MUST pass
// the same base the round's tools resolve relative paths against.
func NewReviewReportDir(baseDir string) (string, error) {
	rel := fmt.Sprintf(".tachi/reviews/%s", time.Now().Format("20060102-150405"))
	dir := rel
	if baseDir != "" {
		dir = filepath.Join(baseDir, rel)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// BuildRoundPrompt computes everything a review round needs from its position
// in the chain: the role (final round always Judge), the orchestrator-owned
// exact output path (no placeholders — written into the prompt verbatim), and
// the full user prompt referencing the prior rounds' report status.
//
// ReviewOrchestrator.Next() calls this for every multi-round spec, so the
// per-round bookkeeping can never drift across frontends.
func BuildRoundPrompt(dir string, round, totalRounds int, provider llm.Provider, prev []RoundReport) (ReviewRole, string, string) {
	role := ResolveRole(round, totalRounds)
	outPath := ReportPathFor(dir, round, role, provider.Model())
	prompt := BuildReviewPrompt(role, round, totalRounds, outPath, prev)
	return role, outPath, prompt
}

// RoundReportFrom verifies whether a report landed on disk (os.Stat — the
// orchestrator does not trust the LLM's word) and records the result as a
// RoundReport for the next round's prompt (Saved=false entries are flagged
// as missing and skipped).
func RoundReportFrom(round int, path string) RoundReport {
	_, err := os.Stat(path)
	return RoundReport{Round: round, Path: path, Saved: err == nil}
}

// ErrStopReview signals a frontend-initiated stop of a review chain (e.g. an
// ACP client disconnect mid-round). Run() propagates it as-is; frontends use
// errors.Is to distinguish "stopped by the user" from "failed".
var ErrStopReview = errors.New("review chain stopped")

// RoundSpec describes one review round: everything a frontend needs to fork
// an agent and start its one-off stream. The orchestrator computes role,
// prompt and output path from its round bookkeeping — frontends must NOT
// re-derive them (that is exactly the drift the TUI/ACP copies had).
type RoundSpec struct {
	Round    int
	Role     ReviewRole
	Provider llm.Provider
	OutPath  string // multi-round: orchestrator-owned report path (in prompt); single-round: empty (LLM names its own)
	Prompt   string
	Kind     string // OneOffMeta.Kind: "review" (single) / "review-round-N"
}

// ReviewOrchestrator owns the full /review orchestration state shared by the
// TUI, ACP and channel frontends: round resolution, per-round providers,
// report directory, round counter and report bookkeeping. Frontends only:
//
//  1. construct it (NewReviewOrchestratorFromCommand, or the low-level
//     NewReviewOrchestrator for tests)
//  2. drive rounds — event-driven frontends (TUI) call Next() to start a
//     round and Complete() when its stream ends; synchronous frontends
//     (ACP/channel) can use Run()
//  3. render per-round output (banner, report hints) from RoundSpec /
//     Complete's RoundReport
//
// Everything else — the single/multi-round route, the
// Reviewer→Challenger→Judge role cycle, deterministic report paths,
// on-disk verification, chain continuation — lives here.
type ReviewOrchestrator struct {
	rounds    int
	providers []llm.Provider
	reportDir string
	current   int
	reports   []RoundReport
	opts      ReviewOptions
}

// NewReviewOrchestrator builds an orchestrator from already-resolved inputs.
// reportDir is only meaningful in multi-round mode (pass "" for single-round
// — the LLM names its own report there). providers must have length == rounds.
func NewReviewOrchestrator(rounds int, providers []llm.Provider, reportDir string, opts ReviewOptions) (*ReviewOrchestrator, error) {
	if rounds < 1 {
		return nil, fmt.Errorf("review rounds must be >= 1, got %d", rounds)
	}
	if len(providers) != rounds {
		return nil, fmt.Errorf("review provider count %d != rounds %d", len(providers), rounds)
	}
	return &ReviewOrchestrator{
		rounds:    rounds,
		providers: providers,
		reportDir: reportDir,
		reports:   make([]RoundReport, 0, rounds), // preallocated: tests snapshot the backing array mid-run
		opts:      opts,
	}, nil
}

// NewReviewOrchestratorFromCommand resolves everything from the command text:
// rounds (ResolveReviewRounds), per-round providers (via the frontend's
// resolve callback — single round returns [review.provider], multi-round goes
// through the adversarial model assignment with fail-fast), and the report
// directory (created up front for multi-round). Any resolution failure aborts
// before round 1.
//
// baseDir anchors the multi-round report directory — pass "" for the process
// CWD (TUI) or the session working directory (ACP sess.cwd). It must match
// the base the round's Bash/WriteFile tools resolve relative paths against,
// otherwise the orchestrator's on-disk verification (os.Stat) and the LLM's
// WriteFile would disagree (see NewReviewReportDir).
func NewReviewOrchestratorFromCommand(input string, opts ReviewOptions, resolve func(rounds int) ([]llm.Provider, error), baseDir string) (*ReviewOrchestrator, error) {
	rounds := ResolveReviewRounds(input)
	providers, err := resolve(rounds)
	if err != nil {
		return nil, err
	}
	reportDir := ""
	if rounds > 1 {
		reportDir, err = NewReviewReportDir(baseDir)
		if err != nil {
			return nil, fmt.Errorf("创建报告目录失败: %w", err)
		}
	}
	return NewReviewOrchestrator(rounds, providers, reportDir, opts)
}

// IsMultiRound reports whether this run is the multi-round adversarial
// review (frontends use it to skip single-round-only UI like banners).
func (o *ReviewOrchestrator) IsMultiRound() bool { return o.rounds > 1 }

// TotalRounds returns the total round count (1 for single-round reviews).
func (o *ReviewOrchestrator) TotalRounds() int { return o.rounds }

// CurrentRound returns the in-flight round (0 before Next()).
func (o *ReviewOrchestrator) CurrentRound() int { return o.current }

// Options returns the resolved /review parameters (max_iterations,
// allowed_tools, thinking) for forking agents.
func (o *ReviewOrchestrator) Options() ReviewOptions { return o.opts }

// Reports returns the per-round report status in round order (incl.
// not-saved markers).
func (o *ReviewOrchestrator) Reports() []RoundReport { return o.reports }

// Next advances to the next round and returns its spec; ok=false means the
// chain is exhausted (only reachable by mis-driving — Complete returns done
// on the final round).
func (o *ReviewOrchestrator) Next() (RoundSpec, bool) {
	if o.current >= o.rounds {
		return RoundSpec{}, false
	}
	o.current++
	round := o.current
	provider := o.providers[round-1]
	if o.rounds == 1 {
		// Single-round path unchanged: standard review prompt, LLM-named report.
		return RoundSpec{
			Round:    1,
			Provider: provider,
			Prompt:   ReviewUserPrompt(),
			Kind:     "review",
		}, true
	}
	role, outPath, prompt := BuildRoundPrompt(o.reportDir, round, o.rounds, provider, o.reports)
	return RoundSpec{
		Round:    round,
		Role:     role,
		Provider: provider,
		OutPath:  outPath,
		Prompt:   prompt,
		Kind:     fmt.Sprintf("review-round-%d", round),
	}, true
}

// Complete records the current round's outcome (on-disk verification via
// RoundReportFrom — the orchestrator does not trust the LLM's word) and
// returns done=true when the chain has finished. A missing report is still
// recorded so the NEXT round's prompt can flag it (BuildReviewPrompt's prev
// parameter). Single-round reviews have no orchestrator-owned path — returns
// (true, empty RoundReport).
func (o *ReviewOrchestrator) Complete() (bool, RoundReport) {
	if o.rounds == 1 {
		return true, RoundReport{}
	}
	role := ResolveRole(o.current, o.rounds)
	outPath := ReportPathFor(o.reportDir, o.current, role, o.providers[o.current-1].Model())
	report := RoundReportFrom(o.current, outPath)
	o.reports = append(o.reports, report)
	return o.current >= o.rounds, report
}

// Run drives the whole chain synchronously — the ACP/channel pattern.
// runRound must execute one round (fork + stream) and return nil to continue;
// any error (including ErrStopReview wrapped by the frontend) terminates the
// chain immediately and is propagated.
func (o *ReviewOrchestrator) Run(runRound func(RoundSpec) error) error {
	for {
		spec, ok := o.Next()
		if !ok {
			return nil
		}
		if err := runRound(spec); err != nil {
			return err
		}
		done, _ := o.Complete()
		if done {
			return nil
		}
	}
}
