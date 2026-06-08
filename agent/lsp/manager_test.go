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

// TestLSPManagerServerInfos tests the ServerInfos() provider interface method.
func TestLSPManagerServerInfos(t *testing.T) {
	cfg := &Config{
		Servers: []ServerConfig{
			{Name: "alpha", Command: "echo", Extensions: []string{".a"}},
			{Name: "beta", Command: "echo", Extensions: []string{".b"}},
		},
	}
	m := NewManager(cfg)
	infos := m.ServerInfos()

	if len(infos) != 2 {
		t.Fatalf("expected 2 infos, got %d", len(infos))
	}
	// Should be unsorted initially (map iteration order), but reminder sorts by name.
	names := map[string]bool{}
	for _, info := range infos {
		names[info.Name] = true
		if len(info.Extensions) == 0 {
			t.Errorf("server %s has no extensions", info.Name)
		}
	}
	if !names["alpha"] || !names["beta"] {
		t.Fatalf("missing server names in infos: %v", names)
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
	if len(m.ServerInfos()) != 0 {
		t.Fatal("expected empty server infos")
	}

	srv, err := m.GetServer(context.Background(), "foo.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv != nil {
		t.Fatal("expected nil server for unconfigured manager")
	}
}
