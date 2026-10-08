package jev_test

// Offline tests for the harness's own arithmetic. The measurement itself
// needs a key, but the parts that turn answers into a report do not — and a
// silent error there would make a run read as a model failure.

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/jev"
)

func fp(v float64) *float64 { return &v }

// repeatFor builds one observed repeat with the given action probability.
func repeatFor(label string, labelProb, actionProb float64) evalRepeat {
	return evalRepeat{label: label, labelProb: labelProb, actionProb: actionProb, actionFound: true}
}

func resultFor(id, expect, group string, repeats ...evalRepeat) evalCaseResult {
	return evalCaseResult{
		evalCase: evalCase{ID: id, Expect: expect, Group: group, State: []byte(`"s"`)},
		repeats:  repeats,
	}
}

func TestAnswerLabel(t *testing.T) {
	tests := []struct {
		name   string
		answer jev.Answer
		want   string
	}{
		{name: "noul above the midpoint", answer: jev.Answer{Type: jev.QuestionNoul, Noul: fp(0.9)}, want: "true"},
		{name: "noul below the midpoint", answer: jev.Answer{Type: jev.QuestionNoul, Noul: fp(0.1)}, want: "false"},
		{name: "noul exactly at the midpoint resolves to true", answer: jev.Answer{Type: jev.QuestionNoul, Noul: fp(0.5)}, want: "true"},
		{name: "noul with no value", answer: jev.Answer{Type: jev.QuestionNoul}, want: ""},
		{name: "choice", answer: jev.Answer{Type: jev.QuestionChoice, Choice: "billing"}, want: "billing"},
		{name: "score has no label", answer: jev.Answer{Type: jev.QuestionScore, Score: fp(1.6)}, want: ""},
		{name: "zero value", answer: jev.Answer{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := answerLabel(tc.answer); got != tc.want {
				t.Errorf("answerLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// The two probabilities come from the SAME answer but different labels: one is
// "how sure was it of the right answer", the other is "what a gate would
// threshold". Confusing them is the easiest way to report a flattering
// calibration, so pin them apart.
func TestObserveResolvesLabelAndActionProbabilities(t *testing.T) {
	answer := jev.Answer{Type: jev.QuestionNoul, Noul: fp(0.8)}

	t.Run("expect matches the action label", func(t *testing.T) {
		rep := observe(answer, "true", "true", time.Millisecond, jev.Usage{InputTokens: 10, OutputTokens: 2})
		if math.Abs(rep.labelProb-0.8) > 1e-9 || math.Abs(rep.actionProb-0.8) > 1e-9 {
			t.Errorf("labelProb=%v actionProb=%v, want both 0.8", rep.labelProb, rep.actionProb)
		}
		if rep.label != "true" || !rep.actionFound {
			t.Errorf("label=%q actionFound=%v", rep.label, rep.actionFound)
		}
	})

	t.Run("expect is the other pole", func(t *testing.T) {
		rep := observe(answer, "false", "true", time.Millisecond, jev.Usage{})
		if math.Abs(rep.labelProb-0.2) > 1e-9 {
			t.Errorf("labelProb = %v, want 0.2 (the complement)", rep.labelProb)
		}
		if math.Abs(rep.actionProb-0.8) > 1e-9 {
			t.Errorf("actionProb = %v, want 0.8", rep.actionProb)
		}
	})

	t.Run("a label the answer cannot carry", func(t *testing.T) {
		rep := observe(answer, "true", "not_an_option", time.Millisecond, jev.Usage{})
		if rep.actionFound {
			t.Error("actionFound = true for a label the answer cannot carry")
		}
		if rep.actionProb != 0 {
			t.Errorf("actionProb = %v, want 0", rep.actionProb)
		}
	})

	t.Run("confidence is absent for a noul", func(t *testing.T) {
		rep := observe(answer, "true", "true", time.Millisecond, jev.Usage{})
		if rep.hasConf {
			t.Error("hasConf = true, but a noul answer carries no confidence")
		}
	})
}

func TestSweepGroupSeparatesTheTwoHalves(t *testing.T) {
	results := []evalCaseResult{
		resultFor("a1", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("true", 0.9, 0.9)),
		resultFor("a2", "true", "clear", repeatFor("true", 0.85, 0.85)),
		resultFor("s1", "false", "clear", repeatFor("false", 0.9, 0.1), repeatFor("false", 0.9, 0.1)),
		resultFor("s2", "false", "ambiguous", repeatFor("false", 0.6, 0.4)),
	}

	tests := []struct {
		name          string
		threshold     float64
		wantActHits   int
		wantActTotal  int
		wantSkipHits  int
		wantSkipTotal int
	}{
		{name: "low cut-off lets the wrong half through", threshold: 0.1, wantActHits: 3, wantActTotal: 3, wantSkipHits: 3, wantSkipTotal: 3},
		{name: "mid cut-off separates them", threshold: 0.8, wantActHits: 3, wantActTotal: 3, wantSkipHits: 0, wantSkipTotal: 3},
		{name: "high cut-off drops the marginal should-act case", threshold: 0.88, wantActHits: 2, wantActTotal: 3, wantSkipHits: 0, wantSkipTotal: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actHits, actTotal := sweepGroup(results, "true", tc.threshold, true)
			if actHits != tc.wantActHits || actTotal != tc.wantActTotal {
				t.Errorf("should-act = %d/%d, want %d/%d", actHits, actTotal, tc.wantActHits, tc.wantActTotal)
			}
			skipHits, skipTotal := sweepGroup(results, "true", tc.threshold, false)
			if skipHits != tc.wantSkipHits || skipTotal != tc.wantSkipTotal {
				t.Errorf("should-not-act = %d/%d, want %d/%d", skipHits, skipTotal, tc.wantSkipHits, tc.wantSkipTotal)
			}
		})
	}
}

// A missing action label must count as "did not act", not as zero confidence
// that happens to compare favourably.
func TestSweepGroupTreatsAMissingLabelAsNoAction(t *testing.T) {
	missing := evalRepeat{label: "true", labelProb: 0.9, actionProb: 0.99, actionFound: false}
	results := []evalCaseResult{resultFor("a1", "true", "clear", missing)}

	hits, total := sweepGroup(results, "true", 0.5, true)
	if hits != 0 || total != 1 {
		t.Errorf("should-act = %d/%d, want 0/1 — actionFound=false means the label was absent", hits, total)
	}
}

func TestReportSweepNamesTheSeparatingThresholds(t *testing.T) {
	file := evalCaseFile{Name: "n", ActionLabel: "true"}
	results := []evalCaseResult{
		resultFor("a1", "true", "clear", repeatFor("true", 0.99, 0.99)),
		resultFor("s1", "false", "clear", repeatFor("false", 0.99, 0.01)),
	}

	var b bytes.Buffer
	reportSweep(&b, file, results)
	out := b.String()

	if !strings.Contains(out, "SHOULD-ACT") {
		t.Errorf("sweep output is missing its header:\n%s", out)
	}
	if !strings.Contains(out, "separating thresholds: 0.30, 0.35, 0.40") {
		t.Errorf("sweep output did not report the separating run:\n%s", out)
	}
}

func TestReportSweepReportsNoSeparation(t *testing.T) {
	file := evalCaseFile{Name: "n", ActionLabel: "true"}
	// Both halves clear every threshold: nothing separates them.
	results := []evalCaseResult{
		resultFor("a1", "true", "clear", repeatFor("true", 0.99, 0.99)),
		resultFor("s1", "false", "clear", repeatFor("true", 0.1, 0.99)),
	}

	var b bytes.Buffer
	reportSweep(&b, file, results)
	if !strings.Contains(b.String(), "no swept threshold separates the two sets") {
		t.Errorf("sweep output claimed a separation that does not exist:\n%s", b.String())
	}
}

func TestStabilityHelpers(t *testing.T) {
	t.Run("confidence bounds and drift", func(t *testing.T) {
		stable := resultFor("c", "true", "clear",
			evalRepeat{confidence: 0.90, hasConf: true, actionFound: true},
			evalRepeat{confidence: 0.93, hasConf: true, actionFound: true},
		)
		lo, hi, ok := confBounds(stable)
		if !ok || math.Abs(lo-0.90) > 1e-9 || math.Abs(hi-0.93) > 1e-9 {
			t.Errorf("confBounds = %v..%v (ok=%v), want 0.90..0.93", lo, hi, ok)
		}
		if d := confDrift(stable); math.Abs(d-0.03) > 1e-9 {
			t.Errorf("confDrift = %v, want 0.03", d)
		}
	})

	t.Run("a noul has no confidence to drift", func(t *testing.T) {
		noul := resultFor("n", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("false", 0.1, 0.1))
		if _, _, ok := confBounds(noul); ok {
			t.Error("confBounds reported a confidence for a noul answer")
		}
		if d := confDrift(noul); d != 0 {
			t.Errorf("confDrift = %v, want 0", d)
		}
	})
}

func TestIdenticalEverywhere(t *testing.T) {
	tests := []struct {
		name    string
		results []evalCaseResult
		want    bool
	}{
		{
			name: "every repeat identical — also what a cached response looks like",
			results: []evalCaseResult{
				resultFor("a", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("true", 0.9, 0.9)),
			},
			want: true,
		},
		{
			name: "a label moved",
			results: []evalCaseResult{
				resultFor("a", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("false", 0.1, 0.1)),
			},
			want: false,
		},
		{
			name: "a probability moved",
			results: []evalCaseResult{
				resultFor("a", "true", "clear", repeatFor("true", 0.90, 0.9), repeatFor("true", 0.91, 0.9)),
			},
			want: false,
		},
		{
			name:    "a single repeat proves nothing",
			results: []evalCaseResult{resultFor("a", "true", "clear", repeatFor("true", 0.9, 0.9))},
			want:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := identicalEverywhere(tc.results); got != tc.want {
				t.Errorf("identicalEverywhere = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMostCommonIsDeterministicOnATie(t *testing.T) {
	// A tie must not render differently between runs: the report is meant to
	// be diffable.
	for i := 0; i < 20; i++ {
		if got := mostCommon([]string{"beta", "alpha"}); got != "alpha" {
			t.Fatalf("mostCommon tie = %q, want the lexicographically first label", got)
		}
	}
	if got := mostCommon([]string{"a", "b", "b"}); got != "b" {
		t.Errorf("mostCommon = %q, want b", got)
	}
}

func TestRenderReportCoversEverySection(t *testing.T) {
	file := evalCaseFile{
		Name:        "sample",
		Notes:       "synthetic",
		ActionLabel: "true",
		Question:    evalQuestion{Type: string(jev.QuestionNoul), Instructions: "Notify?"},
	}
	results := []evalCaseResult{
		resultFor("notify-1", "true", "clear",
			evalRepeat{label: "true", labelProb: 0.95, actionProb: 0.95, actionFound: true,
				latency: 400 * time.Millisecond, inputTokens: 320, outputTokens: 12},
			evalRepeat{label: "true", labelProb: 0.94, actionProb: 0.94, actionFound: true,
				latency: 420 * time.Millisecond, inputTokens: 322, outputTokens: 12},
		),
		resultFor("silent-1", "false", "ambiguous",
			evalRepeat{label: "false", labelProb: 0.80, actionProb: 0.20, actionFound: true,
				latency: 380 * time.Millisecond, inputTokens: 310, outputTokens: 11},
			evalRepeat{label: "false", labelProb: 0.78, actionProb: 0.22, actionFound: true,
				latency: 390 * time.Millisecond, inputTokens: 311, outputTokens: 11},
		),
	}

	out := renderReport(file, "testdata/sample.json", "jev-1.13.0", 2, 4, results)

	for _, want := range []string{
		"=== sample (testdata/sample.json) ===",
		`model=jev-1.13.0 cases=2 repeats=2 concurrency=4 action_label="true"`,
		"notes: synthetic",
		"PER CASE",
		"CASE", "GROUP", "WANT", "GOT", "AGREE", "P(WANT)", "P(ACTION)", "CONF", "LATENCY",
		"notify-1", "silent-1",
		"STABILITY",
		"identical across all 2 repeats",
		"COST",
		"4 calls: input", // 320+322+310+311
		"THRESHOLD SWEEP",
		"SHOULD-ACT", "SHOULD-NOT-ACT", "SEPARATION",
		"1 cases should act, 1 should not",
		"0.90", // the separating threshold this data should expose
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q\n--- report ---\n%s", want, out)
		}
	}

	// A noul question has no confidence, so the column must say so rather
	// than print a misleading 0.00.
	if !strings.Contains(out, "n/a") {
		t.Errorf("report did not mark the absent confidence for a noul answer:\n%s", out)
	}
	// The cost line must sum all four calls, not just one case's.
	if !strings.Contains(out, "1263") {
		t.Errorf("report did not total the input tokens:\n%s", out)
	}
}

// The order of the sections is the order a reader needs them in: what
// happened, whether it is repeatable, what it cost, then what to do about it.
func TestRenderReportSectionOrder(t *testing.T) {
	file := evalCaseFile{Name: "n", ActionLabel: "true", Question: evalQuestion{Type: string(jev.QuestionNoul)}}
	out := renderReport(file, "p", "m", 2, 1, []evalCaseResult{
		resultFor("a", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("true", 0.9, 0.9)),
		resultFor("b", "false", "clear", repeatFor("false", 0.9, 0.1), repeatFor("false", 0.9, 0.1)),
	})

	order := []string{"PER CASE", "STABILITY", "COST", "THRESHOLD SWEEP"}
	previous := -1
	for _, section := range order {
		at := strings.Index(out, section)
		if at < 0 {
			t.Fatalf("report is missing the %q section:\n%s", section, out)
		}
		if at < previous {
			t.Errorf("section %q appears out of order:\n%s", section, out)
		}
		previous = at
	}
}

// A noul answer carries no confidence, so its drift is vacuously zero. The
// stability verdict must still fail on labels that flap — reporting "✓ stable"
// for a case that answered true then false is how a flapping gate gets
// trusted.
func TestStabilityVerdictDoesNotRelyOnConfidenceAlone(t *testing.T) {
	flapping := []evalCaseResult{
		resultFor("flaps", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("false", 0.1, 0.1)),
	}

	var b bytes.Buffer
	reportStability(&b, flapping, 2)
	out := b.String()

	if strings.Contains(out, "✓") {
		t.Errorf("stability reported success for a case that changed label:\n%s", out)
	}
	if !strings.Contains(out, "changed label between identical calls") {
		t.Errorf("stability did not name the problem:\n%s", out)
	}
	if !strings.Contains(out, "not carried by these answers") {
		t.Errorf("stability did not say the confidence was absent rather than zero:\n%s", out)
	}
}

func TestStabilityVerdictPassesOnAgreement(t *testing.T) {
	steady := []evalCaseResult{
		resultFor("a", "true", "clear", repeatFor("true", 0.95, 0.95), repeatFor("true", 0.94, 0.94)),
		resultFor("b", "false", "clear", repeatFor("false", 0.9, 0.1), repeatFor("false", 0.91, 0.09)),
	}

	var b bytes.Buffer
	reportStability(&b, steady, 2)
	if out := b.String(); !strings.Contains(out, "✓ labels held") {
		t.Errorf("stability did not confirm a steady set:\n%s", out)
	}
}

func TestValidateEvalFileRejectsBrokenCaseSets(t *testing.T) {
	validQuestion := evalQuestion{
		Type:         string(jev.QuestionChoice),
		Instructions: "Which is it?",
		Options:      map[string]string{"a": "", "other": ""},
	}
	validCases := []evalCase{
		{ID: "c1", Group: "clear", Expect: "a", State: []byte(`"x"`)},
		{ID: "c2", Group: "clear", Expect: "other", State: []byte(`"y"`)},
	}
	base := func() evalCaseFile {
		return evalCaseFile{
			Name:        "n",
			ActionLabel: "other",
			Question:    validQuestion,
			Cases:       append([]evalCase{}, validCases...),
		}
	}

	if err := validateEvalFile(base()); err != nil {
		t.Fatalf("the baseline case file is invalid: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*evalCaseFile)
		want   string
	}{
		{name: "no name", mutate: func(f *evalCaseFile) { f.Name = "" }, want: "name is required"},
		{name: "no cases", mutate: func(f *evalCaseFile) { f.Cases = nil }, want: "one side"},
		{
			name:   "action label the question cannot answer",
			mutate: func(f *evalCaseFile) { f.ActionLabel = "nope" },
			want:   "not a label this question can answer",
		},
		{
			name:   "expect label the question cannot answer",
			mutate: func(f *evalCaseFile) { f.Cases[0].Expect = "nope" },
			want:   "not a label this question can answer",
		},
		{
			name:   "duplicate case id",
			mutate: func(f *evalCaseFile) { f.Cases[1].ID = f.Cases[0].ID },
			want:   "duplicate case id",
		},
		{
			name:   "missing group",
			mutate: func(f *evalCaseFile) { f.Cases[0].Group = "" },
			want:   "has no group",
		},
		{
			name:   "missing state",
			mutate: func(f *evalCaseFile) { f.Cases[0].State = nil },
			want:   "has no state",
		},
		{
			name:   "all cases on one side of the action label",
			mutate: func(f *evalCaseFile) { f.Cases[0].Expect = "other" },
			want:   "one side",
		},
		{
			name: "score questions have no labels to sweep",
			mutate: func(f *evalCaseFile) {
				f.Question = evalQuestion{Type: string(jev.QuestionScore), Instructions: "How bad?", Levels: []string{"low", "high"}}
				f.Cases = []evalCase{{ID: "c1", Group: "g", Expect: "low", State: []byte(`"x"`)}}
			},
			want: "needs a labelled answer",
		},
		{
			name: "unknown question type",
			mutate: func(f *evalCaseFile) {
				f.Question = evalQuestion{Type: "vibes", Instructions: "?"}
			},
			want: "unknown question type",
		},
		{
			name: "blank instructions",
			mutate: func(f *evalCaseFile) {
				f.Question.Instructions = "  "
			},
			want: "instructions is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := base()
			tc.mutate(&file)
			err := validateEvalFile(file)
			if err == nil {
				t.Fatal("validateEvalFile = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A case whose p(action) spread spans a candidate threshold is invisible in
// the sweep's totals — "21/30 acted" never says which case wavered — and it is
// the one that decides whether a gate feels reliable.
func TestReportBoundaryNamesStraddlingCases(t *testing.T) {
	file := evalCaseFile{Name: "n", ActionLabel: "true"}

	t.Run("a straddling case is named with the thresholds it crosses", func(t *testing.T) {
		results := []evalCaseResult{
			resultFor("steady", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("true", 0.9, 0.9)),
			resultFor("wavers", "true", "clear", repeatFor("true", 0.72, 0.72), repeatFor("true", 0.78, 0.78)),
		}
		var b bytes.Buffer
		reportBoundary(&b, file, results)
		out := b.String()

		if !strings.Contains(out, "BOUNDARY") || !strings.Contains(out, "wavers") {
			t.Errorf("boundary section did not name the straddling case:\n%s", out)
		}
		if strings.Contains(out, "steady") {
			t.Errorf("boundary section listed a case that never moved:\n%s", out)
		}
		if !strings.Contains(out, "0.75") {
			t.Errorf("boundary section is missing the crossed threshold 0.75:\n%s", out)
		}
		// Header and rows must share one alignment pass; a raw tab in the
		// rendered output means the header bypassed the tabwriter.
		if strings.Contains(out, "\t") {
			t.Errorf("boundary section contains an unaligned raw tab:\n%q", out)
		}
		// The reported range must sit under the header it belongs to.
		if !strings.Contains(out, "P(true)") {
			t.Errorf("boundary header does not name the action label:\n%s", out)
		}
	})

	t.Run("a case that moved without crossing a threshold is not listed", func(t *testing.T) {
		results := []evalCaseResult{
			resultFor("wobbles", "true", "clear", repeatFor("true", 0.10, 0.10), repeatFor("true", 0.12, 0.12)),
		}
		var b bytes.Buffer
		reportBoundary(&b, file, results)
		if out := b.String(); strings.Contains(out, "BOUNDARY") {
			t.Errorf("boundary section appeared for movement that crosses nothing:\n%s", out)
		}
	})

	t.Run("a single-valued case is not listed", func(t *testing.T) {
		results := []evalCaseResult{
			resultFor("steady", "true", "clear", repeatFor("true", 0.9, 0.9), repeatFor("true", 0.9, 0.9)),
		}
		var b bytes.Buffer
		reportBoundary(&b, file, results)
		if out := b.String(); strings.Contains(out, "BOUNDARY") {
			t.Errorf("boundary section appeared with nothing to report:\n%s", out)
		}
	})

	t.Run("an answer that never carried the label is not a boundary", func(t *testing.T) {
		absent := evalRepeat{label: "false", labelProb: 0.9, actionFound: false}
		results := []evalCaseResult{resultFor("missing", "false", "clear", absent, absent)}
		var b bytes.Buffer
		reportBoundary(&b, file, results)
		if out := b.String(); strings.Contains(out, "BOUNDARY") {
			t.Errorf("boundary section treated an absent label as a straddle:\n%s", out)
		}
	})
}

func TestActionProbRange(t *testing.T) {
	moved := resultFor("a", "true", "clear", repeatFor("true", 0.72, 0.72), repeatFor("true", 0.78, 0.78))
	if got := actionProbRange(moved); got != "0.72..0.78" {
		t.Errorf("actionProbRange = %q, want 0.72..0.78", got)
	}

	absent := resultFor("a", "true", "clear", evalRepeat{label: "true", actionFound: false})
	if got := actionProbRange(absent); got != "n/a" {
		t.Errorf("actionProbRange = %q, want n/a", got)
	}
}

// A pair is the same judgment in two forms, so the report's job is the gap
// between them — a gap that a per-case table hides by showing each form as
// its own tidy number.
func TestReportPairsComparesBothForms(t *testing.T) {
	t.Run("forms that read alike", func(t *testing.T) {
		results := []evalCaseResult{
			pairedResult("disk-full-en", "disk-full", "en", "true", repeatFor("true", 0.95, 0.95), repeatFor("true", 0.95, 0.95)),
			pairedResult("disk-full-zh", "disk-full", "zh", "true", repeatFor("true", 0.94, 0.94), repeatFor("true", 0.94, 0.94)),
		}
		var b bytes.Buffer
		reportPairs(&b, results)
		out := b.String()

		if !strings.Contains(out, "PAIRS") || !strings.Contains(out, "disk-full") {
			t.Errorf("pairs section did not report the pair:\n%s", out)
		}
		if strings.Contains(out, "\t") {
			t.Errorf("pairs section contains an unaligned raw tab:\n%q", out)
		}
		if !strings.Contains(out, "✓ the forms read alike") {
			t.Errorf("pairs section did not confirm the agreement:\n%s", out)
		}
	})

	t.Run("forms whose labels split", func(t *testing.T) {
		results := []evalCaseResult{
			pairedResult("x-en", "x", "en", "true", repeatFor("true", 0.9, 0.9)),
			pairedResult("x-zh", "x", "zh", "false", repeatFor("false", 0.6, 0.4)),
		}
		var b bytes.Buffer
		reportPairs(&b, results)
		out := b.String()
		if !strings.Contains(out, "different labels") {
			t.Errorf("pairs section missed a label split:\n%s", out)
		}
		// A split is the argmax differing, not proof that no gate can serve
		// both forms: a threshold above the higher form still decides them
		// alike. The report must not overclaim.
		if strings.Contains(out, "no threshold serves both") {
			t.Errorf("pairs section claimed no threshold can serve a split pair:\n%s", out)
		}
		if !strings.Contains(out, "threshold above the highest form still decides them alike") {
			t.Errorf("pairs section did not explain how a split is still serviceable:\n%s", out)
		}
	})

	t.Run("labels agree but the gap is wide", func(t *testing.T) {
		results := []evalCaseResult{
			pairedResult("y-en", "y", "en", "true", repeatFor("true", 0.99, 0.99)),
			pairedResult("y-zh", "y", "zh", "true", repeatFor("true", 0.80, 0.80)),
		}
		var b bytes.Buffer
		reportPairs(&b, results)
		out := b.String()
		if !strings.Contains(out, "spread exceeds the drift tolerance") {
			t.Errorf("pairs section missed a wide spread between agreeing labels:\n%s", out)
		}
	})

	t.Run("three forms report one spread across all of them", func(t *testing.T) {
		results := []evalCaseResult{
			pairedResult("z-en", "z", "en", "true", repeatFor("true", 0.93, 0.93)),
			pairedResult("z-zh-formal", "z", "zh-formal", "true", repeatFor("true", 0.94, 0.94)),
			pairedResult("z-zh-plain", "z", "zh-plain", "true", repeatFor("true", 0.84, 0.84)),
		}
		var b bytes.Buffer
		reportPairs(&b, results)
		out := b.String()

		// The spread must span every form, not just the first two: 0.93..0.84.
		if !strings.Contains(out, "0.100") {
			t.Errorf("pairs section did not spread across all three forms:\n%s", out)
		}
		for _, form := range []string{"en", "zh-formal", "zh-plain"} {
			if !strings.Contains(out, form) {
				t.Errorf("pairs section is missing form %q:\n%s", form, out)
			}
		}
	})

	t.Run("no pairs, no section", func(t *testing.T) {
		results := []evalCaseResult{resultFor("lone", "true", "clear", repeatFor("true", 0.9, 0.9))}
		var b bytes.Buffer
		reportPairs(&b, results)
		if out := b.String(); out != "" {
			t.Errorf("pairs section appeared without any pairs:\n%s", out)
		}
	})
}

func TestValidateEvalFileRejectsBrokenPairs(t *testing.T) {
	base := func() evalCaseFile {
		return evalCaseFile{
			Name:        "n",
			ActionLabel: "true",
			Question:    evalQuestion{Type: string(jev.QuestionNoul), Instructions: "?"},
			Cases: []evalCase{
				{ID: "a-en", Group: "en", Pair: "a", Expect: "true", State: []byte(`"x"`)},
				{ID: "a-zh", Group: "zh", Pair: "a", Expect: "true", State: []byte(`"y"`)},
				{ID: "b", Group: "en", Expect: "false", State: []byte(`"z"`)},
			},
		}
	}
	if err := validateEvalFile(base()); err != nil {
		t.Fatalf("the baseline case file is invalid: %v", err)
	}

	// Three forms is what a controlled experiment needs: vary the dimension
	// under test, hold the content fixed.
	t.Run("three members are allowed", func(t *testing.T) {
		file := base()
		file.Cases = append(file.Cases, evalCase{ID: "a-plain", Group: "zh-plain", Pair: "a", Expect: "true", State: []byte(`"w"`)})
		if err := validateEvalFile(file); err != nil {
			t.Errorf("validateEvalFile = %v, want a three-form pair to be accepted", err)
		}
	})

	t.Run("a lone member", func(t *testing.T) {
		file := base()
		file.Cases[0].Pair = "solo"
		err := validateEvalFile(file)
		if err == nil || !strings.Contains(err.Error(), "want at least 2") {
			t.Errorf("error = %v, want it to reject a pair with one member", err)
		}
	})

	// A pair is the SAME judgment in two forms: two members expecting
	// different labels is an authoring bug, not a measurement result.
	t.Run("members expecting different labels", func(t *testing.T) {
		file := base()
		file.Cases[1].Expect = "false"
		err := validateEvalFile(file)
		if err == nil || !strings.Contains(err.Error(), "authoring error") {
			t.Errorf("error = %v, want it to reject a pair whose members disagree on the expected label", err)
		}
	})
}

func pairedResult(id, pair, group, expect string, repeats ...evalRepeat) evalCaseResult {
	return evalCaseResult{
		evalCase: evalCase{ID: id, Pair: pair, Group: group, Expect: expect, State: []byte(`"s"`)},
		repeats:  repeats,
	}
}
