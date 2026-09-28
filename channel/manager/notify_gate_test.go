package manager

// The cron notify gate's decision rules. What is pinned here is the ORDER and the
// direction of every branch, because each one is a design choice with an
// observable cost:
//
//   - a when_relevant run is judged, an always-run never is;
//   - the fast path (nothing reported, or the marker alone) suppresses without a
//     call, and is deliberately TIGHTER than the fallback's strings.Contains;
//   - no judgment (error, timeout, unanswered) falls back to the marker rather
//     than to "deliver" — the opposite direction from the MCP tool-search gate,
//     because the conservative side of THIS gate is "do not swallow what the user
//     should have seen";
//   - a delivered report has a stray leading marker stripped, so the instruction
//     that is still in the prompt cannot publish itself.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/channel/manager/notifygate"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/jev"
)

// fakeJudge answers one noul question with a fixed probability, or fails, or
// leaves the question unanswered — the three shapes the gate has to tell apart.
type fakeJudge struct {
	p        float64
	err      error
	noAnswer bool

	calls     int
	lastState string
	lastQs    map[string]jev.Question
}

func (f *fakeJudge) Evaluate(_ context.Context, state any, questions map[string]jev.Question) (*jev.Response, error) {
	f.calls++
	f.lastQs = questions
	if s, ok := state.(string); ok {
		f.lastState = s
	}
	if f.err != nil {
		return nil, f.err
	}
	p := f.p
	resp := &jev.Response{
		Model:   "fake-model-1.0",
		Usage:   jev.Usage{InputTokens: 321, OutputTokens: 7},
		Answers: map[string]jev.Answer{},
	}
	if !f.noAnswer {
		resp.Answers[notifygate.QuestionID] = jev.Answer{Type: jev.QuestionNoul, Noul: &p}
	}
	return resp, nil
}

func (f *fakeJudge) Model() string { return "fake-model-1.0" }

func testJob(notify cron.NotifyPolicy) *cron.Job {
	return &cron.Job{
		ID:       "cr_test",
		Name:     "检查磁盘用量",
		Schedule: "0 9 * * *",
		Prompt:   "检查磁盘用量，快满了就告诉我。",
		Notify:   notify,
	}
}

// gateWith wires a Manager to a fake judgment at the configured threshold. A nil
// judge leaves the gate off, which is what an installation without a usable jev
// block looks like.
func gateWith(t *testing.T, judge jev.Judge, threshold float64) *Manager {
	t.Helper()
	m := mustNewManager(t, config.DefaultConfig())
	if judge != nil {
		m.notifyGate = &notifyGate{
			judge:     judge,
			threshold: threshold,
			cfg:       m.cfg,
			usage:     llm.NewUsageRecorder(t.TempDir()),
			logger:    m.logger,
		}
	}
	return m
}

// The appended policy has to match the decider, or the two fight: an agent told
// to judge worth pre-filters everything the gate would have judged.
func TestBuildCronPromptFollowsTheDecider(t *testing.T) {
	job := testJob(cron.NotifyWhenRelevant)

	withGate := gateWith(t, &fakeJudge{p: 0.9}, 0.45)
	gatePrompt := withGate.buildCronPrompt(job)
	assert.Contains(t, gatePrompt, gateNotificationPolicy)
	assert.NotContains(t, gatePrompt, markerNotificationPolicy)
	assert.Contains(t, gatePrompt, job.Prompt, "the job's own words come first and stay intact")

	noGate := gateWith(t, nil, 0.45)
	markerPrompt := noGate.buildCronPrompt(job)
	assert.Contains(t, markerPrompt, markerNotificationPolicy)
	assert.NotContains(t, markerPrompt, gateNotificationPolicy)
}

// A judgment that stopped answering hands the agent its own judgment back: the
// fallback is the marker, and a marker is only readable if the prompt still asks
// for one.
func TestBuildCronPromptRevertsWhenTheGateStopsAnswering(t *testing.T) {
	m := gateWith(t, &fakeJudge{err: errors.New("jev: HTTP 529: overloaded")}, 0.45)
	job := testJob(cron.NotifyWhenRelevant)

	assert.Contains(t, m.buildCronPrompt(job), gateNotificationPolicy)

	m.judgeNotify(t.Context(), job, "磁盘 94%", "sess-1", nil)

	assert.Contains(t, m.buildCronPrompt(job), markerNotificationPolicy,
		"a failed judgment means the next run's agent decides again")
}

// ...and a working judgment takes it away again, so one hiccup costs one run's
// worth of judgment rather than the rest of the process's.
func TestBuildCronPromptRecoversAfterAGoodJudgment(t *testing.T) {
	judge := &fakeJudge{err: errors.New("boom")}
	m := gateWith(t, judge, 0.45)
	job := testJob(cron.NotifyWhenRelevant)

	m.judgeNotify(t.Context(), job, "磁盘 94%", "sess-1", nil)
	require.Contains(t, m.buildCronPrompt(job), markerNotificationPolicy)

	judge.err = nil
	m.judgeNotify(t.Context(), job, "磁盘 94%", "sess-1", nil)

	assert.Contains(t, m.buildCronPrompt(job), gateNotificationPolicy)
}

// A job that is not when_relevant gets no policy at all, whichever decider is
// running: notify=always means there is nothing to decide.
func TestBuildCronPromptLeavesOtherPoliciesAlone(t *testing.T) {
	for name, m := range map[string]*Manager{
		"with gate":    gateWith(t, &fakeJudge{p: 0.9}, 0.45),
		"without gate": gateWith(t, nil, 0.45),
	} {
		t.Run(name, func(t *testing.T) {
			job := testJob(cron.NotifyAlways)
			assert.Equal(t, job.Prompt, m.buildCronPrompt(job))
		})
	}
}

func TestJudgeNotifyDeliversOnHighProbability(t *testing.T) {
	judge := &fakeJudge{p: 0.80}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "磁盘已用 94%", "sess-1", nil)

	assert.True(t, d.Deliver)
	assert.Equal(t, "磁盘已用 94%", d.Content)
	assert.Equal(t, "jev", d.Decider)
	assert.Equal(t, 1, judge.calls)
}

func TestJudgeNotifySuppressesOnLowProbability(t *testing.T) {
	judge := &fakeJudge{p: 0.31}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "磁盘已用 41%，和上次一样", "sess-1", nil)

	assert.False(t, d.Deliver)
	assert.Empty(t, d.Content)
	assert.Equal(t, "jev", d.Decider)
	assert.Equal(t, 1, judge.calls)
}

// The threshold is inclusive: a gate that suppressed p == threshold would make
// the swept number mean "strictly greater than", which no sweep reports.
func TestJudgeNotifyThresholdIsInclusive(t *testing.T) {
	judge := &fakeJudge{p: 0.45}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "有变化", "sess-1", nil)

	assert.True(t, d.Deliver, "p == threshold must deliver")
}

// notify=always is the job's own policy and never reaches the judgment: a job
// that asked for every result must not start being filtered because a jev block
// exists.
func TestJudgeNotifyNeverJudgesAnAlwaysJob(t *testing.T) {
	judge := &fakeJudge{p: 0.01}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyAlways), "一切正常", "sess-1", nil)

	assert.True(t, d.Deliver)
	assert.Equal(t, "一切正常", d.Content)
	assert.Equal(t, "always", d.Decider)
	assert.Zero(t, judge.calls, "an always-job must not spend a judgment call")
}

// An always-job keeps its long-standing silence when the turn itself failed,
// which is why the gate reports that decision separately ("always", not "marker").
func TestJudgeNotifyAlwaysJobStaysSilentOnAFailedTurn(t *testing.T) {
	judge := &fakeJudge{p: 0.99}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyAlways), "半截报告", "sess-1", errors.New("执行超时"))

	assert.False(t, d.Deliver)
	assert.Zero(t, judge.calls)
}

func TestJudgeNotifyEmptyReportSuppressesWithoutACall(t *testing.T) {
	for name, result := range map[string]string{"empty": "", "whitespace": "  \n "} {
		t.Run(name, func(t *testing.T) {
			judge := &fakeJudge{p: 0.99}
			m := gateWith(t, judge, 0.45)

			d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), result, "sess-1", nil)

			assert.False(t, d.Deliver)
			assert.Equal(t, "none", d.Decider)
			assert.Zero(t, judge.calls, "there is nothing to judge, so nothing is spent")
		})
	}
}

// The marker alone — the agent saying "nothing to report" and nothing else — is
// the one reading the marker was ever right about, so the fast path takes it
// without paying for a judgment.
func TestJudgeNotifyMarkerAloneSuppressesWithoutACall(t *testing.T) {
	for name, result := range map[string]string{
		"plain":            "[SILENT]",
		"padded":           "  [SILENT] \n",
		"lowercase":        "[silent]",
		"punctuation only": "[SILENT]。",
	} {
		t.Run(name, func(t *testing.T) {
			judge := &fakeJudge{p: 0.99}
			m := gateWith(t, judge, 0.45)

			d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), result, "sess-1", nil)

			assert.False(t, d.Deliver)
			assert.Equal(t, "marker", d.Decider)
			assert.Zero(t, judge.calls)
		})
	}
}

// The fast path is a TIGHT reading, unlike the fallback's strings.Contains: a
// report that merely mentions the marker is content, and content goes to the
// judgment.
func TestJudgeNotifyMarkerInsideAReportStillGoesToTheJudge(t *testing.T) {
	judge := &fakeJudge{p: 0.80}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "本来想回 [SILENT]，但磁盘已经 94% 了", "sess-1", nil)

	assert.True(t, d.Deliver)
	assert.Equal(t, "jev", d.Decider)
	assert.Equal(t, 1, judge.calls, "a report that mentions the marker is still content")
}

// A report that leads with the marker and then says something is delivered
// WITHOUT the marker: the instruction is still in the prompt, so the marker must
// not be able to publish itself.
func TestJudgeNotifyStripsALeadingMarkerBeforeDelivery(t *testing.T) {
	judge := &fakeJudge{p: 0.80}
	m := gateWith(t, judge, 0.45)

	for name, tc := range map[string]struct{ result, want string }{
		"same line": {"[SILENT] 磁盘 94% 了，建议清理", "磁盘 94% 了，建议清理"},
		"newline":   {"[SILENT]\n磁盘 94% 了", "磁盘 94% 了"},
		"lowercase": {"[silent] 磁盘 94% 了", "磁盘 94% 了"},
	} {
		t.Run(name, func(t *testing.T) {
			d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), tc.result, "sess-1", nil)
			require.True(t, d.Deliver)
			assert.Equal(t, tc.want, d.Content)
			assert.NotContains(t, d.Content, cron.SilentMarker)
		})
	}
}

// No judgment (an error from the service) falls back to the marker — NOT to
// "deliver". The two directions matter: the tool-search gate reads a missing
// answer as "do not offer" because a cluttered tool list is the cost there,
// while here the cost is swallowing something the user asked to hear about.
func TestJudgeNotifyFallsBackToTheMarkerWhenTheServiceFails(t *testing.T) {
	judge := &fakeJudge{err: errors.New("jev: HTTP 529: overloaded")}
	m := gateWith(t, judge, 0.45)

	delivered := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "磁盘 94%", "sess-1", nil)
	assert.True(t, delivered.Deliver, "an unreachable service must not silence a real report")
	assert.Equal(t, "marker", delivered.Decider)

	// And the fallback is the OLD reading, loose containment included.
	suppressed := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "[SILENT] 其实磁盘 94% 了", "sess-1", nil)
	assert.False(t, suppressed.Deliver, "the fallback keeps ShouldSuppressResult as it was")
}

func TestJudgeNotifyFallsBackToTheMarkerWhenTheQuestionIsUnanswered(t *testing.T) {
	judge := &fakeJudge{noAnswer: true}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "磁盘 94%", "sess-1", nil)

	assert.True(t, d.Deliver)
	assert.Equal(t, "marker", d.Decider)
}

// A gate that was never built (matcher=marker, or no usable jev block) leaves
// behaviour exactly as it was before the gate existed.
func TestJudgeNotifyWithoutAGateIsTheOldBehaviour(t *testing.T) {
	m := gateWith(t, nil, 0.45)

	job := testJob(cron.NotifyWhenRelevant)
	kept := m.judgeNotify(t.Context(), job, "磁盘 94%", "sess-1", nil)
	assert.True(t, kept.Deliver)
	assert.Equal(t, "marker", kept.Decider)

	dropped := m.judgeNotify(t.Context(), job, "[SILENT]", "sess-1", nil)
	assert.False(t, dropped.Deliver)
}

// A failed turn whose report was empty has nothing of its own to send, so the
// notice is built here — the judgment read the failure off the state and asked
// for it to be delivered.
func TestJudgeNotifyTellsTheUserAboutAFailedRunWithNoReport(t *testing.T) {
	judge := &fakeJudge{p: 0.90}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "", "sess-1", errors.New("执行超时 (5m0s)"))

	require.True(t, d.Deliver, "a run that could not finish is not silence")
	assert.Contains(t, d.Content, "检查磁盘用量")
	assert.Contains(t, d.Content, "执行超时")
	assert.Equal(t, "jev", d.Decider)
}

// The documented asymmetry of the fallback: with the gate unavailable a failed
// turn delivers nothing, which is the behaviour before the gate existed (the
// scheduler records the failure and retries).
func TestJudgeNotifyFallbackStaysSilentOnAFailedTurn(t *testing.T) {
	judge := &fakeJudge{err: errors.New("boom")}
	m := gateWith(t, judge, 0.45)

	d := m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "半截报告", "sess-1", errors.New("执行超时"))

	assert.False(t, d.Deliver)
	assert.Equal(t, "marker", d.Decider)
}

// The judgment is asked about the production question, over a state built from
// the job record and the turn's own output — the criterion block is the whole
// reason one question can serve jobs that want opposite things.
func TestJudgeNotifySendsTheProductionQuestionAndState(t *testing.T) {
	judge := &fakeJudge{p: 0.80}
	m := gateWith(t, judge, 0.45)
	job := testJob(cron.NotifyWhenRelevant)

	m.judgeNotify(t.Context(), job, "磁盘 94%", "sess-1", nil)

	require.Contains(t, judge.lastQs, notifygate.QuestionID)
	assert.Equal(t, notifygate.Question().Instructions, judge.lastQs[notifygate.QuestionID].Instructions,
		"the question asked has to be the question the case set measured")

	assert.Contains(t, judge.lastState, "任务原话: "+job.Prompt, "the criterion comes from the job's own words")
	assert.Contains(t, judge.lastState, "本次运行: "+notifygate.OutcomeOK)
	assert.Contains(t, judge.lastState, "报告:\n磁盘 94%")
}

func TestJudgeNotifyStateCarriesAFailedRun(t *testing.T) {
	judge := &fakeJudge{p: 0.80}
	m := gateWith(t, judge, 0.45)

	m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "半截报告", "sess-1", errors.New("执行超时 (5m0s)"))

	assert.Contains(t, judge.lastState, notifygate.OutcomeFailed)
	assert.Contains(t, judge.lastState, "执行超时 (5m0s)", "the failure text is what the judgment reads")
}

// Every judgment is a ledger row: a gate that priced nothing would make "what
// does the notify gate cost" unanswerable from /usage.
func TestJudgeNotifyRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	judge := &fakeJudge{p: 0.80}
	m := gateWith(t, judge, 0.45)
	m.notifyGate.usage = llm.NewUsageRecorder(dir)

	m.judgeNotify(t.Context(), testJob(cron.NotifyWhenRelevant), "磁盘 94%", "sess-42", nil)

	rows := notifyLedgerRows(t, dir)
	require.Len(t, rows, 1)
	assert.Equal(t, llm.UsageKindJev, rows[0].Kind)
	assert.Equal(t, llm.JevProviderName, rows[0].Provider)
	assert.Equal(t, "fake-model-1.0", rows[0].Model)
	assert.Equal(t, "sess-42", rows[0].SessionID)
	assert.Equal(t, int64(321), rows[0].InputTokens)
	require.NotNil(t, rows[0].Credit, "a Jev row always carries a credit snapshot, 0 included")
}

// notifyLedgerRows reads the rows a fresh recorder wrote today.
func notifyLedgerRows(t *testing.T, dir string) []llm.UsageRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl"))
	require.NoError(t, err)
	var rows []llm.UsageRow
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var row llm.UsageRow
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		rows = append(rows, row)
	}
	return rows
}
