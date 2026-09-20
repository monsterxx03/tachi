package jev_test

// Batching equivalence for the tool-search gate.
//
// The gate sends ONE request carrying every candidate tool as its own question,
// while the case file — and so the swept threshold — measures ONE question per
// request. The service documents the questions as independent ("run in
// parallel, and cannot see one another's answers"), but that is a claim about
// the API, not a measurement: if the probabilities shift when a tool shares its
// request with twenty others, the threshold was swept for a prompt the gate
// never sends.
//
// So this file measures both shapes over the same (request, tool) pairs and
// prints the difference. It asserts nothing about the size of the difference —
// it reports, the way the rest of the harness does — and skips without a key.
//
// Run: JEV_API_KEY=... go test ./pkg/jev/ -run TestToolSearchBatchVsSolo -v

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/jev"
)

// batchQueryLimit keeps the run cheap: each query costs one batched request
// plus one per tool, so a handful of queries is enough to see a shift.
const batchQueryLimit = 6

// batchDeltaFloor is the difference below which the two shapes are called the
// same for the purpose a threshold cares about — the scale a 0.05 sweep step
// moves on. It is a reporting aid, not a pass/fail line.
const batchDeltaFloor = 0.05

type batchRow struct {
	query   string
	tool    string
	batched float64
	solo    float64
}

func TestToolSearchBatchVsSolo(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv(evalKeyEnv))
	if apiKey == "" {
		t.Skipf("%s is not set — this measures the service, so it needs a key", evalKeyEnv)
	}
	corpus := loadJSON[toolCorpus](t, filepath.Join(toolSearchDir, "corpus.json"))
	queries := loadJSON[toolQuerySet](t, filepath.Join(toolSearchDir, "queries.json"))

	client, err := jev.NewClient(apiKey)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	batched := allQuestions(corpus)

	// The queries to check: every axis the language and the arm vary, so a
	// shift that only appears in one of them is visible.
	checked := pickBatchQueries(queries, batchQueryLimit)

	var rows []batchRow
	var worst []batchRow
	var soloLatency []time.Duration
	batchedInput, soloInput := 0, 0
	var batchedLatency time.Duration
	for _, q := range checked {
		ctx, cancel := context.WithTimeout(t.Context(), evalCaseTimeout)
		start := time.Now()
		resp, err := client.Evaluate(ctx, q, batched)
		batchedLatency += time.Since(start)
		cancel()
		if err != nil {
			t.Fatalf("batched request for %q: %v", q, err)
		}
		batchedInput += resp.Usage.InputTokens

		for _, tool := range corpus.Tools {
			// The SAME per-candidate text as the batched request, alone in its
			// own call: the only thing that varies between the two shapes is
			// whether the question shared its request. Asking the file-level
			// frame here instead would measure a different question entirely —
			// and a question with no tool in it gets a diffuse ~0.45 on every
			// tool, which is the shape of "no signal" rather than a comparison.
			ctx, cancel := context.WithTimeout(t.Context(), evalCaseTimeout)
			start := time.Now()
			one, err := client.Evaluate(ctx, q, map[string]jev.Question{tool.Name: batched[tool.Name]})
			cancel()
			if err != nil {
				t.Fatalf("solo request for %q/%s: %v", q, tool.Name, err)
			}
			soloInput += one.Usage.InputTokens
			soloLatency = append(soloLatency, time.Since(start))

			batchedP, okB := probability(resp, tool.Name)
			soloP, okS := probability(one, tool.Name)
			if !okB || !okS {
				t.Errorf("%q/%s: an answer is missing (batched ok=%v solo ok=%v)", q, tool.Name, okB, okS)
				continue
			}
			r := batchRow{query: q, tool: tool.Name, batched: batchedP, solo: soloP}
			rows = append(rows, r)
			if delta(r) > batchDeltaFloor {
				worst = append(worst, r)
			}
		}
	}

	sort.Slice(worst, func(i, j int) bool { return delta(worst[i]) > delta(worst[j]) })
	maxDelta, meanDelta := 0.0, 0.0
	for _, r := range rows {
		d := delta(r)
		if d > maxDelta {
			maxDelta = d
		}
		meanDelta += d
	}
	if len(rows) > 0 {
		meanDelta /= float64(len(rows))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== tool-search: batched vs solo (per (request, tool) pair) ===\n")
	fmt.Fprintf(&b, "queries=%d tools=%d pairs=%d\n", len(checked), len(corpus.Tools), len(rows))
	fmt.Fprintf(&b, "p(true): |batched - solo|  max=%.3f  mean=%.3f\n", maxDelta, meanDelta)
	fmt.Fprintf(&b, "cost: batched %d input tokens in %d requests; solo %d in %d (a gate pays the batched one)\n",
		batchedInput, len(checked), soloInput, len(rows))
	// The gate's request is on the critical path of a model turn, so whether one
	// request with N questions is slower than one with a single question is a
	// design fact, not a curiosity: it decides the search budget.
	fmt.Fprintf(&b, "latency: batched %s mean over %d requests carrying %d questions each; solo %s mean\n",
		(batchedLatency / time.Duration(len(checked))).Round(time.Millisecond), len(checked), len(corpus.Tools),
		meanLatencyOf(soloLatency))
	if len(worst) == 0 {
		fmt.Fprintf(&b, "no pair moved more than %.2f — the solo-swept threshold transfers\n", batchDeltaFloor)
	} else {
		fmt.Fprintf(&b, "pairs that moved more than %.2f:\n", batchDeltaFloor)
		fmt.Fprintf(&b, "  %-46s %-34s %8s %8s %8s\n", "QUERY", "TOOL", "BATCHED", "SOLO", "DELTA")
		for _, r := range worst {
			fmt.Fprintf(&b, "  %-46s %-34s %8.3f %8.3f %8.3f\n", r.query, r.tool, r.batched, r.solo, delta(r))
		}
	}
	t.Log(b.String())
}

// meanLatencyOf is the mean of the collected solo call latencies.
func meanLatencyOf(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	var sum time.Duration
	for _, v := range d {
		sum += v
	}
	return (sum / time.Duration(len(d))).Round(time.Millisecond)
}

func delta(r batchRow) float64 {
	if r.batched > r.solo {
		return r.batched - r.solo
	}
	return r.solo - r.batched
}

// allQuestions is the question set the gate sends: one per candidate tool,
// keyed by the tool's own name.
// The per-candidate text is the SAME one the generated case file uses.
func allQuestions(corpus toolCorpus) map[string]jev.Question {
	out := make(map[string]jev.Question, len(corpus.Tools))
	for _, tool := range corpus.Tools {
		out[tool.Name] = mustBuildNoul(
			fmt.Sprintf(toolSearchQuestionFmt, tool.Name, tool.Server, tool.Hint, tool.Summary),
			toolSearchYes, toolSearchNo)
	}
	return out
}

func mustBuildNoul(instructions, yes, no string) jev.Question {
	q, err := (evalQuestion{Type: "noul", Instructions: instructions, Yes: yes, No: no}).build()
	if err != nil {
		panic(err)
	}
	return q
}

// probability reads the noul answer for one question key.
func probability(resp *jev.Response, key string) (float64, bool) {
	a, ok := resp.Answers[key]
	if !ok || a.Noul == nil {
		return 0, false
	}
	return *a.Noul, true
}

// pickBatchQueries takes a spread of the authored queries — alternating
// language across the matched intents, plus one no-tool query per language.
// The no-tool arm is where a shift matters most: a false positive there is a
// wrong tool offered to the model.
func pickBatchQueries(q toolQuerySet, limit int) []string {
	var out []string
	for i, intent := range q.Intents {
		if len(out) >= limit-2 {
			break
		}
		if i%2 == 0 {
			out = append(out, intent.ZH)
		} else {
			out = append(out, intent.EN)
		}
	}
	for i, intent := range q.NoTool {
		if i >= 2 {
			break
		}
		if i%2 == 0 {
			out = append(out, intent.ZH)
		} else {
			out = append(out, intent.EN)
		}
	}
	return out
}
