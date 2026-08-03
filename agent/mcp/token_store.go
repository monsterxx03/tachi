package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// DCRInfo holds a dynamically registered client's credentials,
// persisted alongside OAuth tokens so client_id survives restarts.
type DCRInfo struct {
	ClientID              string `json:"client_id"`
	ClientSecret          string `json:"client_secret,omitempty"`
	AuthServerMetadataURL string `json:"auth_server_metadata_url,omitempty"`
}

// OAuthPendingState holds the ephemeral OAuth2 authorization state and PKCE
// verifier for a manual flow that spans two CLI invocations (startManualFlow
// → user pastes URL → CompleteManualAuth).
type OAuthPendingState struct {
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
}

// dcrTokenPath returns the path to a server's DCR info file.
func dcrTokenPath(storageKey string) (string, error) {
	dir := config.MCPTokensDir()
	return filepath.Join(dir, storageKey+"_dcr.json"), nil
}

// mcpTokenPath returns the path to a specific server's token file.
func mcpTokenPath(storageKey string) (string, error) {
	dir := config.MCPTokensDir()
	return filepath.Join(dir, storageKey+".json"), nil
}

// FileTokenStore persists OAuth tokens to disk under the MCP tokens directory.
// Each server gets its own JSON file for tokens and, when DCR is used, a
// separate _dcr.json file for the dynamically registered client credentials.
type FileTokenStore struct {
	storageKey  string
	tokenPath   string
	dcrPath     string
	pendingPath string
}

// NewFileTokenStore creates a FileTokenStore for the given storage key.
// The files are created lazily on first save; the directory is created eagerly.
func NewFileTokenStore(storageKey string) (*FileTokenStore, error) {
	tokenPath, err := mcpTokenPath(storageKey)
	if err != nil {
		return nil, err
	}
	dcrPath, err := dcrTokenPath(storageKey)
	if err != nil {
		return nil, err
	}
	pendingPath, err := mcpPendingPath(storageKey)
	if err != nil {
		return nil, err
	}
	return &FileTokenStore{
		storageKey:  storageKey,
		tokenPath:   tokenPath,
		dcrPath:     dcrPath,
		pendingPath: pendingPath,
	}, nil
}

// loadJSONFile reads path and unmarshals into a new T. A missing file yields
// transport.ErrNoToken; a corrupt file is logged and also yields ErrNoToken
// (the user will re-authenticate). Other read errors are logged and treated
// the same way, so any failure to load persisted state falls back to the
// auth flow rather than surfacing a transient I/O error.
func loadJSONFile[T any](ctx context.Context, path, what string) (*T, error) {
	var v T
	if err := fileutil.ReadJSON(path, &v); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, transport.ErrNoToken
		}
		logger.FromContext(ctx).Error(ctx, "MCP: failed to load "+what, err, "path", path)
		return nil, transport.ErrNoToken
	}
	return &v, nil
}

// GetToken loads the token from disk. Returns transport.ErrNoToken if
// no token file exists or if parsing fails.
func (s *FileTokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return loadJSONFile[transport.Token](ctx, s.tokenPath, "token file")
}

// SaveToken writes the token to disk. Creates the parent directory if needed.
func (s *FileTokenStore) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := fileutil.AtomicWriteJSONPrivate(s.tokenPath, token); err != nil {
		return fmt.Errorf("write token file %q: %w", s.tokenPath, err)
	}

	logger.FromContext(ctx).Info(ctx, "MCP: saved token for server", "server", s.storageKey, "path", s.tokenPath)
	return nil
}

// GetDCRInfo loads the DCR client credentials from disk.
// Returns transport.ErrNoToken if no DCR file exists.
func (s *FileTokenStore) GetDCRInfo(ctx context.Context) (*DCRInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return loadJSONFile[DCRInfo](ctx, s.dcrPath, "DCR file")
}

// SaveDCRInfo persists DCR client credentials to disk.
func (s *FileTokenStore) SaveDCRInfo(ctx context.Context, info *DCRInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := fileutil.AtomicWriteJSONPrivate(s.dcrPath, info); err != nil {
		return fmt.Errorf("write DCR file %q: %w", s.dcrPath, err)
	}

	logger.FromContext(ctx).Info(ctx, "MCP: saved DCR info", "server", s.storageKey)
	return nil
}

// mcpPendingPath returns the path to a server's pending OAuth state file.
func mcpPendingPath(storageKey string) (string, error) {
	dir := config.MCPTokensDir()
	return filepath.Join(dir, storageKey+"_pending.json"), nil
}

// SavePendingState persists the OAuth2 authorization state and PKCE verifier
// for a manual flow so that CompleteManualAuth can pick them up later.
func (s *FileTokenStore) SavePendingState(ctx context.Context, state *OAuthPendingState) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := fileutil.AtomicWriteJSONPrivate(s.pendingPath, state); err != nil {
		return fmt.Errorf("write pending state %q: %w", s.pendingPath, err)
	}

	logger.FromContext(ctx).Info(ctx, "MCP: saved pending OAuth state", "server", s.storageKey)
	return nil
}

// GetPendingState loads the pending OAuth2 authorization state from disk,
// then deletes the file so it can't be replayed.
// Returns transport.ErrNoToken if no pending state exists.
func (s *FileTokenStore) GetPendingState(ctx context.Context) (*OAuthPendingState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	state, err := loadJSONFile[OAuthPendingState](ctx, s.pendingPath, "pending state")
	if err != nil {
		return nil, err
	}

	// Consume the pending state — don't allow replay
	if err := fileutil.RemoveIgnoreNotExist(s.pendingPath); err != nil {
		logger.FromContext(ctx).Error(ctx, "MCP: failed to remove pending state", err, "path", s.pendingPath)
	}
	logger.FromContext(ctx).Info(ctx, "MCP: loaded and consumed pending OAuth state", "server", s.storageKey)
	return state, nil
}
