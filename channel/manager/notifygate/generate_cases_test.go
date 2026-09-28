package notifygate

// Generator for the cron notify gate's case files.
//
// pkg/jev's harness can only measure a state this package BUILT: a hand-written
// case file measures a prompt nobody sends, so the state in the file has to come
// from the same function the gate would call (docs/agents/jev.md, "Measuring a
// threshold"). One authored scenario therefore becomes two states — the flat
// one and the template one — and the two files differ only in that block:
//
//	NOTIFY_CASES_SRC=$PWD/pkg/jev/testdata/notify/source.json \
//	NOTIFY_CASES_OUT=$PWD/pkg/jev/testdata \
//	go test ./channel/manager/notifygate/ -run TestGenerateNotifyCases -v
//
// Both paths are absolute on purpose: `go test` runs the package's own
// directory, so a repo-relative path resolves somewhere else.
//
// Both knobs are required and the test skips without them: the source is
// authored material and the destination is a repo path, so neither has a
// sensible default to guess. The generated files ARE committed (the sweep has to
// be reproducible from them, and the source carries no secrets) — which makes
// hand edits to them pointless, and the notes written into each file say so.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/monsterxx03/tachi/pkg/jev"
)

const (
	// srcEnv names the authored scenario file (see pkg/jev/testdata/notify/source.json).
	srcEnv = "NOTIFY_CASES_SRC"
	// outDirEnv names the directory the case files are written to — pkg/jev/testdata,
	// which is where the harness globs testdata/*.json. A subdirectory would be
	// invisible to it.
	outDirEnv = "NOTIFY_CASES_OUT"

	// flatFileName, templateFileName and promptFileName are the generated case
	// files. All three land in pkg/jev/testdata; the names are what a sweep
	// command names. prompt is the candidate design, flat its control.
	flatFileName     = "notify-gate.json"
	templateFileName = "notify-gate-template.json"
	promptFileName   = "notify-gate-prompt.json"
)

// sourceCase is one authored scenario. Every field is either code-owned
// metadata (job, schedule, outcome) or one of the two report forms: `report` is
// the prose a free-form turn writes, `change`/`impact` the fields a turn on the
// pinned shape fills. Both forms describe the same scenario, so a difference
// between the two files is the shape and not the story.
type sourceCase struct {
	ID       string `json:"id"`
	Group    string `json:"group"`
	Expect   string `json:"expect"`
	Job      string `json:"job"`
	Schedule string `json:"schedule"`
	// Prompt is the job's own words — the text that created the task, and the
	// only place a job can say when it wants to be told. Some scenarios state a
	// condition (the four the deployment actually runs), some only say what to
	// do: the second kind is the graceful half of the candidate design.
	Prompt string `json:"prompt"`
	// Outcome is "ok" or "error" — the assembler's constants carry the wording,
	// so the source carries the fact.
	Outcome string `json:"outcome"`
	Error   string `json:"error"`
	Report  string `json:"report"`
	Change  string `json:"change"`
	Impact  string `json:"impact"`
}

type sourceFile struct {
	Name  string       `json:"name"`
	Notes string       `json:"notes"`
	Cases []sourceCase `json:"cases"`
}

// The generated file mirrors pkg/jev's `evalCaseFile`. Kept in step by hand, as
// the tool-search corpus is: the two are the two ends of one format, and a
// renamed field shows up on the other side as a case with no state (a
// well-formedness failure) rather than as a crash.
type caseFile struct {
	Name        string       `json:"name"`
	Notes       string       `json:"notes"`
	ActionLabel string       `json:"action_label"`
	Question    caseQuestion `json:"question"`
	Cases       []caseRecord `json:"cases"`
}

type caseQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Yes          string `json:"yes,omitempty"`
	No           string `json:"no,omitempty"`
}

type caseRecord struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Expect string `json:"expect"`
	State  string `json:"state"`
}

func TestGenerateNotifyCases(t *testing.T) {
	src := os.Getenv(srcEnv)
	outDir := os.Getenv(outDirEnv)
	if src == "" || outDir == "" {
		t.Skipf("set %s and %s to generate the notify-gate case files", srcEnv, outDirEnv)
	}

	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	var in sourceFile
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("decode %s: %v", src, err)
	}
	if err := validateSource(in); err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	// Two questions, because the candidate reads its criterion out of the state
	// and the control arm cannot: the same file judged by a judge that never
	// saw the job's words is how the criterion block gets priced.
	candidateQ, err := questionRecord(Question())
	if err != nil {
		t.Fatalf("%s: candidate: %v", src, err)
	}
	controlQ, err := questionRecord(GenericQuestion())
	if err != nil {
		t.Fatalf("%s: control: %v", src, err)
	}

	provenance := "GENERATED by channel/manager/notifygate's TestGenerateNotifyCases from " +
		filepath.Base(src) + " — edit THAT file and regenerate, never this one. "
	// The source's own notes ride along: a sweep report prints the notes, and the
	// caveats a reader needs there (the labels are one person's judgement, two
	// members are contested by design) are in them.
	scenarios := " Scenario set: " + in.Notes
	prompt := caseFile{
		Name:        "cron-notify-gate-prompt",
		ActionLabel: ActionLabel,
		Question:    candidateQ,
		Notes: provenance +
			"CANDIDATE design: the job's own words ride in the state (任务原话), so the judgment is 'does this result satisfy THIS job's stated condition' rather than one generic reading of worth. " +
			"Paired with notify-gate.json (flat, same scenarios, no criterion, generic question) as its control — the two differ in the state AND the question by design, so the pair prices the whole design and not the block alone. " +
			"Within this file, cases whose prompt states no condition are the graceful half: the model infers one from the task description." + scenarios,
	}
	flat := caseFile{
		Name:        "cron-notify-gate-flat",
		ActionLabel: ActionLabel,
		Question:    controlQ,
		Notes: provenance +
			"CONTROL design: the report as free prose and no criterion — the shape cron has today, and what the agent's [SILENT] marker was standing in for. " +
			"Its question is GenericQuestion(), i.e. what a judge answers without the job's own words." + scenarios,
	}
	tmpl := caseFile{
		Name:        "cron-notify-gate-template",
		ActionLabel: ActionLabel,
		Question:    controlQ,
		Notes: provenance +
			"VARIANT: the report cut into two labelled observations the job's prompt pins (变化 / 影响), so the register reaching the gate is the same on every run of every job. " +
			"A follow-on to the other two — the fix for free prose, if free prose alone turns out unsweepable — rather than a peer of them. " +
			"Same scenarios, same ids, same labels, same header as notify-gate.json: only the report block differs." + scenarios,
	}

	for _, c := range in.Cases {
		run := Run{
			Job:      c.Job,
			Schedule: c.Schedule,
			Prompt:   c.Prompt,
			Outcome:  OutcomeOK,
			Err:      c.Error,
			Report:   c.Report,
			Fields:   Fields{Change: c.Change, Impact: c.Impact},
		}
		if c.Outcome == "error" {
			run.Outcome = OutcomeFailed
		}
		record := func(state string) caseRecord {
			return caseRecord{ID: c.ID, Group: c.Group, Expect: c.Expect, State: state}
		}
		prompt.Cases = append(prompt.Cases, record(PromptState(run)))
		flat.Cases = append(flat.Cases, record(FlatState(run)))
		tmpl.Cases = append(tmpl.Cases, record(TemplateState(run)))
	}

	for name, file := range map[string]caseFile{
		promptFileName:   prompt,
		flatFileName:     flat,
		templateFileName: tmpl,
	} {
		path := filepath.Join(outDir, name)
		if err := writeCaseFile(path, file); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("%s: %d cases, %d should act", path, len(file.Cases), countActing(file.Cases))
	}
}

// questionRecord renders a production question into the harness's shape. A
// question that is not a noul is a generator error rather than a silent empty
// criteria: the harness reads `yes`/`no` for a noul and would otherwise produce
// a file whose question carries no poles at all.
func questionRecord(q jev.Question) (caseQuestion, error) {
	if err := q.Validate(); err != nil {
		return caseQuestion{}, fmt.Errorf("production question does not validate: %w", err)
	}
	criteria, ok := q.Criteria.(jev.NoulCriteria)
	if !ok {
		return caseQuestion{}, fmt.Errorf("production question is %T, want a noul", q.Criteria)
	}
	return caseQuestion{
		Type:         string(jev.QuestionNoul),
		Instructions: q.Instructions,
		Yes:          criteria.Yes,
		No:           criteria.No,
	}, nil
}

// validateSource fails on the authoring mistakes that would otherwise read as a
// model problem in the report: a missing label, an outcome the assembler cannot
// render, a scenario with no report, or a file sitting on one side of the
// action label (the sweep would have nothing to separate).
func validateSource(in sourceFile) error {
	if len(in.Cases) == 0 {
		return fmt.Errorf("no cases")
	}
	seen := map[string]bool{}
	acting := 0
	for _, c := range in.Cases {
		if c.ID == "" {
			return fmt.Errorf("a case has no id")
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		if c.Group == "" {
			return fmt.Errorf("case %s has no group", c.ID)
		}
		if c.Expect != "true" && c.Expect != "false" {
			return fmt.Errorf("case %s: expect %q, want \"true\" or \"false\"", c.ID, c.Expect)
		}
		if c.Expect == ActionLabel {
			acting++
		}
		if c.Outcome != "ok" && c.Outcome != "error" {
			return fmt.Errorf("case %s: outcome %q, want \"ok\" or \"error\"", c.ID, c.Outcome)
		}
		if c.Job == "" || c.Schedule == "" {
			return fmt.Errorf("case %s: job and schedule are the code-owned header and both are required", c.ID)
		}
		if c.Prompt == "" {
			return fmt.Errorf("case %s: prompt is required — it is the job's own words and the only place the candidate shape's criterion can come from", c.ID)
		}
		if c.Report == "" && c.Change == "" {
			return fmt.Errorf("case %s: neither report nor change is set — one of the two shapes would render empty", c.ID)
		}
	}
	if acting == 0 || acting == len(in.Cases) {
		return fmt.Errorf("all %d cases sit on one side of %q — the sweep needs both", len(in.Cases), ActionLabel)
	}
	return nil
}

func countActing(cases []caseRecord) int {
	n := 0
	for _, c := range cases {
		if c.Expect == ActionLabel {
			n++
		}
	}
	return n
}

func writeCaseFile(path string, file caseFile) error {
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// TestSourceGroupsAreStable keeps the group vocabulary small enough to read in a
// per-case table: the sweep's failures are supposed to cluster visibly, and a
// group that exists for one case clusters with nothing.
func TestSourceGroupsAreStable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "jev", "testdata", "notify", "source.json"))
	if err != nil {
		t.Skipf("source file not readable from this package: %v", err)
	}
	var in sourceFile
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	counts := map[string]int{}
	for _, c := range in.Cases {
		counts[c.Group]++
	}
	groups := make([]string, 0, len(counts))
	for g := range counts {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		if counts[g] < 2 {
			t.Errorf("group %q has a single case — it clusters with nothing in the report", g)
		}
		t.Logf("  %-18s %d", g, counts[g])
	}
}
