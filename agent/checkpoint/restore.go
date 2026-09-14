package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Preview is what a rewind would do to the workspace, computed before anything
// is written. It exists because a rewind DELETES files as well as restoring
// them: "changes were rewound" is not something a reader should discover
// afterwards.
type Preview struct {
	// Turn is what the caller asked for; Target is the checkpoint that carries
	// the file state for it. They differ when the requested turn wrote nothing
	// (see resolveTarget) — a turn that only reads has no snapshot of its own,
	// and the state at its start is the next writing turn's starting state.
	Turn   int           `json:"turn"`
	Target int           `json:"target"`
	Roots  []RootPreview `json:"roots,omitempty"`
	// Skipped is the reason no file state is available: the requested point had
	// no snapshot because a guard refused it, or because nothing has been
	// snapshotted at all. A rewind can still move the conversation back; it must
	// say that no files were restored rather than implying they were.
	Skipped      string   `json:"skipped,omitempty"`
	Irreversible []string `json:"irreversible,omitempty"`
}

// RootPreview is one root's part of a preview.
type RootPreview struct {
	Root string `json:"root"`
	// Added are files that exist now and would be DELETED by the rewind,
	// Changed ones whose content would be restored, Deleted ones that would come
	// back. Added is called out separately because it is the destructive half.
	Added   []string `json:"added,omitempty"`
	Changed []string `json:"changed,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
	// Stat is git's own diffstat, for display.
	Stat string `json:"stat,omitempty"`
}

// Empty reports whether the preview would change nothing.
func (p Preview) Empty() bool {
	for _, r := range p.Roots {
		if len(r.Added)+len(r.Changed)+len(r.Deleted) > 0 {
			return false
		}
	}
	return true
}

// Restore is what a rewind did.
type Restore struct {
	Turn    int           `json:"turn"`
	Target  int           `json:"target"`
	Roots   []RootRestore `json:"roots,omitempty"`
	Skipped string        `json:"skipped,omitempty"`
}

// RootRestore is one root's part of a restore.
type RootRestore struct {
	Root     string   `json:"root"`
	Restored []string `json:"restored,omitempty"`
	Removed  []string `json:"removed,omitempty"`
}

// Preview computes what rewinding to a turn would change, without changing it.
func (m *Manager) Preview(ctx context.Context, turn int) (Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.preview(ctx, turn)
}

func (m *Manager) preview(ctx context.Context, turn int) (Preview, error) {
	man, err := loadManifest(m.dir)
	if err != nil {
		return Preview{}, err
	}
	target, ok, reason := m.resolveTarget(man, turn)
	if !ok {
		return Preview{Turn: turn, Skipped: reason}, nil
	}
	p := Preview{Turn: turn, Target: target.Turn, Irreversible: target.Irreversible}
	for _, rs := range target.Roots {
		r := m.repoAt(rs.RootIndex)
		// The current state has to become a tree before it can be compared: git
		// cannot see untracked files in a diff against a tree, and a file the
		// agent created is exactly what a reader needs to be warned about. The
		// objects written here are the ones the next snapshot would write anyway.
		cur, err := r.snapshot(ctx)
		if err != nil {
			return Preview{}, err
		}
		changes, err := r.changes(ctx, rs.Tree, cur)
		if err != nil {
			return Preview{}, err
		}
		rp := RootPreview{Root: rs.Root}
		for _, c := range parseChanges(changes) {
			switch c.Status {
			case statusAdded:
				rp.Added = append(rp.Added, c.Path)
			case statusDeleted:
				rp.Deleted = append(rp.Deleted, c.Path)
			default:
				rp.Changed = append(rp.Changed, c.Path)
			}
		}
		if rp.Stat, err = r.stat(ctx, rs.Tree, cur); err != nil {
			return Preview{}, err
		}
		p.Roots = append(p.Roots, rp)
	}
	return p, nil
}

// Restore puts the workspace back to the state a rewind to turn requires.
//
// It writes only the paths that actually differ (a full checkout would refresh
// every mtime and make the next incremental build rebuild the world), and it
// removes the files that were created after the checkpoint — the destructive
// half, which is why a caller shows Preview first.
//
// It does NOT touch the conversation: truncating messages.jsonl and
// api_requests.jsonl is the session store's job, and the two must happen
// together so the model's belief and the disk never disagree.
func (m *Manager) Restore(ctx context.Context, turn int) (Restore, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return Restore{}, err
	}
	target, ok, reason := m.resolveTarget(man, turn)
	if !ok {
		return Restore{Turn: turn, Skipped: reason}, nil
	}

	out := Restore{Turn: turn, Target: target.Turn}
	for _, rs := range target.Roots {
		r := m.repoAt(rs.RootIndex)
		cur, err := r.snapshot(ctx)
		if err != nil {
			return out, err
		}
		changes, err := r.changes(ctx, rs.Tree, cur)
		if err != nil {
			return out, err
		}
		var restore, remove []string
		for _, c := range parseChanges(changes) {
			if c.Status == statusAdded {
				remove = append(remove, c.Path)
				continue
			}
			restore = append(restore, c.Path)
		}

		// The index is pointed at the checkpoint first, so checkout-index writes
		// the recorded content rather than whatever the index happened to hold.
		if len(restore) > 0 {
			if err := r.setIndex(ctx, rs.Tree); err != nil {
				return out, err
			}
			if err := r.writePaths(ctx, restore); err != nil {
				return out, err
			}
		}
		if err := removePaths(r.root, remove); err != nil {
			return out, err
		}
		rr := RootRestore{Root: rs.Root, Removed: remove}
		if len(restore) > 0 {
			sort.Strings(restore)
			rr.Restored = restore
		}
		out.Roots = append(out.Roots, rr)
	}
	return out, nil
}

// change is one entry of git diff --name-status.
type change struct {
	Status string
	Path   string
}

// Git's status letters, as far as a rewind cares: anything that is not A or D
// (M, T, C, ...) is "restore the recorded content".
const (
	statusAdded   = "A"
	statusDeleted = "D"
)

// parseChanges reads `git diff --name-status -z` output: alternating status and
// path fields, NUL-terminated. NUL separation is what makes it safe for paths
// with tabs, quotes, or non-UTF-8 bytes in them.
func parseChanges(out []byte) []change {
	fields := strings.Split(string(out), "\x00")
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			break
		}
		changes = append(changes, change{Status: fields[i], Path: fields[i+1]})
	}
	return changes
}

// removePaths deletes files a rewind is taking back, then prunes directories
// they leave empty — a rewind that leaves an empty dist/ behind is not what
// "back to that point" means.
func removePaths(root string, paths []string) error {
	for _, rel := range paths {
		abs := filepath.Join(root, rel)
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", abs, err)
		}
		pruneEmptyParents(root, filepath.Dir(abs))
	}
	return nil
}

// pruneEmptyParents walks up from dir removing empty directories, stopping at
// root (which is never removed) or at the first directory that still has
// something in it.
func pruneEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
