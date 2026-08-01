package lsp

import (
	"encoding/json"
	"testing"
)

// =============================================================================
// Protocol type serialization tests
// =============================================================================

// TestProtocolRoundTrip tests JSON round-trip for key protocol types.
func TestProtocolRoundTrip(t *testing.T) {
	t.Run("document_symbol", func(t *testing.T) {
		sym := DocumentSymbol{
			Name:           "MyClass",
			Kind:           SKClass,
			Detail:         "class MyClass",
			Range:          Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 20, Character: 1}},
			SelectionRange: Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 7}},
			Children: []DocumentSymbol{
				{Name: "method1", Kind: SKMethod, Range: Range{Start: Position{Line: 3, Character: 2}, End: Position{Line: 5, Character: 3}}, SelectionRange: Range{Start: Position{Line: 3, Character: 2}, End: Position{Line: 3, Character: 9}}},
			},
		}

		data, err := json.Marshal(sym)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}

		var decoded DocumentSymbol
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if decoded.Name != "MyClass" {
			t.Fatalf("expected MyClass, got %s", decoded.Name)
		}
		if len(decoded.Children) != 1 || decoded.Children[0].Name != "method1" {
			t.Fatalf("expected 1 child 'method1', got %d children", len(decoded.Children))
		}
	})

	t.Run("call_hierarchy_item", func(t *testing.T) {
		item := CallHierarchyItem{
			Name:           "handleRequest",
			Kind:           SKFunction,
			Detail:         "func(string) error",
			URI:            "file:///test/main.go",
			Range:          Range{Start: Position{Line: 10, Character: 0}, End: Position{Line: 20, Character: 1}},
			SelectionRange: Range{Start: Position{Line: 10, Character: 5}, End: Position{Line: 10, Character: 18}},
		}
		data, _ := json.Marshal(item)
		var decoded CallHierarchyItem
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if decoded.Name != "handleRequest" || decoded.Detail != "func(string) error" {
			t.Fatalf("unexpected decoded: %+v", decoded)
		}
	})

	t.Run("diagnostic", func(t *testing.T) {
		diag := Diagnostic{
			Range:    Range{Start: Position{Line: 5, Character: 0}, End: Position{Line: 5, Character: 10}},
			Severity: SeverityError,
			Message:  "undeclared variable: foo",
		}
		data, _ := json.Marshal(diag)
		var decoded Diagnostic
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
		if decoded.Message != "undeclared variable: foo" {
			t.Fatalf("expected message, got %s", decoded.Message)
		}
		if decoded.Severity != SeverityError {
			t.Fatalf("expected SeverityError, got %d", decoded.Severity)
		}
	})
}
