package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

type Manager struct {
	store   Store
	current *Session
	maxKeep int // max sessions to retain; 0 = no cleanup
	mu      sync.Mutex
	logger  *logger.Logger
	// retention is the shared answer to "may this session be deleted?" (see retention.go).
	// Nil means this manager cannot reach the session data dir (a custom store), and then it
	// makes no claim about protection: it neither skips a held session nor refuses a delete,
	// because it cannot tell. Managers over the real store always have one.
	retention *Retention
	// onSwitch, when set, is called when a conversation MOVES to another session (compaction)
	// — see SetSessionSwitchHook.
	onSwitch func(old, next *Session)
}

// DefaultMaxKeep is how many sessions a manager retains when the caller sets no
// cap of its own. It is the same number config's session_cleanup_max_count
// defaults to, and it is named because a manager built from a custom store
// (NewManagerWithStore) starts at zero and has to be able to ask for it.
const DefaultMaxKeep = 100

func NewManager(l *logger.Logger) (*Manager, error) {
	if l == nil {
		l = logger.New("session")
	}
	dir, err := config.SessionDir()
	if err != nil {
		return nil, fmt.Errorf("session dir: %w", err)
	}

	store, err := NewFileStore(dir)
	if err != nil {
		return nil, err
	}

	retention, err := NewRetention(dir, l)
	if err != nil {
		return nil, fmt.Errorf("session dir: %w", err)
	}

	return &Manager{store: store, maxKeep: DefaultMaxKeep, logger: l, retention: retention}, nil
}

// NewManagerWithStore creates a Manager with a custom store implementation.
//
// A store that knows its directory (`BaseDir`) also brings retention with it: the protection
// layer is nothing but files beside the sessions, so a manager that can see them can honour
// them. A store that does not (a test double, an in-memory store) has no holds to honour
// either — the node is the only writer, and it always works through the real store.
func NewManagerWithStore(store Store, l *logger.Logger) *Manager {
	if l == nil {
		// Defaulted HERE rather than at each log call: this manager logs from paths that decide
		// whether a session is deleted, and a nil logger there is a panic on the refusal path —
		// the one path a reader reaches exactly when something already went wrong.
		l = logger.New("session")
	}
	m := &Manager{store: store, logger: l}
	if withDir, ok := store.(interface{ BaseDir() string }); ok {
		if retention, err := NewRetention(withDir.BaseDir(), l); err == nil {
			m.retention = retention
		}
	}
	return m
}

// BaseDir reports the directory this manager's store keeps its sessions in, or "" when the
// store has none (an in-memory or otherwise custom store).
//
// It answers a question about LAYOUT, and the layers that live BESIDE the sessions are the
// ones that ask — a session's checkpoint store records its manifest and its snapshots inside
// that session's own directory. Asking the manager that WRITES the session is what keeps the
// two together: resolving the directory from a process-global default instead puts a session
// created under an explicit directory (a node's per-context manager, a test's temp store)
// somewhere else entirely, and leaves its leftovers behind in the real one.
func (m *Manager) BaseDir() string {
	if withDir, ok := m.store.(interface{ BaseDir() string }); ok {
		return withDir.BaseDir()
	}
	return ""
}

// SetSessionSwitchHook installs the callback a conversation's move to another session fires.
//
// It exists for the state a host keeps keyed by session id — a receiving node's retention
// holds and its context binding — which must follow the CONVERSATION, not the id it used to
// live in. Compaction is the only thing that moves a conversation (agent/compact.go), and it
// is also the one moment when the new session is unprotected and already being written to, so
// the notification has to arrive from there rather than being inferred later from the ids.
func (m *Manager) SetSessionSwitchHook(fn func(old, next *Session)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onSwitch = fn
}

// NotifySessionSwitch is how the compaction path tells the manager its conversation moved.
// It is a no-op without a hook (the ordinary case: most managers have nothing keyed by id).
func (m *Manager) NotifySessionSwitch(old, next *Session) {
	m.mu.Lock()
	fn := m.onSwitch
	m.mu.Unlock()
	if fn != nil {
		fn(old, next)
	}
}

// Retention exposes the protection layer for a host that owns the holds (the receiving node).
func (m *Manager) Retention() *Retention { return m.retention }

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
//
// A PINNED session is never deleted either, wherever it sits in the order: the pin is the
// reader's statement that this conversation must still be there tomorrow, and a session can
// be pinned precisely BECAUSE it is old and rarely touched — the two rules would otherwise
// point in opposite directions on the same session. The consequence is deliberate: pinned
// sessions inside the excess stay, so the list can remain above maxKeep until their pin is
// lifted. Nothing else is affected — the sweep still trims every unpinned session it would
// have trimmed.
//
// A HELD session is never deleted either, and that protection is not this manager's memory:
// it is the persisted record beside the sessions (retention.go), which every process sharing
// this data dir reads and writes under one coordination lock. So a receiving node's session
// survives a sweep run by the desktop, by another TUI, or by a manager inside the node's own
// process that never heard of that context.
//
// Storage errors (e.g. listing sessions) are logged internally and reported as
// zero removals, so callers don't need to handle an error return.
func (m *Manager) CleanupOldSessions() int {
	m.mu.Lock()
	maxKeep := m.maxKeep
	m.mu.Unlock()
	if maxKeep <= 0 {
		return 0
	}

	sessions, err := m.store.ListSessions()
	if err != nil {
		m.logger.Error(context.Background(), "session cleanup: list sessions error", err)
		return 0
	}

	if len(sessions) <= maxKeep {
		return 0
	}

	// sessions are sorted newest-first, so excess are at the end
	excess := sessions[maxKeep:]

	removed := 0
	skippedThread := 0
	skippedPinned := 0
	skippedCron := 0
	skippedHeld := 0
	skippedCurrent := 0
	skipped := make([]string, 0)
	held := make([]string, 0)
	for _, s := range excess {
		// Pinned and thread-bound are decided from the record as it was enumerated: both are
		// the user's (or a channel's) own statements about the session, and a sweep that reads
		// them one candidate at a time would only re-read the same file.
		if s.Pinned {
			skippedPinned++
			continue
		}
		// A scheduled task writes into this conversation. Deleting it would
		// orphan the job, and the sweep cannot read crons.json to find out: the
		// desktop maintains this flag on the record (and compaction moves it to
		// the child), so every entry point's cleanup can honour it.
		if s.CronBound {
			skippedCron++
			continue
		}
		// Skip sessions that are actively used by IM channels.
		// A non-empty ThreadID means a channel thread is bound to
		// this session — it will be found via FindByThreadID on
		// the next message. Only completed threads (cleared via
		// /new) should be cleaned up.
		if s.ThreadID != "" {
			skippedThread++
			skipped = append(skipped, s.ID+"("+s.ThreadID+")")
			continue
		}

		outcome := m.deleteIfUnprotected(s.ID)
		switch outcome {
		case deleteRemoved:
			removed++
		case deleteSkippedCurrent:
			skippedCurrent++
		case deleteSkippedHeld:
			skippedHeld++
			held = append(held, s.ID)
		}
	}

	if skippedThread > 0 {
		m.logger.Info(context.Background(), "session cleanup: skipped sessions with active ThreadID", "skipped", skippedThread, "ids", skipped)
	}
	if skippedPinned > 0 {
		m.logger.Info(context.Background(), "session cleanup: kept pinned sessions past maxKeep", "skipped", skippedPinned)
	}
	if skippedCron > 0 {
		m.logger.Info(context.Background(), "session cleanup: kept sessions a scheduled task writes into", "skipped", skippedCron)
	}
	if skippedHeld > 0 {
		m.logger.Info(context.Background(), "session cleanup: kept sessions a task or context still needs", "skipped", skippedHeld, "ids", held)
	}
	if skippedCurrent > 0 {
		m.logger.Info(context.Background(), "session cleanup: kept this manager's current session", "skipped", skippedCurrent)
	}
	if removed > 0 {
		m.logger.Info(context.Background(), "session cleanup: removed old sessions", "removed", removed, "maxKeep", maxKeep)
	}

	return removed
}

// deleteOutcome is why one candidate did or did not go.
type deleteOutcome int

const (
	deleteFailed deleteOutcome = iota
	deleteRemoved
	deleteSkippedCurrent
	deleteSkippedHeld
)

// deleteIfUnprotected is one deletion decision, made where it has to be: inside the
// coordination lock, with the manager's own lock taken inside THAT (the fixed order — file
// lock first, then the in-process one). Two things are re-read here rather than trusted from
// the enumeration, because both can change while a sweep is running: whether this session
// became the manager's current one, and whether something took a hold on it.
func (m *Manager) deleteIfUnprotected(id string) deleteOutcome {
	if m.retention == nil {
		// A manager that cannot see the session data dir (a custom store) also cannot see
		// holds. It makes no claim about protection rather than refusing to clean at all.
		m.mu.Lock()
		current := m.current != nil && m.current.ID == id
		m.mu.Unlock()
		if current {
			return deleteSkippedCurrent
		}
		return m.deleteNow(id)
	}

	outcome := deleteFailed
	err := m.retention.Lock(func() error {
		if err := m.retention.protected(id); err != nil {
			var held *SessionHeldError
			if errors.As(err, &held) || errors.Is(err, errProtectionUnknown) {
				// Held, or unknowable — both mean "keep it", and the difference is only what
				// gets logged (see deleteIfUnprotected's caller).
				m.logger.Info(context.Background(), "session cleanup: keeping a session; held or unreadable",
					"id", id, "cause", err)
				outcome = deleteSkippedHeld
				return nil
			}
			return err
		}
		m.mu.Lock()
		current := m.current != nil && m.current.ID == id
		m.mu.Unlock()
		if current {
			outcome = deleteSkippedCurrent
			return nil
		}
		outcome = m.deleteNow(id)
		if outcome == deleteRemoved {
			m.clearRetiredLocked(id)
		}
		return nil
	})
	if err != nil {
		m.logger.Error(context.Background(), "session cleanup: retention decision failed", err, "id", id)
		return deleteFailed
	}
	return outcome
}

// deleteNow is the one place a session directory is removed inside this package.
func (m *Manager) deleteNow(id string) deleteOutcome {
	if err := m.store.DeleteSession(id); err != nil {
		// Log but continue — best-effort cleanup
		m.logger.Error(context.Background(), "session cleanup: failed to delete", err, "id", id)
		return deleteFailed
	}
	return deleteRemoved
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
func (m *Manager) New(providerName, workingDir string) (*Session, error) {
	// newLocked is named for what it does — `m.current` is manager state — so the lock it documents
	// has to be taken here. It cannot stay held across the cleanup below: the sweep takes the
	// cross-process coordination lock first and this manager's second (see retention.go), and
	// holding it while acquiring that one would invert the order.
	m.mu.Lock()
	session, err := m.newLocked(providerName, workingDir)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Cleanup runs AFTER the new session exists and this manager's lock is released: the sweep
	// takes the cross-process coordination lock, and the fixed order is that lock first, then
	// the manager's own (see retention.go). Doing it here also means the new session can never
	// be one of the candidates — it is the newest by construction.
	m.CleanupOldSessions()
	return session, nil
}

// newLocked is New's first half: create the session and make it current.
// Caller must hold m.mu.
func (m *Manager) newLocked(providerName, workingDir string) (*Session, error) {
	now := time.Now()
	session := &Session{
		ID:           GenerateID(),
		ProviderName: providerName,
		WorkingDir:   workingDir,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := m.store.CreateSession(session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	m.current = session
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

// SetTitle updates the session title (typically called after first user message).
// Errors are logged internally — this is a best-effort metadata write.
func (m *Manager) SetTitle(title string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return
	}

	m.current.Title = ExtractTitle(title)
	m.current.UpdatedAt = time.Now()

	if err := m.store.UpdateMeta(m.current); err != nil {
		m.logger.Error(context.Background(), "session: failed to persist title", err)
	}
}

// SetThreadID records the channel ThreadID on the session for later lookup.
// Errors are logged internally — this is a best-effort metadata write.
func (m *Manager) SetThreadID(threadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return
	}

	m.current.ThreadID = threadID
	m.current.UpdatedAt = time.Now()

	if err := m.store.UpdateMeta(m.current); err != nil {
		m.logger.Error(context.Background(), "session: failed to persist thread ID", err)
	}
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

// LoadSessionMessages loads all messages for a specific session by ID.
// Unlike LoadMessages(), this does not require the session to be current —
// it reads directly from the store. Used by TopicBackend's session fallback
// to retrieve recent user messages for temporal queries.
func (m *Manager) LoadSessionMessages(sessionID string) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.LoadMessages(sessionID)
}

// LoadImage reads an image blob the current session's records refer to. Scoped to the current
// session exactly as LoadMessages is, because that is how every caller converts a history: the
// records it is holding are the current session's, and a record is only meaningful next to the
// directory its references live in.
func (m *Manager) LoadImage(ref string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return nil, fmt.Errorf("no active session")
	}
	return m.store.LoadImage(m.current.ID, ref)
}

// LoadSubagentMessages loads all subagent messages for a session.
// Returns a map of subagentID → messages. Returns empty map if no subagents exist.
//
// The root comes from the store this manager writes through (BaseDir), so the transcripts it
// reads are the ones beside the records it holds.
func (m *Manager) LoadSubagentMessages(sessionID string) (map[string][]Message, error) {
	return LoadSubagentMessages(m.BaseDir(), sessionID)
}

// AppendAPIRequest records one LLM API call's request payload (system prompt
// + tool schemas) to the current session. Best-effort: errors are returned
// for the caller to log — recording must never break the agent loop.
func (m *Manager) AppendAPIRequest(req *APIRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return fmt.Errorf("no current session")
	}
	return m.store.AppendAPIRequest(m.current.ID, req)
}

// LoadAPIRequests reads all recorded API requests for a session by ID.
// Returns nil (no error) when the session has no api_requests.jsonl.
func (m *Manager) LoadAPIRequests(sessionID string) ([]APIRequest, error) {
	return m.store.LoadAPIRequests(sessionID)
}

// Delete deletes a session by ID, on the owner's explicit instruction.
//
// A session a TURN is writing into is REFUSED, and the refusal names the task so the reader knows
// what to end first (or to stop, when it will not end by itself: the desktop's banner button,
// node's `session:end`). The decision is made under the coordination lock and against the
// persisted holds, not against this process's memory: the same check has to stop a desktop
// delete, an ACP delete and a count-based sweep in another process, and only a shared, persisted
// answer can (design §9.4: 拒绝判定落在共享保留/删除层).
//
// Everything ELSE about a session's life is the session store's own business — the count-based
// cleanup, the user's pin, this delete. A remote conversation used to be an exception, held for a
// retention window of its own; it is not any more (session/retention.go's ReasonTask comment).
func (m *Manager) Delete(id string) error {
	if m.retention == nil {
		return m.store.DeleteSession(id)
	}
	var refusal error
	err := m.retention.Lock(func() error {
		if err := m.retention.protected(id); err != nil {
			refusal = err
			return nil
		}
		return m.store.DeleteSession(id)
	})
	if err != nil {
		return err
	}
	if refusal == nil {
		m.clearRetiredLocked(id)
	}
	return refusal
}

// clearRetiredLocked drops what is left of a session's record once the session itself is gone.
//
// The decision that allowed the delete proved nothing LIVE was holding it, so anything still on
// the record is a retired reason (session/retention.go's ReasonContextRetired). With the session
// gone that record names a directory which does not exist, and leaving it for the owner's next
// startup to migrate is the kind of tidiness that turns into a mystery file in the data dir: the
// delete is the moment the record stops having a subject.
//
// Callers hold the coordination lock.
func (m *Manager) clearRetiredLocked(id string) {
	if m.retention == nil {
		return
	}
	if err := m.retention.clearLocked(id); err != nil {
		m.logger.Error(context.Background(),
			"session: could not clear a deleted session's retention record", err, "id", id)
	}
}

// UpdateMeta persists the given session's metadata. It is safe to call on
// non-current sessions (e.g., updating compacted_child_id on the old session
// after /compact). The Manager's current session pointer is NOT updated.
func (m *Manager) UpdateMeta(session *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session == nil {
		return fmt.Errorf("no session provided")
	}
	return m.store.UpdateMeta(session)
}

// TruncateTo rewinds the session's recorded history to a boundary: it keeps the
// first keepMessages records of messages.jsonl and the first keepAPIRequests of
// api_requests.jsonl, moving the rest into sidecars tagged tag.
//
// The two files are cut TOGETHER, because the request log describes the
// conversation and must not outlive it. The usage ledger is deliberately left
// alone: those tokens were really spent, and a rewind is not a refund.
func (m *Manager) TruncateTo(keepMessages, keepAPIRequests int, tag string) (TruncateResult, TruncateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return TruncateResult{}, TruncateResult{}, fmt.Errorf("no active session")
	}
	msgs, err := m.store.TruncateMessages(m.current.ID, keepMessages, tag)
	if err != nil {
		return TruncateResult{}, TruncateResult{}, err
	}
	reqs, err := m.store.TruncateAPIRequests(m.current.ID, keepAPIRequests, tag)
	if err != nil {
		return msgs, TruncateResult{}, err
	}
	m.current.UpdatedAt = time.Now()
	// Best-effort, like the other metadata writes on this path: the history has
	// already moved, and failing here would report a rewind that did happen as
	// one that did not.
	_ = m.store.UpdateMeta(m.current)
	return msgs, reqs, nil
}

// sessionForker is the optional half of a Store: one that can copy a
// conversation's head into a new session. FileStore can; a test double that keeps
// its sessions in memory has nothing to copy into. Asked as an interface rather
// than added to Store so that "a store can be written for a test" stays cheap
// (the same way retention is reached — see NewManagerWithStore).
type sessionForker interface {
	ForkSession(src *Session, keepRecords, keepAPI int, title string) (*Session, error)
}

// Fork copies the CURRENT session's history up to a record count into a new
// session and returns that new session. The current session is not modified, and
// it stays current: a fork is a new conversation, and which session a manager is
// on is a decision its owner makes (the desktop loads the fork explicitly).
//
// The counts are records, not turns — see FileStore.ForkSession, which owns the
// copy and carries the reasoning for what a fork does and does not take with it.
func (m *Manager) Fork(keepRecords, keepAPI int, title string) (*Session, error) {
	src := m.Current()
	if src == nil {
		return nil, fmt.Errorf("no active session")
	}
	forker, ok := m.store.(sessionForker)
	if !ok {
		return nil, fmt.Errorf("this session store cannot fork a session")
	}
	return forker.ForkSession(src, keepRecords, keepAPI, title)
}
