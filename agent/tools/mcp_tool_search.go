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
	Query string
	// Server restricts the candidate set to one MCP server's tools, spelled the
	// way the deferred-tools reminder lists it ("github"); NormalizeMCPServer
	// has already been applied by the tool. Empty means every server.
	Server string
	// MaxResults caps the answer (the tool clamps it before it gets here).
	MaxResults int
}

// MCPSearchResultItem is a raw search result from the pool.
// Defined here to avoid circular dependency; the pool converts to this.
type MCPSearchResultItem struct {
	Name        string
	Description string
	Schema      Schema
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
			Description: "Optional. Search only this server's tools, named as the deferred-tools " +
				"block lists it (e.g. \"github\"; an \"mcp__\" prefix is accepted). A tool you NAME " +
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

type mcpSearchOutput struct {
	Matches  []MCPSearchResult `json:"matches"`
	Query    string            `json:"query"`
	Total    int               `json:"total_deferred_tools"`
	ToolDefs map[string]any    `json:"tool_definitions,omitempty"`
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

	if params.Query == "" {
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
	return strings.HasPrefix(name, MCPServerPrefix)
}

// IsMCPSearchTool returns true if the name is the MCP search tool itself.
func IsMCPSearchTool(name string) bool {
	return name == ToolNameMCPSearchTools
}
