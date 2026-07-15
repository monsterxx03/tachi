package acp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// ── Initialize tests ────────────────────────────────────────────────────────

func TestInitialize(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test-version")

	resp, err := ta.Initialize(t.Context(), acp.InitializeRequest{
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

// ── findLatestSessionByCwd tests ────────────────────────────────────────────

func TestFindLatestSessionByCwd_NoSessions(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	result := findLatestSessionByCwd(sm, "/some/path")
	assert.Nil(t, result, "expected nil when no sessions exist")
}

func TestFindLatestSessionByCwd_FindsMatching(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	// Create a session with matching cwd
	sess, err := sm.New("openai", "/my/project")
	require.NoError(t, err)
	require.NotNil(t, sess)

	result := findLatestSessionByCwd(sm, "/my/project")
	require.NotNil(t, result)
	assert.Equal(t, sess.ID, result.ID)
}

func TestFindLatestSessionByCwd_NoMatch(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	_, err = sm.New("openai", "/project/a")
	require.NoError(t, err)

	// Different cwd — should not match
	result := findLatestSessionByCwd(sm, "/project/b")
	assert.Nil(t, result, "expected nil for non-matching cwd")
}

func TestFindLatestSessionByCwd_ReturnsLatest(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	// Create sessions with different cwds, last one matching
	_, err = sm.New("openai", "/project/other")
	require.NoError(t, err)

	sess2, err := sm.New("openai", "/project/target")
	require.NoError(t, err)

	_, err = sm.New("openai", "/project/another")
	require.NoError(t, err)

	// The second session matches /project/target.
	result := findLatestSessionByCwd(sm, "/project/target")
	require.NotNil(t, result)
	assert.Equal(t, sess2.ID, result.ID)
}

func TestFindLatestSessionByCwd_MultipleMatching_ReturnsNewest(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)

	// Create multiple sessions with same cwd
	_, err = sm.New("openai", "/project/shared")
	require.NoError(t, err)
	_, err = sm.New("openai", "/project/shared")
	require.NoError(t, err)
	sess3, err := sm.New("openai", "/project/shared")
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

	_, err := ta.LoadSession(t.Context(), acp.LoadSessionRequest{
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

	resp, err := ta.LoadSession(t.Context(), acp.LoadSessionRequest{
		Cwd: "/tmp/test-project",
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Modes)
	assert.Equal(t, acp.SessionModeId(agent.ModeAuto), resp.Modes.CurrentModeId)
	assert.Len(t, resp.Modes.AvailableModes, 3)
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
	sm, err := session.NewManager(nil)
	require.NoError(t, err)
	sess, err := sm.New("openai", "/existing/project")
	require.NoError(t, err)
	sessID := sess.ID
	sm.EndCurrent()

	// Now load it via ACP
	ta := NewTachiAgent(cfg, "test")
	resp, err := ta.LoadSession(t.Context(), acp.LoadSessionRequest{
		Cwd: "/existing/project",
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Modes)
	assert.Equal(t, acp.SessionModeId(agent.ModeAuto), resp.Modes.CurrentModeId)
	assert.Len(t, resp.Modes.AvailableModes, 3)

	// Verify the ACP session was created with the same disk session ID
	acpSess, ok := ta.sessions.Get(sessID)
	require.True(t, ok, "ACP session should exist with the disk session ID")
	assert.Equal(t, "/existing/project", acpSess.cwd)
}

func TestLoadSession_WithSessionId_NotFound_ReturnsError(t *testing.T) {
	// When LoadSession is called with an explicit sessionId that doesn't
	// exist on disk, it should return an error (not silently create a new session).
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

	ta := NewTachiAgent(cfg, "test")

	// Try to load a non-existent session by explicit ID
	_, err := ta.LoadSession(t.Context(), acp.LoadSessionRequest{
		SessionId:  "non-existent-session-id",
		Cwd:        "/tmp/test-project",
		McpServers: []acp.McpServer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found on disk")

	// Verify no ACP session was created for this non-existent ID
	_, ok := ta.sessions.Get("non-existent-session-id")
	assert.False(t, ok, "ACP session should NOT exist for a non-existent session ID")
}
