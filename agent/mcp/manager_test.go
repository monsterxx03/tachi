package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubMCPClient implements MCPClient for testing.
type stubMCPClient struct {
	mu         sync.Mutex
	initialize func(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error)
	listTools  func(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)
	callTool   func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	closeFn    func() error
}

func (s *stubMCPClient) Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialize != nil {
		return s.initialize(ctx, req)
	}
	return &mcp.InitializeResult{}, nil
}

func (s *stubMCPClient) ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listTools != nil {
		return s.listTools(ctx, req)
	}
	return &mcp.ListToolsResult{}, nil
}

func (s *stubMCPClient) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callTool != nil {
		return s.callTool(ctx, req)
	}
	return &mcp.CallToolResult{}, nil
}

func (s *stubMCPClient) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeFn != nil {
		return s.closeFn()
	}
	return nil
}

// addTestClient injects a mock client into the Manager for testing.
func addTestClient(m *Manager, name string, client MCPClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[name] = client
}

func TestNewManager(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	assert.NotNil(t, m)
	assert.Empty(t, m.ConnectedServers())
}

func TestManager_IsConnected(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	assert.False(t, m.IsConnected("nonexistent"))

	addTestClient(m, "test-server", &stubMCPClient{})
	assert.True(t, m.IsConnected("test-server"))
}

func TestManager_ConnectedServers(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	addTestClient(m, "server-a", &stubMCPClient{})
	addTestClient(m, "server-b", &stubMCPClient{})

	servers := m.ConnectedServers()
	assert.ElementsMatch(t, []string{"server-a", "server-b"}, servers)
}

func TestManager_Disconnect_NotConnected(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	err := m.Disconnect("nonexistent")
	assert.NoError(t, err)
}

func TestManager_Disconnect_Success(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	closed := false
	addTestClient(m, "test-server", &stubMCPClient{
		closeFn: func() error {
			closed = true
			return nil
		},
	})

	err := m.Disconnect("test-server")
	assert.NoError(t, err)
	assert.True(t, closed, "Close() should be called on disconnect")
	assert.False(t, m.IsConnected("test-server"))
}

func TestManager_Close(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	var mu sync.Mutex
	closeCount := 0
	addTestClient(m, "server-a", &stubMCPClient{
		closeFn: func() error {
			mu.Lock()
			defer mu.Unlock()
			closeCount++
			return nil
		},
	})
	addTestClient(m, "server-b", &stubMCPClient{
		closeFn: func() error {
			mu.Lock()
			defer mu.Unlock()
			closeCount++
			return nil
		},
	})

	m.Close()
	assert.Equal(t, 2, closeCount)
	assert.Empty(t, m.ConnectedServers())
}

func TestManager_CallTool_NotConnected(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	_, err := m.CallTool(t.Context(), "nonexistent", "tool", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestManager_CallTool_Success(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	addTestClient(m, "test-server", &stubMCPClient{
		callTool: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			assert.Equal(t, "my_tool", req.Params.Name)
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{Type: "text", Text: "result data"},
				},
			}, nil
		},
	})

	result, err := m.CallTool(t.Context(), "test-server", "my_tool", nil)
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestManager_CallTool_Error(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	addTestClient(m, "test-server", &stubMCPClient{
		callTool: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("tool execution failed")
		},
	})

	_, err := m.CallTool(t.Context(), "test-server", "failing_tool", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "tool execution failed")
}

func TestManager_GetOAuthHandler_NotConnected(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	h := m.GetOAuthHandler("nonexistent")
	assert.Nil(t, h)
}

func TestManager_GetOAuthHandler_StubClient(t *testing.T) {
	// Stub clients (not *client.Client) should return nil OAuth handler
	m := NewManager(t.Context(), nil, nil)
	addTestClient(m, "test-server", &stubMCPClient{})
	h := m.GetOAuthHandler("test-server")
	assert.Nil(t, h)
}

func TestManager_Constructor(t *testing.T) {
	m := NewManager(t.Context(), nil, logger.Default())
	assert.NotNil(t, m)
}

func TestManager_Reconnect_ServerNotConnected(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	// ConnectAll to an empty config — should succeed with no tools
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	tools, errs := m.ConnectAll(t.Context(), nil)
	assert.Empty(t, tools)
	assert.Empty(t, errs)
}

func TestManager_Concurrency(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			addTestClient(m, "server", &stubMCPClient{})
		})
		wg.Go(func() {
			_ = m.IsConnected("server")
		})
		wg.Go(func() {
			_ = m.ConnectedServers()
		})
	}
	wg.Wait()
	// No race — run with -race to verify
}

func TestManager_Disconnect_CloseError(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	addTestClient(m, "failing-server", &stubMCPClient{
		closeFn: func() error {
			return errors.New("close failure")
		},
	})

	err := m.Disconnect("failing-server")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "close failure")
	assert.False(t, m.IsConnected("failing-server"),
		"server should be removed even if Close() errors")
}

func TestManager_Close_SomeErrors(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)

	addTestClient(m, "good-server", &stubMCPClient{
		closeFn: func() error { return nil },
	})
	addTestClient(m, "bad-server", &stubMCPClient{
		closeFn: func() error { return errors.New("fail") },
	})

	// Close should not panic — errors are logged, not returned
	m.Close()
	assert.Empty(t, m.ConnectedServers())
}

func TestManager_ConnectAll_DisabledServers(t *testing.T) {
	m := NewManager(t.Context(), nil, nil)
	baseDir := t.TempDir()
	config.SetBaseDir(baseDir)
	t.Cleanup(func() { config.SetBaseDir("") })

	// A disabled server should be skipped
	servers := []config.MCPServerConfig{
		{Name: "disabled-server", Enabled: new(false)},
	}

	tools, errs := m.ConnectAll(t.Context(), servers)
	assert.Empty(t, tools)
	assert.Empty(t, errs)
}

func TestIsWhitelisted(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		whitelist []string
		want      bool
	}{
		{
			name:      "exact match",
			toolName:  "search_users",
			whitelist: []string{"search_users", "get_calendar"},
			want:      true,
		},
		{
			name:      "case insensitive match",
			toolName:  "Search_Users",
			whitelist: []string{"search_users"},
			want:      true,
		},
		{
			name:      "no match",
			toolName:  "delete_everything",
			whitelist: []string{"search_users", "get_calendar"},
			want:      false,
		},
		{
			name:      "empty whitelist — no filtering",
			toolName:  "anything",
			whitelist: nil,
			want:      false, // isWhitelisted returns false for empty list; caller checks len>0
		},
		{
			name:      "wildcard: prefix * matches any suffix",
			toolName:  "search_users",
			whitelist: []string{"search_*"},
			want:      true,
		},
		{
			name:      "wildcard: suffix * matches any prefix",
			toolName:  "search_users",
			whitelist: []string{"*_users"},
			want:      true,
		},
		{
			name:      "wildcard: middle * matches any middle",
			toolName:  "server_search_users_v2",
			whitelist: []string{"*search*"},
			want:      true,
		},
		{
			name:      "wildcard: ? matches exactly one character",
			toolName:  "get_users",
			whitelist: []string{"get_?sers"},
			want:      true,
		},
		{
			name:      "wildcard: ? does not match more than one char",
			toolName:  "get_uusers",
			whitelist: []string{"get_?sers"},
			want:      false,
		},
		{
			name:      "wildcard: case insensitive",
			toolName:  "Search_Users_Admin",
			whitelist: []string{"search_*"},
			want:      true,
		},
		{
			name:      "wildcard: multiple * in one pattern",
			toolName:  "mcp__server1__tool_name",
			whitelist: []string{"mcp__*__tool_*"},
			want:      true,
		},
		{
			name:      "wildcard: character class [abc]",
			toolName:  "get_file",
			whitelist: []string{"get_[af]ile"},
			want:      true,
		},
		{
			name:      "wildcard: character class no match",
			toolName:  "get_zile",
			whitelist: []string{"get_[af]ile"},
			want:      false,
		},
		{
			name:      "no glob chars: exact substring is not a match",
			toolName:  "search_users_admin",
			whitelist: []string{"search_users"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isWhitelisted(tt.toolName, tt.whitelist)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMatchWildcard(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		s       string
		want    bool
	}{
		{
			name:    "exact match",
			pattern: "search_users",
			s:       "search_users",
			want:    true,
		},
		{
			name:    "case insensitive",
			pattern: "SEARCH_USERS",
			s:       "search_users",
			want:    true,
		},
		{
			name:    "invalid glob pattern returns false",
			pattern: "[unclosed",
			s:       "anything",
			want:    false,
		},
		{
			name:    "star matches empty string",
			pattern: "prefix_*",
			s:       "prefix_",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchWildcard(tt.pattern, tt.s)
			assert.Equal(t, tt.want, got)
		})
	}
}
