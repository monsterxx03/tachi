package systemreminder

import (
	"strings"
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil when no provider, got %v", lines)
	}
}

func TestDeferredToolReminder_EmptyPool(t *testing.T) {
	r := DeferredToolReminder{
		Provider: &stubDeferredProvider{},
	}
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if lines != nil {
		t.Errorf("expected nil when pool is empty, got %v", lines)
	}
}

func TestDeferredToolReminder_NotFirstMessage(t *testing.T) {
	r := &DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: "Query database"},
			},
		},
	}
	// With async MCP init, the reminder should fire on ANY non-tool-result
	// message where undiscovered tools exist, not just the first message.
	lines := r.Generate(t.Context(), Context{IsFirstMessage: false})
	if lines == nil {
		t.Error("expected non-nil with undiscovered tools even if not first message")
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true, IsToolResult: true})
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) == 0 {
		t.Fatal("expected non-nil result with undiscovered tools")
	}

	// Should mention the undiscovered tool
	full := lines[0]
	// Check format: tool name — description
	if !strings.Contains(full, "mcp__pg__query") {
		t.Errorf("expected undiscovered tool name in output, got: %s", full)
	}
	// Should NOT mention the already-discovered tool
	if strings.Contains(full, "mcp__gh__pr") {
		t.Errorf("did not expect discovered tool name in output, got: %s", full)
	}
	// Should include a hint about total tools
	if !strings.Contains(full, "2 个 MCP 工具") {
		t.Errorf("expected total tool count hint, got: %s", full)
	}
	if !strings.Contains(full, "1 个已加载") {
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) == 0 {
		t.Fatal("expected non-nil result with undiscovered tools")
	}
	full := lines[0]
	if !strings.Contains(full, "mcp__pg__query") || !strings.Contains(full, "mcp__gh__pr") {
		t.Errorf("expected both tool names in output, got: %s", full)
	}
	// When all undiscovered, should show "共 N 个 MCP 工具可用"
	if !strings.Contains(full, "2 个 MCP 工具可用") {
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) == 0 {
		t.Fatal("expected non-nil result")
	}
	// Description should be truncated to 100 runes + "..."
	if !strings.Contains(lines[0], "...") {
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) == 0 {
		t.Fatal("expected non-nil result")
	}
	if strings.Contains(lines[0], "Second line") {
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
	lines := r.Generate(t.Context(), Context{IsFirstMessage: true})
	if len(lines) == 0 {
		t.Fatal("expected non-nil result")
	}
	if !strings.Contains(lines[0], "mcp__x__y — foo") {
		t.Errorf("expected 'name — desc' format, got: %s", lines[0])
	}
}

func TestDeferredToolReminder_FiresOnlyOnce(t *testing.T) {
	r := &DeferredToolReminder{
		Provider: &stubDeferredProvider{
			tools: []DeferredToolRecord{
				{Name: "mcp__pg__query", Description: "Query database"},
			},
		},
	}
	// First call should fire
	lines1 := r.Generate(t.Context(), Context{})
	if lines1 == nil {
		t.Fatal("expected output on first call")
	}
	// Second call should NOT fire (HasFired = true)
	lines2 := r.Generate(t.Context(), Context{})
	if lines2 != nil {
		t.Error("expected nil on second call (HasFired guard)")
	}
}
