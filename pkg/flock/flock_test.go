package flock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// lockPath returns a path whose parent does not exist yet, so every test also
// exercises the directory creation.
func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "node", "node.lock")
}

func TestTryAcquireExcludesASecondDescriptor(t *testing.T) {
	path := lockPath(t)

	first, err := TryAcquire(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, first.Release()) }()

	// A second descriptor, in the SAME process: flock is per open file
	// description, which is what makes this usable for in-process callers too.
	_, err = TryAcquire(path)
	require.ErrorIs(t, err, ErrHeld)
}

func TestReleaseAdmitsTheNextHolderAndKeepsTheFile(t *testing.T) {
	path := lockPath(t)

	first, err := TryAcquire(path)
	require.NoError(t, err)
	require.True(t, first.Held())
	require.NoError(t, first.Release())
	require.False(t, first.Held())
	require.NoError(t, first.Release(), "release is idempotent")

	second, err := TryAcquire(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, second.Release()) }()
	require.Equal(t, path, second.Path())

	// The lock file outlives its holders: a recreated inode would be a lock
	// nobody contests.
	_, err = os.Stat(path)
	require.NoError(t, err)
}

func TestAcquireWaitsForTheHolderToRelease(t *testing.T) {
	path := lockPath(t)

	held, err := TryAcquire(path)
	require.NoError(t, err)
	released := make(chan struct{})
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = held.Release()
		close(released)
	}()

	got, err := Acquire(context.Background(), path, 2*time.Second)
	require.NoError(t, err)
	require.NoError(t, got.Release())
	<-released
}

func TestAcquireReportsTheBudgetRunningOut(t *testing.T) {
	path := lockPath(t)

	held, err := TryAcquire(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Release()) }()

	started := time.Now()
	_, err = Acquire(context.Background(), path, 70*time.Millisecond)
	require.ErrorIs(t, err, ErrHeld)
	require.GreaterOrEqual(t, time.Since(started), 50*time.Millisecond)
}

func TestAcquireHonorsContextCancellation(t *testing.T) {
	path := lockPath(t)

	held, err := TryAcquire(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Release()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	// budget 0 = wait until ctx says otherwise, which is the only thing that can.
	_, err = Acquire(ctx, path, 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
