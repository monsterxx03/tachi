package agent

import (
	"github.com/monsterxx03/tachi/session"
)

// SessionManager defines the session operations used by AIAgent and its
// external callers (TUI, channel mode). The concrete *session.Manager
// satisfies this interface.
//
// Separating the interface from the concrete implementation lets tests inject
// a lightweight fake that avoids real file I/O and supports error injection.
type SessionManager interface {
	// HasCurrent returns true when there is an active session.
	HasCurrent() bool

	// Current returns the active session, or nil.
	Current() *session.Session

	// SetCurrent sets the current session (for loading existing sessions).
	SetCurrent(session *session.Session)

	// New creates and activates a new session with the given provider and
	// working directory. The previous current session (if any) is replaced.
	New(providerName, workingDir string) (*session.Session, error)

	// Load loads a session by ID and sets it as current.
	Load(id string) (*session.Session, error)

	// SetTitle updates the current session's title. Errors are non-fatal and
	// logged internally.
	SetTitle(title string)

	// SetThreadID records the channel ThreadID on the current session.
	SetThreadID(threadID string)

	// EndCurrent ends the current session. Session data is retained on disk.
	EndCurrent()

	// AppendMessage records a message on the current session.
	AppendMessage(msg *session.Message) error

	// LoadMessages loads all messages for the current session.
	LoadMessages() ([]session.Message, error)

	// LoadSessionMessages loads messages for a specific session by ID.
	LoadSessionMessages(sessionID string) ([]session.Message, error)

	// List returns all sessions sorted by created_at descending.
	List() ([]*session.Session, error)

	// FindByThreadID looks up a session by its ThreadID field.
	FindByThreadID(threadID string) (*session.Session, error)

	// UpdateMeta persists the given session's metadata.
	UpdateMeta(session *session.Session) error

	// Delete deletes a session by ID.
	Delete(id string) error

	// LoadSubagentMessages loads all subagent messages for a session.
	// Returns a map of subagentID → messages. Returns empty map if no
	// subagents exist.
	LoadSubagentMessages(sessionID string) (map[string][]session.Message, error)
}
