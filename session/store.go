package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// Store defines the interface for session persistence
type Store interface {
	CreateSession(session *Session) error
	LoadMeta(id string) (*Session, error)
	AppendMessage(id string, msg *Message) error
	LoadMessages(id string) ([]Message, error)
	ReplaceLastMessage(id string, msg *Message) error
	UpdateMeta(session *Session) error
	ListSessions() ([]*Session, error)
	DeleteSession(id string) error
}

// FileStore implements Store interface using filesystem
type FileStore struct {
	baseDir string
}

// NewFileStore creates a new FileStore
func NewFileStore(baseDir string) (*FileStore, error) {
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	return &FileStore{baseDir: baseDir}, nil
}

func (s *FileStore) sessionDir(id string) string {
	return filepath.Join(s.baseDir, id)
}

func (s *FileStore) metaPath(id string) string {
	return filepath.Join(s.sessionDir(id), "meta.json")
}

func (s *FileStore) messagesPath(id string) string {
	return filepath.Join(s.sessionDir(id), "messages.jsonl")
}

// CreateSession creates a new session directory and meta.json
func (s *FileStore) CreateSession(session *Session) error {
	if err := fileutil.WriteJSONPrivate(s.metaPath(session.ID), session); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}

	// Create empty messages.jsonl
	if err := fileutil.WriteFilePrivate(s.messagesPath(session.ID), []byte{}); err != nil {
		return fmt.Errorf("create messages.jsonl: %w", err)
	}
	return nil
}

// LoadMeta loads meta.json for a session
func (s *FileStore) LoadMeta(id string) (*Session, error) {
	var session Session
	if err := fileutil.ReadJSON(s.metaPath(id), &session); err != nil {
		return nil, fmt.Errorf("read meta.json: %w", err)
	}
	return &session, nil
}

// AppendMessage appends a message to the session's messages.jsonl
func (s *FileStore) AppendMessage(id string, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	f, err := os.OpenFile(s.messagesPath(id), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open messages.jsonl: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	return nil
}

// LoadMessages reads all messages from messages.jsonl
func (s *FileStore) LoadMessages(id string) ([]Message, error) {
	f, err := os.Open(s.messagesPath(id))
	if err != nil {
		return nil, fmt.Errorf("open messages.jsonl: %w", err)
	}
	defer f.Close()

	var messages []Message
	scanner := bufio.NewScanner(f)
	// Increase buffer size to handle large messages (default 64KB is
	// insufficient for messages with large tool results or file contents).
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("unmarshal message: %w", err)
		}
		messages = append(messages, msg)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan messages: %w", err)
	}

	return messages, nil
}

// ReplaceLastMessage overwrites the last message in messages.jsonl. Used by
// artifact-reminder merging (session.artifact.go) to extend an existing
// reminder block in place; rewrites the whole file since jsonl append-only
// stores cannot patch the tail in place. Session histories are bounded, so
// the rewrite is cheap.
//
// Concurrency: this is a read-modify-write over the whole file. Callers MUST
// guarantee no concurrent writes to the same session (e.g. hold the Manager
// mutex or the cached-agent lock) — a concurrent AppendMessage from another
// Manager instance during the rewrite window would be lost.
func (s *FileStore) ReplaceLastMessage(id string, msg *Message) error {
	msgs, err := s.LoadMessages(id)
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("no messages to replace")
	}
	msgs[len(msgs)-1] = *msg

	var sb strings.Builder
	for i := range msgs {
		data, err := json.Marshal(&msgs[i])
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		sb.WriteString(string(data))
		sb.WriteByte('\n')
	}

	path := s.messagesPath(id)
	if err := fileutil.AtomicWriteFilePrivate(path, []byte(sb.String())); err != nil {
		return fmt.Errorf("rewrite messages.jsonl: %w", err)
	}
	return nil
}

// UpdateMeta updates the meta.json file
func (s *FileStore) UpdateMeta(session *Session) error {
	if err := fileutil.WriteJSONPrivate(s.metaPath(session.ID), session); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}
	return nil
}

// ListSessions returns all sessions sorted by created_at descending
func (s *FileStore) ListSessions() ([]*Session, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, fmt.Errorf("read session dir: %w", err)
	}

	var sessions []*Session
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		id := entry.Name()
		session, err := s.LoadMeta(id)
		if err != nil {
			continue // skip invalid sessions
		}

		sessions = append(sessions, session)
	}

	// Sort by CreatedAt descending (newest first)
	slices.SortFunc(sessions, func(a, b *Session) int {
		return b.CreatedAt.Compare(a.CreatedAt) // descending
	})

	return sessions, nil
}

// DeleteSession removes a session directory
func (s *FileStore) DeleteSession(id string) error {
	dir := s.sessionDir(id)
	return os.RemoveAll(dir)
}

// GenerateID generates a new session ID in format: YYYY-MM-DD-HHMMSS-uuid
func GenerateID() string {
	t := time.Now()
	shortUUID := strutil.ShortUUID(8)
	return fmt.Sprintf("%d-%02d-%02d-%02d%02d%02d-%s",
		t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		shortUUID)
}

// ExtractTitle extracts the first user message content as title, capped at
// 50 runes (ellipsis included in the budget when truncated). Uses rune-based
// truncation to safely handle multi-byte characters (e.g. CJK).
func ExtractTitle(content string) string {
	return strutil.TruncateFitted(content, 50)
}

// LoadSubagentMessages loads all subagent messages for a session.
// Returns a map of subagentID → messages.
func LoadSubagentMessages(sessionID string) (map[string][]Message, error) {
	dir, err := config.SessionDir()
	if err != nil {
		return nil, fmt.Errorf("session dir: %w", err)
	}

	subDir := filepath.Join(dir, sessionID, "subagent")
	entries, err := os.ReadDir(subDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No subagents
		}
		return nil, fmt.Errorf("read subagent dir: %w", err)
	}

	result := make(map[string][]Message)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}

		subID := entry.Name()[:len(entry.Name())-len(".jsonl")]
		path := filepath.Join(subDir, entry.Name())

		f, err := os.Open(path)
		if err != nil {
			continue
		}

		var msgs []Message
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var msg Message
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			msgs = append(msgs, msg)
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			continue // skip files with read errors (e.g. oversized lines)
		}

		if len(msgs) > 0 {
			result[subID] = msgs
		}
	}

	return result, nil
}
