package mcp

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/monsterxx03/tachi/agent/tools"
)

// DeferredTool holds metadata about an MCP tool for search purposes.
type DeferredTool struct {
	Name        string       // "mcp__postgres__query"
	ServerName  string       // "postgres"
	Description string       // original description from MCP
	SearchHint  string       // search keywords hint
	Schema      tools.Schema // full parameter schema
}

// DeferredPool stores all MCP tools that are not yet loaded into the
// LLM's active tool set. Thread-safe.
type DeferredPool struct {
	mu    sync.RWMutex
	tools map[string]*DeferredTool
}

// NewDeferredPool creates an empty deferred pool.
func NewDeferredPool() *DeferredPool {
	return &DeferredPool{tools: make(map[string]*DeferredTool)}
}

// Add inserts a tool into the pool.
func (p *DeferredPool) Add(t *DeferredTool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools[t.Name] = t
}

// Get returns a tool by name, or nil if not found.
func (p *DeferredPool) Get(name string) *DeferredTool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tools[name]
}

// Len returns the number of tools in the pool.
func (p *DeferredPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.tools)
}

// All returns all tools in the pool (for directory listing).
func (p *DeferredPool) All() []*DeferredTool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*DeferredTool, 0, len(p.tools))
	for _, t := range p.tools {
		result = append(result, t)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// SearchResult is a single search result returned by Search.
type SearchResult = tools.MCPSearchResultItem

// Search finds tools matching the query string.
//
// Query forms:
//   - "exact:ToolName"          — exact name match, returns that tool directly
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

	p.mu.RLock()
	allTools := make([]*DeferredTool, 0, len(p.tools))
	for _, t := range p.tools {
		allTools = append(allTools, t)
	}
	p.mu.RUnlock()

	query = strings.TrimSpace(query)
	if query == "" {
		return p.searchAll(allTools, maxResults)
	}

	// 1. Exact match via "exact:" prefix
	if exact, ok := strings.CutPrefix(query, "exact:"); ok {
		name := strings.TrimSpace(exact)
		for _, t := range allTools {
			if strings.EqualFold(t.Name, name) {
				return []SearchResult{p.toResult(t)}
			}
		}
		return nil
	}

	// 2. Direct selection via "select:" prefix
	if sel, ok := strings.CutPrefix(query, "select:"); ok {
		names := strings.Split(sel, ",")
		var results []SearchResult
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			for _, t := range allTools {
				if strings.EqualFold(t.Name, name) {
					results = append(results, p.toResult(t))
					break
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
	for i := 0; i < n; i++ {
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
func (p *DeferredPool) keywordSearch(tools []*DeferredTool, query string, maxResults int) []SearchResult {
	query = strings.ToLower(strings.TrimSpace(query))
	terms := tokenize(query)

	// Partition into required (+prefixed) and optional terms
	var required, optional []string
	for _, term := range terms {
		if strings.HasPrefix(term, "+") && len(term) > 1 {
			required = append(required, term[1:])
		} else {
			optional = append(optional, term)
		}
	}
	allScoringTerms := optional
	if len(required) > 0 {
		allScoringTerms = append(required, optional...)
	}

	type scored struct {
		tool  *DeferredTool
		score int
	}
	var scoredTools []scored

	for _, t := range tools {
		parts := parseToolName(t.Name)

		// Pre-filter: ALL required terms must match
		if len(required) > 0 {
			matchesAll := true
			for _, req := range required {
				if !matchesAny(req, parts, t.Description, t.SearchHint) {
					matchesAll = false
					break
				}
			}
			if !matchesAll {
				continue
			}
		}

		// Score against all terms
		totalScore := 0
		for _, term := range allScoringTerms {
			totalScore += scoreTool(term, parts, t.Description, t.SearchHint, t.ServerName)
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
	for i := 0; i < n; i++ {
		results[i] = p.toResult(scoredTools[i].tool)
	}
	return results
}

// parseToolName splits a tool name into searchable parts.
// "mcp__postgres__query" → ["postgres", "query"]
// "mcp__github__create_pr" → ["github", "create", "pr"]
func parseToolName(name string) []string {
	if !strings.HasPrefix(name, "mcp__") {
		return []string{strings.ToLower(name)}
	}
	rest := strings.ToLower(name[5:]) // strip "mcp__"
	var parts []string
	for _, segment := range strings.Split(rest, "__") {
		for _, word := range splitOnUnderscoreOrCamel(segment) {
			if word != "" {
				parts = append(parts, word)
			}
		}
	}
	return parts
}

// splitOnUnderscoreOrCamel splits a string on underscores and CamelCase boundaries.
func splitOnUnderscoreOrCamel(s string) []string {
	// First split on underscores
	var segments []string
	for _, part := range strings.Split(s, "_") {
		if part == "" {
			continue
		}
		// Then split CamelCase within each part
		var words []string
		start := 0
		for i, r := range part {
			if i > 0 && unicode.IsUpper(r) {
				words = append(words, strings.ToLower(part[start:i]))
				start = i
			}
		}
		if start < len(part) {
			words = append(words, strings.ToLower(part[start:]))
		}
		segments = append(segments, words...)
	}
	return segments
}

// tokenize splits a query string into lowercase terms.
func tokenize(query string) []string {
	// Split on whitespace
	fields := strings.Fields(query)
	// Remove empty and single-char terms (except +prefix)
	var result []string
	for _, f := range fields {
		clean := strings.TrimSpace(f)
		if clean == "" {
			continue
		}
		if strings.HasPrefix(clean, "+") && len(clean) > 1 {
			term := strings.TrimSpace(clean[1:])
			if term != "" {
				result = append(result, "+"+term)
			}
		} else if len(clean) >= 1 {
			result = append(result, clean)
		}
	}
	return result
}

// matchesAny checks if a term matches any part of the tool metadata.
func matchesAny(term string, parts []string, description, searchHint string) bool {
	termLower := strings.ToLower(term)
	descLower := strings.ToLower(description)
	hintLower := strings.ToLower(searchHint)

	// Check tool name parts
	for _, p := range parts {
		if p == termLower {
			return true
		}
		if strings.Contains(p, termLower) {
			return true
		}
	}

	// Check search hint (compiled regex for word boundary)
	pattern := `\b` + regexp.QuoteMeta(termLower) + `\b`
	if matched, _ := regexp.MatchString(pattern, hintLower); matched {
		return true
	}

	// Check description
	if matched, _ := regexp.MatchString(pattern, descLower); matched {
		return true
	}

	return false
}

// scoreTool calculates a relevance score for a term against tool metadata.
func scoreTool(term string, parts []string, description, searchHint, serverName string) int {
	termLower := strings.ToLower(term)
	descLower := strings.ToLower(description)
	hintLower := strings.ToLower(searchHint)
	serverLower := strings.ToLower(serverName)

	score := 0

	// Server name match (highest — user looking for a specific server)
	if serverLower == termLower {
		score += 15
	} else if strings.Contains(serverLower, termLower) {
		score += 8
	}

	// Exact part match in tool name
	for _, p := range parts {
		if p == termLower {
			score += 10
		} else if strings.Contains(p, termLower) {
			score += 5
		}
	}

	// Search hint match (curated signal)
	pattern := `\b` + regexp.QuoteMeta(termLower) + `\b`
	if matched, _ := regexp.MatchString(pattern, hintLower); matched {
		score += 4
	}

	// Description match
	if matched, _ := regexp.MatchString(pattern, descLower); matched {
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

	return &DeferredTool{
		Name:        t.Name(),
		ServerName:  t.serverName,
		Description: t.serverTool.Description,
		SearchHint:  hint,
		Schema:      tools.ToSchema(t),
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
