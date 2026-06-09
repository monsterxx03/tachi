package systemreminder

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/lsp"
)

// mockLSPDiagProvider is a test stub for LSPDiagnosticsProvider.
type mockLSPDiagProvider struct {
	configured bool
	servers    map[string]*lsp.LSPServer
}

func (m *mockLSPDiagProvider) IsConfigured() bool                    { return m.configured }
func (m *mockLSPDiagProvider) Servers() map[string]*lsp.LSPServer    { return m.servers }

// We can't create real LSPServer instances without an LSP process, so
// LSPDiagnosticsReminder tests focus on the guard-rails and skip the
// full diagnostic-collection path. The core logic is exercised in
// integration tests with a real LSP server in manager_test.go.

func TestLSPDiagnosticsReminder_SkipsWhenNotConfigured(t *testing.T) {
	r := &LSPDiagnosticsReminder{
		Provider: &mockLSPDiagProvider{configured: false},
	}
	lines := r.Generate(Context{IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil when not configured, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_SkipsWhenNotToolResult(t *testing.T) {
	r := &LSPDiagnosticsReminder{
		Provider: &mockLSPDiagProvider{configured: true},
	}
	lines := r.Generate(Context{IsToolResult: false})
	if lines != nil {
		t.Errorf("expected nil when not tool result, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_SkipsWhenNilProvider(t *testing.T) {
	r := &LSPDiagnosticsReminder{Provider: nil}
	lines := r.Generate(Context{IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil when provider is nil, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_SkipsWhenNoServers(t *testing.T) {
	r := &LSPDiagnosticsReminder{
		Provider: &mockLSPDiagProvider{configured: true, servers: map[string]*lsp.LSPServer{}},
	}
	lines := r.Generate(Context{IsToolResult: true})
	if lines != nil {
		t.Errorf("expected nil when no servers, got %v", lines)
	}
}

func TestLSPDiagnosticsReminder_TaggedReminder(t *testing.T) {
	r := &LSPDiagnosticsReminder{}
	if r.WrapperTag() != "lsp-diagnostics" {
		t.Errorf("expected 'lsp-diagnostics' tag, got %q", r.WrapperTag())
	}
}

func TestLSPDiagnosticsReminder_CollectorIntegration(t *testing.T) {
	// Ensures it plays nicely with the Collector's TaggedReminder logic.
	c := NewCollector(
		DateReminder{},
		&LSPDiagnosticsReminder{Provider: &mockLSPDiagProvider{configured: false}},
	)
	result := c.Collect(Context{
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
	r := &LSPDiagnosticsReminder{Provider: &mockLSPDiagProvider{configured: true, servers: map[string]*lsp.LSPServer{}}}
	if r.lastHashes != nil {
		t.Error("expected nil lastHashes initially")
	}
}

// Verify the WrapperTag interface assertion compiles.
var _ TaggedReminder = (*LSPDiagnosticsReminder)(nil)

func TestLSPDiagnosticsReminder_CollectorTaggedOutput(t *testing.T) {
	// When a TaggedReminder fires with a collector, it should produce its own
	// <lsp-diagnostics> block, not be merged into <system-reminder>.
	// Because we can't create real LSPServer instances with diagnostics,
	// we test the collector's tag-handling via mockTaggedReminder with the
	// same tag to ensure the collector handles this tag correctly.
	c := NewCollector(
		&mockTaggedReminder{tag: "lsp-diagnostics", content: []string{"gopls: 1 diagnostics (1 errors)"}},
	)
	result := c.Collect(Context{IsToolResult: true})
	if !strings.Contains(result, "<lsp-diagnostics>") {
		t.Errorf("expected <lsp-diagnostics> tag, got: %s", result)
	}
	if strings.Contains(result, "<system-reminder>") {
		t.Errorf("expected no <system-reminder> for tagged-only, got: %s", result)
	}
}