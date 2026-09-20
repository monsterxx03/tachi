package mcp

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp/jevsearch"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/jev"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// JevSearcher matches a search query against the deferred pool with TypeSafe
// Jev instead of BM25 keywords. Design and the measured cut-off:
// docs/2026-09-20-jev-tool-search-design.md.
//
// One search is:
//
//   - the query that NAMES something (select: / exact or suffix name /
//     mcp__server / empty) is resolved by the pool, no judgment involved;
//   - anything else becomes prose to judge: every candidate tool gets its own
//     noul question ("can this tool carry the request out directly?"), all
//     sharing the query as the one state. The wording lives in
//     agent/mcp/jevsearch, which is also what the case file that produced the
//     threshold was generated from;
//   - answers at or above the threshold are ranked by probability (ties by
//     name, so a result is reproducible) and cut to max_results.
//
// Anything that stops the judgment — no judge, a failure, the budget — falls
// back to the pool's own keyword search, which is what the tool did before this
// existed. A search degrades; it does not fail, and it does not wait: it runs
// inside a model turn, and the client's own retry loop can outlast the budget
// several times over.
type JevSearcher struct {
	pool      *DeferredPool
	judge     Judge
	threshold float64
	budget    time.Duration
	usage     *llm.UsageRecorder
	// degraded is set once a search has fallen back to the keyword path and
	// never cleared here: this searcher is replaced, not repaired (a settings
	// change builds a new one). It decides the advice the tool gives the model
	// — promising "describe it in a sentence" while a keyword search runs
	// behind it is how a tool becomes unreachable.
	degraded atomic.Bool
	// OnDegrade is called when a prose search fell back to the keyword path.
	// The caller uses it to stop telling the model to describe a scenario: the
	// deferred-tools block offers per-server summaries (which only a matcher
	// that reads sentences can use) while the gate is healthy, and the per-tool
	// catalog — the names the keyword matcher actually matches on — once it is
	// not. Optional.
	OnDegrade func(error)
}

// Judge is the Jev call the searcher needs: what a client does, narrowed so a
// test can answer without a service. *jev.Client satisfies it as it stands.
type Judge interface {
	Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.Response, error)
	// Model is the version the answers came from: recorded with the cost, so a
	// calibration can be traced to the weights that produced it.
	Model() string
}

const (
	// jevSearchChunkTokens bounds one REQUEST's question text. A pool large
	// enough to overflow it is judged in several sequential calls rather than
	// one huge one: the service charges per input token and counts question
	// COUNT against its total budget, while state + the longest single question
	// is what binds at ~32k (pkg/jev's package doc).
	jevSearchChunkTokens = 8000
	// jevSearchTokensPerQuestion is the chunking estimate per question (tool
	// text + criteria). It is a budgeting slack, not a measurement.
	jevSearchTokensPerQuestion = 80
	// jevSearchMaxQuestions caps how much of a pool one search judges. Past it
	// the remaining tools are not judged at all and the log says so — a silent
	// truncation would read as "the gate found nothing".
	jevSearchMaxQuestions = 400
)

// NewJevSearcher builds the gate. threshold and budget come from
// mcp_tool_search.jev_threshold / jev_budget; usage may be nil (no ledger row)
// and onDegrade is the fallback notification (see JevSearcher.OnDegrade).
func NewJevSearcher(pool *DeferredPool, judge Judge, threshold float64, budget time.Duration, usage *llm.UsageRecorder, onDegrade func(error)) *JevSearcher {
	return &JevSearcher{pool: pool, judge: judge, threshold: threshold, budget: budget, usage: usage, OnDegrade: onDegrade}
}

// Search answers with the tools the query is about: the exact-name path as it
// was, then the judgment, then (on any failure) the keyword path.
func (s *JevSearcher) Search(ctx context.Context, query string, maxResults int) []tools.MCPSearchResultItem {
	if exact, ok := s.pool.SearchExact(query, maxResults); ok {
		return exact
	}
	results, err := s.judgeQuery(ctx, query, clampMaxResults(maxResults))
	if err != nil {
		logger.FromContext(ctx).Warn(ctx, "MCP: jev tool search failed, using keyword search",
			"err", err, "query", strutil.Truncate(query, 120))
		s.degraded.Store(true)
		if s.OnDegrade != nil {
			s.OnDegrade(err)
		}
		return s.pool.Search(query, maxResults)
	}
	return results
}

// SearchAdvice tells the model how to write a query: a sentence while the
// judgment is serving, the keywords a keyword search can match once it is not.
// The advice is what the model reads before writing, so it has to describe the
// matcher that will actually receive the query.
func (s *JevSearcher) SearchAdvice() string {
	if s.degraded.Load() {
		return tools.KeywordSearchAdvice
	}
	return tools.ProseSearchAdvice
}

// judgeQuery asks the judgment for every candidate tool and returns the ones it
// cleared the threshold on. An error means "no judgment available" — the caller
// degrades rather than passing a partial set off as an answer.
func (s *JevSearcher) judgeQuery(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if s.judge == nil {
		return nil, errors.New("jev: no judge configured")
	}
	// Sorted by name: the snapshot's order is a map's, and chunk boundaries and
	// probability ties must not move between two searches of the same pool.
	all := s.pool.snapshot()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	if len(all) == 0 {
		return nil, nil
	}
	if extra := len(all) - jevSearchMaxQuestions; extra > 0 {
		logger.FromContext(ctx).Warn(ctx, "MCP: jev tool search judged only part of the pool",
			"judged", jevSearchMaxQuestions, "skipped", extra)
		all = all[:jevSearchMaxQuestions]
	}

	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()

	probs := make(map[string]float64, len(all))
	for start := 0; start < len(all); {
		end := min(start+jevSearchChunkTokens/jevSearchTokensPerQuestion, len(all))
		questions := make(map[string]jev.Question, end-start)
		for _, t := range all[start:end] {
			questions[t.Name] = jevsearch.Question(candidateOf(t))
		}
		resp, err := s.judge.Evaluate(ctx, query, questions)
		if err != nil {
			return nil, err
		}
		s.recordUsage(ctx, resp)
		for _, t := range all[start:end] {
			// A question the service did not answer is NOT a zero: it is no
			// answer, and the only safe reading of it is "do not select".
			if a, ok := resp.Answers[t.Name]; ok && a.Noul != nil {
				probs[t.Name] = *a.Noul
			}
		}
		start = end
	}

	type hit struct {
		tool *DeferredTool
		p    float64
	}
	var hits []hit
	unanswered := 0
	for _, t := range all {
		p, ok := probs[t.Name]
		if !ok {
			unanswered++
			continue
		}
		if p >= s.threshold {
			hits = append(hits, hit{tool: t, p: p})
		}
	}
	if unanswered > 0 {
		logger.FromContext(ctx).Warn(ctx, "MCP: jev tool search had unanswered questions",
			"unanswered", unanswered, "of", len(all))
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].p != hits[j].p {
			return hits[i].p > hits[j].p
		}
		return hits[i].tool.Name < hits[j].tool.Name
	})
	n := min(len(hits), maxResults)
	out := make([]SearchResult, 0, n)
	for _, h := range hits[:n] {
		out = append(out, s.pool.toResult(h.tool))
	}
	return out, nil
}

// candidateOf is the tool as the question needs it: the fields the pool already
// caches, with the description reduced to its first line — the same text the
// reminder shows the model.
func candidateOf(t *DeferredTool) jevsearch.Candidate {
	return jevsearch.Candidate{
		Name:    t.Name,
		Server:  t.ServerName,
		Hint:    t.SearchHint,
		Summary: strutil.FirstLineOrTruncate(t.Description, 100),
	}
}

// recordUsage writes the ledger row for one judgment call. Jev is not a
// provider, so the decorator that bills providers never sees it: the row is
// written here, with the session it belongs to and the price the built-in table
// resolves for the model.
//
// The built-in table is the only source consulted: a per-provider price
// override lives on a configured provider (`pricing:`), and Jev has none — it
// is reached through the jev block, not through the provider list.
func (s *JevSearcher) recordUsage(ctx context.Context, resp *jev.Response) {
	if s.usage == nil || resp == nil {
		return
	}
	now := time.Now()
	row := llm.UsageRow{
		TS:           now,
		SessionID:    tools.SessionIDFromCtx(ctx),
		Kind:         llm.UsageKindJev,
		Provider:     "typesafe",
		Model:        s.judge.Model(),
		InputTokens:  int64(resp.Usage.InputTokens),
		OutputTokens: int64(resp.Usage.OutputTokens),
		// Jev earns no 积分 (there is no credit_rate for it), and an explicit 0
		// keeps the row out of the "written before credits existed" bucket that
		// aggregators recompute from the current rate.
		Credit: new(float64),
	}
	if price := llm.GetBuiltinModelPriceAt(row.Model, now); price != nil {
		row.InputPrice = price.InputPrice
		row.OutputPrice = price.OutputPrice
		row.CacheReadPrice = price.CacheReadInputPrice
		row.CacheCreationPrice = price.CacheCreationInputPrice
	}
	if err := s.usage.Record(row); err != nil {
		logger.FromContext(ctx).Warn(ctx, "MCP: jev usage not recorded", "err", err)
	}
}
