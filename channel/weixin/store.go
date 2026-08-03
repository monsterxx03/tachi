package weixin

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// stateStore manages persistent state on disk for the Weixin channel.
type stateStore struct {
	dir string
	mu  sync.RWMutex
}

func newStateStore(dir string) (*stateStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	return &stateStore{dir: dir}, nil
}

// --- Account IDs ---

// normalizeID converts account ID (e.g. "a1b2c3d4@im.bot") to a
// filesystem-safe form (e.g. "a1b2c3d4-im-bot").
func normalizeID(id string) string {
	s := strings.ReplaceAll(id, "@", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

// accountsListPath returns the path to the registered accounts list file.
func (s *stateStore) accountsListPath() string {
	return filepath.Join(s.dir, "accounts.json")
}

// accountPath returns the path for an account's credential file.
func (s *stateStore) accountPath(accountID string) string {
	return filepath.Join(s.dir, "accounts", normalizeID(accountID)+".json")
}

// syncPath returns the path for an account's sync (get_updates_buf) file.
func (s *stateStore) syncPath(accountID string) string {
	return filepath.Join(s.dir, "accounts", normalizeID(accountID)+".sync.json")
}

// contextTokensPath returns the path for an account's context tokens file.
func (s *stateStore) contextTokensPath(accountID string) string {
	return filepath.Join(s.dir, "accounts", normalizeID(accountID)+".context-tokens.json")
}

// allowFromPath returns the path for an account's allowed users file.
func (s *stateStore) allowFromPath(accountID string) string {
	return filepath.Join(s.dir, normalizeID(accountID)+"-allowFrom.json")
}

// --- Read/Write ---

func (s *stateStore) loadJSON(path string, v any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fileutil.ReadJSON(path, v)
}

func (s *stateStore) saveJSON(path string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fileutil.WriteJSONPrivate(path, v)
}

// --- Accounts ---

func (s *stateStore) loadAccountList() ([]string, error) {
	var list []string
	if err := s.loadJSON(s.accountsListPath(), &list); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return list, nil
}

func (s *stateStore) saveAccountList(list []string) error {
	return s.saveJSON(s.accountsListPath(), list)
}

func (s *stateStore) addAccountToList(accountID string) error {
	list, _ := s.loadAccountList()
	if slices.Contains(list, accountID) {
		return nil
	}
	list = append(list, accountID)
	return s.saveAccountList(list)
}

func (s *stateStore) loadAccount(accountID string) (*AccountData, error) {
	var data AccountData
	if err := s.loadJSON(s.accountPath(accountID), &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func (s *stateStore) saveAccount(accountID string, data *AccountData) error {
	if err := s.saveJSON(s.accountPath(accountID), data); err != nil {
		return err
	}
	return s.addAccountToList(accountID)
}

func (s *stateStore) deleteAccount(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := []string{
		s.accountPath(accountID),
		s.syncPath(accountID),
		s.contextTokensPath(accountID),
		s.allowFromPath(accountID),
	}
	for _, f := range files {
		_ = fileutil.RemoveIgnoreNotExist(f)
	}
	return nil
}

// --- Sync (get_updates_buf) ---

func (s *stateStore) loadSyncBuf(accountID string) string {
	var data SyncData
	if err := s.loadJSON(s.syncPath(accountID), &data); err != nil {
		return ""
	}
	return data.GetUpdatesBuf
}

func (s *stateStore) saveSyncBuf(accountID string, buf string) error {
	data := SyncData{GetUpdatesBuf: buf}
	return s.saveJSON(s.syncPath(accountID), &data)
}

// --- Context Tokens ---

func (s *stateStore) loadContextTokens(accountID string) (ContextTokens, error) {
	tokens := make(ContextTokens)
	if err := s.loadJSON(s.contextTokensPath(accountID), &tokens); err != nil {
		if os.IsNotExist(err) {
			return tokens, nil
		}
		return nil, err
	}
	return tokens, nil
}

func (s *stateStore) saveContextTokens(accountID string, tokens ContextTokens) error {
	return s.saveJSON(s.contextTokensPath(accountID), tokens)
}

func (s *stateStore) loadContextToken(accountID, userID string) string {
	tokens, err := s.loadContextTokens(accountID)
	if err != nil {
		return ""
	}
	return tokens[userID]
}

func (s *stateStore) saveContextToken(accountID, userID, token string) error {
	tokens, err := s.loadContextTokens(accountID)
	if err != nil {
		tokens = make(ContextTokens)
	}
	tokens[userID] = token
	return s.saveContextTokens(accountID, tokens)
}

// --- Allow From ---

func (s *stateStore) loadAllowFrom(accountID string) (*AllowFromData, error) {
	var data AllowFromData
	if err := s.loadJSON(s.allowFromPath(accountID), &data); err != nil {
		if os.IsNotExist(err) {
			return &AllowFromData{Version: 1}, nil
		}
		return nil, err
	}
	return &data, nil
}

func (s *stateStore) saveAllowFrom(accountID string, data *AllowFromData) error {
	return s.saveJSON(s.allowFromPath(accountID), data)
}

func (s *stateStore) isUserAllowed(accountID, userID string) bool {
	data, err := s.loadAllowFrom(accountID)
	if err != nil {
		return false
	}
	return slices.Contains(data.AllowFrom, userID)
}
