package systemreminder

import (
	"testing"
)

// stubDeferredProvider implements DeferredToolProvider for testing.
type stubDeferredProvider struct {
	tools []DeferredToolRecord
}

func (s *stubDeferredProvider) All() []DeferredToolRecord {
	return s.tools
}

// stubDeferredTracker implements DeferredToolTracker for testing.
type stubDeferredTracker struct {
	discovered map[string]bool
}

func (s *stubDeferredTracker) Contains(name string) bool {
	return s.discovered[name]
}

func TestDeferredToolReminder_NoProvider(t *testing.T) {
	r := DeferredToolReminder{Provider: nil}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil when no provider, got %v", lines)
	}
}

func TestDeferredToolReminder_EmptyPool(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{},
	}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil when pool is empty, got %v", lines)
	}
}

func TestDeferredToolReminder_NotFirstMessage(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: "Query database"},
			},
		},
	}
	lines := r.Generate(Context{IsFirstMessage: false})
	if lines != nil {
		t.Errorf("expected nil when not first message, got %v", lines)
	}
}

func TestDeferredToolReminder_ToolResultBoundary(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: "Query database"},
			},
		},
	}
	lines := r.Generate(Context{IsFirstMessage: true, IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil at tool-result boundary, got %v", lines)
	}
}

func TestDeferredToolReminder_AllDiscovered(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: "Query database"},
			},
		},
		Tracker: &stubDeferredTracker{
			discovered: map[string]bool{"mcp__pg__query": true},
		},
	}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil when all tools discovered, got %v", lines)
	}
}

func TestDeferredToolReminder_UndiscoveredTools(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: "Query the PostgreSQL database"},
				{Name: "mcp__gh__pr", Description: "Create and manage pull requests"},
			},
		},
		Tracker: &stubDeferredTracker{
			discovered: map[string]bool{"mcp__gh__pr": true},
		},
	}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines == nil || len(lines) == 0 {
		t.Fatal("expected non-nil result with undiscovered tools")
	}

	// Should mention the undiscovered tool
	full := lines[0]
	// Check format: tool name — description
	if !contains(full, "mcp__pg__query") {
		t.Errorf("expected undiscovered tool name in output, got: %s", full)
	}
	// Should NOT mention the already-discovered tool
	if contains(full, "mcp__gh__pr") {
		t.Errorf("did not expect discovered tool name in output, got: %s", full)
	}
	// Should include a hint about total tools
	if !contains(full, "2 个 MCP 工具") {
		t.Errorf("expected total tool count hint, got: %s", full)
	}
	if !contains(full, "1 个已加载") {
		t.Errorf("expected loaded count hint, got: %s", full)
	}
}

func TestDeferredToolReminder_AllUndiscovered(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: "Query database"},
				{Name: "mcp__gh__pr", Description: "Create PRs"},
			},
		},
		Tracker: nil, // no tracker = all undiscovered
	}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines == nil || len(lines) == 0 {
		t.Fatal("expected non-nil result with undiscovered tools")
	}
	full := lines[0]
	if !contains(full, "mcp__pg__query") || !contains(full, "mcp__gh__pr") {
		t.Errorf("expected both tool names in output, got: %s", full)
	}
	// When all undiscovered, should show "共 N 个 MCP 工具可用"
	if !contains(full, "2 个 MCP 工具可用") {
		t.Errorf("expected 'N 个工具可用' hint, got: %s", full)
	}
}

func TestDeferredToolReminder_DescriptionTruncation(t *testing.T) {
	longDesc := "This is a very long description that goes on and on and should be truncated to one hundred characters at most by the reminder generator"
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: longDesc},
			},
		},
	}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines == nil || len(lines) == 0 {
		t.Fatal("expected non-nil result")
	}
	// Description should be truncated to 100 runes + "..."
	if !contains(lines[0], "...") {
		t.Errorf("expected truncated description ending with ..., got: %s", lines[0])
	}
}

func TestDeferredToolReminder_MultilineDescription(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{
					Name:        "mcp__pg__query",
					Description: "First line\nSecond line\nThird line",
				},
			},
		},
	}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines == nil || len(lines) == 0 {
		t.Fatal("expected non-nil result")
	}
	if contains(lines[0], "Second line") {
		t.Errorf("expected only first line of description, got: %s", lines[0])
	}
}

func TestDeferredToolReminder_FirstLineOnly(t *testing.T) {
	// Generates multiple lines; first line has the format "  name — desc"
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__x__y", Description: "foo"},
			},
		},
	}
	lines := r.Generate(Context{IsFirstMessage: true})
	if lines == nil || len(lines) == 0 {
		t.Fatal("expected non-nil result")
	}
	if !contains(lines[0], "mcp__x__y — foo") {
		t.Errorf("expected 'name — desc' format, got: %s", lines[0])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
