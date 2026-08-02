package mcp

import (
	"regexp"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/container"
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

// searchTerm is a pre-compiled search term for efficient matching.
// The regex pattern is compiled once per query, not per tool.
type searchTerm struct {
	raw      string
	required bool
	pattern  *regexp.Regexp // compiled \b word boundary pattern
}

// compileWordPattern compiles a word-boundary regex for the given term.
func compileWordPattern(term string) *regexp.Regexp {
	pattern := `\b` + regexp.QuoteMeta(term) + `\b`
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

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
		names := strings.Split(sel, ",")
		var results []SearchResult
		seen := container.NewSet[string]() // dedup across match strategies
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
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

// keywordSearch scores tools by keyword relevance and returns top results.
// Uses pre-compiled regex patterns and cached lowercase fields to avoid
// repeated allocations in the inner loop.
func (p *DeferredPool) keywordSearch(tools []*DeferredTool, query string, maxResults int) []SearchResult {
	query = strings.TrimSpace(query)
	tokens := tokenize(query) // tokenize first — CamelCase detection needs original case

	// Build pre-compiled search terms — regex compiled once per term, reused across all tools
	var allTerms []searchTerm
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "+") && len(tok) > 1 {
			allTerms = append(allTerms, searchTerm{
				raw:      tok[1:],
				required: true,
				pattern:  compileWordPattern(tok[1:]),
			})
		} else {
			allTerms = append(allTerms, searchTerm{
				raw:     tok,
				pattern: compileWordPattern(tok),
			})
		}
	}

	// Partition: dedup scoring terms, collect required terms for pre-filter
	seen := make(map[string]bool)
	var requiredTerms, scoringTerms []searchTerm
	for _, st := range allTerms {
		if !seen[st.raw] {
			seen[st.raw] = true
			scoringTerms = append(scoringTerms, st)
		}
		if st.required {
			requiredTerms = append(requiredTerms, st)
		}
	}

	type scored struct {
		tool  *DeferredTool
		score int
	}
	var scoredTools []scored

	for _, t := range tools {
		// Pre-filter: ALL required terms must match
		skip := false
		for _, rt := range requiredTerms {
			if !matchesAny(rt, t) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Single scoring pass — uses cached fields + pre-compiled regex
		totalScore := 0
		for _, st := range scoringTerms {
			totalScore += scoreTool(st, t)
		}

		if totalScore > 0 {
			scoredTools = append(scoredTools, scored{tool: t, score: totalScore})
		}
	}

	// Sort by score descending
	sort.Slice(scoredTools, func(i, j int) bool {
		return scoredTools[i].score > scoredTools[j].score
	})

	n := min(len(scoredTools), maxResults)
	results := make([]SearchResult, n)
	for i := range n {
		results[i] = p.toResult(scoredTools[i].tool)
	}
	return results
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

// matchesAny checks if a search term matches any part of the tool metadata.
// Uses pre-compiled regex and cached lowercase fields — no regex compilation in hot path.
func matchesAny(st searchTerm, t *DeferredTool) bool {
	termLower := strings.ToLower(st.raw)

	// Server name
	if t.serverLower == termLower || strings.Contains(t.serverLower, termLower) {
		return true
	}

	// Tool name parts (pre-parsed, lowercased)
	for _, p := range t.nameParts {
		if p == termLower || strings.Contains(p, termLower) {
			return true
		}
	}

	// Search hint (pre-compiled regex)
	if st.pattern != nil && st.pattern.MatchString(t.hintLower) {
		return true
	}

	// Description (pre-compiled regex)
	if st.pattern != nil && st.pattern.MatchString(t.descLower) {
		return true
	}

	return false
}

// scoreTool calculates a relevance score for a search term against tool metadata.
// Uses pre-compiled regex and cached lowercase fields — no regex compilation in hot path.
func scoreTool(st searchTerm, t *DeferredTool) int {
	termLower := strings.ToLower(st.raw)
	score := 0

	// Server name match (highest — user likely looking for a specific server)
	if t.serverLower == termLower {
		score += 15
	} else if strings.Contains(t.serverLower, termLower) {
		score += 8
	}

	// Tool name parts (pre-parsed, lowercased)
	for _, p := range t.nameParts {
		if p == termLower {
			score += 10
		} else if strings.Contains(p, termLower) {
			score += 5
		}
	}

	// Search hint (curated signal, pre-compiled regex)
	if st.pattern != nil && st.pattern.MatchString(t.hintLower) {
		score += 4
	}

	// Description (pre-compiled regex)
	if st.pattern != nil && st.pattern.MatchString(t.descLower) {
		score += 2
	}

	return score
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
