// Package mcp manages MCP (Model Context Protocol) server connections
// and exposes their tools through the Tachi tool registry.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/pkg/proxy"
)

// MCPClient is the interface for MCP client operations used by Manager.
// Extracted for testability — the real implementation is *client.Client
// from github.com/mark3labs/mcp-go/client.
type MCPClient interface {
	Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error)
	ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	Close() error
}

// Manager manages the lifecycle of MCP client connections and their tools.
//
// Manager also owns the ToolSearch state — DeferredPool (all discovered MCP
// tools, searchable but not yet registered with the LLM), DiscoveredSet (the
// subset of tools the LLM has explicitly opted into via MCPSearchTools), and
// the initDone channel that signals async ConnectAll completion. These are
// owned here because they share the manager's lifecycle: they're created when
// a manager is built and torn down when it's closed. Callers (AIAgent,
// channel.Manager) used to hold them as separate fields; that worked but
// duplicated lifecycle bookkeeping at every layer. Now there is one source of
// truth.
//
// Pool / DiscoveredSet / InitDone are always non-nil after NewManager.
// MarkInitDone closes the init channel exactly once when ConnectAll-style
// population is finished.
type Manager struct {
	clients map[string]MCPClient // server name -> client
	logger  *debuglog.Logger
	mu      sync.RWMutex

	pool     *DeferredPool
	set      *DiscoveredSet
	initDone chan struct{}

	initDoneOnce sync.Once

	// Tool result truncation — configured from config.ToolResult.
	// 0 means no limit (opt-out).
	maxResultChars int
	resultFileDir  string
}

// NewManager creates an empty MCP client manager with the given tool result
// truncation config. maxChars <= 0 means no limit. Old tool result files in
// fileDir are cleaned up on construction (best-effort, errors are logged).
func NewManager(maxChars int, fileDir string) *Manager {
	if fileDir != "" {
		cleanupOldToolResults(fileDir, defaultToolResultMaxAge)
	}
	return &Manager{
		clients:        make(map[string]MCPClient),
		logger:         debuglog.DefaultLogger,
		pool:           NewDeferredPool(),
		set:            NewDiscoveredSet(),
		initDone:       make(chan struct{}),
		maxResultChars: maxChars,
		resultFileDir:  fileDir,
	}
}

// Pool returns the deferred-tool pool owned by this manager. Always non-nil.
func (m *Manager) Pool() *DeferredPool { return m.pool }

// ToolResultMaxChars returns the configured max chars for tool results.
// 0 means no limit.
func (m *Manager) ToolResultMaxChars() int { return m.maxResultChars }

// ToolResultFileDir returns the configured directory for storing oversized results.
func (m *Manager) ToolResultFileDir() string { return m.resultFileDir }

// DiscoveredSet returns the discovered-tools set owned by this manager.
// Always non-nil.
func (m *Manager) DiscoveredSet() *DiscoveredSet { return m.set }

// InitDone returns a channel that is closed once the manager's async
// initialization (ConnectAll + pool population) has finished. Callers can
// select on it (or use WaitInit) to block until tools are ready.
//
// If the manager is never populated (e.g. no servers configured), the channel
// stays open. Callers that don't care should not block on it.
func (m *Manager) InitDone() <-chan struct{} { return m.initDone }

// MarkInitDone closes the InitDone channel, idempotently. Should be called
// once after ConnectAll-style population has finished.
func (m *Manager) MarkInitDone() {
	m.initDoneOnce.Do(func() { close(m.initDone) })
}

// WaitInit blocks until MarkInitDone is called or the context is cancelled.
// Returns ctx.Err() on cancellation, nil on success.
func (m *Manager) WaitInit(ctx context.Context) error {
	select {
	case <-m.initDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PopulateFromConnect connects to all servers in cfg.MCPServers, inflates the
// manager's deferred pool with every discovered tool, and adds the subset of
// "auto-load" tools (when ToolSearch is disabled, or the per-server
// always_load list matches) to the discovered set.
//
// The discovered set is the contract used by AIAgent.filterActiveSchemas:
// any tool in it is exposed to the LLM. Whether the tool is also eagerly
// registered into a tool.Registry is up to the caller — single-agent callers
// register them on the spot, while channel mode leaves them lazy and lets
// AIAgent.lazyRegisterMCPTool do the registration on first invocation.
//
// Returns:
//   - autoLoad: tools the caller may want to RegisterTool eagerly
//   - all:      every discovered tool (already added to pool)
//   - errs:     per-server connection errors (non-fatal; partial discovery is fine)
//
// PopulateFromConnect does NOT call MarkInitDone — the caller decides when
// initialization is "done" (e.g. after additionally registering reminders).
func (m *Manager) PopulateFromConnect(ctx context.Context, cfg *config.Config) (autoLoad, all []MCPTool, errs []error) {
	all, errs = m.ConnectAll(ctx, cfg.MCPServers)
	if len(all) == 0 {
		return nil, nil, errs
	}

	// Build server config lookup once.
	serverCfgs := make(map[string]config.MCPServerConfig, len(cfg.MCPServers))
	for _, srv := range cfg.MCPServers {
		serverCfgs[srv.Name] = srv
	}

	useToolSearch := cfg.MCPToolSearch.IsEnabled() &&
		len(all) > cfg.MCPToolSearch.MinToolsForSearch

	for _, t := range all {
		srvCfg, hasCfg := serverCfgs[t.ServerName()]

		// Whitelist filtering: if configured, skip tools not in the list.
		if hasCfg && len(srvCfg.Whitelist) > 0 && !isWhitelisted(t.ToolName(), srvCfg.Whitelist) {
			continue
		}

		var searchHint string
		if hasCfg && srvCfg.SearchHints != nil {
			searchHint = srvCfg.SearchHints[t.ToolName()]
		}

		dt := NewDeferredToolFromMCPTool(t, searchHint)
		m.pool.Add(dt)

		isAutoLoad := !useToolSearch
		if !isAutoLoad && hasCfg {
			for _, name := range srvCfg.AlwaysLoadTools {
				if strings.EqualFold(name, t.ToolName()) {
					isAutoLoad = true
					break
				}
			}
		}

		if isAutoLoad {
			m.set.Add(t.Name())
			autoLoad = append(autoLoad, t)
		}
	}

	m.logger.Log("MCP: populated %d tools (%d auto-load, ToolSearch=%v, threshold=%d)",
		m.pool.Len(), len(autoLoad), useToolSearch, cfg.MCPToolSearch.MinToolsForSearch)
	return autoLoad, all, errs
}

// isWhitelisted checks whether a tool name matches an entry in the whitelist.
// Matching is case-insensitive, consistent with AlwaysLoadTools.
// Returns true if the whitelist is empty (no filtering).
func isWhitelisted(toolName string, whitelist []string) bool {
	for _, w := range whitelist {
		if strings.EqualFold(w, toolName) {
			return true
		}
	}
	return false
}

// SetLogger overrides the manager's logger. Channel callers use this to inject
// a channel-specific logger so debug output is tagged with the correct source.
func (m *Manager) SetLogger(l *debuglog.Logger) {
	m.logger = l
}

// ConnectAll connects to all configured MCP servers concurrently. Each server
// connection runs in its own goroutine so a slow or failing server does not
// block others. Returns all discovered MCPTool wrappers and any per-server
// errors (non-fatal; some servers may succeed while others fail).
func (m *Manager) ConnectAll(ctx context.Context, servers []config.MCPServerConfig) ([]MCPTool, []error) {
	var wg sync.WaitGroup

	// Collected concurrently — protected by a single mutex since both
	// slices are always accessed together in critical sections.
	var (
		mu    sync.Mutex
		tools []MCPTool
		errs  []error
	)

	for i := range servers {
		srv := &servers[i]
		if !srv.IsEnabled() {
			debuglog.Log(ctx, "MCP: server %q is disabled, skipping", srv.Name)
			continue
		}

		wg.Go(func() {
			serverTools, err := m.connect(ctx, srv)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("mcp server %q: %w", srv.Name, err))
				return
			}
			tools = append(tools, serverTools...)
		})
	}

	wg.Wait()
	return tools, errs
}

// connect establishes a connection to a single MCP server and discovers its tools.
func (m *Manager) connect(ctx context.Context, srv *config.MCPServerConfig) ([]MCPTool, error) {
	timeout := time.Duration(srv.Timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var c MCPClient
	var err error

	switch srv.Type {
	case config.MCPTransportStdio:
		c, err = m.connectStdio(srv)
	case config.MCPTransportHTTP:
		c, err = m.connectHTTP(srv, timeout)
	default:
		return nil, fmt.Errorf("unsupported MCP transport type: %s", srv.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	// Send initialization request with required protocol version support
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "tachi",
		Version: "1.0.0",
	}
	initReq.Params.Capabilities = mcp.ClientCapabilities{}

	if _, err := c.Initialize(ctx, initReq); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize failed: %w", err)
	}

	debuglog.Log(ctx, "MCP: connected to server %q (%s)",
		srv.Name, srv.Type)

	// Discover tools
	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		// If OAuth is configured but the token isn't available yet,
		// the transport returns ErrOAuthAuthorizationRequired.
		// Give a user-friendly hint.
		if errors.Is(err, transport.ErrOAuthAuthorizationRequired) {
			return nil, &OAuthRequiredError{ServerName: srv.Name}
		}
		return nil, fmt.Errorf("list tools failed: %w", err)
	}

	m.mu.Lock()
	m.clients[srv.Name] = c
	m.mu.Unlock()

	debuglog.Log(ctx, "MCP: server %q has %d tools", srv.Name, len(toolsResult.Tools))

	// Wrap tools
	mcpTools := make([]MCPTool, 0, len(toolsResult.Tools))
	for i := range toolsResult.Tools {
		mcpTools = append(mcpTools, MCPTool{
			serverName: srv.Name,
			serverTool: &toolsResult.Tools[i],
			manager:    m,
		})
	}

	return mcpTools, nil
}

func (m *Manager) connectStdio(srv *config.MCPServerConfig) (MCPClient, error) {
	command := srv.Command
	if command == "" {
		return nil, fmt.Errorf("command is required for stdio transport")
	}

	env := make([]string, 0, len(srv.Env))
	for k, v := range srv.Env {
		env = append(env, k+"="+v)
	}

	return client.NewStdioMCPClient(command, env, srv.Args...)
}

func (m *Manager) connectHTTP(srv *config.MCPServerConfig, timeout time.Duration) (MCPClient, error) {
	if srv.URL == "" {
		return nil, fmt.Errorf("url is required for http transport")
	}
	opts := []transport.StreamableHTTPCOption{
		transport.WithHTTPTimeout(timeout),
	}
	if len(srv.Headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(srv.Headers))
	}
	// Proxy support for HTTP MCP servers.
	if srv.Proxy != "" {
		httpClient, err := proxy.NewHTTPClient(srv.Proxy, timeout)
		if err != nil {
			m.logger.Log("MCP: invalid proxy %q for server %q: %v", srv.Proxy, srv.Name, err)
		} else {
			opts = append(opts, transport.WithHTTPBasicClient(httpClient))
			m.logger.Log("MCP: using proxy %q for server %q", srv.Proxy, srv.Name)
		}
	}
	// If OAuth isn't explicitly configured, check for persisted token / DCR
	// info on disk — a previous DCR-based auth may have left valid tokens.
	if !srv.HasOAuth() && hasPersistedAuth(srv.TokenStorageName()) {
		srv.OAuth = &config.MCPOAuthConfig{}
	}
	if srv.HasOAuth() {
		opts = append(opts, m.oauthOption(srv))
	}
	return client.NewStreamableHttpClient(srv.URL, opts...)
}

// hasPersistedAuth returns true if there's a token file or DCR info file
// on disk for the given storage key. Used to auto-detect OAuth that was set
// up via DCR in a prior session, without explicit oauth: config.
func hasPersistedAuth(storageKey string) bool {
	tokenPath, err := mcpTokenPath(storageKey)
	if err != nil {
		return false
	}
	if _, err := os.Stat(tokenPath); err == nil {
		return true
	}
	dcrPath, err := dcrTokenPath(storageKey)
	if err != nil {
		return false
	}
	_, err = os.Stat(dcrPath)
	return err == nil
}

// oauthOption builds the WithHTTPOAuth transport option from the server config.
// If ClientID is empty but persisted DCR info exists, it is loaded from disk
// so that token refresh works across process restarts.
func (m *Manager) oauthOption(srv *config.MCPServerConfig) transport.StreamableHTTPCOption {
	oauthCfg := srv.OAuth
	tokenStore, err := NewFileTokenStore(srv.TokenStorageName())
	if err != nil {
		m.logger.Log("MCP: failed to create token store for %q: %v", srv.TokenStorageName(), err)
		tokenStore = nil
	}

	clientID := oauthCfg.ClientID
	clientSecret := oauthCfg.ClientSecret

	// DCR: if config has no client_id, try persisted DCR info for refresh support
	if clientID == "" && tokenStore != nil {
		if dcr, err := tokenStore.GetDCRInfo(context.Background()); err == nil {
			clientID = dcr.ClientID
			clientSecret = dcr.ClientSecret
			m.logger.Log("MCP: loaded DCR client_id for %q from disk", srv.Name)
		}
	}

	var scopes []string
	if len(oauthCfg.Scopes) > 0 {
		scopes = oauthCfg.Scopes
	}

	return transport.WithHTTPOAuth(transport.OAuthConfig{
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		ClientURI:             oauthCfg.ClientURI,
		Scopes:                scopes,
		PKCEEnabled:           true,
		AuthServerMetadataURL: oauthCfg.AuthServerMetadataURL,
		TokenStore:            tokenStore,
	})
}

// CallTool invokes a tool on the named MCP server.
func (m *Manager) CallTool(ctx context.Context, serverName string, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	c, ok := m.clients[serverName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("MCP server %q is not connected", serverName)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = arguments

	return c.CallTool(ctx, req)
}

// GetOAuthHandler returns the OAuthHandler for a connected HTTP server,
// or nil if the server is not connected or has no OAuth configured.
func (m *Manager) GetOAuthHandler(name string) *transport.OAuthHandler {
	m.mu.RLock()
	c, ok := m.clients[name]
	m.mu.RUnlock()
	if !ok {
		return nil
	}

	// Type-assert to *client.Client for GetTransport access.
	// This only works for real mcp-go clients; mock clients return nil.
	if realClient, ok := c.(*client.Client); ok {
		trans := realClient.GetTransport()
		if httpConn, ok := trans.(*transport.StreamableHTTP); ok {
			return httpConn.GetOAuthHandler()
		}
	}
	return nil
}

// IsConnected returns whether an MCP server is currently connected.
func (m *Manager) IsConnected(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.clients[name]
	return ok
}

// ConnectedServers returns a list of all connected server names.
func (m *Manager) ConnectedServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	servers := make([]string, 0, len(m.clients))
	for name := range m.clients {
		servers = append(servers, name)
	}
	return servers
}

// Disconnect closes the connection to a specific MCP server and removes it
// from the manager. Returns nil if the server was not connected.
func (m *Manager) Disconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.clients[name]
	if !ok {
		return nil
	}
	delete(m.clients, name)
	if err := c.Close(); err != nil {
		m.logger.Log("MCP: error disconnecting %q: %v", name, err)
		return fmt.Errorf("disconnect %q: %w", name, err)
	}
	m.logger.Log("MCP: disconnected %q", name)
	return nil
}

// Reconnect disconnects an existing server connection (if any) and
// establishes a fresh connection using the given config. Returns the
// server's tools on success.
func (m *Manager) Reconnect(ctx context.Context, srv *config.MCPServerConfig) ([]MCPTool, error) {
	// Disconnect existing connection (ignore errors)
	if _, ok := m.clients[srv.Name]; ok {
		_ = m.Disconnect(srv.Name)
	}

	return m.connect(ctx, srv)
}
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, c := range m.clients {
		if err := c.Close(); err != nil {
			m.logger.Log("MCP: error closing client %q: %v", name, err)
		}
	}
	m.clients = make(map[string]MCPClient)
}

// formatMCPResult formats a CallToolResult into a human-readable string.
func formatMCPResult(result *mcp.CallToolResult) (string, error) {
	if result.IsError {
		return "", &MCPToolError{contentToString(result.Content)}
	}

	var sb strings.Builder
	for _, item := range result.Content {
		if textContent, ok := item.(mcp.TextContent); ok {
			sb.WriteString(textContent.Text)
		}
	}
	return sb.String(), nil
}

// contentToString converts MCP content items to a user-facing string.
func contentToString(content []mcp.Content) string {
	var sb strings.Builder
	for _, item := range content {
		if textContent, ok := item.(mcp.TextContent); ok {
			sb.WriteString(textContent.Text)
		} else {
			sb.WriteString("[non-text content]")
		}
	}
	return sb.String()
}

// MCPToolError represents an error returned by an MCP tool (isError=true).
type MCPToolError struct {
	Message string
}

func (e *MCPToolError) Error() string {
	return fmt.Sprintf("MCP tool error: %s", e.Message)
}
