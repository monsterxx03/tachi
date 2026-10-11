package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The context binding: which Tachi session a remote conversation (an A2A context) is
// being held in.
//
// One file per context, same discipline as the task records — it is a fact about the
// node, it must survive a restart, and the node is its only writer:
//
//	<DataDir>/contexts/<contextID>.json
//
// The design keeps TWO associations apart and this is one of them (§7.1.1): the context
// owns a session (this file), while a task merely references a context. That is what lets
// one peer hold several isolated conversations, and what lets a follow-up task join the
// history of the previous one without any of them being "the peer's session".
type contextBinding struct {
	ContextID string `json:"context_id"`
	// CallerID is the authenticated principal that opened this context, empty on loopback,
	// where nobody is identified. It is what refuses a context to a caller that is not its
	// owner (the same granularity rule as the dedupe key) — set from the TLS handshake's
	// identity, so it cannot be claimed (node/auth.go).
	CallerID string `json:"caller_id,omitempty"`
	// SessionID is the session the context is held in RIGHT NOW. Compaction moves the
	// conversation to a successor session, and this field follows it — the context id and
	// the task ids never change. The node's retention hold follows with it (see
	// runner.onSessionSwitch), which is why the move happens while the turn is running
	// rather than at the next request.
	SessionID string `json:"session_id"`
	// Workspace is the working directory this context was opened in, recorded so a later
	// task in the same conversation runs in the same place even if the node's config has
	// since changed (a conversation cannot move half-way through).
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const contextsDirName = "contexts"

type contextStore struct {
	dir string

	mu       sync.Mutex
	bindings map[string]*contextBinding
}

func openContextStore(dir string) (*contextStore, error) {
	s := &contextStore{dir: dir, bindings: make(map[string]*contextBinding)}
	if err := os.MkdirAll(dir, dataDirPerm); err != nil {
		return nil, fmt.Errorf("node: create context store %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("node: read context store %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var b contextBinding
		path := filepath.Join(dir, e.Name())
		if err := fileutil.ReadJSON(path, &b); err != nil {
			return nil, fmt.Errorf("node: read context binding %s: %w", path, err)
		}
		if b.ContextID == "" || b.SessionID == "" {
			return nil, fmt.Errorf("node: context binding %s is incomplete", path)
		}
		s.bindings[b.ContextID] = &b
	}
	return s, nil
}

// get returns a copy: a caller must never hold a pointer into the store's state.
func (s *contextStore) get(contextID string) (*contextBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[contextID]
	if !ok {
		return nil, false
	}
	out := *b
	return &out, true
}

// bySession returns the binding whose session is this one right now, and nothing else.
//
// It is the lookup an ENTRY POINT can make: a reader sees a session id (a row, a window), not a
// context id, and asking "is anything running in this one" has to start from what they have.
// A conversation's binding FOLLOWS it — compaction re-points it at the successor — so the
// current session is the only one that matches. An ancestor of a compacted conversation
// therefore matches nothing, which is the truth: the work moved to the successor, and its
// holder is that session's.
func (s *contextStore) bySession(sessionID string) (*contextBinding, bool) {
	if sessionID == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		if b.SessionID != sessionID {
			continue
		}
		out := *b
		return &out, true
	}
	return nil, false
}

func (s *contextStore) put(b *contextBinding) error {
	if b == nil || b.ContextID == "" {
		return errors.New("node: refusing to store a context binding without a context id")
	}
	if err := checkRecordID("context", b.ContextID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	b.UpdatedAt = time.Now().UTC()
	path := filepath.Join(s.dir, b.ContextID+".json")
	if err := fileutil.AtomicWriteJSONPrivate(path, b); err != nil {
		return fmt.Errorf("node: write context binding %s: %w", b.ContextID, err)
	}
	stored := *b
	s.bindings[b.ContextID] = &stored
	return nil
}
