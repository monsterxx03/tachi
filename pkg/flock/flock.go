// Package flock is a process-level exclusive lock on a file.
//
// It is deliberately tiny: a node service needs exactly one thing from it — "one
// holder per identity, however many tried" — and the kernel's flock(2) already is
// that primitive. Three properties matter to callers:
//
//   - flock is per OPEN FILE DESCRIPTION, not per process, so two descriptors on
//     the same path conflict even inside ONE process. That is what makes a
//     start-or-attach call safe against concurrent callers in the same process,
//     not just against a second process.
//   - Release does NOT remove the lock file. Removing it would let the next
//     process create a fresh inode and take a lock nobody contests, while the old
//     holder still believes it is alone. The mutual exclusion IS the file's
//     identity, so the file must outlive every holder.
//   - a lock file records nothing about its holder. A PID file is the opposite:
//     it records an intention, and a crashed process leaves one behind pointing
//     at a stranger. Never use one as the lock.
//
// Linux and darwin only — the two release targets. A Windows port would put
// LockFileEx behind the same three functions; callers never see which syscall.
package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	lockDirPerm  os.FileMode = 0o700
	lockFilePerm os.FileMode = 0o600

	// retryInterval is how often Acquire re-tries a held lock: small enough to be
	// invisible to a process waiting out another one's startup, large enough not
	// to spin a core.
	retryInterval = 25 * time.Millisecond
)

// ErrHeld reports that someone else holds the lock. TryAcquire returns it instead
// of waiting; Acquire returns it only when its budget runs out.
var ErrHeld = errors.New("flock: lock is held")

// Lock is a held lock. Release it once the resource it guards is gone.
type Lock struct {
	path string
	f    *os.File
}

// TryAcquire takes the lock, or reports ErrHeld without waiting.
func TryAcquire(path string) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrHeld, path)
		}
		return nil, fmt.Errorf("flock: acquire %s: %w", path, err)
	}
	return &Lock{path: path, f: f}, nil
}

// Acquire waits for the lock to become free, up to budget (0 = wait until ctx is
// done). Running out of budget reports ErrHeld — the lock is still someone
// else's, which is a different fact from a ctx cancellation.
func Acquire(ctx context.Context, path string, budget time.Duration) (*Lock, error) {
	var expire <-chan time.Time
	if budget > 0 {
		timer := time.NewTimer(budget)
		defer timer.Stop()
		expire = timer.C
	}

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	for {
		l, err := TryAcquire(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, ErrHeld) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-expire:
			return nil, fmt.Errorf("%w: %s (waited %s)", ErrHeld, path, budget)
		case <-ticker.C:
		}
	}
}

// Path is the lock file's path.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Held reports whether this handle still holds the lock.
func (l *Lock) Held() bool { return l != nil && l.f != nil }

// Release unlocks and closes the descriptor, leaving the lock FILE in place (see
// the package comment). Release is idempotent, so a caller that releases on a
// failed start and again on shutdown unlocks nothing the second time.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil

	ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	cerr := f.Close()
	if ferr != nil {
		return fmt.Errorf("flock: release %s: %w", l.path, ferr)
	}
	if cerr != nil {
		return fmt.Errorf("flock: close %s: %w", l.path, cerr)
	}
	return nil
}

func openLockFile(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, lockDirPerm); err != nil {
			return nil, fmt.Errorf("flock: create %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("flock: open %s: %w", path, err)
	}
	return f, nil
}
