package session

import (
	"os"
	"path/filepath"
	"testing"
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
		{"exactly fifty characters is this string 123456", "exactly fifty characters is this string 123456"}, // 46 chars
		{"12345678901234567890123456789012345678901234567890", "12345678901234567890123456789012345678901234567890"}, // 50 chars, no truncation
		{"123456789012345678901234567890123456789012345678901", "12345678901234567890123456789012345678901234567..."}, // 51 chars -> truncated
	}

	for _, tt := range tests {
		result := ExtractTitle(tt.input)
		if result != tt.expected {
			t.Errorf("ExtractTitle(%q) = %q, want %q", tt.input, result, tt.expected)
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
		ID:        GenerateID(),
		Title:     "Test Session",
		Provider:  "openai",
		Model:     "gpt-4",
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
