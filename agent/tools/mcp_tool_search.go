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

// MCPSearcher is the interface for searching deferred MCP tools.
// Implemented by mcp.DeferredPool.
type MCPSearcher interface {
	Search(query string, maxResults int) []MCPSearchResultItem
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

// MCPSearchToolsTool allows the LLM to search for and load MCP tools.
// Registered as a built-in tool when MCP servers are configured.
type MCPSearchToolsTool struct {
	searcher MCPSearcher
	tracker  MCPSearchTracker
}

// NewMCPSearchToolsTool creates a new MCPSearchTools tool.
func NewMCPSearchToolsTool(searcher MCPSearcher, tracker MCPSearchTracker) *MCPSearchToolsTool {
	return &MCPSearchToolsTool{searcher: searcher, tracker: tracker}
}

func (t *MCPSearchToolsTool) Name() string { return ToolNameMCPSearchTools }

func (t *MCPSearchToolsTool) Description() string {
	return "Search for and load MCP tools by name or capability. " +
		"MCP tools provide access to external services and are not loaded by default — " +
		"use this tool to find and load them. " +
		"Query forms: \"select:ToolName1,ToolName2\" for exact names " +
		"(suffix-only names like \"query\" match \"mcp__server__query\" automatically), " +
		"\"mcp__serverName\" for all tools of a server, " +
		"or keywords like \"database query\" for relevance search. " +
		"Prefix a term with + to require it (e.g. \"+postgres query\"). " +
		"Once loaded, a tool can be called like any built-in tool."
}
func (t *MCPSearchToolsTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"query": {
			Type: "string",
			Description: "Search query. Forms: " +
				"\"select:ToolName1,ToolName2\" — load exact tools by name; " +
				"\"mcp__serverName\" — load all tools for a server; " +
				"keywords — search by capability. " +
				"Prefix a term with + to require it (e.g. \"+postgres query\").",
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
	results := t.searcher.Search(params.Query, params.MaxResults)

	// Mark matched tools as discovered
	toolDefs := make(map[string]any)
	for _, r := range results {
		t.tracker.Add(r.Name)

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
func schemaToPropsJSON(props map[string]PropertySchema) map[string]any {
	result := make(map[string]any, len(props))
	for name, prop := range props {
		p := map[string]any{
			"type":        prop.Type,
			"description": prop.Description,
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
