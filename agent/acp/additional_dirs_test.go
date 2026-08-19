package acp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
)

// testACPConfig returns a config with a resolvable provider and an isolated
// base dir, the minimal setup NewSession/LoadSession need to build an agent.
func testACPConfig(t *testing.T) *config.Config {
	t.Helper()
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
	return cfg
}

func TestInitialize_AdvertisesAdditionalDirectories(t *testing.T) {
	ta := NewTachiAgent(config.DefaultConfig(), "test")
	resp, err := ta.Initialize(t.Context(), acp.InitializeRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.AdditionalDirectories,
		"sessionCapabilities.additionalDirectories must be advertised")
}

func TestValidateAdditionalDirectories(t *testing.T) {
	t.Run("nil and empty yield nil", func(t *testing.T) {
		got, err := validateAdditionalDirectories("/cwd", nil)
		require.NoError(t, err)
		assert.Nil(t, got)

		got, err = validateAdditionalDirectories("/cwd", []string{})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("absolute paths pass in order", func(t *testing.T) {
		got, err := validateAdditionalDirectories("/cwd", []string{"/a", "/b"})
		require.NoError(t, err)
		assert.Equal(t, []string{"/a", "/b"}, got)
	})

	t.Run("empty entry rejected with invalid_params", func(t *testing.T) {
		_, err := validateAdditionalDirectories("/cwd", []string{"/a", ""})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid_params")
	})

	t.Run("relative path rejected with invalid_params", func(t *testing.T) {
		_, err := validateAdditionalDirectories("/cwd", []string{"relative/path"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid_params")
	})

	t.Run("duplicates and cwd-identical entries dropped preserving order", func(t *testing.T) {
		got, err := validateAdditionalDirectories("/cwd", []string{"/cwd", "/a", "/b", "/a", "/b", "/cwd"})
		require.NoError(t, err)
		assert.Equal(t, []string{"/a", "/b"}, got)
	})
}

func TestNewSession_WithAdditionalDirectories(t *testing.T) {
	ta := NewTachiAgent(testACPConfig(t), "test")

	resp, err := ta.NewSession(t.Context(), acp.NewSessionRequest{
		Cwd:                   "/project",
		AdditionalDirectories: []string{"/shared-lib", "/docs"},
		McpServers:            []acp.McpServer{},
	})
	require.NoError(t, err)

	sess, ok := ta.sessions.Get(string(resp.SessionId))
	require.True(t, ok)
	assert.Equal(t, "/project", sess.cwd)
	assert.Equal(t, []string{"/shared-lib", "/docs"}, sess.additionalDirs)

	// session/list reports the complete ordered root list.
	listResp, err := ta.ListSessions(t.Context(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, listResp.Sessions, 1)
	assert.Equal(t, []string{"/shared-lib", "/docs"}, listResp.Sessions[0].AdditionalDirectories)
}

func TestNewSession_RejectsInvalidAdditionalDirectories(t *testing.T) {
	ta := NewTachiAgent(testACPConfig(t), "test")

	_, err := ta.NewSession(t.Context(), acp.NewSessionRequest{
		Cwd:                   "/project",
		AdditionalDirectories: []string{"relative/path"},
		McpServers:            []acp.McpServer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_params")
	assert.Empty(t, ta.sessions.List(), "no session must be created on invalid roots")
}

func TestLoadSession_WithAdditionalDirectories(t *testing.T) {
	ta := NewTachiAgent(testACPConfig(t), "test")

	// No existing disk session for /project → LoadSession creates a fresh
	// one, carrying the requested roots.
	resp, err := ta.LoadSession(t.Context(), acp.LoadSessionRequest{
		Cwd:                   "/project",
		AdditionalDirectories: []string{"/shared-lib"},
		McpServers:            []acp.McpServer{},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Modes)

	sessions := ta.sessions.List()
	require.Len(t, sessions, 1)
	assert.Equal(t, []string{"/shared-lib"}, sessions[0].additionalDirs)
}

func TestResumeSession_InMemoryAppliesRequestedRoots(t *testing.T) {
	ta := NewTachiAgent(testACPConfig(t), "test")

	resp, err := ta.NewSession(t.Context(), acp.NewSessionRequest{
		Cwd:                   "/project",
		AdditionalDirectories: []string{"/shared-lib"},
		McpServers:            []acp.McpServer{},
	})
	require.NoError(t, err)
	sid := resp.SessionId

	// Resume with a different list — it becomes the complete resulting list.
	_, err = ta.ResumeSession(t.Context(), acp.ResumeSessionRequest{
		SessionId:             sid,
		Cwd:                   "/project",
		AdditionalDirectories: []string{"/new-root"},
		McpServers:            []acp.McpServer{},
	})
	require.NoError(t, err)
	sess, ok := ta.sessions.Get(string(sid))
	require.True(t, ok)
	assert.Equal(t, []string{"/new-root"}, sess.additionalDirs)

	// Omitting the field clears the list (explicit-list only, per spec).
	_, err = ta.ResumeSession(t.Context(), acp.ResumeSessionRequest{
		SessionId:  sid,
		Cwd:        "/project",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	assert.Nil(t, sess.additionalDirs, "omitted additionalDirectories must activate none")
}

func TestBuildSystemPromptWithRoots(t *testing.T) {
	prompt := agent.BuildSystemPromptWithRoots("en", "/project", []string{"/shared-lib", "/docs"}, "sess-1", "")
	assert.Contains(t, prompt, "- Working directory: /project")
	assert.Contains(t, prompt, "- Additional workspace roots: /shared-lib, /docs")

	// Plain BuildSystemPrompt must not emit the roots section.
	plain := agent.BuildSystemPrompt("en", "/project", "sess-1", "")
	assert.NotContains(t, plain, "Additional workspace roots")
}

// TestBuildSystemPromptForCwd_WithRoots pins the ACP wiring: the session's
// additional roots flow into the model's system prompt.
func TestBuildSystemPromptForCwd_WithRoots(t *testing.T) {
	cfg := &config.Config{Language: "en"}
	prompt := buildSystemPromptForCwd(cfg, "/project", []string{"/shared-lib"}, agent.ModeAuto, "sess-1")
	assert.Contains(t, prompt, "- Additional workspace roots: /shared-lib")
}
