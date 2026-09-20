package jev_test

// Measurement harness for the Jev gates.
//
// Every number a gate depends on drifts: the model is versioned, the service
// adjusts its limits dynamically, and a confidence threshold is relative to
// the option set it was swept against. So a calibrated threshold is a
// HYPOTHESIS with an expiry date, and this file is how it gets re-checked.
// It measures the three things that decide whether a gate is viable:
//
//   - Cost and limits: what a realistic request actually spends in input
//     tokens, which is the limit that binds (state + longest question < ~32k,
//     not the 64k total).
//   - Calibration: which threshold separates the cases that should act from
//     the ones that should not, swept rather than guessed.
//   - Stability: whether the same case answers the same way twice, and by how
//     much the confidence moves when it does.
//
// It asserts nothing about the numbers — a harness that fails whenever
// calibration is poor teaches people to ignore it. It fails only on request
// errors and malformed case files, which are not measurements.
//
// Run one file:  JEV_API_KEY=... go test ./pkg/jev/ -run TestJevEval -v \
//                    -args -case notify-gate.json
// Or every file: JEV_API_KEY=... go test ./pkg/jev/ -run TestJevEval -v
//
// Environment:
//
//	JEV_API_KEY       API key. Unset → the whole harness skips.
//	JEV_EVAL_FILE     Run only this file under testdata/ (e.g. notify-gate.json).
//	JEV_EVAL_REPEATS  Repeats per case (default 5). Below 5 the stability
//	                  numbers cannot distinguish "drifts" from "drifted once".
//	JEV_EVAL_MODEL    Model override. Record which version produced a
//	                  calibration — the default is a pinned version, and a
//	                  threshold does not survive a change of weights.
//
// A caveat the skill that seeded this file insists on: repeats try to bypass
// response caching and cannot force it. Identical labels AND identical
// confidences across every repeat is what a cached answer looks like too, so
// the report flags that pattern instead of calling it stability.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/monsterxx03/tachi/pkg/jev"
)

const (
	// evalKeyEnv gates the harness: without a key every case here skips, so
	// `go test ./...` stays offline.
	evalKeyEnv = "JEV_API_KEY"
	// evalFileEnv narrows a run to one case file under testdata/.
	evalFileEnv = "JEV_EVAL_FILE"
	// evalRepeatsEnv overrides the repeat count per case.
	evalRepeatsEnv = "JEV_EVAL_REPEATS"
	// evalModelEnv overrides the model version, for re-measuring a calibration
	// against a new one.
	evalModelEnv = "JEV_EVAL_MODEL"

	// defaultEvalRepeats is the repeat count when unset. Five is the floor
	// that makes a drift measurement meaningful.
	defaultEvalRepeats = 5

	// evalCaseTimeout bounds the repeats of one case. A retried request can
	// spend the client's default timeout several times over.
	evalCaseTimeout = 3 * time.Minute

	// driftTolerance is how far a confidence may move between repeats before
	// the report stops calling it stable. Below this a threshold sits still;
	// above it, a case near a cut-off can flip decisions between calls.
	driftTolerance = 0.02

	// cacheSuspectTolerance is the spread below which repeats are treated as
	// byte-identical — the pattern a cached response also produces.
	cacheSuspectTolerance = 1e-9

	// minPairMembers is how many forms a pair needs. Two is the floor for a
	// comparison; three is what a controlled experiment needs (content,
	// language, register — vary one, hold the rest).
	minPairMembers = 2

	// evalQuestionID is the map key for the single question each case asks.
	// The key is never sent to the model (only its value, the instructions,
	// is), so it is purely a lookup handle here.
	evalQuestionID = "q"
)

// evalThresholds are the candidate gate cut-offs, swept in the report. They
// span "act on almost everything" to "act only on near-certainty".
var evalThresholds = []float64{0.50, 0.60, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95, 0.99}

// evalQuestion is the JSON form of a question. jev.Question cannot be
// decoded directly — its Criteria is a sealed interface — so a case file
// spells the criteria out and build assembles the typed question.
type evalQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Yes          string            `json:"yes,omitempty"`
	No           string            `json:"no,omitempty"`
	Options      map[string]string `json:"options,omitempty"`
	Levels       []string          `json:"levels,omitempty"`
}

func (q evalQuestion) build() (jev.Question, error) {
	switch q.Type {
	case string(jev.QuestionNoul):
		return jev.NewNoul(q.Instructions, jev.NoulCriteria{Yes: q.Yes, No: q.No}), nil
	case string(jev.QuestionChoice):
		options := make([]jev.ChoiceOption, 0, len(q.Options))
		for name, rubric := range q.Options {
			options = append(options, jev.ChoiceOption{Name: name, Rubric: rubric})
		}
		// Sorted so two runs marshal identical bytes: an option order that
		// wanders between runs would make a diff of the measurement useless.
		sort.Slice(options, func(i, j int) bool { return options[i].Name < options[j].Name })
		return jev.NewChoice(q.Instructions, options...), nil
	case string(jev.QuestionScore):
		return jev.NewScore(q.Instructions, q.Levels...), nil
	default:
		return jev.Question{}, fmt.Errorf("unknown question type %q", q.Type)
	}
}

// evalCase is one state to judge, with the label a human says is correct.
type evalCase struct {
	ID string `json:"id"`
	// Group is the author's bucket — "clear" / "ambiguous" / "boundary" are
	// the useful ones, but a language ("zh"/"en") works too. The sweep does
	// not depend on it; it exists so the per-case table shows whether
	// failures cluster.
	Group string `json:"group"`
	// Pair names the OTHER forms of the same judgment: translations, wording
	// variants, a before/after. At least two cases share a pair, and all of
	// them must expect the same label — the report compares them, which is
	// what turns "every form was right" into "and they were read with the
	// same confidence".
	Pair string `json:"pair,omitempty"`
	// Expect is the label this case should receive: an option name, or
	// "true"/"false" for a noul.
	Expect string `json:"expect"`
	// State is sent verbatim, so a case can be a bare string or structure.
	State json.RawMessage `json:"state"`
	// Instructions overrides the file-level question's TEXT for this case,
	// keeping its type and criteria.
	//
	// A gate whose question is per-case cannot be measured with one shared
	// text. Tool search is the case that forced this: it asks one question per
	// candidate tool, so the tool's own name and description belong in the
	// question — the file-level text would measure a prompt the gate never
	// sends, and the threshold swept from it would belong to that other
	// question. Empty means "use the file's", so every existing case file
	// reads exactly as before.
	Instructions string `json:"instructions,omitempty"`
}

// evalCaseFile is one measurement case set.
type evalCaseFile struct {
	Name string `json:"name"`
	// Notes is free text carried into the report, so the assumptions behind
	// the ground-truth labels travel with them.
	Notes string `json:"notes"`
	// ActionLabel is the label a production gate would threshold on — e.g.
	// "true" for "should we notify?". Cases whose Expect equals it are the
	// ones the gate is supposed to let through.
	ActionLabel string       `json:"action_label"`
	Question    evalQuestion `json:"question"`
	Cases       []evalCase   `json:"cases"`
}

// evalRepeat is one API call's outcome for one case.
type evalRepeat struct {
	label string
	// labelProb is the probability the model put on the label this case
	// should have received — "how sure was it of the right answer".
	labelProb float64
	// actionProb is the probability on ActionLabel — what a gate thresholds.
	// actionFound is false when the answer carried no such label, which the
	// gate must treat as "do not act" rather than as zero confidence.
	actionProb   float64
	actionFound  bool
	confidence   float64
	hasConf      bool
	latency      time.Duration
	inputTokens  int
	outputTokens int
}

// evalCaseResult gathers every repeat of one case.
type evalCaseResult struct {
	evalCase
	repeats []evalRepeat
}

// TestEvalCaseFilesAreWellFormed is the offline half of the harness: it
// checks the case files before anyone spends an API call on them. A typo in a
// label turns into "the gate never separated anything" after the run, which
// reads exactly like a model failure.
func TestEvalCaseFilesAreWellFormed(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no case files found under testdata/")
	}
	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var file evalCaseFile
			if err := json.Unmarshal(raw, &file); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := validateEvalFile(file); err != nil {
				t.Fatalf("invalid case file: %v", err)
			}
		})
	}
}

// validateEvalFile reports the first structural problem that would make a run
// unreadable or misleading.
func validateEvalFile(file evalCaseFile) error {
	if strings.TrimSpace(file.Name) == "" {
		return fmt.Errorf("name is required")
	}
	question, err := file.Question.build()
	if err != nil {
		return err
	}
	if err := question.Validate(); err != nil {
		return fmt.Errorf("question: %w", err)
	}

	labels, err := questionLabels(file.Question)
	if err != nil {
		return err
	}
	if !labels[file.ActionLabel] {
		return fmt.Errorf("action_label %q is not a label this question can answer (%s)",
			file.ActionLabel, strings.Join(sortedKeys(labels), ", "))
	}

	seen := map[string]bool{}
	pairMembers := map[string]int{}
	pairExpect := map[string]string{}
	shouldAct := 0
	for _, c := range file.Cases {
		if c.ID == "" {
			return fmt.Errorf("a case has no id")
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		if c.Group == "" {
			return fmt.Errorf("case %s has no group — the per-case table uses it to show whether failures cluster", c.ID)
		}
		if !labels[c.Expect] {
			return fmt.Errorf("case %s: expect %q is not a label this question can answer (%s)",
				c.ID, c.Expect, strings.Join(sortedKeys(labels), ", "))
		}
		if len(c.State) == 0 || string(c.State) == "null" {
			return fmt.Errorf("case %s has no state", c.ID)
		}
		if c.Instructions != "" {
			// The override must still be a usable question. Checked HERE, in
			// the offline half: a blank or malformed text would otherwise fail
			// at the API during the run, where it reads like a model problem
			// rather than an authoring one.
			q, err := questionFor(file.Question, c)
			if err != nil {
				return fmt.Errorf("case %s: instructions override: %w", c.ID, err)
			}
			if err := q.Validate(); err != nil {
				return fmt.Errorf("case %s: instructions override: %w", c.ID, err)
			}
		}
		if c.Pair != "" {
			pairMembers[c.Pair]++
			if prev, ok := pairExpect[c.Pair]; ok && prev != c.Expect {
				return fmt.Errorf("pair %q expects different labels (%q and %q) — a pair is the SAME judgment in two forms, so a difference here is an authoring error, not a finding",
					c.Pair, prev, c.Expect)
			}
			pairExpect[c.Pair] = c.Expect
		}
		if c.Expect == file.ActionLabel {
			shouldAct++
		}
	}
	// Without both halves the sweep has nothing to separate.
	if shouldAct == 0 || shouldAct == len(file.Cases) {
		return fmt.Errorf("all %d cases sit on one side of action_label %q — the sweep needs both",
			len(file.Cases), file.ActionLabel)
	}
	pairNames := make([]string, 0, len(pairMembers))
	for name := range pairMembers {
		pairNames = append(pairNames, name)
	}
	sort.Strings(pairNames)
	for _, name := range pairNames {
		if n := pairMembers[name]; n < minPairMembers {
			return fmt.Errorf("pair %q has %d member(s), want at least %d — the comparison needs something to compare against",
				name, n, minPairMembers)
		}
	}
	return nil
}

// questionLabels is the set of labels a question's answer can carry. It is
// what makes "expect" and "action_label" checkable offline: a label the
// question cannot produce would silently never match.
func questionLabels(q evalQuestion) (map[string]bool, error) {
	switch q.Type {
	case string(jev.QuestionNoul):
		return map[string]bool{"true": true, "false": true}, nil
	case string(jev.QuestionChoice):
		if len(q.Options) == 0 {
			return nil, fmt.Errorf("choice question has no options")
		}
		labels := make(map[string]bool, len(q.Options))
		for name := range q.Options {
			labels[name] = true
		}
		return labels, nil
	default:
		// A score answer is a position on ordered levels, not a label, so the
		// sweep's should-act / should-not-act split has nothing to stand on.
		return nil, fmt.Errorf("question type %q is not supported by this harness: the sweep needs a labelled answer", q.Type)
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestJevEval(t *testing.T) {
	apiKey := os.Getenv(evalKeyEnv)
	if apiKey == "" {
		t.Skipf("%s not set, skipping the Jev measurement harness", evalKeyEnv)
	}
	paths := evalCasePaths(t)
	if len(paths) == 0 {
		t.Fatal("no case files found under testdata/")
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			runEvalFile(t, apiKey, path)
		})
	}
}

// evalCasePaths lists the case files to run: one from the environment, or all
// of testdata/ in a stable order.
func evalCasePaths(t *testing.T) []string {
	t.Helper()
	if name := strings.TrimSpace(os.Getenv(evalFileEnv)); name != "" {
		return []string{filepath.Join("testdata", name)}
	}
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	sort.Strings(paths)
	return paths
}

// evalRepeats resolves the repeat count, flooring at 1.
func evalRepeats(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(evalRepeatsEnv))
	if raw == "" {
		return defaultEvalRepeats
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("%s = %q, want a positive integer", evalRepeatsEnv, raw)
	}
	return n
}

// runEvalFile measures one case file end to end and logs the report.
func runEvalFile(t *testing.T, apiKey, path string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var file evalCaseFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if err := validateEvalFile(file); err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	model := strings.TrimSpace(os.Getenv(evalModelEnv))
	opts := []jev.Option{}
	if model != "" {
		opts = append(opts, jev.WithModel(model))
	}
	client, err := jev.NewClient(apiKey, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	repeats := evalRepeats(t)

	results := make([]evalCaseResult, 0, len(file.Cases))
	for _, c := range file.Cases {
		q, err := questionFor(file.Question, c)
		if err != nil {
			t.Fatalf("%s: case %s: %v", path, c.ID, err)
		}
		res, err := measureCase(t, client, q, c, file.ActionLabel, repeats)
		if err != nil {
			t.Fatalf("%v", err)
		}
		results = append(results, res)
	}

	t.Log(renderReport(file, path, client.Model(), repeats, results))
}

// questionFor returns the question a case asks: the file's, with the
// instructions text replaced when the case carries its own.
//
// Only the TEXT varies — type and criteria stay the file's, so an override can
// never turn a noul into a choice by accident. That is the whole point of
// putting the per-case part in the instructions: the thing that differs
// between two cases of one file is what they ask ABOUT, not what kind of
// judgment it is.
func questionFor(base evalQuestion, c evalCase) (jev.Question, error) {
	if c.Instructions == "" {
		return base.build()
	}
	withText := base
	withText.Instructions = c.Instructions
	return withText.build()
}

// countOverrides is how many cases carry their own instructions. It is printed
// in the report so a table read later is not attributed to the file-level text.
func countOverrides(cases []evalCase) int {
	n := 0
	for _, c := range cases {
		if c.Instructions != "" {
			n++
		}
	}
	return n
}

// TestEvalCaseInstructionsOverride pins the per-case text: the override
// replaces the TEXT and nothing else, and an empty one keeps the file's.
//
// This is the property a per-case gate's calibration rests on — the tool-search
// threshold is only meaningful if the question measured is the question the
// gate sends, and "the type and criteria still come from the file" is what
// makes the override safe to generate (a generator that also swapped the
// criteria would silently turn a noul into a choice).
func TestEvalCaseInstructionsOverride(t *testing.T) {
	base := evalQuestion{Type: "noul", Instructions: "文件级：这条工具能不能完成请求？", Yes: "能", No: "不能"}

	fromFile, err := questionFor(base, evalCase{ID: "c1"})
	if err != nil {
		t.Fatalf("file-level question: %v", err)
	}
	own, err := questionFor(base, evalCase{ID: "c2", Instructions: "工具 mcp__postgres__query（server: postgres）：查询数据库。能不能完成请求？"})
	if err != nil {
		t.Fatalf("override question: %v", err)
	}

	fromFileJSON, err := json.Marshal(fromFile)
	if err != nil {
		t.Fatalf("marshal file-level: %v", err)
	}
	ownJSON, err := json.Marshal(own)
	if err != nil {
		t.Fatalf("marshal override: %v", err)
	}
	want := strings.Replace(string(fromFileJSON), base.Instructions, "工具 mcp__postgres__query（server: postgres）：查询数据库。能不能完成请求？", 1)
	if string(ownJSON) != want {
		t.Errorf("override changed more than the instructions text:\n got %s\nwant %s", ownJSON, want)
	}
	if !strings.Contains(string(ownJSON), "mcp__postgres__query") {
		t.Errorf("override text is not in the built question: %s", ownJSON)
	}
}

// measureCase asks one case `repeats` times and collects the outcomes.
func measureCase(t *testing.T, client *jev.Client, question jev.Question, c evalCase, actionLabel string, repeats int) (evalCaseResult, error) {
	t.Helper()

	var state any
	if err := json.Unmarshal(c.State, &state); err != nil {
		return evalCaseResult{}, fmt.Errorf("case %s: decode state: %w", c.ID, err)
	}

	res := evalCaseResult{evalCase: c}
	for i := 0; i < repeats; i++ {
		// Per-repeat timeout: one slow case must not consume the budget of
		// the rest of the file.
		ctx, cancel := context.WithTimeout(t.Context(), evalCaseTimeout)
		start := time.Now()
		resp, err := client.Evaluate(ctx, state, map[string]jev.Question{evalQuestionID: question})
		latency := time.Since(start)
		cancel()
		if err != nil {
			return res, fmt.Errorf("case %s: %w", c.ID, err)
		}
		answer, ok := resp.Answers[evalQuestionID]
		if !ok {
			return res, fmt.Errorf("case %s: response carried no answer for question %q", c.ID, evalQuestionID)
		}
		res.repeats = append(res.repeats, observe(answer, c.Expect, actionLabel, latency, resp.Usage))
	}
	return res, nil
}

// observe reduces one answer to what the report needs.
func observe(a jev.Answer, expect, actionLabel string, latency time.Duration, usage jev.Usage) evalRepeat {
	rep := evalRepeat{
		label:        answerLabel(a),
		latency:      latency,
		inputTokens:  usage.InputTokens,
		outputTokens: usage.OutputTokens,
	}
	if a.Confidence != nil {
		rep.confidence = *a.Confidence
		rep.hasConf = true
	}
	// The label this case SHOULD have received: how sure was the model of the
	// right answer.
	if p, ok := a.ProbabilityOf(expect); ok {
		rep.labelProb = p
	}
	// The label a production gate thresholds. Distinguishing "the model put
	// zero mass on it" from "the answer carried no such label" is the whole
	// reason for the second bool: a gate must treat both as "do not act", but
	// the report should show the difference.
	if p, ok := a.ProbabilityOf(actionLabel); ok {
		rep.actionProb = p
		rep.actionFound = true
	}
	return rep
}

// answerLabel is the model's decision, reduced to a comparable string: a noul
// resolves to "true" or "false" at the midpoint, a choice to its option.
func answerLabel(a jev.Answer) string {
	switch a.Type {
	case jev.QuestionNoul:
		if a.Noul == nil {
			return ""
		}
		if *a.Noul >= 0.5 {
			return "true"
		}
		return "false"
	case jev.QuestionChoice:
		return a.Choice
	default:
		return ""
	}
}

// renderReport formats the whole measurement. It returns a string rather
// than writing to t so the layout can be asserted offline — a formatting
// panic would otherwise only surface during a metered run.
func renderReport(file evalCaseFile, path, model string, repeats int, results []evalCaseResult) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\n=== %s (%s) ===\n", file.Name, path)
	fmt.Fprintf(&b, "model=%s cases=%d repeats=%d action_label=%q\n", model, len(results), repeats, file.ActionLabel)
	if n := countOverrides(file.Cases); n > 0 {
		fmt.Fprintf(&b, "instructions: %d/%d cases carry their own (the file-level text is their frame)\n", n, len(file.Cases))
	}
	if file.Notes != "" {
		fmt.Fprintf(&b, "notes: %s\n", file.Notes)
	}

	reportCases(&b, file, results)
	reportStability(&b, results, repeats)
	reportCost(&b, results)
	reportSweep(&b, file, results)
	reportBoundary(&b, file, results)
	reportPairs(&b, results)
	return b.String()
}

// reportPairs compares the forms of each pair.
//
// "Every form came out right" is a weak claim about a paraphrase: the
// interesting failure is the one where all forms land on the expected label
// and the probabilities still span a third of the scale, because that is the
// pair a single threshold cannot serve. The spread within a pair is the
// effect the varying dimension has — language, register, template — and it is
// what a threshold would trip on.
func reportPairs(b *bytes.Buffer, results []evalCaseResult) {
	pairs := groupPairs(results)
	if len(pairs) == 0 {
		return
	}

	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nPAIRS (the same judgment in several forms, compared on mean p(action))")
	fmt.Fprintln(w, "PAIR\tFORM\tLABEL\tP(ACTION)\tSPREAD")

	var totalSpread, maxSpread float64
	splitPairs := 0
	for _, members := range pairs {
		// Two passes: the spread is a property of the whole pair, so it cannot
		// be written on the first row before the last member has been read.
		probs := make([]float64, len(members))
		labels := make([]string, len(members))
		lo, hi := math.MaxFloat64, -math.MaxFloat64
		distinct := map[string]bool{}
		for i, m := range members {
			probs[i] = meanActionProbOf(m)
			labels[i] = mostCommon(labelsOf(m))
			distinct[labels[i]] = true
			lo, hi = math.Min(lo, probs[i]), math.Max(hi, probs[i])
		}
		spread := hi - lo
		totalSpread += spread
		maxSpread = math.Max(maxSpread, spread)
		if len(distinct) > 1 {
			splitPairs++
		}

		for i, m := range members {
			// The pair's name and spread ride on its first row: repeating them
			// per form would bury the one number the section exists for.
			pairCol, spreadCol := "", ""
			if i == 0 {
				pairCol, spreadCol = m.Pair, fmt.Sprintf("%.3f", spread)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%s\n", pairCol, m.Group, labels[i], probs[i], spreadCol)
		}
	}
	_ = w.Flush()

	fmt.Fprintf(b, "  mean spread %.3f, max %.3f across %d pairs; %d pair(s) put the forms on different labels\n",
		totalSpread/float64(len(pairs)), maxSpread, len(pairs), splitPairs)
	switch {
	case splitPairs > 0:
		// A label split is the ARGMAX differing (the forms sit on opposite
		// sides of 0.5) — which is not the same as no gate being able to serve
		// them. A threshold above the highest form still decides them all the
		// same way, so a split constrains the threshold rather than ruling it
		// out.
		fmt.Fprintf(b, "  ⚠ %d pair(s) put the forms on different labels: the spread crosses the decision midpoint,\n"+
			"    so they disagree by default. A threshold above the highest form still decides them alike —\n"+
			"    check the sweep for one that also holds for every other case.\n", splitPairs)
	case maxSpread > driftTolerance:
		fmt.Fprintf(b, "  ⚠ labels agree but the spread exceeds the drift tolerance: the threshold sits closer to one form's edge than another's\n")
	default:
		fmt.Fprintf(b, "  ✓ the forms read alike: a threshold calibrated on one holds for the others\n")
	}
}

// groupPairs collects the pairs in first-seen order, dropping any that have
// fewer than two members (validateEvalFile rejects those before a run).
func groupPairs(results []evalCaseResult) [][]evalCaseResult {
	var order []string
	byPair := map[string][]evalCaseResult{}
	for _, r := range results {
		if r.Pair == "" {
			continue
		}
		if _, seen := byPair[r.Pair]; !seen {
			order = append(order, r.Pair)
		}
		byPair[r.Pair] = append(byPair[r.Pair], r)
	}
	pairs := make([][]evalCaseResult, 0, len(order))
	for _, name := range order {
		if members := byPair[name]; len(members) >= minPairMembers {
			pairs = append(pairs, members)
		}
	}
	return pairs
}

func labelsOf(r evalCaseResult) []string {
	labels := make([]string, 0, len(r.repeats))
	for _, rep := range r.repeats {
		labels = append(labels, rep.label)
	}
	return labels
}

// meanActionProbOf averages p(action) over the repeats that carried the label.
func meanActionProbOf(r evalCaseResult) float64 {
	return meanOf(r, func(rep evalRepeat) float64 {
		if !rep.actionFound {
			return 0
		}
		return rep.actionProb
	})
}

// reportCases is the per-case table: what the model answered, how sure it was
// of the right answer, and how long the call took.
//
// P(ACTION) is printed as a RANGE, not a mean, because that is the decision
// the table exists to support: a case whose range spans a candidate threshold
// will flap there, and a mean renders that as a tidy number right up until it
// does not.
func reportCases(b *bytes.Buffer, file evalCaseFile, results []evalCaseResult) {
	b.WriteString("\nPER CASE\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CASE\tGROUP\tWANT\tGOT\tAGREE\tP(WANT)\tP(ACTION)\tCONF\tLATENCY")
	for _, r := range results {
		labels := make([]string, 0, len(r.repeats))
		for _, rep := range r.repeats {
			labels = append(labels, rep.label)
		}
		agree := 0
		for _, l := range labels {
			if l == r.Expect {
				agree++
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d/%d\t%.3f\t%s\t%s\t%s\n",
			r.ID, r.Group, r.Expect, mostCommon(labels), agree, len(r.repeats),
			meanLabelProb(r), actionProbRange(r), confRange(r), meanLatency(r))
	}
	_ = w.Flush()
}

// reportStability answers "does the same case answer the same way twice".
//
// The verdict weighs label agreement AND confidence drift, because they can
// disagree: a noul answer carries no confidence at all, so its drift is
// vacuously zero while its labels flap. Reporting that as "stable" is exactly
// the reading that gets a flapping gate trusted.
func reportStability(b *bytes.Buffer, results []evalCaseResult, repeats int) {
	b.WriteString("\nSTABILITY\n")

	var unstable []string
	maxDrift := 0.0
	driftMeasured := false
	for _, r := range results {
		distinct := map[string]bool{}
		for _, rep := range r.repeats {
			distinct[rep.label] = true
		}
		if len(distinct) > 1 {
			unstable = append(unstable, r.ID)
		}
		if _, _, ok := confBounds(r); ok {
			driftMeasured = true
			maxDrift = math.Max(maxDrift, confDrift(r))
		}
	}

	if len(unstable) == 0 {
		fmt.Fprintf(b, "  labels: identical across all %d repeats (%d cases)\n", repeats, len(results))
	} else {
		fmt.Fprintf(b, "  labels: %d/%d cases changed between repeats — %s\n",
			len(unstable), len(results), strings.Join(unstable, ", "))
	}
	if driftMeasured {
		fmt.Fprintf(b, "  confidence: max drift %.3f across repeats (tolerance %.3f)\n", maxDrift, driftTolerance)
	} else {
		fmt.Fprintf(b, "  confidence: not carried by these answers — a noul has none, so label agreement is the whole signal\n")
	}

	switch {
	case len(unstable) > 0:
		fmt.Fprintf(b, "  ⚠ %d case(s) changed label between identical calls: a gate reading these answers will flap.\n"+
			"    Prefer more headroom around the cut-off, or hysteresis — not a re-run hoping for a cleaner sample.\n", len(unstable))
	case driftMeasured && maxDrift > driftTolerance:
		fmt.Fprintf(b, "  ⚠ drift exceeds tolerance: leave headroom around the chosen threshold, or add hysteresis\n")
	default:
		fmt.Fprintf(b, "  ✓ labels held and no drift exceeded tolerance: a threshold swept on these numbers holds\n")
	}

	if identicalEverywhere(results) {
		fmt.Fprintf(b, "  ⚠ every repeat returned the same label AND the same confidence — that is also what a CACHED response looks like.\n"+
			"    Confirm this is the model settling, not the service replaying, before treating it as stability.\n")
	}
}

// reportCost sums the request cost. Input tokens are what get billed; the
// limit that binds is the 32k one (state + longest question), which is why
// the largest request is printed alongside the total.
func reportCost(b *bytes.Buffer, results []evalCaseResult) {
	b.WriteString("\nCOST\n")
	var totalIn, totalOut, maxIn, minIn int
	minIn = math.MaxInt
	var calls int
	for _, r := range results {
		for _, rep := range r.repeats {
			totalIn += rep.inputTokens
			totalOut += rep.outputTokens
			maxIn = max(maxIn, rep.inputTokens)
			minIn = min(minIn, rep.inputTokens)
			calls++
		}
	}
	if calls == 0 {
		b.WriteString("  no calls completed\n")
		return
	}
	fmt.Fprintf(b, "  %d calls: input %d total (mean %d, min %d, max %d), output %d total\n",
		calls, totalIn, totalIn/calls, minIn, maxIn, totalOut)
	fmt.Fprintf(b, "  largest request %d input tokens against the ~32000 ceiling for state + longest question\n", maxIn)
}

// reportSweep is the point of the harness: which cut-off actually separates
// the cases that should act from the ones that should not.
//
// "Act" is per CALL, not per case — the fraction of repeats whose probability
// on ActionLabel cleared the threshold — because that folds in instability,
// which a production gate would feel as flapping. The expectation comes from
// the case's own Expect label, so a group name never has to mean anything to
// the maths.
func reportSweep(b *bytes.Buffer, file evalCaseFile, results []evalCaseResult) {
	b.WriteString("\nTHRESHOLD SWEEP (act when p(" + file.ActionLabel + ") >= t)\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "T\tSHOULD-ACT\tSHOULD-NOT-ACT\tSEPARATION")

	shouldAct := 0
	shouldNotAct := 0
	for _, r := range results {
		if r.Expect == file.ActionLabel {
			shouldAct++
		} else {
			shouldNotAct++
		}
	}

	perfect := []string{}
	for _, threshold := range evalThresholds {
		actHits, actTotal := sweepGroup(results, file.ActionLabel, threshold, true)
		skipHits, skipTotal := sweepGroup(results, file.ActionLabel, threshold, false)
		actRate := ratio(actHits, actTotal)
		skipRate := ratio(skipHits, skipTotal)
		fmt.Fprintf(w, "%.2f\t%d/%d (%.2f)\t%d/%d (%.2f)\t%+.2f\n",
			threshold, actHits, actTotal, actRate, skipHits, skipTotal, skipRate, actRate-skipRate)
		if actRate == 1 && skipRate == 0 {
			perfect = append(perfect, fmt.Sprintf("%.2f", threshold))
		}
	}
	_ = w.Flush()

	fmt.Fprintf(b, "  %d cases should act, %d should not\n", shouldAct, shouldNotAct)
	if len(perfect) == 0 {
		fmt.Fprintf(b, "  no swept threshold separates the two sets completely — inspect the per-case table for the overlap before picking one\n")
		return
	}
	fmt.Fprintf(b, "  separating thresholds: %s — pick from the middle of the run, not its edge, so drift cannot walk a case across\n",
		strings.Join(perfect, ", "))
}

// sweepGroup counts, over every repeat in the matching half of the file, how
// many would have acted at the threshold.
func sweepGroup(results []evalCaseResult, actionLabel string, threshold float64, act bool) (hits, total int) {
	for _, r := range results {
		if (r.Expect == actionLabel) != act {
			continue
		}
		for _, rep := range r.repeats {
			total++
			if rep.actionFound && rep.actionProb >= threshold {
				hits++
			}
		}
	}
	return hits, total
}

// reportBoundary names the cases whose p(action) spread crosses a candidate
// threshold.
//
// The sweep table shows this only in aggregate: "21/30 acted" does not say
// WHICH case wavered, and the wavering case is the one that decides whether a
// gate feels reliable. It is the boundary-sample warning made operational —
// for a noul there is no confidence field to drift, so this range is the only
// place a near-tie shows up at all.
func reportBoundary(b *bytes.Buffer, file evalCaseFile, results []evalCaseResult) {
	// Header and rows share one tabwriter, so the columns line up; the whole
	// block is built aside and only emitted when it has content.
	var block bytes.Buffer
	w := tabwriter.NewWriter(&block, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CASE\tP("+file.ActionLabel+")\tCROSSES")
	found := false
	for _, r := range results {
		lo, hi, ok := actionBounds(r)
		if !ok || hi <= lo {
			continue
		}
		var crossed []string
		for _, t := range evalThresholds {
			if lo < t && t <= hi {
				crossed = append(crossed, fmt.Sprintf("%.2f", t))
			}
		}
		if len(crossed) == 0 {
			continue
		}
		found = true
		fmt.Fprintf(w, "%s\t%.3f..%.3f\t%s\n", r.ID, lo, hi, strings.Join(crossed, ", "))
	}
	_ = w.Flush()
	if !found {
		return
	}

	fmt.Fprintf(b, "\nBOUNDARY (p(%s) moved between repeats across a candidate threshold)\n", file.ActionLabel)
	b.Write(block.Bytes())
	b.WriteString("  a case listed here acts on some calls and not others at the thresholds named — do not pick one of them\n")
}

// actionProbRange formats the spread of p(action) across repeats, or "n/a"
// when the answer never carried that label.
func actionProbRange(r evalCaseResult) string {
	lo, hi, ok := actionBounds(r)
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.2f..%.2f", lo, hi)
}

func actionBounds(r evalCaseResult) (lo, hi float64, ok bool) {
	lo, hi = math.MaxFloat64, -math.MaxFloat64
	for _, rep := range r.repeats {
		if !rep.actionFound {
			continue
		}
		ok = true
		lo = math.Min(lo, rep.actionProb)
		hi = math.Max(hi, rep.actionProb)
	}
	return lo, hi, ok
}

// --- small helpers -------------------------------------------------------

func ratio(hits, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

func mostCommon(labels []string) string {
	counts := map[string]int{}
	for _, l := range labels {
		counts[l]++
	}
	best, bestN := "", -1
	for l, n := range counts {
		if n > bestN || (n == bestN && l < best) {
			best, bestN = l, n
		}
	}
	return best
}

func meanLabelProb(r evalCaseResult) float64 {
	return meanOf(r, func(rep evalRepeat) float64 { return rep.labelProb })
}

func meanOf(r evalCaseResult, pick func(evalRepeat) float64) float64 {
	if len(r.repeats) == 0 {
		return 0
	}
	var sum float64
	for _, rep := range r.repeats {
		sum += pick(rep)
	}
	return sum / float64(len(r.repeats))
}

func meanLatency(r evalCaseResult) time.Duration {
	if len(r.repeats) == 0 {
		return 0
	}
	var sum time.Duration
	for _, rep := range r.repeats {
		sum += rep.latency
	}
	return (sum / time.Duration(len(r.repeats))).Round(time.Millisecond)
}

// confRange formats the confidence spread across repeats, or "n/a" for a noul
// answer, which carries no confidence at all.
func confRange(r evalCaseResult) string {
	lo, hi, ok := confBounds(r)
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.2f..%.2f", lo, hi)
}

func confBounds(r evalCaseResult) (lo, hi float64, ok bool) {
	lo, hi = math.MaxFloat64, -math.MaxFloat64
	for _, rep := range r.repeats {
		if !rep.hasConf {
			continue
		}
		ok = true
		lo = math.Min(lo, rep.confidence)
		hi = math.Max(hi, rep.confidence)
	}
	return lo, hi, ok
}

func confDrift(r evalCaseResult) float64 {
	lo, hi, ok := confBounds(r)
	if !ok {
		return 0
	}
	return hi - lo
}

// identicalEverywhere reports the pattern a cached response also produces.
func identicalEverywhere(results []evalCaseResult) bool {
	for _, r := range results {
		if len(r.repeats) < 2 {
			return false
		}
		first := r.repeats[0]
		for _, rep := range r.repeats[1:] {
			if rep.label != first.label {
				return false
			}
			if math.Abs(rep.labelProb-first.labelProb) > cacheSuspectTolerance {
				return false
			}
		}
	}
	return true
}
