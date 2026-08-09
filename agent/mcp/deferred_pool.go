package mcp

import (
	"sort"
	"strings"
	"unicode"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/bm25"
	"github.com/monsterxx03/tachi/pkg/container"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// DeferredTool holds metadata about an MCP tool for search purposes
// and the actual tool instance for lazy registration.
type DeferredTool struct {
	Name        string       // "mcp__postgres__query"
	ServerName  string       // "postgres"
	Description string       // original description from MCP
	SearchHint  string       // search keywords hint
	Schema      tools.Schema // full parameter schema
	Tool        tools.Tool   // actual MCP tool instance for lazy registration

	// Cached search fields — computed once at creation, never modified.
	// Avoids repeated ToLower/parseToolName/regex compilation in hot search paths.
	nameParts   []string // parsed tool name parts (lowercased)
	descLower   string
	hintLower   string
	serverLower string
}

// deferredToolPtr disambiguates container.LockedMap[string]*DeferredTool in
// the struct field (Go parses the trailing * as part of the type but the
// field position needs the alias).
type deferredToolPtr = *DeferredTool

// DeferredPool stores all MCP tools that are not yet loaded into the
// LLM's active tool set. Thread-safe.
type DeferredPool struct {
	tools container.LockedMap[string, deferredToolPtr]
}

// BM25 field boosts for keyword search, mirroring the previous hand-tuned
// weights (server 15 > name 10 > hint 4 > desc 2): the server name is the
// strongest signal, then the tool name, then the curated search hint, then
// the raw description. Field order is positional — bm25 matches fields by
// index — so it must stay in sync with newSearchIndex.
const (
	searchBoostServer = 6
	searchBoostName   = 4
	searchBoostHint   = 2
	searchBoostDesc   = 1
)

// NewDeferredPool creates an empty deferred pool.
func NewDeferredPool() *DeferredPool {
	return &DeferredPool{}
}

// Add inserts a tool into the pool.
func (p *DeferredPool) Add(t *DeferredTool) {
	p.tools.Store(t.Name, t)
}

// Get returns a tool by name, or nil if not found.
func (p *DeferredPool) Get(name string) *DeferredTool {
	v, _ := p.tools.Load(name)
	return v
}

// Len returns the number of tools in the pool.
func (p *DeferredPool) Len() int {
	return p.tools.Len()
}

// All returns all tools in the pool (for directory listing).
func (p *DeferredPool) All() []*DeferredTool {
	all := make([]*DeferredTool, 0, p.tools.Len())
	p.tools.Range(func(_ string, t *DeferredTool) bool {
		all = append(all, t)
		return true
	})
	sort.Slice(all, func(i, j int) bool {
		return all[i].Name < all[j].Name
	})
	return all
}

// RemoveByServer removes all tools belonging to the given server from the pool.
// Returns the number of tools removed.
func (p *DeferredPool) RemoveByServer(serverName string) int {
	var toRemove []string
	p.tools.Range(func(name string, t *DeferredTool) bool {
		if t.ServerName == serverName {
			toRemove = append(toRemove, name)
		}
		return true
	})
	for _, name := range toRemove {
		p.tools.Delete(name)
	}
	return len(toRemove)
}

// Remove removes a single tool from the pool by its full name.
// Returns the removed tool, or nil if not found.
func (p *DeferredPool) Remove(name string) *DeferredTool {
	v, _ := p.tools.LoadAndDelete(name)
	return v
}

// SearchResult is a single search result returned by Search.
type SearchResult = tools.MCPSearchResultItem

// Search finds tools matching the query string.
//
// Query forms:
//   - "select:Name1,Name2"     — fetch exact tools by name (comma-separated)
//   - "keyword1 keyword2"      — keyword search, scored by relevance
//   - "+mustHave term"         — "+" prefix means term is required in name/server
func (p *DeferredPool) Search(query string, maxResults int) []SearchResult {
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 20 {
		maxResults = 20
	}

	allTools := make([]*DeferredTool, 0, p.tools.Len())
	p.tools.Range(func(_ string, t *DeferredTool) bool {
		allTools = append(allTools, t)
		return true
	})

	query = strings.TrimSpace(query)
	if query == "" {
		return p.searchAll(allTools, maxResults)
	}

	// 1. Direct selection via "select:" prefix.
	// When a name doesn't include the "mcp__server__" prefix (i.e. the user gives just
	// the tool name like "switch_backend_endpoint"), a suffix fallback matches against
	// the full "mcp__server__tool" name so the user doesn't need to know which server
	// hosts each tool.
	if sel, ok := strings.CutPrefix(query, "select:"); ok {
		var results []SearchResult
		seen := container.NewSet[string]() // dedup across match strategies
		for _, name := range strutil.SplitBy(sel, ",") {
			// Phase 1: exact full-name match (e.g. "mcp__pg__query")
			found := false
			for _, t := range allTools {
				if strings.EqualFold(t.Name, name) {
					if !seen.Has(t.Name) {
						results = append(results, p.toResult(t))
						seen.Add(t.Name)
					}
					found = true
					break
				}
			}
			if found {
				continue
			}
			// Phase 2: suffix match — query is just the tool name without server prefix
			// e.g. "switch_backend_endpoint" matches "mcp__hoyocloud__switch_backend_endpoint"
			suffix := "__" + name
			for _, t := range allTools {
				if strings.HasSuffix(strings.ToLower(t.Name), strings.ToLower(suffix)) {
					if !seen.Has(t.Name) {
						results = append(results, p.toResult(t))
						seen.Add(t.Name)
					}
				}
			}
		}
		return results
	}

	// 3. Server prefix match: "mcp__server" → find all tools for that server
	if strings.HasPrefix(query, "mcp__") {
		prefix := strings.ToLower(query)
		var results []SearchResult
		for _, t := range allTools {
			if strings.HasPrefix(strings.ToLower(t.Name), prefix) {
				results = append(results, p.toResult(t))
				if len(results) >= maxResults {
					break
				}
			}
		}
		if len(results) > 0 {
			return results
		}
	}

	// 4. Keyword search with scoring
	return p.keywordSearch(allTools, query, maxResults)
}

func (p *DeferredPool) searchAll(allTools []*DeferredTool, maxResults int) []SearchResult {
	n := min(len(allTools), maxResults)
	results := make([]SearchResult, n)
	for i := range n {
		results[i] = p.toResult(allTools[i])
	}
	return results
}

func (p *DeferredPool) toResult(t *DeferredTool) SearchResult {
	return SearchResult{
		Name:        t.Name,
		Description: t.Description,
		Schema:      t.Schema,
	}
}

// keywordSearch scores tools by BM25 relevance and returns the top results.
//
// The index is rebuilt from scratch on every call: the corpus is small (tens
// to hundreds of short documents), so rebuild cost is negligible, and it
// keeps Search free of shared mutable state — safe to call concurrently with
// Add/Remove.
func (p *DeferredPool) keywordSearch(tools []*DeferredTool, query string, maxResults int) []SearchResult {
	query = strings.TrimSpace(query)
	required, scoring := parseSearchQuery(query)

	ix := newSearchIndex(tools)
	scores := ix.Scores(scoring)

	type scored struct {
		tool  *DeferredTool
		score float64
		doc   int
	}
	var ranked []scored
	for i, t := range tools {
		if scores[i] <= 0 {
			continue
		}
		// BM25 has no notion of required terms; enforce them as a pre-filter.
		if !matchesRequired(ix, i, required) {
			continue
		}
		ranked = append(ranked, scored{tool: t, score: scores[i], doc: i})
	}

	// Sort by score descending; ties broken by document order for stability.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].doc < ranked[j].doc
	})

	n := min(len(ranked), maxResults)
	results := make([]SearchResult, n)
	for i := range n {
		results[i] = p.toResult(ranked[i].tool)
	}
	return results
}

// parseSearchQuery splits a search query into required and scoring terms.
//
// Terms are split on underscores and CamelCase (so "get_mcp_server_detail"
// yields four tokens) and deduplicated — bm25 does not deduplicate query
// terms, each occurrence would inflate the score. A "+" prefix marks a term
// as required: every result must contain it. Required terms also participate
// in scoring, matching the previous behavior.
func parseSearchQuery(query string) (required, scoring []string) {
	seen := make(map[string]bool)
	for _, tok := range tokenize(query) {
		term := strings.TrimPrefix(tok, "+")
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		scoring = append(scoring, term)
		if strings.HasPrefix(tok, "+") {
			required = append(required, term)
		}
	}
	return required, scoring
}

// newSearchIndex builds a fresh BM25 index over the given tools.
//
// Field order (0-3) is the positional contract with bm25 — keep in sync with
// the boost constants above. The server name appears both inside the
// name-part field (parseToolName includes the server segment) and in its own
// field; the double hit deliberately reinforces the "looking for a server"
// signal, as the previous scoring did.
func newSearchIndex(tools []*DeferredTool) *bm25.Index {
	docs := make([]bm25.Document, len(tools))
	for i, t := range tools {
		docs[i] = bm25.Document{Fields: []bm25.Field{
			// 0: tool name parts ("mcp__pg__query" → [pg, query])
			{Boost: searchBoostName, Tokens: t.nameParts},
			// 1: server name ("postgres")
			{Boost: searchBoostServer, Tokens: tokenizeField(t.serverLower)},
			// 2: curated search hint
			{Boost: searchBoostHint, Tokens: tokenizeField(t.hintLower)},
			// 3: full description
			{Boost: searchBoostDesc, Tokens: tokenizeField(t.descLower)},
		}}
	}
	return bm25.New(docs, bm25.DefaultParams())
}

// tokenizeField splits a pre-lowercased metadata string into search tokens:
// words are split on underscores and CamelCase, with stop words and empties
// removed (bm25 expects pre-normalized tokens). Used for the server, search
// hint and description fields; tool name parts are already tokens.
func tokenizeField(s string) []string {
	var toks []string
	for _, word := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		for _, sub := range splitOnUnderscoreOrCamel(word) {
			if sub != "" && !isStopWord(sub) {
				toks = append(toks, sub)
			}
		}
	}
	return toks
}

// matchesRequired reports whether document doc contains every required term.
// A term "matches" when its BM25 score against the document alone is
// non-zero, i.e. the term appears in at least one indexed field.
func matchesRequired(ix *bm25.Index, doc int, required []string) bool {
	for _, term := range required {
		if ix.Score([]string{term}, doc) <= 0 {
			return false
		}
	}
	return true
}

// "mcp__postgres__query" → ["postgres", "query"]
// "mcp__github__createPullRequest" → ["github", "create", "pull", "request"]
func parseToolName(name string) []string {
	if !strings.HasPrefix(name, "mcp__") {
		return []string{strings.ToLower(name)}
	}
	rest := name[5:] // keep original case for CamelCase detection
	var parts []string
	for segment := range strings.SplitSeq(rest, "__") {
		for _, word := range splitOnUnderscoreOrCamel(segment) {
			if word != "" {
				parts = append(parts, strings.ToLower(word))
			}
		}
	}
	return parts
}

// splitOnUnderscoreOrCamel splits a string on underscores and CamelCase boundaries.
// Handles acronyms correctly: "SQL" stays ["sql"], "HTTPServer" → ["http", "server"],
// "getHTTPResponse" → ["get", "http", "response"], "camelCase" → ["camel", "case"].
func splitOnUnderscoreOrCamel(s string) []string {
	// First split on underscores
	var segments []string
	for part := range strings.SplitSeq(s, "_") {
		if part == "" {
			continue
		}
		// Then split CamelCase within each part
		var words []string
		start := 0
		for i := 1; i < len(part); i++ {
			prev := part[i-1]
			curr := part[i]

			if isLowerByte(prev) && isUpperByte(curr) {
				// Standard CamelCase: lowercase→uppercase transition
				// e.g., "camelCase" → split before 'C'
				words = append(words, strings.ToLower(part[start:i]))
				start = i
			} else if isUpperByte(prev) && isLowerByte(curr) {
				// End of acronym: the last uppercase belongs to the following lowercase word
				// e.g., "HTTPServer" at 'S':'e' → split before the last uppercase in the run
				// i-1 is the index of the last uppercase before the lowercase transition
				if i-1 > start {
					words = append(words, strings.ToLower(part[start:i-1]))
					start = i - 1
				}
			}
		}
		if start < len(part) {
			words = append(words, strings.ToLower(part[start:]))
		}
		segments = append(segments, words...)
	}
	return segments
}

// isUpperByte reports whether b is an ASCII uppercase letter.
func isUpperByte(b byte) bool { return b >= 'A' && b <= 'Z' }

// isLowerByte reports whether b is an ASCII lowercase letter.
func isLowerByte(b byte) bool { return b >= 'a' && b <= 'z' }

// tokenize splits a query string into lowercase search terms.
// Whitespace-separated tokens are further split on underscores and CamelCase
// (using the same splitOnUnderscoreOrCamel as parseToolName) so that queries
// like "get_mcp_server_detail" or "+getResult" match the tool's parsed name parts.
func tokenize(query string) []string {
	fields := strings.Fields(query)
	var result []string
	for _, f := range fields {
		clean := strings.TrimSpace(f)
		if clean == "" {
			continue
		}
		if strings.HasPrefix(clean, "+") && len(clean) > 1 {
			term := strings.TrimLeft(clean, "+")
			for _, sub := range splitOnUnderscoreOrCamel(term) {
				if sub != "" {
					result = append(result, "+"+sub)
				}
			}
		} else {
			for _, sub := range splitOnUnderscoreOrCamel(clean) {
				if sub != "" {
					result = append(result, sub)
				}
			}
		}
	}
	return result
}

// NewDeferredToolFromMCPTool converts an MCPTool to a DeferredTool for storage.
// If searchHintOverride is non-empty, it overrides the auto-generated search hint.
func NewDeferredToolFromMCPTool(t MCPTool, searchHintOverride string) *DeferredTool {
	// Build search hint from name parts + description (or use override)
	hint := searchHintOverride
	if hint == "" {
		hint = buildSearchHint(t)
	}

	name := t.Name()
	serverName := t.serverName
	desc := t.serverTool.Description

	return &DeferredTool{
		Name:        name,
		ServerName:  serverName,
		Description: desc,
		SearchHint:  hint,
		Schema:      tools.ToSchema(t),
		Tool:        t,
		// Cached search fields — computed once, reused across all searches
		nameParts:   parseToolName(name),
		descLower:   strings.ToLower(desc),
		hintLower:   strings.ToLower(hint),
		serverLower: strings.ToLower(serverName),
	}
}

// buildSearchHint generates a search hint from the tool's name and description.
// In future, this can read _meta.anthropic/searchHint from the MCP tool.
func buildSearchHint(t MCPTool) string {
	// Start with server name
	parts := []string{t.serverName}

	// Add tool name parts (after server name separator)
	toolName := t.serverTool.Name
	for _, w := range splitOnUnderscoreOrCamel(toolName) {
		if w != "" && !contains(parts, w) {
			parts = append(parts, w)
		}
	}

	// Add key nouns from description (simple heuristic: first 5 significant words)
	desc := t.serverTool.Description
	if desc != "" {
		words := strings.Fields(desc)
		count := 0
		for _, w := range words {
			w = strings.Trim(w, ".,;:!?()[]{}")
			if len(w) > 3 && !isStopWord(w) && !contains(parts, strings.ToLower(w)) {
				parts = append(parts, strings.ToLower(w))
				count++
				if count >= 5 {
					break
				}
			}
		}
	}

	return strings.Join(parts, ", ")
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func isStopWord(w string) bool {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "can": true, "shall": true,
		"to": true, "of": true, "in": true, "for": true, "on": true,
		"with": true, "at": true, "by": true, "from": true, "as": true,
		"into": true, "through": true, "during": true, "before": true,
		"after": true, "above": true, "below": true, "between": true,
		"and": true, "but": true, "or": true, "nor": true, "not": true,
		"so": true, "yet": true, "this": true, "that": true, "these": true,
		"those": true, "it": true, "its": true, "them": true, "their": true,
	}
	return stopWords[strings.ToLower(w)]
}
