package jev_test

// Pool-mode sweep: the tool-search gate measured in the shape it actually runs.
//
// The case-file harness asks ONE question per request, so it can only price the
// (query, tool) pairs an author NAMED. Production asks every candidate — over a
// real pool that is ~184 questions, split into requests of
// jevSearchQuestionsPerChunk — and the failures that matter there are the ones a
// hand-written distractor list never contained: an unrelated tool that happens
// to share vocabulary with the request. A threshold picked off the named pairs
// alone is a threshold picked without looking at most of what decides a search.
//
// So this file asks the pool the way the gate does and reports:
//
//   - the corridor — named pairs, should-act against should-not-act,
//   - the UNLABELLED surface — candidates no intent named — which is where a
//     threshold actually earns its keep,
//   - the no-tool arm's leak rate: how often a search that has no right answer
//     offered something anyway,
//   - the zh/en spread and the repeat stability, the two things that walk a case
//     across a cut-off without any code changing.
//
// It asserts nothing about the numbers (like the rest of the harness) and skips
// without a key.
//
//	# the committed fixture
//	JEV_API_KEY=... go test ./pkg/jev/ -run TestToolSearchPoolSweep -v -timeout 40m
//
//	# a real pool: dump it first (agent/mcp/pool_dump_test.go), then sweep it
//	JEV_API_KEY=... TOOL_SEARCH_CORPUS=testdata/tool-search/local/corpus.json \
//	  TOOL_SEARCH_QUERIES=testdata/tool-search/local/queries.json \
//	  go test ./pkg/jev/ -run TestToolSearchPoolSweep -v -timeout 40m
//
// Environment:
//
//	JEV_API_KEY        API key. Unset → skip.
//	TOOL_SEARCH_CORPUS Corpus file (default testdata/tool-search/corpus.json).
//	TOOL_SEARCH_QUERIES Intents file (default testdata/tool-search/queries.json).
//	JEV_EVAL_REPEATS   Repeats per query (default 5; see the eval harness).
//	JEV_EVAL_MODEL     Model override, for re-measuring against new weights.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/monsterxx03/tachi/pkg/jev"
)

const (
	poolCorpusEnv  = "TOOL_SEARCH_CORPUS"
	poolQueriesEnv = "TOOL_SEARCH_QUERIES"

	defaultPoolCorpus  = "testdata/tool-search/corpus.json"
	defaultPoolQueries = "testdata/tool-search/queries.json"

	// prodChunkQuestions is how many candidates one request carries, mirroring
	// agent/mcp's jevSearchQuestionsPerChunk (jevSearchChunkTokens /
	// jevSearchTokensPerQuestion = 8000/80). It is duplicated here rather than
	// imported because this harness must not depend on the production package —
	// the measurement is supposed to be able to outlive an implementation — and
	// a stale copy only means the sweep sends a shape production no longer
	// sends, which the A/B test at the bottom of this file prints for exactly
	// that reason.
	prodChunkQuestions = 100

	// poolConcurrency bounds in-flight requests. One query is one request per
	// chunk, and a run is ~92 queries × repeats: serial it is a coffee break,
	// and the service is asked for the same total either way.
	poolConcurrency = 4

	// poolUnlabelledPrintFloor is the mean probability above which an unnamed
	// (query, tool) pair is listed for review. A high-scoring unnamed pair is
	// not automatically a false positive — it is the shape a MISSING label
	// takes, and the only way to tell the two apart is to look.
	poolUnlabelledPrintFloor = 0.40

	// poolLanguagePrintFloor is the zh/en spread above which a (intent, tool)
	// pair gets a row in the language table. It is a display floor, not a
	// verdict: the summary line counts every pair, and this only keeps the
	// table to the rows that explain it.
	poolLanguagePrintFloor = 0.20
)

// poolThresholds are the candidate cut-offs. The low end matters: the fixture
// sweep put tool search's corridor at 0.215 < τ ≤ 0.470, and a gate that misses
// costs a whole re-search turn while a gate that over-offers costs one extra
// candidate the model sees.
var poolThresholds = []float64{0.15, 0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60}

// poolLabel is what the authored intents say about one candidate.
type poolLabel string

const (
	labelExpect   poolLabel = "expect"
	labelDistract poolLabel = "distract"
	// labelUnnamed is every tool no intent mentioned. It is not "should not
	// act" — the labels are incomplete by construction (a real pool is far
	// larger than any intent list), so an unnamed pair is unknown, and the
	// report keeps it in its own column instead of folding it into either side.
	labelUnnamed poolLabel = ""
)

// poolQuery is one authored request, judged against the whole pool.
type poolQuery struct {
	Intent string
	Lang   string
	Text   string
	Labels map[string]poolLabel // every corpus tool, named or not
	// NoTool marks the arm where the right answer is "nothing here fits": the
	// tools named by such an intent are ones that must NOT be offered.
	NoTool bool
}

// poolRun is one query measured over all repeats and chunks.
type poolRun struct {
	q           poolQuery
	probs       map[string][]float64 // tool → p(true), one entry per answered repeat
	inputTokens int
	latency     time.Duration
	err         error
}

// TestToolSearchPoolSweep is the measurement: every candidate judged for every
// authored request, in production's chunk shape.
func TestToolSearchPoolSweep(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv(evalKeyEnv))
	if apiKey == "" {
		t.Skipf("%s is not set — this measures the service, so it needs a key", evalKeyEnv)
	}
	corpusPath := envOrDefault(poolCorpusEnv, defaultPoolCorpus)
	queriesPath := envOrDefault(poolQueriesEnv, defaultPoolQueries)
	corpus := loadJSON[toolCorpus](t, corpusPath)
	queries := loadJSON[toolQuerySet](t, queriesPath)
	qs := buildPoolQueries(t, corpus, queries)

	client, err := jev.NewClient(apiKey)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	repeats := evalRepeats(t)
	chunks := poolChunks(t, corpus)

	runs := make([]poolRun, len(qs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, poolConcurrency)
	for i := range qs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			runs[i] = runPoolQuery(t.Context(), client, qs[i], chunks, repeats)
		})
	}
	wg.Wait()

	for _, r := range runs {
		if r.err != nil {
			t.Errorf("query %q (%s): %v", r.q.Text, r.q.Intent, r.err)
		}
	}
	t.Log(renderPoolReport(corpusPath, queriesPath, client.Model(), repeats, corpus, runs))
	dumpPoolRun(t, corpusPath, queriesPath, client.Model(), repeats, corpus, runs)
}

// buildPoolQueries turns the authored intents into per-query label maps, one
// entry per corpus tool. A tool the intents do not mention is labelUnnamed, not
// a negative: naming it false would score the sweep against a claim the author
// never made.
func buildPoolQueries(t *testing.T, corpus toolCorpus, queries toolQuerySet) []poolQuery {
	t.Helper()
	inPool := make(map[string]bool, len(corpus.Tools))
	for _, tool := range corpus.Tools {
		inPool[tool.Name] = true
	}
	labelsFor := func(id string, expect, distract []string) map[string]poolLabel {
		labels := make(map[string]poolLabel, len(corpus.Tools))
		for _, tool := range corpus.Tools {
			labels[tool.Name] = labelUnnamed
		}
		for _, n := range expect {
			if !inPool[n] {
				t.Fatalf("intent %s expects %s, which the corpus does not define", id, n)
			}
			labels[n] = labelExpect
		}
		for _, n := range distract {
			if !inPool[n] {
				t.Fatalf("intent %s names %s as a distractor, which the corpus does not define", id, n)
			}
			if labels[n] == labelExpect {
				t.Fatalf("intent %s names %s in both arms", id, n)
			}
			labels[n] = labelDistract
		}
		return labels
	}

	var out []poolQuery
	add := func(it toolIntent, lang, text string, noTool bool) {
		out = append(out, poolQuery{
			Intent: it.ID, Lang: lang, Text: text,
			Labels: labelsFor(it.ID, it.Expect, it.Distract),
			NoTool: noTool,
		})
	}
	for _, it := range queries.Intents {
		add(it, "zh", it.ZH, false)
		add(it, "en", it.EN, false)
	}
	for _, it := range queries.NoTool {
		add(it, "zh", it.ZH, true)
		add(it, "en", it.EN, true)
	}
	return out
}

// poolChunks is the question set production would send: every tool as its own
// question, in name order, cut into requests of prodChunkQuestions. Name order
// is production's too (judgeQuery sorts before chunking) — it is what makes a
// chunk boundary reproducible.
func poolChunks(t *testing.T, corpus toolCorpus) []map[string]jev.Question {
	t.Helper()
	all := allQuestions(corpus)
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)

	var chunks []map[string]jev.Question
	for start := 0; start < len(names); start += prodChunkQuestions {
		end := min(start+prodChunkQuestions, len(names))
		chunk := make(map[string]jev.Question, end-start)
		for _, name := range names[start:end] {
			chunk[name] = all[name]
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// runPoolQuery asks one query against every chunk, `repeats` times.
func runPoolQuery(ctx context.Context, client *jev.Client, q poolQuery, chunks []map[string]jev.Question, repeats int) poolRun {
	run := poolRun{q: q, probs: make(map[string][]float64, len(q.Labels))}
	for i := range repeats {
		for _, chunk := range chunks {
			// Per-chunk timeout, not per-repeat: one slow request must not eat
			// the budget of the rest of this query's repeats.
			callCtx, cancel := context.WithTimeout(ctx, evalCaseTimeout)
			start := time.Now()
			resp, err := client.Evaluate(callCtx, q.Text, chunk)
			cancel()
			if err != nil {
				run.err = fmt.Errorf("repeat %d: %w", i, err)
				return run
			}
			run.latency += time.Since(start)
			run.inputTokens += resp.Usage.InputTokens
			for name := range chunk {
				p, ok := probability(resp, name)
				if !ok {
					// An unanswered question is not a zero — it is no answer,
					// and the gate would read it as "do not select". Dropping
					// it here is the same reading; counting it as 0 would make
					// a silent service look decisive.
					continue
				}
				run.probs[name] = append(run.probs[name], p)
			}
		}
	}
	return run
}

// envOrDefault is the file's only env helper: an unset knob means the committed
// fixture, so a bare `go test` run needs no path arguments at all.
func envOrDefault(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// --- report -----------------------------------------------------------------

func renderPoolReport(corpusPath, queriesPath, model string, repeats int, corpus toolCorpus, runs []poolRun) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "\n=== tool-search: whole-pool sweep ===\n")
	fmt.Fprintf(&b, "corpus=%s (%d tools)\nqueries=%s (%d requests: %d intents × zh/en + %d no-tool × zh/en)\n",
		corpusPath, len(corpus.Tools), queriesPath, len(runs), countIntents(runs), countNoToolIntents(runs))
	fmt.Fprintf(&b, "model=%s repeats=%d chunk=%d questions/request → %d request(s) per query\n",
		model, repeats, prodChunkQuestions, (len(corpus.Tools)+prodChunkQuestions-1)/prodChunkQuestions)

	poolReportCost(&b, runs)
	poolReportStability(&b, runs)
	poolReportSweep(&b, runs, repeats)
	poolReportNoTool(&b, runs)
	poolReportUnlabelled(&b, runs)
	poolReportQueries(&b, runs)
	poolReportLanguage(&b, runs)
	poolReportDecision(&b, runs, repeats)
	return b.String()
}

func countIntents(runs []poolRun) int {
	seen := map[string]bool{}
	for _, r := range runs {
		if !r.q.NoTool {
			seen[r.q.Intent] = true
		}
	}
	return len(seen)
}

func countNoToolIntents(runs []poolRun) int {
	seen := map[string]bool{}
	for _, r := range runs {
		if r.q.NoTool {
			seen[r.q.Intent] = true
		}
	}
	return len(seen)
}

// poolReportCost sums the run's spend and service time.
func poolReportCost(b *bytes.Buffer, runs []poolRun) {
	b.WriteString("\nCOST\n")
	var totalIn, maxIn int
	var latency time.Duration
	for _, r := range runs {
		totalIn += r.inputTokens
		latency += r.latency
		maxIn = max(maxIn, r.inputTokens)
	}
	// A query's token total is chunks × repeats; the per-REQUEST figure is what
	// the 32k ceiling applies to, so it is derived from the chunk count rather
	// than reported as if it were the whole query's.
	fmt.Fprintf(b, "  input %d tokens over %d requests (mean %d per request, max %d)\n",
		totalIn, len(runs), totalIn/max(1, len(runs)), maxIn)
	fmt.Fprintf(b, "  service time %s summed over requests (%d-way concurrent)\n",
		latency.Round(time.Millisecond), poolConcurrency)
}

// poolReportStability answers "does the same request produce the same numbers
// twice" for the pairs the labels cover.
func poolReportStability(b *bytes.Buffer, runs []poolRun) {
	b.WriteString("\nSTABILITY (labelled pairs, over repeats)\n")
	flips := 0
	pairs := 0
	maxSpread := 0.0
	worst := ""
	for _, r := range runs {
		for name, ps := range r.probs {
			if r.q.Labels[name] == labelUnnamed || len(ps) < 2 {
				continue
			}
			pairs++
			lo, hi := math.MaxFloat64, -math.MaxFloat64
			decisions := map[bool]bool{}
			for _, p := range ps {
				lo, hi = math.Min(lo, p), math.Max(hi, p)
				decisions[p >= 0.5] = true
			}
			if len(decisions) > 1 {
				flips++
			}
			if spread := hi - lo; spread > maxSpread {
				maxSpread, worst = spread, fmt.Sprintf("%s/%s", r.q.Intent, name)
			}
		}
	}
	if pairs == 0 {
		b.WriteString("  no labelled pair was answered twice\n")
		return
	}
	fmt.Fprintf(b, "  %d labelled (query, tool) pairs: %d flipped the 0.5 label between repeats, max p spread %.3f (%s)\n",
		pairs, flips, maxSpread, worst)
	if flips > 0 {
		fmt.Fprintf(b, "  ⚠ a pair that flips at 0.5 constrains the cut-off: keep it clear of every flipped pair's range\n")
	}
}

// poolReportSweep is the corridor, with the unlabelled surface beside it.
//
// Two numbers per row are decisive and neither is a hit rate: the unlabelled
// count per query (how much noise a search hands the model) and the no-tool
// leak rate (how often a request with no right answer got an answer). A
// threshold that looks perfect on named pairs can still be unusable if either
// is high.
func poolReportSweep(b *bytes.Buffer, runs []poolRun, repeats int) {
	b.WriteString("\nTHRESHOLD SWEEP\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "T\tEXPECT-HIT\tDISTRACT-HIT\tUNNAMED-HIT\tUNNAMED/QUERY\tNO-TOOL LEAK\tSEPARATION")

	type row struct {
		threshold                                                    float64
		exp, dis, unm, expTotal, disTotal, unmTotal, noTool, noToolN int
	}
	var rows []row
	for _, th := range poolThresholds {
		r := row{threshold: th}
		for _, run := range runs {
			for name, ps := range run.probs {
				for _, p := range ps {
					hit := p >= th
					switch run.q.Labels[name] {
					case labelExpect:
						r.expTotal++
						if hit {
							r.exp++
						}
					case labelDistract:
						r.disTotal++
						if hit {
							r.dis++
						}
					default:
						r.unmTotal++
						if hit {
							r.unm++
						}
					}
				}
			}
		}
		// The no-tool arm is counted per (query, repeat): one leaked tool is
		// already the failure, so counting per pair would dilute it by the pool
		// size.
		for _, run := range runs {
			if !run.q.NoTool {
				continue
			}
			for i := range repeats {
				r.noToolN++
				for _, ps := range run.probs {
					if i < len(ps) && ps[i] >= th {
						r.noTool++
						break
					}
				}
			}
		}
		rows = append(rows, r)
	}

	for _, r := range rows {
		// Unnamed hits are counted per (query, repeat) average: the pool is
		// large and every query asks all of it, so a per-pair rate would report
		// a number that shrinks simply because the pool grew.
		unnamedPerQuery := ratio(r.unm, max(1, len(runs)*repeats))
		fmt.Fprintf(w, "%.2f\t%s\t%s\t%s\t%.1f\t%s\t%+.2f\n", r.threshold,
			rate(r.exp, r.expTotal), rate(r.dis, r.disTotal), rate(r.unm, r.unmTotal),
			unnamedPerQuery, rate(r.noTool, r.noToolN),
			ratio(r.exp, r.expTotal)-ratio(r.dis, r.disTotal))
	}
	_ = w.Flush()

	fmt.Fprintf(b, "  named pairs: %d should-act, %d should-not-act, over %d request(s) each\n",
		rows[0].expTotal, rows[0].disTotal, repeats)
	fmt.Fprintf(b, "  unlabelled pairs: %d — reported, never counted as either side\n", rows[0].unmTotal)
}

func poolReportNoTool(b *bytes.Buffer, runs []poolRun) {
	var noTool []poolRun
	for _, r := range runs {
		if r.q.NoTool {
			noTool = append(noTool, r)
		}
	}
	if len(noTool) == 0 {
		return
	}
	b.WriteString("\nNO-TOOL ARM (the right answer is: nothing here fits)\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "QUERY\tTOP TOOL\tP\tNAMED-DISTRACTOR\tP")
	for _, r := range noTool {
		topName, topP := topOf(r, func(name string) bool { return true })
		disName, disP := topOf(r, func(name string) bool { return r.q.Labels[name] == labelDistract })
		fmt.Fprintf(w, "%s [%s]\t%s\t%.3f\t%s\t%.3f\n", r.q.Intent, r.q.Lang, topName, topP, disName, disP)
	}
	_ = w.Flush()
	b.WriteString("  the TOP TOOL column is the whole pool's best answer to a request nothing can serve —\n" +
		"  a number near the gate's cut-off there is the false positive a model would act on\n")
}

// poolReportUnlabelled lists the unnamed pairs that scored high enough to be
// worth a label decision. This is the section that finds authoring errors: on a
// real pool, most tools a request could talk about were never named.
func poolReportUnlabelled(b *bytes.Buffer, runs []poolRun) {
	type hit struct {
		intent, lang, tool string
		p                  float64
	}
	var hits []hit
	for _, r := range runs {
		for name, ps := range r.probs {
			if r.q.Labels[name] != labelUnnamed || len(ps) == 0 {
				continue
			}
			if p := mean(ps); p >= poolUnlabelledPrintFloor {
				hits = append(hits, hit{r.q.Intent, r.q.Lang, name, p})
			}
		}
	}
	if len(hits) == 0 {
		fmt.Fprintf(b, "\nUNLABELLED ≥ %.2f\n  none — every high scorer was a tool an intent named\n", poolUnlabelledPrintFloor)
		return
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].p != hits[j].p {
			return hits[i].p > hits[j].p
		}
		return hits[i].tool < hits[j].tool
	})
	fmt.Fprintf(b, "\nUNLABELLED ≥ %.2f (%d pairs — each is either a missing label or a false positive; the difference is a judgment call)\n",
		poolUnlabelledPrintFloor, len(hits))
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "QUERY\tLANG\tTOOL\tP")
	for _, h := range hits {
		fmt.Fprintf(w, "%s\t%s\t%s\t%.3f\n", h.intent, h.lang, h.tool, h.p)
	}
	_ = w.Flush()
}

// poolReportQueries is the per-request view: the best named answer, the best
// near-miss, and the best unnamed candidate, side by side. A row where the
// near-miss or the unnamed tool outranks the expectation is the row a threshold
// cannot fix.
func poolReportQueries(b *bytes.Buffer, runs []poolRun) {
	b.WriteString("\nPER REQUEST (mean p over repeats; ! = a non-expect outscored the best expect)\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "QUERY\tLANG\tBEST EXPECT\tP\tBEST DISTRACT\tP\tBEST UNNAMED\tP")
	for _, r := range runs {
		expName, expP := topOf(r, func(name string) bool { return r.q.Labels[name] == labelExpect })
		disName, disP := topOf(r, func(name string) bool { return r.q.Labels[name] == labelDistract })
		unmName, unmP := topOf(r, func(name string) bool { return r.q.Labels[name] == labelUnnamed })

		mark := ""
		if !r.q.NoTool && (disP > expP || unmP > expP) {
			mark = "!"
		}
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%.3f\t%s\t%.3f\t%s\t%.3f\n",
			r.q.Intent, mark, r.q.Lang, expName, expP, disName, disP, unmName, unmP)
	}
	_ = w.Flush()
}

// poolReportLanguage is the zh/en comparison, aggregated per intent: the same
// judgment asked twice in two languages. The fixture measurement found this
// spread WIDER than the corridor, which is the single most important thing to
// re-check on a real pool.
func poolReportLanguage(b *bytes.Buffer, runs []poolRun) {
	type pair struct{ zh, en map[string][]float64 }
	byIntent := map[string]*pair{}
	var order []string
	for _, r := range runs {
		if r.q.NoTool {
			continue
		}
		if byIntent[r.q.Intent] == nil {
			byIntent[r.q.Intent] = &pair{}
			order = append(order, r.q.Intent)
		}
		if r.q.Lang == "zh" {
			byIntent[r.q.Intent].zh = r.probs
		} else {
			byIntent[r.q.Intent].en = r.probs
		}
	}
	if len(order) == 0 {
		return
	}
	b.WriteString("\nLANGUAGE (mean p per tool, zh vs en, same intent)\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "INTENT\tTOOL\tZH\tEN\tSPREAD")
	var total, maxSpread float64
	compared, widest := 0, 0
	worstIntent := ""
	for _, id := range order {
		p := byIntent[id]
		names := make([]string, 0, len(p.zh))
		for name := range p.zh {
			names = append(names, name)
		}
		sort.Strings(names)
		printedIntent := false
		for _, name := range names {
			en, ok := p.en[name]
			if !ok {
				continue
			}
			zhP, enP := mean(p.zh[name]), mean(en)
			spread := math.Abs(zhP - enP)
			total += spread
			compared++
			if spread > maxSpread {
				maxSpread, worstIntent = spread, id
			}
			if spread <= poolLanguagePrintFloor {
				continue
			}
			widest++
			// The intent name rides on the first wide row only: repeating it
			// per tool would bury the table's own summary line.
			intentCol := ""
			if !printedIntent {
				intentCol, printedIntent = id, true
			}
			fmt.Fprintf(w, "%s\t%s\t%.3f\t%.3f\t%.3f\n", intentCol, name, zhP, enP, spread)
		}
	}
	_ = w.Flush()
	if compared == 0 {
		return
	}
	fmt.Fprintf(b, "  %d (intent, tool) pairs compared: mean spread %.3f, max %.3f (%s); %d pairs spread more than %.2f\n",
		compared, total/float64(compared), maxSpread, worstIntent, widest, poolLanguagePrintFloor)
}

// --- helpers ----------------------------------------------------------------

// topOf is the highest-probability tool matching a filter, by mean over repeats.
func topOf(r poolRun, keep func(name string) bool) (string, float64) {
	best, bestP := "", -1.0
	names := make([]string, 0, len(r.probs))
	for name := range r.probs {
		names = append(names, name)
	}
	sort.Strings(names) // ties resolve by name, so a report is reproducible
	for _, name := range names {
		if !keep(name) {
			continue
		}
		if p := mean(r.probs[name]); p > bestP {
			best, bestP = name, p
		}
	}
	if bestP < 0 {
		return "-", 0
	}
	return best, bestP
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// rate formats a hit rate with the counts behind it: a rate alone hides "0/0".
func rate(hits, total int) string {
	return fmt.Sprintf("%d/%d (%.2f)", hits, total, ratio(hits, total))
}

// --- shape probe: solo / server / one-shot / production chunks ---------------

// poolShapeProbeEnv bounds the probe's query sample.
const poolShapeProbeQueries = 6

// TestToolSearchShapeProbe separates the three things that could explain a
// probability by asking the same question in four request shapes:
//
//	solo        one question, the gate's case-file shape and the fixture's
//	server      every tool of that tool's server
//	one-shot    every tool in the pool, one request
//	production  every tool in the pool, cut into jevSearchQuestionsPerChunk
//
// The production arm is the one the gate actually sends, and the sweep above
// runs in it. The others answer two questions that decide how the threshold may
// be maintained:
//
//   - one-shot vs production: is the sweep's own shape load-bearing? (If not,
//     the sweep can stay cheap; if so, no fixture sweep can ever be trusted.)
//   - solo vs production: a threshold swept SOLO is only valid if the pool the
//     question rides in does not move the answer — which is what the fixture
//     measurement assumed, on 22 tools.
//
// Run: JEV_API_KEY=... TOOL_SEARCH_CORPUS=... TOOL_SEARCH_QUERIES=... \
//
//	go test ./pkg/jev/ -run TestToolSearchShapeProbe -v
func TestToolSearchShapeProbe(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv(evalKeyEnv))
	if apiKey == "" {
		t.Skipf("%s is not set — this measures the service, so it needs a key", evalKeyEnv)
	}
	corpus := loadJSON[toolCorpus](t, envOrDefault(poolCorpusEnv, defaultPoolCorpus))
	queries := loadJSON[toolQuerySet](t, envOrDefault(poolQueriesEnv, defaultPoolQueries))
	client, err := jev.NewClient(apiKey)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	questions := allQuestions(corpus)
	chunks := poolChunks(t, corpus)
	// Which chunk each tool lands in, so the production arm asks exactly the
	// request the gate would send for that candidate.
	chunkOf := map[string]map[string]jev.Question{}
	for _, chunk := range chunks {
		for name := range chunk {
			chunkOf[name] = chunk
		}
	}
	byServer := map[string]map[string]jev.Question{}
	for _, tool := range corpus.Tools {
		if byServer[tool.Server] == nil {
			byServer[tool.Server] = map[string]jev.Question{}
		}
		byServer[tool.Server][tool.Name] = questions[tool.Name]
	}
	serverOf := map[string]string{}
	for _, tool := range corpus.Tools {
		serverOf[tool.Name] = tool.Server
	}

	const repeats = 2
	type arm struct {
		name      string
		questions map[string]jev.Question
	}
	type probe struct {
		intent, lang, tool string
		p                  map[string]float64 // arm → mean p over repeats
	}
	var probes []probe

	for _, q := range sampleShapeProbes(t, corpus, queries, poolShapeProbeQueries) {
		for tool := range q.Labels {
			if q.Labels[tool] != labelExpect {
				continue
			}
			p := probe{intent: q.Intent, lang: q.Lang, tool: tool, p: map[string]float64{}}
			arms := []arm{
				{"solo", map[string]jev.Question{tool: questions[tool]}},
				{"server", byServer[serverOf[tool]]},
				{"one-shot", questions},
				{"production", chunkOf[tool]},
			}
			for _, a := range arms {
				var got []float64
				for range repeats {
					resp, err := evaluate(t, client, q.Text, a.questions)
					if err != nil {
						t.Fatalf("%s/%s arm %s: %v", q.Intent, tool, a.name, err)
					}
					if v, ok := probability(resp, tool); ok {
						got = append(got, v)
					}
				}
				p.p[a.name] = mean(got)
			}
			probes = append(probes, p)
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "\n=== the same question in four request shapes ===\n")
	fmt.Fprintf(&b, "corpus=%d tools, chunk=%d, repeats=%d per arm\n", len(corpus.Tools), prodChunkQuestions, repeats)
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "INTENT\tLANG\tPROBE TOOL\tSOLO\tSERVER\tONE-SHOT\tPRODUCTION\tSOLO-PROD")
	var soloVsProd, shotVsProd float64
	for _, p := range probes {
		dSolo := p.p["solo"] - p.p["production"]
		soloVsProd += math.Abs(dSolo)
		shotVsProd += math.Abs(p.p["one-shot"] - p.p["production"])
		fmt.Fprintf(w, "%s\t%s\t%s\t%.3f\t%.3f\t%.3f\t%.3f\t%+.3f\n",
			p.intent, p.lang, p.tool, p.p["solo"], p.p["server"], p.p["one-shot"], p.p["production"], dSolo)
	}
	_ = w.Flush()
	if n := len(probes); n > 0 {
		fmt.Fprintf(&b, "  mean |solo - production| %.3f, mean |one-shot - production| %.3f over %d probe(s)\n",
			soloVsProd/float64(n), shotVsProd/float64(n), n)
		fmt.Fprintf(&b, "  production is the arm the gate sends and the arm the sweep uses; the other three\n"+
			"  are the shapes a threshold might have been swept in by mistake\n")
	}
	t.Log(b.String())
}

// sampleShapeProbes picks the requests the probe runs on: the first intents
// with an expectation, alternating language — so the sample is half zh, half en
// by construction rather than by luck.
func sampleShapeProbes(t *testing.T, corpus toolCorpus, queries toolQuerySet, limit int) []poolQuery {
	t.Helper()
	byIntent := map[string]map[string]poolQuery{} // intent → lang → query
	var order []string
	for _, q := range buildPoolQueries(t, corpus, queries) {
		if q.NoTool || !hasExpect(q) {
			continue
		}
		if byIntent[q.Intent] == nil {
			byIntent[q.Intent] = map[string]poolQuery{}
			order = append(order, q.Intent)
		}
		byIntent[q.Intent][q.Lang] = q
	}
	var picked []poolQuery
	for i, id := range order {
		if len(picked) >= limit {
			break
		}
		lang := "zh"
		if i%2 == 1 {
			lang = "en"
		}
		if q, ok := byIntent[id][lang]; ok {
			picked = append(picked, q)
		}
	}
	return picked
}

func hasExpect(q poolQuery) bool {
	for _, l := range q.Labels {
		if l == labelExpect {
			return true
		}
	}
	return false
}

// evaluate is one timed call with its own timeout, shared by the probe above.
func evaluate(t *testing.T, client *jev.Client, state any, questions map[string]jev.Question) (*jev.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), evalCaseTimeout)
	defer cancel()
	return client.Evaluate(ctx, state, questions)
}

// --- raw run dump -----------------------------------------------------------

// poolRawEnv names the file a sweep's raw probabilities are written to.
const poolRawEnv = "TOOL_SEARCH_SWEEP_RAW"

// poolRawRun is every number a sweep produced, per request and per tool.
//
// The report is a READING of the run; this is the run. A threshold argued about
// later — with a different candidate, a different metric, or after someone
// questions the labels — should be re-derived from this file rather than from
// another metered run, and the questions the report does not answer (how many
// tools cleared a cut-off, where an expectation ranked among them) are exactly
// the ones a stored run can be asked.
type poolRawRun struct {
	Corpus  string    `json:"corpus"`
	Queries string    `json:"queries"`
	Model   string    `json:"model"`
	Repeats int       `json:"repeats"`
	Chunk   int       `json:"chunk_questions"`
	Tools   []string  `json:"tools"`
	Raw     []poolRaw `json:"requests"`
}

type poolRaw struct {
	Intent      string               `json:"intent"`
	Lang        string               `json:"lang"`
	Text        string               `json:"text"`
	NoTool      bool                 `json:"no_tool"`
	Labels      map[string]string    `json:"labels"` // named candidates only
	P           map[string][]float64 `json:"p"`      // tool → p(true) per answered repeat
	InputTokens int                  `json:"input_tokens"`
}

// dumpPoolRun writes the raw run when TOOL_SEARCH_SWEEP_RAW names a file, and
// logs the absolute path either way so a relative one cannot land somewhere
// surprising unnoticed (a test binary's working directory is its package).
func dumpPoolRun(t *testing.T, corpusPath, queriesPath, model string, repeats int, corpus toolCorpus, runs []poolRun) {
	t.Helper()
	out := strings.TrimSpace(os.Getenv(poolRawEnv))
	if out == "" {
		return
	}
	names := make([]string, 0, len(corpus.Tools))
	for _, tool := range corpus.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	dump := poolRawRun{
		Corpus: corpusPath, Queries: queriesPath, Model: model, Repeats: repeats,
		Chunk: prodChunkQuestions, Tools: names,
		Raw: make([]poolRaw, 0, len(runs)),
	}
	for _, r := range runs {
		labels := map[string]string{}
		for name, l := range r.q.Labels {
			if l != labelUnnamed {
				labels[name] = string(l)
			}
		}
		dump.Raw = append(dump.Raw, poolRaw{
			Intent: r.q.Intent, Lang: r.q.Lang, Text: r.q.Text, NoTool: r.q.NoTool,
			Labels: labels, P: r.probs, InputTokens: r.inputTokens,
		})
	}
	raw, err := json.Marshal(dump)
	if err != nil {
		t.Errorf("marshal raw run: %v", err)
		return
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o600); err != nil {
		t.Errorf("write %s: %v", out, err)
		return
	}
	abs, _ := filepath.Abs(out)
	t.Logf("raw run written to %s", abs)
}

// --- the decision: recall, offers, leak -------------------------------------

// poolMaxResults is the gate's default cap on returned tools
// (agent/mcp's clampMaxResults(0) = 5). It is what makes a flat pool dangerous:
// a low cut-off can fill the list with near-misses and crowd the one tool the
// search was for out of the answer.
const poolMaxResults = 5

// poolReportDecision is the section a cut-off is actually chosen from.
//
// The sweep table's hit rates cannot choose one: most intents name two or three
// legitimate answers, so "what share of labelled pairs cleared t" understates a
// search that found ONE of them — which is a successful search. What decides:
//
//	recall     did any expectation clear t (the search worked)
//	offers     tools handed to the model per search — each is a schema registered
//	           and a tool-list change, which invalidates the prompt prefix
//	precision  share of those offers that were expectations
//	truncated  an expectation cleared t but ranked outside max_results
//	leak       a request nothing can serve, answered with something
//
// And the failures are listed by name, because a threshold's cost is a list of
// searches that stop working — not a rate.
func poolReportDecision(b *bytes.Buffer, runs []poolRun, repeats int) {
	var matched, noTool []poolRun
	for _, r := range runs {
		if len(r.probs) == 0 {
			continue
		}
		if r.q.NoTool {
			noTool = append(noTool, r)
			continue
		}
		matched = append(matched, r)
	}
	if len(matched) == 0 {
		return
	}

	// p(tool) as the gate reads it: the mean over a query's repeats. A gate
	// sees one call, so the mean is the honest summary of a run of them.
	meanP := func(r poolRun) map[string]float64 {
		out := make(map[string]float64, len(r.probs))
		for name, ps := range r.probs {
			out[name] = mean(ps)
		}
		return out
	}

	b.WriteString("\nDECISION (per REQUEST, not per pair)\n")
	w := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "T\tRECALL\tOFFERS/QUERY\tPRECISION\tUNLABELLED/QUERY\tTRUNCATED\tNO-TOOL LEAK")
	for _, th := range poolThresholds {
		recall, offers, expOffers, unnamedOffers, truncated := 0, 0, 0, 0, 0
		for _, r := range matched {
			ps := meanP(r)
			var hit []string
			for name, p := range ps {
				if p >= th {
					hit = append(hit, name)
				}
			}
			sort.Slice(hit, func(i, j int) bool {
				if ps[hit[i]] != ps[hit[j]] {
					return ps[hit[i]] > ps[hit[j]]
				}
				return hit[i] < hit[j]
			})
			found := false
			for _, name := range hit {
				if r.q.Labels[name] == labelExpect {
					found = true
					break
				}
			}
			if found {
				recall++
				// The gate returns the top max_results only: a search whose
				// expectations all rank past the cut reported itself as
				// successful and hands the model something else.
				inside := false
				for _, name := range hit[:min(len(hit), poolMaxResults)] {
					if r.q.Labels[name] == labelExpect {
						inside = true
						break
					}
				}
				if !inside {
					truncated++
				}
			}
			offers += len(hit)
			for _, name := range hit {
				switch r.q.Labels[name] {
				case labelExpect:
					expOffers++
				case labelUnnamed:
					unnamedOffers++
				}
			}
		}
		leak := 0
		for _, r := range noTool {
			ps := meanP(r)
			for _, p := range ps {
				if p >= th {
					leak++
					break
				}
			}
		}
		n := float64(len(matched))
		fmt.Fprintf(w, "%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%s\n", th,
			float64(recall)/n, float64(offers)/n, float64(expOffers)/float64(max(1, offers)),
			float64(unnamedOffers)/n, float64(truncated)/n, rate(leak, len(noTool)))
	}
	_ = w.Flush()

	// The cost of a cut-off, itemised: every search it stops working.
	for _, th := range []float64{0.15, 0.20, 0.25} {
		var missed []string
		for _, r := range matched {
			ps := meanP(r)
			best, tool := 0.0, ""
			for name, p := range ps {
				if r.q.Labels[name] == labelExpect && p > best {
					best, tool = p, name
				}
			}
			if tool != "" && best < th {
				missed = append(missed, fmt.Sprintf("%s/%s %.3f", r.q.Intent, r.q.Lang, best))
			}
		}
		sort.Strings(missed)
		fmt.Fprintf(b, "  t=%.2f loses %d/%d searches: %s\n", th, len(missed), len(matched),
			strings.Join(missed, ", "))
	}
}

// --- offline re-analysis ----------------------------------------------------

// TestToolSearchRawAnalysis re-prints the decision section from a stored raw
// run, with no API calls.
//
// A metered run should be argued with, not repeated: the raw dump holds every
// probability, so a different cut-off, a corrected label, or a metric nobody
// thought of during the run can be answered for free — and a claim about the
// measurement can be checked by whoever reads it, on their own machine.
//
//	TOOL_SEARCH_SWEEP_RAW=/path/raw.json go test ./pkg/jev/ -run TestToolSearchRawAnalysis -v
func TestToolSearchRawAnalysis(t *testing.T) {
	path := strings.TrimSpace(os.Getenv(poolRawEnv))
	if path == "" {
		t.Skipf("%s is not set — point it at a raw run from TestToolSearchPoolSweep", poolRawEnv)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var dump poolRawRun
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}

	runs := make([]poolRun, 0, len(dump.Raw))
	for _, r := range dump.Raw {
		labels := make(map[string]poolLabel, len(dump.Tools))
		for _, name := range dump.Tools {
			labels[name] = labelUnnamed
		}
		for name, l := range r.Labels {
			labels[name] = poolLabel(l)
		}
		probs := make(map[string][]float64, len(r.P))
		for name, ps := range r.P {
			probs[name] = ps
		}
		runs = append(runs, poolRun{
			q:     poolQuery{Intent: r.Intent, Lang: r.Lang, Text: r.Text, NoTool: r.NoTool, Labels: labels},
			probs: probs,
		})
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "\n=== stored run: %s ===\n", path)
	fmt.Fprintf(&b, "model=%s repeats=%d chunk=%d tools=%d requests=%d\n",
		dump.Model, dump.Repeats, dump.Chunk, len(dump.Tools), len(runs))
	poolReportDecision(&b, runs, dump.Repeats)
	t.Log(b.String())
}
