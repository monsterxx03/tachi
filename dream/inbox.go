package dream

// The inbox handoff.
//
// inbox.md is a WRITE-AHEAD LOG, not a store: the MemoryRecord tool appends to
// it while a dream is running, and the dream consumes it. The old code let the
// agent read inbox.md and then force-truncated whatever was left — so a memory
// recorded during a dream (which can take minutes) was silently deleted.
//
// The handoff makes the two halves disjoint: at the start of a run the inbox is
// RENAMED to inbox.processing.md, so writes that arrive afterwards land in a
// fresh inbox.md the running dream never sees. Rename is atomic within the
// memory directory. On success the snapshot is deleted; on failure its content
// goes back into the inbox (duplicates are cheaper than a lost memory, and the
// next dream dedupes when it consolidates).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

const (
	inboxName       = memory.InboxFile
	inboxProcessing = memory.InboxProcessingFile
)

// inboxPath returns the paths a domain's inbox handoff uses.
func inboxPath(memoryRoot string) string {
	return filepath.Join(memoryRoot, inboxName)
}

func processingPath(memoryRoot string) string {
	return filepath.Join(memoryRoot, inboxProcessing)
}

// TakeInbox moves the current inbox aside for the dream agent to consume and
// reports whether a snapshot was taken.
//
// A processing file left behind by a crashed run is put back into the inbox
// first: it was never integrated, and moving it forward would hide it.
func TakeInbox(memoryRoot string, l *logger.Logger) (bool, error) {
	procPath := processingPath(memoryRoot)
	if fileutil.Exists(procPath) {
		if err := appendFile(inboxPath(memoryRoot), procPath); err != nil {
			return false, fmt.Errorf("recover previous inbox snapshot: %w", err)
		}
		if err := os.Remove(procPath); err != nil {
			return false, fmt.Errorf("remove recovered snapshot: %w", err)
		}
		if l != nil {
			l.Warn(context.Background(), "dream: recovered an unprocessed inbox snapshot from a previous run")
		}
	}

	info, err := os.Stat(inboxPath(memoryRoot))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}

	if err := os.Rename(inboxPath(memoryRoot), procPath); err != nil {
		return false, fmt.Errorf("move inbox aside: %w", err)
	}
	return true, nil
}

// FinishInbox closes the handoff.
//
// success removes the snapshot: the agent integrated it (or chose not to, but
// it saw it). Failure puts the content back — whatever the agent managed to
// write into topic files stays; the inbox keeps the raw material for the next
// run, and recall's content dedupe absorbs the overlap.
func FinishInbox(memoryRoot string, success bool, l *logger.Logger) {
	procPath := processingPath(memoryRoot)
	if !fileutil.Exists(procPath) {
		return
	}
	if success {
		if err := os.Remove(procPath); err != nil {
			if l != nil {
				l.Error(context.Background(), "dream: remove inbox snapshot", err, "path", procPath)
			}
		}
		return
	}

	if err := appendFile(inboxPath(memoryRoot), procPath); err != nil {
		// Leaving the snapshot in place is the safe failure: the next run's
		// TakeInbox recovers it.
		if l != nil {
			l.Error(context.Background(), "dream: restore inbox snapshot", err, "path", procPath)
		}
		return
	}
	_ = os.Remove(procPath)
}

// appendFile appends src's bytes to dst, creating dst when missing.
func appendFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	f, err := os.OpenFile(dst, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
