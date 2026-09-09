package mcp

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/monsterxx03/tachi/config"
)

// Per-turn user-scoped MCP token support.
//
// Channels that implement channel.MCPTokenUserChannel tag each
// agent turn with the conversation participant's identity via
// WithMCPTokenUser — the key of a per-user token file under ~/.tachi/mcp_tokens/.
// MCP HTTP requests then present that participant's own token (refreshed by
// the channel when the message landed) instead of always using the
// server-level token, falling back to it when the participant has none.

type mcpTokenUserCtxKey struct{}

// WithMCPTokenUser returns a context carrying the per-user MCP token file
// key for the current turn. The key is typically the message sender's
// platform identity (e.g. a domain account); empty keys are ignored by
// MCPTokenUserFromCtx.
func WithMCPTokenUser(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, mcpTokenUserCtxKey{}, key)
}

// MCPTokenUserFromCtx extracts the per-user MCP token file key tagged on the
// context by WithMCPTokenUser, or "" when the current turn has no
// participant-scoped token (non-MCPTokenUserChannel channels, bot senders,
// fixed-identity deployments).
func MCPTokenUserFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(mcpTokenUserCtxKey{}).(string)
	return v
}

// userScopedTokenStore wraps a FileTokenStore so that GetToken resolves the
// token for the current turn's participant first, then falls back to the
// server-level token file:
//
//  1. <mcp_tokens>/<MCPTokenUserFromCtx(ctx)>.json  — per-user token, when
//     the ctx carries a participant key AND the file exists
//  2. <mcp_tokens>/<storageKey>.json                — server-level token
//
// The per-user files are written by the channel (which refreshes them on every
// incoming message) using the same JSON layout as transport.Token, so they
// load with the regular token loader. A missing or unparseable per-user file
// silently falls back — the server token is the source of truth for
// connectivity.
//
// SaveToken and the DCR helpers delegate to the embedded FileTokenStore:
// OAuth authorization flows and refreshers run without a participant context
// and keep writing the server-level files exactly as before.
type userScopedTokenStore struct {
	*FileTokenStore
	storageKey string
}

// newUserScopedTokenStore creates the store for the given server storage key.
func newUserScopedTokenStore(storageKey string) (*userScopedTokenStore, error) {
	base, err := NewFileTokenStore(storageKey)
	if err != nil {
		return nil, err
	}
	return &userScopedTokenStore{FileTokenStore: base, storageKey: storageKey}, nil
}

// localhostStorageKeyMarker marks local development MCP servers. Locally run
// servers (e.g. http://localhost:xxxx) have no participant-scoped tokens — the
// server-level token file is always the right credential for them, so the
// per-user lookup is skipped entirely.
const localhostStorageKeyMarker = "localhost"

// GetToken implements transport.TokenStore with per-user → server fallback.
// Servers whose storage key contains "localhost" skip the per-user lookup and
// always use the server-level token file.
func (s *userScopedTokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !strings.Contains(s.storageKey, localhostStorageKeyMarker) {
		if userKey := MCPTokenUserFromCtx(ctx); userKey != "" {
			path := filepath.Join(config.MCPTokensDir(), userKey+".json")
			if tok, err := loadJSONFile[transport.Token](ctx, path, "user token file"); err == nil {
				return tok, nil
			}
			// Missing or corrupt per-user file — fall through to the
			// server-level token rather than failing the request.
		}
	}

	return s.FileTokenStore.GetToken(ctx)
}
