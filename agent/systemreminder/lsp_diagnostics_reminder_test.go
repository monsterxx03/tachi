package systemreminder

import (
	"testing"

	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/tools"
)

// mockLSPDiagProvider is a test stub for LSPDiagnosticsProvider.
type mockLSPDiagProvider struct {
	configured bool
	servers    map[string]*lsp.LSPServer
}

func (m *mockLSPDiagProvider) IsConfigured() bool                 { return m.configured }
func (m *mockLSPDiagProvider) Servers() map[string]*lsp.LSPServer { return m.servers }

// We can't create real LSPServer instances without an LSP process, so
// LSPDiagnosticsReminder tests focus on the guard-rails and skip the
// full diagnostic-collection path. The core logic is exercised in
// integration tests with a real LSP server in manager_test.go.

func TestLSPDiagnosticsReminder_SkipsWhenNotConfigured(t *testing.T) {
	r := &LSPDiagnosticsReminder{
		Provider: &mockLSPDiagProvider{configured: false},
	}
	lines := r.Generate(t.Context(), Context{IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil when not configured, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_SkipsWhenNotToolResult(t *testing.T) {
	r := &LSPDiagnosticsReminder{
		Provider: &mockLSPDiagProvider{configured: true},
	}
	lines := r.Generate(t.Context(), Context{IsToolResult: false, ToolNames: []string{tools.ToolNameEdit}})
	if lines != nil {
		t.Errorf("expected nil when not tool result, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_SkipsWhenNoEditFile(t *testing.T) {
	// Should skip even at a tool-result boundary when the tool list
	// does not include EditFile.
	r := &LSPDiagnosticsReminder{
		Provider: &mockLSPDiagProvider{configured: true},
	}
	lines := r.Generate(t.Context(), Context{IsToolResult: true, ToolNames: []string{"ReadFile", "Grep"}})
	if lines != nil {
		t.Errorf("expected nil when no EditFile in tool names, got %v", lines)
	}
	// Also skip when ToolNames is nil.
	lines = r.Generate(t.Context(), Context{IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil when ToolNames is nil, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_SkipsWhenNilProvider(t *testing.T) {
	r := &LSPDiagnosticsReminder{Provider: nil}
	lines := r.Generate(t.Context(), Context{IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil when provider is nil, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_SkipsWhenNoServers(t *testing.T) {
	r := &LSPDiagnosticsReminder{
		Provider: &mockLSPDiagProvider{configured: true, servers: map[string]*lsp.LSPServer{}},
	}
	lines := r.Generate(t.Context(), Context{IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil when no servers, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_CollectorIntegration(t *testing.T) {
	c := NewCollector(
		DateReminder{},
		&LSPDiagnosticsReminder{Provider: &mockLSPDiagProvider{configured: false}},
	)
	result := c.Collect(t.Context(), Context{
		IsFirstMessage: false,
		IsToolResult:   true,
	})
	// LSPDiagnosticsReminder shouldn't fire (not configured), so only
	// DateReminder should be in the output — but DateReminder also skips
	// on IsToolResult, so we expect empty.
	if result != "" {
		t.Errorf("expected empty when no reminders fire, got: %s", result)
	}
}

func TestSeverityAbbrev(t *testing.T) {
	tests := []struct {
		sev  lsp.DiagnosticSeverity
		want string
	}{
		{lsp.SeverityError, "Error"},
		{lsp.SeverityWarning, "Warn"},
		{lsp.SeverityInformation, "Info"},
		{lsp.SeverityHint, "Hint"},
		{99, "?"},
	}
	for _, tt := range tests {
		got := severityAbbrev(tt.sev)
		if got != tt.want {
			t.Errorf("severityAbbrev(%d) = %q, want %q", tt.sev, got, tt.want)
		}
	}
}

func TestShortURI(t *testing.T) {
	got := shortURI("file:///Users/will/test.go")
	if got != "/Users/will/test.go" {
		t.Errorf("expected stripped prefix, got %q", got)
	}
	got = shortURI("no-prefix")
	if got != "no-prefix" {
		t.Errorf("expected unchanged, got %q", got)
	}
}

func TestLSPDiagnosticsReminder_HashDedup(t *testing.T) {
	// Verify that the same diagnostics hash doesn't re-fire.
	// Since we can't create real LSPServer instances, we verify the struct
	// initial state: lastHashes should start nil.
	r := &LSPDiagnosticsReminder{}
	if r.lastHashes != nil {
		t.Error("expected nil lastHashes initially")
	}
}
