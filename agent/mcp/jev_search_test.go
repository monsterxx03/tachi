package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/jev"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeJudge answers like the service without being one: one probability per
// tool name, plus the knobs a test needs to make it fail, block or stay silent.
type fakeJudge struct {
	answers map[string]float64 // tool name → p(true)
	missing map[string]bool    // names the service returned no answer for
	err     error
	block   bool // wait for the context instead of answering (the budget test)
	model   string

	calls  [][]string // the question keys sent, one entry per Evaluate call
	states []any      // the state each call carried
}

func (f *fakeJudge) Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.Response, error) {
	f.states = append(f.states, state)
	names := make([]string, 0, len(questions))
	for n := range questions {
		names = append(names, n)
	}
	sort.Strings(names)
	f.calls = append(f.calls, names)

	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	resp := &jev.Response{
		Model:   f.model,
		Answers: make(map[string]jev.Answer, len(names)),
		Usage:   jev.Usage{InputTokens: 100, OutputTokens: 5},
	}
	for _, n := range names {
		if f.missing[n] {
			continue
		}
		p := f.answers[n]
		resp.Answers[n] = jev.Answer{Type: jev.QuestionNoul, Noul: &p}
	}
	return resp, nil
}

func (f *fakeJudge) Model() string { return f.model }

// jevTestPool is a pool whose tools carry the fields the question text reads.
func jevTestPool(tools ...*DeferredTool) *DeferredPool {
	p := NewDeferredPool()
	for _, tl := range tools {
		if tl.SearchHint == "" {
			tl.SearchHint = tl.ServerName + ", " + tl.Name
		}
		p.Add(tl)
	}
	return p
}

func newTestSearcher(pool *DeferredPool, judge Judge, threshold float64) *JevSearcher {
	return NewJevSearcher(pool, judge, threshold, time.Second, nil, nil)
}

// A query that NAMES what it wants is a lookup: no judgment call, and the same
// answer the keyword path gave before this existed.
func TestJevSearcher_NamesAreLookedUpNotJudged(t *testing.T) {
	pool := jevTestPool(
		testDeferredTool("mcp__pg__query", "pg", "Query database"),
		testDeferredTool("mcp__gh__pr", "gh", "Create PR"),
	)
	judge := &fakeJudge{answers: map[string]float64{}}
	s := newTestSearcher(pool, judge, 0.35)

	for _, tc := range []struct {
		query string
		want  []string
	}{
		// select: is how a name is spelled unambiguously, including by suffix.
		{"select:mcp__pg__query", []string{"mcp__pg__query"}},
		{"select:query", []string{"mcp__pg__query"}},
		{"mcp__pg__query", []string{"mcp__pg__query"}},
		{"mcp__pg", []string{"mcp__pg__query"}},
		{"", []string{"mcp__gh__pr", "mcp__pg__query"}},
	} {
		got := s.Search(context.Background(), tc.query, 5)
		names := make([]string, 0, len(got))
		for _, r := range got {
			names = append(names, r.Name)
		}
		sort.Strings(names)
		assert.Equal(t, tc.want, names, "query %q", tc.query)
	}
	assert.Empty(t, judge.calls, "a named query must not reach the service")

	// A bare word is prose, not a name: it is the case the judgment exists for.
	judge.answers = map[string]float64{"mcp__pg__query": 0.9}
	assert.Len(t, s.Search(context.Background(), "query", 5), 1)
	assert.Len(t, judge.calls, 1)
}

// The threshold decides, and the order is by probability.
func TestJevSearcher_ThresholdAndRanking(t *testing.T) {
	pool := jevTestPool(
		testDeferredTool("mcp__a__one", "a", "one"),
		testDeferredTool("mcp__b__two", "b", "two"),
		testDeferredTool("mcp__c__three", "c", "three"),
		testDeferredTool("mcp__d__four", "d", "four"),
	)
	judge := &fakeJudge{answers: map[string]float64{
		"mcp__a__one":   0.10,
		"mcp__b__two":   0.90,
		"mcp__c__three": 0.35, // exactly at the cut-off: kept
		"mcp__d__four":  0.34,
	}}
	s := newTestSearcher(pool, judge, 0.35)

	got := s.Search(context.Background(), "find me a can opener", 5)
	require.Len(t, got, 2)
	assert.Equal(t, "mcp__b__two", got[0].Name)
	assert.Equal(t, "mcp__c__three", got[1].Name)
	assert.Equal(t, []any{"find me a can opener"}, judge.states, "the state is the query as written")
	assert.Len(t, judge.calls, 1)
	assert.Len(t, judge.calls[0], 4, "every candidate gets a question")
}

// Equal probabilities must still produce one order: a result that shuffles
// between two searches of the same pool is not reproducible.
func TestJevSearcher_TiesBreakByName(t *testing.T) {
	pool := jevTestPool(
		testDeferredTool("mcp__z__tool", "z", "tool"),
		testDeferredTool("mcp__a__tool", "a", "tool"),
	)
	judge := &fakeJudge{answers: map[string]float64{"mcp__z__tool": 0.8, "mcp__a__tool": 0.8}}
	s := newTestSearcher(pool, judge, 0.35)

	got := s.Search(context.Background(), "anything", 5)
	require.Len(t, got, 2)
	assert.Equal(t, "mcp__a__tool", got[0].Name)
	assert.Equal(t, "mcp__z__tool", got[1].Name)
}

// A question the service did not answer is NOT a zero: it is no answer, and the
// only safe reading is "do not offer this tool" — while the answered ones still
// come through.
func TestJevSearcher_MissingAnswerIsNotSelected(t *testing.T) {
	pool := jevTestPool(
		testDeferredTool("mcp__a__good", "a", "good"),
		testDeferredTool("mcp__b__silent", "b", "silent"),
	)
	judge := &fakeJudge{
		answers: map[string]float64{"mcp__a__good": 0.9},
		missing: map[string]bool{"mcp__b__silent": true},
	}
	s := newTestSearcher(pool, judge, 0.35)

	got := s.Search(context.Background(), "something to judge", 5)
	require.Len(t, got, 1)
	assert.Equal(t, "mcp__a__good", got[0].Name)
}

// Any failure degrades to the keyword search rather than failing the tool call
// or answering with a partial judgment.
// A degraded search is announced: the caller has to know, because the shape it
// promised the model (describe a scenario) is not what just ran.
func TestJevSearcher_ReportsDegradation(t *testing.T) {
	pool := jevTestPool(testDeferredTool("mcp__pg__query", "pg", "Query database"))

	var degraded []error
	judge := &fakeJudge{err: errors.New("503 overloaded")}
	s := NewJevSearcher(pool, judge, 0.35, time.Second, nil, func(err error) { degraded = append(degraded, err) })
	s.Search(context.Background(), "database query", 5)
	require.Len(t, degraded, 1)
	assert.ErrorContains(t, degraded[0], "503")
	// The advice follows: the model is told to write keywords, because that is
	// what the next search will match on.
	assert.Equal(t, tools.KeywordSearchAdvice, s.SearchAdvice())

	// A search that worked says nothing.
	working := NewJevSearcher(pool, &fakeJudge{answers: map[string]float64{"mcp__pg__query": 0.9}},
		0.35, time.Second, nil, func(error) { degraded = append(degraded, nil) })
	working.Search(context.Background(), "database query", 5)
	assert.Len(t, degraded, 1)
	assert.Equal(t, tools.ProseSearchAdvice, working.SearchAdvice())
}

func TestJevSearcher_ErrorDegradesToKeywords(t *testing.T) {
	pool := jevTestPool(
		testDeferredTool("mcp__pg__query", "pg", "Query database"),
		testDeferredTool("mcp__gh__pr", "gh", "Create PR"),
	)
	judge := &fakeJudge{err: errors.New("503 overloaded")}
	s := newTestSearcher(pool, judge, 0.35)

	got := s.Search(context.Background(), "database query", 5)
	assert.Equal(t, pool.Search("database query", 5), got)
	assert.NotEmpty(t, got, "the fixture must be one the keyword path can answer")
}

// The budget is a deadline, not a hope: a search that overruns degrades instead
// of making the model turn wait out the client's retry loop.
func TestJevSearcher_BudgetDegrades(t *testing.T) {
	pool := jevTestPool(testDeferredTool("mcp__pg__query", "pg", "Query database"))
	judge := &fakeJudge{block: true}
	s := NewJevSearcher(pool, judge, 0.35, 20*time.Millisecond, nil, nil)

	start := time.Now()
	got := s.Search(context.Background(), "database query", 5)
	assert.Equal(t, pool.Search("database query", 5), got)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// A pool larger than one request's question budget is judged in chunks, and
// every tool is still judged.
func TestJevSearcher_ChunksThePool(t *testing.T) {
	pool := NewDeferredPool()
	answers := map[string]float64{}
	const total = 101 // one more than a full chunk at the current constants
	for i := range total {
		name := "mcp__s__tool" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		pool.Add(testDeferredTool(name, "s", "a tool"))
		answers[name] = 0.5
	}
	judge := &fakeJudge{answers: answers}
	s := newTestSearcher(pool, judge, 0.35)

	got := s.Search(context.Background(), "find any of them", 5)
	assert.Len(t, judge.calls, 2, "the pool does not fit one request")
	assert.Len(t, judge.calls[0], jevSearchChunkTokens/jevSearchTokensPerQuestion)
	assert.Len(t, judge.calls[1], total-jevSearchChunkTokens/jevSearchTokensPerQuestion)
	assert.Len(t, got, 5, "max_results still caps the answer")
}

// Past the cap the rest of the pool is not judged at all — and the search still
// answers, rather than reporting nothing.
func TestJevSearcher_CapsThePool(t *testing.T) {
	pool := NewDeferredPool()
	answers := map[string]float64{}
	for i := range jevSearchMaxQuestions + 5 {
		name := "mcp__s__tool" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + string(rune('a'+i/676))
		pool.Add(testDeferredTool(name, "s", "a tool"))
		answers[name] = 0.9
	}
	judge := &fakeJudge{answers: answers}
	s := newTestSearcher(pool, judge, 0.35)

	got := s.Search(context.Background(), "find any of them", 3)
	asked := 0
	for _, c := range judge.calls {
		asked += len(c)
	}
	assert.LessOrEqual(t, asked, jevSearchMaxQuestions)
	assert.Len(t, got, 3)
}

func TestJevSearcher_EmptyPool(t *testing.T) {
	judge := &fakeJudge{answers: map[string]float64{}}
	s := newTestSearcher(NewDeferredPool(), judge, 0.35)

	assert.Empty(t, s.Search(context.Background(), "anything at all", 5))
	assert.Empty(t, judge.calls)
}

// Jev is not a provider, so nothing wraps it: the gate writes the ledger row
// itself, under its own kind.
func TestJevSearcher_RecordsUsage(t *testing.T) {
	pool := jevTestPool(testDeferredTool("mcp__pg__query", "pg", "Query database"))
	judge := &fakeJudge{answers: map[string]float64{"mcp__pg__query": 0.9}, model: "jev-1.13.0"}
	dir := t.TempDir()
	s := NewJevSearcher(pool, judge, 0.35, time.Second, llm.NewUsageRecorder(dir), nil)

	ctx := tools.WithSessionID(context.Background(), "session-1")
	require.Len(t, s.Search(ctx, "database query", 5), 1)

	day := time.Now().Format("2006-01-02")
	raw, err := os.ReadFile(filepath.Join(dir, day+".jsonl"))
	require.NoError(t, err)
	var row llm.UsageRow
	require.NoError(t, json.Unmarshal(raw, &row))
	assert.Equal(t, llm.UsageKindJev, row.Kind)
	assert.Equal(t, "session-1", row.SessionID)
	assert.Equal(t, "jev-1.13.0", row.Model)
	assert.Equal(t, int64(100), row.InputTokens)

	// Priced, not "unpriced": the row is what /usage sums, so a 0 here would
	// make the gate look free. Jev bills input only.
	assert.Positive(t, row.InputPrice, "the built-in table must price jev")
	assert.Zero(t, row.OutputPrice, "Jev output is free")
	require.NotNil(t, row.Credit, "an explicit credit snapshot, not the pre-upgrade nil")
	assert.Zero(t, *row.Credit)

	// The number the reader ends up seeing: cost = tokens x price (input only).
	cost := llm.CostForUsage(
		&llm.Usage{InputTokens: row.InputTokens},
		&llm.ModelPrice{InputPrice: row.InputPrice},
		"anthropic",
	)
	assert.Positive(t, cost, "a 100-token judgment call must cost something")
}
