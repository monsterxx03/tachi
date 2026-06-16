package lsp

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LSPManager manages multiple LSP servers and routes requests by file extension.
// Servers are started lazily on first access.
type LSPManager struct {
	cfg     *Config
	servers map[string]*LSPServer // name → server
	extIdx  map[string]*LSPServer // ".go" → server (lowercase extension)

	configured bool
	mu         sync.Mutex
}

// NewManager creates an LSPManager from configuration. It does not start any servers.
func NewManager(cfg *Config) *LSPManager {
	m := &LSPManager{
		cfg:    cfg,
		servers: make(map[string]*LSPServer),
		extIdx:  make(map[string]*LSPServer),
	}

	// Build index.
	for i := range cfg.Servers {
		srvCfg := cfg.Servers[i]
		rootURI := srvCfg.WorkspaceFolder
		if rootURI == "" {
			cwd, _ := os.Getwd()
			rootURI = PathToURI(cwd)
		}

		server := NewLSPServer(srvCfg.Name, srvCfg, rootURI)
		m.servers[srvCfg.Name] = server

		for _, ext := range srvCfg.Extensions {
			norm := strings.ToLower(ext)
			if !strings.HasPrefix(norm, ".") {
				norm = "." + norm
			}
			m.extIdx[norm] = server
		}
	}

	if len(cfg.Servers) > 0 {
		m.configured = true
	}

	return m
}

// GetServer returns the LSP server for the given file path, starting it if needed.
func (m *LSPManager) GetServer(ctx context.Context, filePath string) (*LSPServer, error) {
	if !m.configured {
		return nil, nil
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	m.mu.Lock()
	server := m.extIdx[ext]
	m.mu.Unlock()

	if server == nil {
		return nil, nil
	}

	// Lazy start.
	if !server.IsHealthy() {
		if server.State() == StateStarting {
			// Another goroutine is starting this server — wait for it.
			select {
			case <-server.startCh:
				// Start() completed; re-check health below.
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if !server.IsHealthy() {
				return nil, fmt.Errorf("LSP server %s failed to start", server.name)
			}
		} else {
			slog.Debug("Lazy-starting LSP server", "name", server.name, "ext", ext)
			if err := server.Start(ctx); err != nil {
				return nil, fmt.Errorf("start LSP server %s: %w", server.name, err)
			}
		}
	}

	return server, nil
}

// SyncFile ensures the file is opened on the appropriate LSP server.
// If already open, sends didChange with current content.
func (m *LSPManager) SyncFile(ctx context.Context, filePath string) error {
	server, err := m.GetServer(ctx, filePath)
	if err != nil {
		return err
	}
	if server == nil {
		return nil
	}

	uri := PathToURI(filePath)
	if server.IsFileOpen(uri) {
		// File already open — send didChange if content changed.
		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read file for sync: %w", err)
		}
		return server.ChangeFile(ctx, uri, string(content))
	}

	// First time opening this file.
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file for open: %w", err)
	}
	if int64(len(content)) > m.cfg.MaxFileSize {
		return nil // skip oversized files
	}

	langID := detectLanguage(filePath)
	return server.OpenFile(ctx, uri, langID, string(content))
}

// Shutdown gracefully stops all LSP servers.
func (m *LSPManager) Shutdown(ctx context.Context) {
	var wg sync.WaitGroup
	for _, server := range m.servers {
		wg.Go(func() {
			if err := server.Stop(ctx); err != nil {
				slog.Warn("LSP server stop error", "name", server.name, "error", err)
			}
		})
	}
	wg.Wait()
}

// WaitForDiagnostics waits up to timeout for diagnostics to stabilize on
// all servers. Call this after SyncFile to ensure fresh diagnostics are
// available before the reminder collector runs.
func (m *LSPManager) WaitForDiagnostics(ctx context.Context, timeout time.Duration) {
	var wg sync.WaitGroup
	for _, server := range m.servers {
		wg.Go(func() {
			server.WaitForDiagnostics(ctx, timeout)
		})
	}
	wg.Wait()
}

// CloseMissingFiles checks all open files across all servers and closes any
// that no longer exist on disk. Call after tool operations that may delete,
// rename, or move files (e.g. Bash).
func (m *LSPManager) CloseMissingFiles(ctx context.Context) {
	for _, server := range m.servers {
		server.CloseMissingFiles(ctx)
	}
}

// IsConfigured returns true if at least one LSP server is configured.
func (m *LSPManager) IsConfigured() bool { return m.configured }

// Servers returns the map of all configured servers (name → server).
func (m *LSPManager) Servers() map[string]*LSPServer {
	return m.servers
}

// ServerCount returns the number of configured servers.
func (m *LSPManager) ServerCount() int {
	return len(m.servers)
}

// MaxResults returns the configured max_results, defaulting to 50.
func (m *LSPManager) MaxResults() int {
	if m.cfg == nil || m.cfg.MaxResults <= 0 {
		return 50
	}
	return m.cfg.MaxResults
}

// DetectLanguage maps a file path to a language ID for LSP.
func detectLanguage(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc", ".cxx":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".php":
		return "php"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".scala":
		return "scala"
	case ".vue":
		return "vue"
	case ".svelte":
		return "svelte"
	case ".css", ".scss", ".less":
		return "css"
	case ".html", ".htm":
		return "html"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md":
		return "markdown"
	case ".sql":
		return "sql"
	case ".sh", ".bash", ".zsh":
		return "shellscript"
	case ".elisp", ".el":
		return "elisp"
	default:
		return "plaintext"
	}
}

// PathToURI converts a file path to a file:// URI.
func PathToURI(filePath string) string {
	abs, err := filepath.Abs(filePath)
	if err != nil {
		abs = filePath
	}
	// Use net/url for proper encoding.
	u := &url.URL{
		Scheme: "file",
		Path:   abs,
	}
	return u.String()
}

// URItoPath converts a file:// URI back to a file path.
func URItoPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	if u.Scheme == "file" {
		return u.Path
	}
	return uri
}
