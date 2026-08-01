package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWebSearchTool_Name(t *testing.T) {
	tool := WebSearchTool{}
	if tool.Name() != "WebSearch" {
		t.Errorf("Expected name 'WebSearch', got '%s'", tool.Name())
	}
}

func TestWebSearchTool_Required(t *testing.T) {
	tool := WebSearchTool{}
	required := tool.Required()
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("Expected required ['query'], got %v", required)
	}
}

func TestWebSearchTool_Properties(t *testing.T) {
	tool := WebSearchTool{}
	props := tool.Properties()

	if _, ok := props["query"]; !ok {
		t.Error("Expected 'query' property")
	}
	if _, ok := props["num"]; !ok {
		t.Error("Expected 'num' property")
	}
}

func TestWebSearchTool_Execute_MissingQuery(t *testing.T) {
	tool := WebSearchTool{}
	args := `{}`
	_, err := tool.ExecuteContext(context.TODO(), args)
	if err == nil {
		t.Error("Expected error for missing query")
	}
}

func TestWebSearchTool_Execute_EmptyQuery(t *testing.T) {
	tool := WebSearchTool{}
	args := `{"query": ""}`
	_, err := tool.ExecuteContext(context.TODO(), args)
	if err == nil {
		t.Error("Expected error for empty query")
	}
}

func TestWebSearchTool_Execute_NoAPIKey(t *testing.T) {
	// This test will fail if no API key is configured
	tool := WebSearchTool{}
	args := `{"query": "test"}`
	_, err := tool.ExecuteContext(context.TODO(), args)
	if err == nil {
		t.Error("Expected error when no API key is configured")
	}
}

func TestWebSearchResult_Marshal(t *testing.T) {
	result := &WebSearchResult{
		Query:      "test query",
		NumResults: 2,
		Results: []SearchResult{
			{
				Title:   "Test Title 1",
				Link:    "https://example.com/1",
				Snippet: "Test snippet 1",
			},
			{
				Title:   "Test Title 2",
				Link:    "https://example.com/2",
				Snippet: "Test snippet 2",
			},
		},
		DurationMs: 100,
		Provider:   "brave",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Failed to marshal result: %v", err)
	}

	var unmarshaled WebSearchResult
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if unmarshaled.Query != result.Query {
		t.Errorf("Query mismatch: expected '%s', got '%s'", result.Query, unmarshaled.Query)
	}
	if unmarshaled.NumResults != result.NumResults {
		t.Errorf("NumResults mismatch: expected %d, got %d", result.NumResults, unmarshaled.NumResults)
	}
	if len(unmarshaled.Results) != len(result.Results) {
		t.Errorf("Results length mismatch: expected %d, got %d", len(result.Results), len(unmarshaled.Results))
	}
}
