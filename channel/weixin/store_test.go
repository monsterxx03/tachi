package weixin

import (
	"os"
	"testing"
)

func TestStateStore_Accounts(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(dir)
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}

	// Initially empty.
	list, err := store.loadAccountList()
	if err != nil {
		t.Fatalf("loadAccountList: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %v", list)
	}

	// Save an account.
	accountID := "test-bot@im.bot"
	data := &AccountData{
		Token:   "token-abc123",
		SavedAt: "2025-01-01T00:00:00Z",
		BaseURL: "https://example.com",
		UserID:  "wx_user@im.wechat",
	}
	if err := store.saveAccount(accountID, data); err != nil {
		t.Fatalf("saveAccount: %v", err)
	}

	// Load it back.
	loaded, err := store.loadAccount(accountID)
	if err != nil {
		t.Fatalf("loadAccount: %v", err)
	}
	if loaded.Token != data.Token {
		t.Errorf("token: got %q, want %q", loaded.Token, data.Token)
	}
	if loaded.UserID != data.UserID {
		t.Errorf("userID: got %q, want %q", loaded.UserID, data.UserID)
	}

	// Account list should contain it.
	list, err = store.loadAccountList()
	if err != nil {
		t.Fatalf("loadAccountList: %v", err)
	}
	if len(list) != 1 || list[0] != accountID {
		t.Errorf("expected [%s], got %v", accountID, list)
	}
}

func TestStateStore_SyncBuf(t *testing.T) {
	dir := t.TempDir()
	store, _ := newStateStore(dir)

	accountID := "sync-test@im.bot"

	// Empty initially.
	buf := store.loadSyncBuf(accountID)
	if buf != "" {
		t.Errorf("expected empty buf, got %q", buf)
	}

	// Save and reload.
	if err := store.saveSyncBuf(accountID, "buf-value-123"); err != nil {
		t.Fatalf("saveSyncBuf: %v", err)
	}

	buf = store.loadSyncBuf(accountID)
	if buf != "buf-value-123" {
		t.Errorf("got %q, want %q", buf, "buf-value-123")
	}
}

func TestStateStore_ContextTokens(t *testing.T) {
	dir := t.TempDir()
	store, _ := newStateStore(dir)

	accountID := "ctx-test@im.bot"

	// Empty initially.
	tok := store.loadContextToken(accountID, "user-1")
	if tok != "" {
		t.Errorf("expected empty token, got %q", tok)
	}

	// Save a token.
	if err := store.saveContextToken(accountID, "user-1", "token-aaa"); err != nil {
		t.Fatalf("saveContextToken: %v", err)
	}

	tok = store.loadContextToken(accountID, "user-1")
	if tok != "token-aaa" {
		t.Errorf("got %q, want %q", tok, "token-aaa")
	}

	// Other user still empty.
	tok = store.loadContextToken(accountID, "user-2")
	if tok != "" {
		t.Errorf("expected empty token for user-2, got %q", tok)
	}
}

func TestStateStore_AllowFrom(t *testing.T) {
	dir := t.TempDir()
	store, _ := newStateStore(dir)

	accountID := "allow-test@im.bot"

	// Initially empty allowlist.
	if store.isUserAllowed(accountID, "user-a") {
		t.Error("user-a should not be allowed initially")
	}

	// Add a user.
	data := &AllowFromData{Version: 1, AllowFrom: []string{"user-a"}}
	if err := store.saveAllowFrom(accountID, data); err != nil {
		t.Fatalf("saveAllowFrom: %v", err)
	}

	if !store.isUserAllowed(accountID, "user-a") {
		t.Error("user-a should be allowed now")
	}
	if store.isUserAllowed(accountID, "user-b") {
		t.Error("user-b should not be allowed")
	}
}

func TestStateStore_DeleteAccount(t *testing.T) {
	dir := t.TempDir()
	store, _ := newStateStore(dir)

	accountID := "delete-test@im.bot"
	data := &AccountData{Token: "t", BaseURL: "u", UserID: "u"}
	store.saveAccount(accountID, data)
	store.saveSyncBuf(accountID, "buf")
	store.saveContextToken(accountID, "u", "ctx")

	// Delete.
	if err := store.deleteAccount(accountID); err != nil {
		t.Fatalf("deleteAccount: %v", err)
	}

	// Files should be gone.
	_, err := store.loadAccount(accountID)
	if !os.IsNotExist(err) {
		t.Errorf("expected NotExist for account file, got %v", err)
	}

	buf := store.loadSyncBuf(accountID)
	if buf != "" {
		t.Errorf("expected empty buf after delete, got %q", buf)
	}
}
