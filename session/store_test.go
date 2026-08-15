package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateID(t *testing.T) {
	id := GenerateID()
	// Format: YYYY-MM-DD-HHMMSS-uuid(8chars)
	if len(id) < 24 {
		t.Errorf("ID too short: %s", id)
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"short", "short"},
		{"exactly fifty characters is this string 123456", "exactly fifty characters is this string 123456"},          // 46 chars
		{"12345678901234567890123456789012345678901234567890", "12345678901234567890123456789012345678901234567890"},  // 50 chars, no truncation
		{"123456789012345678901234567890123456789012345678901", "1234567890123456789012345678901234567890123456789…"}, // 51 chars -> truncated to 49 + "…"
		// CJK characters: each is 3 bytes in UTF-8. Byte-level truncation would corrupt these.
		{"你好世界", "你好世界"}, // 4 runes, well under 50
		{"这是一个测试标题用于验证中文截断功能是否正常工作", "这是一个测试标题用于验证中文截断功能是否正常工作"},                                                                       // 20 runes, under 50
		{"这是一个很长的中文标题用于测试截断功能是否正常运作当标题超过五十个字符时应该被正确截断而不是出现乱码这个问题需要被修复以确保用户体验良好", "这是一个很长的中文标题用于测试截断功能是否正常运作当标题超过五十个字符时应该被正确截断而不是出现乱…"}, // 68 runes -> truncated to 49 + "…"
		// Mixed CJK and ASCII
		{"Hello世界ThisIsAMixedTitleWithChineseCharacters用来测试混合字符截断", "Hello世界ThisIsAMixedTitleWithChineseCharacters用来测试…"}, // 55 runes -> truncated to 49 + "…"
	}

	for _, tt := range tests {
		result := ExtractTitle(tt.input)
		if result != tt.expected {
			t.Errorf("ExtractTitle(%q) = %q, want %q", tt.input, result, tt.expected)
		}
		// Verify result is valid UTF-8
		if len(result) > 0 {
			_ = []rune(result) // will panic if invalid UTF-8, but let's be explicit
		}
	}
}

func TestStore(t *testing.T) {
	// Create temp dir
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	// Test CreateSession
	session := &Session{
		ID:           GenerateID(),
		Title:        "Test Session",
		ProviderName: "openai",
	}
	if err := store.CreateSession(session); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Verify directory created
	sessionDir := filepath.Join(tmpDir, session.ID)
	if _, err := os.Stat(sessionDir); os.IsNotExist(err) {
		t.Error("session directory not created")
	}

	// Test LoadMeta
	loaded, err := store.LoadMeta(session.ID)
	if err != nil {
		t.Fatalf("LoadMeta failed: %v", err)
	}
	if loaded.Title != session.Title {
		t.Errorf("title mismatch: got %s, want %s", loaded.Title, session.Title)
	}

	// Test AppendMessage
	msg := &Message{
		Type:    MessageTypeUser,
		Content: "Hello",
	}
	if err := store.AppendMessage(session.ID, msg); err != nil {
		t.Fatalf("AppendMessage failed: %v", err)
	}

	// Test LoadMessages
	messages, err := store.LoadMessages(session.ID)
	if err != nil {
		t.Fatalf("LoadMessages failed: %v", err)
	}
	if len(messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Content != "Hello" {
		t.Errorf("message content mismatch: got %s, want Hello", messages[0].Content)
	}

	// Test ListSessions
	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(sessions))
	}

	// Test DeleteSession
	if err := store.DeleteSession(session.ID); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Error("session directory still exists after delete")
	}
}

func TestStoreAPIRequests(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	sess := &Session{ID: GenerateID(), Title: "API Req Test"}
	if err := store.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// No file yet → empty, no error.
	reqs, err := store.LoadAPIRequests(sess.ID)
	if err != nil {
		t.Fatalf("LoadAPIRequests on fresh session failed: %v", err)
	}
	if len(reqs) != 0 {
		t.Fatalf("expected 0 requests, got %d", len(reqs))
	}

	// Append two requests.
	r1 := &APIRequest{
		Timestamp:    time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC),
		SystemPrompt: "You are Tachi.",
		Tools: []APITool{{
			Name:        "ReadFile",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
	r2 := &APIRequest{
		Timestamp:    time.Date(2026, 8, 15, 10, 1, 0, 0, time.UTC),
		SystemPrompt: "You are Tachi.",
		Tools:        nil,
	}
	for _, r := range []*APIRequest{r1, r2} {
		if err := store.AppendAPIRequest(sess.ID, r); err != nil {
			t.Fatalf("AppendAPIRequest failed: %v", err)
		}
	}

	reqs, err = store.LoadAPIRequests(sess.ID)
	if err != nil {
		t.Fatalf("LoadAPIRequests failed: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	if reqs[0].SystemPrompt != "You are Tachi." {
		t.Errorf("system prompt mismatch: %q", reqs[0].SystemPrompt)
	}
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "ReadFile" {
		t.Errorf("tools not round-tripped: %+v", reqs[0].Tools)
	}
	if string(reqs[0].Tools[0].Parameters) != `{"type":"object","properties":{"path":{"type":"string"}}}` {
		t.Errorf("parameters not round-tripped: %s", reqs[0].Tools[0].Parameters)
	}

	// Malformed line is skipped, valid lines still load.
	reqPath := filepath.Join(tmpDir, sess.ID, "api_requests.jsonl")
	f, err := os.OpenFile(reqPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open api_requests.jsonl: %v", err)
	}
	if _, err := f.WriteString("{not-json\n"); err != nil {
		t.Fatalf("write malformed line: %v", err)
	}
	f.Close()

	reqs, err = store.LoadAPIRequests(sess.ID)
	if err != nil {
		t.Fatalf("LoadAPIRequests with malformed line failed: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests after malformed line, got %d", len(reqs))
	}

	// Manager round-trip.
	mgr := NewManagerWithStore(store, nil)
	mgr.SetCurrent(sess)
	if err := mgr.AppendAPIRequest(&APIRequest{
		Timestamp:    time.Now(),
		SystemPrompt: "third",
	}); err != nil {
		t.Fatalf("Manager.AppendAPIRequest failed: %v", err)
	}
	reqs, err = mgr.LoadAPIRequests(sess.ID)
	if err != nil {
		t.Fatalf("Manager.LoadAPIRequests failed: %v", err)
	}
	if len(reqs) != 3 || reqs[2].SystemPrompt != "third" {
		t.Fatalf("manager round-trip mismatch: %+v", reqs)
	}
}
