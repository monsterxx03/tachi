package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

type Manager struct {
	store         Store
	current       *Session
	maxKeep       int // max sessions to retain; 0 = no cleanup
	mu            sync.Mutex
	logger        *debuglog.Logger
}

func NewManager() (*Manager, error) {
	dir, err := config.SessionDir()
	if err != nil {
		return nil, fmt.Errorf("session dir: %w", err)
	}

	store, err := NewFileStore(dir)
	if err != nil {
		return nil, err
	}

	return &Manager{store: store, maxKeep: 100, logger: debuglog.DefaultLogger}, nil
}

// NewManagerWithStore creates a Manager with a custom store implementation
func NewManagerWithStore(store Store) *Manager {
	return &Manager{store: store, logger: debuglog.DefaultLogger}
}

// SetMaxKeep sets the maximum number of sessions to retain.
// 0 means no automatic cleanup.
func (m *Manager) SetMaxKeep(maxKeep int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxKeep = maxKeep
}

// CleanupOldSessions removes the oldest sessions when the total count exceeds
// maxKeep. Sessions are sorted by CreatedAt descending (newest first), so the
// oldest sessions are at the end of the list. The current session is never
// deleted. Returns the number of sessions removed.
func (m *Manager) CleanupOldSessions() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupLocked(), nil
}

// cleanupLocked is the lock-holding variant of CleanupOldSessions.
// Caller must hold m.mu.
func (m *Manager) cleanupLocked() int {
	if m.maxKeep <= 0 {
		return 0
	}

	sessions, err := m.store.ListSessions()
	if err != nil {
		m.logger.Log("session cleanup: list sessions error: %v", err)
		return 0
	}

	if len(sessions) <= m.maxKeep {
		return 0
	}

	// sessions are sorted newest-first, so excess are at the end
	excess := sessions[m.maxKeep:]

	// Collect current session ID to avoid deleting it
	currentID := ""
	if m.current != nil {
		currentID = m.current.ID
	}

	removed := 0
	for _, s := range excess {
		if s.ID == currentID {
			continue
		}
		if err := m.store.DeleteSession(s.ID); err != nil {
			// Log but continue — best-effort cleanup
			m.logger.Log("session cleanup: failed to delete %s: %v", s.ID, err)
			continue
		}
		removed++
	}

	if removed > 0 {
		m.logger.Log("session cleanup: removed %d old sessions (maxKeep=%d)", removed, m.maxKeep)
	}

	return removed
}

// Current returns the current session
func (m *Manager) Current() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// HasCurrent returns true if there is an active session
func (m *Manager) HasCurrent() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current != nil
}

// New creates a new session
func (m *Manager) New(provider, model string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	session := &Session{
		ID:        GenerateID(),
		Provider:  provider,
		Model:     model,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := m.store.CreateSession(session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	m.current = session

	// Clean up old sessions if we exceed the limit (lock already held).
	m.cleanupLocked()

	return session, nil
}

// SetCurrent sets the current session (for loading existing sessions)
func (m *Manager) SetCurrent(session *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = session
}

// EndCurrent ends the current session without deleting it from disk.
// The next call to HasCurrent() will return false.
func (m *Manager) EndCurrent() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = nil
}

// AppendMessage appends a message to the current session and persists immediately
func (m *Manager) AppendMessage(msg *Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return fmt.Errorf("no active session")
	}

	msg.Timestamp = time.Now()

	if err := m.store.AppendMessage(m.current.ID, msg); err != nil {
		return fmt.Errorf("append message: %w", err)
	}

	m.current.UpdatedAt = time.Now()
	if err := m.store.UpdateMeta(m.current); err != nil {
		return fmt.Errorf("update meta: %w", err)
	}

	return nil
}

// SetTitle updates the session title (typically called after first user message)
func (m *Manager) SetTitle(title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return fmt.Errorf("no active session")
	}

	m.current.Title = ExtractTitle(title)
	m.current.UpdatedAt = time.Now()

	return m.store.UpdateMeta(m.current)
}

// SetThreadID records the channel ThreadID on the session for later lookup.
func (m *Manager) SetThreadID(threadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return fmt.Errorf("no active session")
	}

	m.current.ThreadID = threadID
	m.current.UpdatedAt = time.Now()

	return m.store.UpdateMeta(m.current)
}

// FindByThreadID looks up a session by its ThreadID field, loads it,
// and returns it. Returns nil if no session matches.
func (m *Manager) FindByThreadID(threadID string) (*Session, error) {
	if threadID == "" {
		return nil, nil
	}

	sessions, err := m.List()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	for _, s := range sessions {
		if s.ThreadID == threadID {
			_, err := m.Load(s.ID)
			if err != nil {
				return nil, fmt.Errorf("load session: %w", err)
			}
			return s, nil
		}
	}

	return nil, nil
}

// List returns all sessions sorted by created_at descending
func (m *Manager) List() ([]*Session, error) {
	return m.store.ListSessions()
}

// Load loads a session by ID
func (m *Manager) Load(id string) (*Session, error) {
	session, err := m.store.LoadMeta(id)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}

	m.SetCurrent(session)
	return session, nil
}

// LoadMessages loads all messages for the current session
func (m *Manager) LoadMessages() ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return nil, fmt.Errorf("no active session")
	}

	return m.store.LoadMessages(m.current.ID)
}

// Delete deletes a session by ID
func (m *Manager) Delete(id string) error {
	return m.store.DeleteSession(id)
}
