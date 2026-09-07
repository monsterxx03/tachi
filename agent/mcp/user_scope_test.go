package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUserScopeEnv creates a userScopedTokenStore for storage key "server-key"
// backed by a temp base dir, mirroring the isolation used by token_store_test.
func newUserScopeEnv(t *testing.T) (*userScopedTokenStore, string) {
	t.Helper()
	baseDir := t.TempDir()
	config.SetBaseDir(baseDir)
	t.Cleanup(func() { config.SetBaseDir("") })

	store, err := newUserScopedTokenStore("server-key")
	require.NoError(t, err)
	return store, filepath.Join(baseDir, "mcp_tokens")
}

// writeUserTokenFile writes a per-user token file exactly as a channel would
// (matching transport.Token's JSON layout).
func writeUserTokenFile(t *testing.T, tokensDir, userKey, accessToken string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(tokensDir, 0o700))
	tok := &transport.Token{AccessToken: accessToken, TokenType: "Bearer", ExpiresIn: 3600}
	require.NoError(t, fileutil.AtomicWriteJSONPrivate(filepath.Join(tokensDir, userKey+".json"), tok))
}

func TestUserScopedTokenStore_PrefersUserToken(t *testing.T) {
	store, tokensDir := newUserScopeEnv(t)

	// Server-level token (the legacy default).
	require.NoError(t, store.SaveToken(t.Context(), &transport.Token{AccessToken: "server-token", TokenType: "Bearer"}))
	// Per-user token, as refreshed by the channel when the message landed.
	writeUserTokenFile(t, tokensDir, "san.zhang", "user-token")

	ctx := WithMCPTokenUser(t.Context(), "san.zhang")
	got, err := store.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "user-token", got.AccessToken)
}

func TestUserScopedTokenStore_FallsBackWhenUserFileMissing(t *testing.T) {
	store, _ := newUserScopeEnv(t)

	require.NoError(t, store.SaveToken(t.Context(), &transport.Token{AccessToken: "server-token", TokenType: "Bearer"}))

	// ctx carries a user key but no per-user file exists (e.g. the channel
	// could not refresh it) → server-level token is used.
	ctx := WithMCPTokenUser(t.Context(), "san.zhang")
	got, err := store.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "server-token", got.AccessToken)
}

func TestUserScopedTokenStore_FallsBackOnCorruptUserFile(t *testing.T) {
	store, tokensDir := newUserScopeEnv(t)

	require.NoError(t, store.SaveToken(t.Context(), &transport.Token{AccessToken: "server-token", TokenType: "Bearer"}))
	require.NoError(t, os.MkdirAll(tokensDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tokensDir, "san.zhang.json"), []byte("{not json"), 0o600))

	ctx := WithMCPTokenUser(t.Context(), "san.zhang")
	got, err := store.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "server-token", got.AccessToken)
}

func TestUserScopedTokenStore_NoUserKeyUsesServerToken(t *testing.T) {
	store, _ := newUserScopeEnv(t)

	require.NoError(t, store.SaveToken(t.Context(), &transport.Token{AccessToken: "server-token", TokenType: "Bearer"}))

	// Non-MCPTokenUserChannel turns never tag the ctx — legacy behaviour.
	got, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "server-token", got.AccessToken)
}

func TestUserScopedTokenStore_NoTokenAnywhere(t *testing.T) {
	store, _ := newUserScopeEnv(t)

	_, err := store.GetToken(WithMCPTokenUser(t.Context(), "san.zhang"))
	assert.ErrorIs(t, err, transport.ErrNoToken)
}

func TestMCPTokenUserCtxHelpers(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", MCPTokenUserFromCtx(ctx))

	ctx = WithMCPTokenUser(ctx, "san.zhang")
	assert.Equal(t, "san.zhang", MCPTokenUserFromCtx(ctx))

	var nilCtx context.Context // typed-nil interface exercises the nil-context guard
	assert.Equal(t, "", MCPTokenUserFromCtx(nilCtx))
}
