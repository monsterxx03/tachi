package notifygate

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/pkg/jev"
)

func okRun() Run {
	return Run{
		Job:      "检查磁盘用量",
		Schedule: "0 9 * * *",
		Outcome:  OutcomeOK,
		Report:   "/data 已用 94%，剩余 12GB，上次是 71%。建议清理日志。",
		Fields: Fields{
			Change: "/data 从 71% 升到 94%，剩余 12GB",
			Impact: "写入可能失败",
		},
	}
}

func TestFlatState(t *testing.T) {
	want := "任务: 检查磁盘用量\n" +
		"计划: 0 9 * * *\n" +
		"本次运行: 成功\n" +
		"报告:\n" +
		"/data 已用 94%，剩余 12GB，上次是 71%。建议清理日志。"
	if got := FlatState(okRun()); got != want {
		t.Errorf("FlatState:\n got %q\nwant %q", got, want)
	}
}

func TestTemplateState(t *testing.T) {
	want := "任务: 检查磁盘用量\n" +
		"计划: 0 9 * * *\n" +
		"本次运行: 成功\n" +
		"变化: /data 从 71% 升到 94%，剩余 12GB\n" +
		"影响: 写入可能失败"
	if got := TemplateState(okRun()); got != want {
		t.Errorf("TemplateState:\n got %q\nwant %q", got, want)
	}
}

// TestStatesDifferOnlyInTheReportBlock is the property the two sweeps rest on:
// the shapes may differ in the last block and nowhere else, or a difference
// between their thresholds could be the header's doing instead of the report's.
func TestStatesDifferOnlyInTheReportBlock(t *testing.T) {
	run := okRun()
	header := "任务: 检查磁盘用量\n计划: 0 9 * * *\n本次运行: 成功\n"

	flat := FlatState(run)
	templated := TemplateState(run)
	for name, state := range map[string]string{"flat": flat, "template": templated} {
		if !strings.HasPrefix(state, header) {
			t.Errorf("%s state does not start with the shared header:\n%q", name, state)
		}
		if strings.Contains(state, header+"\n") {
			t.Errorf("%s state has a blank line after the header: %q", name, state)
		}
	}
	if flat == templated {
		t.Fatal("the two shapes rendered identically — the measurement would compare nothing")
	}
}

func TestHeaderCarriesTheFailure(t *testing.T) {
	run := okRun()
	run.Outcome = OutcomeFailed
	run.Err = "执行超时 (5m0s)"
	want := "本次运行: 失败 — 执行超时 (5m0s)"
	for name, state := range map[string]string{"flat": FlatState(run), "template": TemplateState(run)} {
		if !strings.Contains(state, want) {
			t.Errorf("%s state lost the failure line %q:\n%s", name, want, state)
		}
	}
}

// A field that arrives multi-line (a pasted stack trace, a report the model
// broke across lines) must not break the one-field-per-line shape: a state
// whose fields have moved is not the template the case file measured.
func TestMultiLineValuesCollapseToOneLine(t *testing.T) {
	run := okRun()
	run.Fields.Change = "构建失败，错误如下：\npanic: nil map\n  at main.go:42"
	state := TemplateState(run)
	if strings.Count(state, "\n") != 4 {
		t.Errorf("state should have exactly the header's three lines plus two fields:\n%q", state)
	}
	if !strings.Contains(state, "变化: 构建失败，错误如下： panic: nil map at main.go:42") {
		t.Errorf("the multi-line field was not collapsed:\n%q", state)
	}
}

func TestEmptyReportLeavesTheShapeIntact(t *testing.T) {
	run := Run{Job: "检查 CI", Schedule: "@hourly", Outcome: OutcomeOK}
	if got, want := FlatState(run), "任务: 检查 CI\n计划: @hourly\n本次运行: 成功\n报告:"; got != want {
		t.Errorf("FlatState with an empty report:\n got %q\nwant %q", got, want)
	}
	// The template shape still names both fields even when the turn filled
	// neither: an empty value is "the model did not say", which is a different
	// thing from a field that is absent.
	if got, want := TemplateState(run), "任务: 检查 CI\n计划: @hourly\n本次运行: 成功\n变化:\n影响:"; got != want {
		t.Errorf("TemplateState with empty fields:\n got %q\nwant %q", got, want)
	}
}

func TestPromptState(t *testing.T) {
	run := okRun()
	run.Prompt = "检查磁盘用量，快满了就告诉我。"
	want := "任务: 检查磁盘用量\n" +
		"计划: 0 9 * * *\n" +
		"任务原话: 检查磁盘用量，快满了就告诉我。\n" +
		"本次运行: 成功\n" +
		"报告:\n" +
		"/data 已用 94%，剩余 12GB，上次是 71%。建议清理日志。"
	if got := PromptState(run); got != want {
		t.Errorf("PromptState:\n got %q\nwant %q", got, want)
	}
}

// TestPromptStateAddsOnlyTheCriterion is what makes the candidate and the flat
// control comparable: the criterion block plus the question are the whole
// difference, so a sweep of the two is not comparing report treatment as well.
func TestPromptStateAddsOnlyTheCriterion(t *testing.T) {
	run := okRun()
	run.Prompt = "检查磁盘用量，快满了就告诉我。"

	flat := FlatState(run)
	criterion := "任务原话: 检查磁盘用量，快满了就告诉我。\n"
	prompt := PromptState(run)
	// It has to be a REMOVAL of that one line, not a Comparison that ignores
	// order: a block moved between fields would pass the latter.
	without := strings.Replace(prompt, criterion, "", 1)
	if without == prompt {
		t.Fatal("the criterion line is not in the candidate state at all")
	}
	if without != flat {
		t.Errorf("stripping the criterion did not leave the flat state:\n got %q\nwant %q", without, flat)
	}
}

func TestQuestionIsANoulWithBothPolesDescribed(t *testing.T) {
	for name, q := range map[string]jev.Question{"candidate": Question(), "control": GenericQuestion()} {
		if err := q.Validate(); err != nil {
			t.Fatalf("%s question does not validate: %v", name, err)
		}
		criteria, ok := q.Criteria.(jev.NoulCriteria)
		if !ok {
			t.Fatalf("%s criteria is %T, want jev.NoulCriteria", name, q.Criteria)
		}
		// Both poles described is what makes the probability interpretable — an
		// undescribed "no" invites the model to guess what it covers, and the guess
		// inflates the number (pkg/jev's NoulCriteria doc).
		if strings.TrimSpace(criteria.Yes) == "" || strings.TrimSpace(criteria.No) == "" {
			t.Errorf("%s: both poles must be described: yes=%q no=%q", name, criteria.Yes, criteria.No)
		}
		if strings.TrimSpace(q.Instructions) == "" {
			t.Errorf("%s: the instructions carry the whole judgment and must not be empty", name)
		}
	}
}

// TestCandidateQuestionCarriesTheFailureRule pins a POLICY, not a wording
// detail: jobs never say "and tell me if you could not fetch it", so the gate
// has to say it for every job at once. Without it a job that quietly stopped
// working answers exactly like a job with nothing to report, and the user finds
// out weeks later.
func TestCandidateQuestionCarriesTheFailureRule(t *testing.T) {
	criteria, ok := Question().Criteria.(jev.NoulCriteria)
	if !ok {
		t.Fatal("candidate question is not a noul")
	}
	for _, want := range []string{"没能完成", "没取到数据", "运行报错"} {
		if !strings.Contains(criteria.Yes, want) {
			t.Errorf("the yes pole does not name %q, so a job that failed reads as silent:\n%s", want, criteria.Yes)
		}
	}
	if strings.Contains(criteria.No, "没能完成") {
		t.Error("the no pole claims the failure case — one pole, one meaning")
	}
}

// TestQuestionsDiffer: the control exists to price the criterion block, which
// only works while the two questions are actually different.
func TestQuestionsDiffer(t *testing.T) {
	if Question().Instructions == GenericQuestion().Instructions {
		t.Error("the candidate and the control ask the same question — the sweep would compare nothing")
	}
	if strings.Contains(GenericQuestion().Instructions, "任务原话") {
		t.Error("the control question mentions 任务原话, which the flat state does not carry")
	}
}
