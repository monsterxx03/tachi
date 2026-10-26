package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/flock"
)

// hostLockFile is the one-host rule's lock file, beside config.yaml in the base directory.
//
// WHY a process-wide lock: the desktop app and the headless host are two ways to run the SAME
// session runtime over the SAME ~/.tachi. Nothing else stops both from starting — they are
// different binaries with different single-instance mechanisms (Wails' session-bus name on Linux,
// the app bundle on macOS), and neither knows about the other. Two live agents over one
// conversation means two writers to one messages.jsonl, each building its next request from its own
// copy of a history the other is still appending to. No merge makes that safe, so the rule is:
// **one host per base directory, whichever entry point gets there first**.
//
// It is flock, not a PID file, for the reason pkg/flock spells out: the kernel releases it when the
// process dies, however it dies. A crashed host leaves nothing to clean up — and a lock file that
// outlived its holder would lock the user out of their own data.
const hostLockFile = "host.lock"

// acquireHostLock takes the one-host lock for this process's base directory.
//
// TryAcquire, not Acquire: there is nothing to wait for. A second host that blocks for five seconds
// and then gives up is worse than one that says so immediately — the user either meant to start it
// (and has forgotten the other one) or did not.
func acquireHostLock() (*flock.Lock, error) {
	path := filepath.Join(config.BaseDir(), hostLockFile)
	lock, err := flock.TryAcquire(path)
	if err == nil {
		return lock, nil
	}
	if errors.Is(err, flock.ErrHeld) {
		return nil, hostLockedError(path)
	}
	return nil, fmt.Errorf("拿不到 %s：%w", path, err)
}

// errHostLocked is the sentinel both entry points report, so a caller (and a test) can tell "another
// host owns this data directory" apart from "the lock file itself could not be opened".
var errHostLocked = errors.New("已经有一个 Tachi 在使用这份数据")

// hostLockedError is the sentence both entry points show. It names the DIRECTORY rather than the
// other process because that is what the rule is about: the same data, two writers. Which program
// holds it (a window, a background host) is not something this process can know, and guessing would
// send the reader looking for the wrong window.
func hostLockedError(lockPath string) error {
	return fmt.Errorf("%w（%s）—— 同一份数据同时只能由一个进程跑"+
		"（桌面窗口或后台的无头宿主）。两个进程会往同一批会话里写，所以先退出那个再启动这个。",
		errHostLocked, filepath.Dir(lockPath))
}
