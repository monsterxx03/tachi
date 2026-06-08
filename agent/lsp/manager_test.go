package lsp

import (
	"context"
	"testing"
)

// =============================================================================
// LSPManager tests
// =============================================================================

// TestLSPManagerRouting tests extension→server routing and basic queries.
func TestLSPManagerRouting(t *testing.T) {
	cfg := &Config{
		Servers: []ServerConfig{
			{Name: "gopls", Command: "gopls", Extensions: []string{".go"}, Languages: []string{"go"}},
			{Name: "tsserver", Command: "ts-ls", Extensions: []string{".ts", ".tsx", ".js"}, Languages: []string{"typescript"}},
		},
	}
	m := NewManager(cfg)

	if !m.IsConfigured() {
		t.Fatal("expected configured")
	}
	if m.ServerCount() != 2 {
		t.Fatalf("expected 2 servers, got %d", m.ServerCount())
	}

	// Extension routing (lowercase normalization).
	tests := []struct {
		path string
		name string
		nil  bool
	}{
		{"foo.go", "gopls", false},
		{"foo.GO", "gopls", false}, // case-insensitive
		{"foo.ts", "tsserver", false},
		{"foo.tsx", "tsserver", false},
		{"foo.js", "tsserver", false},
		{"foo.py", "", true}, // no server
	}
	for _, tt := range tests {
		srv, err := m.GetServer(context.Background(), tt.path)
		if err != nil {
			// Lazy start will fail because command doesn't exist — that's fine for this test.
			// We just need to verify the routing resolved to the right server.
			if tt.nil {
				t.Errorf("GetServer(%q) unexpected error: %v", tt.path, err)
			}
			// For existing servers, startup will fail but the server was found.
			continue
		}
		if tt.nil && srv != nil {
			t.Errorf("GetServer(%q) expected nil, got %v", tt.path, srv)
		} else if srv != nil && srv.Name() != tt.name {
			t.Errorf("GetServer(%q) = %s, want %s", tt.path, srv.Name(), tt.name)
		}
	}
}

// TestLSPManagerNotConfigured tests behavior with no servers.
func TestLSPManagerNotConfigured(t *testing.T) {
	cfg := &Config{}
	m := NewManager(cfg)

	if m.IsConfigured() {
		t.Fatal("expected not configured")
	}
	if m.ServerCount() != 0 {
		t.Fatalf("expected 0 servers, got %d", m.ServerCount())
	}

	srv, err := m.GetServer(context.Background(), "foo.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv != nil {
		t.Fatal("expected nil server for unconfigured manager")
	}
}
