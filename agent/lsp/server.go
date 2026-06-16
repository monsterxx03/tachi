package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ServerState represents the current state of an LSP server instance.
type ServerState int32

const (
	StateStopped  ServerState = 0
	StateStarting ServerState = 1
	StateRunning  ServerState = 2
	StateError    ServerState = 3
)

// LSPServer manages a single LSP server process and its JSON-RPC connection.
type LSPServer struct {
	name    string
	cfg     ServerConfig
	rootURI string

	state     atomic.Int32
	startTime time.Time
	lastError error
	crashCnt  int

	cmd    *exec.Cmd
	conn   *jsonrpcConn
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu          sync.Mutex
	isStopping  bool
	initialized bool
	// startCh is closed when Start() completes (success or failure).
	// Used by GetServer to wait for lazy-start to finish when racing with
	// another goroutine that initiated Start() first.
	startCh chan struct{}

	// sem limits concurrent requests to this server (concurrency_limit).
	sem chan struct{}

	// capabilities from the server's InitializeResult, used for capability
	// pre-checks before sending operations.
	capabilities ServerCapabilities

	// settings from config, returned via workspace/configuration handler.
	settings map[string]any

	// openFiles tracks which files have been opened via didOpen on this server.
	openFiles map[string]struct{} // URI → open
	ofMu      sync.Mutex

	// diagnostics caches the latest diagnostics published by the server.
	diags      map[string][]Diagnostic // URI → diagnostics
	diagsMu    sync.RWMutex
	diagsVer   atomic.Int64 // incremented on each publishDiagnostics
	diagsCount int          // total diagnostic count (cached)
}

// NewLSPServer creates a new LSP server instance. It does not start the process.
func NewLSPServer(name string, cfg ServerConfig, rootURI string) *LSPServer {
	concurrencyLimit := cfg.ConcurrencyLimit
	if concurrencyLimit <= 0 {
		concurrencyLimit = 4
	}
	return &LSPServer{
		name:      name,
		cfg:       cfg,
		rootURI:   rootURI,
		sem:       make(chan struct{}, concurrencyLimit),
		openFiles: make(map[string]struct{}),
		diags:     make(map[string][]Diagnostic),
		settings:  cfg.Settings,
	}
}

// Name returns the server name.
func (s *LSPServer) Name() string { return s.name }

// State returns the current server state.
func (s *LSPServer) State() ServerState { return ServerState(s.state.Load()) }

// IsHealthy returns true when the server is fully initialized and running.
func (s *LSPServer) IsHealthy() bool {
	return s.State() == StateRunning && s.initialized
}

// Extensions returns the file extensions this server handles.
func (s *LSPServer) Extensions() []string { return s.cfg.Extensions }

// Start launches the LSP server process and performs the LSP initialize handshake.
func (s *LSPServer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.State() == StateRunning {
		return nil
	}
	if s.State() == StateStarting {
		return fmt.Errorf("server %s is already starting", s.name)
	}

	// Create a fresh done channel for this start attempt and ensure it is
	// always closed so waiters in GetServer are unblocked.
	s.startCh = make(chan struct{})
	defer func() { close(s.startCh) }()

	s.state.Store(int32(StateStarting))
	s.isStopping = false

	// Resolve startup timeout: use per-server or global default.
	timeout := s.cfg.StartupTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the command with own context for lifecycle control.
	// We do NOT use exec.CommandContext because that kills the process
	// when the context is cancelled — we manage the process lifecycle
	// ourselves via Stop().
	workDir := s.cfg.WorkspaceFolder
	if workDir == "" {
		workDir = "." // will be resolved from rootURI later
	}
	cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
	cmd.Dir = workDir

	// Process group isolation.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Environment: inherit + overrides.
	cmd.Env = os.Environ()
	for k, v := range s.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Create stdio pipes.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.setError(fmt.Errorf("stdin pipe: %w", err))
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.setError(fmt.Errorf("stdout pipe: %w", err))
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.setError(fmt.Errorf("stderr pipe: %w", err))
		return err
	}

	// Capture stderr for debugging.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				slog.Debug("[LSP "+s.name+"]", "stderr", string(buf[:n]))
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		s.setError(fmt.Errorf("start process: %w", err))
		return err
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout

	// Create JSON-RPC connection.
	s.conn = newRPCConn(stdout, stdin)

	// Register workspace/configuration handler to return configured settings.
	s.conn.RegisterHandler("workspace/configuration", func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if s.settings != nil {
			return []map[string]any{s.settings}, nil
		}
		return []map[string]any{{}}, nil
	})

	// Register diagnostics handler to cache publishDiagnostics notifications.
	s.conn.RegisterHandler("textDocument/publishDiagnostics", func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		var p PublishDiagnosticsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, nil // silently ignore malformed diagnostics
		}
		s.diagsMu.Lock()
		s.diags[p.URI] = p.Diagnostics
		total := 0
		for _, diags := range s.diags {
			total += len(diags)
		}
		s.diagsCount = total
		s.diagsVer.Add(1)
		s.diagsMu.Unlock()
		return nil, nil
	})

	// Perform LSP initialize handshake.
	initParams := InitializeParams{
		ProcessID: os.Getpid(),
		ClientInfo: &ClientInfo{
			Name:    "tachi",
			Version: "0.1.0",
		},
		RootURI: s.rootURI,
		Trace:   "off",
		Capabilities: map[string]any{
			"textDocument": map[string]any{
				"definition":     map[string]any{"dynamicRegistration": false, "linkSupport": true},
				"references":     map[string]any{"dynamicRegistration": false},
				"hover":          map[string]any{"dynamicRegistration": false, "contentFormat": []string{"markdown", "plaintext"}},
				"documentSymbol": map[string]any{"dynamicRegistration": false, "hierarchicalDocumentSymbolSupport": true},
				"callHierarchy":  map[string]any{"dynamicRegistration": false},
			},
			"workspace": map[string]any{
				"workspaceFolders": true,
				"configuration":    true,
			},
			"general": map[string]any{
				"positionEncodings": []string{"utf-16"},
			},
		},
		InitializationOptions: s.cfg.InitializationOpts,
		WorkspaceFolders: []WorkspaceFolder{
			{URI: s.rootURI, Name: filepath.Base(s.rootURI)},
		},
	}

	var initResult InitializeResult
	if err := s.conn.Call(startCtx, "initialize", initParams, &initResult); err != nil {
		s.cleanupProcess()
		s.setError(fmt.Errorf("initialize: %w", err))
		return err
	}

	// Send initialized notification.
	if err := s.conn.Notify(startCtx, "initialized", map[string]any{}); err != nil {
		s.cleanupProcess()
		s.setError(fmt.Errorf("initialized: %w", err))
		return err
	}

	// Store server capabilities for capability pre-checks.
	s.capabilities = initResult.Capabilities

	s.initialized = true
	s.state.Store(int32(StateRunning))
	s.startTime = time.Now()
	slog.Debug("LSP server started", "name", s.name, "pid", cmd.Process.Pid)

	// Background goroutine to detect unexpected exit (crash).
	go func() {
		if err := cmd.Wait(); err != nil {
			if !s.isStopping {
				slog.Warn("LSP server crashed", "name", s.name, "error", err)
				s.state.Store(int32(StateError))
				s.lastError = err
				s.crashCnt++
			}
		}
	}()

	return nil
}

// Stop gracefully shuts down the LSP server.
func (s *LSPServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.State() == StateStopped {
		return nil
	}

	s.isStopping = true

	// Graceful LSP shutdown.
	if s.conn != nil && s.initialized {
		_ = s.conn.Call(ctx, "shutdown", nil, nil)
		_ = s.conn.Notify(ctx, "exit", nil)
		s.conn.Close()
	}

	s.cleanupProcess()
	s.state.Store(int32(StateStopped))
	s.openFiles = make(map[string]struct{})
	s.initialized = false

	return nil
}

// Capabilities returns the server's capabilities from the InitializeResult.
// Only valid after IsHealthy() returns true.
func (s *LSPServer) Capabilities() ServerCapabilities {
	return s.capabilities
}

// Call sends an LSP request and unmarshals the result.
// It respects concurrency_limit and retries ContentModified errors with
// exponential backoff (500ms → 1s → 2s).
func (s *LSPServer) Call(ctx context.Context, method string, params, result any) error {
	if !s.IsHealthy() {
		return fmt.Errorf("server %s is %s", s.name, s.stateString())
	}

	// Acquire semaphore slot (concurrency control).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// ContentModified exponential backoff (error code -32801).
	for attempt := range 4 {
		err := s.conn.Call(ctx, method, params, result)
		if err == nil {
			return nil
		}
		if isContentModified(err) && attempt < 3 {
			delay := time.Duration(500*(1<<attempt)) * time.Millisecond
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return err
	}
	return nil
}

// Notify sends an LSP notification.
func (s *LSPServer) Notify(ctx context.Context, method string, params any) error {
	if !s.IsHealthy() {
		return fmt.Errorf("server %s is %s", s.name, s.stateString())
	}

	// Acquire semaphore slot for notification too, to prevent excessive
	// concurrent writes to the server's stdin.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	return s.conn.Notify(ctx, method, params)
}

// isContentModified reports whether a JSON-RPC error is -32801 (ContentModified),
// which some LSP servers (notably rust-analyzer) send when the result is stale
// due to ongoing indexing.
func isContentModified(err error) bool {
	if rpcErr, ok := err.(*jsonrpcErr); ok && rpcErr.Code == -32801 {
		return true
	}
	return false
}

// OpenFile sends textDocument/didOpen for the given file.
func (s *LSPServer) OpenFile(ctx context.Context, uri, langID, content string) error {
	s.ofMu.Lock()
	if _, exists := s.openFiles[uri]; exists {
		s.ofMu.Unlock()
		return nil // already open
	}
	s.ofMu.Unlock()

	params := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": langID,
			"version":    1,
			"text":       content,
		},
	}
	if err := s.Notify(ctx, "textDocument/didOpen", params); err != nil {
		return err
	}

	s.ofMu.Lock()
	s.openFiles[uri] = struct{}{}
	s.ofMu.Unlock()
	return nil
}

// ChangeFile sends textDocument/didChange for the given file.
func (s *LSPServer) ChangeFile(ctx context.Context, uri, content string) error {
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":     uri,
			"version": 2,
		},
		"contentChanges": []map[string]any{
			{"text": content},
		},
	}
	return s.Notify(ctx, "textDocument/didChange", params)
}

// CloseFile sends textDocument/didClose for the given file.
func (s *LSPServer) CloseFile(ctx context.Context, uri string) error {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}
	if err := s.Notify(ctx, "textDocument/didClose", params); err != nil {
		return err
	}
	s.ofMu.Lock()
	delete(s.openFiles, uri)
	s.ofMu.Unlock()
	return nil
}

// IsFileOpen returns true if the file has been opened on this server.
func (s *LSPServer) IsFileOpen(uri string) bool {
	s.ofMu.Lock()
	defer s.ofMu.Unlock()
	_, ok := s.openFiles[uri]
	return ok
}

// CloseMissingFiles checks all files currently open on this server and sends
// textDocument/didClose for any that no longer exist on disk (deleted, renamed,
// or moved). Call after tool operations that may remove files (e.g. Bash).
func (s *LSPServer) CloseMissingFiles(ctx context.Context) {
	s.ofMu.Lock()
	var missing []string
	for uri := range s.openFiles {
		path := URItoPath(uri)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, uri)
		}
	}
	s.ofMu.Unlock()

	for _, uri := range missing {
		if err := s.CloseFile(ctx, uri); err != nil {
			slog.Debug("LSP close missing file", "name", s.name, "uri", uri, "error", err)
		}
	}
}

// GetDiagnostics returns all cached diagnostics for all files on this server.
func (s *LSPServer) GetDiagnostics() map[string][]Diagnostic {
	s.diagsMu.RLock()
	defer s.diagsMu.RUnlock()
	result := make(map[string][]Diagnostic, len(s.diags))
	maps.Copy(result, s.diags)
	return result
}

// GetFileDiagnostics returns cached diagnostics for a specific file.
func (s *LSPServer) GetFileDiagnostics(uri string) []Diagnostic {
	s.diagsMu.RLock()
	defer s.diagsMu.RUnlock()
	return s.diags[uri]
}

// DiagnosticCount returns the total number of cached diagnostics.
func (s *LSPServer) DiagnosticCount() int {
	s.diagsMu.RLock()
	defer s.diagsMu.RUnlock()
	return s.diagsCount
}

// WaitForDiagnostics blocks until diagnostics stop changing for settleDuration,
// or the context is cancelled. Uses a version counter to detect changes.
// Useful after didChange to wait for the server to publish fresh diagnostics.
func (s *LSPServer) WaitForDiagnostics(ctx context.Context, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	lastVer := s.diagsVer.Load()
	stableAt := time.Now()
	const settleDuration = 300 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
			ver := s.diagsVer.Load()
			if ver != lastVer {
				lastVer = ver
				stableAt = time.Now()
			} else if time.Since(stableAt) >= settleDuration {
				return // stable for settleDuration
			}
		}
	}
}

// setError updates the server state to error and records the error.
func (s *LSPServer) setError(err error) {
	s.state.Store(int32(StateError))
	s.lastError = err
	s.initialized = false
}

// cleanupProcess tears down the OS process.
func (s *LSPServer) cleanupProcess() {
	if s.cmd != nil && s.cmd.Process != nil {
		pgid := s.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		time.Sleep(500 * time.Millisecond)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.cmd = nil
	s.conn = nil
}

func (s *LSPServer) stateString() string {
	switch s.State() {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateError:
		if s.lastError != nil {
			return "error: " + s.lastError.Error()
		}
		return "error"
	default:
		return "unknown"
	}
}
