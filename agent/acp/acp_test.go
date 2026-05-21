package acp

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
)

func TestInitialize(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test-version")

	resp, err := ta.Initialize(context.Background(), acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})

	require.NoError(t, err)
	assert.Equal(t, acp.ProtocolVersion(acp.ProtocolVersionNumber), resp.ProtocolVersion)
	assert.NotNil(t, resp.AgentInfo)
	assert.Equal(t, "tachi", resp.AgentInfo.Name)
	assert.Equal(t, "test-version", resp.AgentInfo.Version)
	assert.True(t, resp.AgentCapabilities.PromptCapabilities.EmbeddedContext)
	assert.True(t, resp.AgentCapabilities.McpCapabilities.Http)
	assert.NotNil(t, resp.AgentCapabilities.SessionCapabilities.List)
	assert.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Close)
	assert.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Resume)
}

func TestConvertContentBlocks_TextOnly(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("Hello, world!"),
	}
	result := convertContentBlocks(blocks)
	assert.Equal(t, "Hello, world!", result)
}

func TestConvertContentBlocks_MultipleBlocks(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("First part. "),
		acp.TextBlock("Second part."),
	}
	result := convertContentBlocks(blocks)
	assert.Equal(t, "First part. Second part.", result)
}

func TestConvertContentBlocks_ResourceEmbed(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.TextBlock("Check this file:\n"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{
				Uri:  "file:///tmp/test.go",
				Text: "package main\n",
			},
		}),
	}
	result := convertContentBlocks(blocks)
	expected := "Check this file:\n--- BEGIN UNTRUSTED FILE CONTENT: /tmp/test.go ---\npackage main\n\n--- END UNTRUSTED FILE CONTENT ---\n"
	assert.Equal(t, expected, result)
}

func TestConvertContentBlocks_ResourceLink(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.ResourceLinkBlock("test.txt", "file:///tmp/test.txt"),
	}
	result := convertContentBlocks(blocks)
	assert.Contains(t, result, "[@file: file:///tmp/test.txt]")
}

func TestExtractPathFromURI(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"file:///tmp/test.go", "/tmp/test.go"},
		{"file:///home/user/code/main.rs", "/home/user/code/main.rs"},
		{"/tmp/test.go", "/tmp/test.go"},
		{"relative/path.txt", "relative/path.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractPathFromURI(tt.input))
		})
	}
}

func TestACPSessionManager_Lifecycle(t *testing.T) {
	sm := NewACPSessionManager()

	// Create a session
	sess := sm.New(context.Background(), "/tmp", "openai", nil, nil, nil)
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "/tmp", sess.cwd)
	assert.Equal(t, "openai", sess.providerType)

	// Get it back
	got, ok := sm.Get(sess.ID)
	assert.True(t, ok)
	assert.Equal(t, sess.ID, got.ID)

	// List
	list := sm.List()
	assert.Len(t, list, 1)

	// Delete
	sm.Delete(sess.ID)
	_, ok = sm.Get(sess.ID)
	assert.False(t, ok)
	assert.Empty(t, sm.List())
}

func TestACPSessionManager_CloseAll(t *testing.T) {
	sm := NewACPSessionManager()

	// Create multiple sessions
	sess1 := sm.New(context.Background(), "/tmp/a", "openai", nil, nil, nil)
	sess2 := sm.New(context.Background(), "/tmp/b", "anthropic", nil, nil, nil)
	assert.Len(t, sm.List(), 2)

	// Close all
	sm.CloseAll()
	assert.Empty(t, sm.List())

	// Verify contexts are cancelled
	assert.Error(t, sess1.ctx.Err())
	assert.Error(t, sess2.ctx.Err())
}

func TestCancel_NonexistentSession(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test")

	// Should not error on nonexistent session
	err := ta.Cancel(context.Background(), acp.CancelNotification{
		SessionId: "nonexistent-id",
	})
	assert.NoError(t, err)
}

func TestCloseSession_NonexistentSession(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test")

	// Should not error on nonexistent session
	resp, err := ta.CloseSession(context.Background(), acp.CloseSessionRequest{
		SessionId: "nonexistent-id",
	})
	assert.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestMapToolKind(t *testing.T) {
	assert.Equal(t, acp.ToolKindRead, mapToolKind("ReadFile"))
	assert.Equal(t, acp.ToolKindEdit, mapToolKind("WriteFile"))
	assert.Equal(t, acp.ToolKindEdit, mapToolKind("EditFile"))
	assert.Equal(t, acp.ToolKindExecute, mapToolKind("Bash"))
	assert.Equal(t, acp.ToolKind(""), mapToolKind("UnknownTool"))
}

func TestMapStopReason(t *testing.T) {
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("stop"))
	assert.Equal(t, acp.StopReasonCancelled, mapStopReason("cancelled"))
	assert.Equal(t, acp.StopReasonCancelled, mapStopReason("interrupted"))
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("error"))
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("budget_exhausted"))
}

func TestListSessions_FilterByCwd(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test")

	// Add sessions with different cwds (in-memory only — no disk scan needed)
	ta.sessions.New(context.Background(), "/home/user/project-a", "openai", nil, nil, nil)
	ta.sessions.New(context.Background(), "/home/user/project-b", "openai", nil, nil, nil)
	ta.sessions.New(context.Background(), "/home/user/project-a", "anthropic", nil, nil, nil)

	// Filter by cwd — only checks in-memory sessions first, then disk.
	// Since we can't control disk sessions in unit tests, just verify in-memory filtering works.
	cwd := "/home/user/project-a"
	resp, err := ta.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &cwd})
	require.NoError(t, err)
	// Should have at least 2 from in-memory
	found := 0
	for _, s := range resp.Sessions {
		if s.Cwd == cwd {
			found++
		}
	}
	assert.GreaterOrEqual(t, found, 2)

	// Verify all returned sessions with project-a cwd match
	for _, s := range resp.Sessions {
		assert.Equal(t, cwd, s.Cwd)
	}
}

func TestConvertMCPServers_StdioOnly(t *testing.T) {
	acpServers := []acp.McpServer{
		{Stdio: &acp.McpServerStdio{
			Name:    "test-server",
			Command: "/usr/bin/mcp-server",
			Args:    []string{"--port", "8080"},
			Env:     []acp.EnvVariable{{Name: "TOKEN", Value: "abc123"}},
		}},
		{Stdio: nil}, // non-stdio server — should be skipped
	}

	result := convertMCPServers(acpServers, "client_wins", nil)
	require.Len(t, result, 1)
	assert.Equal(t, "test-server", result[0].Name)
	assert.Equal(t, "/usr/bin/mcp-server", result[0].Command)
	assert.Equal(t, []string{"--port", "8080"}, result[0].Args)
	assert.Equal(t, "abc123", result[0].Env["TOKEN"])
}

func TestConvertMCPServers_ConflictPolicy(t *testing.T) {
	existing := []config.MCPServerConfig{
		{Name: "shared-server"},
	}

	acpServers := []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "shared-server", Command: "/editor/version"}},
		{Stdio: &acp.McpServerStdio{Name: "new-server", Command: "/new/server"}},
	}

	// client_wins: both should pass through
	result := convertMCPServers(acpServers, "client_wins", existing)
	assert.Len(t, result, 2)

	// agent_wins: only new-server should pass
	result = convertMCPServers(acpServers, "agent_wins", existing)
	assert.Len(t, result, 1)
	assert.Equal(t, "new-server", result[0].Name)
}
