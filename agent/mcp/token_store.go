package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

const mcpTokensDirName = "mcp_tokens"

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
func dcrTokenPath(serverName string) (string, error) {
	dir, err := mcpTokensDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, serverName+"_dcr.json"), nil
}

// mcpTokensDir returns the path to the MCP tokens storage directory.
func mcpTokensDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tachi", mcpTokensDirName), nil
}

// mcpTokenPath returns the path to a specific server's token file.
func mcpTokenPath(serverName string) (string, error) {
	dir, err := mcpTokensDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, serverName+".json"), nil
}

// FileTokenStore persists OAuth tokens to disk under ~/.tachi/mcp_tokens/.
// Each server gets its own JSON file for tokens and, when DCR is used, a
// separate _dcr.json file for the dynamically registered client credentials.
type FileTokenStore struct {
	serverName   string
	tokenPath    string
	dcrPath      string
	pendingPath  string
}

// NewFileTokenStore creates a FileTokenStore for the given server.
// The files are created lazily on first save; the directory is created eagerly.
func NewFileTokenStore(serverName string) (*FileTokenStore, error) {
	tokenPath, err := mcpTokenPath(serverName)
	if err != nil {
		return nil, err
	}
	dcrPath, err := dcrTokenPath(serverName)
	if err != nil {
		return nil, err
	}
	pendingPath, err := mcpPendingPath(serverName)
	if err != nil {
		return nil, err
	}
	return &FileTokenStore{
		serverName:  serverName,
		tokenPath:   tokenPath,
		dcrPath:     dcrPath,
		pendingPath: pendingPath,
	}, nil
}

// GetToken loads the token from disk. Returns transport.ErrNoToken if
// no token file exists or if parsing fails.
func (s *FileTokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.tokenPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, transport.ErrNoToken
		}
		return nil, fmt.Errorf("read token file %q: %w", s.tokenPath, err)
	}

	var token transport.Token
	if err := json.Unmarshal(data, &token); err != nil {
		// Corrupt file — treat as no token, the user will re-authenticate
		debuglog.Log(ctx, "MCP: failed to parse token file %q: %v", s.tokenPath, err)
		return nil, transport.ErrNoToken
	}

	return &token, nil
}

// SaveToken writes the token to disk. Creates the parent directory if needed.
func (s *FileTokenStore) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dir := filepath.Dir(s.tokenPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create token dir %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}

	if err := os.WriteFile(s.tokenPath, data, 0600); err != nil {
		return fmt.Errorf("write token file %q: %w", s.tokenPath, err)
	}

	debuglog.Log(ctx, "MCP: saved token for server %q to %s", s.serverName, s.tokenPath)
	return nil
}

// GetDCRInfo loads the DCR client credentials from disk.
// Returns transport.ErrNoToken if no DCR file exists.
func (s *FileTokenStore) GetDCRInfo(ctx context.Context) (*DCRInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.dcrPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, transport.ErrNoToken
		}
		return nil, fmt.Errorf("read DCR file %q: %w", s.dcrPath, err)
	}

	var info DCRInfo
	if err := json.Unmarshal(data, &info); err != nil {
		debuglog.Log(ctx, "MCP: failed to parse DCR file %q: %v", s.dcrPath, err)
		return nil, transport.ErrNoToken
	}

	return &info, nil
}

// SaveDCRInfo persists DCR client credentials to disk.
func (s *FileTokenStore) SaveDCRInfo(ctx context.Context, info *DCRInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dir := filepath.Dir(s.dcrPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create token dir %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal DCR info: %w", err)
	}

	if err := os.WriteFile(s.dcrPath, data, 0600); err != nil {
		return fmt.Errorf("write DCR file %q: %w", s.dcrPath, err)
	}

	debuglog.Log(ctx, "MCP: saved DCR info for %q", s.serverName)
	return nil
}

// mcpPendingPath returns the path to a server's pending OAuth state file.
func mcpPendingPath(serverName string) (string, error) {
	dir, err := mcpTokensDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, serverName+"_pending.json"), nil
}

// SavePendingState persists the OAuth2 authorization state and PKCE verifier
// for a manual flow so that CompleteManualAuth can pick them up later.
func (s *FileTokenStore) SavePendingState(ctx context.Context, state *OAuthPendingState) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dir := filepath.Dir(s.pendingPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create token dir %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pending state: %w", err)
	}

	if err := os.WriteFile(s.pendingPath, data, 0600); err != nil {
		return fmt.Errorf("write pending state %q: %w", s.pendingPath, err)
	}

	debuglog.Log(ctx, "MCP: saved pending OAuth state for %q", s.serverName)
	return nil
}

// GetPendingState loads the pending OAuth2 authorization state from disk,
// then deletes the file so it can't be replayed.
// Returns transport.ErrNoToken if no pending state exists.
func (s *FileTokenStore) GetPendingState(ctx context.Context) (*OAuthPendingState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.pendingPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, transport.ErrNoToken
		}
		return nil, fmt.Errorf("read pending state %q: %w", s.pendingPath, err)
	}

	var state OAuthPendingState
	if err := json.Unmarshal(data, &state); err != nil {
		debuglog.Log(ctx, "MCP: failed to parse pending state %q: %v", s.pendingPath, err)
		return nil, transport.ErrNoToken
	}

	// Consume the pending state — don't allow replay
	if err := os.Remove(s.pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		debuglog.Log(ctx, "MCP: failed to remove pending state %q: %v", s.pendingPath, err)
	}
	debuglog.Log(ctx, "MCP: loaded and consumed pending OAuth state for %q", s.serverName)
	return &state, nil
}
