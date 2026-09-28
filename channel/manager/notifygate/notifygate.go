// Package notifygate is the Jev gate a cron job's notify=when_relevant policy
// is being moved onto: instead of the agent typing a silence marker into its
// own reply and code string-matching it, an evaluation endpoint is asked
// whether the finished run is worth interrupting the user for.
//
// This package owns the question and the state; channel/manager's notify_gate.go
// owns the decision — when the gate runs, what its failure means, and how the
// verdict reaches the channel. The division is the one docs/agents/jev.md asks
// for: the question and the state live beside the measurement that produced
// them (a case file has to be generated from the production text), because
// editing the sentence invalidates a swept threshold.
//
// The state shape is the CANDIDATE that measurement picked: the job's own words
// ride in the state (任务原话), so the judgment is "does this result satisfy
// THIS job's stated condition" rather than one generic reading of worth. It beat
// a control that carried no criterion on the case set now in
// pkg/jev/testdata/notify-gate-prompt.json; docs/agents/jev.md records what the
// sweep showed and what it does not settle.
//
// Why the marker is the thing being replaced (docs/agents/jev.md, "When to use
// it"): a marker protocol is a generative call wearing a judgment's clothes.
// The agent's [SILENT] is string-matched by cron.Job.ShouldSuppressResult, and
// that has three problems the gate answers — the marker rides in the text that
// gets PUBLISHED, so untrusted content can talk the agent into it; it is
// matched with strings.Contains, so a reply that merely mentions the marker is
// swallowed; and the decision leaves no trace beyond one log line, so a wrong
// suppression cannot be reviewed or tuned.
//
// The decision rules themselves (all four, because the failure direction
// differs per case and the conservative side of THIS gate is "do not swallow
// what the user should have seen"):
//
//	evaluation p >= threshold, report non-empty  → deliver (with a stray
//	                                               marker stripped)
//	evaluation p <  threshold                    → suppress
//	no judgment (error, timeout, unanswered)     → fall back to
//	                                               ShouldSuppressResult, i.e.
//	                                               the marker, which is why it
//	                                               stays in the prompt
//	report empty or the marker alone             → suppress, no call
//
// So "the service did not answer" reads as the marker path here, NOT as
// "deliver" — the opposite of the MCP tool-search gate, where an unanswered
// question means "do not offer". Same missing data, opposite conservative
// side: the tool list is cluttered by a wrong yes, the user's attention is
// spent by a wrong no.
//
// Keeping a marker in the prompt is a decision with a cost worth stating, and
// the gate pays only half of it: the instruction it appends asks the agent to
// REPORT and leaves the judgment here, so the agent no longer drops a finding it
// judged unworthy before this gate sees it (channel/manager's buildCronPrompt
// owns that wording, and hands the old instruction back the moment a judgment
// stops answering). What remains is that the fallback can only read a report the
// agent still chose to write. What the gate buys is an observable, tunable
// judgment (a probability, a threshold, a model version, one log line per call)
// — not a smaller chance of missing something.
package notifygate

import (
	"strings"

	"github.com/monsterxx03/tachi/pkg/jev"
)

// The two outcomes a cron firing can have, as they are written into the state.
// A failed run is usually worth reporting on its own — the gate should be able
// to say so without reading the report.
const (
	OutcomeOK     = "成功"
	OutcomeFailed = "失败"
)

// ActionLabel is the noul pole that means "deliver": the label whose probability
// the gate thresholds on, and the one the generated case files name as
// action_label. Whoever calls Question() does not choose it — a question with a
// different pole is a different calibration.
const ActionLabel = "true"

// QuestionID is the key one judgment is asked under. It is for the caller only
// (the id never reaches the model), but the gate and the case files have to
// agree on it like everything else about the question.
const QuestionID = "deliver"

// Run is one cron firing as the gate sees it: the fields code owns, plus the
// turn's own output.
type Run struct {
	// Job is the job's name (cron.Job.Name), and Schedule its cron expression
	// (cron.Job.Schedule). Both come from the job record, so they cost nothing
	// and are the same on every run of that job.
	Job      string
	Schedule string

	// Outcome is OutcomeOK, or OutcomeFailed with Err carrying why. It is what
	// code knows about the turn's success — the agent's own account of it is
	// the report.
	Outcome string
	Err     string

	// Report is the turn's final assistant text: the free-form half, and the
	// half this measurement is about. A job whose prompt pins a report shape
	// writes Fields instead.
	Report string

	// Fields is the same run reported in the pinned shape.
	Fields Fields

	// Prompt is the job's own words — cron.Job.Prompt, the text that created
	// this task — and the ONLY place a job can say when it wants to be told.
	// PromptState carries it, and it is what distinguishes the candidate design
	// from the control: one shared question cannot know that one job wants a
	// summary whenever the market is open while another only wants to hear that
	// something broke.
	Prompt string
}

// Fields is a report whose shape the job's prompt pins, so the register that
// reaches the gate is the same on every run of every job — the reason the
// template shape exists at all.
//
// Both fields are the OBSERVATION, never the decision: what changed, and what
// it means. Whether that is worth interrupting the user for is the gate's
// question, which is why a job on this shape no longer needs the marker
// instruction (and why keeping one would let the agent drop a finding before
// the gate reads it).
type Fields struct {
	// Change is what differs from the last check — "无变化" when nothing does.
	Change string
	// Impact is what that means for the reader.
	Impact string
}

// Question is the judgment the gate asks. Wording is part of the calibration and
// not a style choice: p(action) is a probability over the yes/no this text
// draws, so editing the sentence — or translating it — invalidates a swept
// threshold. The deployment's jobs run in Chinese (config's language), so the
// question and the state's labels are Chinese; a deployment in another language
// needs its own sweep, not this one translated.
//
// It reads the criterion out of the STATE (PromptState's 任务原话 block) rather
// than stating one, which is the point: "is this worth interrupting" is not one
// question across jobs — one job wants a summary whenever the market is open,
// another only wants to hear that something broke, and only the job can say
// which. Two consequences worth knowing before editing it:
//
//   - A job whose prompt states no condition gets no criterion, and the model
//     infers one from the task description. That is the graceful half of the
//     same design, not a bug.
//   - The failure rule below is OUR policy, not the job's: nobody writes
//     "and tell me if you could not fetch it" into a cron prompt, so
//     "could not get the data" is named in the yes pole for every job at once.
//     Drop it and a job that quietly stops working looks exactly like a job
//     with nothing to report.
//
// The bar is the JOB's, and this question never raises it: a job that says
// "开市了就总结" must be told about a flat market, and a job that says "有异常
// 就告诉我" must be told about one that recovered. A job whose bar is too low is
// a job prompt to tighten, which is the caller's business — a gate that
// second-guessed it would suppress exactly the results whose job asked for them.
func Question() jev.Question {
	return jev.NewNoul(
		"一个定时任务刚刚跑完。任务原话是用户对这件事的完整要求，里面写了他希望什么时候被告知（也可能只写了要做什么事）。这一次的运行结果，是否需要现在告诉用户？",
		jev.NoulCriteria{
			Yes: "按任务原话的要求，现在就该通知：拿到了新内容、出现了它要盯着的变化或问题；或者任务没能完成（没取到数据、抓取失败、运行报错）—— 后者同样要通知",
			No:  "按任务原话的要求应当保持沉默：它明确说了这种情况不用回复，或者在运行正常的前提下，结果里没有它要盯着的东西",
		},
	)
}

// GenericQuestion is the CONTROL arm's question: the same judgment asked by a
// judge that cannot see the job's own words — i.e. what the gate would have to
// ask if the state could not carry a per-job criterion (the flat and template
// shapes). It exists so a sweep can price the criterion block: if the two
// questions answer a case file the same way, carrying the job's prompt is
// complexity nobody needed.
func GenericQuestion() jev.Question {
	return jev.NewNoul(
		"一个定时任务刚刚跑完。它的结果里有没有用户现在就需要知道的东西 —— 出问题了、发生了有意义的变化、或者是他要求盯着的那件事有了结果？例行完成、读数没变、什么都没发生的运行不算。",
		jev.NoulCriteria{
			Yes: "有可行动的问题、有意义的变化，或用户要求盯着的事情有了结果，现在就需要知道",
			No:  "例行完成、读数未变或什么都没发生，对用户不携带新的信息",
		},
	)
}

// PromptState renders a run the way the candidate design hands it to the gate:
// the job's own words, then the result as the turn wrote it.
//
// Two things differ from FlatState at once, and the comparison is honest about
// it: the state gains the 任务原话 block AND the question becomes Question()
// instead of GenericQuestion(). A criterion in the state that the question does
// not tell the model to read is dead weight, so those two cannot be varied
// separately — flat (with GenericQuestion) is the control for the whole design,
// not for the criterion block alone.
func PromptState(run Run) string {
	var b strings.Builder
	writeField(&b, "任务", run.Job)
	writeField(&b, "计划", run.Schedule)
	writeField(&b, "任务原话", run.Prompt)
	writeOutcome(&b, run)
	b.WriteString("报告:")
	if report := strings.TrimSpace(run.Report); report != "" {
		b.WriteByte('\n')
		b.WriteString(report)
	}
	return strings.TrimRight(b.String(), " \n")
}

// FlatState renders a run whose report is the free-form text it is — the state
// shape cron produces today, and the one that keeps the job prompts unchanged.
//
// It is the control arm of the measurement, and docs/agents/jev.md's register
// trap predicts it will not be sweepable: tone moves p(action) further than
// content does (pkg/jev/testdata/notify-phrasing.json is the 2x2 that recorded
// an EMPTY corridor), and one threshold has to serve every job's prose style.
func FlatState(run Run) string {
	var b strings.Builder
	writeHeader(&b, run)
	b.WriteString("报告:")
	if report := strings.TrimSpace(run.Report); report != "" {
		b.WriteByte('\n')
		b.WriteString(report)
	}
	return strings.TrimRight(b.String(), " \n")
}

// TemplateState renders a run whose report arrives in the pinned field shape.
//
// The header is byte-identical to FlatState's, so a sweep of the two files
// differs in exactly one dimension: the last block — a paragraph of prose, or
// two labelled observations.
func TemplateState(run Run) string {
	var b strings.Builder
	writeHeader(&b, run)
	writeField(&b, "变化", run.Fields.Change)
	writeField(&b, "影响", run.Fields.Impact)
	return strings.TrimRight(b.String(), " \n")
}

// writeHeader writes the fields code owns. The failure text rides on the
// outcome line, so a run that failed is legible before the report is read at
// all — and an empty Err leaves the line short rather than saying "失败 — ".
func writeHeader(b *strings.Builder, run Run) {
	writeField(b, "任务", run.Job)
	writeField(b, "计划", run.Schedule)
	writeOutcome(b, run)
}

// writeOutcome writes the one line code knows about how the turn went.
func writeOutcome(b *strings.Builder, run Run) {
	detail := oneLine(run.Outcome)
	if err := oneLine(run.Err); err != "" {
		detail += " — " + err
	}
	writeField(b, "本次运行", detail)
}

// writeField writes one "label: value" line, with no trailing space when the
// value is empty: the state is read against a case file by eye, and "变化: "
// with a stray space is a difference a diff shows.
func writeField(b *strings.Builder, label, value string) {
	b.WriteString(label)
	b.WriteByte(':')
	if v := oneLine(value); v != "" {
		b.WriteByte(' ')
		b.WriteString(v)
	}
	b.WriteByte('\n')
}

// oneLine collapses a value onto one line. A field that arrived with newlines
// (a report pasted into it, a command's stack trace) would otherwise break the
// line-per-field shape the template exists to hold; Chinese text has no spaces
// to lose.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
