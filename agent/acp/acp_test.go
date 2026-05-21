package acp

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
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
	assert.True(t, resp.AgentCapabilities.LoadSession)
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
	sess := sm.New(context.Background(), "/tmp", "openai", nil, nil, nil, nil)
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
	sess1 := sm.New(context.Background(), "/tmp/a", "openai", nil, nil, nil, nil)
	sess2 := sm.New(context.Background(), "/tmp/b", "anthropic", nil, nil, nil, nil)
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
	assert.Equal(t, acp.ToolKindSearch, mapToolKind("Glob"))
	assert.Equal(t, acp.ToolKindSearch, mapToolKind("Grep"))
	assert.Equal(t, acp.ToolKindFetch, mapToolKind("WebSearch"))
	assert.Equal(t, acp.ToolKindFetch, mapToolKind("WebFetch"))
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
		ta.sessions.New(context.Background(), "/home/user/project-a", "openai", nil, nil, nil, nil)
	ta.sessions.New(context.Background(), "/home/user/project-b", "openai", nil, nil, nil, nil)
	ta.sessions.New(context.Background(), "/home/user/project-a", "anthropic", nil, nil, nil, nil)

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

func TestSetConnection(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "1.0")
	// Just ensure it doesn't panic — conn is nil-safe in tests
	ta.SetConnection(nil)
}

func TestStubMethods_ReturnEmpty(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "1.0")

	t.Run("Authenticate", func(t *testing.T) {
		resp, err := ta.Authenticate(context.Background(), acp.AuthenticateRequest{})
		assert.NoError(t, err)
		assert.Empty(t, resp)
	})

	t.Run("SetSessionConfigOption", func(t *testing.T) {
		resp, err := ta.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{})
		assert.NoError(t, err)
		assert.Empty(t, resp)
	})

	t.Run("SetSessionMode", func(t *testing.T) {
		resp, err := ta.SetSessionMode(context.Background(), acp.SetSessionModeRequest{})
		assert.NoError(t, err)
		assert.Empty(t, resp)
	})
}

func TestCloseAll(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "1.0")

	// Add sessions
	sess := ta.sessions.New(context.Background(), "/tmp", "test", nil, nil, nil, nil)
	assert.Len(t, ta.sessions.List(), 1)

	ta.CloseAll()
	assert.Empty(t, ta.sessions.List())
	assert.Error(t, sess.ctx.Err()) // context should be cancelled
}

func TestACPSession_CloseWithMCPandSessionManager(t *testing.T) {
	sm := NewACPSessionManager()
	// No MCP manager, no session manager — just verify Close works
	sess := sm.New(context.Background(), "/tmp", "test", nil, nil, nil, nil)
	assert.NotPanics(t, func() { sess.Close() })
	assert.Error(t, sess.ctx.Err())
}

func TestACPSession_setPromptCancel(t *testing.T) {
	sm := NewACPSessionManager()
	sess := sm.New(context.Background(), "/tmp", "test", nil, nil, nil, nil)

	// setPromptCancel stores and clears the cancel func
	cancelCalled := false
	cancelFn := func() { cancelCalled = true }
	sess.setPromptCancel(cancelFn)
	assert.NotNil(t, sess.promptCancel)

	// Invoke it
	sess.promptCancel()
	assert.True(t, cancelCalled)

	// Clear it
	sess.setPromptCancel(nil)
	assert.Nil(t, sess.promptCancel)
}

func TestBuildSystemPromptForCwd(t *testing.T) {
	t.Run("contains basic info", func(t *testing.T) {
		prompt := buildSystemPromptForCwd("中文", "/home/user/project")
		assert.Contains(t, prompt, "Reply in 中文")
		assert.Contains(t, prompt, "- Working directory: /home/user/project")
		assert.Contains(t, prompt, "Tachi")
	})

	t.Run("language fallback", func(t *testing.T) {
		prompt := buildSystemPromptForCwd("", "/tmp")
		assert.Contains(t, prompt, "Reply in ")
	})

	t.Run("git detection", func(t *testing.T) {
		// Use a temp dir that's not a git repo
		prompt := buildSystemPromptForCwd("en", "/nonexistent-dir")
		assert.Contains(t, prompt, "Git repository: no")
	})
}

func TestBuildSystemPromptForCwd_InGitRepo(t *testing.T) {
	// Create temp dir and init git repo
	tmpDir := t.TempDir()
	err := exec.Command("git", "-C", tmpDir, "init").Run()
	require.NoError(t, err)

	prompt := buildSystemPromptForCwd("en", tmpDir)
	assert.Contains(t, prompt, "Git repository: yes")
	assert.Contains(t, prompt, "Working directory: "+tmpDir)
}

// Build a minimal mock ACP agent for testing buildPermissionHandler.
type mockACPAgent struct {
	acp.Agent
	initializeCalled bool
}

func (m *mockACPAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	m.initializeCalled = true
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func TestBuildPermissionHandler_AllowOnce(t *testing.T) {
	// Two independent pipes: agent→client, client→agent
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	// Build the handler
	aiAgent := agent.NewAIAgent(nil, "test-model", 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	// Goroutine simulates the ACP client reading the JSON-RPC request and sending a response
	go func() {
		var reqMap map[string]interface{}
		decoder := json.NewDecoder(agentToClientR)
		err := decoder.Decode(&reqMap)
		require.NoError(t, err)

		// Correct format: outcome discriminator "selected" with optionId in camelCase
		response := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": "allow",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	approved, err := handler(context.Background(), "EditFile", "tool-1", "diff content", "args here")
	assert.NoError(t, err)
	assert.True(t, approved)
}

func TestBuildPermissionHandler_Reject(t *testing.T) {
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	aiAgent := agent.NewAIAgent(nil, "test-model", 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	go func() {
		var reqMap map[string]interface{}
		json.NewDecoder(agentToClientR).Decode(&reqMap)
		response := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": "reject",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	approved, err := handler(context.Background(), "EditFile", "tool-1", "diff", "args")
	assert.NoError(t, err)
	assert.False(t, approved)
}

func TestBuildPermissionHandler_AllowAll(t *testing.T) {
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	aiAgent := agent.NewAIAgent(nil, "test-model", 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	go func() {
		var reqMap map[string]interface{}
		json.NewDecoder(agentToClientR).Decode(&reqMap)
		response := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": "allow_all",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	approved, err := handler(context.Background(), "EditFile", "tool-2", "diff", "args")
	assert.NoError(t, err)
	assert.True(t, approved)
}

func TestBuildPermissionHandler_Cancelled(t *testing.T) {
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	aiAgent := agent.NewAIAgent(nil, "test-model", 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	go func() {
		var reqMap map[string]interface{}
		json.NewDecoder(agentToClientR).Decode(&reqMap)
		response := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome": "cancelled",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	approved, err := handler(context.Background(), "EditFile", "tool-3", "diff", "args")
	assert.NoError(t, err)
	assert.False(t, approved)
}

// ── buildACPAvailableCommands tests ──────────────────────────────────────────

// expectedACPCmds lists all static commands we expect in buildACPAvailableCommands.
var expectedACPCmds = []struct {
	name      string
	hasInput  bool
}{
	{name: "commit"},
	{name: "init"},
	{name: "compact"},
	{name: "usage"},
	{name: "mcp", hasInput: true},
	{name: "skill", hasInput: true},
	{name: "transcript"},
}

func TestBuildACPAvailableCommands_StaticCommands(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, "test-model", 0)
	cmds := buildACPAvailableCommands(aiAgent)

	// Collect all returned command names for duplicate checking
	names := make(map[string]int)
	for _, c := range cmds {
		names[c.Name]++
	}

	// Check each expected command
	for _, ec := range expectedACPCmds {
		t.Run(ec.name, func(t *testing.T) {
			count, found := names[ec.name]
			assert.True(t, found, "command %q should be present in available commands", ec.name)
			assert.Equal(t, 1, count, "command %q should appear exactly once", ec.name)

			// Find the command and check fields
			for _, c := range cmds {
				if c.Name == ec.name {
					assert.NotEmpty(t, c.Description, "command %q should have a non-empty description", ec.name)
					if ec.hasInput {
						assert.NotNil(t, c.Input, "command %q should have Input set", ec.name)
						if c.Input != nil {
							assert.NotNil(t, c.Input.Unstructured, "command %q Input should have Unstructured", ec.name)
							assert.NotEmpty(t, c.Input.Unstructured.Hint, "command %q Input should have a non-empty Hint", ec.name)
						}
					} else {
						assert.Nil(t, c.Input, "command %q should NOT have Input set", ec.name)
					}
				}
			}
		})
	}
}

func TestBuildACPAvailableCommands_Count(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, "test-model", 0)
	cmds := buildACPAvailableCommands(aiAgent)

	// Should have exactly len(expectedACPCmds) commands (no skills configured)
	assert.Len(t, cmds, len(expectedACPCmds))
}

func TestBuildACPAvailableCommands_NoDuplicates(t *testing.T) {
	aiAgent := agent.NewAIAgent(nil, "test-model", 0)
	cmds := buildACPAvailableCommands(aiAgent)

	names := make(map[string]int)
	for _, c := range cmds {
		names[c.Name]++
	}

	for name, count := range names {
		assert.Equal(t, 1, count, "command %q appears %d times — no duplicates expected", name, count)
	}
}

func TestBuildACPAvailableCommands_NilAgent(t *testing.T) {
	// Passing nil should not panic; returns empty static commands
	cmds := buildACPAvailableCommands(nil)
	assert.Len(t, cmds, len(expectedACPCmds),
		"nil agent should still return static commands (no skills)")
}


// ── findLatestSessionByCwd tests ────────────────────────────────────────────

func TestFindLatestSessionByCwd_NoSessions(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store)

	result := findLatestSessionByCwd(sm, "/some/path")
	assert.Nil(t, result, "expected nil when no sessions exist")
}

func TestFindLatestSessionByCwd_FindsMatching(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store)

	// Create a session with matching cwd
	sess, err := sm.New("openai", "gpt-4", "/my/project")
	require.NoError(t, err)
	require.NotNil(t, sess)

	result := findLatestSessionByCwd(sm, "/my/project")
	require.NotNil(t, result)
	assert.Equal(t, sess.ID, result.ID)
}

func TestFindLatestSessionByCwd_NoMatch(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store)

	_, err = sm.New("openai", "gpt-4", "/project/a")
	require.NoError(t, err)

	// Different cwd — should not match
	result := findLatestSessionByCwd(sm, "/project/b")
	assert.Nil(t, result, "expected nil for non-matching cwd")
}

func TestFindLatestSessionByCwd_ReturnsLatest(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store)

	// Create sessions with different cwds, last one matching
	_, err = sm.New("openai", "gpt-4", "/project/other")
	require.NoError(t, err)

	sess2, err := sm.New("openai", "gpt-4", "/project/target")
	require.NoError(t, err)

	_, err = sm.New("openai", "gpt-4", "/project/another")
	require.NoError(t, err)

	// The second session matches /project/target.
	result := findLatestSessionByCwd(sm, "/project/target")
	require.NotNil(t, result)
	assert.Equal(t, sess2.ID, result.ID)
}

func TestFindLatestSessionByCwd_MultipleMatching_ReturnsNewest(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store)

	// Create multiple sessions with same cwd
	_, err = sm.New("openai", "gpt-4", "/project/shared")
	require.NoError(t, err)
	_, err = sm.New("openai", "gpt-4", "/project/shared")
	require.NoError(t, err)
	sess3, err := sm.New("openai", "gpt-4", "/project/shared")
	require.NoError(t, err)

	// Should return the most recent (sess3)
	result := findLatestSessionByCwd(sm, "/project/shared")
	require.NotNil(t, result)
	assert.Equal(t, sess3.ID, result.ID,
		"expected the newest session with matching cwd")
}

// ── LoadSession tests ───────────────────────────────────────────────────────

func TestLoadSession_NoProvider(t *testing.T) {
	// With an empty config (no providers), LoadSession should return an error.
	origBase := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(origBase) })

	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test")

	_, err := ta.LoadSession(context.Background(), acp.LoadSessionRequest{
		Cwd: "/tmp/test-project",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no provider configured")
}

func TestLoadSession_CreatesNewSession(t *testing.T) {
	origBase := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(origBase) })

	cfg := config.DefaultConfig()
	cfg.Providers = []config.ProviderConfig{
		{
			Name:    "test-provider",
			Type:    "openai",
			Model:   "gpt-4o-mini",
			APIKey:  "sk-test-key-12345",
			BaseURL: "https://api.openai.com/v1",
		},
	}
	cfg.Provider = "test-provider"

	ta := NewTachiAgent(cfg, "test")

	resp, err := ta.LoadSession(context.Background(), acp.LoadSessionRequest{
		Cwd: "/tmp/test-project",
	})
	require.NoError(t, err)
	assert.Empty(t, resp) // LoadSessionResponse is empty on success
}

func TestLoadSession_LoadsExistingSessionByCwd(t *testing.T) {
	origBase := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(origBase) })

	cfg := config.DefaultConfig()
	cfg.Providers = []config.ProviderConfig{
		{
			Name:   "test-provider",
			Type:   "openai",
			Model:  "gpt-4o-mini",
			APIKey: "sk-test-key-12345",
		},
	}
	cfg.Provider = "test-provider"

	// Create a session on disk with matching cwd
	sm, err := session.NewManager()
	require.NoError(t, err)
	sess, err := sm.New("openai", "gpt-4o-mini", "/existing/project")
	require.NoError(t, err)
	sessID := sess.ID
	sm.EndCurrent()

	// Now load it via ACP
	ta := NewTachiAgent(cfg, "test")
	resp, err := ta.LoadSession(context.Background(), acp.LoadSessionRequest{
		Cwd: "/existing/project",
	})
	require.NoError(t, err)
	assert.Empty(t, resp)

	// Verify the ACP session was created with the same disk session ID
	acpSess, ok := ta.sessions.Get(sessID)
	require.True(t, ok, "ACP session should exist with the disk session ID")
	assert.Equal(t, "/existing/project", acpSess.cwd)
}
