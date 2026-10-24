package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// TestHostLockIsOnePerBaseDir pins the rule the two entry points rely on: within one base directory
// only the first caller gets the lock, and releasing it makes the directory available again.
//
// In-process is a real test of this: flock is per open FILE DESCRIPTION, not per process, so two
// descriptors on the same path conflict even here (pkg/flock says so, and node's own tests use the
// same trick). That is what makes the second launch refuse instead of racing the first.
func TestHostLockIsOnePerBaseDir(t *testing.T) {
	config.SetBaseDir(t.TempDir())

	first, err := acquireHostLock()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if second, err := acquireHostLock(); err == nil {
		second.Release()
		t.Fatal("a second host got the lock while the first held it")
	} else if !errors.Is(err, errHostLocked) {
		t.Fatalf("second acquire error = %v, want errHostLocked", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	again, err := acquireHostLock()
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = again.Release()
}

// TestHostLockLeavesALockFile pins the half pkg/flock warns about: the FILE must outlive its holder.
// Removing it on release would let the next process create a fresh inode and take a lock nobody
// contests while the old holder still believes it is alone.
func TestHostLockLeavesALockFile(t *testing.T) {
	dir := t.TempDir()
	config.SetBaseDir(dir)

	lock, err := acquireHostLock()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_ = lock.Release()

	if _, err := os.Stat(filepath.Join(dir, hostLockFile)); err != nil {
		t.Fatalf("lock file should still exist after release: %v", err)
	}
}
