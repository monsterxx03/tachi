package lsp

import (
	"strings"
	"testing"
)

// =============================================================================
// Formatter tests — call hierarchy
// =============================================================================

// TestFormatCallHierarchy tests prepareCallHierarchy formatting.
func TestFormatCallHierarchy(t *testing.T) {
	wd := "/test/project"

	t.Run("single_item", func(t *testing.T) {
		items := []CallHierarchyItem{{
			Name: "handleRequest",
			Kind: SKFunction,
			URI:  "file:///test/project/src/main.go",
			Range: Range{
				Start: Position{Line: 9, Character: 0},
				End:   Position{Line: 15, Character: 1},
			},
		}}
		result := formatPrepareCallHierarchy(items, wd)
		if !strings.Contains(result, "handleRequest") {
			t.Fatalf("expected handleRequest in result, got: %s", result)
		}
		if !strings.Contains(result, "src/main.go:10") {
			t.Fatalf("expected src/main.go:10 in result, got: %s", result)
		}
	})

	t.Run("multiple_items", func(t *testing.T) {
		items := []CallHierarchyItem{
			{Name: "foo", Kind: SKFunction, URI: "file:///test/project/a.go", Range: Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 0}}},
			{Name: "bar", Kind: SKFunction, URI: "file:///test/project/b.go", Range: Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 0}}},
		}
		result := formatPrepareCallHierarchy(items, wd)
		if !strings.Contains(result, "2 call hierarchy items") {
			t.Fatalf("expected '2 call hierarchy items', got: %s", result)
		}
	})

	t.Run("empty", func(t *testing.T) {
		result := formatPrepareCallHierarchy(nil, wd)
		if result != "No call hierarchy item found at this position." {
			t.Fatalf("expected no items message, got: %s", result)
		}
	})
}

// TestFormatIncomingCalls tests incoming call formatting.
func TestFormatIncomingCalls(t *testing.T) {
	wd := "/test/project"

	calls := []CallHierarchyIncomingCall{{
		From: CallHierarchyItem{
			Name:  "caller",
			Kind:  SKFunction,
			URI:   "file:///test/project/src/caller.go",
			Range: Range{Start: Position{Line: 4, Character: 0}, End: Position{Line: 10, Character: 1}},
		},
		FromRanges: []Range{
			{Start: Position{Line: 6, Character: 2}, End: Position{Line: 6, Character: 8}},
		},
	}}
	result := formatIncomingCalls(calls, wd)
	if !strings.Contains(result, "1 incoming call") {
		t.Fatalf("expected '1 incoming call', got: %s", result)
	}
	if !strings.Contains(result, "caller") {
		t.Fatalf("expected 'caller' in result, got: %s", result)
	}
	if !strings.Contains(result, "[calls at: 7:3]") {
		t.Fatalf("expected '[calls at: 7:3]', got: %s", result)
	}

	t.Run("empty", func(t *testing.T) {
		result := formatIncomingCalls(nil, wd)
		if !strings.Contains(result, "No incoming calls") {
			t.Fatalf("expected no incoming calls, got: %s", result)
		}
	})
}

// TestFormatOutgoingCalls tests outgoing call formatting.
func TestFormatOutgoingCalls(t *testing.T) {
	wd := "/test/project"

	calls := []CallHierarchyOutgoingCall{{
		To: CallHierarchyItem{
			Name:  "callee",
			Kind:  SKFunction,
			URI:   "file:///test/project/src/callee.go",
			Range: Range{Start: Position{Line: 20, Character: 0}, End: Position{Line: 25, Character: 1}},
		},
		FromRanges: []Range{
			{Start: Position{Line: 3, Character: 4}, End: Position{Line: 3, Character: 10}},
			{Start: Position{Line: 7, Character: 4}, End: Position{Line: 7, Character: 10}},
		},
	}}
	result := formatOutgoingCalls(calls, wd)
	if !strings.Contains(result, "1 outgoing call") {
		t.Fatalf("expected '1 outgoing call', got: %s", result)
	}
	if !strings.Contains(result, "callee") {
		t.Fatalf("expected 'callee' in result, got: %s", result)
	}
	if !strings.Contains(result, "[called from: 4:5, 8:5]") {
		t.Fatalf("expected '[called from: 4:5, 8:5]', got: %s", result)
	}

	t.Run("empty", func(t *testing.T) {
		result := formatOutgoingCalls(nil, wd)
		if !strings.Contains(result, "No outgoing calls") {
			t.Fatalf("expected no outgoing calls, got: %s", result)
		}
	})
}

// TestFormatCallHierarchyWithDetail tests that Detail field is appended.
func TestFormatCallHierarchyWithDetail(t *testing.T) {
	wd := "/test/project"
	items := []CallHierarchyItem{{
		Name:   "doWork",
		Kind:   SKMethod,
		Detail: "func(string) error",
		URI:    "file:///test/project/src/work.go",
		Range:  Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 0}},
	}}
	result := formatPrepareCallHierarchy(items, wd)
	if !strings.Contains(result, "[func(string) error]") {
		t.Fatalf("expected detail in result, got: %s", result)
	}
}

// =============================================================================
// MarkupContent extraction tests
// =============================================================================

// TestExtractMarkupText tests the hover content extraction from various formats.
func TestExtractMarkupText(t *testing.T) {
	t.Run("markup_content", func(t *testing.T) {
		mc := MarkupContent{Kind: "markdown", Value: "**bold** text"}
		result := extractMarkupText(mc)
		if result != "**bold** text" {
			t.Fatalf("expected markdown text, got: %s", result)
		}
	})

	t.Run("plain_string", func(t *testing.T) {
		result := extractMarkupText("plain string")
		if result != "plain string" {
			t.Fatalf("expected plain string, got: %s", result)
		}
	})

	t.Run("map_value", func(t *testing.T) {
		result := extractMarkupText(map[string]any{"value": "from map"})
		if result != "from map" {
			t.Fatalf("expected 'from map', got: %s", result)
		}
	})

	t.Run("slice_of_maps", func(t *testing.T) {
		result := extractMarkupText([]any{
			map[string]any{"value": "part1"},
			map[string]any{"value": "part2"},
		})
		if !strings.Contains(result, "part1") || !strings.Contains(result, "part2") {
			t.Fatalf("expected both parts, got: %s", result)
		}
	})

	t.Run("slice_of_strings", func(t *testing.T) {
		result := extractMarkupText([]any{"line1", "line2"})
		if !strings.Contains(result, "line1") || !strings.Contains(result, "line2") {
			t.Fatalf("expected both lines, got: %s", result)
		}
	})
}
