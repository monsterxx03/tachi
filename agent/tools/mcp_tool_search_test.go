package tools

import (
	"encoding/json"
	"testing"
)

// stubSearcher implements MCPSearcher for testing.
type stubSearcher struct {
	results []MCPSearchResultItem
}

func (s *stubSearcher) Search(query string, maxResults int) []MCPSearchResultItem {
	return s.results
}

// stubTracker implements MCPSearchTracker for testing.
type stubTracker struct {
	added []string
}

func (s *stubTracker) Add(name string) {
	s.added = append(s.added, name)
}

func (s *stubTracker) List() []string {
	return s.added
}

func TestMCPSearchToolsTool_Name(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	if tool.Name() != "MCPSearchTools" {
		t.Errorf("expected MCPSearchTools, got %s", tool.Name())
	}
}

func TestMCPSearchToolsTool_Description(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	if tool.Description() == "" {
		t.Error("expected non-empty description")
	}
}

func TestMCPSearchToolsTool_Properties(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	props := tool.Properties()
	if _, ok := props["query"]; !ok {
		t.Error("expected query property")
	}
	if _, ok := props["max_results"]; !ok {
		t.Error("expected max_results property")
	}
}

func TestMCPSearchToolsTool_Required(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	required := tool.Required()
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("expected [query], got %v", required)
	}
}

func TestMCPSearchToolsTool_Parallel(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	if !tool.Parallel() {
		t.Error("expected MCPSearchTools to be parallel")
	}
}

func TestMCPSearchToolsTool_Execute_Success(t *testing.T) {
	tool := NewMCPSearchToolsTool(
		&stubSearcher{
			results: []MCPSearchResultItem{
				{
					Name:        "mcp__pg__query",
					Description: "Query a PostgreSQL database",
				},
				{
					Name:        "mcp__pg__list_tables",
					Description: "List all tables",
				},
			},
		},
		&stubTracker{},
	)

	output, err := tool.ExecuteContext(t.Context(), `{"query": "select:mcp__pg__query,mcp__pg__list_tables", "max_results": 5}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse output JSON: %v", err)
	}

	matches, ok := result["matches"].([]any)
	if !ok {
		t.Fatal("expected matches array")
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	// Check tool_definitions are included
	toolDefs, ok := result["tool_definitions"].(map[string]any)
	if !ok {
		t.Fatal("expected tool_definitions map")
	}
	if _, ok := toolDefs["mcp__pg__query"]; !ok {
		t.Error("expected mcp__pg__query in tool_definitions")
	}
	if _, ok := toolDefs["mcp__pg__list_tables"]; !ok {
		t.Error("expected mcp__pg__list_tables in tool_definitions")
	}
}

func TestMCPSearchToolsTool_Execute_TrackerCalled(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewMCPSearchToolsTool(
		&stubSearcher{
			results: []MCPSearchResultItem{
				{Name: "mcp__pg__query", Description: "Query DB"},
			},
		},
		tracker,
	)

	_, err := tool.ExecuteContext(t.Context(), `{"query": "select:mcp__pg__query"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tracker.added) != 1 || tracker.added[0] != "mcp__pg__query" {
		t.Errorf("expected tracker.Add called with 'mcp__pg__query', got %v", tracker.added)
	}
}

func TestMCPSearchToolsTool_Execute_EmptyResults(t *testing.T) {
	tool := NewMCPSearchToolsTool(
		&stubSearcher{results: nil},
		&stubTracker{},
	)

	output, err := tool.ExecuteContext(t.Context(), `{"query": "nonexistent"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse output JSON: %v", err)
	}

	matches, _ := result["matches"].([]any)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}

	total, _ := result["total_deferred_tools"].(float64)
	if total != 0 {
		t.Errorf("expected total_deferred_tools=0, got %v", total)
	}
}

func TestMCPSearchToolsTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	_, err := tool.ExecuteContext(t.Context(), `not json`)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestMCPSearchToolsTool_Execute_MissingQuery(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	_, err := tool.ExecuteContext(t.Context(), `{}`)
	if err == nil {
		t.Error("expected error for missing query")
	}
}

func TestMCPSearchToolsTool_Execute_EmptyQuery(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	_, err := tool.ExecuteContext(t.Context(), `{"query": ""}`)
	if err == nil {
		t.Error("expected error for empty query")
	}
}

func TestMCPSearchToolsTool_Execute_DefaultMaxResults(t *testing.T) {
	// When max_results is not specified, should default to 5
	tool := NewMCPSearchToolsTool(
		&stubSearcher{results: nil},
		&stubTracker{},
	)

	output, err := tool.ExecuteContext(t.Context(), `{"query": "something"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse output JSON: %v", err)
	}
	// Just verify it doesn't crash — default max_results=5 is handled by searcher
	if _, ok := result["query"]; !ok {
		t.Error("expected query in output")
	}
}

func TestMCPSearchToolsTool_SchemaToPropsJSON(t *testing.T) {
	props := map[string]PropertySchema{
		"name": {
			Type:        "string",
			Description: "Name field",
			Items:       nil,
		},
		"count": {
			Type:        "integer",
			Description: "Count field",
			Items:       map[string]any{"type": "number"},
		},
	}

	result := schemaToPropsJSON(props)
	if _, ok := result["name"]; !ok {
		t.Error("expected name in result")
	}
	nameMap := result["name"].(map[string]any)
	if nameMap["type"] != "string" {
		t.Errorf("expected type string, got %v", nameMap["type"])
	}
	if nameMap["description"] != "Name field" {
		t.Errorf("expected 'Name field', got %v", nameMap["description"])
	}

	// Items should be included when present
	countMap := result["count"].(map[string]any)
	if countMap["items"] == nil {
		t.Error("expected items for count property")
	}
}

func TestIsMCPSchema(t *testing.T) {
	if !IsMCPSchema("mcp__pg__query") {
		t.Error("expected true for mcp__ prefix")
	}
	if IsMCPSchema("Bash") {
		t.Error("expected false for non-MCP tool")
	}
	if IsMCPSchema("") {
		t.Error("expected false for empty string")
	}
}

func TestIsMCPSearchTool(t *testing.T) {
	if !IsMCPSearchTool("MCPSearchTools") {
		t.Error("expected true for MCPSearchTools")
	}
	if IsMCPSearchTool("mcp__pg__query") {
		t.Error("expected false for MCP tool")
	}
	if IsMCPSearchTool("Bash") {
		t.Error("expected false for built-in tool")
	}
}
