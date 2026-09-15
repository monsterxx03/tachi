package session

import (
	"encoding/json"
	"fmt"
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

func TestCountMessages(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	sess := &Session{ID: GenerateID(), Title: "count"}
	if err := store.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Empty session → zero counts.
	if total, tools := store.CountMessages(sess.ID); total != 0 || tools != 0 {
		t.Fatalf("empty session = %d/%d, want 0/0", total, tools)
	}
	// Unknown session → zero counts (best-effort).
	if total, tools := store.CountMessages("does-not-exist"); total != 0 || tools != 0 {
		t.Fatalf("missing session = %d/%d, want 0/0", total, tools)
	}

	msgs := []*Message{
		{Type: MessageTypeUser, Content: "hello"},
		{Type: MessageTypeAssistant, Content: "hi"},
		{Type: MessageTypeToolCall, Name: "read_file"},
		{Type: MessageTypeToolResult, Result: `{"type":"tool_call"} in content`},
	}
	for _, m := range msgs {
		if err := store.AppendMessage(sess.ID, m); err != nil {
			t.Fatalf("AppendMessage failed: %v", err)
		}
	}

	total, tools := store.CountMessages(sess.ID)
	if total != len(msgs) {
		t.Fatalf("total = %d, want %d", total, len(msgs))
	}
	if tools != 1 {
		t.Fatalf("tool_calls = %d, want 1 (only the real tool_call message)", tools)
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

// ── the session-list snapshot ───────────────────────────────────────────────
//
// FileStore memoizes the session list so that the callers needing "every session"
// in the same breath — the desktop's sidebar asks for its rows, then for the
// per-project counts, then for one project's members, all as separate calls — pay
// for one directory walk instead of one each. These pin the contract that makes
// that safe, and the one hole it is allowed to have.

// newListStore returns a store holding n sessions, newest first.
func newListStore(t *testing.T, n int) *FileStore {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	base := time.Now().Add(-time.Duration(n) * time.Minute)
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := store.CreateSession(&Session{
			ID:        fmt.Sprintf("2026-01-01-0000%02d-%08d", i, i),
			Title:     fmt.Sprintf("会话 %d", i),
			CreatedAt: at,
			UpdatedAt: at,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	return store
}

// listTitles returns the listed sessions' titles, in list order.
func listTitles(t *testing.T, store *FileStore) []string {
	t.Helper()
	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	titles := make([]string, 0, len(sessions))
	for _, s := range sessions {
		titles = append(titles, s.Title)
	}
	return titles
}

// TestListSessionsServesTheSnapshot pins the point of the cache: a repeat call is
// answered from the snapshot, not from disk.
//
// The metadata is removed behind the store's back first — a removal INSIDE a
// session directory, which moves no directory mtime — so a re-scan would find
// nothing while the snapshot still holds three titles. Turning that into "three
// titles came back" is the only way to assert the second call never touched the
// filesystem, which is exactly what the sidebar's three-calls-one-refresh pattern
// depends on.
func TestListSessionsServesTheSnapshot(t *testing.T) {
	store := newListStore(t, 3)
	if got := listTitles(t, store); len(got) != 3 {
		t.Fatalf("first list: got %d titles, want 3", len(got))
	}

	entries, err := os.ReadDir(store.baseDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(store.baseDir, e.Name(), "meta.json")); err != nil {
			t.Fatalf("remove meta.json: %v", err)
		}
	}

	if got := listTitles(t, store); len(got) != 3 {
		t.Errorf("second list re-read the directory: got %d titles, want the 3 the snapshot holds", len(got))
	}
}

// TestListSessionsSeesItsOwnWrites is the other half: an in-place meta.json
// rewrite (a rename, a title, a fresh UpdatedAt) moves no directory mtime, so the
// stamp cannot catch it — only the write path dropping the snapshot can.
func TestListSessionsSeesItsOwnWrites(t *testing.T) {
	store := newListStore(t, 2)
	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	sessions[0].Title = "改名了"
	if err := store.UpdateMeta(sessions[0]); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got := listTitles(t, store); got[0] != "改名了" {
		t.Errorf("update not visible: got %q", got[0])
	}

	if err := store.DeleteSession(sessions[0].ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got := listTitles(t, store); len(got) != 1 {
		t.Errorf("delete not visible: got %d titles, want 1", len(got))
	}

	now := time.Now()
	if err := store.CreateSession(&Session{ID: "2026-01-02-000000-aaaaaaaa", Title: "新的", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := listTitles(t, store); len(got) != 2 {
		t.Errorf("create not visible: got %d titles, want 2", len(got))
	}
}

// TestListSessionsSeesAnotherProcess pins the baseDir-mtime check, which exists
// for the writers this store cannot hear from: a session created or deleted by a
// second process (the TUI beside the desktop, a channel bot) moves the directory's
// mtime, and the next read must notice.
func TestListSessionsSeesAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	mine, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	other, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if got := listTitles(t, mine); len(got) != 0 {
		t.Fatalf("empty store: got %d titles, want 0", len(got))
	}

	now := time.Now()
	if err := other.CreateSession(&Session{ID: "2026-02-01-000000-bbbbbbbb", Title: "别人建的", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := listTitles(t, mine); len(got) != 1 || got[0] != "别人建的" {
		t.Errorf("a session created elsewhere was missed: got %v", got)
	}

	if err := other.DeleteSession("2026-02-01-000000-bbbbbbbb"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got := listTitles(t, mine); len(got) != 0 {
		t.Errorf("a session deleted elsewhere was missed: got %v", got)
	}
}

// TestListSessionsHandsOutCopies pins that the snapshot is not handed out for
// editing: a caller is free to mutate what it was given (the session manager loads
// one into its current field and edits it in place), and the next reader must not
// inherit that.
func TestListSessionsHandsOutCopies(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	now := time.Now()
	if err := store.CreateSession(&Session{
		ID:             "2026-03-01-000000-cccccccc",
		Title:          "原文",
		AdditionalDirs: []string{"/tmp/a"},
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	first, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	first[0].Title = "调用方改的"
	first[0].AdditionalDirs[0] = "/tmp/调用方改的"

	second, err := store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if second[0].Title != "原文" {
		t.Errorf("title mutation leaked into the snapshot: %q", second[0].Title)
	}
	if second[0].AdditionalDirs[0] != "/tmp/a" {
		t.Errorf("AdditionalDirs mutation leaked into the snapshot: %q", second[0].AdditionalDirs[0])
	}
}

// TestListSessionsMissesInPlaceWritesByAnotherWriter pins the snapshot's ONE hole,
// so that a later reader finds it stated rather than assumes it is not there: a
// second writer rewriting an EXISTING meta.json in place moves no directory mtime,
// so this store keeps serving what it read until a create or delete follows.
//
// Only the title and the timestamps can go stale through it. Project membership
// cannot: ProjectID is written by the desktop alone, and every one of those writes
// goes through a store that invalidates.
func TestListSessionsMissesInPlaceWritesByAnotherWriter(t *testing.T) {
	dir := t.TempDir()
	mine, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	other, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	now := time.Now()
	const id = "2026-04-01-000000-dddddddd"
	if err := other.CreateSession(&Session{ID: id, Title: "原题", CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := listTitles(t, mine); len(got) != 1 {
		t.Fatalf("priming list: got %v", got)
	}

	renamed, err := other.LoadMeta(id)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	renamed.Title = "别的进程改的"
	if err := other.UpdateMeta(renamed); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got := listTitles(t, mine); got[0] != "原题" {
		t.Fatalf("expected the snapshot to still hold the old title, got %q — if this now reads the new one, the hole closed and this test is obsolete", got[0])
	}

	// The staleness is bounded, not permanent: the next structural change releases it.
	if err := other.CreateSession(&Session{ID: "2026-04-01-000000-eeeeeeee", Title: "后来的", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got := listTitles(t, mine)
	if len(got) != 2 || got[1] != "别的进程改的" {
		t.Errorf("a directory change did not release the snapshot: got %v", got)
	}
}
