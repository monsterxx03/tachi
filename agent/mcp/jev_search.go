package mcp

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp/jevsearch"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
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
//     mcp__server) is resolved by the pool, no judgment involved, and against
//     the WHOLE pool — a name is an identity. An empty query is a listing, not
//     a name, and the pool answers it from opts.Server's tools alone;
//   - anything else becomes prose to judge: every candidate tool gets its own
//     noul question ("can this tool carry the request out directly?"), all
//     sharing the query as the one state. The wording lives in
//     agent/mcp/jevsearch, which is also what the case file that produced the
//     threshold was generated from. The candidates are the tools of ONE server
//     when the caller named one (opts.Server) and the whole pool otherwise —
//     that is the cost lever, not a different question;
//   - answers at or above the threshold are ranked by probability (ties by
//     name, so a result is reproducible) and cut to max_results.
//
// Anything that stops the judgment — no judge, a failure, the budget — falls
// back to the pool's own keyword search, which is what the tool did before this
// existed. A search degrades; it does not fail, and it does not wait: it runs
// inside a model turn, and the client's own retry loop can outlast the budget
// several times over.
type JevSearcher struct {
	pool  *DeferredPool
	judge Judge
	// cfg is the config the ledger half of the gate reads — the jev block's
	// `pricing:` override and credit_rate (see recordUsage). It is held as the
	// live config rather than a value copied at build time, so a price change
	// needs no new searcher; nil is the zero block.
	cfg       *config.Config
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

// Judge is the Jev call a gate needs: what a client does, narrowed so a test can
// answer without a service. The shape lives in pkg/jev (jev.Judge) because the
// channel's cron notify gate needs exactly the same seam, and *jev.Client
// satisfies it as it stands.
type Judge = jev.Judge

const (
	// jevSearchMaxQuestions caps how much of a pool one search judges. Past it
	// the remaining tools are not judged at all and the log says so — a silent
	// truncation would read as "the gate found nothing".
	jevSearchMaxQuestions = 400
)

// questionsPerRequest is how many tools ONE request judges: the endpoint's cap
// (see config.JevConfig.QuestionsPerRequest — a configured value, else a known
// gateway's measured cap, else the reference default).
//
// Read from the LIVE config per search rather than captured at build time, so a
// settings save reaches the next search; on the desktop the `jev` row also
// rebuilds this searcher (a.RefreshMCPSearch). A searcher built without a config
// (a test) resolves like an empty block, i.e. the reference default.
func (s *JevSearcher) questionsPerRequest() int {
	if s.cfg == nil {
		return config.JevConfig{}.QuestionsPerRequest()
	}
	return s.cfg.Jev.QuestionsPerRequest()
}

// NewJevSearcher builds the gate. threshold and budget come from
// mcp_tool_search.jev_threshold / jev_budget; usage may be nil (no ledger row)
// and onDegrade is the fallback notification (see JevSearcher.OnDegrade). cfg
// is what the ledger prices a call from — the jev block's `pricing:` override
// and credit_rate — and may be nil (built-in table only, credit 0).
func NewJevSearcher(pool *DeferredPool, judge Judge, cfg *config.Config, threshold float64, budget time.Duration, usage *llm.UsageRecorder, onDegrade func(error)) *JevSearcher {
	return &JevSearcher{pool: pool, judge: judge, cfg: cfg, threshold: threshold, budget: budget, usage: usage, OnDegrade: onDegrade}
}

// Search answers with the tools the query is about: the exact-name path as it
// was, then the judgment, then (on any failure) the keyword path.
//
// A narrowed request (opts.Server) costs less and asks a different question
// than a full-pool one: the judgment only sees that server's tools, so its
// answers are a verdict about THAT candidate set. The exact half gets opts.Server
// too, so a narrowed LISTING is that server's tools — while a name stays global,
// because a name is an identity. Both rules live in DeferredPool.SearchExact,
// which is why it takes the server rather than this method filtering afterwards.
func (s *JevSearcher) Search(ctx context.Context, opts tools.MCPSearchOptions) []tools.MCPSearchResultItem {
	if exact, ok := s.pool.SearchExact(opts.Query, opts.Server, opts.MaxResults); ok {
		return exact
	}
	results, err := s.judgeQuery(ctx, opts)
	if err != nil {
		logger.FromContext(ctx).Warn(ctx, "MCP: jev tool search failed, using keyword search",
			"err", err, "query", strutil.Truncate(opts.Query, 120), "server", opts.Server)
		s.degraded.Store(true)
		if s.OnDegrade != nil {
			s.OnDegrade(err)
		}
		return s.pool.SearchIn(opts.Query, opts.Server, opts.MaxResults)
	}
	return results
}

// Servers is the pool's server list (tools.MCPServerLister), so the search tool
// can reject an unknown `server` by name before anything is judged.
func (s *JevSearcher) Servers() []string { return s.pool.Servers() }

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
func (s *JevSearcher) judgeQuery(ctx context.Context, opts tools.MCPSearchOptions) ([]SearchResult, error) {
	if s.judge == nil {
		return nil, errors.New("jev: no judge configured")
	}
	// Sorted by name: the snapshot's order is a map's, and chunk boundaries and
	// probability ties must not move between two searches of the same pool.
	all := s.pool.snapshotFor(opts.Server)
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	if len(all) == 0 {
		return nil, nil
	}
	if extra := len(all) - jevSearchMaxQuestions; extra > 0 {
		logger.FromContext(ctx).Warn(ctx, "MCP: jev tool search judged only part of the pool",
			"server", opts.Server, "judged", jevSearchMaxQuestions, "skipped", extra)
		all = all[:jevSearchMaxQuestions]
	}

	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()

	probs := make(map[string]float64, len(all))
	per := s.questionsPerRequest()
	chunks := (len(all) + per - 1) / per
	for start := 0; start < len(all); {
		end := min(start+per, len(all))
		questions := make(map[string]jev.Question, end-start)
		for _, t := range all[start:end] {
			questions[t.Name] = jevsearch.Question(candidateOf(t))
		}
		resp, err := s.judge.Evaluate(ctx, opts.Query, questions)
		if err != nil {
			return nil, err
		}
		s.logAnswers(ctx, opts.Query, opts.Server, all[start:end], resp, start/per+1, chunks)
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
	n := min(len(hits), clampMaxResults(opts.MaxResults))
	out := make([]SearchResult, 0, n)
	for _, h := range hits[:n] {
		out = append(out, s.pool.matchedResult(h.tool, tools.MatchByJudgment, h.p))
	}
	return out, nil
}

// logAnswers writes what the judgment answered, one line per request, with
// every question's probability in it.
//
// One line rather than a field per answer: the answer map is the whole point of
// reading this log — who scored near the cut-off, and which question came back
// empty — and a handler printing a 100-entry map would interleave it with the
// metadata. The metadata goes first, the list last, so the tail can be as long
// as the pool needs. server is logged because it is what the answer is ABOUT:
// the same query judged against a narrowed candidate set is a different
// question, and a probability without it cannot be compared to anything.
func (s *JevSearcher) logAnswers(ctx context.Context, query, server string, judged []*DeferredTool, resp *jev.Response, chunk, chunks int) {
	if resp == nil {
		return
	}
	logger.FromContext(ctx).Info(ctx, "MCP: jev tool search judged tools",
		"query", strutil.Truncate(query, 120),
		"server", server,
		"model", resp.Model,
		"chunk", strconv.Itoa(chunk)+"/"+strconv.Itoa(chunks),
		"questions", len(judged),
		"threshold", s.threshold,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
		"answers", answerList(judged, resp))
}

// answerList renders one request's answers as "name=0.912, name=0.044" in
// probability order (ties by name), with "name=-" for a question the service
// left unanswered.
//
// Order is the point: the top of the list is where the threshold decides, and
// the "-" is the one reading that must never look like a zero — an unanswered
// question is not judged, so it is not selected (see judgeQuery).
func answerList(judged []*DeferredTool, resp *jev.Response) string {
	type scored struct {
		name string
		p    float64
		got  bool
	}
	entries := make([]scored, 0, len(judged))
	for _, t := range judged {
		e := scored{name: t.Name}
		if a, ok := resp.Answers[t.Name]; ok && a.Noul != nil {
			e.p, e.got = *a.Noul, true
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].got != entries[j].got {
			return entries[i].got // answered before unanswered
		}
		if entries[i].p != entries[j].p {
			return entries[i].p > entries[j].p
		}
		return entries[i].name < entries[j].name
	})

	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.name)
		b.WriteString("=")
		if !e.got {
			b.WriteString("-")
			continue
		}
		b.WriteString(strconv.FormatFloat(e.p, 'f', 3, 64))
	}
	return b.String()
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

// recordUsage writes the ledger row for one judgment call through the shared
// llm.RecordJevUsage, which owns the pricing order and the credit arithmetic
// (the channel's notify gate writes the same row through it).
//
// The jev block is read HERE rather than captured when the searcher was built:
// the row carries a price snapshot, and that snapshot has to be the price in
// force when the call was made. The searcher's other settings (threshold,
// budget, the client) are build-time because they shape the SEARCH.
func (s *JevSearcher) recordUsage(ctx context.Context, resp *jev.Response) {
	if s.usage == nil || resp == nil {
		return
	}
	err := llm.RecordJevUsage(s.usage, s.jevConfig(), s.judge.Model(),
		tools.SessionIDFromCtx(ctx), resp.Usage.InputTokens, resp.Usage.OutputTokens, time.Now())
	if err != nil {
		logger.FromContext(ctx).Warn(ctx, "MCP: jev usage not recorded", "err", err)
	}
}

// jevConfig is the live jev block. A searcher built without a config (a test)
// reads as the zero block: no override, no credit rate, and the built-in table
// still prices the row.
func (s *JevSearcher) jevConfig() config.JevConfig {
	if s.cfg == nil {
		return config.JevConfig{}
	}
	return s.cfg.Jev
}
