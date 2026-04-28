// Package mcp manages MCP (Model Context Protocol) server connections
// and exposes their tools through the Tachi tool registry.
package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// Manager manages the lifecycle of MCP client connections and their tools.
type Manager struct {
	clients map[string]*client.Client // server name -> client
	mu      sync.RWMutex
}

// NewManager creates an empty MCP client manager.
func NewManager() *Manager {
	return &Manager{
		clients: make(map[string]*client.Client),
	}
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
			debuglog.Log("MCP: server %q is disabled, skipping", srv.Name)
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
	timeout := srv.MCPTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var c *client.Client
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

	debuglog.Log("MCP: connected to server %q (%s)",
		srv.Name, srv.Type)

	// Discover tools
	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("list tools failed: %w", err)
	}

	m.mu.Lock()
	m.clients[srv.Name] = c
	m.mu.Unlock()

	debuglog.Log("MCP: server %q has %d tools", srv.Name, len(toolsResult.Tools))

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

func (m *Manager) connectStdio(srv *config.MCPServerConfig) (*client.Client, error) {
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

func (m *Manager) connectHTTP(srv *config.MCPServerConfig, timeout time.Duration) (*client.Client, error) {
	if srv.URL == "" {
		return nil, fmt.Errorf("url is required for http transport")
	}
	opts := []transport.StreamableHTTPCOption{
		transport.WithHTTPTimeout(timeout),
	}
	if len(srv.Headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(srv.Headers))
	}
	return client.NewStreamableHttpClient(srv.URL, opts...)
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
		debuglog.Log("MCP: error disconnecting %q: %v", name, err)
		return fmt.Errorf("disconnect %q: %w", name, err)
	}
	debuglog.Log("MCP: disconnected %q", name)
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
			debuglog.Log("MCP: error closing client %q: %v", name, err)
		}
	}
	m.clients = make(map[string]*client.Client)
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
