package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockLSPServer is a minimal LSP server that speaks the protocol over stdio.
type mockLSPServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

// startMockLSP starts a mock LSP server compiled from testdata/mocklsp/main.go.
func startMockLSP(t *testing.T) *mockLSPServer {
	t.Helper()

	srcPath := filepath.Join("testdata", "mocklsp", "main.go")
	if _, err := os.Stat(srcPath); err != nil {
		t.Skipf("mock server source not found: %v", err)
	}

	dir := t.TempDir()
	binPath := filepath.Join(dir, "mock-lsp")
	buildCmd := exec.Command("go", "build", "-o", binPath, srcPath)
	buildCmd.Dir = "."
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build mock LSP server: %v\n%s", err, string(buildOut))
	}

	cmd := exec.Command(binPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start mock LSP: %v", err)
	}

	t.Cleanup(func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})

	return &mockLSPServer{cmd: cmd, stdin: stdin, stdout: stdout}
}

// ---- Tests ----

// TestJSONRPCConn tests the low-level JSON-RPC framing over stdio.
func TestJSONRPCConn(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	conn := newRPCConn(clientR, clientW)
	defer conn.Close()

	go func() {
		reader := bufio.NewReader(serverR)
		for {
			var contentLength int
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					break
				}
				if strings.HasPrefix(line, "Content-Length: ") {
					fmt.Sscanf(line, "Content-Length: %d", &contentLength)
				}
			}
			body := make([]byte, contentLength)
			io.ReadFull(reader, body)

			var req jsonrpcMessage
			json.Unmarshal(body, &req)

			if req.Method == "test/echo" {
				resp := jsonrpcMessage{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  req.Params,
				}
				data, _ := json.Marshal(resp)
				serverW.Write(fmt.Appendf(nil, "Content-Length: %d\r\n\r\n", len(data)))
				serverW.Write(data)
			}
		}
	}()

	var result json.RawMessage
	params := map[string]string{"hello": "world"}
	if err := conn.Call(t.Context(), "test/echo", params, &result); err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["hello"] != "world" {
		t.Fatalf("expected hello=world, got %v", got)
	}
}

// TestLSPServerLifecycle tests the full server lifecycle with a mock LSP process.
func TestLSPServerLifecycle(t *testing.T) {
	mock := startMockLSP(t)

	rootURI := "file:///test"
	cfg := ServerConfig{
		Name:       "mock-test",
		Command:    mock.cmd.Path,
		Args:       []string{},
		Extensions: []string{".go"},
	}
	server := NewLSPServer("mock-test", cfg, rootURI)

	// Directly inject the connection connected to the mock process.
	conn := newRPCConn(mock.stdout, mock.stdin)
	server.conn = conn
	server.state.Store(int32(StateRunning))
	server.initialized = true
	server.startTime = time.Now()
	server.openFiles = make(map[string]struct{})
	server.diags = make(map[string][]Diagnostic)

	// Test goToDefinition
	var locations []Location
	err := server.Call(t.Context(), "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": "file:///test/foo.go"},
		"position":     map[string]any{"line": 5, "character": 3},
	}, &locations)
	if err != nil {
		t.Fatalf("goToDefinition call failed: %v", err)
	}
	if len(locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(locations))
	}
	if locations[0].URI != "file:///test/foo.go" {
		t.Fatalf("expected URI file:///test/foo.go, got %s", locations[0].URI)
	}
	if locations[0].Range.Start.Line != 10 {
		t.Fatalf("expected line 10, got %d", locations[0].Range.Start.Line)
	}

	// Test hover
	var hover Hover
	err = server.Call(t.Context(), "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": "file:///test/foo.go"},
		"position":     map[string]any{"line": 5, "character": 3},
	}, &hover)
	if err != nil {
		t.Fatalf("hover call failed: %v", err)
	}
	if !strings.Contains(hover.Contents.Value, "Foo") {
		t.Fatalf("expected hover content to contain 'Foo', got %s", hover.Contents.Value)
	}

	// Test findReferences (empty result)
	var refs []Location
	err = server.Call(t.Context(), "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": "file:///test/foo.go"},
		"position":     map[string]any{"line": 5, "character": 3},
		"context":      map[string]any{"includeDeclaration": true},
	}, &refs)
	if err != nil {
		t.Fatalf("references call failed: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected 0 references, got %d", len(refs))
	}
}

// TestLSPToolFormatting tests the formatters with sample data.
func TestLSPToolFormatting(t *testing.T) {
	wd := "/test/project"

	t.Run("goToDefinition", func(t *testing.T) {
		result := FormatGoToDefinition(Location{
			URI: "file:///test/project/src/main.go",
			Range: Range{
				Start: Position{Line: 42, Character: 5},
				End:   Position{Line: 42, Character: 15},
			},
		}, wd)
		if !strings.Contains(result, "src/main.go:43:6") {
			t.Fatalf("expected src/main.go:43:6 in result, got: %s", result)
		}
	})

	t.Run("goToDefinition_multiple", func(t *testing.T) {
		result := FormatGoToDefinition([]Location{
			{URI: "file:///test/project/src/a.go", Range: Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 5}}},
			{URI: "file:///test/project/src/b.go", Range: Range{Start: Position{Line: 10, Character: 3}, End: Position{Line: 10, Character: 8}}},
		}, wd)
		if !strings.Contains(result, "2 definitions") {
			t.Fatalf("expected '2 definitions' in result, got: %s", result)
		}
	})

	t.Run("hover", func(t *testing.T) {
		result := FormatHover(&Hover{
			Contents: MarkupContent{Kind: "markdown", Value: "**func** Foo()"},
		}, wd)
		if !strings.Contains(result, "Foo") {
			t.Fatalf("expected hover content containing 'Foo', got: %s", result)
		}
	})

	t.Run("findReferences_grouped", func(t *testing.T) {
		result := FormatFindReferences([]Location{
			{URI: "file:///test/project/src/main.go", Range: Range{Start: Position{Line: 5, Character: 0}, End: Position{Line: 5, Character: 5}}},
			{URI: "file:///test/project/src/main.go", Range: Range{Start: Position{Line: 12, Character: 3}, End: Position{Line: 12, Character: 8}}},
		}, wd)
		if !strings.Contains(result, "2 references") {
			t.Fatalf("expected '2 references', got: %s", result)
		}
		if !strings.Contains(result, "src/main.go") {
			t.Fatalf("expected src/main.go in result, got: %s", result)
		}
	})

	t.Run("documentSymbol", func(t *testing.T) {
		result := formatDocSymNode(DocumentSymbol{
			Name: "MyStruct",
			Kind: SKStruct,
			Range: Range{
				Start: Position{Line: 1, Character: 0},
				End:   Position{Line: 20, Character: 1},
			},
			Children: []DocumentSymbol{
				{
					Name: "Field1",
					Kind: SKField,
					Range: Range{
						Start: Position{Line: 3, Character: 2},
						End:   Position{Line: 3, Character: 10},
					},
				},
			},
		}, 0)
		formatted := strings.Join(result, "\n")
		if !strings.Contains(formatted, "MyStruct") || !strings.Contains(formatted, "Field1") {
			t.Fatalf("expected nested symbols, got: %s", formatted)
		}
	})

	t.Run("workspaceSymbol", func(t *testing.T) {
		result := FormatWorkspaceSymbol([]SymbolInformation{
			{Name: "Foo", Kind: SKFunction, Location: Location{URI: "file:///test/project/src/main.go", Range: Range{Start: Position{Line: 1, Character: 0}}}},
			{Name: "Bar", Kind: SKVariable, Location: Location{URI: "file:///test/project/src/utils.go", Range: Range{Start: Position{Line: 5, Character: 0}}}},
		}, wd)
		if !strings.Contains(result, "2 symbols") {
			t.Fatalf("expected '2 symbols', got: %s", result)
		}
	})

	t.Run("no_results", func(t *testing.T) {
		if r := FormatGoToDefinition(nil, wd); r != "No definition found." {
			t.Fatalf("expected 'No definition found.', got: %s", r)
		}
		if r := FormatFindReferences(nil, wd); r != "No references found." {
			t.Fatalf("expected 'No references found.', got: %s", r)
		}
		if r := FormatHover(nil, wd); r != "No hover information available." {
			t.Fatalf("expected 'No hover information available.', got: %s", r)
		}
	})
}

// TestLSPServerConfig tests config conversion.
func TestLSPServerConfig(t *testing.T) {
	cfg := ServerConfig{
		Name:       "gopls",
		Command:    "gopls",
		Args:       []string{},
		Extensions: []string{".go"},
		Languages:  []string{"go"},
	}
	if cfg.Name != "gopls" {
		t.Fatalf("expected name gopls, got %s", cfg.Name)
	}
	if cfg.Command != "gopls" {
		t.Fatalf("expected command gopls, got %s", cfg.Command)
	}
	if len(cfg.Args) != 0 {
		t.Fatalf("expected empty args, got %v", cfg.Args)
	}
	if len(cfg.Extensions) != 1 || cfg.Extensions[0] != ".go" {
		t.Fatalf("expected [.go], got %v", cfg.Extensions)
	}
	if len(cfg.Languages) != 1 || cfg.Languages[0] != "go" {
		t.Fatalf("expected [go], got %v", cfg.Languages)
	}
}

// TestJSONRPCConnNotification verifies server-to-client notifications dispatch.
func TestJSONRPCConnNotification(t *testing.T) {
	clientR, serverW := io.Pipe()
	_, clientW := io.Pipe()

	conn := newRPCConn(clientR, clientW)
	defer conn.Close()

	notifCh := make(chan string, 1)
	conn.RegisterHandler("textDocument/publishDiagnostics", func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		notifCh <- string(params)
		return nil, nil
	})

	notif := jsonrpcMessage{
		JSONRPC: "2.0",
		Method:  "textDocument/publishDiagnostics",
		Params:  json.RawMessage(`{"uri":"file:///test.go","diagnostics":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":5}},"severity":1,"message":"test error"}]}`),
	}
	data, _ := json.Marshal(notif)
	serverW.Write(fmt.Appendf(nil, "Content-Length: %d\r\n\r\n", len(data)))
	serverW.Write(data)

	select {
	case params := <-notifCh:
		if !strings.Contains(params, "test error") {
			t.Fatalf("expected 'test error' in notification, got: %s", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}
}

// TestDetectLanguage tests language detection.
func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"foo.go", "go"},
		{"foo.ts", "typescript"},
		{"foo.tsx", "typescript"},
		{"foo.js", "javascript"},
		{"foo.py", "python"},
		{"foo.rs", "rust"},
		{"foo.java", "java"},
		{"foo.cpp", "cpp"},
		{"foo.rb", "ruby"},
		{"foo.md", "markdown"},
		{"foo.unknown", "plaintext"},
	}
	for _, tt := range tests {
		got := detectLanguage(tt.path)
		if got != tt.expected {
			t.Errorf("detectLanguage(%q) = %q, want %q", tt.path, got, tt.expected)
		}
	}
}

// TestFindReferencesEmptyResult verifies empty reference results.
func TestFindReferencesEmptyResult(t *testing.T) {
	for _, r := range []string{FormatFindReferences([]Location{}, "/test"), FormatFindReferences(nil, "/test")} {
		if r != "No references found." {
			t.Fatalf("expected 'No references found.', got: %s", r)
		}
	}
}

// TestURIHelpers tests URI conversion.
func TestURIHelpers(t *testing.T) {
	uri := PathToURI("/home/user/project/main.go")
	if !strings.HasPrefix(uri, "file://") {
		t.Fatalf("expected file:// URI, got: %s", uri)
	}
	path := URItoPath("file:///home/user/project/main.go")
	if path != "/home/user/project/main.go" {
		t.Fatalf("expected /home/user/project/main.go, got: %s", path)
	}
}

// TestSymbolKindStrings tests SymbolKind string conversion.
func TestSymbolKindStrings(t *testing.T) {
	tests := []struct {
		kind     SymbolKind
		expected string
	}{
		{SKFile, "File"},
		{SKFunction, "Function"},
		{SKClass, "Class"},
		{SKInterface, "Interface"},
		{SKStruct, "Struct"},
		{SKMethod, "Method"},
		{SKVariable, "Variable"},
		{SymbolKind(999), "Unknown"},
	}
	for _, tt := range tests {
		if got := SymbolKindString(tt.kind); got != tt.expected {
			t.Errorf("SymbolKindString(%d) = %q, want %q", tt.kind, got, tt.expected)
		}
	}
}
