package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// Tool name constant for the MCP search tool.
const ToolNameMCPSearchTools = "MCPSearchTools"

// maxMCPSearchMaxResults caps max_results both in the schema and at runtime.
// Schema constraints are hints to the model; ExecuteContext clamps so an
// out-of-range value can never fan out an unbounded search.
const maxMCPSearchMaxResults = 20

// MCPSearchResult holds a single tool search result, as the MODEL reads it.
//
// It is deliberately a MENU — the name of a tool that exists, and how strongly
// it matched. It carries no description and no parameter schema, because
// neither would be new information: the discovered tools are admitted into the
// NEXT request's tool list with their full schema from the pool
// (`filterActiveSchemas`), and a call resolves the tool from that same pool
// (`lazyRegisterMCPTool`). Rendering the definitions here too put a verbatim
// second copy of them in the transcript — measured at ~2.7 KB per search, half
// the payload, paid again in every later request's history.
//
// Score and MatchedBy describe HOW the tool was found. They are a pair on
// purpose: 0.21 (a Jev probability) and 3.7 (a BM25 score) are different
// quantities, so a bare number would invite a comparison across searches that
// means nothing. A tool found BY NAME carries neither — a lookup is not ranked.
type MCPSearchResult struct {
	Name      string   `json:"name"`
	MatchedBy string   `json:"matched_by,omitempty"`
	Score     *float64 `json:"score,omitempty"`
}

// MCPSearchOptions is one search request: what is being asked, which candidates
// it may be answered from, and how many results to cap at.
//
// It is a struct rather than more positional parameters because Server names a
// SET while Query and MaxResults bound a LOOKUP — two adjacent strings at a call
// site would be transposed silently, and a search answered from the wrong set
// looks exactly like a search that found nothing.
type MCPSearchOptions struct {
	// Query is the request as the model wrote it. A query that NAMES what it
	// wants (select:, an exact or suffix tool name, an mcp__server prefix) is
	// resolved against every server whatever Server says — a name is an
	// identity, and a mis-remembered server must not turn a hit into a miss.
	// An empty query is a LISTING, which is not an identity: it is answered
	// from Server's tools alone (DeferredPool.SearchExact). The tool refuses a
	// whitespace query before it gets here, so that is the pool's rule rather
	// than a spelling the model can reach by accident.
	Query string
	// Server restricts the candidate set to one MCP server's tools, spelled the
	// way the deferred-tools reminder lists it ("github"); NormalizeMCPServer
	// has already been applied by the tool. Empty means every server.
	Server string
	// MaxResults caps the answer (the tool clamps it before it gets here).
	MaxResults int
}

// What produced a result, i.e. what its Score MEANS. The searcher sets it, so a
// search that degraded mid-way says "keywords" for the results it produced that
// way — the label cannot drift from the matcher that actually ran.
const (
	// MatchByName: an exact/suffix name, a `select:` list, an `mcp__server`
	// prefix or the empty-query listing. A lookup, never ranked: Score is unused.
	MatchByName = "name"
	// MatchByJudgment: the Jev gate. Score is a noul probability in [0,1], and
	// every result already cleared the configured threshold.
	MatchByJudgment = "judgment"
	// MatchByKeywords: BM25. Score is an unbounded relevance number, comparable
	// within one search and meaningless outside it.
	MatchByKeywords = "keywords"
)

// MCPSearchResultItem is a raw search result from the pool.
// Defined here to avoid circular dependency; the pool converts to this.
//
// It carries no Schema: the item used to feed a `tool_definitions` block in the
// result, and that block is gone (see MCPSearchResult) — the schema a call needs
// comes from the pool, on the next request's tool list and again at call time.
type MCPSearchResultItem struct {
	Name string
	// MatchedBy is one of the MatchBy* constants ("" from a searcher that does
	// not report it, which renders as a result with no score at all).
	MatchedBy string
	// Score is the matcher's own number; meaningful only when MatchedBy says
	// which matcher it came from (see MatchByName).
	Score float64
}

// MCPServerLister is an optional MCPSearcher method: the MCP servers the
// searcher's candidates belong to. The tool asks for it to reject an unknown
// Server by naming the servers that do exist — searching nothing instead would
// be indistinguishable from "no tool matches", which is the reading that makes
// a model give up on the tool rather than correct the name.
type MCPServerLister interface {
	Servers() []string
}

// MCPServerPrefix is the segment every MCP tool name starts with.
const MCPServerPrefix = "mcp__"

// NormalizeMCPServer turns whatever the model wrote into the form the candidate
// set is matched against: the forms it can produce from reading the reminder
// block ("github", "mcp__github", " GITHUB ") all reduce to "github". An empty
// input stays empty, which means "every server".
//
// It lowercases because the comparison is case-insensitive on the pool side
// (the server's own spelling comes from config, not from the model).
func NormalizeMCPServer(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), MCPServerPrefix)
}

// What the tool's description tells the model to write. One of these is picked
// per searcher (see MCPSearchAdvice): the keyword path can only match terms
// that appear in the tools' own text and has no answer for a sentence in
// Chinese, while the Jev gate judges a request written in any language. A
// description that promised the wrong one would send the model down a path that
// returns nothing, and it would read as "there is no such tool".
const (
	KeywordSearchAdvice = "Search with keywords that appear in the tool names and descriptions (English)."
	ProseSearchAdvice   = "Describe what you are trying to do in a sentence — in any language, the request you would have made to a person."
	// ExactNameAdvice is true of every searcher: a name is a lookup, not a search.
	ExactNameAdvice = "To load a tool you already know, pass its name (or \"select:name1,name2\" for several, or \"mcp__server\" for every tool of a server) — a name is resolved exactly and skips the search."
)

// MCPSearchAdvice is an optional MCPSearcher method: how THIS searcher wants to
// be asked, in its own words. A searcher without it is described as a keyword
// one, which is what the pool does.
type MCPSearchAdvice interface {
	SearchAdvice() string
}

// ScoreAdvice frames the numbers a result carries, in one clause: the tool
// schemas ride on EVERY request (and were just trimmed for exactly that
// reason), so it earns its tokens only if it stops the one misreading that
// matters — the cut-off already said yes, so a low score is a weak match, not a
// wrong tool, and a model that reads it as a verdict drops the marginal tool the
// threshold was swept to include and spends a turn searching again.
const ScoreAdvice = "A result's `score` (with `matched_by`) is relative strength, not a verdict — every result already passed the cut-off."

// MCPSearcher is the interface for searching deferred MCP tools.
// Implemented by mcp.PoolSearcher and mcp.JevSearcher.
type MCPSearcher interface {
	// Search takes a context because a searcher may reach a service (the Jev
	// gate does): the call has to die with the turn that made it, and a search
	// that overruns degrades inside the implementation rather than blocking.
	Search(ctx context.Context, opts MCPSearchOptions) []MCPSearchResultItem
}

// MCPSearchTracker tracks which MCP tools the LLM has discovered.
// Implemented by mcp.DiscoveredSet.
type MCPSearchTracker interface {
	Add(name string)
	List() []string
}

// MCPSearchTrackerProvider resolves the tracker for a given session. The
// LLM's session ID is carried in the tool-call context (tools.WithSessionID,
// injected by the agent loop); the provider maps it to that session's
// discovered-tool set so discoveries are recorded per session.
// Implemented by mcp.Manager.
type MCPSearchTrackerProvider interface {
	TrackerFor(sessionID string) MCPSearchTracker
}

// MCPSearchToolsTool allows the LLM to search for and load MCP tools.
// Registered as a built-in tool when MCP servers are configured.
type MCPSearchToolsTool struct {
	searcher MCPSearcher
	provider MCPSearchTrackerProvider
}

// NewMCPSearchToolsTool creates a new MCPSearchTools tool.
func NewMCPSearchToolsTool(searcher MCPSearcher, provider MCPSearchTrackerProvider) *MCPSearchToolsTool {
	return &MCPSearchToolsTool{searcher: searcher, provider: provider}
}

func (t *MCPSearchToolsTool) Name() string { return ToolNameMCPSearchTools }

// Description carries the how-to-ask advice ONCE (the `query` property points
// back at it): the same sentence used to appear in both places, and the query
// property is read at the moment the model writes the request, which is what
// the sentence is about — but the description is what it reads while deciding
// to call at all, and a description that omits it is how the keyword path gets
// asked a question it cannot answer.
func (t *MCPSearchToolsTool) Description() string {
	return "Search for and load MCP tools. " +
		"MCP tools provide access to external services and are not loaded by default. " +
		t.advice() + " " +
		ExactNameAdvice + " " +
		"Once loaded, a tool can be called like any built-in tool."
}

// advice is what the searcher behind this tool says about how to ask it.
func (t *MCPSearchToolsTool) advice() string {
	if a, ok := t.searcher.(MCPSearchAdvice); ok {
		return a.SearchAdvice()
	}
	return KeywordSearchAdvice
}
func (t *MCPSearchToolsTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"query": {
			Type:        "string",
			Description: "What to look for, phrased the way the tool description explains.",
		},
		"server": {
			Type: "string",
			Description: "Optional. Search only this server's tools, named exactly as the deferred-tools " +
				"block lists them (the \"mcp__\" prefix is optional). A tool you NAME " +
				"is still resolved against every server.",
		},
		"max_results": {
			Type:        "integer",
			Description: "Maximum results to return (default 5, max 20).",
			Minimum:     new(1.0),
			Maximum:     new(float64(maxMCPSearchMaxResults)),
			Default:     5,
		},
	}
}

func (t *MCPSearchToolsTool) Required() []string {
	return []string{"query"}
}

func (t *MCPSearchToolsTool) Parallel() bool { return true }

// mcpSearchOutput is the whole result the model reads: a list of matches and
// nothing else.
//
// It used to echo the caller's `query` back and carry a `total_deferred_tools`
// count — both were dropped. The query is the model's OWN text from the previous
// message (a parallel search is still unambiguous, since each result is attached
// to its call by tool_call_id), and the count was `len(matches)` under a name
// that claimed to describe the POOL — the array already says how many there are,
// and how much the pool holds is the reminder block's line, which knows the
// difference between total and still-undiscovered.
type mcpSearchOutput struct {
	Matches []MCPSearchResult `json:"matches"`
}

func (t *MCPSearchToolsTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Query      string `json:"query"`
		Server     string `json:"server"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// TrimSpace, not == "": a whitespace-only query is a caller that forgot to
	// fill the field, and it must not be read as the empty-query listing. That
	// listing is a lookup over a page of the pool — a spelling the MODEL gets by
	// naming a server (`mcp__server`), never by leaving the field nearly blank —
	// and interpreting a stray space as one silently loaded another server's
	// tools into the next request's tool list (see DeferredPool.SearchExact).
	if strings.TrimSpace(params.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	if params.MaxResults <= 0 {
		params.MaxResults = 5
	}
	// Clamp to the schema-declared upper bound; schema constraints are hints,
	// not enforcement (see maxMCPSearchMaxResults).
	params.MaxResults = min(params.MaxResults, maxMCPSearchMaxResults)

	server := NormalizeMCPServer(params.Server)
	if err := t.checkServer(server, params.Server); err != nil {
		return "", err
	}

	// Search the deferred pool
	results := t.searcher.Search(ctx, MCPSearchOptions{
		Query:      params.Query,
		Server:     server,
		MaxResults: params.MaxResults,
	})

	// Resolve the tracker for the session active in this tool call. The
	// agent loop injects the session ID into the context (tools.WithSessionID);
	// a missing ID (e.g. non-loop invocation) means we still return search
	// results but don't persist the discovery to any session.
	sid := SessionIDFromCtx(ctx)
	tracker := t.provider.TrackerFor(sid)

	// Mark matched tools as discovered in the active session's set. That is what
	// makes them appear — with their full schema, taken from the pool — in the
	// NEXT request's tool list (see mcpSearchOutput), so this loop is the whole
	// "loading" step: nothing is rendered here beyond the names.
	for _, r := range results {
		if tracker != nil {
			tracker.Add(r.Name)
		}
	}

	// Build search result metadata. Score rides with its provenance: it is
	// emitted only when a matcher produced the result (a tool found by NAME was
	// not ranked, and a 0 there would read as "matched terribly"), and rounded
	// to three decimals — the same precision the judgment's own log line uses,
	// because anything finer is false precision on a probability.
	meta := make([]MCPSearchResult, len(results))
	for i, r := range results {
		meta[i] = MCPSearchResult{
			Name:      r.Name,
			MatchedBy: r.MatchedBy,
		}
		if r.MatchedBy != "" && r.MatchedBy != MatchByName {
			score := math.Round(r.Score*1000) / 1000
			meta[i].Score = &score
		}
	}

	output := mcpSearchOutput{Matches: meta}

	b, err := fileutil.MarshalJSON(output)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}

// checkServer rejects a `server` the searcher does not have, naming the ones it
// does. server is the normalized name, asWritten the model's spelling (so the
// message quotes back what it wrote).
//
// An empty answer is what this prevents: it is indistinguishable from "no tool
// matches", and a model that reads it that way stops trying instead of fixing
// the name. A searcher that cannot list its servers is not validated — the
// search goes through and an unknown name simply matches nothing, which is the
// behaviour every caller had before this parameter existed.
//
// It runs before the search, so it also covers a query that NAMES a tool: an
// unusable `server` is a malformed argument, and the model is told to drop it
// rather than served an answer about a candidate set it never asked for.
func (t *MCPSearchToolsTool) checkServer(server, asWritten string) error {
	if server == "" {
		return nil
	}
	lister, ok := t.searcher.(MCPServerLister)
	if !ok {
		return nil
	}
	known := lister.Servers()
	for _, name := range known {
		if strings.EqualFold(name, server) {
			return nil
		}
	}
	if len(known) == 0 {
		return fmt.Errorf("unknown MCP server %q: no MCP server is connected yet, so there is nothing to search; call this tool again without the server parameter", asWritten)
	}
	return fmt.Errorf("unknown MCP server %q; the connected MCP servers are: %s",
		asWritten, strings.Join(known, ", "))
}

// IsMCPSchema returns true if the schema name is an MCP tool.
func IsMCPSchema(name string) bool {
	return strings.HasPrefix(name, MCPServerPrefix)
}

// IsMCPSearchTool returns true if the name is the MCP search tool itself.
func IsMCPSearchTool(name string) bool {
	return name == ToolNameMCPSearchTools
}
