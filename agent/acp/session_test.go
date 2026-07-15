package acp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"

	"github.com/monsterxx03/tachi/config"
)

func TestACPSessionManager_Lifecycle(t *testing.T) {
	sm := NewACPSessionManager()

	// Create a session
	sess := sm.New(t.Context(), "/tmp", nil, nil, nil, nil)
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "/tmp", sess.cwd)
	assert.Empty(t, sess.ProviderType())

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
	sess1 := sm.New(t.Context(), "/tmp/a", nil, nil, nil, nil)
	sess2 := sm.New(t.Context(), "/tmp/b", nil, nil, nil, nil)
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
	err := ta.Cancel(t.Context(), acp.CancelNotification{
		SessionId: "nonexistent-id",
	})
	assert.NoError(t, err)
}

func TestCloseSession_NonexistentSession(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test")

	// Should not error on nonexistent session
	resp, err := ta.CloseSession(t.Context(), acp.CloseSessionRequest{
		SessionId: "nonexistent-id",
	})
	assert.NoError(t, err)
	assert.NotNil(t, resp)
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
		resp, err := ta.Authenticate(t.Context(), acp.AuthenticateRequest{})
		assert.NoError(t, err)
		assert.Empty(t, resp)
	})

	t.Run("SetSessionConfigOption", func(t *testing.T) {
		resp, err := ta.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{})
		assert.Error(t, err)
		assert.Empty(t, resp)
	})

	t.Run("SetSessionMode", func(t *testing.T) {
		resp, err := ta.SetSessionMode(t.Context(), acp.SetSessionModeRequest{})
		assert.Error(t, err) // no session exists
		assert.Empty(t, resp)
	})
}

func TestCloseAll(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "1.0")

	// Add sessions
	sess := ta.sessions.New(t.Context(), "/tmp", nil, nil, nil, nil)
	assert.Len(t, ta.sessions.List(), 1)

	ta.CloseAll()
	assert.Empty(t, ta.sessions.List())
	assert.Error(t, sess.ctx.Err()) // context should be cancelled
}

func TestACPSession_CloseWithMCPandSessionManager(t *testing.T) {
	sm := NewACPSessionManager()
	// No MCP manager, no session manager — just verify Close works
	sess := sm.New(t.Context(), "/tmp", nil, nil, nil, nil)
	assert.NotPanics(t, func() { sess.Close() })
	assert.Error(t, sess.ctx.Err())
}

func TestACPSession_setPromptCancel(t *testing.T) {
	sm := NewACPSessionManager()
	sess := sm.New(t.Context(), "/tmp", nil, nil, nil, nil)

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
