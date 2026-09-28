package manager

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/channel/manager/notifygate"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/jev"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// notifyGate asks a System One judgment whether a finished when_relevant run
// should be delivered, on top of the [SILENT] marker the agent types into its
// own reply. The question, the state and the case set that calibrated the
// threshold live in channel/manager/notifygate; this file owns the decision.
//
// The marker stays the FALLBACK, and the prompt follows the decider (see
// buildCronPrompt's two policies): while this gate is answering, the agent is
// told to REPORT rather than to judge, because an agent that has already decided
// a finding is not worth mentioning hands the gate nothing to judge; while it is
// not, the agent gets its old instruction back, because the fallback is a marker
// and a marker has to be asked for. What that costs is stated where it belongs:
// the gate filters what it is shown, and never recovers a notification the agent
// decided not to write.
type notifyGate struct {
	judge     jev.Judge
	threshold float64
	// cfg is the live config, read when a row is priced: a ledger row carries a
	// price snapshot, and the snapshot has to be the one in force at the call.
	cfg    *config.Config
	usage  *llm.UsageRecorder
	logger *logger.Logger

	// degraded reports that the LAST judgment failed (or was left unanswered).
	// buildCronPrompt reads it to hand the agent its own judgment back: while
	// the service is not answering, the fallback is the marker, and a marker is
	// only readable if the prompt still asks for one. It is recomputed per call
	// rather than latched — a healthy judgment clears it, so one hiccup costs
	// one run's worth of judgment instead of the rest of the process's.
	degraded atomic.Bool
}

// notifyDecision is the verdict on one finished run.
type notifyDecision struct {
	// Deliver is whether the user is told about this run.
	Deliver bool
	// Content is what to send. It can be empty on a delivery the gate decided
	// on, which the caller treats as nothing to say.
	Content string
	// Decider names the layer that decided, for the log: "always" (the job's
	// policy is not when_relevant), "jev" (the judgment answered), "marker"
	// (the fallback, or the report was the marker alone) or "none" (nothing was
	// reported at all). A suppression has to be traceable to one of them.
	Decider string
}

// initNotifyGate builds the judgment a when_relevant job's result goes through,
// or leaves notifyGate nil so the marker decides alone.
//
// Like the MCP tool-search gate, the client is built once per process (channel
// mode reads its config at startup — there is no hot reload to push into) and
// the matcher decides whether it exists: "marker" forces the old path (and is
// how the gate is turned off without emptying the jev block), "jev" forces the
// gate, "auto" takes it when config.Jev.Configured() — enabled AND holding a
// key — so an installation that never filled that block keeps exactly the
// behaviour it had.
func (m *Manager) initNotifyGate(ctx context.Context) {
	matcher := m.cfg.Cron.NotifyMatcher
	jevCfg := m.cfg.Jev

	switch {
	case matcher == "marker":
		m.logger.Info(ctx, "channel: cron notify judged by the agent's marker", "matcher", matcher)
		return
	case matcher == "auto" && !jevCfg.Configured():
		m.logger.Info(ctx, "channel: cron notify judged by the agent's marker",
			"matcher", matcher, "reason", "the jev block is not usable (needs enabled: true and an api_key)")
		return
	}

	client, err := jev.NewClient(jevCfg.APIKey,
		jev.WithModel(jevCfg.Model),
		jev.WithBaseURL(jevCfg.BaseURL),
		jev.WithHTTPClient(httpx.NewClient(time.Duration(jevCfg.Timeout), jevCfg.Proxy)),
		jev.WithLogger(m.logger),
	)
	if err != nil {
		// Includes the forced case (matcher=jev with an unusable block): a gate
		// that cannot run is silently the old path, so the warning names both.
		m.logger.Warn(ctx, "channel: cron notify gate unavailable — the marker decides",
			"matcher", matcher, "err", err)
		return
	}

	m.notifyGate = &notifyGate{
		judge:     client,
		threshold: m.cfg.Cron.JevThreshold,
		cfg:       m.cfg,
		usage:     agent.GlobalUsageRecorder(),
		logger:    m.logger,
	}
	m.logger.Info(ctx, "channel: cron notify judged by Jev",
		"model", client.Model(), "threshold", m.cfg.Cron.JevThreshold)
}

// judgeNotify decides whether a finished cron run should reach the user. It is
// called for EVERY cron run — notify=always included — so the policy lives in
// one place instead of in whichever branch happens to deliver.
//
// runErr is how the turn ended, which is what the state's outcome line reports.
// It is passed in rather than read off the job because the scheduler records
// LastRunStatus only AFTER this handler returns, so the job still carries the
// PREVIOUS run's status here.
func (m *Manager) judgeNotify(ctx context.Context, job *cron.Job, result, sessionID string, runErr error) notifyDecision {
	if job.EffectiveNotify() != cron.NotifyWhenRelevant {
		// The gate owns when_relevant only: an always-job keeps the behaviour it
		// has had, including its silence when the turn itself failed.
		return notifyDecision{Deliver: runErr == nil, Content: result, Decider: "always"}
	}

	// A finished run that reported nothing gives the judgment nothing to read,
	// so there is no call to spend. The agent was asked for the marker and
	// produced nothing else, which is the one reading the marker was ever right
	// about. A run that FAILED with an empty report is not this case: the
	// failure is the content, and the yes pole names it.
	trimmed := strings.TrimSpace(result)
	if runErr == nil && trimmed == "" {
		return notifyDecision{Decider: "none"}
	}
	if runErr == nil && isMarkerOnly(trimmed) {
		return notifyDecision{Decider: "marker"}
	}

	if m.notifyGate != nil {
		if decision := m.notifyGate.ask(ctx, job, result, sessionID, runErr); decision != nil {
			return *decision
		}
	}
	// No gate, or no judgment from it (service down, timed out, question left
	// unanswered): the marker decides, exactly as it did before this existed.
	return markerDecision(job, result, runErr)
}

// markerDecision is the pre-gate behaviour, kept as the fallback and NOT
// tightened alongside the gate's own fast path: a fallback that quietly changed
// what "suppressed" means would make the two layers indistinguishable in a log,
// and this one is deliberately the reading the gate was built to replace —
// loose strings.Contains included. A failed turn delivers nothing here, which
// is the long-standing behaviour: the scheduler owns a failed run (it records
// the status and retries), and telling the user about it is NOT this fallback's
// job. Under a working gate the same run may be delivered, which is the gate
// adding a layer rather than the fallback changing.
func markerDecision(job *cron.Job, result string, runErr error) notifyDecision {
	if runErr != nil {
		return notifyDecision{Decider: "marker"}
	}
	if job.ShouldSuppressResult(result) {
		return notifyDecision{Decider: "marker"}
	}
	return notifyDecision{Deliver: true, Content: result, Decider: "marker"}
}

// ask asks the evaluation endpoint and returns its verdict, or nil when there
// is no verdict to have — an error, a timeout, or a question the service left
// unanswered. Nil is what sends the caller to the fallback; it is not a "no".
func (g *notifyGate) ask(ctx context.Context, job *cron.Job, result, sessionID string, runErr error) *notifyDecision {
	run := notifygate.Run{
		Job:      job.Name,
		Schedule: job.Schedule,
		// The job's own words are the criterion — the only place a job can say
		// when it wants to be told, and the reason one question can serve jobs
		// that want opposite things.
		Prompt:  job.Prompt,
		Outcome: notifygate.OutcomeOK,
		Report:  result,
	}
	if runErr != nil {
		run.Outcome, run.Err = notifygate.OutcomeFailed, runErr.Error()
	}

	start := time.Now()
	resp, err := g.judge.Evaluate(ctx, notifygate.PromptState(run),
		map[string]jev.Question{notifygate.QuestionID: notifygate.Question()})
	if err != nil {
		g.degraded.Store(true)
		g.logger.Warn(ctx, "channel: cron notify judgment failed, falling back to the marker",
			"job", job.ID, "err", err)
		return nil
	}
	g.recordUsage(ctx, resp, sessionID)

	p, ok := answerProbability(resp)
	if !ok {
		g.degraded.Store(true)
		g.logger.Warn(ctx, "channel: cron notify judgment carried no answer, falling back to the marker",
			"job", job.ID, "model", resp.Model, "questions", len(resp.Answers))
		return nil
	}
	// A judgment that answered is a service that is working, so the prompt goes
	// back to asking the agent to report rather than to judge.
	g.degraded.Store(false)
	g.logAnswers(ctx, job, resp, p, time.Since(start))

	if p < g.threshold {
		return &notifyDecision{Decider: "jev"}
	}
	content := stripLeadingMarker(result)
	if strings.TrimSpace(content) == "" {
		// The gate decided to speak and the turn wrote nothing: on a failed run
		// that is the failure itself, which the state carried and the judgment
		// read. The notice is built here because nothing else can say it.
		content = failureNotice(job, runErr)
	}
	return &notifyDecision{Deliver: true, Content: content, Decider: "jev"}
}

// failureNotice is what the user gets for a run that could not finish and
// produced no report of its own.
func failureNotice(job *cron.Job, runErr error) string {
	reason := "任务没能完成"
	if runErr != nil {
		reason = runErr.Error()
	}
	return fmt.Sprintf("⚠️ 定时任务「%s」执行失败：%s", job.Name, reason)
}

// answerProbability reads the asked question's probability that the run should
// be delivered. A missing answer, or one without the probability a noul carries,
// is NOT a zero: it was not judged, and that difference is what decides whether
// the marker fallback runs (see ask).
func answerProbability(resp *jev.Response) (float64, bool) {
	if resp == nil {
		return 0, false
	}
	answer, ok := resp.Answers[notifygate.QuestionID]
	if !ok {
		return 0, false
	}
	return answer.ProbabilityOf(notifygate.ActionLabel)
}

// logAnswers writes what the judgment answered, one Info line per call: the
// metadata first and the score last, so a suppressed notification can be read
// back as "0.31 against a 0.45 threshold by jev-1.13.0" instead of leaving no
// trace at all — which is the state the marker protocol left behind.
func (g *notifyGate) logAnswers(ctx context.Context, job *cron.Job, resp *jev.Response, p float64, elapsed time.Duration) {
	model := ""
	var input, output int
	if resp != nil {
		model, input, output = resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens
	}
	g.logger.Info(ctx, "channel: cron notify judged",
		"job", job.ID, "model", model, "threshold", g.threshold,
		"input_tokens", input, "output_tokens", output,
		"elapsed", elapsed.Round(time.Millisecond).String(),
		"answers", fmt.Sprintf("%s=%.3f", notifygate.ActionLabel, p))
}

// recordUsage writes the ledger row for one judgment call through the shared
// writer; failure only warns — a gate must never break a cron run because the
// ledger is unwritable.
func (g *notifyGate) recordUsage(ctx context.Context, resp *jev.Response, sessionID string) {
	if resp == nil {
		return
	}
	if err := llm.RecordJevUsage(g.usage, g.cfg.Jev, g.judge.Model(), sessionID,
		resp.Usage.InputTokens, resp.Usage.OutputTokens, time.Now()); err != nil {
		g.logger.Warn(ctx, "channel: cron notify usage not recorded", "err", err)
	}
}

// isMarkerOnly reports whether a report IS the marker and nothing else — the
// tight reading the gate's fast path uses, deliberately not the loose
// strings.Contains cron.Job.ShouldSuppressResult applies: a report that merely
// mentions the marker, or leads with it and then says something, is content the
// user may need, and it goes to the judgment like any other. Only a reply with
// nothing else in it is silence.
func isMarkerOnly(result string) bool {
	rest := stripLeadingMarker(result)
	return rest == "" || strings.Trim(rest, " \t\r\n.!！。…—") == ""
}

// stripLeadingMarker removes the silence marker from the front of a report,
// case-insensitively, with the whitespace that follows it. The instruction is
// still in the prompt, so a model that leads with the marker while also saying
// something would otherwise publish it verbatim.
func stripLeadingMarker(result string) string {
	trimmed := strings.TrimSpace(result)
	if len(trimmed) < len(cron.SilentMarker) || !strings.EqualFold(trimmed[:len(cron.SilentMarker)], cron.SilentMarker) {
		return trimmed
	}
	return strings.TrimSpace(trimmed[len(cron.SilentMarker):])
}
