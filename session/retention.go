package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/flock"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Retention: the one answer to "may this session be deleted?" (design §9.4).
//
// The count-based cleanup keeps the CURRENT session of the manager that runs it, plus the
// user's pins and the channel thread bindings. None of those is a promise another process can
// rely on: a receiving node's session is written by a turn living in ANOTHER process, whose
// idea of "current" means nothing to a sweep in the desktop, and a session can be pinned
// precisely because nobody looks at it. So the reason a session must survive is PERSISTED,
// in a management directory no session cleanup touches, and every deletion path asks the same
// place.
//
// Why not reuse a field that is already there: `Pinned` is the READER's statement that a
// conversation stays at hand, and lifting a pin is a UI decision — using it for a running
// task would mean the user unpinning a conversation silently drops a task's session (and, the
// other way round, that a task keeps a session the user pinned for unrelated reasons).
// `ThreadID` is a channel's lookup key, and `Kind` says what a session IS, not how long it
// has to live: a finished remote conversation is still a remote conversation.
//
// The record is per session and per REFERENCE: a hold names (owner, ref), and releasing it names
// the same pair, so "release" can never mean "release everything holding this session". The owner
// is the subsystem that took the hold — this node is one owner among however many share the data
// dir — and the ref is one execution, so an execution ending says nothing about another one still
// running in the same session, nor about anybody else's hold.
//
// Layout, inside the session data dir:
//
//	<sessionDir>/retention/retention.lock   the coordination lock (see Lock)
//	<sessionDir>/retention/holds/<id>.json  one session's holds
//
// The lock file is not the node's lifecycle lock and never shares its path: a node being
// online must not stop another process from cleaning up legally (design §9.4), and the two
// locks are held for completely different spans. Like every flock in this repo the file is
// never removed on release — the mutual exclusion IS the file's identity.
type Retention struct {
	// sessionDir is the store's base directory: the retention record refers to sessions by
	// the directory name that holds them, so this layer and the store agree by construction.
	sessionDir string
	dir        string
	log        *logger.Logger
}

const (
	retentionDirName  = "retention"
	holdsDirName      = "holds"
	retentionLockName = "retention.lock"

	// holdFilePerm / holdDirPerm: a hold is private bookkeeping, like everything else in the
	// data dir.
	holdFilePerm os.FileMode = 0o600
	holdDirPerm  os.FileMode = 0o700

	// lockWaitBudget bounds waiting for the coordination lock. The critical sections it guards
	// are file reads and one delete, so anything slower is a wedged holder rather than a busy
	// one — and a cleanup that gives up loudly beats a process stuck forever behind a peer
	// that crashed mid-decision. (The OS releases a dead holder's flock, so this budget
	// normally waits out nothing but a genuinely concurrent operation.)
	lockWaitBudget = 5 * time.Second
)

// Reasons a hold exists. They are stored in the record (an operator reading the file should
// see why), and they are what a refusal quotes to the user.
//
// There is ONE reason a hold may be WRITTEN for, and that is the shape the design landed on: a
// hold means an EXECUTION is running in this session right now. A remote conversation used to hold
// its session for a retention window of its own, which made the retention layer a second clock
// beside the session store's — two answers to "how long does this live", and two rules for whether
// a delete may go through. What is left is the session's own lifecycle (count-based cleanup, pins,
// an explicit delete) for everything else, and this one fact for the thing that genuinely cannot
// lose its session. The reason that went — a conversation's own window — is named below rather than
// forgotten: a record an older version left behind is still something this version has to answer
// for.
const (
	// ReasonTask: a task needs this session until it reaches a terminal state.
	ReasonTask = "task"

	// ReasonContextRetired is the reason an older version wrote for "a remote conversation inside
	// its retention window". This version writes it for nothing and keeps nothing for it: the
	// window is gone (see above), so a hold carrying it protects nothing anybody can act on.
	//
	// It is NAMED rather than left to the unknown-reason path, and the difference is what the
	// reader sees. An unknown reason has to be kept (a newer version may be relying on it), so it
	// would surface as a refusal telling this machine's owner to end a task in a conversation that
	// has no task in it — a dead end. Naming it lets both ends be right: the questions this layer
	// ANSWERS ignore it (may this be deleted, is an execution running), and the node's startup
	// reconciliation RELEASES it, so a data dir converges on the one reason that exists
	// (node/runner.go's reconcile).
	ReasonContextRetired = "context"
)

// retiredReason reports whether a hold names a reason this version has retired: readable,
// understood, and no longer a reason to keep anything.
func retiredReason(reason string) bool { return reason == ReasonContextRetired }

// liveHolds drops the retired ones. It is what the QUESTIONS this layer answers must see, while
// the record on disk keeps them until its owner reconciles — dropping them silently at read time
// would hide a migration that has not happened yet.
func liveHolds(holds []Hold) []Hold {
	kept := make([]Hold, 0, len(holds))
	for _, h := range holds {
		if retiredReason(h.Reason) {
			continue
		}
		kept = append(kept, h)
	}
	return kept
}

// Hold is one reason a session must not be deleted.
type Hold struct {
	// Owner is the subsystem holding it. Ownership is recorded rather than assumed: several
	// owners will share this data dir, and a release must only ever drop one owner's hold.
	Owner string `json:"owner"`
	// Reason is what kind of thing is holding it (ReasonTask, today the only one).
	Reason string `json:"reason"`
	// Ref identifies the holder: a task id. Release is by (owner, ref), which is what makes
	// "one of them ended" expressible.
	Ref string `json:"ref"`
	// SessionID is the session the hold is written on. It is duplicated from the file name so
	// a record read on its own says what it is about; the file name stays the authority.
	SessionID string `json:"session_id"`
	// Since is when the hold was taken (diagnostics; also the honest answer to "why is this
	// still here").
	//
	// There is deliberately no deadline: a running execution has no predictable end, and a hold
	// that expired on the clock would release a session a turn is still writing into. What ends
	// this hold is the execution ending — or, when a process died mid-execution, the startup
	// reconciliation that owns this ref's lifetime (node/runner.go's reconcile).
	Since time.Time `json:"since"`
}

// Describe renders one hold as a short phrase for a refusal message.
func (h Hold) Describe() string {
	what := h.Reason
	if h.Reason == ReasonTask {
		what = "任务"
	}
	if h.Ref == "" {
		return what
	}
	return what + " " + h.Ref
}

// SessionHeldError is the refusal a delete gets for a session a running execution protects.
//
// It is a value rather than a bare error so the frontends can render the REASON (which task)
// instead of a generic failure: the reader's next action is to end that task — or, when it will
// not end by itself, to stop it (the desktop's banner button, node's `session:end`) — and a
// refusal that does not say which one is not actionable (design §9.4: the interfaces only display
// the reason, the decision lives here).
type SessionHeldError struct {
	SessionID string
	Holds     []Hold
}

func (e *SessionHeldError) Error() string {
	parts := make([]string, 0, len(e.Holds))
	for _, h := range e.Holds {
		parts = append(parts, h.Describe())
	}
	return fmt.Sprintf("会话 %s 仍在使用中（%s），请先结束对应任务再删除",
		e.SessionID, strings.Join(parts, "、"))
}

// NewRetention returns a handle onto the retention records under a session data dir.
//
// Several handles may exist (the store, a manager, a node), in one process or several: all
// the state is on disk and every decision that matters is made under the coordination lock,
// so a handle is nothing more than the path it works in.
func NewRetention(sessionDir string, log *logger.Logger) (*Retention, error) {
	if strings.TrimSpace(sessionDir) == "" {
		return nil, errors.New("session: a retention store needs the session data dir")
	}
	abs, err := filepath.Abs(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("session: resolve %s: %w", sessionDir, err)
	}
	if log == nil {
		log = logger.New("session")
	}
	return &Retention{sessionDir: abs, dir: filepath.Join(abs, retentionDirName), log: log}, nil
}

// Lock runs fn holding the coordination lock.
//
// Every decision that ends in a DELETION must be made inside it, and so must every change to
// a hold: "check the holds, then delete" and "create the session, then hold it" are each two
// steps, and without the lock another process slips between them (design §9.4). Enumeration —
// listing sessions, deciding which are candidates — belongs OUTSIDE, where it costs nothing.
//
// Callers must not hold an in-process lock when they call this: the fixed order is the FILE
// lock first, then the session manager's or store's own lock. Holding the coordination lock
// while taking an in-process lock is exactly that order; the reverse is the deadlock the
// design forbids.
func (r *Retention) Lock(fn func() error) error {
	if err := r.ensureDir(); err != nil {
		return err
	}
	lock, err := flock.Acquire(context.Background(), filepath.Join(r.dir, retentionLockName), lockWaitBudget)
	if err != nil {
		return fmt.Errorf("session: retention lock: %w", err)
	}
	defer func() { _ = lock.Release() }()
	return fn()
}

// Acquire adds a hold.
//
// It refuses when the session is not there: a caller that creates a session and protects it in
// two steps must be able to hear that the first step's result is gone, rather than write a
// hold for a session another process deleted in between and then work in a directory nobody
// owns (design §9.4: 既有 session 在获保护前须确认仍存在).
func (r *Retention) Acquire(h Hold) error {
	if h.SessionID == "" {
		return errors.New("session: a hold needs the session it is on")
	}
	if h.Owner == "" || h.Ref == "" {
		return errors.New("session: a hold needs an owner and a ref")
	}
	return r.Lock(func() error {
		if !r.sessionExists(h.SessionID) {
			return fmt.Errorf("session %s is gone; it cannot be protected", h.SessionID)
		}
		holds, err := r.read(h.SessionID)
		if err != nil {
			return err
		}
		if h.Since.IsZero() {
			h.Since = time.Now().UTC()
		}
		// Same (owner, ref) twice is not two reasons: a retry must not make the record grow.
		for i, existing := range holds {
			if existing.Owner == h.Owner && existing.Ref == h.Ref {
				holds[i] = h
				return r.writeLocked(h.SessionID, holds)
			}
		}
		return r.writeLocked(h.SessionID, append(holds, h))
	})
}

// Release drops one hold, identified by (owner, ref), and only that one: a task ending says
// nothing about a conversation that can still be continued, and vice versa.
//
// Releasing something that is not held is not an error: a crash and a restart both end up
// releasing the same holds twice.
func (r *Retention) Release(sessionID, owner, ref string) error {
	if sessionID == "" || owner == "" || ref == "" {
		return nil
	}
	return r.Lock(func() error { return r.releaseLocked(sessionID, owner, ref) })
}

// releaseLocked is Release with the coordination lock already held.
func (r *Retention) releaseLocked(sessionID, owner, ref string) error {
	holds, err := r.read(sessionID)
	if err != nil {
		return err
	}
	kept := holds[:0]
	for _, h := range holds {
		if h.Owner == owner && h.Ref == ref {
			continue
		}
		kept = append(kept, h)
	}
	if len(kept) == len(holds) {
		return nil
	}
	return r.writeLocked(sessionID, kept)
}

// clearLocked removes a session's whole record.
//
// It is what a DELETE leaves behind: the decision that allowed the delete proved nothing LIVE was
// holding the session, so whatever is still in the file is a retired reason — and with the session
// gone, that file names a directory which does not exist (see Manager.clearRetiredLocked).
//
// Callers hold the coordination lock.
func (r *Retention) clearLocked(sessionID string) error {
	return r.writeLocked(sessionID, nil)
}

// Held returns the holds that keep a session, which is what a refusal and a repair both need.
//
// It reads without the coordination lock: writes publish atomically, so a reader either sees
// the whole old record or the whole new one. Reading is not deciding — a deletion must go
// through Lock.
//
// It is the RECORD, not the answer: a retired reason is included here (that is what a repair has
// to see in order to release it), while the two questions this layer answers — may this be
// deleted, is an execution running — go through liveHolds first.
//
// Every hold in it is live: a hold ends by being RELEASED (the execution ended) or by the
// startup reconciliation that owns its ref, never by the clock (see Hold.Since).
func (r *Retention) Held(sessionID string) ([]Hold, error) {
	if sessionID == "" {
		return nil, nil
	}
	return r.read(sessionID)
}

// BusySessions lists the sessions a running execution holds.
//
// It exists for the one decision an entry point makes about someone else's work: whether to offer
// to STOP it (the node's own `session:end` route, node/local.go) and whether a delete it is about
// to attempt will be refused. A sidebar asks this about every row it draws, so the cost matters:
// one directory listing plus a read per HELD session — nobody writes a hold for an idle one —
// rather than a file read per row.
//
// A hold that cannot be READ is an error rather than "not busy": the caller is about to enable a
// destructive action, and guessing there is worse than reporting that it does not know.
func (r *Retention) BusySessions() (map[string]bool, error) {
	entries, err := os.ReadDir(r.holdsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing was ever held in this data dir
		}
		return nil, fmt.Errorf("session: read retention holds: %w", err)
	}
	busy := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		holds, err := r.read(id)
		if err != nil {
			return nil, fmt.Errorf("session: read retention hold for %s: %w", id, err)
		}
		if len(liveHolds(holds)) > 0 {
			busy[id] = true
		}
	}
	return busy, nil
}

// HoldsByOwner returns every hold this owner has, keyed by session.
//
// It exists for RECONCILIATION: the owner of a hold's lifetime is the subsystem that knows
// whether the thing behind the ref still exists, and after a crash only it can say which
// leftovers are stale (design §9.4: 崩溃留下的中间状态仍需…对账).
func (r *Retention) HoldsByOwner(owner string) (map[string][]Hold, error) {
	// No ensureDir: not having written a hold yet is the common case, and the read below
	// already answers it with "no holds".
	entries, err := os.ReadDir(r.holdsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read retention holds: %w", err)
	}

	out := make(map[string][]Hold)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		var file holdFile
		if err := fileutil.ReadJSON(r.holdPath(id), &file); err != nil {
			// A hold we cannot read is not a hold we may ignore: swallowing it would let a
			// sweep delete a session something may still need, so the caller hears about it
			// and stays conservative.
			return nil, fmt.Errorf("session: read retention hold for %s: %w", id, err)
		}
		for _, h := range file.Holds {
			if h.Owner != owner {
				continue
			}
			h.SessionID = id
			out[id] = append(out[id], h)
		}
	}
	return out, nil
}

// errProtectionUnknown says the retention record could not be read at all. It is separate from
// "held" because the two lead to the same decision for different reasons — and the reason is what
// the log has to say.
var errProtectionUnknown = errors.New("session: retention record unreadable")

// protected answers the ONE question every deletion path asks: is an execution running in this
// session right now?
//
// It used to be two questions — a count-based sweep respected any hold, while an owner's explicit
// delete respected only an execution's — because a remote conversation held its session for a
// retention window of its own. That window is gone (see the ReasonTask comment): with a single
// reason, "may this be deleted?" has a single answer, and the sweep and the reader's click agree
// by construction. A second reason would split this again, and the split belongs HERE, next to the
// reasons, rather than in each caller.
//
// Callers that are about to DELETE call it inside Lock (see Manager.Delete and the cleanup): the
// answer must be the one that was true at the moment of the decision. Unreadable protection is
// not "no protection" — it is an error, and both callers keep their hands off. A RETIRED hold is
// neither: this version knows what it means and knows it protects nothing (liveHolds).
func (r *Retention) protected(sessionID string) error {
	holds, err := r.read(sessionID)
	if err != nil {
		return fmt.Errorf("%w: %s: %s", errProtectionUnknown, sessionID, err)
	}
	holds = liveHolds(holds)
	if len(holds) == 0 {
		return nil
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].Since.Before(holds[j].Since) })
	return &SessionHeldError{SessionID: sessionID, Holds: holds}
}

// holdFile is what one session's hold file contains.
type holdFile struct {
	Holds []Hold `json:"holds"`
}

func (r *Retention) holdsDir() string { return filepath.Join(r.dir, holdsDirName) }
func (r *Retention) holdPath(id string) string {
	return filepath.Join(r.holdsDir(), id+".json")
}

// sessionExists reports whether the session's directory is still there. It is a filesystem
// fact, deliberately not a store call: this layer must be usable by any manager over the same
// data dir, and the directory name IS the session id in both.
func (r *Retention) sessionExists(id string) bool {
	if !fileutil.IsSinglePathElement(id) {
		return false
	}
	info, err := os.Stat(filepath.Join(r.sessionDir, id))
	return err == nil && info.IsDir()
}

func (r *Retention) read(id string) ([]Hold, error) {
	if !fileutil.IsSinglePathElement(id) {
		return nil, fmt.Errorf("session: refusing hold lookup for %q", id)
	}
	var file holdFile
	err := fileutil.ReadJSON(r.holdPath(id), &file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: read retention hold for %s: %w", id, err)
	}
	return file.Holds, nil
}

// writeLocked stores a session's holds, removing the file when nothing is left, so an
// unprotected session is indistinguishable from one that was never held.
func (r *Retention) writeLocked(id string, holds []Hold) error {
	if !fileutil.IsSinglePathElement(id) {
		return fmt.Errorf("session: refusing hold write for %q", id)
	}
	if len(holds) == 0 {
		if err := os.Remove(r.holdPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: clear retention hold for %s: %w", id, err)
		}
		return nil
	}
	for i := range holds {
		holds[i].SessionID = id
	}
	if err := fileutil.AtomicWriteJSONPrivate(r.holdPath(id), holdFile{Holds: holds}); err != nil {
		return fmt.Errorf("session: write retention hold for %s: %w", id, err)
	}
	return nil
}

func (r *Retention) ensureDir() error {
	if err := os.MkdirAll(r.holdsDir(), holdDirPerm); err != nil {
		return fmt.Errorf("session: create retention dir: %w", err)
	}
	return nil
}
