package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/session"
)

// fakeSessionManager is an in-memory SessionManager for testing.
// It avoids real file I/O and supports error injection for testing
// failure paths that are difficult to trigger with the real manager.
type fakeSessionManager struct {
	mu       sync.Mutex
	current  *session.Session
	messages []session.Message

	// apiRequests accumulates recorded API request payloads (system prompt
	// + tool schemas) for assertions in tests.
	apiRequests []session.APIRequest

	// truncations records what TruncateTo was asked for, so tests can pin the
	// cut points a rewind chose (the filesystem behaviour — sidecars and all —
	// is covered where it lives, in the session package).
	truncations []truncateCall

	// forks records what Fork was asked for (and, in forked, the head it handed
	// over), for the same reason: the agent's job is to resolve WHERE a fork cuts.
	forks  []forkCall
	forked []session.Message

	// appendErr, when non-nil, is returned by every AppendMessage call.
	// Set this to test how the agent loop handles session write failures.
	appendErr error

	// loadErr, when non-nil, is returned by every history READ (LoadMessages and
	// LoadAPIRequests). It is how a test simulates the shape a crash mid-append
	// leaves behind: one torn line makes the real store return nothing at all.
	loadErr error

	// others are sessions List returns besides the current one. It is how a test
	// models a conversation that MOVED ON: a compaction's successor names this
	// session as its parent (and may be the only side of that link that exists —
	// compact.go writes the predecessor's side best-effort).
	others []*session.Session

	// images answers LoadImage, keyed by blob reference. The fake holds records in memory, so
	// a record it wrote still carries its image inline (Part.Data) and a conversion never
	// reaches here; a test that wants the RELOAD shape — refs, and their bytes only in the
	// store — strips Data and fills this map. Leaving it nil is the other interesting case: a
	// reference whose blob is gone.
	images map[string][]byte
}

func (f *fakeSessionManager) HasCurrent() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current != nil
}

func (f *fakeSessionManager) Current() *session.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func (f *fakeSessionManager) SetCurrent(s *session.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = s
}

func (f *fakeSessionManager) New(providerName, workingDir string) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.current = &session.Session{
		ID:           fmt.Sprintf("fake-sess-%d", now.UnixNano()),
		ProviderName: providerName,
		WorkingDir:   workingDir,
		Title:        "",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	return f.current, nil
}

func (f *fakeSessionManager) Load(id string) (*session.Session, error) {
	return nil, fmt.Errorf("fakeSessionManager: Load not implemented")
}

func (f *fakeSessionManager) SetTitle(title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current != nil {
		f.current.Title = title
	}
}

func (f *fakeSessionManager) SetThreadID(threadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current != nil {
		f.current.ThreadID = threadID
	}
}

func (f *fakeSessionManager) EndCurrent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = nil
}

func (f *fakeSessionManager) AppendMessage(msg *session.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return f.appendErr
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	f.messages = append(f.messages, *msg)
	return nil
}

// AppendAPIRequest stores the request in memory (no file I/O in the fake).
func (f *fakeSessionManager) AppendAPIRequest(req *session.APIRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Timestamp.IsZero() {
		req.Timestamp = time.Now()
	}
	f.apiRequests = append(f.apiRequests, *req)
	return nil
}

// LoadAPIRequests returns a copy of the recorded requests.
func (f *fakeSessionManager) LoadAPIRequests(sessionID string) ([]session.APIRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	out := make([]session.APIRequest, len(f.apiRequests))
	copy(out, f.apiRequests)
	return out, nil
}

// truncateCall is one invocation of TruncateTo.
type truncateCall struct {
	KeepMessages int
	KeepAPI      int
	Tag          string
}

// TruncateTo mirrors the real manager's contract in memory: the tail is dropped
// from the fake's slices and reported as "removed". Tests that need the sidecar
// files themselves live in the session package, where the filesystem is real.
func (f *fakeSessionManager) TruncateTo(keepMessages, keepAPIRequests int, tag string) (session.TruncateResult, session.TruncateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.truncations = append(f.truncations, truncateCall{KeepMessages: keepMessages, KeepAPI: keepAPIRequests, Tag: tag})
	if f.current == nil {
		return session.TruncateResult{}, session.TruncateResult{}, fmt.Errorf("no active session")
	}
	cut := func(n, keep int) (session.TruncateResult, int) {
		if keep >= n {
			return session.TruncateResult{Kept: n}, n
		}
		return session.TruncateResult{Kept: keep, Removed: n - keep}, keep
	}
	msgs, keepM := cut(len(f.messages), keepMessages)
	reqs, keepR := cut(len(f.apiRequests), keepAPIRequests)
	f.messages = f.messages[:keepM]
	f.apiRequests = f.apiRequests[:keepR]
	return msgs, reqs, nil
}

// forkCall is one invocation of Fork.
type forkCall struct {
	KeepRecords int
	KeepAPI     int
	Title       string
}

// Fork mirrors the real manager's contract in memory: the current session's head
// becomes a new session, and the current one is left alone. Tests that need the
// copied BYTES (and the image blobs) live in the session package, where the
// filesystem is real.
func (f *fakeSessionManager) Fork(keepRecords, keepAPI int, title string) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forks = append(f.forks, forkCall{KeepRecords: keepRecords, KeepAPI: keepAPI, Title: title})
	if f.current == nil {
		return nil, fmt.Errorf("no active session")
	}
	head := func(n, keep int) int {
		if keep >= n {
			return n
		}
		return keep
	}
	keepM := head(len(f.messages), keepRecords)
	fork := &session.Session{ID: "fork-1", Title: title, WorkingDir: f.current.WorkingDir}
	forked := make([]session.Message, keepM)
	copy(forked, f.messages[:keepM])
	f.forked = forked
	return fork, nil
}

// AppendArtifact records the artifact as a reminder message (no merging —
// the fake is permissive; merge semantics are covered by the real manager).
func (f *fakeSessionManager) AppendArtifact(ref session.ArtifactRef) error {
	return f.AppendMessage(&session.Message{Type: session.MessageTypeReminder, Content: ref.Title + " " + ref.Path})
}

func (f *fakeSessionManager) LoadMessages() ([]session.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	out := make([]session.Message, len(f.messages))
	copy(out, f.messages)
	return out, nil
}

func (f *fakeSessionManager) LoadSessionMessages(sessionID string) ([]session.Message, error) {
	return f.LoadMessages()
}

// LoadImage answers a stored image blob (see the images field).
func (f *fakeSessionManager) LoadImage(ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if data, ok := f.images[ref]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("fakeSessionManager: no image blob %q", ref)
}

func (f *fakeSessionManager) List() ([]*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]*session.Session(nil), f.others...)
	if f.current != nil {
		out = append([]*session.Session{f.current}, out...)
	}
	return out, nil
}

func (f *fakeSessionManager) FindByThreadID(threadID string) (*session.Session, error) {
	return nil, nil
}

func (f *fakeSessionManager) UpdateMeta(s *session.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current != nil && f.current.ID == s.ID {
		f.current = s
	}
	return nil
}

func (f *fakeSessionManager) Delete(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current != nil && f.current.ID == id {
		f.current = nil
	}
	return nil
}

func (f *fakeSessionManager) LoadSubagentMessages(sessionID string) (map[string][]session.Message, error) {
	return nil, nil
}
