package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// AppendAPIRequest records one LLM API call's request payload (system
	// prompt + tool schemas) to the session's api_requests.jsonl.
	AppendAPIRequest(id string, req *APIRequest) error
	// LoadAPIRequests reads all recorded API requests for a session.
	// Returns nil (no error) when the session has no api_requests.jsonl.
	LoadAPIRequests(id string) ([]APIRequest, error)
	// TruncateMessages keeps the first keep records of a session's
	// messages.jsonl and moves the rest into a sidecar under tag.
	TruncateMessages(id string, keep int, tag string) (TruncateResult, error)
	// TruncateAPIRequests does the same for api_requests.jsonl. A rewind cuts
	// both files together: the request log describes the conversation.
	TruncateAPIRequests(id string, keep int, tag string) (TruncateResult, error)
}

// FileStore implements Store interface using filesystem
type FileStore struct {
	baseDir string

	// listCache memoizes the last completed session-list scan.
	//
	// Several callers need "every session" in the same breath and for different
	// reasons — the desktop asks for the sidebar's rows, then for the per-project
	// counts, then for a project's members, all through separate calls — and each
	// of them used to walk the base directory and re-read every meta.json
	// (measured: ~49ms for 1000 sessions, essentially all of it open/read/close).
	// This holds one scan so the repeats are free.
	//
	// Correctness rests on three things:
	//   - writes drop it (invalidate): a meta.json rewritten in place — title,
	//     UpdatedAt — does NOT change the base directory's mtime, so the stamp
	//     below cannot see it;
	//   - reads re-stat baseDir and drop it when the mtime moved: that is what
	//     covers a session created or DELETED by another process;
	//   - a scan publishes only if no write landed while it ran (gen), so a
	//     snapshot taken before a concurrent write can never be installed.
	//
	// What survives all three is another process rewriting an EXISTING meta.json
	// in place: the snapshot then lasts until the next create, delete or baseDir
	// change. Only the title and timestamps can go stale through that door —
	// project membership cannot, because ProjectID is written by the desktop alone
	// (bind on create, detach on project delete, inherit on compaction) and every
	// one of those writes goes through a store that invalidates.
	mu       sync.Mutex
	cached   []*Session
	cacheDir time.Time // baseDir's mtime when cached was read
	cacheOK  bool
	// gen counts writes, so a scan can tell whether the store changed under it.
	gen uint64
}

// NewFileStore creates a new FileStore
func NewFileStore(baseDir string) (*FileStore, error) {
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	return &FileStore{baseDir: baseDir}, nil
}

// invalidate drops the memoized session list. Every method that can change what
// ListSessions returns — CreateSession, UpdateMeta, DeleteSession — must call it,
// and must call it AFTER the write, never before: a scan that starts in the gap
// between an early invalidate and the write itself would read the old content and
// still be published, since no write landed during it.
//
// Dropping a snapshot that did not need dropping costs a bool — the next
// ListSessions is what pays for the rebuild — so when in doubt, drop it.
func (s *FileStore) invalidate() {
	s.mu.Lock()
	s.gen++
	s.cacheOK = false
	s.mu.Unlock()
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

func (s *FileStore) apiRequestsPath(id string) string {
	return filepath.Join(s.sessionDir(id), "api_requests.jsonl")
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
	s.invalidate()
	return nil
}

// LoadMeta loads meta.json for a session.
//
// It reads the file on every call, deliberately: this is what feeds the LIVE
// session — the manager's current, and the read half of every metadata
// read-modify-write (desktop's updateSessionMeta, which rewrites the whole
// meta.json and would clobber anything a snapshot had missed in between). Only
// ListSessions is memoized (see FileStore); serving this from that snapshot would
// turn a stale read into a lost write.
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

// CountMessages streams a session's messages.jsonl and returns the total
// message count plus the number of tool_call messages — the two numbers the
// session-list UI needs. Unlike LoadMessages it never unmarshals full
// messages (a single huge tool result can be megabytes), so it stays cheap
// for large histories and is safe to call per list page.
//
// The type field is always the first JSON key of a serialized Message
// (struct field order), so a line-level prefix check is exact — no
// false positives from content text. Best-effort: malformed or unreadable
// lines just stop the scan early with what was counted so far.
func (s *FileStore) CountMessages(id string) (total, toolCalls int) {
	f, err := os.Open(s.messagesPath(id))
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	const toolCallPrefix = `{"type":"tool_call"`
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		total++
		if bytes.HasPrefix(line, []byte(toolCallPrefix)) {
			toolCalls++
		}
	}
	return total, toolCalls
}

// AppendAPIRequest appends one API request record to api_requests.jsonl.
// The file is created on first write; missing session directories surface
// as an error so callers can log (recording is best-effort by design).
func (s *FileStore) AppendAPIRequest(id string, req *APIRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal api request: %w", err)
	}

	f, err := os.OpenFile(s.apiRequestsPath(id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open api_requests.jsonl: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write api request: %w", err)
	}

	return nil
}

// LoadAPIRequests reads all API request records for a session. Missing file
// (no requests recorded yet) returns nil, nil. Malformed lines are skipped —
// this is auxiliary debug data, and a single bad line must not break report
// generation.
func (s *FileStore) LoadAPIRequests(id string) ([]APIRequest, error) {
	f, err := os.Open(s.apiRequestsPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open api_requests.jsonl: %w", err)
	}
	defer f.Close()

	var reqs []APIRequest
	scanner := bufio.NewScanner(f)
	// Generous buffer: system prompts and tool schemas can be large.
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req APIRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue // skip malformed line
		}
		reqs = append(reqs, req)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan api requests: %w", err)
	}

	return reqs, nil
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
	s.invalidate()
	return nil
}

// ListSessions returns all sessions sorted by created_at descending.
//
// It serves the memoized snapshot when one is valid and scans otherwise; either
// way the caller gets its own copy (see cloneSessions).
func (s *FileStore) ListSessions() ([]*Session, error) {
	if cached, ok := s.cachedList(); ok {
		return cached, nil
	}

	// The generation is read BEFORE the scan and re-checked before publishing: a
	// write that lands mid-scan must leave the cache empty rather than hand the
	// next reader a snapshot taken from under it.
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()

	sessions, err := s.scanSessions()
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(s.baseDir)
	if err != nil {
		// An unreadable stamp means the snapshot could never be validated, so
		// serve this scan uncached instead of caching something unverifiable.
		return sessions, nil
	}
	s.mu.Lock()
	if s.gen == gen {
		s.cached, s.cacheDir, s.cacheOK = sessions, info.ModTime(), true
	}
	s.mu.Unlock()

	return cloneSessions(sessions), nil
}

// cachedList returns a copy of the memoized list when it is still valid. The base
// directory's mtime is the whole filesystem check: one stat, and it catches
// exactly what our own writes cannot — a session created or deleted by another
// process (both move the directory's mtime). See FileStore for what it misses.
func (s *FileStore) cachedList() ([]*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cacheOK {
		return nil, false
	}
	info, err := os.Stat(s.baseDir)
	if err != nil || !info.ModTime().Equal(s.cacheDir) {
		return nil, false
	}
	return cloneSessions(s.cached), true
}

// scanSessions reads every session's meta.json under the base directory. A
// directory that does not parse is skipped, exactly as it always was.
func (s *FileStore) scanSessions() ([]*Session, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, fmt.Errorf("read session dir: %w", err)
	}

	var sessions []*Session
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		session, err := s.LoadMeta(entry.Name())
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

// cloneSessions returns copies of the entries, because a snapshot outlives the
// call that received it: a caller may mutate what it was handed (the session
// manager loads one into its current field and edits it in place) and the next
// reader must not see that. AdditionalDirs is the only reference field.
func cloneSessions(in []*Session) []*Session {
	if in == nil {
		return nil
	}
	out := make([]*Session, 0, len(in))
	for _, s := range in {
		c := *s
		c.AdditionalDirs = slices.Clone(s.AdditionalDirs)
		out = append(out, &c)
	}
	return out
}

// DeleteSession removes a session directory
func (s *FileStore) DeleteSession(id string) error {
	dir := s.sessionDir(id)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	s.invalidate()
	return nil
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

// TruncateResult is what a truncation kept, and where the removed tail went.
//
// The tail is never deleted: it is moved to a sidecar beside the session so the
// abandoned branch survives the rewind (an audit trail, and the raw material a
// future /branch would read). Kept is the number of records left in place.
type TruncateResult struct {
	Kept    int
	Removed int
	Sidecar string // empty when nothing was removed
}

// TruncateMessages keeps the first keep records of a session's messages.jsonl
// and moves the rest into the session's rewound/ directory under tag.
func (s *FileStore) TruncateMessages(id string, keep int, tag string) (TruncateResult, error) {
	return truncateJSONL(s.messagesPath(id), filepath.Join(s.sessionDir(id), rewoundDirName, "messages-"+tag+".jsonl"), keep)
}

// TruncateAPIRequests does the same for api_requests.jsonl. Both files are cut
// together: a request log describing turns the conversation no longer has would
// make the request panel and any cache investigation read a history that is not
// there.
func (s *FileStore) TruncateAPIRequests(id string, keep int, tag string) (TruncateResult, error) {
	return truncateJSONL(s.apiRequestsPath(id), filepath.Join(s.sessionDir(id), rewoundDirName, "api-"+tag+".jsonl"), keep)
}

// rewoundDirName holds the tails of rewound histories, beside the subagent/ and
// oneoff/ transcripts the session already keeps. It lives INSIDE the session
// directory on purpose: a rewind's discarded branch belongs to the session it
// was cut from, and dies with it.
const rewoundDirName = "rewound"

// truncateJSONL keeps the first keep lines of src and moves the remainder to
// dst, byte for byte.
//
// The order is deliberate: the sidecar is written BEFORE the source is
// rewritten, so a crash in between leaves the records duplicated rather than
// lost. A rewind can be retried; a lost branch cannot.
func truncateJSONL(src, dst string, keep int) (TruncateResult, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return TruncateResult{}, nil // nothing recorded yet
		}
		return TruncateResult{}, fmt.Errorf("read %s: %w", filepath.Base(src), err)
	}

	offset, total := 0, 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		total++
		if total == keep {
			offset = i + 1
		}
	}
	// A last line without a trailing newline still counts as a record.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
		if total <= keep {
			offset = len(data)
		}
	}
	if total <= keep {
		return TruncateResult{Kept: total}, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return TruncateResult{}, fmt.Errorf("create rewound dir: %w", err)
	}
	if err := fileutil.AtomicWriteFilePrivate(dst, data[offset:]); err != nil {
		return TruncateResult{}, fmt.Errorf("write sidecar: %w", err)
	}
	if err := fileutil.AtomicWriteFilePrivate(src, data[:offset]); err != nil {
		return TruncateResult{}, fmt.Errorf("rewrite %s: %w", filepath.Base(src), err)
	}
	return TruncateResult{Kept: keep, Removed: total - keep, Sidecar: dst}, nil
}
