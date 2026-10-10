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
// The record is per session and per REFERENCE. One task ending releases that task's hold and
// leaves the context's alone, so "release" can never mean "release everything holding this
// session" — the node's reason for keeping a session and the context's are different reasons
// with different lifetimes, and one of them ending says nothing about the other.
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
const (
	// ReasonTask: a task needs this session until it reaches a terminal state.
	ReasonTask = "task"
	// ReasonContext: a remote conversation is still within its retention period, so it can be
	// continued even while no task is running (§7.1.2's "idle but answerable" window).
	ReasonContext = "context"
)

// Hold is one reason a session must not be deleted.
type Hold struct {
	// Owner is the subsystem holding it. Ownership is recorded rather than assumed: several
	// owners will share this data dir, and a release must only ever drop one owner's hold.
	Owner string `json:"owner"`
	// Reason is what kind of thing is holding it (ReasonTask / ReasonContext).
	Reason string `json:"reason"`
	// Ref identifies the holder: a task id, a context id. Release is by (owner, ref), which is
	// what makes "one of them ended" expressible.
	Ref string `json:"ref"`
	// SessionID is the session the hold is written on. It is duplicated from the file name so
	// a record read on its own says what it is about; the file name stays the authority.
	SessionID string `json:"session_id"`
	// Since is when the hold was taken (diagnostics; also the honest answer to "why is this
	// still here").
	Since time.Time `json:"since"`
	// ExpiresAt ends the hold without anyone having to act, for holds whose reason has a
	// limited life (a context's retention window). Zero means "until released".
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Expired reports whether a hold's deadline has passed.
func (h Hold) Expired(now time.Time) bool {
	return !h.ExpiresAt.IsZero() && !now.Before(h.ExpiresAt)
}

// Describe renders one hold as a short phrase for a refusal message.
func (h Hold) Describe() string {
	what := h.Reason
	switch h.Reason {
	case ReasonTask:
		what = "任务"
	case ReasonContext:
		what = "对话"
	}
	if h.Ref == "" {
		return what
	}
	return what + " " + h.Ref
}

// SessionHeldError is the refusal an explicit delete gets for a session a hold protects.
//
// It is a value rather than a bare error so the frontends can render the REASON (which hold,
// for what) instead of a generic failure: the reader's next action is to end that task or
// close that conversation, and a refusal that does not say which one is not actionable
// (design §9.4: the interfaces only display the reason, the decision lives here).
type SessionHeldError struct {
	SessionID string
	Holds     []Hold
}

func (e *SessionHeldError) Error() string {
	parts := make([]string, 0, len(e.Holds))
	for _, h := range e.Holds {
		parts = append(parts, h.Describe())
	}
	return fmt.Sprintf("会话 %s 仍在使用中（%s），请先结束对应任务或关闭该对话再删除",
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

// Release drops the holds matching this owner and ref. Releasing something that is not held is
// not an error: a crash and a restart both end up releasing the same holds twice.
func (r *Retention) Release(sessionID, owner, ref string) error {
	if sessionID == "" || owner == "" || ref == "" {
		return nil
	}
	return r.Lock(func() error {
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
	})
}

// Held returns the holds that keep a session, which is what a refusal and a repair both need.
//
// It reads without the coordination lock: writes publish atomically, so a reader either sees
// the whole old record or the whole new one. Reading is not deciding — a deletion must go
// through Lock.
func (r *Retention) Held(sessionID string) ([]Hold, error) {
	if sessionID == "" {
		return nil, nil
	}
	holds, err := r.read(sessionID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	live := make([]Hold, 0, len(holds))
	for _, h := range holds {
		if !h.Expired(now) {
			live = append(live, h)
		}
	}
	return live, nil
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

// Sweep drops holds whose deadline has passed and reports how many it dropped.
//
// It is the other half of "a hold has a lifetime": a context's protection must end by itself,
// because the node that took it may never run again to release it. Dropping a hold is the
// only safe direction of that mistake — the session merely becomes deletable.
func (r *Retention) Sweep() (int, error) {
	now := time.Now().UTC()
	dropped := 0
	err := r.Lock(func() error {
		// The lock made the directory (see Lock), so this read cannot be looking at nothing.
		entries, err := os.ReadDir(r.holdsDir())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			holds, err := r.read(id)
			if err != nil {
				return err
			}
			kept := holds[:0]
			for _, h := range holds {
				if h.Expired(now) {
					dropped++
					continue
				}
				kept = append(kept, h)
			}
			if len(kept) == len(holds) {
				continue
			}
			if err := r.writeLocked(id, kept); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if dropped > 0 && r.log != nil {
		r.log.Info(context.Background(), "session: retention holds expired", "dropped", dropped)
	}
	return dropped, nil
}

// errProtectionUnknown reports that the holds could not be read, so no decision about
// deletion can be made. It is distinct from ErrSessionHeld because the two mean different
// consequences for the reader: "something still needs this" versus "we cannot tell, and
// deleting could lose what something needs" (design §9.4: 保护元数据暂不可读…保守跳过并报告).
var errProtectionUnknown = errors.New("session: cannot tell whether this session is protected")

// protected returns the error an explicit delete of this session must be refused with, or nil.
//
// Callers that are about to DELETE call it inside Lock (see Manager.Delete and the cleanup):
// the answer must be the one that was true at the moment of the decision.
func (r *Retention) protected(sessionID string) error {
	holds, err := r.read(sessionID)
	if err != nil {
		// Unreadable protection is not the same as no protection.
		return fmt.Errorf("%w: %s: %s", errProtectionUnknown, sessionID, err)
	}
	now := time.Now().UTC()
	live := make([]Hold, 0, len(holds))
	for _, h := range holds {
		if !h.Expired(now) {
			live = append(live, h)
		}
	}
	if len(live) == 0 {
		return nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Since.Before(live[j].Since) })
	return &SessionHeldError{SessionID: sessionID, Holds: live}
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
