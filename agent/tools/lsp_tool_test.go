package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/lsp"
)

// =============================================================================
// LSPTool ExecuteContext tests
// =============================================================================

// TestLSPToolInvalidArgs tests ExecuteContext with bad input.
func TestLSPToolInvalidArgs(t *testing.T) {
	tool := NewLSPTool(nil)

	// JSON parse error.
	result, err := tool.ExecuteContext(context.Background(), `{bad json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "invalid arguments") {
		t.Fatalf("expected invalid arguments error, got: %s", result)
	}
}

// TestLSPToolUnknownOperation tests that an unknown operation is rejected.
func TestLSPToolUnknownOperation(t *testing.T) {
	tool := NewLSPTool(nil)
	desc := tool.Description()
	expected := []string{
		"goToDefinition", "findReferences", "hover",
		"documentSymbol", "workspaceSymbol", "goToImplementation",
		"prepareCallHierarchy", "incomingCalls", "outgoingCalls",
	}
	for _, op := range expected {
		if !strings.Contains(desc, op) {
			t.Errorf("description missing operation: %s", op)
		}
	}
	if strings.Contains(desc, "flyToMoon") {
		t.Fatal("flyToMoon should not be in description")
	}
}

// TestLSPToolNoServer tests ExecuteContext when no server matches the extension.
func TestLSPToolNoServer(t *testing.T) {
	cfg := &lsp.Config{
		Servers: []lsp.ServerConfig{
			{Name: "gopls", Command: "gopls", Extensions: []string{".go"}},
		},
	}
	m := lsp.NewManager(cfg)
	tool := NewLSPTool(m)

	input := `{"operation": "goToDefinition", "filePath": "/tmp/test.py", "line": 1, "character": 1}`
	result, err := tool.ExecuteContext(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "No LSP server") {
		t.Fatalf("expected 'No LSP server', got: %s", result)
	}
}

// TestFormatRawLocations tests the Location / LocationLink JSON parser.
func TestFormatRawLocations(t *testing.T) {
	wd := "/test/project"

	t.Run("location_array", func(t *testing.T) {
		raw := json.RawMessage(`[{"uri":"file:///test/project/src/a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":5}}}]`)
		result, err := formatRawLocations("goToDefinition", raw, wd, "/test/project/src/a.go")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "a.go:1:1") {
			t.Fatalf("expected a.go:1:1, got: %s", result)
		}
	})

	t.Run("single_location", func(t *testing.T) {
		raw := json.RawMessage(`{"uri":"file:///test/project/src/main.go","range":{"start":{"line":5,"character":3},"end":{"line":5,"character":10}}}`)
		result, err := formatRawLocations("goToDefinition", raw, wd, "/test/project/src/main.go")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "main.go:6:4") {
			t.Fatalf("expected main.go:6:4, got: %s", result)
		}
	})

	t.Run("location_link_array", func(t *testing.T) {
		raw := json.RawMessage(`[{"targetUri":"file:///test/project/src/impl.go","targetRange":{"start":{"line":10,"character":0},"end":{"line":15,"character":1}},"targetSelectionRange":{"start":{"line":10,"character":4},"end":{"line":10,"character":14}}}]`)
		result, err := formatRawLocations("goToDefinition", raw, wd, "/test/project/src/iface.go")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "impl.go") {
			t.Fatalf("expected impl.go, got: %s", result)
		}
	})

	t.Run("null", func(t *testing.T) {
		result, err := formatRawLocations("goToDefinition", json.RawMessage("null"), wd, "/test/project/src/main.go")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "No definition found") {
			t.Fatalf("expected 'No definition found', got: %s", result)
		}
	})

	t.Run("nil", func(t *testing.T) {
		result, err := formatRawLocations("goToDefinition", nil, wd, "/test/project/src/main.go")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "No definition found") {
			t.Fatalf("expected 'No definition found', got: %s", result)
		}
	})
}

// TestLSPMarshalError tests the lspMarshalError helper.
func TestLSPMarshalError(t *testing.T) {
	result := lspMarshalError("hover", "something went wrong")
	if !strings.Contains(result, "something went wrong") {
		t.Fatalf("expected error message, got: %s", result)
	}
	if !strings.Contains(result, "hover") {
		t.Fatalf("expected operation name, got: %s", result)
	}
}
