package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenStore_SaveAndGetToken(t *testing.T) {
	store := newTestTokenStore(t)

	token := &transport.Token{
		AccessToken: "test-access-token",
		TokenType:   "bearer",
	}
	err := store.SaveToken(context.Background(), token)
	require.NoError(t, err)

	got, err := store.GetToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, token.AccessToken, got.AccessToken)
	assert.Equal(t, token.TokenType, got.TokenType)
}

func TestTokenStore_GetToken_NotExist(t *testing.T) {
	store := newTestTokenStore(t)

	_, err := store.GetToken(context.Background())
	assert.ErrorIs(t, err, transport.ErrNoToken)
}

func TestTokenStore_GetToken_CorruptFile(t *testing.T) {
	store := newTestTokenStore(t)

	// Write corrupt data to the token path
	err := os.MkdirAll(filepath.Dir(store.tokenPath), 0700)
	require.NoError(t, err)
	err = os.WriteFile(store.tokenPath, []byte("not json{"), 0600)
	require.NoError(t, err)

	_, err = store.GetToken(context.Background())
	assert.ErrorIs(t, err, transport.ErrNoToken)
}

func TestTokenStore_SaveAndGetDCRInfo(t *testing.T) {
	store := newTestTokenStore(t)

	info := &DCRInfo{
		ClientID:              "dcr-client-id",
		ClientSecret:          "dcr-secret",
		AuthServerMetadataURL: "https://example.com/.well-known/oauth-authorization-server",
	}
	err := store.SaveDCRInfo(context.Background(), info)
	require.NoError(t, err)

	got, err := store.GetDCRInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, info.ClientID, got.ClientID)
	assert.Equal(t, info.ClientSecret, got.ClientSecret)
	assert.Equal(t, info.AuthServerMetadataURL, got.AuthServerMetadataURL)
}

func TestTokenStore_GetDCRInfo_NotExist(t *testing.T) {
	store := newTestTokenStore(t)

	_, err := store.GetDCRInfo(context.Background())
	assert.ErrorIs(t, err, transport.ErrNoToken)
}

func TestTokenStore_SaveAndGetPendingState(t *testing.T) {
	store := newTestTokenStore(t)

	state := &OAuthPendingState{
		State:        "csrf-state",
		CodeVerifier: "pkce-verifier",
	}
	err := store.SavePendingState(context.Background(), state)
	require.NoError(t, err)

	// Verify file exists on disk
	_, err = os.Stat(store.pendingPath)
	require.NoError(t, err, "pending file should exist after SavePendingState")

	got, err := store.GetPendingState(context.Background())
	require.NoError(t, err)
	assert.Equal(t, state.State, got.State)
	assert.Equal(t, state.CodeVerifier, got.CodeVerifier)

	// Verify file was consumed (deleted) after GetPendingState
	_, err = os.Stat(store.pendingPath)
	assert.True(t, os.IsNotExist(err), "pending file should be deleted after GetPendingState")
}

func TestTokenStore_GetPendingState_NotExist(t *testing.T) {
	store := newTestTokenStore(t)

	_, err := store.GetPendingState(context.Background())
	assert.ErrorIs(t, err, transport.ErrNoToken)
}

func TestTokenStore_GetPendingState_Corrupt(t *testing.T) {
	store := newTestTokenStore(t)

	err := os.MkdirAll(filepath.Dir(store.pendingPath), 0700)
	require.NoError(t, err)
	err = os.WriteFile(store.pendingPath, []byte("not json"), 0600)
	require.NoError(t, err)

	_, err = store.GetPendingState(context.Background())
	assert.ErrorIs(t, err, transport.ErrNoToken)
}

func TestTokenStore_ContextCancellation(t *testing.T) {
	store := newTestTokenStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	_, err := store.GetToken(ctx)
	assert.ErrorIs(t, err, context.Canceled)

	_, err = store.GetDCRInfo(ctx)
	assert.ErrorIs(t, err, context.Canceled)

	_, err = store.GetPendingState(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestTokenStore_TokenExpiry(t *testing.T) {
	store := newTestTokenStore(t)

	// Token without expiry
	token := &transport.Token{
		AccessToken: "no-expiry-token",
	}
	err := store.SaveToken(context.Background(), token)
	require.NoError(t, err)

	got, err := store.GetToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "no-expiry-token", got.AccessToken)
}

func TestTokenStore_MultipleServers(t *testing.T) {
	baseDir := t.TempDir()
	config.SetBaseDir(baseDir)
	t.Cleanup(func() { config.SetBaseDir("") })

	store1, err := NewFileTokenStore("server-a")
	require.NoError(t, err)
	store2, err := NewFileTokenStore("server-b")
	require.NoError(t, err)

	err = store1.SaveToken(context.Background(), &transport.Token{AccessToken: "token-a"})
	require.NoError(t, err)
	err = store2.SaveToken(context.Background(), &transport.Token{AccessToken: "token-b"})
	require.NoError(t, err)

	got1, _ := store1.GetToken(context.Background())
	assert.Equal(t, "token-a", got1.AccessToken)
	got2, _ := store2.GetToken(context.Background())
	assert.Equal(t, "token-b", got2.AccessToken)
}

func TestTokenStore_PathStructure(t *testing.T) {
	baseDir := t.TempDir()
	config.SetBaseDir(baseDir)
	t.Cleanup(func() { config.SetBaseDir("") })

	store, err := NewFileTokenStore("my-server")
	require.NoError(t, err)

	expectedDir := filepath.Join(baseDir, "mcp_tokens")
	assert.Equal(t, filepath.Join(expectedDir, "my-server.json"), store.tokenPath)
	assert.Equal(t, filepath.Join(expectedDir, "my-server_dcr.json"), store.dcrPath)
	assert.Equal(t, filepath.Join(expectedDir, "my-server_pending.json"), store.pendingPath)
}

// newTestTokenStore creates a FileTokenStore backed by a temp directory.
func newTestTokenStore(t *testing.T) *FileTokenStore {
	t.Helper()
	baseDir := t.TempDir()
	config.SetBaseDir(baseDir)
	t.Cleanup(func() { config.SetBaseDir("") })

	store, err := NewFileTokenStore("test-server")
	require.NoError(t, err)
	return store
}
