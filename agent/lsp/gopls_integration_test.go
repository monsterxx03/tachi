//go:build integration

package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestWithGopls tests the LSP client against a real gopls server.
// Run with: go test -tags=integration -run TestWithGopls ./agent/lsp/
func TestWithGopls(t *testing.T) {
	// Check if gopls is available.
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed:", err)
	}

	// Create a temp Go project.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}

	src := `package test

// Hello returns a greeting.
func Hello(name string) string {
	return "Hello, " + name
}

// Add adds two numbers.
func Add(a, b int) int {
	return a + b
}
`
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	rootURI := PathToURI(dir)
	cfg := ServerConfig{
		Name:       "gopls",
		Command:    "gopls",
		Args:       []string{},
		Extensions: []string{".go"},
		Languages:  []string{"go"},
		WorkspaceFolder: dir,
	}

	server := NewLSPServer("gopls", cfg, rootURI)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Start gopls.
	t.Log("Starting gopls...")
	if err := server.Start(ctx); err != nil {
		t.Fatalf("start gopls: %v", err)
	}
	defer server.Stop(t.Context())

	if !server.IsHealthy() {
		t.Fatal("gopls not healthy after start")
	}
	t.Logf("gopls started, state=%d", server.State())

	// Open the file.
	uri := PathToURI(srcPath)
	content, _ := os.ReadFile(srcPath)
	if err := server.OpenFile(ctx, uri, "go", string(content)); err != nil {
		t.Fatalf("open file: %v", err)
	}

	// Wait for gopls to index.
	t.Log("Waiting for gopls to index...")
	time.Sleep(1 * time.Second)

	// Test 1: hover on "Hello" (line 3, char 6 — the function name).
	t.Log("Testing hover...")
	var hover Hover
	err := server.Call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 2, "character": 6},
	}, &hover)
	if err != nil {
		t.Fatalf("hover call failed: %v", err)
	}
	if hover.Contents.Value == "" {
		t.Fatal("hover returned empty content")
	}
	t.Logf("  hover result: %s", hover.Contents.Value[:min(100, len(hover.Contents.Value))])

	// Test 2: goToDefinition on "Add" (line 9, char 6).
	t.Log("Testing goToDefinition...")
	var locations []Location
	err = server.Call(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 8, "character": 6},
	}, &locations)
	if err != nil {
		t.Fatalf("definition call failed: %v", err)
	}
	if len(locations) == 0 {
		t.Fatal("definition returned no locations")
	}
	t.Logf("  defined at: %s:%d:%d",
		locations[0].URI,
		locations[0].Range.Start.Line+1,
		locations[0].Range.Start.Character+1)

	// Test 3: documentSymbol.
	t.Log("Testing documentSymbol...")
	var symResult jsonRaw
	// We'll just decode it raw since it can be either DocumentSymbol[] or SymbolInformation[]
	err = server.Call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}, &symResult)
	if err != nil {
		t.Fatalf("documentSymbol call failed: %v", err)
	}
	t.Logf("  documentSymbol result length: %d bytes", len(symResult))

	// Test 4: findReferences on "Add"
	t.Log("Testing findReferences...")
	var refs []Location
	err = server.Call(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 8, "character": 6},
		"context":      map[string]any{"includeDeclaration": true},
	}, &refs)
	if err != nil {
		t.Fatalf("references call failed: %v", err)
	}
	t.Logf("  found %d references", len(refs))
	for _, r := range refs {
		t.Logf("    %s:%d", URItoPath(r.URI), r.Range.Start.Line+1)
	}
}

// jsonRaw is a raw JSON message for decoding dynamic LSP responses.
type jsonRaw []byte

func (j *jsonRaw) UnmarshalJSON(b []byte) error {
	*j = b
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
