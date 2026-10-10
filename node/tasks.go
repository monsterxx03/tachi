package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The task store: this node's authoritative record of every task it accepted.
//
// Layout, all inside the node's data dir:
//
//	<DataDir>/tasks/<taskID>.json   the AUTHORITY: the task snapshot, its version, and
//	                                the fingerprint of the request that created it
//	<DataDir>/tasks/index.json      DERIVED: fingerprint -> taskID, rebuilt by scanning
//	                                the records whenever it is missing or stale
//
// One file per task, one writer: the node holds the single-instance lock, so no other
// process writes here and the store needs no cross-process locking. Within the process
// every mutation writes the record through to disk BEFORE it is visible — a task that
// was answered is on disk.
//
// The index is a cache and nothing else. It exists to make the replay check cheap, and
// because a lookup falls back to scanning the records, a crash between the two writes
// costs latency, never correctness. That is why the design lets derived indexes be
// rebuilt instead of pretending the two files are one transaction.
const (
	tasksDirName = "tasks"
	// maxRecordIDLen bounds an id used as a filename (a task's or a context's): generous
	// for a UUID and well under the 255-byte component limit of the filesystems we target.
	maxRecordIDLen = 128
	// maxTaskIDLen bounds a task id used as a filename. Generous for a UUID and well
	// under the 255-byte component limit of the filesystems we target.
	maxTaskIDLen    = 128
	indexFileName   = "index.json"
	interruptReason = "the hosting process was interrupted; side effects before that point are unknown"
)

// taskRecord is what we persist for one accepted task.
type taskRecord struct {
	Task      *a2a.Task `json:"task"`
	Version   int64     `json:"version"`
	MessageID string    `json:"message_id"`
	// Digest is the fingerprint of the message that created this task: the same
	// messageId with DIFFERENT content is a client bug we must refuse, and this is what
	// lets us tell the two apart instead of silently returning the old task.
	Digest string `json:"digest"`
	// CallerID is the authenticated identity that submitted this task, empty for a request with
	// no credential (loopback, or a node that configures no callers). It is the OWNER as well as
	// part of the dedupe key (see fingerprint.key): List filters by it, and Get refuses a caller
	// that is not it — an id is not a capability (design §6.3). A record written before this
	// node had callers has no owner, and nobody can claim one.
	CallerID string `json:"caller_id,omitempty"`
	// SelfReportedName is the name the caller declared for itself (node.name, sent as a header).
	// The FIELD NAME says what it is: anyone with a valid key can claim any name, so this is a
	// label for a human reading the record and never an identity — CallerID, which is derived from
	// the key the handshake accepted, is the identity (design §2.2).
	SelfReportedName string         `json:"self_reported_name,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	Interrupt        *interruptNote `json:"interrupt,omitempty"`
}

// interruptNote records why a task was resolved as interrupted at startup.
type interruptNote struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// fingerprint identifies the request that created a task.
type fingerprint struct {
	caller    string
	messageID string
	digest    string
}

// dedupeKeySep joins the two halves of a dedupe key. It must not occur inside a caller
// identity: `a|b` + `c` and `a` + `b|c` would otherwise be the same key, and two callers
// sharing a key means the second one is answered with the FIRST one's task — a
// cross-principal leak wearing the costume of an idempotent replay. Callers are ids we
// mint, so this is a guard, not a parser: it is rejected at write time.
const dedupeKeySep = "|"

// key is the dedupe key. With no caller identity yet it is the messageId alone, and the
// day callers can be authenticated it becomes (caller, messageId) by filling CallerID —
// no format change, no migration.
//
// The caller half must be at least as fine-grained as the credential: anything that can
// send a messageId of its own has to be distinguishable here, or one principal would be
// served the other's task.
func (f fingerprint) key() string {
	if f.caller == "" {
		return f.messageID
	}
	return f.caller + dedupeKeySep + f.messageID
}

// digestMessage hashes the message as it arrived. The SDK's own marshalling is the
// canonical form here (encoding/json sorts map keys, so metadata cannot reorder it): a
// replay that sends the same bytes hashes the same, and one field of difference does not.
func digestMessage(msg *a2a.Message) string {
	raw, err := json.Marshal(msg)
	if err != nil {
		// A message that cannot be marshalled cannot have been accepted either.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type indexDoc struct {
	Entries map[string]string `json:"entries"`
}

// taskStore holds the loaded task set and the derived index.
type taskStore struct {
	dir string

	mu    sync.Mutex
	tasks map[a2a.TaskID]*taskRecord
	index map[string]string
}

// openTaskStore loads the node's tasks, rebuilds the derived index, and resolves every
// task that was still running when the process died.
//
// The recovery pass is the design's rule made concrete: an execution cannot be resumed
// across processes, so a non-terminal task does not stay "running" in a record nobody
// will ever finish — it becomes FAILED, with the reason saying that the hosting process
// was interrupted and that side effects are unknown. Reporting it as running would be
// the record lying about a process that no longer exists.
func openTaskStore(dir string) (*taskStore, error) {
	s := &taskStore{
		dir:   dir,
		tasks: make(map[a2a.TaskID]*taskRecord),
		index: make(map[string]string),
	}
	if err := os.MkdirAll(dir, dataDirPerm); err != nil {
		return nil, fmt.Errorf("node: create task store %s: %w", dir, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("node: read task store %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || e.Name() == indexFileName {
			continue
		}
		var rec taskRecord
		path := filepath.Join(dir, e.Name())
		if err := fileutil.ReadJSON(path, &rec); err != nil {
			// A record we cannot read is not a record we may drop: refusing to start is
			// the only answer that cannot lose a task silently.
			return nil, fmt.Errorf("node: read task record %s: %w", path, err)
		}
		if rec.Task == nil || rec.Task.ID == "" {
			return nil, fmt.Errorf("node: task record %s has no task", path)
		}
		s.tasks[rec.Task.ID] = &rec
		s.reindex(&rec)
	}

	if err := s.resolveInterrupted(); err != nil {
		return nil, err
	}
	return s, nil
}

// reindex adds a record's fingerprint to the derived index.
func (s *taskStore) reindex(rec *taskRecord) {
	if rec.MessageID == "" {
		return
	}
	s.index[fingerprint{caller: rec.CallerID, messageID: rec.MessageID}.key()] = string(rec.Task.ID)
}

// resolveInterrupted fails every task that was mid-flight when the previous process
// stopped — running OR parked on a question.
//
// A parked task belongs here for the same reason a running one does (§7.4): the goroutine
// that was waiting for the answer did not survive the process, so nothing can receive that
// answer any more. It is marked FAILED rather than left in INPUT_REQUIRED (which would claim
// somebody is still waiting) or quietly re-armed as if waiting were resumable — and the
// QUESTION is kept in the status, because "the node died before this was answered" is only
// useful alongside what was asked.
func (s *taskStore) resolveInterrupted() error {
	now := time.Now().UTC()
	for _, rec := range s.tasks {
		if rec.Task.Status.State.Terminal() {
			continue
		}
		rec.Interrupt = &interruptNote{At: now, Reason: interruptReason}
		s.endUnfinished(rec, interruptReason, now)
		if err := s.writeRecord(rec); err != nil {
			return err
		}
	}
	// Whatever we changed has to be in the index's view too; a full rewrite is the
	// cheapest way to keep the derived file honest.
	return s.writeIndex()
}

// failQuestion ends a task whose question was never answered.
//
// It is the deadline's half of parking: the state must not keep saying INPUT_REQUIRED, which
// means "a turn in this process is waiting for the answer". The task is FAILED, and the
// question stays in its status so the record still says what was asked.
func (s *taskStore) failQuestion(taskID a2a.TaskID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("%w: %s", a2a.ErrTaskNotFound, taskID)
	}
	if rec.Task.Status.State.Terminal() {
		// Someone else already ended it — an answer that arrived first, or a cancel. Ending
		// it again would only overwrite a state that is already the truth.
		return nil
	}
	s.endUnfinished(rec, reason, time.Now().UTC())
	return s.putLocked(rec)
}

// endUnfinished writes the status of a task that will not finish, keeping the question it was
// last waiting on.
//
// Callers hold the store's lock. The reason comes first and the question after it: the record
// is read by whoever comes looking for what happened, and what happened is the reason.
func (s *taskStore) endUnfinished(rec *taskRecord, reason string, now time.Time) {
	text := reason
	if question := messageText(rec.Task.Status.Message); question != "" {
		text += "\n\n" + question
	}
	rec.Task.Status = a2a.TaskStatus{
		State:   a2a.TaskStateFailed,
		Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text)),
	}
	rec.Version++
	rec.UpdatedAt = now
}

// parkedTaskIn returns the task in this context that is waiting for an answer.
//
// It is the sibling of contextIsBusy, and it exists because "not busy" is not the same as
// "open for new work": a context whose task is PARKED accepts one thing only — the answer to
// that task's question (design §7.1.2).
func (s *taskStore) parkedTaskIn(contextID string) (*taskRecord, bool) {
	if contextID == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.tasks {
		if rec.Task.ContextID != contextID {
			continue
		}
		if rec.Task.Status.State == a2a.TaskStateInputRequired {
			return rec.copy(), true
		}
	}
	return nil, false
}

// copy returns a snapshot that is safe to hand to a caller.
//
// The store is the single writer, which is exactly why a reader must never hold a pointer
// INTO its state: Update replaces a record's task wholesale, and a caller holding the old
// pointer would read a value that is being changed under it (a real data race, caught by
// -race the first time a parked task was read while its status changed). Copying the task
// as well keeps a reader's view consistent instead of pointing at a half-applied update.
func (rec *taskRecord) copy() *taskRecord {
	if rec == nil {
		return nil
	}
	out := *rec
	if rec.Task != nil {
		task := *rec.Task
		out.Task = &task
	}
	return &out
}

// lookup returns a snapshot of the record created by this fingerprint, or false.
func (s *taskStore) lookup(fp fingerprint) (*taskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.lookupLocked(fp)
	return rec.copy(), ok
}

func (s *taskStore) lookupLocked(fp fingerprint) (*taskRecord, bool) {
	if fp.messageID == "" {
		return nil, false
	}
	// The index names the record; whether the CONTENT matches is a separate question
	// (see matches), because "found" and "the same request" are not the same answer.
	if id, ok := s.index[fp.key()]; ok {
		if rec, ok := s.tasks[a2a.TaskID(id)]; ok {
			return rec, true
		}
	}
	// The index can be stale or missing; the records decide.
	for _, rec := range s.tasks {
		if rec.MessageID == fp.messageID && rec.CallerID == fp.caller {
			return rec, true
		}
	}
	return nil, false
}

// matches reports whether this record was created by that request.
//
// An empty digest on either side means the content could not be compared, and the honest
// answer there is "same request": claiming a difference we cannot see would refuse work
// for no reason.
func (rec *taskRecord) matches(fp fingerprint) bool {
	if rec.Digest == "" || fp.digest == "" {
		return true
	}
	return rec.Digest == fp.digest
}

// contextIsBusy reports whether this context has a task that is EXECUTING — the one
// condition under which a NEW work request is refused (design §7.1.1).
//
// "Executing" excludes the interrupted states on purpose. A task in INPUT_REQUIRED is not
// busy: it is PARKED, and the message that answers its question is exactly the message this
// context is waiting for. Counting it as busy would refuse that answer — the caller locked
// out of the conversation the node just asked it to continue — which is the opposite of
// what the rule is for.
func (s *taskStore) contextIsBusy(contextID string) bool {
	if contextID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.tasks {
		if rec.Task.ContextID != contextID {
			continue
		}
		switch rec.Task.Status.State {
		case a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired:
			continue // parked: the next message is the answer it is waiting for
		}
		if !rec.Task.Status.State.Terminal() {
			return true
		}
	}
	return false
}

// record returns a snapshot of a task's record by id.
func (s *taskStore) record(id a2a.TaskID) (*taskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[id]
	return rec.copy(), ok
}

// snapshot returns every record, newest first.
func (s *taskStore) snapshot() []*taskRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*taskRecord, 0, len(s.tasks))
	for _, rec := range s.tasks {
		out = append(out, rec.copy())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *taskStore) putLocked(rec *taskRecord) error {
	if err := s.writeRecord(rec); err != nil {
		return err
	}
	s.tasks[rec.Task.ID] = rec
	s.reindex(rec)
	return s.writeIndex()
}

// checkRecordID refuses an id that cannot be one file name.
//
// Both ids this node turns into filenames — a task's and a context's — arrive from outside
// the package (the request handler generates a task id, and a message may carry a context
// id to continue a conversation). Today the ones we write are server-generated UUIDs, so
// this is a guard rather than a parser — but the failure it prevents is a write escaping
// the directory we own, and that is not a failure worth being merely unlikely.
//
// fileutil.IsSinglePathElement is the house rule for exactly this crossing; the length
// bound is the other half of it, because a filesystem component is 255 bytes on the
// platforms we target and an unbounded id would surface as an opaque write error instead
// of a clear refusal.
func checkRecordID(kind, id string) error {
	if !fileutil.IsSinglePathElement(id) {
		return fmt.Errorf("node: refusing %s id %q: not a single path element", kind, id)
	}
	if len(id) > maxRecordIDLen {
		return fmt.Errorf("node: refusing %s id of %d bytes (limit %d)", kind, len(id), maxRecordIDLen)
	}
	return nil
}

func (s *taskStore) path(id a2a.TaskID) string {
	return filepath.Join(s.dir, string(id)+".json")
}

func (s *taskStore) writeRecord(rec *taskRecord) error {
	if rec.Task == nil || rec.Task.ID == "" {
		return errors.New("node: refusing to write a task record without an id")
	}
	if err := checkRecordID("task", string(rec.Task.ID)); err != nil {
		return err
	}
	if err := fileutil.AtomicWriteJSONPrivate(s.path(rec.Task.ID), rec); err != nil {
		return fmt.Errorf("node: write task %s: %w", rec.Task.ID, err)
	}
	return nil
}

func (s *taskStore) writeIndex() error {
	doc := indexDoc{Entries: make(map[string]string, len(s.index))}
	for k, v := range s.index {
		doc.Entries[k] = v
	}
	if err := fileutil.AtomicWriteJSONPrivate(filepath.Join(s.dir, indexFileName), &doc); err != nil {
		return fmt.Errorf("node: write task index: %w", err)
	}
	return nil
}
