// Package mcpsupport manages MCP (Model Context Protocol) server connections
// and exposes their tools through the Tachi tool registry.
package mcpsupport

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

// ConnectAll connects to all configured MCP servers. It returns a slice of
// MCPTool wrappers that were discovered, along with any per-server errors
// (non-fatal; some servers may succeed while others fail).
func (m *Manager) ConnectAll(ctx context.Context, servers []config.MCPServerConfig) ([]MCPTool, []error) {
	var tools []MCPTool
	var errs []error

	for i := range servers {
		srv := &servers[i]
		serverTools, err := m.connect(ctx, srv)
		if err != nil {
			errs = append(errs, fmt.Errorf("mcp server %q: %w", srv.Name, err))
			continue
		}
		tools = append(tools, serverTools...)
	}

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

// Close disconnects all MCP server connections gracefully.
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
