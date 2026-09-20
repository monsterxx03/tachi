package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// Tool name constant for the MCP search tool.
const ToolNameMCPSearchTools = "MCPSearchTools"

// maxMCPSearchMaxResults caps max_results both in the schema and at runtime.
// Schema constraints are hints to the model; ExecuteContext clamps so an
// out-of-range value can never fan out an unbounded search.
const maxMCPSearchMaxResults = 20

// MCPSearchResult holds a single tool search result.
// The Schema is serialized as JSON for the LLM to read.
type MCPSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
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

// MCPSearcher is the interface for searching deferred MCP tools.
// Implemented by mcp.PoolSearcher and mcp.JevSearcher.
type MCPSearcher interface {
	// Search takes a context because a searcher may reach a service (the Jev
	// gate does): the call has to die with the turn that made it, and a search
	// that overruns degrades inside the implementation rather than blocking.
	Search(ctx context.Context, query string, maxResults int) []MCPSearchResultItem
}

// MCPSearchResultItem is a raw search result from the pool.
// Defined here to avoid circular dependency; the pool converts to this.
type MCPSearchResultItem struct {
	Name        string
	Description string
	Schema      Schema
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

func (t *MCPSearchToolsTool) Description() string {
	return "Search for and load MCP tools. " +
		"MCP tools provide access to external services and are not loaded by default — " +
		"use this tool to find and load them. " +
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
			Description: t.advice() + " " + ExactNameAdvice,
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

type mcpSearchOutput struct {
	Matches  []MCPSearchResult `json:"matches"`
	Query    string            `json:"query"`
	Total    int               `json:"total_deferred_tools"`
	ToolDefs map[string]any    `json:"tool_definitions,omitempty"`
}

func (t *MCPSearchToolsTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	if params.MaxResults <= 0 {
		params.MaxResults = 5
	}
	// Clamp to the schema-declared upper bound; schema constraints are hints,
	// not enforcement (see maxMCPSearchMaxResults).
	params.MaxResults = min(params.MaxResults, maxMCPSearchMaxResults)

	// Search the deferred pool
	results := t.searcher.Search(ctx, params.Query, params.MaxResults)

	// Resolve the tracker for the session active in this tool call. The
	// agent loop injects the session ID into the context (tools.WithSessionID);
	// a missing ID (e.g. non-loop invocation) means we still return search
	// results but don't persist the discovery to any session.
	sid := SessionIDFromCtx(ctx)
	tracker := t.provider.TrackerFor(sid)

	// Mark matched tools as discovered in the active session's set
	toolDefs := make(map[string]any)
	for _, r := range results {
		if tracker != nil {
			tracker.Add(r.Name)
		}

		// Build a JSON-friendly tool definition for the LLM
		toolDefs[r.Name] = map[string]any{
			"description": r.Description,
			"parameters": map[string]any{
				"type":       r.Schema.Parameters.Type,
				"properties": schemaToPropsJSON(r.Schema.Parameters.Properties),
				"required":   r.Schema.Parameters.Required,
			},
		}
	}

	// Build search result metadata
	meta := make([]MCPSearchResult, len(results))
	for i, r := range results {
		meta[i] = MCPSearchResult{
			Name:        r.Name,
			Description: r.Description,
		}
	}

	output := mcpSearchOutput{
		Matches:  meta,
		Query:    params.Query,
		Total:    len(meta),
		ToolDefs: toolDefs,
	}

	b, err := fileutil.MarshalJSON(output)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}

// schemaToPropsJSON converts PropertySchema map to JSON-compatible map.
// A property with no usable type is emitted without the keyword — the tool
// search result must describe the same schema the API request will carry, and
// "type": "" is not valid JSON Schema.
func schemaToPropsJSON(props map[string]PropertySchema) map[string]any {
	result := make(map[string]any, len(props))
	for name, prop := range props {
		p := map[string]any{}
		if prop.Type != "" {
			p["type"] = prop.Type
		}
		if prop.Description != "" {
			p["description"] = prop.Description
		}
		if len(prop.Enum) > 0 {
			p["enum"] = prop.Enum
		}
		if prop.Format != "" {
			p["format"] = prop.Format
		}
		if prop.Minimum != nil {
			p["minimum"] = *prop.Minimum
		}
		if prop.Maximum != nil {
			p["maximum"] = *prop.Maximum
		}
		if prop.Default != nil {
			p["default"] = prop.Default
		}
		if prop.Items != nil {
			p["items"] = prop.Items
		}
		result[name] = p
	}
	return result
}

// IsMCPSchema returns true if the schema name is an MCP tool.
func IsMCPSchema(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

// IsMCPSearchTool returns true if the name is the MCP search tool itself.
func IsMCPSearchTool(name string) bool {
	return name == ToolNameMCPSearchTools
}
