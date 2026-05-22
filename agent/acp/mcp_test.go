package acp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
)

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

func TestConvertMCPServers_NilStdio(t *testing.T) {
	acpServers := []acp.McpServer{
		{Stdio: nil}, // non-stdio — should be skipped
		{Stdio: &acp.McpServerStdio{Name: "valid", Command: "/bin/valid"}},
	}
	result := convertMCPServers(acpServers, "client_wins", nil)
	require.Len(t, result, 1)
	assert.Equal(t, "valid", result[0].Name)
}

func TestConvertMCPServers_EmptyNameUsesCommand(t *testing.T) {
	acpServers := []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "", Command: "/bin/mcp-server"}},
	}
	result := convertMCPServers(acpServers, "client_wins", nil)
	require.Len(t, result, 1)
	assert.Equal(t, "/bin/mcp-server", result[0].Name)
}

func TestConvertMCPServers_EnvConversion(t *testing.T) {
	acpServers := []acp.McpServer{
		{Stdio: &acp.McpServerStdio{
			Name:    "env-server",
			Command: "/bin/mcp",
			Env: []acp.EnvVariable{
				{Name: "KEY", Value: "val1"},
				{Name: "TOKEN", Value: "secret"},
			},
		}},
	}
	result := convertMCPServers(acpServers, "client_wins", nil)
	require.Len(t, result, 1)
	assert.Equal(t, "val1", result[0].Env["KEY"])
	assert.Equal(t, "secret", result[0].Env["TOKEN"])
}
